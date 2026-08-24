package env

import (
	"strings"
	"testing"
	"time"
)

func TestStringAndRequire(t *testing.T) {
	l := NewFromMap(map[string]string{"A": "va", "BLANK": "   "})

	if got := l.String("A", "def"); got != "va" {
		t.Errorf("String(A) = %q, want va", got)
	}
	if got := l.String("MISSING", "def"); got != "def" {
		t.Errorf("String(MISSING) = %q, want def", got)
	}
	if got := l.String("BLANK", "def"); got != "def" {
		t.Errorf("blank value should fall back to default, got %q", got)
	}
	if got := l.Require("A"); got != "va" {
		t.Errorf("Require(A) = %q, want va", got)
	}
	if err := l.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	l.Require("MISSING")
	err := l.Err()
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("Require(MISSING) should error, got %v", err)
	}
}

func TestIntBoolDuration(t *testing.T) {
	l := NewFromMap(map[string]string{
		"N":       "42",
		"FLAG":    "TRUE",
		"FLAG2":   "no",
		"TIMEOUT": "1500",
		"WINDOW":  "24h",
	})

	if got := l.Int("N", 1); got != 42 {
		t.Errorf("Int(N) = %d, want 42", got)
	}
	if got := l.Int("NOPE", 7); got != 7 {
		t.Errorf("Int(NOPE) = %d, want 7", got)
	}
	if !l.Bool("FLAG", false) {
		t.Error("Bool(FLAG) should be true")
	}
	if l.Bool("FLAG2", true) {
		t.Error("Bool(FLAG2) should be false")
	}
	if got := l.Millis("TIMEOUT", time.Second); got != 1500*time.Millisecond {
		t.Errorf("Millis(TIMEOUT) = %s, want 1.5s", got)
	}
	if got := l.Duration("WINDOW", time.Hour); got != 24*time.Hour {
		t.Errorf("Duration(WINDOW) = %s, want 24h", got)
	}
	if err := l.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestErrorsAreAccumulated(t *testing.T) {
	l := NewFromMap(map[string]string{
		"N":       "abc",
		"FLAG":    "maybe",
		"TIMEOUT": "-5",
		"WINDOW":  "24 hours",
		"SCHED":   "15m,oops",
		"COUNT":   "0",
	})

	l.Int("N", 1)
	l.Bool("FLAG", false)
	l.Millis("TIMEOUT", time.Second)
	l.Duration("WINDOW", time.Hour)
	l.DurationList("SCHED", []time.Duration{time.Minute})
	l.PositiveInt("COUNT", 1)
	l.Require("MISSING")

	err := l.Err()
	if err == nil {
		t.Fatal("expected accumulated errors")
	}
	for _, key := range []string{"N", "FLAG", "TIMEOUT", "WINDOW", "SCHED", "COUNT", "MISSING"} {
		if !strings.Contains(err.Error(), "env "+key) {
			t.Errorf("error should mention %s: %v", key, err)
		}
	}
}

func TestDurationList(t *testing.T) {
	l := NewFromMap(map[string]string{"SCHED": " 15m , 1h,4h "})
	got := l.DurationList("SCHED", nil)
	want := []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if err := l.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	def := []time.Duration{time.Minute}
	if got := l.DurationList("NOPE", def); len(got) != 1 || got[0] != time.Minute {
		t.Fatalf("default not returned: %v", got)
	}
}

func TestNewUsesProcessEnv(t *testing.T) {
	t.Setenv("ENV_PKG_TEST_KEY", "hello")
	if got := New().String("ENV_PKG_TEST_KEY", "def"); got != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}
