package kernel_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/native"
	"github.com/google/uuid"
)

func TestWasmTimeoutReturnsErrTimeout(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &sleepingFailExec{err: context.DeadlineExceeded})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "slow",
		Kind: kernel.KindWasm, Active: true, Price: 10,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	p, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "slow", Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrTimeout) {
		t.Errorf("expected ErrTimeout for DeadlineExceeded, got %v", err)
	}
}

// failingSubCallExec dispatches on source: "outer" calls the inner action but
// handles the failure gracefully (returns success); anything else returns an error.
type failingSubCallExec struct {
	targetUser   string
	targetAction string
}

func (f *failingSubCallExec) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	return src, "fakehash", nil
}

func (f *failingSubCallExec) Execute(ctx context.Context, src []byte, _ []byte, host kernel.HostFunctions) ([]byte, error) {
	if string(src) == "outer" {
		_, _ = host.Call(ctx, f.targetUser+"/"+f.targetAction, []byte(`{}`))
		return []byte(`{"handled":true}`), nil
	}
	return nil, kernel.ErrExecutionFailed.Wrap("inner always fails")
}

func TestSubCostNotIncrementedOnFailedSubCall(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice-vat", 500)
	bob := setupUser(t, st, "@bob-vat", 0)
	feeUser := setupUser(t, st, "@fee-vat", 0)
	carol := setupUser(t, st, "@carol-vat", 50)

	inner := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "inner",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Public: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, inner)
	outer := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "outer",
		Kind: kernel.KindWasm, Source: "outer", Active: true, Public: true, Price: 50,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outer)

	exec := &failingSubCallExec{targetUser: bob.ID, targetAction: "inner"}
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeBPS = 2000
	cfg.FeeRecipientID = feeUser.ID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(st, exec, nil, nil, cfg, nil)

	p, tr := beginTestRun(t, st, carol.ID, outer)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: carol.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "outer", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("outer call should succeed when it handles sub-call failure: %v", err)
	}

	// subCost must be 0 (failed sub-call not counted), so taxable == gross == 50.
	// fee = ceil(50 * 2000 / 10000) = 10, net = 40.
	tx, err := st.ReadTransaction(ctx, reply.TxID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Fee != 10 {
		t.Errorf("fee: got %d, want 10 (full fee on gross=50 with feeBPS=2000)", tx.Fee)
	}
	if tx.Net != 40 {
		t.Errorf("net: got %d, want 40", tx.Net)
	}
}

// ---- Call invariant tests ----

func TestCallClosedProcessFails(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 1000)
	target := setupUser(t, st, "@bob", 0)
	_ = setupAction(t, st, target.ID, "echo", 0)

	p := setupProcess(t, st, owner.ID, 100)
	_ = k.EndProcess(ctx, owner.ID, p.ID)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:     owner.ID,
		ProcessID:    p.ID,
		TargetUserID: target.ID,
		ActionName:   "echo",
		Args:         map[string]any{},
	})
	if err == nil {
		t.Error("expected error calling on closed process")
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Code != "invalid_state" {
		t.Errorf("expected invalid_state error, got %v", err)
	}
}

func TestCallInactiveActionDeniedForNonOwner(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)

	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: bob.ID,
		Name:        "svc",
		Kind:        kernel.KindNative,
		Active:      false,
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        alice.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    bob.ID,
		ActionName:      "svc",
		Args:            map[string]any{},
	})
	if err == nil {
		t.Error("expected error calling inactive action as non-owner")
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Code != "invalid_state" {
		t.Errorf("expected invalid_state error, got %v", err)
	}
}


func TestCallPrivateDenied(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	setupAction(t, st, bob.ID, "private", 0)

	// Alice (not the owner) tries to run bob's private action via Run, which enforces CanCall.
	_, err := k.Run(ctx, alice.ID, "@bob/private", map[string]any{})
	if err == nil {
		t.Error("expected call denial for private action owned by another user")
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Code != "unauthorized" {
		t.Errorf("expected unauthorized error, got %v", err)
	}
}

func TestCallPublicActionAnyOwner(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: bob.ID,
		Name:        "svc",
		Kind:        kernel.KindWasm,
		Active:      true,
		Public:      true,
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, tr := beginTestRun(t, st, alice.ID, a)
	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        alice.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    bob.ID,
		ActionName:      "svc",
		Args:            map[string]any{},
	})
	if err != nil {
		t.Fatalf("public action should be callable by any process owner: %v", err)
	}
}

