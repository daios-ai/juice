package script

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestInspectModuleEchoWASM(t *testing.T) {
	imports, exports, err := InspectModule(echoWASM)
	if err != nil {
		t.Fatalf("InspectModule: %v", err)
	}
	if len(imports) != 0 {
		t.Errorf("echoWASM should have no imports, got %+v", imports)
	}
	hasAlloc, hasRun := false, false
	for _, e := range exports {
		if e == "alloc" {
			hasAlloc = true
		}
		if e == "run" {
			hasRun = true
		}
	}
	if !hasAlloc {
		t.Error("echoWASM should export alloc")
	}
	if !hasRun {
		t.Error("echoWASM should export run")
	}
}

func TestInspectModuleEmitCallerWASM(t *testing.T) {
	imports, _, err := InspectModule(emitCallerWASM)
	if err != nil {
		t.Fatalf("InspectModule: %v", err)
	}
	found := false
	for _, imp := range imports {
		if imp.Module == "juice" && imp.Name == "emit" {
			found = true
		}
	}
	if !found {
		t.Errorf("emitCallerWASM should import juice.emit, got %+v", imports)
	}
}

func TestInspectModuleInvalidBytes(t *testing.T) {
	_, _, err := InspectModule([]byte("not wasm"))
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for invalid WASM, got %v", err)
	}
}

func TestFakeCompilerDefaultsToMinimalEchoWASM(t *testing.T) {
	f := &FakeCompiler{}
	art, hash, err := f.CompileSource(context.Background(), []byte("package main"))
	if err != nil {
		t.Fatalf("FakeCompiler: %v", err)
	}
	if len(art) == 0 {
		t.Error("expected non-empty artifact")
	}
	if hash == "" {
		t.Error("expected non-empty hash")
	}
	// Must be valid WASM that the executor can compile.
	e := New(Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	if _, _, err := e.Compile(context.Background(), art); err != nil {
		t.Errorf("FakeCompiler output should be valid WASM: %v", err)
	}
}

func TestFakeCompilerCustomArtifact(t *testing.T) {
	f := &FakeCompiler{Artifact: echoWASM}
	art, _, err := f.CompileSource(context.Background(), []byte("source"))
	if err != nil {
		t.Fatal(err)
	}
	if string(art) != string(echoWASM) {
		t.Error("expected custom artifact to be returned")
	}
}

func TestFakeCompilerError(t *testing.T) {
	f := &FakeCompiler{Err: kernel.ErrInvalidInput.Wrap("bad source")}
	_, _, err := f.CompileSource(context.Background(), []byte("bad"))
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestTinyGoCompilerAbsent(t *testing.T) {
	// Override PATH so tinygo is not found.
	t.Setenv("PATH", t.TempDir())
	c := NewTinyGoCompiler(CompileConfig{})
	_, _, err := c.CompileSource(context.Background(), []byte("package main\nfunc main() {}"))
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState when tinygo absent, got %v", err)
	}
}

func TestSDKEmbedded(t *testing.T) {
	if TinyGoSDK == "" {
		t.Error("TinyGoSDK must be non-empty")
	}
	if len(TinyGoSDK) < 100 {
		t.Errorf("TinyGoSDK suspiciously short (%d bytes)", len(TinyGoSDK))
	}
}

func TestMagicByteDetection(t *testing.T) {
	// WASM magic bytes → compileWasmSource should pass directly to ScriptExecutor.
	if len(minimalEchoWASM) < 4 {
		t.Fatal("minimalEchoWASM too short")
	}
	if minimalEchoWASM[0] != 0x00 || minimalEchoWASM[1] != 0x61 || minimalEchoWASM[2] != 0x73 || minimalEchoWASM[3] != 0x6d {
		t.Error("minimalEchoWASM must start with WASM magic bytes")
	}
}
