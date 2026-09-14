package kernel_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/native"
	"github.com/google/uuid"
)

// TestCallValidatesRootActionSnapshot proves Call is self-validating for the root path:
// after the root trace is funded, the action is deactivated, and Call must still reject the
// call via checkCallPreconditions (not trust an upstream gate). Guards against a future entry
// point inheriting a validation exemption.
func TestCallValidatesRootActionSnapshot(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "alice-selfvalidate", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "act",
		Kind: kernel.KindWasm, Source: "x", Active: true, Price: 10,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_, tr := beginTestRun(t, st, alice.ID, a)

	// Deactivate the action AFTER the root trace is funded.
	a.Active = false
	if err := st.UpdateAction(ctx, a); err != nil {
		t.Fatalf("UpdateAction: %v", err)
	}

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "act", Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("Call on funded root trace with deactivated action: got %v, want ErrInvalidState", err)
	}
}

func TestWasmTimeoutReturnsErrTimeout(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &sleepingFailExec{err: context.DeadlineExceeded})
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "slow",
		Kind: kernel.KindWasm, Active: true, Price: 10,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
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

	alice := setupUser(t, st, "alice-vat", 500)
	bob := setupUser(t, st, "bob-vat", 0)
	feeUser := setupUser(t, st, "fee-vat", 0)
	carol := setupUser(t, st, "carol-vat", 50)

	inner := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "inner",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Visibility: kernel.VisibilityPublic, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, inner)
	outer := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "outer",
		Kind: kernel.KindWasm, Source: "outer", Active: true, Visibility: kernel.VisibilityPublic, Price: 50,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outer)

	exec := &failingSubCallExec{targetUser: bob.ID, targetAction: "inner"}
	cfg := testConfig()
	cfg.FeeRecipientID = feeUser.ID
	k := newKernel(cfg, kernel.Dependencies{Store: st, Scripts: exec})

	_, tr := beginTestRun(t, st, carol.ID, outer)

	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: carol.ID, ExistingTraceID: tr.ID,
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

	owner := setupUser(t, st, "alice", 1000)
	target := setupUser(t, st, "bob", 0)
	a := setupAction(t, st, target.ID, "echo", 0)

	p, tr := beginTestRun(t, st, owner.ID, a)
	_ = k.EndProcess(ctx, owner.ID, p.ID)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        owner.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    target.ID,
		ActionName:      "echo",
		Args:            map[string]any{},
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

	alice := setupUser(t, st, "alice", 1000)
	bob := setupUser(t, st, "bob", 0)

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

	_, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        alice.ID,
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

	alice := setupUser(t, st, "alice", 1000)
	bob := setupUser(t, st, "bob", 0)
	setupAction(t, st, bob.ID, "private", 0)

	// Alice (not the owner) tries to run bob's private action via Run, which enforces CanCall.
	_, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "bob/private", Args: map[string]any{}})
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

	alice := setupUser(t, st, "alice", 1000)
	bob := setupUser(t, st, "bob", 0)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: bob.ID,
		Name:        "svc",
		Kind:        kernel.KindWasm,
		Active:      true,
		Visibility:  kernel.VisibilityPublic,
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, alice.ID, a)
	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        alice.ID,
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

	alice := setupUser(t, st, "alice", 1000)
	bob := setupUser(t, st, "bob", 1000)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: bob.ID,
		Name:        "priv",
		Kind:        kernel.KindWasm,
		Active:      true,
		Visibility:  kernel.VisibilityPrivate,
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	// Bob (the owner) can call his own private action.
	_, trBob := beginTestRun(t, st, bob.ID, a)
	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        bob.ID,
		ExistingTraceID: trBob.ID,
		TargetUserID:    bob.ID,
		ActionName:      "priv",
		Args:            map[string]any{},
	})
	if err != nil {
		t.Fatalf("owner should call their own private action: %v", err)
	}

	// Alice (not the owner) cannot run bob's private action. Validated by Run → beginRun.
	_, err = k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "bob/priv", Args: map[string]any{}})
	if err == nil {
		t.Error("non-owner should not be able to call private action")
	}
}

func TestCallInactiveActionBlocked(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 1000)
	_ = st.CreateAction(ctx, &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "inactive",
		Kind: kernel.KindWasm, Active: false, Visibility: kernel.VisibilityPublic, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	// Run enforces CanCall (which requires active=true) in beginRun.
	_, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice/inactive", Args: map[string]any{}})
	if err == nil {
		t.Error("inactive action should be blocked regardless of public flag")
	}
}

// TestCallSuspendedOwnerActionBlocked proves a suspended owner's active public action is
// uncallable with ErrInvalidState (§12 hide+disable), and that unsuspend restores it.
func TestCallSuspendedOwnerActionBlocked(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 1000) // process owner / caller
	bob := setupUser(t, st, "bob", 0)        // action owner (provider)
	_ = st.CreateAction(ctx, &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "svc",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	// Callable before suspension.
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "bob/svc", Args: map[string]any{}}); err != nil {
		t.Fatalf("action should be callable before owner suspension: %v", err)
	}

	// Suspending the owner makes the action uncallable with ErrInvalidState.
	if err := st.SuspendUser(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "bob/svc", Args: map[string]any{}}); !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("suspended owner's action should fail with ErrInvalidState, got %v", err)
	}

	// Unsuspending restores callability.
	if err := st.UnsuspendUser(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "bob/svc", Args: map[string]any{}}); err != nil {
		t.Fatalf("unsuspend should restore callability: %v", err)
	}
}

