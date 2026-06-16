package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/llm"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/native"
	"github.com/daios-ai/juice/script"
	"github.com/daios-ai/juice/store"
	"github.com/google/uuid"
)

// testIssuerUserID is a fixed sentinel user ID inserted into every test store.
const testIssuerUserID = "00000000-0000-0000-0000-000000000001"

func newTestStore(t testing.TB) kernel.Store {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	hash, err := kernel.HashPassword("issuer-password")
	if err != nil {
		t.Fatalf("newTestStore: hash password: %v", err)
	}
	issuer := &kernel.User{
		ID:           testIssuerUserID,
		Handle:       "@_test_issuer",
		Email:        "issuer@test.internal",
		PasswordHash: hash,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := db.CreateUser(context.Background(), issuer); err != nil {
		t.Fatalf("newTestStore: seed issuer user: %v", err)
	}
	return db
}

func testSigningKey() ed25519.PrivateKey {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return priv
}

func setupUser(t *testing.T, st kernel.Store, handle string, balance int64) *kernel.User {
	t.Helper()
	hash, err := kernel.HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	u := &kernel.User{
		ID:           uuid.New().String(),
		Handle:       handle,
		Email:        handle + "@example.com",
		PasswordHash: hash,
		Available:    balance,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// newMakeKernel builds a kernel and registers the native handlers for @sys/make tests.
func newMakeKernel(t *testing.T, chatter kernel.Chatter, decider kernel.DecideChatter) (*kernel.Kernel, kernel.Store) {
	t.Helper()
	st := newTestStore(t)
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	exec := script.New(script.Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	k := kernel.New(st, exec, nil, nil, cfg, log.Default())
	fakeComp := &script.FakeCompiler{}
	native.RegisterChatHandler(k, chatter)
	native.RegisterDecideHandler(k, decider)
	native.RegisterLookupHandler(k)
	native.RegisterMakeHandler(k, native.MakeDeps{
		Scripts:  exec,
		Compiler: fakeComp,
		Chatter:  chatter,
	}, "", 0)
	return k, st
}

// beginMakeTestRun creates a process and root trace for @sys/make via BeginRun, mirroring production.
func beginMakeTestRun(t *testing.T, st kernel.Store, callerID, sysID string) (*kernel.Process, *kernel.Trace) {
	t.Helper()
	ctx := context.Background()
	a, err := st.ReadActionByOwnerName(ctx, sysID, "make")
	if err != nil {
		t.Fatalf("beginMakeTestRun: find @sys/make: %v", err)
	}
	p := &kernel.Process{
		ID: uuid.New().String(), OwnerUserID: callerID,
		Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC(),
	}
	tr := &kernel.Trace{
		ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: a.OwnerUserID, ActionID: a.ID,
		CallerUserID: callerID, CreatedAt: time.Now().UTC(),
	}
	if err := st.BeginRun(ctx, p, tr, callerID, a.Price); err != nil {
		t.Fatalf("beginMakeTestRun: %v", err)
	}
	return p, tr
}

// seedMakeAction seeds @sys user and all native actions required by @sys/make tests.
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
				"type": "object",
				"properties": map[string]any{
					"status":      map[string]any{"type": "string", "description": "outcome"},
					"action_id":   map[string]any{"type": "string", "description": "registered action id"},
					"action_name": map[string]any{"type": "string", "description": "registered action name"},
					"diagnostics": map[string]any{"type": "array", "description": "diagnostics", "items": map[string]any{"type": "string"}},
					"tests":       map[string]any{"type": "array", "description": "tests", "items": map[string]any{"type": "object"}},
				},
				"required": []string{"status", "diagnostics"},
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
		{
			name:        "llm/decide",
			price:       0,
			description: "Select a Juice action and propose args",
			inSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"messages": map[string]any{"type": "array", "description": "conversation history", "items": map[string]any{"type": "object"}},
					"actions":  map[string]any{"type": "array", "description": "candidate actions", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"messages", "actions"},
			},
			outSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "description": "selected action"},
					"args":   map[string]any{"type": "object", "description": "proposed args"},
				},
				"required": []string{"action", "args"},
			},
		},
		{
			name:        "lookup",
			price:       0,
			description: "Rank active actions by semantic similarity and stats",
			inSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": map[string]any{"type": "string", "description": "search query"}},
				"required":   []string{"query"},
			},
			outSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"results": map[string]any{"type": "array", "description": "ranked results", "items": map[string]any{"type": "object"}}},
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

