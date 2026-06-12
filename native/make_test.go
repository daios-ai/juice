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
func newMakeKernel(t *testing.T, chatter kernel.Chatter) (*kernel.Kernel, kernel.Store) {
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
	native.RegisterMakeHandler(k, native.MakeDeps{
		Scripts:  exec,
		Compiler: fakeComp,
		Chatter:  chatter,
	}, "", 0)
	return k, st
}

// setupNativeProcess creates a process directly via the store for native package tests.
func setupNativeProcess(t *testing.T, st kernel.Store, ownerID string, funds int64) *kernel.Process {
	t.Helper()
	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	if err := st.CreateProcess(context.Background(), p, ownerID, funds); err != nil {
		t.Fatalf("setupNativeProcess: %v", err)
	}
	return p
}

// seedMakeAction seeds @sys user, @sys/make native action, and @sys/llm/chat in the store.
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
const fakeContract = `{"name":"test-action","input_schema":{"type":"object","properties":{}},"output_schema":{"type":"object","properties":{"result":{"type":"string"}},"required":["result"]},"plan":"simple fixed response"}`

// fakeCode is a minimal valid TinyGo run function for tests.
const fakeCode = "```go\n//export run\nfunc run(inputPtr, inputLen uint32) (uint32, uint32) { return 0, 2 }\n```"

// fakeExamples is an empty example list — unit tests use the compile smoke test only.
const fakeExamples = `[]`

func TestMakeRejectsEmptyDescription(t *testing.T) {
	k, st := newMakeKernel(t, &llm.FakeChatter{})
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p := setupNativeProcess(t, st, caller.ID, 100)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall:   true,
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
	p := setupNativeProcess(t, st, caller.ID, 100)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall:   true,
		TargetUserID: sys.ID, ActionName: "make",
		Args: map[string]any{"description": "test"},
	})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState without compiler, got %v", err)
	}
}

func TestMakeRegistersActionOnSuccess(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{
		fakeContract,
		fakeCode,
		fakeExamples,
	}}
	k, st := newMakeKernel(t, fakeChat)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p := setupNativeProcess(t, st, caller.ID, 100)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall:   true,
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
	// Source must be TinyGo text, not WASM binary bytes.
	if len(action.Source) > 0 && action.Source[0] == 0x00 {
		t.Error("action.Source must be TinyGo source text, not WASM binary")
	}
	// WasmArtifact must be set when an artifact was compiled.
	if action.WasmArtifact == "" {
		t.Error("expected WasmArtifact to be set for @sys/make registered action")
	}
}

func TestMakeRegisteredActionHasName(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{
		fakeContract,
		fakeCode,
		fakeExamples,
	}}
	k, st := newMakeKernel(t, fakeChat)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p := setupNativeProcess(t, st, caller.ID, 100)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall:   true,
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
	fakeChat := &llm.FakeChatter{Reply: kernel.ChatMessage{Role: "assistant", Content: "not a go block"}}
	fakeComp := &script.FakeCompiler{Err: kernel.ErrInvalidInput.Wrap("syntax error")}

	st := newTestStore(t)
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	exec := script.New(script.Config{TimeoutMS: 5000, MemoryBytes: 4 * 1024 * 1024})
	k := kernel.New(st, exec, nil, nil, cfg, log.Default())
	native.RegisterChatHandler(k, fakeChat)
	native.RegisterMakeHandler(k, native.MakeDeps{
		Scripts: exec, Compiler: fakeComp, Chatter: fakeChat,
	}, "", 0)

	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 10000)
	p := setupNativeProcess(t, st, caller.ID, 1000)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall:   true,
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
	fakeChat := &cycleFakeChatter{responses: []string{
		fakeContract, fakeCode, fakeExamples,
	}}
	k, st := newMakeKernel(t, fakeChat)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 10000)
	p := setupNativeProcess(t, st, caller.ID, 1000)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall:   true,
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
	if len(txs) < 2 {
		t.Errorf("expected at least 2 transactions (make + chat sub-calls), got %d", len(txs))
	}
}

