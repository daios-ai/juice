package native_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/native"
	"github.com/daios-ai/juice/script"
)

// TestMakeRealCompileSynthesizesAction is the deterministic end-to-end guard for
// @sys/make: it drives the FULL synthesis pipeline (prepareSource → real TinyGo
// compile → checkWASMImports → real wazero Execute → register → activate) with the
// real compiler and executor, but a FAKE LLM returning known-good contract, code,
// and example inputs. This removes Ollama nondeterminism, so any failure here is a
// genuine code regression in the make/SDK/compile/execute path — not model quality.
//
// The fakes form a consistent echo action: Handle returns its input, the input
// schema is empty, the example inputs carry a "result" key, and the output schema
// requires "result" — so the echoed output satisfies the contract.
//
// Skipped when tinygo is not installed (mirrors TestSDKCompilesWithRealTinyGo).
func TestMakeRealCompileSynthesizesAction(t *testing.T) {
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not installed; skipping real-compile make integration test")
	}

	chatter := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}

	st := newTestStore(t)
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()

	// Real executor + real TinyGo compiler + the embedded SDK — the production path.
	executor := script.New(script.Config{TimeoutMS: 30000, MemoryBytes: 16 * 1024 * 1024})
	compiler := script.NewTinyGoCompiler(script.CompileConfig{TimeoutMS: 120000})

	k := kernel.New(st, executor, nil, nil, cfg, log.Default())
	native.RegisterChatHandler(k, chatter)
	native.RegisterDecideHandler(k, decider)
	native.RegisterLookupHandler(k)
	registerJSONIfSupported(k, chatter)
	native.RegisterMakeHandler(k, native.MakeDeps{
		Scripts:  executor,
		Compiler: compiler,
		Chatter:  chatter,
	}, script.TinyGoSDK, 1)

	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	_, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        caller.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    sys.ID,
		ActionName:      "make",
		Args:            map[string]any{"description": "echo the input back"},
	})
	if err != nil {
		t.Fatalf("@sys/make Call: %v", err)
	}

	b, _ := json.Marshal(reply.Result)
	var result native.MakeResult
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("decode make result: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("expected status=success from deterministic real-compile synthesis; got %q\ndiagnostics=%v\ntests=%+v",
			result.Status, result.Diagnostics, result.Tests)
	}
	if result.ActionID == "" || result.ActionName == "" {
		t.Fatalf("expected registered action id+name, got id=%q name=%q", result.ActionID, result.ActionName)
	}

	// The synthesized action must be registered, active, and runnable.
	act, err := k.ReadAction(ctx, result.ActionID)
	if err != nil {
		t.Fatalf("read synthesized action: %v", err)
	}
	if !act.Active {
		t.Errorf("synthesized action should be active")
	}
}
