package log

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
