package log

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ansiRE strips terminal color escape sequences so assertions can match plain text.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestNewAndLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		l, err := New(Config{Level: level, Format: "json"})
		if err != nil {
			t.Fatalf("New level=%q: %v", level, err)
		}
		if l == nil {
			t.Errorf("New level=%q returned nil logger", level)
		}
	}
}

func TestDefault(t *testing.T) {
	if Default() == nil {
		t.Error("Default() returned nil")
	}
}

func TestDiscard(t *testing.T) {
	l := Discard()
	if l == nil {
		t.Error("Discard() returned nil")
	}
	// Should not panic.
	l.Debug("discard.debug")
	l.Info("discard.info")
	l.Warn("discard.warn")
	l.Error("discard.error")
}

func TestFileOutput(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "test.log")
	l, err := New(Config{Level: "info", FilePath: logPath, Format: "text"})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ctx = WithRequestID(ctx, "req-abc")
	l.With(ctx).Info("test.event", "key", "value")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "test.event") {
		t.Errorf("log file missing event; content: %s", string(data))
	}
	if !strings.Contains(string(data), "req-abc") {
		t.Errorf("log file missing request_id; content: %s", string(data))
	}
}

func TestContextFields(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ctx.log")
	l, err := New(Config{Level: "info", FilePath: logPath, Format: "json"})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ctx = WithRequestID(ctx, "rid-1")
	ctx = WithCallerUserID(ctx, "uid-2")
	ctx = WithProcessID(ctx, "pid-3")
	ctx = WithTraceID(ctx, "tid-4")
	ctx = WithActionID(ctx, "aid-5")
	ctx = WithTxID(ctx, "txid-6")
	l.With(ctx).Info("ctx.test")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{"rid-1", "uid-2", "pid-3", "tid-4", "aid-5", "txid-6"} {
		if !strings.Contains(s, want) {
			t.Errorf("log missing field value %q; content: %s", want, s)
		}
	}
}

func TestWithEmptyContext(t *testing.T) {
	l := Discard()
	// With an empty context should return the same logger (no panic).
	l2 := l.With(context.Background())
	if l2 == nil {
		t.Error("With(empty ctx) returned nil")
	}
}

func TestTerminalWritesToStderr(t *testing.T) {
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	l, _ := New(Config{Level: "info", Format: "text"})
	l.Info("stderr.test.event")

	w.Close()
	os.Stderr = origStderr

	var buf bytes.Buffer
	io.Copy(&buf, r)
	if !strings.Contains(buf.String(), "stderr.test.event") {
		t.Errorf("expected log output on stderr, got: %q", buf.String())
	}
}

func TestShortID(t *testing.T) {
	cases := map[string]string{
		"e32c2802-38b8-494e-9068-b600cc16890a": "e32c2802", // UUID → first group
		"short":                                "short",    // already short, unchanged
		"":                                     "",
		"12345678":                             "12345678", // exactly 8, unchanged
		"123456789":                            "12345678", // 9 → truncated
	}
	for in, want := range cases {
		if got := shortID(in); got != want {
			t.Errorf("shortID(%q) = %q, want %q", in, got, want)
		}
	}
}

// attrsToMap renders a transformed attr batch into key→string-value for assertions.
func attrsToMap(attrs []slog.Attr) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value.String()
	}
	return m
}

func TestTransformAttrs(t *testing.T) {
	full := "e32c2802-38b8-494e-9068-b600cc16890a"

	t.Run("renames, truncation, and action_id drop", func(t *testing.T) {
		out := attrsToMap(transformAttrs([]slog.Attr{
			slog.String("request_id", full),
			slog.String("process_id", full),
			slog.String("trace_id", full),
			slog.String("tx_id", full),
			slog.String("action_id", full),
			slog.String("child_trace_id", full),
			slog.String("action", "llm/chat"),
			slog.Int("price", 10),
		}))
		if out["req"] != "e32c2802" || out["proc"] != "e32c2802" || out["trace"] != "e32c2802" || out["tx"] != "e32c2802" {
			t.Errorf("expected renamed+shortened ids, got %+v", out)
		}
		if _, ok := out["action_id"]; ok {
			t.Errorf("action_id should be dropped, got %+v", out)
		}
		if out["child_trace_id"] != "e32c2802" {
			t.Errorf("unmapped *_id key should be kept and shortened, got %q", out["child_trace_id"])
		}
		if out["action"] != "llm/chat" || out["price"] != "10" {
			t.Errorf("non-id fields must pass through unchanged, got %+v", out)
		}
	})

	t.Run("caller_handle replaces caller_user_id", func(t *testing.T) {
		out := attrsToMap(transformAttrs([]slog.Attr{
			slog.String("caller_user_id", full),
			slog.String("caller_handle", "@sys"),
		}))
		if out["caller"] != "@sys" {
			t.Errorf("expected caller=@sys, got %+v", out)
		}
		if _, ok := out["caller_user_id"]; ok {
			t.Errorf("caller_user_id should be dropped when handle present, got %+v", out)
		}
		if _, ok := out["caller_handle"]; ok {
			t.Errorf("caller_handle should be renamed to caller, got %+v", out)
		}
	})

	t.Run("falls back to short caller_user_id when no handle", func(t *testing.T) {
		out := attrsToMap(transformAttrs([]slog.Attr{
			slog.String("caller_user_id", full),
		}))
		if out["caller"] != "e32c2802" {
			t.Errorf("expected caller=<short id> fallback, got %+v", out)
		}
	})
}

// TestReadableConsoleVsFullFile proves the console output is shortened/handle-ified while
// the JSON log file retains full IDs and all fields.
func TestReadableConsoleVsFullFile(t *testing.T) {
	full := "e32c2802-38b8-494e-9068-b600cc16890a"
	logPath := filepath.Join(t.TempDir(), "console.log")

	// tint captures os.Stderr at New() time, so redirect before constructing.
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	l, err := New(Config{Level: "info", FilePath: logPath, Format: "text"})
	if err != nil {
		os.Stderr = origStderr
		t.Fatal(err)
	}

	ctx := context.Background()
	ctx = WithRequestID(ctx, full)
	ctx = WithCallerUserID(ctx, full)
	ctx = WithCallerHandle(ctx, "@sys")
	ctx = WithActionID(ctx, full)
	l.With(ctx).Info("call.start", "action", "llm/chat")

	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	io.Copy(&buf, r)
	console := ansiRE.ReplaceAllString(buf.String(), "")

	// Console: short ids, caller handle, no full uuids / action_id / caller_user_id.
	if !strings.Contains(console, "req=e32c2802") || !strings.Contains(console, "caller=@sys") {
		t.Errorf("console missing shortened req / caller handle: %q", console)
	}
	if strings.Contains(console, full) {
		t.Errorf("console must not contain full UUID: %q", console)
	}
	for _, bad := range []string{"action_id", "caller_user_id"} {
		if strings.Contains(console, bad) {
			t.Errorf("console must not contain %q: %q", bad, console)
		}
	}

	// File: full fidelity — full ids and all keys retained.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	file := string(data)
	if !strings.Contains(file, full) {
		t.Errorf("file must keep full UUID: %s", file)
	}
	for _, want := range []string{"caller_user_id", "action_id", "caller_handle"} {
		if !strings.Contains(file, want) {
			t.Errorf("file must keep field %q: %s", want, file)
		}
	}
}
