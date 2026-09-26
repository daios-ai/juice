// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

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
	kerr := kernel.ErrInvalidState.Wrap("cannot reach juice server (is `juice kernel serve` running?)").Because(cause)

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
			Use: "deposit USER AMOUNT", Args: cobra.ExactArgs(2),
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

// TestUseLinesUseUppercaseMetavariables enforces C14's notation on the real command tree:
// Use lines carry uppercase metavariables, never the old <angle-bracket> form.
func TestUseLinesUseUppercaseMetavariables(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if strings.Contains(c.Use, "<") {
			t.Errorf("command %q: Use line %q contains '<'; metavariables are uppercase (API.md C14)", c.CommandPath(), c.Use)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}

// renderHelp renders c's help page as `juice … --help` prints it, at the given width.
func renderHelp(t *testing.T, c *cobra.Command, width int) string {
	t.Helper()
	orig := helpWidth
	helpWidth = func() int { return width }
	defer func() { helpWidth = orig }()
	var buf bytes.Buffer
	c.SetOut(&buf)
	defer c.SetOut(nil)
	if err := c.Help(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestHelpFitsWidth holds every help page to its width. At 80 every line fits; narrower, what is
// wrapped fits, while usage lines, examples and a description's indented lines stay as written and
// a word longer than the width stays whole. 60 is narrow yet above where pflag's flag wrapping gives
// out (below 51 columns here, when one word fills a flag description's room).
func TestHelpFitsWidth(t *testing.T) {
	for _, width := range []int{maxHelpWidth, 60} {
		var walk func(c *cobra.Command)
		walk = func(c *cobra.Command) {
			section := "description"
			for _, line := range strings.Split(renderHelp(t, c, width), "\n") {
				switch line {
				case "Usage:", "Aliases:", "Examples:", "Available Commands:", "Flags:", "Global Flags:":
					section = line
				}
				if utf8.RuneCountInString(line) <= width {
					continue
				}
				verbatim := section == "Usage:" || section == "Examples:" || (section == "description" && strings.HasPrefix(line, " "))
				if width < maxHelpWidth && (verbatim || len(strings.Fields(line)) == 1) {
					continue
				}
				t.Errorf("%q at width %d: line of %d columns: %q", c.CommandPath(), width, utf8.RuneCountInString(line), line)
			}
			for _, sub := range c.Commands() {
				walk(sub)
			}
		}
		walk(rootCmd)
	}
}

// TestHelpTextIsParagraphs keeps descriptions as unbroken paragraphs, which help wraps to the
// terminal: a line break inside a paragraph would go ragged in any narrower window. Only a blank
// line or an indented line (a list, an aligned table) may follow a break.
func TestHelpTextIsParagraphs(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		lines := strings.Split(c.Long, "\n")
		for i := 1; i < len(lines); i++ {
			if lines[i-1] != "" && lines[i] != "" && lines[i][0] != ' ' {
				t.Errorf("%q: description breaks a paragraph before %q", c.CommandPath(), lines[i])
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}

func TestWrapText(t *testing.T) {
	for _, tc := range []struct {
		name, in      string
		width, indent int
		want          string
	}{
		{"fills to the width", "one two three four", 9, 0, "one two\nthree\nfour"},
		{"counts runes, not bytes", "a — b — c", 5, 0, "a — b\n— c"},
		{"indents what follows the first line", "one two three", 9, 4, "one\n    two\n    three"},
		{"keeps blank and indented lines", "one two\n\n  x y z w\nthree", 7, 0, "one two\n\n  x y z w\nthree"},
		{"keeps a long word whole", "a verylongword b", 5, 0, "a\nverylongword\nb"},
	} {
		if got := wrapText(tc.in, tc.width, tc.indent); got != tc.want {
			t.Errorf("%s: wrapText(%q, %d, %d) = %q, want %q", tc.name, tc.in, tc.width, tc.indent, got, tc.want)
		}
	}
}

// TestHelpWidth: the terminal's width capped at 80, stderr's when stdout is not a terminal (the
// usage after a mistyped command, or help piped into a pager), and 80 with no terminal at all.
func TestHelpWidth(t *testing.T) {
	stdout, stderr := int(os.Stdout.Fd()), int(os.Stderr.Fd())
	for _, tc := range []struct {
		name  string
		sizes map[int]int
		want  int
	}{
		{"narrow stdout", map[int]int{stdout: 60, stderr: 70}, 60},
		{"wide stdout is capped", map[int]int{stdout: 200}, maxHelpWidth},
		{"stderr when stdout is not a terminal", map[int]int{stderr: 50}, 50},
		{"no terminal", map[int]int{}, maxHelpWidth},
		{"a terminal reporting no width", map[int]int{stdout: 0}, maxHelpWidth},
	} {
		got := widthOf(func(fd int) (int, int, error) {
			if w, ok := tc.sizes[fd]; ok {
				return w, 24, nil
			}
			return 0, 0, errors.New("not a terminal")
		})
		if got != tc.want {
			t.Errorf("%s: width %d, want %d", tc.name, got, tc.want)
		}
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
	selectTestLogin(t, "tester@k", "http://kernel:4040")

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
	if got, want := kernelHome(), filepath.Join(root, "kernels", worldName); got != want {
		t.Errorf("kernelHome: got %q, want %q", got, want)
	}
	if got, want := cacheDir(), filepath.Join(root, "kernels", worldName, "cache"); got != want {
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
	selectTestLogin(t, "tester@k", "http://kernel:4040")

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