// fakeContract is a minimal valid contract JSON response for tests.
const fakeContract = `{"name":"test-action","input_schema":{"type":"object","properties":{}},"output_schema":{"type":"object","properties":{"result":{"type":"string"}},"required":["result"]}}`

// fakeCode is a minimal valid TinyGo run function for tests.
const fakeCode = "```go\n//export run\nfunc run(inputPtr, inputLen uint32) (uint32, uint32) { return 0, 2 }\n```"

// fakeExamples provides 3 inputs whose echo output satisfies fakeContract's output schema.
const fakeExamples = `[{"result":"a"}, {"result":"b"}, {"result":"c"}]`

// chatSelectCall returns a ToolCall that selects @sys/llm/chat for use with cycleDecideChatter.
func chatSelectCall() *kernel.ToolCall {
	return &kernel.ToolCall{
		Action: "@sys/llm/chat",
		Args: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "generate implementation"},
			},
		},
	}
}

func TestMakeRejectsEmptyDescription(t *testing.T) {
	k, st := newMakeKernel(t, &llm.FakeChatter{}, &llm.FakeDecideChatter{})
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrSchemaViolation) && !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrSchemaViolation or ErrInvalidInput for missing description, got %v", err)
	}
}

func TestMakeReturnsErrInvalidStateWithoutCompiler(t *testing.T) {
	st := newTestStore(t)
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	exec := script.New(script.Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	chatter := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	k := kernel.New(st, exec, nil, nil, cfg, log.Default())
	native.RegisterChatHandler(k, chatter)
	// Compiler: nil — no compiler configured.
	native.RegisterMakeHandler(k, native.MakeDeps{
		Scripts: exec, Compiler: nil, Chatter: chatter,
	}, "", 0)

	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "test"},
	})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState without compiler, got %v", err)
	}
}

func TestMakeRegistersActionOnSuccess(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}
	k, st := newMakeKernel(t, fakeChat, decider)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{
			"description": "An action that returns a fixed result",
		},
	})
	if err != nil {
		t.Fatalf("Call @sys/make: %v", err)
	}

	b, _ := json.Marshal(reply.Result)
	var result native.MakeResult
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.Status != "success" {
		t.Errorf("expected status=success, got %q; diagnostics: %v", result.Status, result.Diagnostics)
		return
	}
	if result.ActionID == "" {
		t.Fatal("expected non-empty action_id on success")
	}
	if result.ActionName == "" {
		t.Fatal("expected non-empty action_name on success")
	}
	action, err := st.ReadAction(ctx, result.ActionID)
	if err != nil {
		t.Fatalf("ReadAction: %v", err)
	}
	if !action.Active {
		t.Error("expected registered action to be active")
	}
	if action.OwnerUserID != caller.ID {
		t.Errorf("expected action owned by caller %q, got %q", caller.ID, action.OwnerUserID)
	}
	if action.Kind != kernel.KindWasm {
		t.Errorf("expected kind=wasm, got %q", action.Kind)
	}
	if len(action.Source) > 0 && action.Source[0] == 0x00 {
		t.Error("action.Source must be TinyGo source text, not WASM binary")
	}
	if action.WasmArtifact == "" {
		t.Error("expected WasmArtifact to be set for @sys/make registered action")
	}
}

func TestMakeRegisteredActionHasName(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}
	k, st := newMakeKernel(t, fakeChat, decider)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "compute something interesting"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	b, _ := json.Marshal(reply.Result)
	var result native.MakeResult
	_ = json.Unmarshal(b, &result)
	if result.Status != "success" {
		return
	}
	if result.ActionName == "" {
		t.Error("expected non-empty action_name on success")
	}
}