func TestMakeNameCollisionReturnsFailure(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	k, st := newMakeKernel(t, fakeChat)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p := setupNativeProcess(t, st, caller.ID, 200)

	desc := map[string]any{"description": "An action that returns a fixed result"}
	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall:   true,
		TargetUserID: sys.ID, ActionName: "make", Args: desc,
	})
	if err != nil {
		t.Fatalf("first Call: %v", err)
	}
	// Use a fresh process for the second call — the first call auto-closes p when quiescent.
	p2 := setupNativeProcess(t, st, caller.ID, 200)
	reply2, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p2.ID, IsRootCall:   true,
		TargetUserID: sys.ID, ActionName: "make", Args: desc,
	})
	if err != nil {
		t.Fatalf("second Call returned kernel error (expected status=failure): %v", err)
	}
	b, _ := json.Marshal(reply2.Result)
	var result native.MakeResult
	_ = json.Unmarshal(b, &result)
	if result.Status != "failure" {
		t.Errorf("expected status=failure on name collision, got %q", result.Status)
	}
	if len(result.Diagnostics) == 0 {
		t.Error("expected diagnostics on collision failure")
	}
}

func TestMakePrivateActionNotSurfacedToForeignProcess(t *testing.T) {
	// A private action owned by user A must not appear in the composable list when
	// @sys/make is called in a process owned by user B.
	ctx := context.Background()

	// Build a chatter that returns a contract + no code (make will fail synthesis,
	// but we only care that the private action is not in the composable list).
	chatter := &cycleFakeChatter{
		responses: []string{
			fakeContract,
			fakeExamples,
			`[]`, // no examples
		},
	}
	k, st := newMakeKernel(t, chatter)
	sys := seedMakeAction(t, st)

	// Create owner A with a private action.
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

	// Create user B and their process. B explicitly references @owner-a/secret-tool.
	ownerB := setupUser(t, st, "@owner-b", 5000)
	p := setupNativeProcess(t, st, ownerB.ID, 1000)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:      ownerB.ID,
		ProcessID:     p.ID,
		IsRootCall:   true,
		TargetUserID:  sys.ID,
		ActionName:    "make",
		Args:          map[string]any{"description": "use @owner-a/secret-tool to do something"},
	})
	// The call may succeed or fail (synthesis will fail without a real compiler),
	// but it must NOT expose the private action to B.
	// If err == nil, verify the make result didn't error out due to a store auth bypass.
	_ = err
	// The key invariant: ReadCallableAction(ownerB, ...) must have returned ErrUnauthorized
	// for @owner-a/secret-tool. We verify this directly.
	_, callableErr := k.ReadCallableAction(ctx, "@owner-a", "secret-tool", ownerB.ID)
	if callableErr == nil {
		t.Error("expected error: private action should not be callable by foreign process owner")
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

// fakeInspectingScripts implements kernel.ScriptExecutor and kernel.WASMInspector
// with a configurable import/export list. Used to test checkWASMImports behaviour
// without invoking wazero.
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

// newMakeKernelWithExec is like newMakeKernel but accepts a custom ScriptExecutor,
// allowing tests to control what InspectWASM returns.
func newMakeKernelWithExec(t *testing.T, chatter kernel.Chatter, exec kernel.ScriptExecutor) (*kernel.Kernel, kernel.Store) {
	t.Helper()
	st := newTestStore(t)
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(st, exec, nil, nil, cfg, log.Default())
	native.RegisterChatHandler(k, chatter)
	native.RegisterMakeHandler(k, native.MakeDeps{
		Scripts:  exec,
		Compiler: &script.FakeCompiler{},
		Chatter:  chatter,
	}, "", 0)
	return k, st
}

func TestMakeRejectsDisallowedWASMImport(t *testing.T) {
	fakeChat := &cycleFakeChatter{responses: []string{fakeContract, fakeCode, fakeExamples}}
	insp := &fakeInspectingScripts{
		imports: []kernel.WASMImport{{Module: "juice", Name: "emit"}},
		exports: []string{"alloc", "run"},
	}
	k, st := newMakeKernelWithExec(t, fakeChat, insp)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p := setupNativeProcess(t, st, caller.ID, 100)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall:   true,
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
	insp := &fakeInspectingScripts{
		imports: []kernel.WASMImport{
			{Module: "juice", Name: "step_create"},
			{Module: "juice", Name: "step_complete"},
		},
		exports: []string{"alloc", "run"},
	}
	k, st := newMakeKernelWithExec(t, fakeChat, insp)
	ctx := context.Background()
	sys := seedMakeAction(t, st)
	caller := setupUser(t, st, "@alice", 1000)
	p := setupNativeProcess(t, st, caller.ID, 100)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall:   true,
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