func TestCallPrivateActionOwnerOnly(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 1000)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: bob.ID,
		Name:        "priv",
		Kind:        kernel.KindWasm,
		Active:      true,
		Public:      false,
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	// Bob (the owner) can call his own private action.
	pBob, trBob := beginTestRun(t, st, bob.ID, a)
	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        bob.ID,
		ProcessID:       pBob.ID,
		ExistingTraceID: trBob.ID,
		TargetUserID:    bob.ID,
		ActionName:      "priv",
		Args:            map[string]any{},
	})
	if err != nil {
		t.Fatalf("owner should call their own private action: %v", err)
	}

	// Alice (not the owner) cannot run bob's private action. Validated by Run → beginRun.
	_, err = k.Run(ctx, alice.ID, "@bob/priv", map[string]any{})
	if err == nil {
		t.Error("non-owner should not be able to call private action")
	}
}

func TestCallInactiveActionBlocked(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	_ = st.CreateAction(ctx, &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "inactive",
		Kind: kernel.KindWasm, Active: false, Public: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	// Run enforces CanCall (which requires active=true) in beginRun.
	_, err := k.Run(ctx, alice.ID, "@alice/inactive", map[string]any{})
	if err == nil {
		t.Error("inactive action should be blocked regardless of public flag")
	}
}

func TestCallInsufficientFunds(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 50)
	_ = setupAction(t, st, alice.ID, "expensive", 200)

	_, err := k.Run(ctx, alice.ID, "@alice/expensive", map[string]any{})
	if err == nil {
		t.Error("expected insufficient funds error")
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Code != "insufficient_funds" {
		t.Errorf("expected insufficient_funds error, got %v", err)
	}
}

func TestCallGrossEqualsNetPlusFee(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 2000)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "paid",
		Kind:        kernel.KindWasm,
		Active:      true,
		Price:       100,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, tr := beginTestRun(t, st, alice.ID, a)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        alice.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    alice.ID,
		ActionName:      "paid",
		Args:            map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	tx, err := st.ReadTransaction(ctx, reply.TxID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Gross != tx.Net+tx.Fee {
		t.Errorf("invariant broken: gross=%d, net=%d, fee=%d", tx.Gross, tx.Net, tx.Fee)
	}
	if tx.Gross != 100 {
		t.Errorf("gross: got %d, want 100", tx.Gross)
	}
}

func TestCallCreatesExactlyOneTransaction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "svc",
		Kind:        kernel.KindWasm,
		Active:      true,
		Price:       10,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, tr := beginTestRun(t, st, alice.ID, a)
	beforeTxs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	before := len(beforeTxs)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        alice.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    alice.ID,
		ActionName:      "svc",
		Args:            map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	afterTxs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	if len(afterTxs)-before != 1 {
		t.Errorf("expected exactly 1 new transaction, got %d", len(afterTxs)-before)
	}
}

func TestCallCreatesChildTrace(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "svc",
		Kind: kernel.KindWasm, Active: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, tr := beginTestRun(t, st, alice.ID, a)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        alice.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    alice.ID,
		ActionName:      "svc",
		Args:            map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.TraceID == "" {
		t.Error("trace ID should be non-empty")
	}
	callTrace, err := st.ReadTrace(ctx, reply.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-created root trace has no parent.
	if callTrace.ParentTraceID != nil {
		t.Errorf("root call trace ParentTraceID: got %v, want nil", callTrace.ParentTraceID)
	}
}

func TestCallFailureRefundsFunds(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{err: kernel.ErrExecutionFailed.Wrap("boom")})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "risky",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        alice.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    alice.ID,
		ActionName:      "risky",
		Args:            map[string]any{},
	})
	if err == nil {
		t.Fatal("expected execution failure")
	}

	// Process auto-closes after failure (quiescent). Funds return to alice.
	alice2, _ := st.ReadUser(ctx, alice.ID)
	if alice2.Locked != 0 {
		t.Errorf("user.locked after failure+close: got %d, want 0", alice2.Locked)
	}
	if alice2.Available != 1000 {
		t.Errorf("user.available after failure+close: got %d, want 1000", alice2.Available)
	}
}

func TestWasmPanicRefundsFunds(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &panicScriptExec{})
	ctx := context.Background()

	alice := setupUser(t, st, "@wasm-panic-alice", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "panic-svc",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        alice.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    alice.ID,
		ActionName:      "panic-svc",
		Args:            map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error from panicking WASM executor")
	}

	// Process auto-closes after panic (quiescent). Funds return to alice.
	alice2, _ := st.ReadUser(ctx, alice.ID)
	if alice2.Locked != 0 {
		t.Errorf("user.locked after wasm panic+close: got %d, want 0", alice2.Locked)
	}
	if alice2.Available != 500 {
		t.Errorf("user.available after wasm panic+close: got %d, want 500", alice2.Available)
	}
}

