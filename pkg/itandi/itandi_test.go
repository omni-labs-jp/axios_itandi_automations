package itandi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	opts = append([]Option{WithHTTPClient(srv.Client())}, opts...)
	c, err := NewClient(srv.URL+"/maintenance_company/api/v2/", "tok-123", opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient("", "tok"); err == nil {
		t.Error("empty baseURL should be rejected")
	}
	if _, err := NewClient(ProductionBaseURL, "  "); err == nil {
		t.Error("empty token should be rejected")
	}
	c, err := NewClient(ProductionBaseURL+"/", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != ProductionBaseURL {
		t.Errorf("trailing slash should be trimmed, got %q", c.baseURL)
	}
}

func TestListEntryHeadsRequest(t *testing.T) {
	var gotPath, gotAuth, gotAccept string
	var gotQuery url.Values

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
          {"id":1234567,"company_master_id":6,"entry_status_id":120,"paper":false,"corp":false,
           "foreign_contract_key":"000000000001","priority":1,
           "created_at":"2026-08-01T09:00:00.000+09:00","updated_at":"2026-08-24T10:30:00.000+09:00"}
        ]`))
	})

	since := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	got, err := c.ListEntryHeads(context.Background(), ListParams{
		UpdatedSince: since,
		UpdatedUntil: until,
		StatusIDs:    []int{105, StatusContract},
	})
	if err != nil {
		t.Fatalf("ListEntryHeads: %v", err)
	}

	if gotPath != "/maintenance_company/api/v2/entry_heads" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Token tok-123" {
		t.Errorf("Authorization = %q, want 'Token tok-123'", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if q := gotQuery.Get("datetime"); q != "2026-08-23T10:00:00" {
		t.Errorf("datetime = %q", q)
	}
	if q := gotQuery.Get("end_datetime"); q != "2026-08-24T10:00:00" {
		t.Errorf("end_datetime = %q", q)
	}
	ids := gotQuery["entry_status_ids[]"]
	if len(ids) != 2 || ids[0] != "105" || ids[1] != "120" {
		t.Errorf("entry_status_ids[] = %v, want [105 120]", ids)
	}

	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	e := got[0]
	if e.ID != 1234567 || e.EntryStatusID != StatusContract {
		t.Errorf("unexpected entry: %+v", e)
	}
	wantUpdated := time.Date(2026, 8, 24, 10, 30, 0, 0, time.FixedZone("JST", 9*3600))
	if !e.UpdatedAt.Equal(wantUpdated) {
		t.Errorf("UpdatedAt = %v, want %v", e.UpdatedAt, wantUpdated)
	}
	if e.ForeignContractKey == nil || *e.ForeignContractKey != "000000000001" {
		t.Errorf("ForeignContractKey = %v", e.ForeignContractKey)
	}
}

func TestListEntryHeadsOmitsEmptyParams(t *testing.T) {
	var rawQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	})

	if _, err := c.ListEntryHeads(context.Background(), ListParams{}); err != nil {
		t.Fatal(err)
	}
	if rawQuery != "" {
		t.Fatalf("query should be empty, got %q", rawQuery)
	}
}

func TestListEntryHeadsUsesConfiguredLocation(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	var got string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("datetime")
		_, _ = w.Write([]byte(`[]`))
	}, WithLocation(jst))

	since := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	if _, err := c.ListEntryHeads(context.Background(), ListParams{UpdatedSince: since}); err != nil {
		t.Fatal(err)
	}
	if got != "2026-08-23T19:00:00" {
		t.Fatalf("datetime = %q, want JST-rendered 2026-08-23T19:00:00", got)
	}
}

func TestAPIErrorOnUnauthorized(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
	})

	_, err := c.ListEntryHeads(context.Background(), ListParams{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
	if !apiErr.IsUnauthorized() {
		t.Error("IsUnauthorized() should be true for 401")
	}
	if !strings.Contains(apiErr.Error(), "unauthorized") {
		t.Errorf("error message should include body: %v", apiErr)
	}
}

func TestAPIErrorTruncatesLongBody(t *testing.T) {
	e := &APIError{StatusCode: 500, Method: "GET", Path: "/entry_heads", Body: strings.Repeat("x", 900)}
	msg := e.Error()
	if !strings.Contains(msg, "truncated") {
		t.Fatalf("long body should be truncated: %d chars", len(msg))
	}
}

func TestDecodeErrorOnInvalidJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	})
	if _, err := c.ListEntryHeads(context.Background(), ListParams{}); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestTextBodyLookup(t *testing.T) {
	e := EntryHead{EntryBodies: []EntryBody{
		{Type: "text", Name: "room_name", Text: "アーバン北新宿"},
		{Type: "text", Name: "room_number", Text: "302"},
		{Type: "select", Name: "ignored", Text: "選択肢"},
	}}

	if got := e.RoomName(); got != "アーバン北新宿" {
		t.Errorf("RoomName() = %q", got)
	}
	if got := e.RoomNumber(); got != "302" {
		t.Errorf("RoomNumber() = %q", got)
	}
	// 項目自体が任意入力なので、無い場合は空文字でよい（エラーにしない）。
	if got := e.TextBody("missing"); got != "" {
		t.Errorf("TextBody(missing) = %q, want empty", got)
	}
	// text 以外の型は拾わない。
	if got := e.TextBody("ignored"); got != "" {
		t.Errorf("TextBody(ignored) = %q, want empty (non-text type)", got)
	}
	if got := (EntryHead{}).RoomName(); got != "" {
		t.Errorf("entry_bodies が空でも安全に空文字を返すべき, got %q", got)
	}
}

func TestGetEntryHeadParsesEntryBodies(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":5657694,"entry_status_id":120,"priotity":2,
          "entry_bodies":[
            {"type":"text","id":1,"text":"アーバン北新宿","name":"room_name"},
            {"type":"text","id":2,"text":"302","name":"room_number"}
          ]}`))
	})

	got, err := c.GetEntryHead(context.Background(), 5657694)
	if err != nil {
		t.Fatal(err)
	}
	if got.RoomName() != "アーバン北新宿" || got.RoomNumber() != "302" {
		t.Errorf("物件名/部屋番号が取れていない: %+v", got.EntryBodies)
	}
	// API のフィールド名は "priotity"（綴り間違い）。仕様書の "priority" では取れない。
	if got.Priority != 2 {
		t.Errorf("Priority = %d, want 2", got.Priority)
	}
}

