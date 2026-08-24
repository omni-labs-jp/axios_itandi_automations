package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientValidation(t *testing.T) {
	cases := []string{"", "   ", "ftp://example.com/hook", "not-a-url", "https://"}
	for _, in := range cases {
		if _, err := NewClient(in); err == nil {
			t.Errorf("NewClient(%q) should fail", in)
		}
	}
	c, err := NewClient(" https://example.com/webhook/itandi/contract ")
	if err != nil {
		t.Fatal(err)
	}
	if c.URL() != "https://example.com/webhook/itandi/contract" {
		t.Errorf("URL() = %q", c.URL())
	}
}

func TestSendAccepted(t *testing.T) {
	var gotBody []byte
	var gotCT, gotToken string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		gotCT = r.Header.Get("Content-Type")
		gotToken = r.Header.Get("X-Kicker-Token")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"status":"accepted","jobId":"contract-1234567-1756000000000"}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, WithHTTPClient(srv.Client()), WithHeader("X-Kicker-Token", "s3cret"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := c.Send(context.Background(), map[string]any{"id": 1234567})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !res.Accepted() {
		t.Errorf("Accepted() = false, result = %+v", res)
	}
	if res.Status != "accepted" || res.JobID != "contract-1234567-1756000000000" {
		t.Errorf("unexpected result: %+v", res)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotToken != "s3cret" {
		t.Errorf("custom header = %q", gotToken)
	}

	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body not JSON: %s", gotBody)
	}
	if payload["id"] != float64(1234567) {
		t.Errorf("payload = %v", payload)
	}
	if !strings.Contains(res.Summary(), "accepted") {
		t.Errorf("Summary() = %q", res.Summary())
	}
}

func TestSendSkippedIsStillAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"skipped","reason":"status_not_target"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, WithHTTPClient(srv.Client()))
	res, err := c.Send(context.Background(), map[string]any{"id": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted() {
		t.Error("skipped(200) should count as accepted")
	}
	if res.Reason != "status_not_target" {
		t.Errorf("Reason = %q", res.Reason)
	}
	if !strings.Contains(res.Summary(), "status_not_target") {
		t.Errorf("Summary() = %q", res.Summary())
	}
}

func TestSendNonSuccessIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, WithHTTPClient(srv.Client()))
	res, err := c.Send(context.Background(), map[string]any{"id": 1})
	if err != nil {
		t.Fatalf("non-2xx should not return an error, got %v", err)
	}
	if res.Accepted() {
		t.Error("500 should not be accepted")
	}
	if res.StatusCode != 500 || res.Body != "boom" {
		t.Errorf("unexpected result: %+v", res)
	}
	if !strings.Contains(res.Summary(), "HTTP 500") {
		t.Errorf("Summary() = %q", res.Summary())
	}
}

func TestSendNonJSONBodyIsTolerated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("y", 600)))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, WithHTTPClient(srv.Client()))
	res, err := c.Send(context.Background(), map[string]any{"id": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted() {
		t.Error("200 with plain text should be accepted")
	}
	if !strings.Contains(res.Body, "truncated") {
		t.Errorf("long body should be truncated, len=%d", len(res.Body))
	}
}

func TestSendTimeoutReturnsError(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	c, _ := NewClient(srv.URL, WithTimeout(50*time.Millisecond))
	res, err := c.Send(context.Background(), map[string]any{"id": 1})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if res.Accepted() {
		t.Error("failed transport must not be accepted")
	}
}

func TestSendUnmarshalablePayload(t *testing.T) {
	c, _ := NewClient("https://example.com/hook")
	if _, err := c.Send(context.Background(), func() {}); err == nil {
		t.Fatal("expected encode error")
	}
}

func TestDryRunSender(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	s := DryRunSender{Logger: log, Target: "https://example.com/hook"}

	res, err := s.Send(context.Background(), map[string]any{"id": 42})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted() || !res.DryRun {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.Summary() != "dry-run: not sent" {
		t.Errorf("Summary() = %q", res.Summary())
	}
	out := buf.String()
	if !strings.Contains(out, `\"id\":42`) || !strings.Contains(out, "example.com/hook") {
		t.Errorf("log should contain payload and target: %s", out)
	}

	// Logger が nil でも panic しない。
	if _, err := (DryRunSender{}).Send(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

var (
	_ Sender = (*Client)(nil)
	_ Sender = DryRunSender{}
)
