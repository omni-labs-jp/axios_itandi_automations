package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid JSON log %q: %v", b, err)
	}
	return m
}

func TestNewWritesCloudLoggingFields(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, Options{})

	log.Warn("send failed", "entryHeadId", "1234567", "statusCode", 500)

	m := decode(t, buf.Bytes())
	if m["severity"] != "WARNING" {
		t.Errorf("severity = %v, want WARNING", m["severity"])
	}
	if m["message"] != "send failed" {
		t.Errorf("message = %v, want 'send failed'", m["message"])
	}
	if m["entryHeadId"] != "1234567" {
		t.Errorf("entryHeadId = %v", m["entryHeadId"])
	}
	if _, ok := m["time"].(string); !ok {
		t.Errorf("time should be an RFC3339 string, got %v", m["time"])
	}
	if _, ok := m["level"]; ok {
		t.Error("slog's default 'level' key should be renamed to 'severity'")
	}
	if _, ok := m["msg"]; ok {
		t.Error("slog's default 'msg' key should be renamed to 'message'")
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, Options{Level: slog.LevelWarn})

	log.Info("dropped")
	if buf.Len() != 0 {
		t.Fatalf("info log should be filtered out, got %s", buf.String())
	}

	log.Error("kept")
	if m := decode(t, buf.Bytes()); m["severity"] != "ERROR" {
		t.Fatalf("severity = %v, want ERROR", m["severity"])
	}
}

func TestSeverity(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug:     "DEBUG",
		slog.LevelInfo:      "INFO",
		slog.LevelWarn:      "WARNING",
		slog.LevelError:     "ERROR",
		slog.LevelError + 4: "CRITICAL",
	}
	for lv, want := range cases {
		if got := Severity(lv); got != want {
			t.Errorf("Severity(%v) = %q, want %q", lv, got, want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"INFO":    slog.LevelInfo,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"garbage": slog.LevelInfo,
		"":        slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestGroupedAttrsAreNotRenamed(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, Options{})
	log.Info("x", slog.Group("g", slog.String("message", "inner")))

	m := decode(t, buf.Bytes())
	g, ok := m["g"].(map[string]any)
	if !ok {
		t.Fatalf("group missing: %v", m)
	}
	if g["message"] != "inner" {
		t.Errorf("nested attrs should be untouched: %v", g)
	}
}
