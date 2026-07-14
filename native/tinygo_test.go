package native

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/script"
)

// fakeCompileScripts implements kernel.ScriptExecutor + kernel.WASMInspector with
// configurable imports/exports, for exercising the import/export validation path.
type fakeCompileScripts struct {
	imports []kernel.WASMImport
	exports []string
}

func (f *fakeCompileScripts) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	return src, "fake-hash", nil
}
func (f *fakeCompileScripts) Execute(_ context.Context, _, _ []byte, _ kernel.HostFunctions) ([]byte, error) {
	return []byte("{}"), nil
}
func (f *fakeCompileScripts) InspectWASM(_ []byte) ([]kernel.WASMImport, []string, error) {
	return f.imports, f.exports, nil
}

func TestTinyGoCompileSuccess(t *testing.T) {
	wasm := minimalEchoWASMBytes()
	deps := CompileDeps{
		Compiler: &script.FakeCompiler{Artifact: wasm},
		Scripts:  script.New(script.Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024}),
	}
	out, err := executeTinyGoCompile(context.Background(), map[string]any{
		"source": "func Handle(in map[string]any) (map[string]any, error) { return in, nil }",
	}, deps, script.TinyGoSDK)
	if err != nil {
		t.Fatalf("executeTinyGoCompile: %v", err)
	}
	if out["status"] != "success" {
		t.Fatalf("expected status=success, got %v (diagnostics %v)", out["status"], out["diagnostics"])
	}
	artB64, _ := out["artifact"].(string)
	decoded, decErr := base64.StdEncoding.DecodeString(artB64)
	if decErr != nil {
		t.Fatalf("artifact not valid base64: %v", decErr)
	}
	if string(decoded) != string(wasm) {
		t.Error("decoded artifact does not match compiler output")
	}
	if got, _ := out["artifact_hash"].(string); got != hashHex(wasm) {
		t.Errorf("artifact_hash mismatch: got %q", got)
	}
}

func TestTinyGoCompileEmptySource(t *testing.T) {
	deps := CompileDeps{Compiler: &script.FakeCompiler{}, Scripts: &fakeCompileScripts{exports: []string{"alloc", "run"}}}
	_, err := executeTinyGoCompile(context.Background(), map[string]any{"source": "   "}, deps, "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty source, got %v", err)
	}
}

func TestTinyGoCompileNoCompiler(t *testing.T) {
	_, err := executeTinyGoCompile(context.Background(), map[string]any{"source": "x"}, CompileDeps{}, "")
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState when compiler absent, got %v", err)
	}
}

func TestTinyGoCompileToolchainAbsent(t *testing.T) {
	// CompileSource returning ErrInvalidState (e.g. tinygo not on PATH) is a platform
	// misconfiguration: surfaced as a kernel error, not a charged status=failure.
	deps := CompileDeps{
		Compiler: &script.FakeCompiler{Err: kernel.ErrInvalidState.Wrap("tinygo binary not found in PATH")},
		Scripts:  &fakeCompileScripts{exports: []string{"alloc", "run"}},
	}
	_, err := executeTinyGoCompile(context.Background(), map[string]any{"source": "func Handle() {}"}, deps, "")
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState when toolchain absent, got %v", err)
	}
}

func TestTinyGoCompileCompileError(t *testing.T) {
	deps := CompileDeps{
		Compiler: &script.FakeCompiler{Err: kernel.ErrInvalidInput.Wrap("tinygo compile error: syntax")},
		Scripts:  &fakeCompileScripts{exports: []string{"alloc", "run"}},
	}
	out, err := executeTinyGoCompile(context.Background(), map[string]any{"source": "func Handle() {}"}, deps, "")
	if err != nil {
		t.Fatalf("unexpected kernel error: %v", err)
	}
	if out["status"] != "failure" {
		t.Fatalf("expected status=failure, got %v", out["status"])
	}
	diags, _ := out["diagnostics"].([]any)
	if len(diags) == 0 {
		t.Error("expected diagnostics on compile failure")
	}
}

func TestTinyGoCompileRejectsDisallowedImport(t *testing.T) {
	deps := CompileDeps{
		Compiler: &script.FakeCompiler{},
		Scripts: &fakeCompileScripts{
			imports: []kernel.WASMImport{{Module: "juice", Name: "emit"}},
			exports: []string{"alloc", "run"},
		},
	}
	out, err := executeTinyGoCompile(context.Background(), map[string]any{"source": "func Handle() {}"}, deps, "")
	if err != nil {
		t.Fatalf("unexpected kernel error: %v", err)
	}
	if out["status"] != "failure" {
		t.Fatalf("expected status=failure for disallowed import, got %v", out["status"])
	}
}

func minimalEchoWASMBytes() []byte {
	// FakeCompiler with no Artifact returns the package's minimal echo module.
	art, _, _ := (&script.FakeCompiler{}).CompileSource(context.Background(), []byte("x"))
	return art
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestPrepareSourceStripsGeneratedPackageAndImports guards the SDK+body assembly: the SDK
// owns package+imports, so any package/import in the author's body must be stripped, or
// TinyGo rejects the file with "imports must appear before other declarations".
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