// TestCallNestedTraceTree verifies that the store correctly records parent/child trace IDs
// for a root → subcall → sub-subcall chain. Uses store-level setup to avoid auto-close
// (which fires when a committed root trace has no open work).
func TestCallNestedTraceTree(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 0)
	p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: alice.ID,
		Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
	traceA := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: alice.ID, CallerUserID: alice.ID, CreatedAt: time.Now().UTC()}
	if err := st.BeginRun(ctx, p, traceA, alice.ID, 0); err != nil {
		t.Fatalf("BeginRun A: %v", err)
	}
	traceB := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: alice.ID, CallerUserID: alice.ID, CreatedAt: time.Now().UTC()}
	if err := st.BeginSubcall(ctx, traceA.ID, traceB, 0); err != nil {
		t.Fatalf("BeginSubcall B: %v", err)
	}
	traceC := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: alice.ID, CallerUserID: alice.ID, CreatedAt: time.Now().UTC()}
	if err := st.BeginSubcall(ctx, traceB.ID, traceC, 0); err != nil {
		t.Fatalf("BeginSubcall C: %v", err)
	}

	gotA, _ := st.ReadTrace(ctx, traceA.ID)
	gotB, _ := st.ReadTrace(ctx, traceB.ID)
	gotC, _ := st.ReadTrace(ctx, traceC.ID)

	if gotA.ParentTraceID != nil {
		t.Errorf("traceA.ParentTraceID: got %v, want nil (root)", gotA.ParentTraceID)
	}
	if gotB.ParentTraceID == nil || *gotB.ParentTraceID != traceA.ID {
		t.Errorf("traceB.ParentTraceID: got %v, want %q", gotB.ParentTraceID, traceA.ID)
	}
	if gotC.ParentTraceID == nil || *gotC.ParentTraceID != traceB.ID {
		t.Errorf("traceC.ParentTraceID: got %v, want %q", gotC.ParentTraceID, traceB.ID)
	}
	if gotA.ProcessID != p.ID || gotB.ProcessID != p.ID || gotC.ProcessID != p.ID {
		t.Error("all traces must belong to the same process")
	}

	seen := map[string]bool{}
	for _, id := range []string{traceA.ID, traceB.ID, traceC.ID} {
		if seen[id] {
			t.Errorf("duplicate trace ID %q", id)
		}
		seen[id] = true
	}
}

func TestCallInputSchemaRejection(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "strict",
		Kind:        kernel.KindWasm,
		Active:      true,
		Price:       0,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"name"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	// Use Run, which enforces input schema validation in beginRun.
	_, err := k.Run(ctx, alice.ID, "@alice/strict", map[string]any{"wrong_field": "value"})
	if err == nil {
		t.Error("expected schema violation error for missing required field")
	}
}

func TestCallOutputSchemaRejection(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"unexpected_field": 42}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "typed",
		Kind:        kernel.KindWasm,
		Active:      true,
		Price:       50,
		OutputSchema: map[string]any{
			"type":     "object",
			"required": []any{"result"},
			"properties": map[string]any{
				"result": map[string]any{"type": "string"},
			},
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	p, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "typed", Args: map[string]any{},
	})
	if err == nil {
		t.Error("expected schema violation error for bad output")
	}
	// Process auto-closes after failure (quiescent). Funds return to alice.
	alice2, _ := st.ReadUser(ctx, alice.ID)
	if alice2.Locked != 0 {
		t.Errorf("user.locked after output schema rejection+close: got %d, want 0", alice2.Locked)
	}
	if alice2.Available != 1000 {
		t.Errorf("user.available after output schema rejection+close: got %d, want 1000", alice2.Available)
	}
}

func TestFailedExecutionUpdatesTraceLatencyNotCost(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &sleepingFailExec{
		delay: 20 * time.Millisecond,
		err:   fmt.Errorf("boom"),
	})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "fails",
		Kind:        kernel.KindWasm,
		Active:      true,
		Price:       50,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	p, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "fails", Args: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected execution failure")
	}

	txs, err := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 1 {
		t.Fatalf("transactions: got %d, want 1", len(txs))
	}
	tx := txs[0]
	if tx.Status != kernel.TxFailure {
		t.Fatalf("tx status: got %s, want failure", tx.Status)
	}
	// Gross is the action price attempted (50), even for failed executions.
	if tx.Gross != 50 {
		t.Fatalf("failed tx gross: got %d, want 50 (the action price)", tx.Gross)
	}

	// The pre-created root trace has no parent.
	callTrace, err := st.ReadTrace(ctx, tx.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if callTrace.LatencyMS <= 0 {
		t.Fatalf("call trace latency should be updated on failure, got %d", callTrace.LatencyMS)
	}
	if callTrace.ParentTraceID != nil {
		t.Fatalf("root call trace should have nil ParentTraceID, got %v", callTrace.ParentTraceID)
	}
}

