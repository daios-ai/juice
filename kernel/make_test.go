package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/llm"
	"github.com/daios-ai/juice/script"
	"github.com/google/uuid"
)

// newMakeKernel builds a kernel wired for @sys/make tests.
func newMakeKernel(t *testing.T, chatter kernel.Chatter) (*kernel.Kernel, kernel.Store) {
	t.Helper()
	st := newTestStore(t)
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	exec := script.New(script.Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	k := kernel.New(st, exec, nil, nil, chatter, cfg, nil)
	k.SetCompiler(&script.FakeCompiler{})
	kernel.RegisterChatHandler(k)
	kernel.RegisterMakeHandler(k, "", 0)
	return k, st
}

// seedMakeAction seeds @sys user, @sys/make native action, and @sys/llm/chat in the store.
// Returns the @sys user.
func seedMakeAction(t *testing.T, st kernel.Store) *kernel.User {
	t.Helper()
	ctx := context.Background()
	sys := setupUser(t, st, "@sys", 10000)
	actions := []struct {
		name        string
		price       int64
		inSchema    map[string]any
		outSchema   map[string]any
		description string
	}{
		{
			name:        "make",
			price:       20,
			description: "Generate a WASM action from a natural-language description",
			inSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"description": map[string]any{"type": "string", "description": "task description"}},
				"required":   []string{"description"},
			},
			outSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"status": map[string]any{"type": "string", "description": "outcome"}},
				"required":   []string{"status"},
			},
		},
		{
			name:        "llm/chat",
			price:       0,
			description: "Chat completion",
			inSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"messages": map[string]any{"type": "array", "description": "messages", "items": map[string]any{"type": "object"}}},
				"required":   []string{"messages"},
			},
			outSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"message": map[string]any{"type": "object", "description": "reply"}},
			},
		},
	}
	for _, spec := range actions {
		a := &kernel.Action{
			ID:           uuid.New().String(),
			OwnerUserID:  sys.ID,
			Name:         spec.name,
			Kind:         kernel.KindNative,
			Active:       true,
			Public:       true,
			Price:        spec.price,
			Description:  spec.description,
			Source:       "native",
			InputSchema:  spec.inSchema,
			OutputSchema: spec.outSchema,
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		if err := st.CreateAction(ctx, a); err != nil {
			t.Fatalf("seed %s: %v", spec.name, err)
		}
	}
	return sys
}

func TestMakeRejectsEmptyDescription(t *testing.T) {
	k, st := newMakeKernel(t, &llm.FakeChatter{})
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 100)

	_, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{}, // missing description
	})
	// Missing required field is caught by ValidateInput (ErrSchemaViolation) or
	// parseMakeInput (ErrInvalidInput) depending on schema configuration.
	if !errors.Is(err, kernel.ErrSchemaViolation) && !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrSchemaViolation or ErrInvalidInput for missing description, got %v", err)
	}
}

func TestMakeReturnsErrInvalidStateWithoutCompiler(t *testing.T) {
	k, st := newMakeKernel(t, &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}})
	k.SetCompiler(nil) // remove compiler
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 100)

	_, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "test"},
	})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState without compiler, got %v", err)
	}
}

// fakeContract is a minimal valid contract JSON response for tests.
const fakeContract = `{"name":"test-action","input_schema":{"type":"object","properties":{}},"output_schema":{"type":"object","properties":{"result":{"type":"string"}},"required":["result"]},"plan":"simple fixed response"}`

// fakeCode is a minimal valid TinyGo run function for tests.
const fakeCode = "```go\n//export run\nfunc run(inputPtr, inputLen uint32) (uint32, uint32) { return 0, 2 }\n```"

// fakeExamples is an empty example list — unit tests use the compile smoke test only.
const fakeExamples = `[]`