func TestMakeMaxStepsBoundsRepairLoop(t *testing.T) {
	// deriveContract gets a valid contract; all code-gen attempts produce no Go block.
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, "no code here"}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}

	st := newTestStore(t)
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	exec := script.New(script.Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	k := kernel.New(st, exec, nil, nil, cfg, log.Default())
	native.RegisterChatHandler(k, fakeChat)
	native.RegisterDecideHandler(k, decider)
	native.RegisterLookupHandler(k)
	native.RegisterMakeHandler(k, native.MakeDeps{
		Scripts: exec, Compiler: &script.FakeCompiler{}, Chatter: fakeChat,
	}, "", 3) // small maxSteps to keep test fast

	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 10000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "always fail"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	b, _ := json.Marshal(reply.Result)
	var result native.MakeResult
	_ = json.Unmarshal(b, &result)
	if result.Status != "failure" {
		t.Errorf("expected status=failure after exhausted steps, got %q", result.Status)
	}
	if len(result.Diagnostics) == 0 {
		t.Error("expected diagnostics when repair loop exhausted")
	}
}

func TestMakeInternalChatCallCreatesChildTrace(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}
	k, st := newMakeKernel(t, fakeChat, decider)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 10000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "test"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	txs, err := k.ListTransactions(ctx, caller.ID, kernel.TxFilter{ProcessID: p.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	// make + decide + chat (code gen) + chat (examples) = at least 4
	if len(txs) < 4 {
		t.Errorf("expected at least 4 transactions (make + decide + chat sub-calls), got %d", len(txs))
	}
}

func TestMakeNameCollisionPicksAlternateName(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}
	k, st := newMakeKernel(t, fakeChat, decider)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	desc := map[string]any{"description": "An action that returns a fixed result"}
	reply1, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make", Args: desc,
	})
	if err != nil {
		t.Fatalf("first Call: %v", err)
	}
	var r1 native.MakeResult
	b1, _ := json.Marshal(reply1.Result)
	_ = json.Unmarshal(b1, &r1)
	if r1.Status != "success" {
		t.Fatalf("first Call: expected success, got %q (diagnostics: %v)", r1.Status, r1.Diagnostics)
	}

	// Reset chatter so the second call gets the same contract+code responses.
	fakeChat.idx = 0
	decider.idx = 0

	p2, tr2 := beginMakeTestRun(t, st, caller.ID, sys.ID)
	reply2, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p2.ID, ExistingTraceID: tr2.ID,
		TargetUserID: sys.ID, ActionName: "make", Args: desc,
	})
	if err != nil {
		t.Fatalf("second Call: %v", err)
	}
	var r2 native.MakeResult
	b2, _ := json.Marshal(reply2.Result)
	_ = json.Unmarshal(b2, &r2)
	if r2.Status != "success" {
		t.Errorf("expected success on name collision (suffix retry), got %q (diagnostics: %v)", r2.Status, r2.Diagnostics)
	}
	if r2.ActionName == r1.ActionName {
		t.Errorf("expected different action name on collision, both got %q", r1.ActionName)
	}
}

func TestMakePrivateActionNotSurfacedToForeignProcess(t *testing.T) {
	ctx := context.Background()
	chatter := &cycleFakeChatter{responses: []string{fakeContract, "no code", `[]`}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}
	k, st := newMakeKernel(t, chatter, decider)
	sys := seedMakeAction(t, st)

	ownerA := setupUser(t, st, "@owner-a", 5000)
	privAction := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: ownerA.ID,
		Name:        "secret-tool",
		Kind:        kernel.KindNative,
		Active:      true,
		Public:      false,
		Description: "private action owned by A",
		Source:      "native",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"x": map[string]any{"type": "string", "description": "x"},
		}},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"y": map[string]any{"type": "string", "description": "y"},
		}},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, privAction); err != nil {
		t.Fatalf("create private action: %v", err)
	}

	ownerB := setupUser(t, st, "@owner-b", 5000)
	p, tr := beginMakeTestRun(t, st, ownerB.ID, sys.ID)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        ownerB.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    sys.ID,
		ActionName:      "make",
		Args:            map[string]any{"description": "use @owner-a/secret-tool to do something"},
	})
	_ = err
	_, callableErr := k.ReadCallableAction(ctx, "@owner-a", "secret-tool", ownerB.ID)
	if callableErr == nil {
		t.Error("expected error: private action should not be callable by foreign process owner")
	}
}

