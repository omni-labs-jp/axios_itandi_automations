// Package ledgertest は ledger.Store 実装が満たすべき振る舞いを検証する
// 共通テストスイートを提供する。新しい Store 実装を足したら
// RunStoreTests を呼ぶだけで最低限の互換性を確認できる。
package ledgertest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger"
)

// Factory は空の Store を 1 つ用意する。テストごとに呼ばれる。
type Factory func(t *testing.T) ledger.Store

var base = time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

// RunStoreTests は Store 実装の共通仕様を検証する。
func RunStoreTests(t *testing.T, newStore Factory) {
	t.Helper()

	t.Run("GetMissingReturnsErrNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Get(context.Background(), "nope"); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("PutThenGetRoundTrip", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		want := ledger.Entry{
			ID:           "1234567",
			PropertyName: "アーバン北新宿",
			RoomNumber:   "302",
			Status:       ledger.StatusFailed,
			Attempts:     2,
			DetectedAt:   base,
			SentAt:       base.Add(time.Minute),
			NextRetryAt:  base.Add(time.Hour),
			LastError:    "HTTP 500 body=boom",
			JobID:        "contract-1234567-1756000000000",
			UpdatedAt:    base.Add(time.Minute),
		}
		if err := s.Put(ctx, want); err != nil {
			t.Fatalf("Put: %v", err)
		}

		got, err := s.Get(ctx, want.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		assertEntryEqual(t, got, want)
	})

	t.Run("PutOverwrites", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		e := ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: base}
		if err := s.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
		e.Status = ledger.StatusDone
		e.Attempts = 1
		if err := s.Put(ctx, e); err != nil {
			t.Fatal(err)
		}

		got, err := s.Get(ctx, "1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != ledger.StatusDone || got.Attempts != 1 {
			t.Fatalf("got %+v, want done/1", got)
		}
	})

	t.Run("PutRejectsInvalidEntry", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		bad := []ledger.Entry{
			{ID: "", Status: ledger.StatusPending},
			{ID: "1", Status: "bogus"},
			{ID: "1", Status: ledger.StatusPending, Attempts: -1},
		}
		for _, e := range bad {
			if err := s.Put(ctx, e); err == nil {
				t.Errorf("Put(%+v) should fail", e)
			}
		}
	})

	t.Run("ZeroTimesRoundTripAsZero", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if err := s.Put(ctx, ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: base}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "1")
		if err != nil {
			t.Fatal(err)
		}
		if !got.SentAt.IsZero() || !got.NextRetryAt.IsZero() {
			t.Fatalf("unset times should stay zero: %+v", got)
		}
	})

	t.Run("ListFiltersByStatusAndOrdersByDetectedAt", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		mustPut(t, s,
			ledger.Entry{ID: "new", Status: ledger.StatusPending, DetectedAt: base.Add(2 * time.Hour)},
			ledger.Entry{ID: "old", Status: ledger.StatusPending, DetectedAt: base},
			ledger.Entry{ID: "mid", Status: ledger.StatusPending, DetectedAt: base.Add(time.Hour)},
			ledger.Entry{ID: "done", Status: ledger.StatusDone, DetectedAt: base},
			ledger.Entry{ID: "gaveup", Status: ledger.StatusGaveUp, DetectedAt: base},
		)

		got, err := s.List(ctx, ledger.Query{Status: ledger.StatusPending, OrderBy: ledger.OrderDetectedAt})
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, got, "old", "mid", "new")
	})

	t.Run("ListRespectsLimit", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		mustPut(t, s,
			ledger.Entry{ID: "a", Status: ledger.StatusPending, DetectedAt: base},
			ledger.Entry{ID: "b", Status: ledger.StatusPending, DetectedAt: base.Add(time.Minute)},
		)

		got, err := s.List(ctx, ledger.Query{Status: ledger.StatusPending, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, got, "a")
	})

	t.Run("ListFiltersByNextRetryBefore", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		mustPut(t, s,
			ledger.Entry{ID: "due-2", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: base.Add(-time.Minute)},
			ledger.Entry{ID: "due-1", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: base.Add(-time.Hour)},
			ledger.Entry{ID: "due-now", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: base},
			ledger.Entry{ID: "later", Status: ledger.StatusFailed, Attempts: 1, NextRetryAt: base.Add(time.Hour)},
		)

		got, err := s.List(ctx, ledger.Query{
			Status:          ledger.StatusFailed,
			NextRetryBefore: base,
			OrderBy:         ledger.OrderNextRetryAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, got, "due-1", "due-2", "due-now")
	})

	t.Run("ListEmptyIsNotAnError", func(t *testing.T) {
		s := newStore(t)
		got, err := s.List(context.Background(), ledger.Query{Status: ledger.StatusPending})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d entries, want 0", len(got))
		}
	})
}

func mustPut(t *testing.T, s ledger.Store, entries ...ledger.Entry) {
	t.Helper()
	for _, e := range entries {
		if err := s.Put(context.Background(), e); err != nil {
			t.Fatalf("Put(%s): %v", e.ID, err)
		}
	}
}

func assertIDs(t *testing.T, got []ledger.Entry, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %s, want %v", ids(got), want)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("got %s, want %v", ids(got), want)
		}
	}
}

func ids(entries []ledger.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func assertEntryEqual(t *testing.T, got, want ledger.Entry) {
	t.Helper()
	if got.ID != want.ID || got.Status != want.Status || got.Attempts != want.Attempts {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.PropertyName != want.PropertyName || got.RoomNumber != want.RoomNumber {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.LastError != want.LastError || got.JobID != want.JobID {
		t.Errorf("got %+v, want %+v", got, want)
	}
	for _, p := range []struct {
		name      string
		got, want time.Time
	}{
		{"DetectedAt", got.DetectedAt, want.DetectedAt},
		{"SentAt", got.SentAt, want.SentAt},
		{"NextRetryAt", got.NextRetryAt, want.NextRetryAt},
		{"UpdatedAt", got.UpdatedAt, want.UpdatedAt},
	} {
		if !p.got.Equal(p.want) {
			t.Errorf("%s = %v, want %v", p.name, p.got, p.want)
		}
	}
}