type sleepingFailExec struct {
	delay time.Duration
	err   error
}

func (s *sleepingFailExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (s *sleepingFailExec) Execute(_ context.Context, _ []byte, _ []byte, _ kernel.HostFunctions) ([]byte, error) {
	time.Sleep(s.delay)
	return nil, s.err
}

func TestWasmHostCallPrivateActionDenied(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)

	innerAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "private",
		Kind: kernel.KindNative, Active: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, innerAction)

	outerAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "outer",
		Kind: kernel.KindWasm, Active: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outerAction)

	exec := &hostCallExec{targetUser: bob.ID, targetAction: "private"}
	k := newTestKernelWithScripts(st, exec)
	p, tr := beginTestRun(t, st, alice.ID, outerAction)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "outer", Args: map[string]any{},
	})
	if err == nil {
		t.Error("expected denial when script subcalls a private action not owned by the process owner")
	}
}

type hostCallExec struct {
	targetUser   string
	targetAction string
}

func (h *hostCallExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (h *hostCallExec) Execute(ctx context.Context, _ []byte, _ []byte, host kernel.HostFunctions) ([]byte, error) {
	result, err := host.Call(ctx, h.targetUser+"/"+h.targetAction, []byte(`{}`))
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ---- Process-funded subcall execution model ----

// subcallExec dispatches based on source: "outer" makes a sub-call, anything else returns {"ok":true}.
type subcallExec struct {
	targetUser   string
	targetAction string
}

func (c *subcallExec) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	return src, "fakehash", nil
}

func (c *subcallExec) Execute(ctx context.Context, src []byte, _ []byte, host kernel.HostFunctions) ([]byte, error) {
	if string(src) == "outer" {
		result, err := host.Call(ctx, c.targetUser+"/"+c.targetAction, []byte(`{}`))
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return []byte(`{"ok":true}`), nil
}

func TestProcessFundedSubCallSpendsSameProcess(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 500)
	feeUser := setupUser(t, st, "@fee-recipient", 0)

	inner := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "inner",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Public: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, inner)
	// outer.Price = 150: covers outer's own cost (50) plus the subcall to inner (100).
	// In the trace-level wallet model, subcalls draw from the parent trace's available.
	outer := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "outer",
		Kind: kernel.KindWasm, Source: "outer", Active: true, Price: 150,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outer)

	exec := &subcallExec{targetUser: bob.ID, targetAction: "inner"}
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeBPS = 2000
	cfg.FeeRecipientID = feeUser.ID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(st, exec, nil, nil, cfg, nil)

	p, tr := beginTestRun(t, st, alice.ID, outer)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "outer", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	// Process funded outer (150) which covers outer + inner subcall; available = 0.
	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Available != 0 {
		t.Errorf("process.available: got %d, want 0", proc.Available)
	}
	if proc.Locked != 0 {
		t.Errorf("process.locked: got %d, want 0", proc.Locked)
	}
}

func TestProcessFundedSubCallInsufficientFundsFails(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 50) // enough for outer only, not inner
	bob := setupUser(t, st, "@bob", 0)

	inner := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "inner",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Public: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, inner)
	outer := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "outer",
		Kind: kernel.KindWasm, Source: "outer", Active: true, Price: 50,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outer)

	exec := &subcallExec{targetUser: bob.ID, targetAction: "inner"}
	k := newTestKernelWithScripts(st, exec)

	// Fund only enough for outer, not inner.
	p, tr := beginTestRun(t, st, alice.ID, outer)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "outer", Args: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error when process has insufficient funds for subcall")
	}

	// Process auto-closes after failure (quiescent). Funds return to alice.
	alice2, _ := st.ReadUser(ctx, alice.ID)
	if alice2.Locked != 0 {
		t.Errorf("user.locked after insufficient-funds failure+close: got %d, want 0", alice2.Locked)
	}
	if alice2.Available != 50 {
		t.Errorf("user.available after insufficient-funds failure+close: got %d, want 50", alice2.Available)
	}
}

