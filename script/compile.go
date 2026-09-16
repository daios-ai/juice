// SPDX-License-Identifier: AGPL-3.0-only

package script

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/tetratelabs/wazero"
)

// TinyGoSDK is the shared TinyGo source prepended to every generated WASM action.
//
//go:embed sdk.tmpl
var TinyGoSDK string

// ImportedFunc describes one function import in a WASM module.
// Mirrors kernel.WASMImport; kept as a separate type to avoid an import cycle in tests.
type ImportedFunc struct {
	Module string
	Name   string
}

// InspectModule enumerates the imports and exports of a WASM binary without executing it.
func InspectModule(artifact []byte) (imports []ImportedFunc, exports []string, err error) {
	rt := wazero.NewRuntime(context.Background())
	defer rt.Close(context.Background())
	compiled, err := rt.CompileModule(context.Background(), artifact)
	if err != nil {
		return nil, nil, kernel.ErrInvalidInput.Wrapf("invalid WASM binary: %v", err)
	}
	for _, f := range compiled.ImportedFunctions() {
		modName, funcName, _ := f.Import()
		imports = append(imports, ImportedFunc{Module: modName, Name: funcName})
	}
	for name := range compiled.ExportedFunctions() {
		exports = append(exports, name)
	}
	return imports, exports, nil
}

// InspectWASM implements kernel.WASMInspector on the Executor.
func (e *Executor) InspectWASM(artifact []byte) ([]kernel.WASMImport, []string, error) {
	raw, exports, err := InspectModule(artifact)
	if err != nil {
		return nil, nil, err
	}
	imports := make([]kernel.WASMImport, len(raw))
	for i, r := range raw {
		imports[i] = kernel.WASMImport{Module: r.Module, Name: r.Name}
	}
	return imports, exports, nil
}

// CompileConfig holds settings for the TinyGo source compiler.
type CompileConfig struct {
	TimeoutMS int64 // default 30_000
}

// TinyGoCompiler compiles TinyGo source to WASM by invoking the tinygo binary.
// Results are cached in memory keyed by the SHA-256 hash of the source bytes.
type TinyGoCompiler struct {
	cfg   CompileConfig
	mu    sync.Mutex
	cache map[string][]byte // hex(sha256(source)) → wasm bytes
}

// NewTinyGoCompiler constructs a TinyGoCompiler with the given config.
func NewTinyGoCompiler(cfg CompileConfig) *TinyGoCompiler {
	if cfg.TimeoutMS <= 0 {
		cfg.TimeoutMS = 30_000
	}
	return &TinyGoCompiler{cfg: cfg, cache: make(map[string][]byte)}
}

// CompileSource compiles TinyGo source bytes to a WASM artifact.
// Returns ErrInvalidInput on compilation failure; ErrInvalidState when tinygo is absent from PATH.
func (c *TinyGoCompiler) CompileSource(ctx context.Context, source []byte) ([]byte, string, error) {
	h := sha256.Sum256(source)
	key := hex.EncodeToString(h[:])

	c.mu.Lock()
	if cached, ok := c.cache[key]; ok {
		c.mu.Unlock()
		wh := sha256.Sum256(cached)
		return cached, hex.EncodeToString(wh[:]), nil
	}
	c.mu.Unlock()

	if _, err := exec.LookPath("tinygo"); err != nil {
		return nil, "", kernel.ErrInvalidState.Wrap("tinygo binary not found in PATH")
	}

	dir, err := os.MkdirTemp("", "juice-make-*")
	if err != nil {
		return nil, "", kernel.ErrInternal.Wrapf("create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, source, 0o600); err != nil {
		return nil, "", kernel.ErrInternal.Wrapf("write source: %v", err)
	}

	timeout := time.Duration(c.cfg.TimeoutMS) * time.Millisecond
	compileCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	outPath := filepath.Join(dir, "out.wasm")
	cmd := exec.CommandContext(compileCtx, "tinygo", "build", "-target", "wasip1", "-o", outPath, srcPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return nil, "", kernel.ErrInvalidInput.Wrapf("tinygo compile error: %s", msg)
	}

	wasm, err := os.ReadFile(outPath)
	if err != nil {
		return nil, "", kernel.ErrInternal.Wrapf("read artifact: %v", err)
	}

	c.mu.Lock()
	c.cache[key] = wasm
	c.mu.Unlock()

	wh := sha256.Sum256(wasm)
	return wasm, hex.EncodeToString(wh[:]), nil
}

// minimalEchoWASM is a minimal valid WASM module used by FakeCompiler.
// It exports alloc (bump allocator) and run (echo: returns input ptr+len unchanged).
// These are the same bytes as echoWASM in wasm_test.go.
var minimalEchoWASM = []byte{
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

// FakeCompiler is a deterministic SourceCompiler for tests.
// It returns minimalEchoWASM (or Artifact if set) without invoking tinygo.
type FakeCompiler struct {
	Artifact []byte
	Err      error
}

func (f *FakeCompiler) CompileSource(_ context.Context, _ []byte) ([]byte, string, error) {
	if f.Err != nil {
		return nil, "", f.Err
	}
	art := f.Artifact
	if art == nil {
		art = minimalEchoWASM
	}
	h := sha256.Sum256(art)
	return art, hex.EncodeToString(h[:]), nil
}