func TestCallInsufficientFunds(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 50)
	_ = setupAction(t, st, alice.ID, "expensive", 200)

	_, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice/expensive", Args: map[string]any{}})
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

	alice := setupUser(t, st, "alice", 2000)
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

	_, tr := beginTestRun(t, st, alice.ID, a)

	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        alice.ID,
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

	alice := setupUser(t, st, "alice", 1000)
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

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID,
		ActionName:   "svc",
		Args:         map[string]any{},
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

	alice := setupUser(t, st, "alice", 1000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "svc",
		Kind: kernel.KindWasm, Active: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, alice.ID, a)

	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        alice.ID,
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

	alice := setupUser(t, st, "alice", 1000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "risky",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        alice.ID,
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

// TestWasmHandleErrorSentinelMapsToExecutionFailed verifies that the SDK's error
// envelope {"__juice_error__":"<msg>"} returned by run() is mapped to
// ErrExecutionFailed carrying the message — so a Handle error surfaces its reason
// instead of an opaque wasm "unreachable" trap.
func TestWasmHandleErrorSentinelMapsToExecutionFailed(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"__juice_error__":"boom from handle"}`})
	ctx := context.Background()

	alice := setupUser(t, st, "sentinel-alice", 1000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "handle-err",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "handle-err", Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Fatalf("expected ErrExecutionFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "boom from handle") {
		t.Errorf("expected Handle error message to survive, got %q", err.Error())
	}

	// Funds refunded after the failure settles and the process closes.
	alice2, _ := st.ReadUser(ctx, alice.ID)
	if alice2.Locked != 0 || alice2.Available != 1000 {
		t.Errorf("after handle-error failure: available=%d locked=%d, want 1000/0", alice2.Available, alice2.Locked)
	}
}

// cancelDuringExec cancels the execution context mid-run (simulating a client
// disconnect or timeout while the action executes), then fails.
type cancelDuringExec struct {
	cancel context.CancelFunc
}

func (c *cancelDuringExec) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	return src, "fakehash", nil
}

func (c *cancelDuringExec) Execute(_ context.Context, _ []byte, _ []byte, _ kernel.HostFunctions) ([]byte, error) {
	c.cancel()
	return nil, kernel.ErrExecutionFailed.Wrap("boom after cancel")
}

// TestFailureSettlesUnderCancelledContext is the regression for "could not record
// failure transaction": settlement must commit even when the execution context was
// cancelled, so the locked allocation is never stranded.
func TestFailureSettlesUnderCancelledContext(t *testing.T) {
	st := newTestStore(t)
	exec := &cancelDuringExec{}
	k := newTestKernelWithScripts(st, exec)
	ctx, cancel := context.WithCancel(context.Background())
	exec.cancel = cancel
	defer cancel()

	alice := setupUser(t, st, "cancel-alice", 1000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "cancels",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "cancels", Args: map[string]any{},
	})
	// The call fails with the execution error — NOT an internal settlement failure.
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Fatalf("expected ErrExecutionFailed, got %v", err)
	}
	if errors.Is(err, kernel.ErrInternal) || strings.Contains(err.Error(), "could not record failure transaction") {
		t.Fatalf("settlement was aborted by cancellation (stranded funds): %v", err)
	}

	// The failure transaction committed and funds were refunded despite cancellation.
	alice2, _ := st.ReadUser(context.Background(), alice.ID)
	if alice2.Locked != 0 || alice2.Available != 1000 {
		t.Errorf("after cancelled-call failure: available=%d locked=%d, want 1000/0", alice2.Available, alice2.Locked)
	}
}

func TestWasmPanicRefundsFunds(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &panicScriptExec{})
	ctx := context.Background()

	alice := setupUser(t, st, "wasm-panic-alice", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "panic-svc",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        alice.ID,
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

	alice := setupUser(t, st, "alice", 0)
	p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: alice.ID,
		Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
	traceA := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: alice.ID, CallerUserID: alice.ID, CreatedAt: time.Now().UTC()}
	if err := st.BeginRun(ctx, p, traceA, alice.ID, 0, 0, 0); err != nil {
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

	alice := setupUser(t, st, "alice", 1000)
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
	_, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice/strict", Args: map[string]any{"wrong_field": "value"}})
	if !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Errorf("missing required field: got %v, want ErrSchemaViolation", err)
	}
}

func TestCallOutputSchemaRejection(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"unexpected_field": 42}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 1000)
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
	_, tr := beginTestRun(t, st, alice.ID, a)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
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

	alice := setupUser(t, st, "alice", 1000)
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

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
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

	alice := setupUser(t, st, "alice", 1000)
	bob := setupUser(t, st, "bob", 0)

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
	_, tr := beginTestRun(t, st, alice.ID, outerAction)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "outer", Args: map[string]any{},
	})
	if err == nil {
		t.Error("expected denial when script subcalls a private action not owned by the process owner")
	}
}

// TestSubcallProviderPrivateHelper: a provider's public composite subcalls the provider's OWN
// private helper while a *customer* funds the process. Caller-scoping (§4) makes the subcall's
// caller the composite's owner, so a provider may keep its internals private and still sell a
// composite over them. Under the old process-owner scoping this was denied.
func TestSubcallProviderPrivateHelper(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 0) // provider
	bob := setupUser(t, st, "bob", 1000)  // customer funds the process

	helper := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "helper",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Visibility: kernel.VisibilityPrivate, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, helper)
	composite := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "composite",
		Kind: kernel.KindWasm, Source: "outer", Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, composite)

	exec := &subcallExec{targetUser: alice.ID, targetAction: "helper"}
	k := newTestKernelWithScripts(st, exec)
	_, tr := beginTestRun(t, st, bob.ID, composite)

	if _, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: bob.ID, ExistingTraceID: tr.ID, Action: composite, Args: map[string]any{},
	}); err != nil {
		t.Errorf("provider's public composite should reach its own private helper: %v", err)
	}
}

// TestSubcallConfusedDeputyDenied: foreign code the process owner funds must NOT reach the process
// owner's private actions. bob funds alice's public composite, which tries to subcall bob's private
// action; caller-scoping denies it (caller = alice ≠ owner of the private action). Under the old
// process-owner scoping this was allowed — the confused-deputy bug this change closes.
func TestSubcallConfusedDeputyDenied(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 0) // composite author
	bob := setupUser(t, st, "bob", 1000)  // process owner with a private action

	secret := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "secret",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Visibility: kernel.VisibilityPrivate, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, secret)
	composite := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "composite",
		Kind: kernel.KindWasm, Source: "outer", Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, composite)

	exec := &subcallExec{targetUser: bob.ID, targetAction: "secret"}
	k := newTestKernelWithScripts(st, exec)
	_, tr := beginTestRun(t, st, bob.ID, composite)

	if _, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: bob.ID, ExistingTraceID: tr.ID, Action: composite, Args: map[string]any{},
	}); err == nil {
		t.Error("foreign composite must not reach the process owner's private action (confused deputy)")
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

	alice := setupUser(t, st, "alice", 1000)
	bob := setupUser(t, st, "bob", 500)
	feeUser := setupUser(t, st, "fee-recipient", 0)

	inner := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "inner",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Visibility: kernel.VisibilityPublic, Price: 100,
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
	cfg := testConfig()
	cfg.FeeRecipientID = feeUser.ID
	k := newKernel(cfg, kernel.Dependencies{Store: st, Scripts: exec})

	p, tr := beginTestRun(t, st, alice.ID, outer)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
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

	alice := setupUser(t, st, "alice", 50) // enough for outer only, not inner
	bob := setupUser(t, st, "bob", 0)

	inner := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "inner",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Visibility: kernel.VisibilityPublic, Price: 100,
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
	_, tr := beginTestRun(t, st, alice.ID, outer)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
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

	alice := setupUser(t, st, "alice", 0)
	bob := setupUser(t, st, "bob", 0)

	inner := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "inner",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
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
	outerReply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
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

	alice := setupUser(t, st, "alice", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "noop",
		Kind: kernel.KindWasm, Active: true, Price: 0, Source: "fake",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_, tr := beginTestRun(t, st, alice.ID, a)

	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
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
		net, fee := (kernel.Economy{FeeBPS: tc.feeBPS}).Fee(tc.taxable)
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
		net, fee := (kernel.Economy{FeeBPS: 2000}).Fee(taxable)
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

	caller := setupUser(t, base, "caller", 1000)
	actionOwner := setupUser(t, base, "owner", 0)
	a := setupAction(t, base, actionOwner.ID, "echo", 100)
	a.Visibility = kernel.VisibilityPublic
	_ = base.UpdateAction(ctx, a)

	p, tr := beginTestRun(t, base, caller.ID, a)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
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

	alice := setupUser(t, st, "alice", 1000)
	a := setupAction(t, st, alice.ID, "svc", 100)
	_ = a

	p := setupProcess(t, st, alice.ID, 500)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ParentTraceID: "nonexistent-trace-id",
		TargetUserID: alice.ID,
		ActionName:   "svc",
		Args:         map[string]any{},
	})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for bad parent trace, got %v", err)
	}

	// Root trace funds must be untouched — the failed call must not move any funds.
	root, _ := st.ReadRootTrace(ctx, p.ID)
	if root.Available != 500 {
		t.Errorf("root_trace.available: got %d, want 500 (funds moved before trace validated)", root.Available)
	}
	if root.Locked != 0 {
		t.Errorf("root_trace.locked: got %d, want 0 (subcall created before trace validated)", root.Locked)
	}
}

func TestCallCrossProcessParentTraceRejectedForOwner(t *testing.T) {
	// Even a process owner must not supply a parent trace from a different process
	// on a non-step-completion call: doing so would mutate ancestor cost/latency
	// in the foreign process (B1 fix).
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "svc",
		Kind: kernel.KindWasm, Active: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	// Run a call to get a trace from a completed (auto-closed) process.
	otherReply, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice/svc", Args: map[string]any{}})
	if err != nil {
		t.Fatalf("setup call in otherP: %v", err)
	}

	_, err = k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:      alice.ID,
		ParentTraceID: otherReply.TraceID, // trace from closed process — must be rejected
		TargetUserID:  alice.ID,
		ActionName:    a.Name,
		Args:          map[string]any{},
	})
	if err == nil {
		t.Fatal("using a parent trace from a closed process should be rejected")
	}
}

func TestCallCrossProcessParentTraceRejectedForNonOwner(t *testing.T) {
	// A non-owner caller whose action appears in a different process's trace must not
	// gain authority over the target process via that foreign trace (F2 fix).
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	procOwner := setupUser(t, st, "f2-proc-owner", 500)
	actionOwner := setupUser(t, st, "f2-action-owner", 0)
	targetAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: procOwner.ID, Name: "f2-target",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	callerAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: actionOwner.ID, Name: "f2-caller",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, targetAction)
	_ = st.CreateAction(ctx, callerAction)

	// p2 uses callerAction so its root trace has action_owner_id=actionOwner.ID (callerAction.OwnerUserID),
	// granting actionOwner trace-scoped authority over p2 in the positive test below.
	_, p1tr := beginTestRun(t, st, procOwner.ID, callerAction)
	_, p2Orphan := beginTestRun(t, st, procOwner.ID, callerAction)

	// Create a trace in p1 owned by actionOwner (by calling callerAction in p1).
	reply1, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        procOwner.ID,
		ExistingTraceID: p1tr.ID,
		TargetUserID:    actionOwner.ID,
		ActionName:      "f2-caller",
		Args:            map[string]any{},
	})
	if err != nil {
		t.Fatalf("setup call in p1: %v", err)
	}
	p1TraceID := reply1.TraceID

	// actionOwner tries to use a trace from the now-closed p1 process — must fail.
	_, err = k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:      actionOwner.ID,
		ParentTraceID: p1TraceID, // trace from closed process
		TargetUserID:  procOwner.ID,
		ActionName:    "f2-target",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected error for trace from closed process, got nil")
	}

	// Using the p2 root trace (action_owner_id=actionOwner.ID via callerAction) should succeed.
	_, err = k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:      actionOwner.ID,
		ParentTraceID: p2Orphan.ID, // open process trace → allowed
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

func (f *failingCommitFailedCallStore) CommitFailedCall(_ context.Context, _ *kernel.Transaction, _ func(int64) (*kernel.Receipt, error), _, _, _, _ string, _ int64, _ *kernel.Stats, _, _, _ string) error {
	return kernel.ErrInternal.Wrap("injected CommitFailedCall failure")
}

func TestCommitFailedCallSettlementError(t *testing.T) {
	base := newTestStore(t)
	failing := &failingCommitFailedCallStore{Store: base}
	k := newTestKernelWithScripts(failing, &fakeScriptExec{err: kernel.ErrExecutionFailed.Wrap("boom")})
	ctx := context.Background()

	alice := setupUser(t, base, "alice", 1000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "risky",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = base.CreateAction(ctx, a)

	_, tr := beginTestRun(t, base, alice.ID, a)
	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
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

	alice := setupUser(t, st, "alice-rcpt", 500)
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
	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
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
	k := newKernel(testConfig(), kernel.Dependencies{Store: st})
	native.Register(k, []native.Spec{native.Chat(c)})
	return k
}

func TestCallLLMChat(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	fc := &fakeChatter{reply: kernel.ChatMessage{Role: "assistant", Content: "hello there"}}
	k := newTestKernelWithChatter(st, fc)

	owner := setupUser(t, st, "sys", 1000)
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

	_, tr := beginTestRun(t, st, owner.ID, chatAction)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        owner.ID,
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
	native.Register(k, []native.Spec{native.Chat(nil)})

	owner := setupUser(t, st, "sys", 1000)
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

	_, tr := beginTestRun(t, st, owner.ID, chatAction)
	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        owner.ID,
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

	owner := setupUser(t, st, "alice", 1000)
	target := setupUser(t, st, "bob", 0)
	a := setupAction(t, st, target.ID, "echo", 0)

	_, tr := beginTestRun(t, st, owner.ID, a)

	if err := st.SuspendUser(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        owner.ID,
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
		cfg := testConfig()
		cfg.FeeRecipientID = "" // 20% fee — no recipient set
		k := newKernel(cfg, kernel.Dependencies{Store: st})
		if err := k.ValidateFeeRecipient(ctx); !errors.Is(err, kernel.ErrInvalidState) {
			t.Errorf("expected ErrInvalidState for fee_bps>0 with empty recipient, got %v", err)
		}
	})

	t.Run("nonexistent recipient rejected at startup", func(t *testing.T) {
		st := newTestStore(t)
		cfg := testConfig()
		cfg.FeeRecipientID = "no-such-user"
		k := newKernel(cfg, kernel.Dependencies{Store: st})
		if err := k.ValidateFeeRecipient(ctx); !errors.Is(err, kernel.ErrInvalidState) {
			t.Errorf("expected ErrInvalidState for unknown fee recipient, got %v", err)
		}
	})

	t.Run("zero fee_bps passes with no recipient", func(t *testing.T) {
		st := newTestStore(t)
		cfg := testConfig()
		econ := testEconomy()
		econ.FeeBPS = 0
		k := newKernel(cfg, kernel.Dependencies{Store: st, Economy: econ})
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

	owner := setupUser(t, st, "owner", 0)
	a := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "free",
		Kind:         kernel.KindHTTP,
		Active:       true,
		Price:        0,
		Visibility:   kernel.VisibilityPublic,
		Source:       "http://example.com",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	p, tr := beginTestRun(t, st, owner.ID, a)
	if err := k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}

	_, callErr := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        owner.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    owner.ID,
		ActionName:      "free",
		Args:            map[string]any{},
	})
	if !errors.Is(callErr, kernel.ErrInvalidState) {
		t.Errorf("zero-price call on closed process: want ErrInvalidState, got %v", callErr)
	}
}

// TestCallRemoteProxyMissingExecutorSettlesFailure verifies that a remote-proxy call whose kernel
// has no FederationExecutor settles as a failure (committing one transaction + receipt and refunding
// the locked funds) instead of returning early and stranding the allocation. The "no settlement" rule
// applies only to a network timeout awaiting a remote receipt, not to a missing local adapter.
func TestCallRemoteProxyMissingExecutorSettlesFailure(t *testing.T) {
	st := newTestStore(t)
	// fakeSuccessHTTP implements HTTPExecutor but NOT FederationExecutor → triggers the !ok branch.
	k := newTestKernelWithHTTP(st, &fakeSuccessHTTP{})
	ctx := context.Background()

	owner := setupUser(t, st, "rpme-owner", 0)
	caller := setupUser(t, st, "rpme-caller", 100)
	remoteAct := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID,
		Name: "rpme-action", Kind: kernel.KindRemoteProxy,
		Active: true, Visibility: kernel.VisibilityPublic, Price: 50,
		Source:    "https://remote.example.com/v1/federation/call?action=@rpme-owner/rpme-action&counterparty=us",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, remoteAct); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	// A proxy is addressable by its action id (never a bare owner/name, §8); the missing
	// FederationExecutor then settles the call as a failure.
	_, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: remoteAct.ID, Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState for missing federation executor, got %v", err)
	}

	// Exactly one failure transaction was committed, with a receipt.
	txs, err := st.ListTransactions(ctx, kernel.TxFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("expected exactly one transaction, got %d", len(txs))
	}
	if txs[0].Status != kernel.TxFailure {
		t.Errorf("transaction status: got %q, want failure", txs[0].Status)
	}
	if r, err := st.ReadReceiptByTxID(ctx, txs[0].ID); err != nil || r == nil {
		t.Errorf("expected a receipt for the failure transaction, got %v (err %v)", r, err)
	}

	// Funds are fully refunded — not stranded in the process/trace lock.
	got, err := st.ReadUser(ctx, caller.ID)
	if err != nil {
		t.Fatalf("ReadUser: %v", err)
	}
	if got.Available != 100 || got.Locked != 0 {
		t.Errorf("caller funds not refunded: available=%d locked=%d, want 100/0", got.Available, got.Locked)
	}
}

func TestParseActionRef(t *testing.T) {
	cases := []struct {
		input      string
		wantOwner  string
		wantKernel string
		wantName   string
		wantErr    bool
	}{
		{"alice/greet", "alice", "", "greet", false},                 // bare local
		{"alice/greet/subname", "alice", "", "greet/subname", false}, // names may contain /
		{"sys/llm/chat", "sys", "", "llm/chat", false},               // multi-segment native
		{"bob@acme/foo", "bob", "acme", "foo", false},                // kernel-qualified
		{"@alice/greet", "", "", "", true},                           // sigil-prefixed owner rejected (§14)
		{"acme/alice/foo", "acme", "", "alice/foo", false},           // owner with a slashed action name
		{"bob@/foo", "", "", "", true},                               // empty kernel
		{"bob/", "", "", "", true},                                   // empty name
		{"alice", "", "", "", true},                                  // missing /
		{"/greet", "", "", "", true},                                 // empty owner
		{"", "", "", "", true},                                       // empty string
	}
	for _, c := range cases {
		r, err := kernel.ParseActionRef(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseActionRef(%q): expected error, got %+v", c.input, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseActionRef(%q): unexpected error: %v", c.input, err)
			continue
		}
		if r.Owner != c.wantOwner || r.Kernel != c.wantKernel || r.Name != c.wantName {
			t.Errorf("ParseActionRef(%q): got %+v, want owner=%q kernel=%q name=%q", c.input, r, c.wantOwner, c.wantKernel, c.wantName)
		}
	}
}

// subcallThenFailExec settles a subcall (inner) and then fails, leaving a settled descendant.
type subcallThenFailExec struct {
	targetUser   string
	targetAction string
}

func (c *subcallThenFailExec) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	return src, "fakehash", nil
}

func (c *subcallThenFailExec) Execute(ctx context.Context, src []byte, _ []byte, host kernel.HostFunctions) ([]byte, error) {
	if string(src) == "outer" {
		if _, err := host.Call(ctx, c.targetUser+"/"+c.targetAction, []byte(`{}`)); err != nil {
			return nil, err
		}
		return nil, errors.New("outer fails after settling inner")
	}
	return []byte(`{"ok":true}`), nil
}

// #1: a federated (inbound) call that fails AFTER settling a descendant must surface the
// committed failure receipt — whose charge = gross − refund > 0 — not a zero-charge rejection.
func TestRunFederatedFailureReturnsCommittedReceiptWithCharge(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	caller := setupUser(t, st, "fed-caller", 1000) // proxy/counterparty user
	provider := setupUser(t, st, "provider", 0)
	owner := setupUser(t, st, "owner", 0)

	inner := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "inner",
		Kind: kernel.KindWasm, Source: "inner", Active: true, Visibility: kernel.VisibilityPublic, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, inner); err != nil {
		t.Fatal(err)
	}
	outer := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "outer",
		Kind: kernel.KindWasm, Source: "outer", Active: true, Visibility: kernel.VisibilityPublic, Price: 150,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, outer); err != nil {
		t.Fatal(err)
	}

	exec := &subcallThenFailExec{targetUser: provider.ID, targetAction: "inner"}
	k := newTestKernelWithScripts(st, exec)

	reply, err := k.RunFederated(ctx, caller.ID, owner.ID, "outer", map[string]any{}, "", kernel.BuyerTerms{})
	if err == nil {
		t.Fatal("expected outer call to fail")
	}
	if reply == nil || reply.ReceiptID == "" {
		t.Fatalf("expected committed receipt on failure, got reply=%v", reply)
	}
	rcpt, gerr := k.GetReceiptByID(ctx, reply.ReceiptID)
	if gerr != nil {
		t.Fatalf("GetReceiptByID: %v", gerr)
	}
	if rcpt.Status != kernel.TxFailure {
		t.Errorf("expected failure receipt, got status %s", rcpt.Status)
	}
	// inner settled for 100, so the failed outer charge = gross(150) − refund(50) = 100, not 0.
	if rcpt.Charge != 100 {
		t.Errorf("expected committed charge 100 (settled descendant), got %d", rcpt.Charge)
	}
}

// ---- Canonical resolvers (ResolveAction / ResolveUser) ----

func TestResolveAction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	bob := setupUser(t, st, "bob", 0)
	a := setupAction(t, st, bob.ID, "greet", 0)

	for _, ref := range []string{"bob/greet", "bob/greet", a.ID} {
		got, err := k.ResolveAction(ctx, ref)
		if err != nil {
			t.Fatalf("ResolveAction(%q): %v", ref, err)
		}
		if got.ID != a.ID {
			t.Errorf("ResolveAction(%q): got %s, want %s", ref, got.ID, a.ID)
		}
	}

	if _, err := k.ResolveAction(ctx, "bob/missing"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("ResolveAction(missing): want ErrNotFound, got %v", err)
	}
	if _, err := k.ResolveAction(ctx, uuid.New().String()); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("ResolveAction(unknown id): want ErrNotFound, got %v", err)
	}
}

func TestResolveUser(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 0)
	// A key account, to exercise public-key resolution.
	pub := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if err := st.UpsertKernel(ctx, pub, "peer", "", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	peer := &kernel.Account{
		ID:              uuid.New().String(),
		KernelPublicKey: pub,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	if err := st.CreateUser(ctx, peer); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		ident string
		want  string
	}{
		{"alice", alice.ID},
		{"alice", alice.ID},
		{alice.ID, alice.ID},
		{pub, peer.ID},
	}
	for _, c := range cases {
		got, err := k.ResolveUser(ctx, c.ident)
		if err != nil {
			t.Fatalf("ResolveUser(%q): %v", c.ident, err)
		}
		if got.ID != c.want {
			t.Errorf("ResolveUser(%q): got %s, want %s", c.ident, got.ID, c.want)
		}
	}

	if _, err := k.ResolveUser(ctx, "nobody"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("ResolveUser(missing): want ErrNotFound, got %v", err)
	}
	// A kernel's petname lives in the OTHER namespace (§13): it never resolves as a user, which is
	// what lets a local user and a kernel share the same bare name.
	if _, err := k.ResolveUser(ctx, "peer"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("ResolveUser(petname): want ErrNotFound, got %v", err)
	}
}

// stepCreateHostExec drives the WASM host StepCreate with name references, proving the host
// resolves @owner/name and @handle just like juice.call (previously it required raw UUIDs).
type stepCreateHostExec struct {
	requiredCaller string
	action         string
	stepID         string
	err            error
}

func (e *stepCreateHostExec) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	return src, "fakehash", nil
}

func (e *stepCreateHostExec) Execute(ctx context.Context, _ []byte, _ []byte, host kernel.HostFunctions) ([]byte, error) {
	e.stepID, e.err = host.StepCreate(ctx, []byte(`{"message":"hi"}`), e.requiredCaller, e.action)
	if e.err != nil {
		return nil, e.err
	}
	return []byte(`{"ok":true}`), nil
}

func TestHostStepCreateResolvesNames(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 1000) // process owner, wasm action owner
	bob := setupUser(t, st, "bob", 0)        // onward action owner + required caller

	approve := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "approve",
		Kind: kernel.KindNative, Source: "native", Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, approve); err != nil {
		t.Fatal(err)
	}
	orch := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "orchestrate",
		Kind: kernel.KindWasm, Source: "x", Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, orch); err != nil {
		t.Fatal(err)
	}

	exec := &stepCreateHostExec{requiredCaller: "bob", action: "bob/approve"}
	k := newTestKernelWithScripts(st, exec)

	_, tr := beginTestRun(t, st, alice.ID, orch)
	if _, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "orchestrate", Args: map[string]any{},
	}); err != nil {
		t.Fatalf("run orchestrate: %v", err)
	}
	if exec.err != nil {
		t.Fatalf("host StepCreate: %v", exec.err)
	}

	step, err := st.ReadStep(ctx, exec.stepID)
	if err != nil {
		t.Fatalf("ReadStep: %v", err)
	}
	if step.ActionID != approve.ID {
		t.Errorf("action ref not resolved: got %s, want %s", step.ActionID, approve.ID)
	}
	if step.RequiredCallerUserID != bob.ID {
		t.Errorf("required caller handle not resolved: got %s, want %s", step.RequiredCallerUserID, bob.ID)
	}
	if step.Status != kernel.StepWaiting {
		t.Errorf("step status: got %s, want waiting", step.Status)
	}
}

// stepCompleteHostExec drives the WASM host's juice.step_complete against a step the executing
// trace did not park, recording what the host returned.
type stepCompleteHostExec struct {
	stepID string
	err    error
}

func (e *stepCompleteHostExec) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	return src, "fakehash", nil
}

func (e *stepCompleteHostExec) Execute(ctx context.Context, _ []byte, _ []byte, host kernel.HostFunctions) ([]byte, error) {
	if _, e.err = host.StepComplete(ctx, e.stepID, []byte(`{}`)); e.err != nil {
		return nil, e.err
	}
	return []byte(`{"ok":true}`), nil
}

// TestHostStepCompleteIsTraceConfined: a script resumes only a step its own trace parked (§10). The
// script runs as its action's owner, so being the required caller would otherwise let any execution
// of that action fire a step living in an unrelated process — the WASM half of the capability rule.
func TestHostStepCompleteIsTraceConfined(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	victim := setupUser(t, st, "hsc-victim", 1000)
	mallory := setupUser(t, st, "hsc-mallory", 1000)

	// The step's target and the script are both mallory's, so the script's owner IS the required
	// caller: only the trace check can refuse this.
	target := setupWasmAction(t, st, mallory.ID, "hsc-target", "", 0)
	script := setupWasmAction(t, st, mallory.ID, "hsc-script", "", 0)

	_, victimTrace := setupOrphanTrace(t, st, victim.ID, victim.ID, victim.ID)
	exec := &stepCompleteHostExec{}
	k := newTestKernelWithScripts(st, exec)
	step, err := k.CreateStep(ctx, victimTrace.ID, target.ID, nil, kernel.RequiredCaller{UserID: mallory.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	exec.stepID = step.ID

	_, tr := beginTestRun(t, st, mallory.ID, script)
	_, callErr := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: mallory.ID, ExistingTraceID: tr.ID,
		TargetUserID: mallory.ID, ActionName: "hsc-script", Args: map[string]any{},
	})
	if callErr == nil {
		t.Fatal("the call must fail: the host completion is refused")
	}
	if !errors.Is(exec.err, kernel.ErrUnauthorized) {
		t.Fatalf("host StepComplete across traces must be ErrUnauthorized, got %v", exec.err)
	}
	if s, _ := st.ReadStep(ctx, step.ID); s.Status != kernel.StepWaiting {
		t.Errorf("the step must stay waiting, got %s", s.Status)
	}
}

// ---- Symmetric wasm-artifact update (item 2) ----

func TestUpdateActionAcceptsWasmArtifact(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{})
	ctx := context.Background()

	owner := setupUser(t, st, "owner", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "mod",
		Kind: kernel.KindWasm, Source: "old text source", Active: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	artifact := base64.StdEncoding.EncodeToString([]byte{0x00, 0x61, 0x73, 0x6d})
	updated, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, WasmArtifact: artifact})
	if err != nil {
		t.Fatalf("UpdateAction with artifact: %v", err)
	}
	if updated.WasmArtifact != artifact {
		t.Errorf("WasmArtifact not stored: got %q, want %q", updated.WasmArtifact, artifact)
	}
	if updated.ArtifactHash == "" {
		t.Error("ArtifactHash not recomputed from artifact")
	}
	if updated.Active {
		t.Error("artifact update must deactivate (re-activation required, §7)")
	}

	// Updating with text source clears the stale precompiled artifact so the source is authoritative.
	src := "new text source"
	cleared, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Source: &src})
	if err != nil {
		t.Fatalf("UpdateAction with source: %v", err)
	}
	if cleared.WasmArtifact != "" {
		t.Errorf("text-source update should clear WasmArtifact, got %q", cleared.WasmArtifact)
	}

	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, WasmArtifact: "!!! not base64 !!!"}); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("invalid artifact: want ErrInvalidInput, got %v", err)
	}
}

// TestQuoteHashBindsTerms: the quote pin (§4 precondition 7) is a fingerprint of what a buyer was
// shown. Every field that decides what they can be charged moves it — price, effect, description,
// both schemas, and the stable id — while a field that does not is ignored. Effect is the one that
// matters most: it alone decides whether a call locks a value reserve from the caller's own balance
// (§13), so a peer promoting it under otherwise identical terms must not pass a stale pin.
func TestQuoteHashBindsTerms(t *testing.T) {
	base := func() *kernel.Action {
		return &kernel.Action{
			ID: "local-id", Name: "svc", Price: 100, Description: "d",
			InputSchema:  map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
		}
	}
	h := kernel.QuoteHash(base())
	if h == "" || h != kernel.QuoteHash(base()) {
		t.Fatalf("quote hash must be non-empty and stable, got %q", h)
	}
	for name, mutate := range map[string]func(*kernel.Action){
		"price":       func(a *kernel.Action) { a.Price = 101 },
		"effect":      func(a *kernel.Action) { a.Effect = "transfer" },
		"description": func(a *kernel.Action) { a.Description = "d2" },
		"input":       func(a *kernel.Action) { a.InputSchema = map[string]any{"type": "string"} },
		"output":      func(a *kernel.Action) { a.OutputSchema = map[string]any{"type": "string"} },
		"identity":    func(a *kernel.Action) { a.ID = "other-id" },
	} {
		t.Run(name, func(t *testing.T) {
			a := base()
			mutate(a)
			if kernel.QuoteHash(a) == h {
				t.Errorf("changing %s must move the quote hash", name)
			}
		})
	}
	// Not quoted, so it must not move the hash: the pin binds terms, never implementation.
	impl := base()
	impl.Source, impl.ArtifactHash, impl.Kind = "http://elsewhere", "deadbeef", kernel.KindHTTP
	if kernel.QuoteHash(impl) != h {
		t.Error("the pin binds the quote, not the implementation; source/artifact must not move it")
	}
	// A proxy keys on its REMOTE id, so a catalog hit and the row it resolves to agree even though
	// the local cache UUID differs (§13). This is what lets a hash read from lookup bind a first call.
	p1 := base()
	p1.ID, p1.RemoteActionID, p1.Kind = "local-uuid-1", "remote-id", kernel.KindRemoteProxy
	p2 := base()
	p2.ID, p2.RemoteActionID, p2.Kind = "local-uuid-2", "remote-id", kernel.KindRemoteProxy
	if kernel.QuoteHash(p1) != kernel.QuoteHash(p2) {
		t.Error("a proxy's quote must key on its remote id, not its local cache UUID")
	}
}

// TestRunQuotePinRefusesBeforeFunding: a stale pin is refused with ErrInvalidState carrying the
// current hash and price, and — the point of the whole mechanism — nothing is charged and no
// process exists, because the refusal precedes BeginRun. A matching pin runs normally, and an
// omitted pin leaves behaviour unchanged.
func TestRunQuotePinRefusesBeforeFunding(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice-quote", 5000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "svc",
		Kind: kernel.KindWasm, Source: "x", Active: true, Price: 100,
		Visibility: kernel.VisibilityPublic, Description: "d",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	stale := kernel.QuoteHash(a)

	// The owner raises the price; the buyer still holds the old quote.
	a.Price = 500
	if err := st.UpdateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	_, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice-quote/svc", Args: map[string]any{}, QuoteHash: stale})
	if !errors.Is(err, kernel.ErrTermsChanged) {
		t.Fatalf("stale pin: got %v, want ErrTermsChanged", err)
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Meta["quote_hash"] != kernel.QuoteHash(a) || ke.Meta["price"] != "500" {
		t.Errorf("refusal must carry the current hash and price, got meta %v", ke.Meta)
	}
	// A distinct code, not invalid_state: a client must tell "re-confirm the new terms" from
	// "this action is disabled" without sniffing Meta (§12).
	if errors.Is(err, kernel.ErrInvalidState) || ke.Code != "terms_changed" {
		t.Errorf("changed terms need their own code, got %q", ke.Code)
	}
	u, _ := st.ReadUser(ctx, alice.ID)
	if u.Available != 5000 || u.Locked != 0 {
		t.Errorf("a refused pin must charge and lock nothing; got available=%d locked=%d", u.Available, u.Locked)
	}
	if ps, _ := st.ListProcesses(ctx, alice.ID, 10, 0); len(ps) != 0 {
		t.Errorf("a refused pin must create no process, got %d", len(ps))
	}

	// The buyer re-reads and accepts the new terms; and an unpinned run is unaffected.
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice-quote/svc", Args: map[string]any{}, QuoteHash: kernel.QuoteHash(a)}); err != nil {
		t.Errorf("a matching pin must run normally: %v", err)
	}
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice-quote/svc", Args: map[string]any{}}); err != nil {
		t.Errorf("an omitted pin must leave behaviour unchanged: %v", err)
	}
}

// TestQuoteExcludesImplementation: the quote is what the BUYER agreed to — price, effect,
// description, schemas. It deliberately says nothing about how the action is implemented, so a
// provider re-implementing at unchanged terms does not invalidate outstanding consent. This is why
// a federation contract mismatch (whose hash also covers the artifact and kind, §6 P6) is never
// reported as changed terms: the two hashes answer different questions.
func TestQuoteExcludesImplementation(t *testing.T) {
	base := &kernel.Action{
		ID: "a1", Name: "svc", Kind: kernel.KindWasm, Price: 10, Description: "d",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	}
	reimplemented := *base
	reimplemented.Source = "totally different source"
	reimplemented.ArtifactHash = "sha256-newbuild"
	if kernel.QuoteHash(&reimplemented) != kernel.QuoteHash(base) {
		t.Error("a re-implementation at unchanged terms must not move the buyer's quote")
	}
	repriced := *base
	repriced.Price = 11
	if kernel.QuoteHash(&repriced) == kernel.QuoteHash(base) {
		t.Error("a price change must move the quote")
	}
	reshaped := *base
	reshaped.InputSchema = map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}
	if kernel.QuoteHash(&reshaped) == kernel.QuoteHash(base) {
		t.Error("a schema change must move the quote")
	}
}

// TestQuotePinOrderedAfterVisibility: the pin is compared after the visibility check, so a stranger
// probing a private action gets ErrUnauthorized and never learns its terms; and before input
// validation, so terms that changed enough to invalidate the args still report as changed terms
// rather than a schema violation, which is the case the mechanism exists for.
func TestQuotePinOrderedAfterVisibility(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice-order", 500)
	bob := setupUser(t, st, "bob-order", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "secret",
		Kind: kernel.KindWasm, Source: "x", Active: true, Price: 10,
		Visibility: kernel.VisibilityPrivate, Description: "d",
		InputSchema: map[string]any{"type": "object"},
		CreatedAt:   time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, err := k.Run(ctx, kernel.RunRequest{CallerID: bob.ID, ActionRef: "alice-order/secret", Args: map[string]any{}, QuoteHash: "any-guess"})
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("a private action must refuse on visibility, never disclose terms: got %v", err)
	}
	var ke *kernel.KernelError
	if errors.As(err, &ke) && ke.Meta["quote_hash"] != "" {
		t.Error("a visibility refusal must not carry the action's quote hash")
	}

	// Owner-visible now: the schema tightens so the previously valid args no longer validate.
	stale := kernel.QuoteHash(a)
	a.InputSchema = map[string]any{"type": "object", "properties": map[string]any{
		"need": map[string]any{"type": "string"}}, "required": []any{"need"}}
	if err := st.UpdateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	_, err = k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice-order/secret", Args: map[string]any{}, QuoteHash: stale})
	if !errors.Is(err, kernel.ErrTermsChanged) {
		t.Fatalf("a schema change under a stale pin must report changed terms, got %v", err)
	}
}

// quoteFixtures pins the quote-hash wire format. QuoteHash is a buyer-facing pin (§4
// precondition 7) that clients store and re-present, so its digest must survive refactoring
// byte-for-byte. The expected values were captured from the implementation that introduced
// the pin; never regenerate them to make a test pass — a diff here means the pin broke.
func quoteFixtures() []struct {
	name string
	a    *kernel.Action
	want string
} {
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"outer": map[string]any{"type": "object", "properties": map[string]any{"inner": map[string]any{"type": "number"}}},
		},
	}
	return []struct {
		name string
		a    *kernel.Action
		want string
	}{
		{"local", &kernel.Action{ID: "11111111-1111-1111-1111-111111111111", Description: "a plain local action", Price: 100,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}}, "d1cce6267a6aabb0ac45bac2cff24ac30f444f1ab35a9db317bd9693ef518383"},
		{"proxy-stable-id", &kernel.Action{ID: "22222222-2222-2222-2222-222222222222", RemoteActionID: "33333333-3333-3333-3333-333333333333",
			Description: "a cached remote action", Price: 552,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}}, "a8d61c31665ca328a7e9a8a5de51fedbfb2358a3356d6f66996849409ec2ae8f"},
		{"transfer-effect", &kernel.Action{ID: "44444444-4444-4444-4444-444444444444", Effect: "transfer", Description: "value bearing", Price: 0,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}}, "8bdea08ddc9f546e5647004ee53fbd0adb99004f139235c8a89585e81d2365db"},
		{"nested-schemas", &kernel.Action{ID: "55555555-5555-5555-5555-555555555555", Description: "nested", Price: 7,
			InputSchema: nested, OutputSchema: nested}, "105035b3625ac2a47b526e1782255617e699f08af20b777c2381f86cd3b5ec49"},
		{"non-ascii", &kernel.Action{ID: "66666666-6666-6666-6666-666666666666", Description: "análise de preços — ação", Price: 42,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}}, "05c56b53026e786abd9603b445bf485ecae4a782582fdb21b2cac1793ad6a3e8"},
	}
}

func TestQuoteHashFixture(t *testing.T) {
	for _, tc := range quoteFixtures() {
		if got := kernel.QuoteHash(tc.a); got != tc.want {
			t.Errorf("%s: quote hash changed\n got  %s\n want %s\nClients store this pin and re-present it; a changed digest breaks every outstanding quote.", tc.name, got, tc.want)
		}
	}
}

// TestResolveActionIndexFallback: a reference resolves the exact action first and its index child
// second, at every depth (bob → bob/index, acme/mail → acme/mail/index); an exact acme/mail always
// wins over acme/mail/index, and a raw id is never treated as a path.
func TestResolveActionIndexFallback(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	bob := setupUser(t, st, "bob", 0)
	root := setupAction(t, st, bob.ID, "index", 0)
	mailIndex := setupAction(t, st, bob.ID, "mail/index", 0)
	deep := setupAction(t, st, bob.ID, "mail/eu/index", 0)

	// Depth zero, one, and two: the group is named, the index answers.
	for ref, want := range map[string]string{
		"bob":            root.ID,
		"bob/index":      root.ID,
		"bob/mail":       mailIndex.ID,
		"bob/mail/eu":    deep.ID,
		"bob/mail/index": mailIndex.ID,
	} {
		got, err := k.ResolveAction(ctx, ref)
		if err != nil {
			t.Fatalf("ResolveAction(%q): %v", ref, err)
		}
		if got.ID != want {
			t.Errorf("ResolveAction(%q): got %s, want %s", ref, got.ID, want)
		}
	}

	// An exact action always wins over the index child of the same path.
	exact := setupAction(t, st, bob.ID, "mail", 0)
	got, err := k.ResolveAction(ctx, "bob/mail")
	if err != nil {
		t.Fatalf("ResolveAction(bob/mail): %v", err)
	}
	if got.ID != exact.ID {
		t.Errorf("exact action must win: got %s, want %s", got.ID, exact.ID)
	}

	// A miss names what the caller wrote, never the candidate the resolver tried.
	_, err = k.ResolveAction(ctx, "bob/absent")
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if strings.Contains(err.Error(), "index") {
		t.Errorf("error must name the original reference, got %q", err)
	}

	// An id is an object, not a path: an unknown one never resolves to a user's root, even when
	// the id belongs to a user who has one.
	if _, err := k.ResolveAction(ctx, bob.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("a user id must not resolve to that user's root: got %v", err)
	}

	// Grammar still governs: a malformed reference is invalid input, not a miss.
	if _, err := k.ResolveAction(ctx, "@bob/greet"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("sigil-prefixed ref: want ErrInvalidInput, got %v", err)
	}
}

// TestReadCallableActionIndexFallback: sys/llm/decide's bare local reference reaches an application
// root through the same resolver, and an uncallable exact action is refused rather than passed over
// for its sibling.
func TestReadCallableActionIndexFallback(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	acme := setupUser(t, st, "acme", 0)
	caller := setupUser(t, st, "caller", 0)
	idx := setupAction(t, st, acme.ID, "mail/index", 0)
	idx.Visibility = kernel.VisibilityPublic
	if err := st.UpdateAction(ctx, idx); err != nil {
		t.Fatal(err)
	}

	got, err := k.ReadCallableAction(ctx, "acme/mail", caller.ID)
	if err != nil {
		t.Fatalf("ReadCallableAction(acme, mail): %v", err)
	}
	if got.ID != idx.ID {
		t.Errorf("got %s, want the index %s", got.ID, idx.ID)
	}

	// A private exact action is refused on authority; the resolver does not fall through to the
	// index child, which would answer a different action than the one named.
	priv := setupAction(t, st, acme.ID, "mail", 0)
	priv.Visibility = kernel.VisibilityPrivate
	if err := st.UpdateAction(ctx, priv); err != nil {
		t.Fatal(err)
	}
	if _, err := k.ReadCallableAction(ctx, "acme/mail", caller.ID); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("want ErrUnauthorized for the exact private action, got %v", err)
	}
}