func TestProcessFundedSubCallTraceHasSameProcess(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 0)
	bob := setupUser(t, st, "@bob", 0)

	inner := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "inner",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Public: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, inner)
	outer := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "outer",
		Kind: kernel.KindWasm, Source: "outer", Active: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outer)

	exec := &subcallExec{targetUser: bob.ID, targetAction: "inner"}
	k := newTestKernelWithScripts(st, exec)

	p, tr := beginTestRun(t, st, alice.ID, outer)
	outerReply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "outer", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	traces, _ := st.ListTraces(ctx, p.ID)
	// Expect outer trace (root call) + inner subcall trace = 2 traces total, all in same process.
	if len(traces) < 2 {
		t.Fatalf("expected at least 2 traces, got %d", len(traces))
	}
	for _, tr := range traces {
		if tr.ProcessID != p.ID {
			t.Errorf("trace %s has process %s, want %s", tr.ID, tr.ProcessID, p.ID)
		}
	}

	// Subcall trace parent must be the outer call trace.
	outerTrace, _ := st.ReadTrace(ctx, outerReply.TraceID)
	for _, tr := range traces {
		if tr.ID == outerReply.TraceID {
			continue
		}
		// Must be the inner subcall trace.
		if tr.ParentTraceID == nil || *tr.ParentTraceID != outerTrace.ID {
			t.Errorf("subcall trace parent: got %v, want %q", tr.ParentTraceID, outerTrace.ID)
		}
	}
}

func TestRootTraceHasNilParent(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "noop",
		Kind: kernel.KindWasm, Active: true, Price: 0, Source: "fake",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	p, tr := beginTestRun(t, st, alice.ID, a)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "noop", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	rootTrace, err := st.ReadTrace(ctx, reply.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if rootTrace.ParentTraceID != nil {
		t.Errorf("root trace ParentTraceID should be nil, got %v", rootTrace.ParentTraceID)
	}
}

// ---- Accounting (ComputeFee is defined in call.go) ----

func TestComputeFee(t *testing.T) {
	tests := []struct {
		taxable, feeBPS, wantNet, wantFee int64
	}{
		{0, 2000, 0, 0},
		{100, 2000, 80, 20},
		{1, 2000, 0, 1},
		{5, 2000, 4, 1},
		{1000, 2000, 800, 200},
		{1, 0, 1, 0},
		{100, 0, 100, 0},
		{100, 10000, 0, 100},
	}
	for _, tc := range tests {
		net, fee := kernel.ComputeFee(tc.taxable, tc.feeBPS)
		if net != tc.wantNet || fee != tc.wantFee {
			t.Errorf("ComputeFee(%d, %d) = (%d, %d), want (%d, %d)",
				tc.taxable, tc.feeBPS, net, fee, tc.wantNet, tc.wantFee)
		}
		if tc.taxable > 0 && net+fee != tc.taxable {
			t.Errorf("invariant broken: taxable=%d net=%d fee=%d", tc.taxable, net, fee)
		}
	}
}

func TestComputeFeeInvariant(t *testing.T) {
	for taxable := int64(0); taxable <= 10000; taxable++ {
		net, fee := kernel.ComputeFee(taxable, 2000)
		if net+fee != taxable {
			t.Fatalf("taxable=%d: net(%d)+fee(%d) != taxable", taxable, net, fee)
		}
		if net < 0 || fee < 0 {
			t.Fatalf("taxable=%d: negative component net=%d fee=%d", taxable, net, fee)
		}
	}
}

// failingCommitStore wraps a kernel.Store and makes CommitCall always fail,
// to verify no transaction is committed on settlement failure.
type failingCommitStore struct {
	kernel.Store
	calls int
}

func (f *failingCommitStore) CommitCall(ctx context.Context, tx *kernel.Transaction, receipt *kernel.Receipt, traceID, callerWalletID, callerWalletKind, targetUserID, feeRecipientID string, net, fee int64, stats *kernel.Stats, idempotencyRecordID, stepID string) error {
	f.calls++
	if f.calls > 0 {
		return kernel.ErrInternal.Wrap("injected commit failure")
	}
	return f.Store.CommitCall(ctx, tx, receipt, traceID, callerWalletID, callerWalletKind, targetUserID, feeRecipientID, net, fee, stats, idempotencyRecordID, stepID)
}

func TestCommitCallAtomicOnFailure(t *testing.T) {
	base := newTestStore(t)
	failing := &failingCommitStore{Store: base}
	k := newTestKernel(failing)
	ctx := context.Background()

	caller := setupUser(t, base, "@caller", 1000)
	actionOwner := setupUser(t, base, "@owner", 0)
	a := setupAction(t, base, actionOwner.ID, "echo", 100)
	a.Public = true
	_ = base.UpdateAction(ctx, a)

	p, tr := beginTestRun(t, base, caller.ID, a)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: actionOwner.ID, ActionName: "echo", Args: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error from injected commit failure")
	}

	// Execution fails (no native executor) → CommitFailedCall runs → process auto-closes.
	// Funds return to caller's user balance.
	caller2, _ := base.ReadUser(ctx, caller.ID)
	if caller2.Locked != 0 {
		t.Errorf("caller user.locked after call failure+close: got %d, want 0", caller2.Locked)
	}
	if caller2.Available != 1000 {
		t.Errorf("caller user.available after call failure+close: got %d, want 1000", caller2.Available)
	}

	// No success transaction must exist.
	txs, _ := base.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	for _, tx := range txs {
		if tx.Status == kernel.TxSuccess {
			t.Errorf("found committed success transaction despite call failure: %s", tx.ID)
		}
	}
}

