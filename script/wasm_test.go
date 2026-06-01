package script

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

func TestFakeExecutorEchoes(t *testing.T) {
	f := &FakeExecutor{}
	ctx := context.Background()

	src := []byte(`(module)`)
	artifact, hash, err := f.Compile(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact) == 0 || hash == "" {
		t.Error("expected non-empty artifact and hash")
	}

	input := []byte(`{"key":"value"}`)
	out, err := f.Execute(ctx, artifact, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(input) {
		t.Errorf("expected echo, got %s", out)
	}
}

func TestFakeExecutorCustomResult(t *testing.T) {
	f := &FakeExecutor{Result: []byte(`{"answer":42}`)}
	out, err := f.Execute(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"answer":42}` {
		t.Errorf("unexpected result: %s", out)
	}
}

func TestFakeExecutorError(t *testing.T) {
	f := &FakeExecutor{Err: fmt.Errorf("boom")}
	_, err := f.Execute(context.Background(), nil, nil, nil)
	if err == nil {
		t.Error("expected error from executor")
	}
}

func TestMemoryPages(t *testing.T) {
	tests := []struct {
		bytes int64
		pages uint32
	}{
		{0, 1},
		{1, 1},
		{65536, 1},
		{65537, 2},
		{64 * 1024 * 1024, 1024}, // 64 MiB
		{-1, 1},
	}
	for _, tc := range tests {
		got := MemoryPages(tc.bytes)
		if got != tc.pages {
			t.Errorf("MemoryPages(%d) = %d, want %d", tc.bytes, got, tc.pages)
		}
	}
}

// nilHost is a no-op HostFunctions for tests that don't exercise host calls.
type nilHost struct{}

func (nilHost) Call(_ context.Context, _ string, _ []byte) ([]byte, error) { return nil, nil }
func (nilHost) Emit(_ context.Context, _ string, _ []byte) error           { return nil }
func (nilHost) Log(_ context.Context, _, _ string) error                   { return nil }
func (nilHost) Get(_ context.Context, _ string) ([]byte, error)            { return nil, nil }
func (nilHost) Put(_ context.Context, _ string, _ []byte) error            { return nil }

var _ kernel.HostFunctions = nilHost{}

// echoWASM is a precompiled WASM module whose run() returns the input (ptr, len) unchanged.
// alloc() is a bump allocator starting at address 0.
var echoWASM = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x0d, 0x02,
	0x60, 0x01, 0x7f, 0x01, 0x7f,
	0x60, 0x02, 0x7f, 0x7f, 0x02, 0x7f, 0x7f,
	0x03, 0x03, 0x02, 0x00, 0x01,
	0x05, 0x03, 0x01, 0x00, 0x01,
	0x06, 0x06, 0x01, 0x7f, 0x01, 0x41, 0x00, 0x0b,
	0x07, 0x18, 0x03,
	0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00,
	0x05, 0x61, 0x6c, 0x6c, 0x6f, 0x63, 0x00, 0x00,
	0x03, 0x72, 0x75, 0x6e, 0x00, 0x01,
	0x0a, 0x1a, 0x02,
	0x11, 0x01, 0x01, 0x7f,
	0x23, 0x00, 0x21, 0x01, 0x23, 0x00, 0x20, 0x00, 0x6a, 0x24, 0x00, 0x20, 0x01, 0x0b,
	0x06, 0x00, 0x20, 0x00, 0x20, 0x01, 0x0b,
}

// infiniteLoopWASM is a WASM module whose run() loops forever (tests context cancellation).
var infiniteLoopWASM = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x0d, 0x02,
	0x60, 0x01, 0x7f, 0x01, 0x7f,
	0x60, 0x02, 0x7f, 0x7f, 0x02, 0x7f, 0x7f,
	0x03, 0x03, 0x02, 0x00, 0x01,
	0x05, 0x03, 0x01, 0x00, 0x01,
	0x07, 0x18, 0x03,
	0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00,
	0x05, 0x61, 0x6c, 0x6c, 0x6f, 0x63, 0x00, 0x00,
	0x03, 0x72, 0x75, 0x6e, 0x00, 0x01,
	0x0a, 0x12, 0x02,
	0x04, 0x00, 0x41, 0x00, 0x0b,
	0x0b, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x20, 0x00, 0x20, 0x01, 0x0b,
}

func TestNewExecutorInitializes(t *testing.T) {
	e := New(Config{TimeoutMS: 5000, MemoryBytes: 64 * 1024 * 1024})
	if e == nil || e.runtime == nil {
		t.Fatal("New() should return a non-nil executor with initialized runtime")
	}
}

func TestExecutorCompileAndRun(t *testing.T) {
	e := New(Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	ctx := context.Background()

	artifact, hash, err := e.Compile(ctx, echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(hash) == 0 {
		t.Error("expected non-empty hash")
	}

	input := []byte(`{"msg":"hello"}`)
	out, err := e.Execute(ctx, artifact, input, nilHost{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !bytes.Equal(out, input) {
		t.Errorf("echo output mismatch: got %q, want %q", out, input)
	}
}

func TestExecutorMemoryLimit(t *testing.T) {
	// 1 page = 64 KiB. An input larger than one page should fail to write into memory.
	e := New(Config{TimeoutMS: 5000, MemoryBytes: 65536})
	ctx := context.Background()

	artifact, _, err := e.Compile(ctx, echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	bigInput := make([]byte, 65537)
	_, err = e.Execute(ctx, artifact, bigInput, nilHost{})
	if err == nil {
		t.Error("expected memory overflow error, got nil")
	}
}

func TestExecutorContextTimeout(t *testing.T) {
	e := New(Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	ctx := context.Background()

	artifact, _, err := e.Compile(ctx, infiniteLoopWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	_, err = e.Execute(runCtx, artifact, []byte(`{}`), nilHost{})
	if err == nil {
		t.Error("expected timeout error from infinite loop, got nil")
	}
}

func TestExecutorConfiguredTimeout(t *testing.T) {
	e := New(Config{TimeoutMS: 100, MemoryBytes: 4 * 1024 * 1024})
	ctx := context.Background()

	artifact, _, err := e.Compile(ctx, infiniteLoopWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	_, err = e.Execute(ctx, artifact, []byte(`{}`), nilHost{})
	if err == nil {
		t.Error("expected configured timeout error from infinite loop, got nil")
	}
}

// emitCallerWASM is a precompiled WASM module whose run() calls juice.emit once with empty
// event name and args, then returns empty output. Used to test emit error propagation.
//
// Equivalent WAT:
//
//	(module
//	  (import "juice" "emit" (func $emit (param i32 i32 i32 i32)))
//	  (memory (export "memory") 1)
//	  (func (export "alloc") (param i32) (result i32) i32.const 0)
//	  (func (export "run") (param i32 i32) (result i32 i32)
//	    i32.const 0 i32.const 0 i32.const 0 i32.const 0 call $emit
//	    i32.const 0 i32.const 0))
var emitCallerWASM = []byte{
	// magic + version
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	// type section: (i32,i32,i32,i32)->(), (i32)->(i32), (i32,i32)->(i32,i32)
	0x01, 0x14, 0x03,
	0x60, 0x04, 0x7f, 0x7f, 0x7f, 0x7f, 0x00,
	0x60, 0x01, 0x7f, 0x01, 0x7f,
	0x60, 0x02, 0x7f, 0x7f, 0x02, 0x7f, 0x7f,
	// import section: juice.emit type 0
	0x02, 0x0e, 0x01,
	0x05, 0x6a, 0x75, 0x69, 0x63, 0x65,
	0x04, 0x65, 0x6d, 0x69, 0x74,
	0x00, 0x00,
	// function section: alloc=type1, run=type2
	0x03, 0x03, 0x02, 0x01, 0x02,
	// memory section: 1 page
	0x05, 0x03, 0x01, 0x00, 0x01,
	// export section: memory, alloc(func1), run(func2)
	0x07, 0x18, 0x03,
	0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00,
	0x05, 0x61, 0x6c, 0x6c, 0x6f, 0x63, 0x00, 0x01,
	0x03, 0x72, 0x75, 0x6e, 0x00, 0x02,
	// code section: alloc returns 0; run calls emit(0,0,0,0) then returns (0,0)
	// body size 0x17=23: count(1) + alloc-entry(5) + run-entry(17)
	// run body size 0x10=16: locals(1)+8×i32.const(16)+call(2)+2×i32.const(4)+end(1)
	0x0a, 0x17, 0x02,
	0x04, 0x00, 0x41, 0x00, 0x0b,
	0x10, 0x00, 0x41, 0x00, 0x41, 0x00, 0x41, 0x00, 0x41, 0x00, 0x10, 0x00, 0x41, 0x00, 0x41, 0x00, 0x0b,
}

// errEmitHost is a HostFunctions implementation that always returns an error from Emit.
type errEmitHost struct{ err error }

func (h errEmitHost) Call(_ context.Context, _ string, _ []byte) ([]byte, error) { return nil, nil }
func (h errEmitHost) Emit(_ context.Context, _ string, _ []byte) error           { return h.err }
func (h errEmitHost) Log(_ context.Context, _, _ string) error                   { return nil }

func TestEmitErrorPropagated(t *testing.T) {
	e := New(Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	ctx := context.Background()

	artifact, _, err := e.Compile(ctx, emitCallerWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	host := errEmitHost{err: fmt.Errorf("emit failed intentionally")}
	_, err = e.Execute(ctx, artifact, []byte(`{}`), host)
	if err == nil {
		t.Fatal("expected Execute to fail when juice.emit returns an error, got nil")
	}
}

func TestHostModuleExportsEmit(t *testing.T) {
	e := New(Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	builder := e.runtime.NewHostModuleBuilder("juice-test")
	registerHostFunctions(builder, nilHost{})
	mod, err := builder.Instantiate(context.Background())
	if err != nil {
		t.Fatalf("Instantiate host module: %v", err)
	}
	defer mod.Close(context.Background())

	if mod.ExportedFunctionDefinitions()["emit"] == nil {
		t.Fatal("expected host module to export emit")
	}
}
