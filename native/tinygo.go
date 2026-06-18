package native

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
// error — mirroring @sys/make's "synthesis failures use output failure status" rule.
type CompileResult struct {
	Status       string   `json:"status"` // "success" | "failure"
	Artifact     string   `json:"artifact,omitempty"`      // base64 WASM on success
	ArtifactHash string   `json:"artifact_hash,omitempty"` // SHA-256 hex of the artifact
	Diagnostics  []string `json:"diagnostics"`
}

// RegisterTinyGoCompileHandler registers the @sys/tinygo/compile native action on k.
// sdk is the TinyGo SDK source (script.TinyGoSDK) prepended to the author's body, so
// authors submit only a `func Handle(in map[string]any) (map[string]any, error)`.
func RegisterTinyGoCompileHandler(k *kernel.Kernel, deps CompileDeps, sdk string) {
	k.RegisterNativeHandler("tinygo/compile", func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return executeTinyGoCompile(ctx, args, deps, sdk)
	})
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