// ---- Trace validation precondition tests ----

func TestCallInvalidParentTraceDoesNotLockFunds(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := setupAction(t, st, alice.ID, "svc", 100)
	_ = a

	p := setupProcess(t, st, alice.ID, 500)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:      alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: "nonexistent-trace-id",
		TargetUserID:  alice.ID,
		ActionName:    "svc",
		Args:          map[string]any{},
	})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for bad parent trace, got %v", err)
	}

	// Funds must be untouched — no locking should have occurred.
	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Available != 500 {
		t.Errorf("process.available: got %d, want 500 (funds locked before trace validated)", proc.Available)
	}
	if proc.Locked != 0 {
		t.Errorf("process.locked: got %d, want 0 (funds locked before trace validated)", proc.Locked)
	}
}

func TestCallCrossProcessParentTraceRejectedForOwner(t *testing.T) {
	// Even a process owner must not supply a parent trace from a different process
	// on a non-step-completion call: doing so would mutate ancestor cost/latency
	// in the foreign process (B1 fix).
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "svc",
		Kind: kernel.KindWasm, Active: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p := setupProcess(t, st, alice.ID, 0)
	// Run a call to get a trace from a different (auto-created) process.
	otherReply, err := k.Run(ctx, alice.ID, "@alice/svc", map[string]any{})
	if err != nil {
		t.Fatalf("setup call in otherP: %v", err)
	}

	_, err = k.Call(ctx, kernel.CallRequest{
		CallerID:      alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: otherReply.TraceID, // cross-process — must be rejected
		TargetUserID:  alice.ID,
		ActionName:    a.Name,
		Args:          map[string]any{},
	})
	if err == nil {
		t.Fatal("process owner with cross-process parent trace should be rejected")
	}
}

func TestCallCrossProcessParentTraceRejectedForNonOwner(t *testing.T) {
	// A non-owner caller whose action appears in a different process's trace must not
	// gain authority over the target process via that foreign trace (F2 fix).
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	procOwner := setupUser(t, st, "@f2-proc-owner", 500)
	actionOwner := setupUser(t, st, "@f2-action-owner", 0)
	targetAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: procOwner.ID, Name: "f2-target",
		Kind: kernel.KindWasm, Active: true, Public: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	callerAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: actionOwner.ID, Name: "f2-caller",
		Kind: kernel.KindWasm, Active: true, Public: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, targetAction)
	_ = st.CreateAction(ctx, callerAction)

	// p1 and p2 are both owned by procOwner.
	// p2 uses callerAction so its root trace has action_owner_id=actionOwner.ID (callerAction.OwnerUserID),
	// granting actionOwner trace-scoped authority over p2 in the positive test below.
	p1, p1tr := beginTestRun(t, st, procOwner.ID, callerAction)
	p2, p2Orphan := beginTestRun(t, st, procOwner.ID, callerAction)

	// Create a trace in p1 owned by actionOwner (by calling callerAction in p1).
	reply1, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        procOwner.ID,
		ProcessID:       p1.ID,
		ExistingTraceID: p1tr.ID,
		TargetUserID:    actionOwner.ID,
		ActionName:      "f2-caller",
		Args:            map[string]any{},
	})
	if err != nil {
		t.Fatalf("setup call in p1: %v", err)
	}
	p1TraceID := reply1.TraceID

	// actionOwner tries to call in p2 using the p1 trace for authority — must fail.
	_, err = k.Call(ctx, kernel.CallRequest{
		CallerID:      actionOwner.ID,
		ProcessID:     p2.ID,
		ParentTraceID: p1TraceID, // trace is in p1, not p2
		TargetUserID:  procOwner.ID,
		ActionName:    "f2-target",
		Args:          map[string]any{},
	})
	// The process-membership check fires before the authorization check, so the error
	// is ErrInvalidInput (cross-process parent) rather than ErrUnauthorized.
	if err == nil {
		t.Error("expected error for cross-process trace authority, got nil")
	}

	// Using the p2 root trace (action_owner_id=actionOwner.ID via callerAction) should succeed.
	p2TraceID := p2Orphan.ID

	_, err = k.Call(ctx, kernel.CallRequest{
		CallerID:      actionOwner.ID,
		ProcessID:     p2.ID,
		ParentTraceID: p2TraceID, // same process trace → allowed
		TargetUserID:  procOwner.ID,
		ActionName:    "f2-target",
		Args:          map[string]any{},
	})
	if err != nil {
		t.Errorf("same-process trace authority should succeed, got %v", err)
	}
}

