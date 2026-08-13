package native

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/daios-ai/juice/kernel"
)

// CompileDeps holds the dependencies injected into the @sys/tinygo/compile handler.
// Compiler turns TinyGo source into a WASM artifact; Scripts (a kernel.WASMInspector)
// validates the produced artifact's imports/exports.
type CompileDeps struct {
	Compiler kernel.SourceCompiler
	Scripts  kernel.ScriptExecutor
}

// CompileResult is the output schema of @sys/tinygo/compile.
// A source that fails to compile or fails import/export validation returns
// status="failure" with diagnostics — a charged success transaction, not a kernel
// error — so an author compile error is charged, while a platform misconfiguration is not.
type CompileResult struct {
	Status       string   `json:"status"`                  // "success" | "failure"
	Artifact     string   `json:"artifact,omitempty"`      // base64 WASM on success
	ArtifactHash string   `json:"artifact_hash,omitempty"` // SHA-256 hex of the artifact
	Diagnostics  []string `json:"diagnostics"`
}

// TinyGo declares @sys/tinygo/compile (§9). sdk is the TinyGo SDK source (script.TinyGoSDK)
// prepended to the author's body, so authors submit only a
// `func Handle(in map[string]any) (map[string]any, error)`.
func TinyGo(deps CompileDeps, sdk string) Spec {
	return Spec{
		Name:        "tinygo/compile",
		Description: "Compiles TinyGo source (a Handle function written against the Juice SDK) to a WASM artifact, ready to register with action create --kind wasm --artifact",
		InputSchema: obj(map[string]any{
			"source": str("TinyGo source: a func Handle(in map[string]any) (map[string]any, error) plus any private helpers; the SDK (package, imports, alloc, run, main) is prepended automatically"),
		}, "source"),
		OutputSchema: obj(map[string]any{
			"status":        str("success or failure"),
			"artifact":      str("Base64-encoded WASM artifact, present on success"),
			"artifact_hash": str("SHA-256 hex of the artifact, present on success"),
			"diagnostics":   arrayOf(map[string]any{"type": "string"}, "Compile/validation diagnostics"),
		}, "status", "diagnostics"),
		Handler: func(*kernel.Kernel) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeTinyGoCompile(ctx, args, deps, sdk)
			}
		},
	}
}

func executeTinyGoCompile(ctx context.Context, args map[string]any, deps CompileDeps, sdk string) (map[string]any, error) {
	if deps.Compiler == nil {
		return nil, kernel.ErrInvalidState.Wrap("source compiler not configured")
	}
	source, _ := args["source"].(string)
	if strings.TrimSpace(source) == "" {
		return nil, kernel.ErrInvalidInput.Wrap("source is required")
	}

	// Prepend the SDK (which owns package/imports/alloc/run/main) to the author's body.
	full := prepareSource(sdk, source)

	wasm, _, err := deps.Compiler.CompileSource(ctx, []byte(full))
	if err != nil {
		// A platform misconfiguration (e.g. the tinygo toolchain is absent) is an
		// ErrInvalidState kernel error — the call fails and is not charged. An author
		// compile error (ErrInvalidInput) is a charged status=failure with diagnostics.
		if errors.Is(err, kernel.ErrInvalidState) {
			return nil, err
		}
		return marshalCompileResult(&CompileResult{
			Status:      "failure",
			Diagnostics: []string{err.Error()},
		})
	}

	if diag := checkWASMImports(wasm, deps.Scripts); diag != "" {
		return marshalCompileResult(&CompileResult{
			Status:      "failure",
			Diagnostics: []string{diag},
		})
	}

	hash := sha256.Sum256(wasm)
	return marshalCompileResult(&CompileResult{
		Status:       "success",
		Artifact:     base64.StdEncoding.EncodeToString(wasm),
		ArtifactHash: hex.EncodeToString(hash[:]),
	})
}

func marshalCompileResult(r *CompileResult) (map[string]any, error) {
	if r.Diagnostics == nil {
		r.Diagnostics = []string{}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, kernel.ErrInternal.Wrapf("marshal compile result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, kernel.ErrInternal.Wrapf("unmarshal compile result: %v", err)
	}
	return m, nil
}

// checkWASMImports validates that the artifact only imports allowed host functions.
func checkWASMImports(wasm []byte, scripts kernel.ScriptExecutor) string {
	insp, ok := scripts.(kernel.WASMInspector)
	if !ok {
		return ""
	}
	imports, exports, err := insp.InspectWASM(wasm)
	if err != nil {
		return fmt.Sprintf("WASM inspection failed: %v", err)
	}
	allowedImports := map[string]bool{"call": true, "step_create": true, "step_complete": true, "log": true}
	for _, imp := range imports {
		if imp.Module == "wasi_snapshot_preview1" {
			continue
		}
		if imp.Module != "juice" || !allowedImports[imp.Name] {
			return fmt.Sprintf("WASM imports disallowed function %q from module %q", imp.Name, imp.Module)
		}
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
		return "WASM does not export required function alloc"
	}
	if !hasRun {
		return "WASM does not export required function run"
	}
	return ""
}

// prepareSource combines the SDK with the author-supplied Handle body. The SDK already
// owns `package main` and all imports, so any `package`/`import` declaration in the body
// must be stripped — otherwise the assembled file places imports after the SDK's
// declarations and TinyGo rejects it with "imports must appear before other declarations".
// Stripping a needed import surfaces as a clear "undefined: X", which beats an
// unrecoverable placement error.
func prepareSource(sdk, generated string) string {
	var kept []string
	inImportBlock := false
	for _, line := range strings.Split(generated, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case inImportBlock:
			// Skip the body of a multi-line `import (` ... `)` block.
			if trimmed == ")" || strings.HasSuffix(trimmed, ")") {
				inImportBlock = false
			}
		case strings.HasPrefix(trimmed, "package "):
			// drop package declaration
		case strings.HasPrefix(trimmed, "import ("):
			inImportBlock = !strings.HasSuffix(trimmed, ")") // single-line `import (...)` ends immediately
		case strings.HasPrefix(trimmed, "import "):
			// drop single-line `import "x"` / `import x "y"`
		default:
			kept = append(kept, line)
		}
	}
	body := strings.TrimSpace(strings.Join(kept, "\n"))
	if sdk == "" {
		return "package main\n\n" + body
	}
	return sdk + "\n" + body
}
