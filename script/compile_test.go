package script

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

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

// TestSDKContract pins the author-facing contract: the SDK owns the entry point
// (main + run) and delegates to an author-supplied Handle, exposes the call/step/log
// helpers, and no longer references the removed juice.emit host import.
func TestSDKContract(t *testing.T) {
	must := []string{
		"//export run",
		"func run(",
		"Handle(in)", // run delegates to author's Handle
		"func JuiceCall(",
		"func JuiceStepCreate(",
		"func JuiceStepComplete(",
		"func JuiceLog(",
		"//export alloc",
		"func main()",
	}
	for _, s := range must {
		if !strings.Contains(TinyGoSDK, s) {
			t.Errorf("TinyGoSDK missing required substring %q", s)
		}
	}
	for _, banned := range []string{"emit", "hostEmit", "JuiceEmit"} {
		if strings.Contains(TinyGoSDK, banned) {
			t.Errorf("TinyGoSDK must not reference removed host function %q", banned)
		}
	}
}

// TestSDKCompilesWithRealTinyGo prepends the SDK to a Handle body and compiles it
// with the real TinyGo toolchain, proving the SDK is valid TinyGo and that an action
// author truly only needs to write Handle. Skipped when tinygo is not installed.
func TestSDKCompilesWithRealTinyGo(t *testing.T) {
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not installed; skipping real-compile integration test")
	}
	body := `
func Handle(in map[string]any) (map[string]any, error) {
	n, _ := in["n"].(float64)
	JuiceLog("info", "doubling "+strconv.Itoa(int(n)))
	return map[string]any{"result": math.Abs(n) * 2, "kind": strings.ToUpper("ok")}, nil
}
`
	src := TinyGoSDK + "\n" + body
	c := NewTinyGoCompiler(CompileConfig{TimeoutMS: 60000})
	wasm, hash, err := c.CompileSource(context.Background(), []byte(src))
	if err != nil {
		t.Fatalf("compile SDK+Handle: %v", err)
	}
	if len(wasm) == 0 || hash == "" {
		t.Fatal("expected non-empty artifact and hash")
	}

	imports, exports, err := InspectModule(wasm)
	if err != nil {
		t.Fatalf("inspect artifact: %v", err)
	}
	hasAlloc, hasRun := false, false
	for _, e := range exports {
		switch e {
		case "alloc":
			hasAlloc = true
		case "run":
			hasRun = true
		}
	}
	if !hasAlloc || !hasRun {
		t.Errorf("artifact must export alloc and run; exports=%v", exports)
	}
	allowed := map[string]bool{"call": true, "step_create": true, "step_complete": true, "log": true}
	for _, imp := range imports {
		if imp.Module == "wasi_snapshot_preview1" {
			continue
		}
		if imp.Module != "juice" || !allowed[imp.Name] {
			t.Errorf("artifact imports disallowed %q from %q", imp.Name, imp.Module)
		}
	}

	// The artifact runs: feed input through alloc/run and read back the JSON output.
	e := New(Config{TimeoutMS: 10000, MemoryBytes: 16 * 1024 * 1024})
	out, err := e.Execute(context.Background(), wasm, []byte(`{"n":-3}`), &sdkTestHost{})
	if err != nil {
		t.Fatalf("execute artifact: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, out)
	}
	if got["result"] != float64(6) {
		t.Errorf("result = %v, want 6", got["result"])
	}
	if got["kind"] != "OK" {
		t.Errorf("kind = %v, want OK", got["kind"])
	}
}

// TestSDKHandleErrorSurfacesMessage proves the SDK reports a Handle error as the
// reserved envelope {"__juice_error__":"<msg>"} instead of trapping, so the failure
// reason survives execution. Skipped when tinygo is not installed.
func TestSDKHandleErrorSurfacesMessage(t *testing.T) {
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not installed; skipping real-compile integration test")
	}
	body := `
func Handle(in map[string]any) (map[string]any, error) {
	return nil, errors.New("deliberate handle failure")
}
`
	src := TinyGoSDK + "\n" + body
	c := NewTinyGoCompiler(CompileConfig{TimeoutMS: 60000})
	wasm, _, err := c.CompileSource(context.Background(), []byte(src))
	if err != nil {
		t.Fatalf("compile SDK+Handle: %v", err)
	}

	e := New(Config{TimeoutMS: 10000, MemoryBytes: 16 * 1024 * 1024})
	out, err := e.Execute(context.Background(), wasm, []byte(`{}`), &sdkTestHost{})
	if err != nil {
		t.Fatalf("execute artifact (should return envelope, not trap): %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, out)
	}
	msg, ok := got["__juice_error__"].(string)
	if !ok {
		t.Fatalf("expected __juice_error__ envelope, got %s", out)
	}
	if msg != "deliberate handle failure" {
		t.Errorf("error message = %q, want %q", msg, "deliberate handle failure")
	}
}