func TestGetEntryHead(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/maintenance_company/api/v2/entry_heads/999" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":999,"entry_status_id":120}`))
	})

	got, err := c.GetEntryHead(context.Background(), 999)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 999 {
		t.Errorf("ID = %d", got.ID)
	}

	if _, err := c.GetEntryHead(context.Background(), 0); err == nil {
		t.Error("id <= 0 should be rejected before the request")
	}
}

func TestContextCancellation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ListEntryHeads(ctx, ListParams{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// 403 の原因切り分けのため、リダイレクト経路・接続先・応答元ヘッダが
// APIError に残ることを確認する。
func TestAPIErrorCarriesDiagnostics(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/maintenance_company/api/v2/entry_heads" {
			http.Redirect(w, r, "/blocked", http.StatusFound)
			return
		}
		w.Header().Set("Server", "nginx")
		w.Header().Set("X-Amzn-Trace-Id", "Root=1-abc")
		w.Header().Set("Set-Cookie", "secret=1") // 診断対象外のヘッダは残さない
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html>403 Forbidden</html>"))
	})

	_, err := c.ListEntryHeads(context.Background(), ListParams{StatusIDs: []int{120}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if !strings.HasSuffix(apiErr.URL, "/blocked") {
		t.Errorf("URL = %q, want final URL after redirect", apiErr.URL)
	}
	if len(apiErr.Redirects) != 1 || !strings.HasPrefix(apiErr.Redirects[0], "302 ") ||
		!strings.Contains(apiErr.Redirects[0], "/entry_heads?entry_status_ids") {
		t.Errorf("Redirects = %v", apiErr.Redirects)
	}
	if apiErr.RemoteAddr == "" {
		t.Error("RemoteAddr should be recorded")
	}
	if apiErr.Header["Server"] != "nginx" || apiErr.Header["X-Amzn-Trace-Id"] != "Root=1-abc" {
		t.Errorf("Header = %v", apiErr.Header)
	}
	if _, ok := apiErr.Header["Set-Cookie"]; ok {
		t.Error("non-diagnostic headers must not be kept")
	}
	// Error() の書式は従来どおり（既存のアラート・検索条件を壊さない）。
	if !strings.HasPrefix(apiErr.Error(), "itandi: GET /entry_heads: unexpected status 403") {
		t.Errorf("Error() = %q", apiErr.Error())
	}
}

func TestAPIErrorWithoutRedirect(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := c.ListEntryHeads(context.Background(), ListParams{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Redirects != nil {
		t.Errorf("Redirects = %v, want nil", apiErr.Redirects)
	}
	if !strings.HasSuffix(apiErr.URL, "/maintenance_company/api/v2/entry_heads") {
		t.Errorf("URL = %q", apiErr.URL)
	}
}
