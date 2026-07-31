package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
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

// TestErrorLine pins the red/plain formatting of the one-line error.
func TestErrorLine(t *testing.T) {
	if got := errorLine("boom", false); got != "error: boom" {
		t.Errorf("plain: got %q", got)
	}
	if got := errorLine("boom", true); got != "\x1b[31merror: boom\x1b[0m" {
		t.Errorf("colored: got %q", got)
	}
}

// TestUsageShownOnlyForParseErrors pins the mechanism main relies on: PersistentPreRun runs
// only after flag/argument validation passes, so an arg error never enters the command body
// (main then shows usage), while a runtime error does (main suppresses usage). Uses a local
// cobra tree to avoid the global rootCmd's initConfig side effects.
func TestUsageShownOnlyForParseErrors(t *testing.T) {
	build := func() (*cobra.Command, *bool) {
		entered := false
		root := &cobra.Command{
			Use: "t", SilenceUsage: true, SilenceErrors: true,
			PersistentPreRun: func(_ *cobra.Command, _ []string) { entered = true },
		}
		sub := &cobra.Command{
			Use: "deposit <user> <amount>", Args: cobra.ExactArgs(2),
			RunE: func(_ *cobra.Command, _ []string) error { return errors.New("runtime failure") },
		}
		root.AddCommand(sub)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		return root, &entered
	}

	// Argument error: body never runs, so main would show usage.
	root, entered := build()
	root.SetArgs([]string{"deposit"})
	cmd, err := root.ExecuteC()
	if err == nil {
		t.Fatal("expected an argument error")
	}
	if *entered {
		t.Fatal("arg error must not enter the command body")
	}
	if cmd.UsageString() == "" {
		t.Fatal("usage string should be available to print")
	}

	// Runtime error: body ran, so main would suppress usage.
	root, entered = build()
	root.SetArgs([]string{"deposit", "alice", "5"})
	if _, err := root.ExecuteC(); err == nil {
		t.Fatal("expected a runtime error")
	}
	if !*entered {
		t.Fatal("runtime error must have entered the command body")
	}
}

func TestMain(m *testing.M) {
	kernel.SetBcryptCostForTesting(4)
	kernel.SetMinPasswordLenForTesting(1)
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
		t.Errorf("file: got %q, err %v", got, err)
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

// stubPasswordPrompts replaces promptPassword with a stub that returns the given
// entries in order (and errs once exhausted), restoring the original on cleanup.
// Each promptNewPassword call consumes two entries: the value and its confirmation.
func stubPasswordPrompts(t *testing.T, entries ...string) {
	t.Helper()
	orig := promptPassword
	i := 0
	promptPassword = func(string) (string, error) {
		if i >= len(entries) {
			return "", errors.New("no more scripted password entries")
		}
		v := entries[i]
		i++
		return v, nil
	}
	t.Cleanup(func() { promptPassword = orig })
}

func TestPromptNewPassword(t *testing.T) {
	// Matching entries return the password.
	stubPasswordPrompts(t, "s3cret", "s3cret")
	got, err := promptNewPassword("Password: ")
	if err != nil || got != "s3cret" {
		t.Fatalf("match: got %q, err %v; want s3cret, nil", got, err)
	}

	// Mismatched entries are rejected as invalid input, not returned.
	stubPasswordPrompts(t, "s3cret", "typo")
	if _, err := promptNewPassword("Password: "); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("mismatch: want ErrInvalidInput, got %v", err)
	}

	// A read error from the underlying prompt propagates.
	readErr := errors.New("read failed")
	orig := promptPassword
	promptPassword = func(string) (string, error) { return "", readErr }
	t.Cleanup(func() { promptPassword = orig })
	if _, err := promptNewPassword("Password: "); !errors.Is(err, readErr) {
		t.Fatalf("read error: want propagated, got %v", err)
	}
}

