package ledger

import (
	"context"
	"testing"
	"time"
)

func TestEntityRoundTrip(t *testing.T) {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	in := Entry{
		ID:          "1234567",
		Status:      StatusFailed,
		Attempts:    2,
		DetectedAt:  base,
		SentAt:      base.Add(time.Minute),
		NextRetryAt: base.Add(time.Hour),
		LastError:   "HTTP 500",
		JobID:       "job-1",
		UpdatedAt:   base.Add(time.Minute),
	}

	got := fromEntity(in.ID, toEntity(in))
	if got.ID != in.ID || got.Status != in.Status || got.Attempts != in.Attempts {
		t.Errorf("got %+v, want %+v", got, in)
	}
	if got.LastError != in.LastError || got.JobID != in.JobID {
		t.Errorf("got %+v, want %+v", got, in)
	}
	if !got.DetectedAt.Equal(in.DetectedAt) || !got.SentAt.Equal(in.SentAt) ||
		!got.NextRetryAt.Equal(in.NextRetryAt) || !got.UpdatedAt.Equal(in.UpdatedAt) {
		t.Errorf("timestamps differ: got %+v, want %+v", got, in)
	}
}

func TestEntityOmitsZeroTimes(t *testing.T) {
	ent := toEntity(Entry{ID: "1", Status: StatusPending})
	if ent.DetectedAt != nil || ent.SentAt != nil || ent.NextRetryAt != nil || ent.UpdatedAt != nil {
		t.Fatalf("zero times should map to nil: %+v", ent)
	}

	got := fromEntity("1", ent)
	if !got.DetectedAt.IsZero() || !got.SentAt.IsZero() || !got.NextRetryAt.IsZero() || !got.UpdatedAt.IsZero() {
		t.Fatalf("nil times should map back to zero: %+v", got)
	}
}

func TestEntityNormalizesToUTC(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	local := time.Date(2026, 8, 24, 19, 0, 0, 0, jst)
	ent := toEntity(Entry{ID: "1", Status: StatusPending, DetectedAt: local})
	if ent.DetectedAt.Location() != time.UTC {
		t.Fatalf("location = %v, want UTC", ent.DetectedAt.Location())
	}
	if !ent.DetectedAt.Equal(local) {
		t.Fatalf("instant changed: %v vs %v", ent.DetectedAt, local)
	}
}

func TestNewDatastoreStoreWithClientDefaults(t *testing.T) {
	s := NewDatastoreStoreWithClient(nil, DatastoreConfig{})
	if s.kind != DefaultKind {
		t.Errorf("kind = %q, want %q", s.kind, DefaultKind)
	}
	if s.namespace != "" {
		t.Errorf("namespace = %q, want empty", s.namespace)
	}

	s2 := NewDatastoreStoreWithClient(nil, DatastoreConfig{Kind: " my_kind ", Namespace: " ns "})
	if s2.kind != "my_kind" || s2.namespace != "ns" {
		t.Errorf("values should be trimmed: %+v", s2)
	}

	// クライアントを所有していないので Close は no-op。
	if err := s2.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestDatastoreKeyUsesNamespace(t *testing.T) {
	s := NewDatastoreStoreWithClient(nil, DatastoreConfig{Namespace: "contract-kicker"})
	k := s.key("1234567")
	if k.Kind != DefaultKind || k.Name != "1234567" || k.Namespace != "contract-kicker" {
		t.Fatalf("unexpected key: %+v", k)
	}
}

func TestGetRejectsEmptyID(t *testing.T) {
	s := NewDatastoreStoreWithClient(nil, DatastoreConfig{})
	if _, err := s.Get(context.Background(), "  "); err == nil {
		t.Fatal("empty id should be rejected before touching the client")
	}
}

func TestPutRejectsInvalidEntry(t *testing.T) {
	s := NewDatastoreStoreWithClient(nil, DatastoreConfig{})
	if err := s.Put(context.Background(), Entry{ID: "1", Status: "bogus"}); err == nil {
		t.Fatal("invalid entry should be rejected before touching the client")
	}
}
