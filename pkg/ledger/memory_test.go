package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger/ledgertest"
)

func TestMemoryStoreConformance(t *testing.T) {
	ledgertest.RunStoreTests(t, func(t *testing.T) ledger.Store {
		return ledger.NewMemoryStore()
	})
}

func TestMemoryStoreInjectedFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	s := ledger.NewMemoryStore()
	s.FailGet, s.FailPut, s.FailList = boom, boom, boom

	if _, err := s.Get(ctx, "1"); !errors.Is(err, boom) {
		t.Errorf("Get err = %v", err)
	}
	if err := s.Put(ctx, ledger.Entry{ID: "1", Status: ledger.StatusPending}); !errors.Is(err, boom) {
		t.Errorf("Put err = %v", err)
	}
	if _, err := s.List(ctx, ledger.Query{}); !errors.Is(err, boom) {
		t.Errorf("List err = %v", err)
	}
}

func TestMemoryStoreSeedAndLen(t *testing.T) {
	s := ledger.NewMemoryStore()
	s.Seed(
		ledger.Entry{ID: "1", Status: ledger.StatusPending},
		ledger.Entry{ID: "2", Status: ledger.StatusDone},
	)
	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestMemoryStoreListWithoutStatusReturnsAll(t *testing.T) {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	s := ledger.NewMemoryStore()
	s.Seed(
		ledger.Entry{ID: "1", Status: ledger.StatusPending, DetectedAt: base},
		ledger.Entry{ID: "2", Status: ledger.StatusDone, DetectedAt: base.Add(time.Minute)},
	)

	got, err := s.List(context.Background(), ledger.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestMemoryStoreIsGoroutineSafe(t *testing.T) {
	s := ledger.NewMemoryStore()
	ctx := context.Background()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = s.Put(ctx, ledger.Entry{ID: "1", Status: ledger.StatusPending})
		}
	}()
	for i := 0; i < 200; i++ {
		_, _ = s.List(ctx, ledger.Query{Status: ledger.StatusPending})
		_, _ = s.Get(ctx, "1")
	}
	<-done
}

func TestStatusHelpers(t *testing.T) {
	if !ledger.StatusDone.Terminal() || !ledger.StatusGaveUp.Terminal() {
		t.Error("done/gaveup should be terminal")
	}
	if ledger.StatusPending.Terminal() || ledger.StatusFailed.Terminal() {
		t.Error("pending/failed should not be terminal")
	}
	if ledger.Status("bogus").Valid() {
		t.Error("unknown status should be invalid")
	}
	for _, s := range []ledger.Status{ledger.StatusPending, ledger.StatusDone, ledger.StatusFailed, ledger.StatusGaveUp} {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
}
