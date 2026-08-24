package retry

import (
	"testing"
	"time"
)

func TestDelaySaturatesAtBothEnds(t *testing.T) {
	b := Default()
	cases := map[int]time.Duration{
		-1: 15 * time.Minute,
		0:  15 * time.Minute,
		1:  15 * time.Minute,
		2:  time.Hour,
		3:  4 * time.Hour,
		9:  4 * time.Hour,
	}
	for attempt, want := range cases {
		if got := b.Delay(attempt); got != want {
			t.Errorf("Delay(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestZeroValueBackoffReturnsZero(t *testing.T) {
	var b Backoff
	if got := b.Delay(1); got != 0 {
		t.Fatalf("Delay on zero Backoff = %s, want 0", got)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Error("empty schedule should be rejected")
	}
	if _, err := New([]time.Duration{time.Minute, -time.Second}); err == nil {
		t.Error("negative duration should be rejected")
	}
	b, err := New([]time.Duration{time.Minute})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := b.Delay(5); got != time.Minute {
		t.Fatalf("Delay = %s, want 1m", got)
	}
}

func TestNewCopiesInput(t *testing.T) {
	in := []time.Duration{time.Minute}
	b, err := New(in)
	if err != nil {
		t.Fatal(err)
	}
	in[0] = time.Hour
	if got := b.Delay(1); got != time.Minute {
		t.Fatalf("Backoff should not alias caller slice, got %s", got)
	}
}

func TestDefaultCopiesGlobal(t *testing.T) {
	b := Default()
	b.Schedule[0] = time.Second
	if DefaultSchedule[0] != 15*time.Minute {
		t.Fatal("Default() must not share the global slice")
	}
}

func TestNextAt(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	b := Default()
	if got, want := b.NextAt(now, 2), now.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("NextAt = %v, want %v", got, want)
	}
}