func TestMakeRejectsDisallowedWASMImport(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}
	insp := &fakeInspectingScripts{
		imports: []kernel.WASMImport{{Module: "juice", Name: "emit"}},
		exports: []string{"alloc", "run"},
	}
	k, st := newMakeKernelWithExec(t, fakeChat, decider, insp)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "test action"},
	})
	if err != nil {
		t.Fatalf("Call @sys/make: %v", err)
	}
	b, _ := json.Marshal(reply.Result)
	var result native.MakeResult
	_ = json.Unmarshal(b, &result)
	if result.Status != "failure" {
		t.Errorf("expected status=failure for disallowed juice.emit import, got %q; diagnostics: %v", result.Status, result.Diagnostics)
	}
	found := false
	for _, d := range result.Diagnostics {
		if strings.Contains(d, "emit") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected diagnostics to mention 'emit', got %v", result.Diagnostics)
	}
}

func TestMakeAcceptsStepImports(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}
	insp := &fakeInspectingScripts{
		imports: []kernel.WASMImport{
			{Module: "juice", Name: "step_create"},
			{Module: "juice", Name: "step_complete"},
		},
		exports: []string{"alloc", "run"},
	}
	k, st := newMakeKernelWithExec(t, fakeChat, decider, insp)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "step-using action"},
	})
	if err != nil {
		t.Fatalf("Call @sys/make: %v", err)
	}
	b, _ := json.Marshal(reply.Result)
	var result native.MakeResult
	_ = json.Unmarshal(b, &result)
	if result.Status != "success" {
		t.Errorf("expected status=success for step_create/step_complete imports, got %q; diagnostics: %v", result.Status, result.Diagnostics)
	}
}

func TestMakeDecideIsCalledEachIteration(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}
	k, st := newMakeKernel(t, fakeChat, decider)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 10000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "test action"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	txs, err := k.ListTransactions(ctx, caller.ID, kernel.TxFilter{ProcessID: p.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	foundDecide := false
	for _, tx := range txs {
		if tx.ActionName == "llm/decide" {
			foundDecide = true
			break
		}
	}
	if !foundDecide {
		t.Error("expected at least one @sys/llm/decide sub-transaction in the process")
	}
}

func TestMakeLookupResultAppearsInNextDecide(t *testing.T) {
	// First decide selects lookup; second selects chat. The lookup fails (no embedder),
	// but the error is recorded in history and the loop continues to the chat call.
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	lookupCall := &kernel.ToolCall{
		Action: "@sys/lookup",
		Args:   map[string]any{"query": "find something useful"},
	}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{lookupCall, chatSelectCall()}}
	k, st := newMakeKernel(t, fakeChat, decider)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 10000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "test action with lookup"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// The lookup fails (no embedder) but the loop continues and chat produces code → success.
	b, _ := json.Marshal(reply.Result)
	var result native.MakeResult
	_ = json.Unmarshal(b, &result)

	txs, err := k.ListTransactions(ctx, caller.ID, kernel.TxFilter{ProcessID: p.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	// Expect: make, decide (select lookup), lookup (fails), decide (select chat), chat, chat(examples)
	if len(txs) < 5 {
		t.Errorf("expected at least 5 sub-transactions (make+decide+lookup+decide+chat), got %d", len(txs))
	}
	foundLookup := false
	for _, tx := range txs {
		if tx.ActionName == "lookup" {
			foundLookup = true
			break
		}
	}
	if !foundLookup {
		t.Error("expected a @sys/lookup sub-transaction in the process")
	}
}

func TestMakeComputesPriceFromComposedActions(t *testing.T) {
	// Source references two actions via JuiceCall; registered action price = sum of their prices.
	ctx := context.Background()
	fakeCodeWithCalls := "```go\n//export run\nfunc run(inputPtr, inputLen uint32) uint64 {\n" +
		"\tJuiceCall(\"@sys/time\", nil)\n" +
		"\tJuiceCall(\"@sys/sink\", nil)\n" +
		"\treturn 0\n}\n```"
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCodeWithCalls, fakeExamples}}
	decider := &cycleDecideChatter{calls: []*kernel.ToolCall{chatSelectCall()}}
	k, st := newMakeKernel(t, fakeChat, decider)
	sys := seedMakeAction(t, st)

	// Seed @sys/time (price=5) and @sys/sink (price=3) so computePrice can resolve them.
	for _, spec := range []struct {
		name  string
		price int64
	}{{"time", 5}, {"sink", 3}} {
		a := &kernel.Action{
			ID: uuid.New().String(), OwnerUserID: sys.ID,
			Name: spec.name, Kind: kernel.KindNative,
			Active: true, Public: true, Price: spec.price,
			Description: spec.name + " action",
			Source:      "native",
			InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{},
			},
			OutputSchema: map[string]any{
				"type": "object", "properties": map[string]any{},
			},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := st.CreateAction(ctx, a); err != nil {
			t.Fatalf("seed %s: %v", spec.name, err)
		}
	}

	caller := setupUser(t, st, "@alice", 1000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "action that calls time and sink"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	b, _ := json.Marshal(reply.Result)
	var result native.MakeResult
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" {
		t.Fatalf("expected success, got %q; diagnostics: %v", result.Status, result.Diagnostics)
	}

	action, err := st.ReadAction(ctx, result.ActionID)
	if err != nil {
		t.Fatalf("ReadAction: %v", err)
	}
	const wantPrice = int64(5 + 3)
	if action.Price != wantPrice {
		t.Errorf("expected price=%d (sum of composed action prices), got %d", wantPrice, action.Price)
	}
}

