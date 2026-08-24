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