func TestMakeReturnsDraftOnSuccess(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{
		fakeContract, // step 2: contract derivation
		fakeCode,     // step 5: source generation
		fakeExamples, // step 8: example generation
	}}
	k, st := newMakeKernel(t, fakeChat)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 100)

	reply, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{
			"description":   "An action that returns a fixed result",
		},
	})
	if err != nil {
		t.Fatalf("Call @sys/make: %v", err)
	}

	b, _ := json.Marshal(reply.Result)
	var result kernel.MakeResult
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.Status != "success" {
		t.Errorf("expected status=success, got %q; diagnostics: %v", result.Status, result.Diagnostics)
		return
	}
	if result.Draft == nil {
		t.Fatal("expected non-nil draft on success")
	}
	if result.Draft.Kind != "wasm" {
		t.Errorf("expected kind=wasm, got %q", result.Draft.Kind)
	}
	if result.Draft.ArtifactHash == "" {
		t.Error("expected non-empty artifact_hash")
	}
	if result.Draft.Source == "" {
		t.Error("expected non-empty source in draft")
	}
	if result.Draft.InputSchema == nil {
		t.Error("expected non-nil input_schema in draft")
	}
	if result.Draft.OutputSchema == nil {
		t.Error("expected non-nil output_schema in draft")
	}
}

func TestMakeDraftHasNameAndKind(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{
		fakeContract,
		fakeCode,
		fakeExamples,
	}}
	k, st := newMakeKernel(t, fakeChat)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 100)

	reply, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "compute something interesting"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	b, _ := json.Marshal(reply.Result)
	var result kernel.MakeResult
	_ = json.Unmarshal(b, &result)
	if result.Draft == nil {
		return // already tested elsewhere
	}
	if result.Draft.Kind != "wasm" {
		t.Errorf("expected kind=wasm, got %q", result.Draft.Kind)
	}
	if result.Draft.Name == "" {
		t.Error("expected non-empty name derived from description")
	}
}

func TestMakeMaxStepsBoundsRepairLoop(t *testing.T) {
	// FakeChatter always returns unparseable content — forces the loop to exhaust steps.
	fakeChat := &llm.FakeChatter{Reply: kernel.ChatMessage{Role: "assistant", Content: "not a go block"}}
	// FakeCompiler always fails.
	fakeComp := &script.FakeCompiler{Err: kernel.ErrInvalidInput.Wrap("syntax error")}

	st := newTestStore(t)
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	exec := script.New(script.Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	k := kernel.New(st, exec, nil, nil, fakeChat, cfg, nil)
	k.SetCompiler(fakeComp)
	kernel.RegisterChatHandler(k)
	kernel.RegisterMakeHandler(k, "", 0)

	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 10000)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 1000)

	reply, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{
			"description":   "always fail",
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	b, _ := json.Marshal(reply.Result)
	var result kernel.MakeResult
	_ = json.Unmarshal(b, &result)
	if result.Status != "failure" {
		t.Errorf("expected status=failure after exhausted steps, got %q", result.Status)
	}
	if len(result.Diagnostics) == 0 {
		t.Error("expected diagnostics when repair loop exhausted")
	}
}

func TestMakeInternalChatCallCreatesChildTrace(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{
		fakeContract, fakeCode, fakeExamples,
	}}
	k, st := newMakeKernel(t, fakeChat)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 10000)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 1000)

	_, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{
			"description":   "test",
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// @sys/make itself + at least one @sys/llm/chat sub-call.
	txs, err := k.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) < 2 {
		t.Errorf("expected at least 2 transactions (make + chat sub-calls), got %d", len(txs))
	}
}

// cycleFakeChatter cycles through a list of responses for successive Chat() calls.
type cycleFakeChatter struct {
	responses []string
	idx       int
}

func (c *cycleFakeChatter) Chat(_ context.Context, _ []kernel.ChatMessage) (kernel.ChatMessage, error) {
	if len(c.responses) == 0 {
		return kernel.ChatMessage{Role: "assistant", Content: "{}"}, nil
	}
	r := c.responses[c.idx%len(c.responses)]
	c.idx++
	return kernel.ChatMessage{Role: "assistant", Content: r}, nil
}