// ---- CommitFailedCall settlement error tests ----

type failingCommitFailedCallStore struct {
	kernel.Store
}

func (f *failingCommitFailedCallStore) CommitFailedCall(_ context.Context, _ *kernel.Transaction, _ func(int64) (*kernel.Receipt, error), _, _, _ string, _ int64, _ *kernel.Stats, _, _, _ string) error {
	return kernel.ErrInternal.Wrap("injected CommitFailedCall failure")
}

func TestCommitFailedCallSettlementError(t *testing.T) {
	base := newTestStore(t)
	failing := &failingCommitFailedCallStore{Store: base}
	k := newTestKernelWithScripts(failing, &fakeScriptExec{err: kernel.ErrExecutionFailed.Wrap("boom")})
	ctx := context.Background()

	alice := setupUser(t, base, "@alice", 1000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "risky",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = base.CreateAction(ctx, a)

	p, tr := beginTestRun(t, base, alice.ID, a)
	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "risky", Args: map[string]any{},
	})

	// When CommitFailedCall fails, Call must return ErrInternal (not the original exec error).
	if !errors.Is(err, kernel.ErrInternal) {
		t.Errorf("expected ErrInternal when CommitFailedCall fails, got %v", err)
	}
}

func TestFailedCallReceiptChargeMatchesCommittedCharge(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{err: kernel.ErrExecutionFailed.Wrap("boom")})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice-rcpt", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "fail-act",
		Kind: kernel.KindWasm, Active: true, Price: 200,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	p, tr := beginTestRun(t, st, alice.ID, a)
	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "fail-act", Args: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error from failing executor")
	}

	txs, listErr := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	if listErr != nil || len(txs) == 0 {
		t.Fatalf("expected a failure transaction, got %v / %v", txs, listErr)
	}
	tx := txs[0]
	receipt, rErr := st.ReadReceiptByTxID(ctx, tx.ID)
	if rErr != nil {
		t.Fatalf("ReadReceiptByTxID: %v", rErr)
	}
	// Pure execution failure with no subcalls or steps: trace.available = gross at failure,
	// so refund = gross, charge = gross - refund = 0.
	// This verifies the buildReceipt callback received the atomically-computed refund.
	if receipt.Gross != tx.Gross {
		t.Errorf("receipt.Gross=%d != tx.Gross=%d", receipt.Gross, tx.Gross)
	}
	if receipt.Charge != 0 {
		t.Errorf("receipt.Charge=%d, want 0 (full refund on pure execution failure)", receipt.Charge)
	}
}

// ---- llm/chat native action tests ----

type fakeChatter struct {
	reply kernel.ChatMessage
}

func (f *fakeChatter) Chat(_ context.Context, _ []kernel.ChatMessage) (kernel.ChatMessage, error) {
	return f.reply, nil
}

func newTestKernelWithChatter(st kernel.Store, c kernel.Chatter) *kernel.Kernel {
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(st, nil, nil, nil, cfg, nil)
	native.RegisterChatHandler(k, c)
	return k
}

func TestCallLLMChat(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	fc := &fakeChatter{reply: kernel.ChatMessage{Role: "assistant", Content: "hello there"}}
	k := newTestKernelWithChatter(st, fc)

	owner := setupUser(t, st, "@sys", 1000)
	chatAction := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "llm/chat",
		Kind:         kernel.KindNative,
		Active:       true,
		Price:        0,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, chatAction)

	p, tr := beginTestRun(t, st, owner.ID, chatAction)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        owner.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    owner.ID,
		ActionName:      "llm/chat",
		Args: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Call llm/chat: %v", err)
	}
	msg, ok := reply.Result["message"].(map[string]any)
	if !ok {
		t.Fatalf("expected message in result, got %v", reply.Result)
	}
	if msg["content"] != "hello there" {
		t.Errorf("content: got %q, want %q", msg["content"], "hello there")
	}
}