// TestSDKNestedJuiceCall proves a WASM action can call another action via
// juice.call without self-deadlocking — the host re-enters Execute on the same
// Executor. Before the persistent-host-module change this hung on a global lock.
// Skipped when tinygo is not installed.
func TestSDKNestedJuiceCall(t *testing.T) {
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not installed; skipping real-compile integration test")
	}
	body := `
func Handle(in map[string]any) (map[string]any, error) {
	reply, _ := JuiceCall("@owner/inner", []byte("{}"))
	var sub map[string]any
	_ = json.Unmarshal(reply, &sub)
	return map[string]any{"inner": sub["inner"]}, nil
}
`
	src := TinyGoSDK + "\n" + body
	c := NewTinyGoCompiler(CompileConfig{TimeoutMS: 60000})
	outerWASM, _, err := c.CompileSource(context.Background(), []byte(src))
	if err != nil {
		t.Fatalf("compile outer SDK+Handle: %v", err)
	}

	e := New(Config{TimeoutMS: 10000, MemoryBytes: 16 * 1024 * 1024})

	// The host's Call re-enters the executor to run echoWASM (which returns its input).
	host := &reentrantHost{e: e, reply: []byte(`{"inner":"ok"}`)}

	// Guard against a regression to the deadlock: bound the whole execution.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := e.Execute(ctx, outerWASM, []byte(`{}`), host)
	if err != nil {
		t.Fatalf("nested execute: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, out)
	}
	if got["inner"] != "ok" {
		t.Errorf("inner = %v, want ok", got["inner"])
	}
	if !host.reentered {
		t.Error("host.Call did not re-enter the executor")
	}
}

// reentrantHost.Call re-enters the executor (running echoWASM) to prove nested
// WASM execution works; the other host functions are no-ops.
type reentrantHost struct {
	e         *Executor
	reply     []byte
	reentered bool
}

func (h *reentrantHost) Call(ctx context.Context, _ string, _ []byte) ([]byte, error) {
	h.reentered = true
	// Re-enter Execute on a different artifact; the reply bytes are echoed back.
	return h.e.Execute(ctx, echoWASM, h.reply, nilHost{})
}
func (h *reentrantHost) StepCreate(context.Context, []byte, string, string) (string, error) {
	return "", nil
}
func (h *reentrantHost) StepComplete(context.Context, string, []byte) ([]byte, error) {
	return []byte("{}"), nil
}
func (h *reentrantHost) Log(context.Context, string, string) error { return nil }

// sdkTestHost is a no-op kernel.HostFunctions for the SDK compile test.
type sdkTestHost struct{}

func (sdkTestHost) Call(context.Context, string, []byte) ([]byte, error) { return []byte("{}"), nil }
func (sdkTestHost) StepCreate(context.Context, []byte, string, string) (string, error) {
	return "", nil
}
func (sdkTestHost) StepComplete(context.Context, string, []byte) ([]byte, error) {
	return []byte("{}"), nil
}
func (sdkTestHost) Log(context.Context, string, string) error { return nil }

func TestMagicByteDetection(t *testing.T) {
	// WASM magic bytes → compileWasmSource should pass directly to ScriptExecutor.
	if len(minimalEchoWASM) < 4 {
		t.Fatal("minimalEchoWASM too short")
	}
	if minimalEchoWASM[0] != 0x00 || minimalEchoWASM[1] != 0x61 || minimalEchoWASM[2] != 0x73 || minimalEchoWASM[3] != 0x6d {
		t.Error("minimalEchoWASM must start with WASM magic bytes")
	}
}
