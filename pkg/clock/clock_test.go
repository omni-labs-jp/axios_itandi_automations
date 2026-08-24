package clock

import (
	"testing"
	"time"
)

func TestSystemReturnsMovingTime(t *testing.T) {
	c := System()
	before := time.Now()
	got := c.Now()
	if got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Fatalf("System().Now() = %v, out of expected range", got)
	}
}

func TestFake(t *testing.T) {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	f := NewFake(base)

	if got := f.Now(); !got.Equal(base) {
		t.Fatalf("Now() = %v, want %v", got, base)
	}

	f.Advance(90 * time.Minute)
	if want := base.Add(90 * time.Minute); !f.Now().Equal(want) {
		t.Fatalf("after Advance: Now() = %v, want %v", f.Now(), want)
	}

	f.Set(base)
	if !f.Now().Equal(base) {
		t.Fatalf("after Set: Now() = %v, want %v", f.Now(), base)
	}
}