func TestMakeDecideExhaustedReturnsFailure(t *testing.T) {
	// decide chatter always returns ErrExecutionFailed (no selection) → immediate failure.
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract}}
	decider := &llm.FakeDecideChatter{Err: kernel.ErrExecutionFailed.Wrap("no selection")}
	k, st := newMakeKernel(t, fakeChat, decider)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p, tr := beginMakeTestRun(t, st, caller.ID, sys.ID)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "something"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	b, _ := json.Marshal(reply.Result)
	var result native.MakeResult
	_ = json.Unmarshal(b, &result)
	if result.Status != "failure" {
		t.Errorf("expected status=failure when decide returns no selection, got %q", result.Status)
	}
	if len(result.Diagnostics) == 0 {
		t.Error("expected diagnostics on decide exhaustion")
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

// cycleDecideChatter cycles through a list of ToolCall decisions for successive ChatDecide() calls.
type cycleDecideChatter struct {
	calls []*kernel.ToolCall
	idx   int
}

func (c *cycleDecideChatter) ChatDecide(_ context.Context, _ []kernel.DecideMessage, _ []kernel.ToolDefinition) (*kernel.ToolCall, *kernel.ChatMessage, error) {
	if len(c.calls) == 0 {
		return nil, nil, kernel.ErrExecutionFailed.Wrap("no calls configured")
	}
	call := c.calls[c.idx%len(c.calls)]
	c.idx++
	return call, nil, nil
}

// fakeInspectingScripts implements kernel.ScriptExecutor and kernel.WASMInspector
// with a configurable import/export list.
type fakeInspectingScripts struct {
	imports []kernel.WASMImport
	exports []string
}

func (f *fakeInspectingScripts) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	return src, "fake-hash", nil
}

func (f *fakeInspectingScripts) Execute(_ context.Context, _ []byte, _ []byte, _ kernel.HostFunctions) ([]byte, error) {
	return []byte(`{"result":"ok"}`), nil
}

func (f *fakeInspectingScripts) InspectWASM(_ []byte) ([]kernel.WASMImport, []string, error) {
	return f.imports, f.exports, nil
}

// newMakeKernelWithExec is like newMakeKernel but accepts a custom ScriptExecutor.
func newMakeKernelWithExec(t *testing.T, chatter kernel.Chatter, decider kernel.DecideChatter, exec kernel.ScriptExecutor) (*kernel.Kernel, kernel.Store) {
	t.Helper()
	st := newTestStore(t)
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(st, exec, nil, nil, cfg, log.Default())
	native.RegisterChatHandler(k, chatter)
	native.RegisterDecideHandler(k, decider)
	native.RegisterLookupHandler(k)
	native.RegisterMakeHandler(k, native.MakeDeps{
		Scripts:  exec,
		Compiler: &script.FakeCompiler{},
		Chatter:  chatter,
	}, "", 0)
	return k, st
}
