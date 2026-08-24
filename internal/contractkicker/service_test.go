package contractkicker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/omni-labs-jp/axios_itandi_automations/pkg/clock"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/env"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/itandi"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/logging"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/webhook"
)

var now = time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

// ---- test doubles ----

type fakeLister struct {
	entries []itandi.EntryHead
	err     error
	calls   []itandi.ListParams
}

func (f *fakeLister) ListEntryHeads(_ context.Context, p itandi.ListParams) ([]itandi.EntryHead, error) {
	f.calls = append(f.calls, p)
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

type fakeSender struct {
	result   webhook.Result
	err      error
	payloads []any
}

func (f *fakeSender) Send(ctx context.Context, payload any) (webhook.Result, error) {
	f.payloads = append(f.payloads, payload)
	if err := ctx.Err(); err != nil {
		return webhook.Result{}, err
	}
	return f.result, f.err
}

func acceptedSender() *fakeSender {
	return &fakeSender{result: webhook.Result{StatusCode: 200, Status: "accepted", JobID: "job-1"}}
}

// ---- harness ----

type harness struct {
	svc    *Service
	lister *fakeLister
	store  *ledger.MemoryStore
	sender *fakeSender
	clk    *clock.Fake
	logs   *bytes.Buffer
	cfg    Config
}

func newHarness(t *testing.T, overrides map[string]string) *harness {
	t.Helper()

	m := map[string]string{
		EnvITANDIToken:      "tok",
		EnvHerokuWebhookURL: "https://app.herokuapp.com/webhook/itandi/contract",
	}
	for k, v := range overrides {
		m[k] = v
	}
	cfg, err := LoadConfig(env.NewFromMap(m))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	h := &harness{
		lister: &fakeLister{},
		store:  ledger.NewMemoryStore(),
		sender: acceptedSender(),
		clk:    clock.NewFake(now),
		logs:   &bytes.Buffer{},
		cfg:    cfg,
	}
	h.svc, err = New(cfg, h.lister, h.store, h.sender, Options{
		Clock:  h.clk,
		Logger: logging.New(h.logs, logging.Options{Level: slog.LevelDebug}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func (h *harness) entry(t *testing.T, id string) ledger.Entry {
	t.Helper()
	e, err := h.store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return e
}

func (h *harness) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		out = append(out, m)
	}
	return out
}

func (h *harness) hasLog(t *testing.T, severity, substr string) bool {
	t.Helper()
	for _, l := range h.logLines(t) {
		if l["severity"] == severity && strings.Contains(l["message"].(string), substr) {
			return true
		}
	}
	return false
}

func head(id int64, status int) itandi.EntryHead {
	return itandi.EntryHead{ID: id, EntryStatusID: status, UpdatedAt: now}
}

// ---- New ----

func TestNewRejectsMissingDependencies(t *testing.T) {
	cfg, err := LoadConfig(env.NewFromMap(map[string]string{
		EnvITANDIToken:      "tok",
		EnvHerokuWebhookURL: "https://example.com/hook",
	}))
	if err != nil {
		t.Fatal(err)
	}
	store := ledger.NewMemoryStore()

	if _, err := New(cfg, nil, store, acceptedSender(), Options{}); err == nil {
		t.Error("nil lister should be rejected")
	}
	if _, err := New(cfg, &fakeLister{}, nil, acceptedSender(), Options{}); err == nil {
		t.Error("nil store should be rejected")
	}
	if _, err := New(cfg, &fakeLister{}, store, nil, Options{}); err == nil {
		t.Error("nil sender should be rejected")
	}
	if _, err := New(Config{}, &fakeLister{}, store, acceptedSender(), Options{}); err == nil {
		t.Error("invalid config should be rejected")
	}

	svc, err := New(cfg, &fakeLister{}, store, acceptedSender(), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.clk == nil || svc.log == nil {
		t.Error("Clock/Logger should fall back to defaults")
	}
}

// ---- Phase A ----

func TestDetectQueriesLookbackWindowAndTargetStatus(t *testing.T) {
	h := newHarness(t, map[string]string{EnvLookbackHours: "48"})
	if _, err := h.svc.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(h.lister.calls) != 1 {
		t.Fatalf("lister called %d times, want 1", len(h.lister.calls))
	}
	p := h.lister.calls[0]
	if want := now.Add(-48 * time.Hour); !p.UpdatedSince.Equal(want) {
		t.Errorf("UpdatedSince = %v, want %v", p.UpdatedSince, want)
	}
	if !p.UpdatedUntil.IsZero() {
		t.Errorf("UpdatedUntil should be unset, got %v", p.UpdatedUntil)
	}
	if len(p.StatusIDs) != 1 || p.StatusIDs[0] != itandi.StatusContract {
		t.Errorf("StatusIDs = %v, want [120]", p.StatusIDs)
	}
}

func TestDetectQueuesUnknownEntriesOnly(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.entries = []itandi.EntryHead{head(1, 120), head(2, 120), head(3, 120)}
	h.store.Seed(ledger.Entry{ID: "2", Status: ledger.StatusDone, DetectedAt: now.Add(-time.Hour)})

	res, err := h.svc.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Detected != 3 || res.NewlyQueued != 2 {
		t.Fatalf("res = %+v, want detected 3 / newlyQueued 2", res)
	}

	for _, id := range []string{"1", "3"} {
		e := h.entry(t, id)
		if e.Status != ledger.StatusPending {
			t.Errorf("entry %s status = %s, want pending", id, e.Status)
		}
		if !e.DetectedAt.Equal(now) || !e.UpdatedAt.Equal(now) {
			t.Errorf("entry %s timestamps = %+v", id, e)
		}
		if e.Attempts != 0 {
			t.Errorf("entry %s attempts = %d, want 0", id, e.Attempts)
		}
	}
	// 既存エントリは触らない（巻き戻り再送を避ける）。
	if got := h.entry(t, "2"); got.Status != ledger.StatusDone {
		t.Errorf("existing entry mutated: %+v", got)
	}
}

func TestDetectIsIdempotentAcrossRuns(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.entries = []itandi.EntryHead{head(1, 120)}

	first, err := h.svc.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(15 * time.Minute)
	second, err := h.svc.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if first.NewlyQueued != 1 || second.NewlyQueued != 0 {
		t.Fatalf("newlyQueued: first %d, second %d", first.NewlyQueued, second.NewlyQueued)
	}
	if h.store.Len() != 1 {
		t.Fatalf("store has %d entries, want 1", h.store.Len())
	}
	if got := h.entry(t, "1"); !got.DetectedAt.Equal(now) {
		t.Errorf("DetectedAt should not be overwritten: %v", got.DetectedAt)
	}
}

func TestDetectWarnsWhenListHitsResultCap(t *testing.T) {
	h := newHarness(t, nil)
	for i := 1; i <= itandi.MaxListResults; i++ {
		h.lister.entries = append(h.lister.entries, head(int64(i), 120))
	}

	res, err := h.svc.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ListTruncated {
		t.Error("ListTruncated should be true at the cap")
	}
	if !h.hasLog(t, "WARNING", "result cap") {
		t.Error("expected a WARNING about the 500-result cap")
	}
}

func TestDetectDoesNotWarnBelowCap(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.entries = []itandi.EntryHead{head(1, 120)}

	res, err := h.svc.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ListTruncated {
		t.Error("ListTruncated should be false below the cap")
	}
}

func TestDetectPropagatesListError(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.err = errors.New("itandi down")

	if _, err := h.svc.Detect(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "itandi down") {
		t.Fatalf("err = %v, want the lister error", err)
	}
}

func TestDetectContinuesAfterPerEntryStoreError(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.entries = []itandi.EntryHead{head(1, 120), head(2, 120)}
	h.store.FailPut = errors.New("firestore unavailable")

	res, err := h.svc.Detect(context.Background())
	if err == nil {
		t.Fatal("expected the put errors to be reported")
	}
	if res.NewlyQueued != 0 {
		t.Errorf("NewlyQueued = %d, want 0", res.NewlyQueued)
	}
	// 1 件目で止まらず全件試すこと。
	if n := strings.Count(err.Error(), "firestore unavailable"); n != 2 {
		t.Errorf("both entries should be attempted, got %d errors: %v", n, err)
	}
}

func TestDetectReportsGetErrorWithoutQueueing(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.entries = []itandi.EntryHead{head(1, 120)}
	h.store.FailGet = errors.New("get boom")

	res, err := h.svc.Detect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "get boom") {
		t.Fatalf("err = %v", err)
	}
	if res.NewlyQueued != 0 {
		t.Errorf("NewlyQueued = %d, want 0", res.NewlyQueued)
	}
}

// ---- Phase B ----

func TestDispatchSendsOldestPendingOnly(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Seed(
		ledger.Entry{ID: "20", Status: ledger.StatusPending, DetectedAt: now.Add(-time.Hour)},
		ledger.Entry{ID: "10", Status: ledger.StatusPending, DetectedAt: now.Add(-2 * time.Hour)},
		ledger.Entry{ID: "30", Status: ledger.StatusPending, DetectedAt: now},
	)

	res, err := h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Selected != 1 || res.Sent != 1 || res.Failed != 0 || res.GaveUp != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(h.sender.payloads) != 1 {
		t.Fatalf("sender called %d times, want 1", len(h.sender.payloads))
	}
	if got := h.sender.payloads[0].(contractPayload); got.ID != 10 {
		t.Errorf("sent id = %d, want 10 (oldest detectedAt)", got.ID)
	}

	sent := h.entry(t, "10")
	if sent.Status != ledger.StatusDone || sent.Attempts != 1 || sent.JobID != "job-1" {
		t.Errorf("entry = %+v", sent)
	}
	if !sent.SentAt.Equal(now) || !sent.NextRetryAt.IsZero() || sent.LastError != "" {
		t.Errorf("entry = %+v", sent)
	}
	if h.entry(t, "20").Status != ledger.StatusPending {
		t.Error("other pending entries must stay untouched")
	}
	if !h.hasLog(t, "INFO", "accepted by heroku") {
		t.Error("expected an INFO log for the accepted entry")
	}
}

func TestDispatchPayloadIsJSONWithNumericID(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Seed(ledger.Entry{ID: "1234567", Status: ledger.StatusPending, DetectedAt: now})

	if _, err := h.svc.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(h.sender.payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"id":1234567}` {
		t.Fatalf("payload = %s, want {\"id\":1234567}", b)
	}
}

func TestDispatchPrefersPendingOverRetryable(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Seed(
		ledger.Entry{ID: "1", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: now.Add(-time.Hour)},
		ledger.Entry{ID: "2", Status: ledger.StatusPending, DetectedAt: now},
	)

	if _, err := h.svc.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.sender.payloads[0].(contractPayload); got.ID != 2 {
		t.Fatalf("sent id = %d, want 2 (pending wins over failed)", got.ID)
	}
}

func TestDispatchFallsBackToDueRetry(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Seed(
		ledger.Entry{ID: "1", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: now.Add(time.Hour)},
		ledger.Entry{ID: "2", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: now.Add(-time.Minute)},
		ledger.Entry{ID: "3", Status: ledger.StatusDone, DetectedAt: now},
		ledger.Entry{ID: "4", Status: ledger.StatusGaveUp, Attempts: 3},
	)

	res, err := h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Selected != 1 || res.Sent != 1 {
		t.Fatalf("res = %+v", res)
	}
	if got := h.sender.payloads[0].(contractPayload); got.ID != 2 {
		t.Fatalf("sent id = %d, want 2 (only due retry)", got.ID)
	}
	if got := h.entry(t, "2"); got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.Attempts)
	}
}

func TestDispatchSkipsDoneAndGaveUp(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Seed(
		ledger.Entry{ID: "1", Status: ledger.StatusDone, DetectedAt: now.Add(-time.Hour)},
		ledger.Entry{ID: "2", Status: ledger.StatusGaveUp, Attempts: 3},
	)

	res, err := h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Selected != 0 || len(h.sender.payloads) != 0 {
		t.Fatalf("res = %+v, sends = %d", res, len(h.sender.payloads))
	}
	if !h.hasLog(t, "INFO", "no entry to send") {
		t.Error("expected the 'nothing to send' log")
	}
}

func TestDispatchRespectsSendLimitPerRun(t *testing.T) {
	h := newHarness(t, map[string]string{EnvSendLimitPerRun: "2"})
	h.store.Seed(
		ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now.Add(-3 * time.Hour)},
		ledger.Entry{ID: "2", Status: ledger.StatusPending, DetectedAt: now.Add(-2 * time.Hour)},
		ledger.Entry{ID: "3", Status: ledger.StatusPending, DetectedAt: now.Add(-time.Hour)},
	)

	res, err := h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Selected != 2 || res.Sent != 2 {
		t.Fatalf("res = %+v", res)
	}
	if len(h.sender.payloads) != 2 {
		t.Fatalf("sends = %d, want 2", len(h.sender.payloads))
	}
}

func TestDispatchMixesPendingAndRetryUpToLimit(t *testing.T) {
	h := newHarness(t, map[string]string{EnvSendLimitPerRun: "3"})
	h.store.Seed(
		ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now},
		ledger.Entry{ID: "2", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: now.Add(-time.Hour)},
		ledger.Entry{ID: "3", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: now.Add(-time.Minute)},
	)

	res, err := h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Selected != 3 || res.Sent != 3 {
		t.Fatalf("res = %+v", res)
	}
	var ids []int64
	for _, p := range h.sender.payloads {
		ids = append(ids, p.(contractPayload).ID)
	}
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Fatalf("ids = %v, want [1 2 3] (pending first, then due retries)", ids)
	}
}

func TestDispatchSkippedResponseCountsAsDone(t *testing.T) {
	h := newHarness(t, nil)
	h.sender.result = webhook.Result{StatusCode: 200, Status: "skipped", Reason: "status_not_target"}
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now})

	res, err := h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 1 {
		t.Fatalf("res = %+v", res)
	}
	if got := h.entry(t, "1"); got.Status != ledger.StatusDone {
		t.Errorf("status = %s, want done", got.Status)
	}
	if !h.hasLog(t, "INFO", "accepted by heroku") {
		t.Error("skipped(200) should log as accepted")
	}
}

func TestDispatchFailureSchedulesBackoff(t *testing.T) {
	h := newHarness(t, nil)
	h.sender.result = webhook.Result{StatusCode: 500, Body: "boom"}
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now})

	res, err := h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Sent != 0 || res.GaveUp != 0 {
		t.Fatalf("res = %+v", res)
	}

	got := h.entry(t, "1")
	if got.Status != ledger.StatusFailed || got.Attempts != 1 {
		t.Fatalf("entry = %+v", got)
	}
	if want := now.Add(15 * time.Minute); !got.NextRetryAt.Equal(want) {
		t.Errorf("NextRetryAt = %v, want %v", got.NextRetryAt, want)
	}
	if !strings.Contains(got.LastError, "HTTP 500") {
		t.Errorf("LastError = %q", got.LastError)
	}
	if !h.hasLog(t, "WARNING", "will retry") {
		t.Error("expected a WARNING log while retries remain")
	}
}

func TestDispatchTransportErrorIsRecorded(t *testing.T) {
	h := newHarness(t, nil)
	h.sender.err = errors.New("connection refused")
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now})

	if _, err := h.svc.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := h.entry(t, "1")
	if got.Status != ledger.StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if !strings.Contains(got.LastError, "connection refused") {
		t.Errorf("LastError = %q", got.LastError)
	}
}

func TestDispatchGivesUpAtMaxAttempts(t *testing.T) {
	h := newHarness(t, nil) // MAX_ATTEMPTS=3
	h.sender.result = webhook.Result{StatusCode: 502, Body: "bad gateway"}
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusFailed, Attempts: 2, NextRetryAt: now.Add(-time.Minute)})

	res, err := h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.GaveUp != 1 || res.Failed != 0 {
		t.Fatalf("res = %+v", res)
	}
	got := h.entry(t, "1")
	if got.Status != ledger.StatusGaveUp || got.Attempts != 3 {
		t.Fatalf("entry = %+v", got)
	}
	if !got.NextRetryAt.IsZero() {
		t.Errorf("gaveup entries should not keep a nextRetryAt: %v", got.NextRetryAt)
	}
	if !h.hasLog(t, "ERROR", "giving up") {
		t.Error("expected an ERROR log on gaveup")
	}

	// 以後は選ばれない。
	h.sender.payloads = nil
	if res, err := h.svc.Dispatch(context.Background()); err != nil || res.Selected != 0 {
		t.Fatalf("gaveup entry was selected again: res = %+v, err = %v", res, err)
	}
}

func TestRetryBackoffProgressesAcrossRuns(t *testing.T) {
	h := newHarness(t, map[string]string{EnvMaxAttempts: "4"})
	h.sender.result = webhook.Result{StatusCode: 500}
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now})

	wantDelays := []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour}
	for i, want := range wantDelays {
		if _, err := h.svc.Dispatch(context.Background()); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		got := h.entry(t, "1")
		if got.Attempts != i+1 {
			t.Fatalf("attempt %d: Attempts = %d", i+1, got.Attempts)
		}
		if expect := h.clk.Now().Add(want); !got.NextRetryAt.Equal(expect) {
			t.Fatalf("attempt %d: NextRetryAt = %v, want %v", i+1, got.NextRetryAt, expect)
		}
		// 次のリトライ可能時刻まで進める。
		h.clk.Set(got.NextRetryAt)
	}

	// 4 回目で上限（MAX_ATTEMPTS=4）。
	if _, err := h.svc.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.entry(t, "1"); got.Status != ledger.StatusGaveUp {
		t.Fatalf("status = %s, want gaveup", got.Status)
	}
}

func TestDispatchNotBeforeNextRetryAt(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: now.Add(time.Second)})

	res, err := h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Selected != 0 {
		t.Fatalf("entry should not be due yet: %+v", res)
	}

	h.clk.Advance(time.Second)
	res, err = h.svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Selected != 1 {
		t.Fatalf("entry should be due now: %+v", res)
	}
}

func TestDispatchDryRunDoesNotSendAndLeavesLedgerUntouched(t *testing.T) {
	h := newHarness(t, map[string]string{EnvDryRun: "true"})
	dry := webhook.DryRunSender{Logger: logging.New(h.logs, logging.Options{}), Target: h.cfg.HerokuWebhookURL}
	svc, err := New(h.cfg, h.lister, h.store, dry, Options{
		Clock:  h.clk,
		Logger: logging.New(h.logs, logging.Options{Level: slog.LevelDebug}),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now})

	res, err := svc.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Selected != 1 || res.DryRun != 1 || res.Sent != 0 {
		t.Fatalf("res = %+v; dry run should be counted separately", res)
	}

	// DRY_RUN を外したときに送られるよう pending のまま残すこと。
	got := h.entry(t, "1")
	if got.Status != ledger.StatusPending {
		t.Errorf("status = %s, want pending (dry run must not consume the backlog)", got.Status)
	}
	if got.Attempts != 0 || !got.SentAt.IsZero() {
		t.Errorf("entry should be untouched: %+v", got)
	}
	if !h.hasLog(t, "INFO", "ledger not updated") {
		t.Error("expected a log stating the ledger was not updated")
	}
}

func TestDryRunThenRealRunStillSends(t *testing.T) {
	// DRY_RUN で検証 → 本稼働へ切り替え、という運用手順をそのまま再現する。
	h := newHarness(t, map[string]string{EnvDryRun: "true"})
	h.lister.entries = []itandi.EntryHead{head(1, 120)}

	dryLog := logging.New(h.logs, logging.Options{Level: slog.LevelDebug})
	dry, err := New(h.cfg, h.lister, h.store, webhook.DryRunSender{Logger: dryLog}, Options{
		Clock:  h.clk,
		Logger: dryLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dry.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.entry(t, "1"); got.Status != ledger.StatusPending {
		t.Fatalf("after dry run: status = %s, want pending", got.Status)
	}

	// DRY_RUN=false 相当（実 Sender）へ切り替える。
	h.clk.Advance(15 * time.Minute)
	summary, err := h.svc.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Sent != 1 {
		t.Fatalf("summary = %+v; the entry examined during dry run must still be sent", summary)
	}
	if got := h.entry(t, "1"); got.Status != ledger.StatusDone {
		t.Errorf("status = %s, want done", got.Status)
	}
}

func TestDispatchListErrorIsReturned(t *testing.T) {
	h := newHarness(t, nil)
	h.store.FailList = errors.New("query failed")

	if _, err := h.svc.Dispatch(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "query failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestDispatchPutErrorAfterSendIsLoudAndReported(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now})
	h.store.FailPut = errors.New("write failed")

	res, err := h.svc.Dispatch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("err = %v", err)
	}
	if res.Sent != 0 {
		t.Errorf("res = %+v; an unrecorded send must not count as sent", res)
	}
	if !h.hasLog(t, "ERROR", "may be sent twice") {
		t.Error("expected a loud ERROR when the result cannot be recorded")
	}
}

func TestDispatchSkipsNonNumericLedgerID(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Seed(ledger.Entry{ID: "not-a-number", Status: ledger.StatusPending, DetectedAt: now})

	res, err := h.svc.Dispatch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid ledger id") {
		t.Fatalf("err = %v", err)
	}
	if len(h.sender.payloads) != 0 {
		t.Error("nothing should be sent for a corrupt ledger id")
	}
	if res.Sent != 0 || res.Failed != 0 || res.GaveUp != 0 {
		t.Errorf("res = %+v", res)
	}
}

func TestDispatchAppliesRequestTimeout(t *testing.T) {
	h := newHarness(t, map[string]string{EnvRequestTimeoutMS: "1"})
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now})

	// fakeSender は ctx が既に切れていればエラーを返す。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.svc.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.entry(t, "1"); got.Status != ledger.StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
}

// ---- Run ----

func TestRunExecutesBothPhases(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.entries = []itandi.EntryHead{head(1, 120), head(2, 120)}

	summary, err := h.svc.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Detected != 2 || summary.NewlyQueued != 2 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Selected != 1 || summary.Sent != 1 {
		t.Fatalf("summary = %+v; exactly one entry should be sent per run", summary)
	}
	if !h.hasLog(t, "INFO", "run finished") {
		t.Error("expected the run summary log")
	}
}

func TestRunDispatchesEvenWhenDetectFails(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.err = errors.New("itandi 403 (ip not allowed)")
	h.store.Seed(ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: now})

	summary, err := h.svc.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ip not allowed") {
		t.Fatalf("err = %v", err)
	}
	if summary.Sent != 1 {
		t.Fatalf("summary = %+v; queued entries must still be dispatched", summary)
	}
	if !h.hasLog(t, "ERROR", "detect phase failed") {
		t.Error("expected an ERROR log for the failed detect phase")
	}
}

func TestRunBacklogDrainsOneEntryPerRun(t *testing.T) {
	h := newHarness(t, nil)
	h.lister.entries = []itandi.EntryHead{head(1, 120), head(2, 120), head(3, 120)}

	var sent []int64
	for i := 0; i < 3; i++ {
		if _, err := h.svc.Run(context.Background()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		h.clk.Advance(15 * time.Minute)
	}
	for _, p := range h.sender.payloads {
		sent = append(sent, p.(contractPayload).ID)
	}
	if len(sent) != 3 || sent[0] != 1 || sent[1] != 2 || sent[2] != 3 {
		t.Fatalf("sent = %v, want [1 2 3] in detection order", sent)
	}
	for _, id := range []string{"1", "2", "3"} {
		if got := h.entry(t, id); got.Status != ledger.StatusDone {
			t.Errorf("entry %s status = %s, want done", id, got.Status)
		}
	}

	// 4 回目は送るものが無い。
	before := len(h.sender.payloads)
	if _, err := h.svc.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.payloads) != before {
		t.Error("nothing should be sent once the backlog is drained")
	}
}

func TestSummaryLogAttrs(t *testing.T) {
	s := Summary{Detected: 5, NewlyQueued: 2, Selected: 1, Sent: 1, Failed: 0, GaveUp: 0, ListTruncated: true}
	attrs := s.LogAttrs()
	if len(attrs)%2 != 0 {
		t.Fatalf("LogAttrs must be key/value pairs: %v", attrs)
	}
	found := map[string]any{}
	for i := 0; i < len(attrs); i += 2 {
		found[attrs[i].(string)] = attrs[i+1]
	}
	if found["detected"] != 5 || found["newlyQueued"] != 2 || found["listTruncated"] != true || found["dryRun"] != 0 {
		t.Fatalf("unexpected attrs: %v", found)
	}
}

func TestFailureReason(t *testing.T) {
	cases := []struct {
		name string
		res  webhook.Result
		err  error
		want string
	}{
		{"http error", webhook.Result{StatusCode: 500, Body: "boom"}, nil, "HTTP 500 body=boom"},
		{"transport error", webhook.Result{}, errors.New("dial tcp: refused"), "dial tcp: refused"},
		{"both", webhook.Result{StatusCode: 502}, errors.New("read: eof"), "HTTP 502: read: eof"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := failureReason(c.res, c.err); got != c.want {
				t.Fatalf("failureReason = %q, want %q", got, c.want)
			}
		})
	}
}