// TestDBPathResolution pins the DB-path precedence: explicit --db wins, else the fixed
// per-user default $JUICE_HOME/kernel/juice.db (JUICE_HOME defaulting to ~/.juice), never the
// working directory. The fixed default is what stops `juice serve` from silently minting a new
// kernel identity when run from an unexpected folder. initConfig co-locates the config beside
// the DB and creates the home directory, so it is exercised end-to-end here against a temp HOME.
func TestDBPathResolution(t *testing.T) {
	home := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	// Isolate JUICE_HOME for the whole test; each case sets it explicitly.
	origJH, hadJH := os.LookupEnv("JUICE_HOME")
	os.Unsetenv("JUICE_HOME")
	t.Cleanup(func() {
		if hadJH {
			os.Setenv("JUICE_HOME", origJH)
		} else {
			os.Unsetenv("JUICE_HOME")
		}
	})

	// defaultDBPath is $JUICE_HOME/kernel/juice.db, i.e. ~/.juice/kernel/juice.db by default.
	want := filepath.Join(home, ".juice", "kernel", "juice.db")
	if got := defaultDBPath(); got != want {
		t.Fatalf("defaultDBPath: got %q, want %q", got, want)
	}

	// Save/restore the globals initConfig mutates.
	origDB, origConfig, origResolved := flagDB, flagConfig, resolvedConfigPath
	t.Cleanup(func() { flagDB, flagConfig, resolvedConfigPath = origDB, origConfig, origResolved })

	alt := t.TempDir() // stands in for a JUICE_HOME override root

	cases := []struct {
		name   string
		flag   string
		jhome  string // JUICE_HOME override ("" = unset, falls back to ~/.juice)
		want   string
		config string // expected co-located config path
	}{
		{"default", "", "", want, filepath.Join(home, ".juice", "kernel", "config.json")},
		{"JUICE_HOME override", "", alt, filepath.Join(alt, "kernel", "juice.db"), filepath.Join(alt, "kernel", "config.json")},
		{"flag wins over JUICE_HOME", filepath.Join(home, "flag", "f.db"), alt, filepath.Join(home, "flag", "f.db"), filepath.Join(home, "flag", "config.json")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flagDB, flagConfig = tc.flag, ""
			if tc.jhome == "" {
				os.Unsetenv("JUICE_HOME")
			} else {
				os.Setenv("JUICE_HOME", tc.jhome)
			}
			initConfig()
			if flagDB != tc.want {
				t.Errorf("flagDB: got %q, want %q", flagDB, tc.want)
			}
			if resolvedConfigPath != tc.config {
				t.Errorf("config path: got %q, want %q", resolvedConfigPath, tc.config)
			}
			if _, err := os.Stat(filepath.Dir(tc.want)); err != nil {
				t.Errorf("home dir not created: %v", err)
			}
		})
	}
}

// TestJuiceHomeResolution pins the root resolution: JUICE_HOME is honored verbatim, with
// kernel/ and cache/ hanging off it; unset falls back to ~/.juice. The fallback is fixed and
// absolute so state never lands in the working directory.
func TestJuiceHomeResolution(t *testing.T) {
	origJH, hadJH := os.LookupEnv("JUICE_HOME")
	t.Cleanup(func() {
		if hadJH {
			os.Setenv("JUICE_HOME", origJH)
		} else {
			os.Unsetenv("JUICE_HOME")
		}
	})

	root := t.TempDir()
	os.Setenv("JUICE_HOME", root)
	if got, want := juiceHome(), root; got != want {
		t.Errorf("juiceHome: got %q, want %q", got, want)
	}
	if got, want := kernelHome(), filepath.Join(root, "kernel"); got != want {
		t.Errorf("kernelHome: got %q, want %q", got, want)
	}
	if got, want := cacheDir(), filepath.Join(root, "kernel", "cache"); got != want {
		t.Errorf("cacheDir: got %q, want %q", got, want)
	}

	os.Unsetenv("JUICE_HOME")
	hdir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", hdir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })
	if got, want := juiceHome(), filepath.Join(hdir, ".juice"); got != want {
		t.Errorf("juiceHome fallback: got %q, want %q", got, want)
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
