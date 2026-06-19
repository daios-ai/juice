package native

import (
	"strings"
	"testing"
)

// TestPrepareSourceStripsGeneratedPackageAndImports guards the assembly that broke a
// real synthesis: the SDK owns package+imports, so any package/import the model emits
// must be stripped, or TinyGo rejects the file with "imports must appear before other
// declarations".
func TestPrepareSourceStripsGeneratedPackageAndImports(t *testing.T) {
	sdk := "package main\n\nimport \"encoding/json\"\n\nfunc mustMarshal(v any) []byte { _ = json.Marshal; return nil }"
	gen := "package main\n\nimport (\n\t\"fmt\"\n\t\"bytes\"\n)\nimport \"errors\"\n\nfunc Handle(in map[string]any) (map[string]any, error) {\n\treturn in, nil\n}"
	out := prepareSource(sdk, gen)

	for _, banned := range []string{`"fmt"`, `"bytes"`, "import \"errors\""} {
		if strings.Contains(out, banned) {
			t.Errorf("generated import %q was not stripped:\n%s", banned, out)
		}
	}
	if n := strings.Count(out, "package main"); n != 1 {
		t.Errorf("want exactly one package declaration, got %d", n)
	}
	if !strings.Contains(out, "func Handle(") || !strings.Contains(out, "mustMarshal") {
		t.Errorf("expected SDK + Handle body in output:\n%s", out)
	}
	// No import may appear after the first declaration (the placement error).
	firstFunc := strings.Index(out, "func ")
	if i := strings.Index(out, "\nimport "); firstFunc >= 0 && i > firstFunc {
		t.Errorf("import appears after a declaration (would not compile):\n%s", out)
	}
}

// TestRetryJSON covers the bounded retry that shields a make repair step from a
// transient example-generation failure (truncated JSON / slow model).
func TestRetryJSON(t *testing.T) {
	// Succeeds on the first try — no extra calls.
	calls := 0
	v, diag := retryJSON(3, func() (any, string) { calls++; return "ok", "" })
	if diag != "" || v != "ok" || calls != 1 {
		t.Fatalf("first-try success: v=%v diag=%q calls=%d", v, diag, calls)
	}

	// Fails twice, then succeeds — returns the recovered value.
	calls = 0
	v, diag = retryJSON(3, func() (any, string) {
		calls++
		if calls < 3 {
			return nil, "transient"
		}
		return "recovered", ""
	})
	if diag != "" || v != "recovered" || calls != 3 {
		t.Fatalf("retry-then-succeed: v=%v diag=%q calls=%d", v, diag, calls)
	}

	// Exhausts all attempts — returns the last diagnostic.
	calls = 0
	_, diag = retryJSON(2, func() (any, string) { calls++; return nil, "always" })
	if diag != "always" || calls != 2 {
		t.Fatalf("exhaust: diag=%q calls=%d", diag, calls)
	}

	// Non-positive attempts means a single call.
	calls = 0
	_, _ = retryJSON(0, func() (any, string) { calls++; return nil, "x" })
	if calls != 1 {
		t.Fatalf("zero attempts should call once, got %d", calls)
	}
}
