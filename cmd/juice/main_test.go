package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = orig
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

// TestRenderError pins the single error-rendering contract: "error: <message>" to stderr,
// no usage dump, cause hidden by default and revealed by --verbose; plain errors still render.
func TestRenderError(t *testing.T) {
	t.Cleanup(func() { flagVerbose = false })
	cause := errors.New("dial tcp 127.0.0.1:4040: connect: connection refused")
	kerr := kernel.ErrInvalidState.Wrap("cannot reach juice server (is `juice serve` running?)").Because(cause)

	flagVerbose = false
	out := captureStderr(t, func() { renderError(kerr) })
	if !strings.HasPrefix(out, "error: cannot reach juice server") {
		t.Fatalf("want 'error: <message>', got %q", out)
	}
	if strings.Contains(out, "Usage:") || strings.Contains(out, "connection refused") {
		t.Fatalf("default render must omit usage and raw cause: %q", out)
	}

	flagVerbose = true
	out = captureStderr(t, func() { renderError(kerr) })
	if !strings.Contains(out, "connection refused") {
		t.Fatalf("--verbose should surface the cause: %q", out)
	}

	flagVerbose = false
	out = captureStderr(t, func() { renderError(errors.New("boom")) })
	if strings.TrimSpace(out) != "error: boom" {
		t.Fatalf("plain error render: %q", out)
	}
}

func TestMain(m *testing.M) {
	kernel.SetBcryptCostForTesting(4)
	os.Exit(m.Run())
}

func TestTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	if err := saveToken("tok123"); err != nil {
		t.Fatal(err)
	}
	got, err := loadToken()
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok123" {
		t.Errorf("loadToken: got %q, want %q", got, "tok123")
	}
	if err := removeToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(); err == nil {
		t.Error("expected error after removeToken")
	}
}

func TestLoadJSONArg(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "in.json")
	if err := os.WriteFile(file, []byte(`{"x":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Empty input defaults to an empty object.
	if got, err := loadJSONArg(""); err != nil || string(got) != "{}" {
		t.Errorf("empty: got %q, err %v; want {}", got, err)
	}

	// Inline JSON is passed through.
	if got, err := loadJSONArg(`{"a":2}`); err != nil || string(got) != `{"a":2}` {
		t.Errorf("inline: got %q, err %v", got, err)
	}

	// @file reads and validates the file contents.
	if got, err := loadJSONArg("@" + file); err != nil || string(got) != `{"x":1}` {
		t.Errorf("@file: got %q, err %v", got, err)
	}

	// Missing file is an error.
	if _, err := loadJSONArg("@" + filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file: expected error")
	}

	// Non-JSON inline input is rejected rather than shipped as literal bytes.
	if _, err := loadJSONArg("not json"); err == nil {
		t.Error("garbage: expected error")
	}
}

func TestRefreshTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	if err := saveRefreshToken("rt456"); err != nil {
		t.Fatal(err)
	}
	got, err := loadRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if got != "rt456" {
		t.Errorf("loadRefreshToken: got %q, want %q", got, "rt456")
	}
	if err := removeRefreshToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRefreshToken(); err == nil {
		t.Error("expected error after removeRefreshToken")
	}
}