func TestCallLLMChatNoChatter(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	k := newTestKernel(st)
	native.RegisterChatHandler(k, nil)

	owner := setupUser(t, st, "@sys", 1000)
	chatAction := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "llm/chat",
		Kind:         kernel.KindNative,
		Active:       true,
		Price:        0,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, chatAction)

	p, tr := beginTestRun(t, st, owner.ID, chatAction)
	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        owner.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    owner.ID,
		ActionName:      "llm/chat",
		Args:            map[string]any{"messages": []any{}},
	})
	if err == nil {
		t.Fatal("expected error when no chatter configured")
	}
}

func TestCallSuspendedSubjectRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 1000)
	target := setupUser(t, st, "@bob", 0)
	a := setupAction(t, st, target.ID, "echo", 0)

	p, tr := beginTestRun(t, st, owner.ID, a)

	if err := st.SuspendUser(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        owner.ID,
		ProcessID:       p.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    target.ID,
		ActionName:      "echo",
		Args:            map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error for suspended subject")
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Code != "unauthenticated" {
		t.Errorf("expected unauthenticated error, got %v", err)
	}
}

// ---- Fee recipient enforcement ----

func TestCallWithFeeAndNoRecipientRejected(t *testing.T) {
	// The invariant fee_bps > 0 => fee_recipient_id != "" is now enforced at
	// startup via ValidateFeeRecipient, not at call time.
	ctx := context.Background()

	t.Run("missing recipient rejected at startup", func(t *testing.T) {
		st := newTestStore(t)
		cfg := kernel.DefaultConfig()
		cfg.FeeBPS = 2000 // 20% fee — no FeeRecipientID set
		k := kernel.New(st, nil, nil, nil, cfg, nil)
		if err := k.ValidateFeeRecipient(ctx); !errors.Is(err, kernel.ErrInvalidState) {
			t.Errorf("expected ErrInvalidState for fee_bps>0 with empty recipient, got %v", err)
		}
	})

	t.Run("nonexistent recipient rejected at startup", func(t *testing.T) {
		st := newTestStore(t)
		cfg := kernel.DefaultConfig()
		cfg.FeeBPS = 2000
		cfg.FeeRecipientID = "no-such-user"
		k := kernel.New(st, nil, nil, nil, cfg, nil)
		if err := k.ValidateFeeRecipient(ctx); !errors.Is(err, kernel.ErrInvalidState) {
			t.Errorf("expected ErrInvalidState for unknown fee recipient, got %v", err)
		}
	})

	t.Run("zero fee_bps passes with no recipient", func(t *testing.T) {
		st := newTestStore(t)
		cfg := kernel.DefaultConfig()
		cfg.FeeBPS = 0
		k := kernel.New(st, nil, nil, nil, cfg, nil)
		if err := k.ValidateFeeRecipient(ctx); err != nil {
			t.Errorf("expected no error when fee_bps=0, got %v", err)
		}
	})
}

// TestZeroPriceCallOnClosedProcessReturnsErrInvalidState verifies that
// BeginCall enforces the open-process invariant atomically even when price == 0.
func TestZeroPriceCallOnClosedProcessReturnsErrInvalidState(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@owner", 0)
	a := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "free",
		Kind:         kernel.KindHTTP,
		Active:       true,
		Price:        0,
		Public:       true,
		Source:       "http://example.com",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	p := setupProcess(t, st, owner.ID, 0)
	if err := k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}

	_, callErr := k.Call(ctx, kernel.CallRequest{
		CallerID:     owner.ID,
		ProcessID:    p.ID,
		TargetUserID: owner.ID,
		ActionName:   "free",
		Args:         map[string]any{},
	})
	if !errors.Is(callErr, kernel.ErrInvalidState) {
		t.Errorf("zero-price call on closed process: want ErrInvalidState, got %v", callErr)
	}
}

func TestParseActionRef(t *testing.T) {
	cases := []struct {
		input       string
		wantOwner   string
		wantName    string
		wantErr     bool
	}{
		{"@alice/greet", "@alice", "greet", false},
		{"@alice/greet/subname", "@alice", "greet/subname", false}, // names may contain /
		{"@bob/", "", "", true},                                     // empty name
		{"alice/greet", "", "", true},                               // missing @
		{"@alice", "", "", true},                                    // missing /
		{"@/greet", "", "", true},                                   // empty owner
		{"", "", "", true},                                          // empty string
	}
	for _, c := range cases {
		owner, name, err := kernel.ParseActionRef(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseActionRef(%q): expected error, got owner=%q name=%q", c.input, owner, name)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseActionRef(%q): unexpected error: %v", c.input, err)
			continue
		}
		if owner != c.wantOwner || name != c.wantName {
			t.Errorf("ParseActionRef(%q): got (%q, %q), want (%q, %q)", c.input, owner, name, c.wantOwner, c.wantName)
		}
	}
}
