package kernel

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- Call invariant tests ----

func TestCallClosedProcessFails(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 1000)
	target := setupUser(t, st, "@bob", 0)
	_ = setupAction(t, st, target.ID, "/echo", 0)

	p, root, _ := k.StartProcess(ctx, owner.ID, 100)
	_ = k.EndProcess(ctx, owner.ID, p.ID)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     owner.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  target.ID,
		ActionName:    "/echo",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected error calling on closed process")
	}
	var ke *KernelError
	if !errors.As(err, &ke) || ke.Code != "invalid_state" {
		t.Errorf("expected invalid_state error, got %v", err)
	}
}

func TestCallInactiveActionDenied(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 1000)

	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: owner.ID,
		Name:        "/svc",
		Kind:        KindNative,
		Active:      false,
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, owner.ID, 100)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     owner.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  owner.ID,
		ActionName:    "/svc",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected error calling inactive action")
	}
	var ke *KernelError
	if !errors.As(err, &ke) || ke.Code != "invalid_state" {
		t.Errorf("expected invalid_state error, got %v", err)
	}
}

func TestCallACLDenied(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	_ = setupAction(t, st, bob.ID, "/private", 0)

	p, root, _ := k.StartProcess(ctx, alice.ID, 100)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  bob.ID,
		ActionName:    "/private",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected ACL denial error")
	}
	var ke *KernelError
	if !errors.As(err, &ke) || ke.Code != "unauthorized" {
		t.Errorf("expected unauthorized error, got %v", err)
	}
}

func TestCallACLGrantAndRevoke(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: bob.ID,
		Name:        "/svc",
		Kind:        KindWasm,
		Active:      true,
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_ = k.GrantACL(ctx, alice.ID, a.ID, PermCall, bob.ID)
	p, root, _ := k.StartProcess(ctx, alice.ID, 0)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  bob.ID,
		ActionName:    "/svc",
		Args:          map[string]any{},
	})
	if err != nil {
		t.Fatalf("expected success with ACL, got: %v", err)
	}

	_ = k.RevokeACL(ctx, alice.ID, a.ID, PermCall, bob.ID)

	_, err = k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  bob.ID,
		ActionName:    "/svc",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected ACL denial after revoke")
	}
}

func TestCallInsufficientFunds(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 50)
	_ = setupAction(t, st, alice.ID, "/expensive", 200)

	p, root, _ := k.StartProcess(ctx, alice.ID, 50)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  alice.ID,
		ActionName:    "/expensive",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected insufficient funds error")
	}
	var ke *KernelError
	if !errors.As(err, &ke) || ke.Code != "insufficient_funds" {
		t.Errorf("expected insufficient_funds error, got %v", err)
	}
}

func TestCallGrossEqualsNetPlusFee(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 2000)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "/paid",
		Kind:        KindWasm,
		Active:      true,
		Price:       100,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	reply, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  alice.ID,
		ActionName:    "/paid",
		Args:          map[string]any{},
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
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "/svc",
		Kind:        KindWasm,
		Active:      true,
		Price:       10,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, alice.ID, 100)
	before := len(st.transactions)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  alice.ID,
		ActionName:    "/svc",
		Args:          map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.transactions)-before != 1 {
		t.Errorf("expected exactly 1 new transaction, got %d", len(st.transactions)-before)
	}
}

func TestCallCreatesChildTrace(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/svc",
		Kind: KindWasm, Active: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, alice.ID, 0)

	reply, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  alice.ID,
		ActionName:    "/svc",
		Args:          map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.TraceID == root.ID {
		t.Error("child trace should have a new ID")
	}
	childTrace, err := st.ReadTrace(ctx, reply.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if childTrace.ParentTraceID != root.ID {
		t.Errorf("child trace ParentTraceID: got %q, want %q", childTrace.ParentTraceID, root.ID)
	}
}

func TestCallFailureRefundsFunds(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{err: ErrExecutionFailed.Wrap("boom")})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/risky",
		Kind: KindWasm, Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  alice.ID,
		ActionName:    "/risky",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Fatal("expected execution failure")
	}

	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Available != 500 {
		t.Errorf("process.available after failure: got %d, want 500", proc.Available)
	}
	if proc.Locked != 0 {
		t.Errorf("process.locked after failure: got %d, want 0", proc.Locked)
	}
}

func TestWasmPanicRefundsFunds(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &panicScriptExec{})
	ctx := context.Background()

	alice := setupUser(t, st, "@wasm-panic-alice", 500)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/panic-svc",
		Kind: KindWasm, Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, alice.ID, 300)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  alice.ID,
		ActionName:    "/panic-svc",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error from panicking WASM executor")
	}

	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Available != 300 {
		t.Errorf("process.available after wasm panic: got %d, want 300", proc.Available)
	}
	if proc.Locked != 0 {
		t.Errorf("process.locked after wasm panic: got %d, want 0", proc.Locked)
	}
}

func TestCallNestedTraceTree(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	for _, name := range []string{"/a", "/b", "/c"} {
		_ = st.CreateAction(ctx, &Action{
			ID: uuid.New().String(), OwnerUserID: alice.ID, Name: name,
			Kind: KindWasm, Active: true, Price: 0, Source: "fake",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
	}

	p, root, _ := k.StartProcess(ctx, alice.ID, 0)

	replyA, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/a", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	replyB, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: replyA.TraceID,
		TargetUserID: alice.ID, ActionName: "/b", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	replyC, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: replyB.TraceID,
		TargetUserID: alice.ID, ActionName: "/c", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	traceA, _ := st.ReadTrace(ctx, replyA.TraceID)
	traceB, _ := st.ReadTrace(ctx, replyB.TraceID)
	traceC, _ := st.ReadTrace(ctx, replyC.TraceID)

	if traceA.ParentTraceID != root.ID {
		t.Errorf("traceA.ParentTraceID: got %q, want %q", traceA.ParentTraceID, root.ID)
	}
	if traceB.ParentTraceID != replyA.TraceID {
		t.Errorf("traceB.ParentTraceID: got %q, want %q", traceB.ParentTraceID, replyA.TraceID)
	}
	if traceC.ParentTraceID != replyB.TraceID {
		t.Errorf("traceC.ParentTraceID: got %q, want %q", traceC.ParentTraceID, replyB.TraceID)
	}
	if traceA.ProcessID != p.ID || traceB.ProcessID != p.ID || traceC.ProcessID != p.ID {
		t.Error("all child traces must belong to the same process")
	}

	seen := map[string]bool{root.ID: true}
	for _, id := range []string{replyA.TraceID, replyB.TraceID, replyC.TraceID} {
		if seen[id] {
			t.Errorf("duplicate trace ID %q", id)
		}
		seen[id] = true
	}
}

func TestCallInputSchemaRejection(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "/strict",
		Kind:        KindWasm,
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
	p, root, _ := k.StartProcess(ctx, alice.ID, 100)

	_, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/strict",
		Args: map[string]any{"wrong_field": "value"},
	})
	if err == nil {
		t.Error("expected schema violation error for missing required field")
	}
	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Locked != 0 {
		t.Errorf("funds should not be locked after schema rejection: locked=%d", proc.Locked)
	}
}

func TestCallOutputSchemaRejection(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"unexpected_field": 42}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "/typed",
		Kind:        KindWasm,
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
	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	_, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/typed", Args: map[string]any{},
	})
	if err == nil {
		t.Error("expected schema violation error for bad output")
	}
	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Locked != 0 {
		t.Errorf("funds should be refunded after output schema rejection: locked=%d", proc.Locked)
	}
	if proc.Available != 500 {
		t.Errorf("process available should be restored: got %d, want 500", proc.Available)
	}
}

func TestFailedExecutionUpdatesTraceLatencyNotCost(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &sleepingFailExec{
		delay: 20 * time.Millisecond,
		err:   fmt.Errorf("boom"),
	})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "/fails",
		Kind:        KindWasm,
		Active:      true,
		Price:       50,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	_, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/fails", Args: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected execution failure")
	}

	txs, err := st.ListTransactions(ctx, TxFilter{ProcessID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 1 {
		t.Fatalf("transactions: got %d, want 1", len(txs))
	}
	tx := txs[0]
	if tx.Status != TxFailure {
		t.Fatalf("tx status: got %s, want failure", tx.Status)
	}
	if tx.Gross != 0 {
		t.Fatalf("failed tx gross: got %d, want 0", tx.Gross)
	}

	child, err := st.ReadTrace(ctx, tx.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Cost != 0 {
		t.Fatalf("child trace cost: got %d, want 0", child.Cost)
	}
	if child.LatencyMS <= 0 {
		t.Fatalf("child trace latency should be updated on failure, got %d", child.LatencyMS)
	}

	rootTrace, err := st.ReadTrace(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rootTrace.Cost != 0 {
		t.Fatalf("root trace cost: got %d, want 0", rootTrace.Cost)
	}
	if rootTrace.LatencyMS <= 0 {
		t.Fatalf("root trace latency should be updated on failure, got %d", rootTrace.LatencyMS)
	}
}

type sleepingFailExec struct {
	delay time.Duration
	err   error
}

func (s *sleepingFailExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (s *sleepingFailExec) Execute(_ context.Context, _ []byte, _ []byte, _ HostFunctions) ([]byte, error) {
	time.Sleep(s.delay)
	return nil, s.err
}

func TestWasmHostCallRespectsACL(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)

	innerAction := &Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "/private",
		Kind: KindNative, Active: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, innerAction)

	outerAction := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/outer",
		Kind: KindWasm, Active: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outerAction)

	exec := &hostCallExec{targetUser: bob.ID, targetAction: "/private"}
	k := newTestKernelWithScripts(st, exec)
	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	_, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/outer", Args: map[string]any{},
	})
	if err == nil {
		t.Error("expected ACL denial when script calls action without permission")
	}
}

type hostCallExec struct {
	targetUser   string
	targetAction string
}

func (h *hostCallExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (h *hostCallExec) Execute(ctx context.Context, _ []byte, _ []byte, host HostFunctions) ([]byte, error) {
	result, err := host.Call(ctx, h.targetUser+"/"+h.targetAction[1:], []byte(`{}`))
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ---- Contractor execution model ----

// contractorExec dispatches based on source: "outer" makes a sub-call, anything else returns {"ok":true}.
type contractorExec struct {
	targetUser   string
	targetAction string
}

func (c *contractorExec) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	return src, "fakehash", nil
}

func (c *contractorExec) Execute(ctx context.Context, src []byte, _ []byte, host HostFunctions) ([]byte, error) {
	if string(src) == "outer" {
		result, err := host.Call(ctx, c.targetUser+"/"+c.targetAction[1:], []byte(`{}`))
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return []byte(`{"ok":true}`), nil
}

func TestContractorSubCallChargedToActionOwner(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()

	// alice owns the outer action (contractor); bob owns the inner action.
	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 500)
	feeUser := setupUser(t, st, "@fee-recipient", 0)

	inner := &Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "/inner",
		Kind: KindWasm, Source: "inner", Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, inner)
	outer := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/outer",
		Kind: KindWasm, Source: "outer", Active: true, Price: 50,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outer)

	exec := &contractorExec{targetUser: bob.ID, targetAction: "/inner"}
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = "test-issuer-id"
	cfg.FeeBPS = 2000
	cfg.FeeRecipientID = feeUser.ID
	cfg.SigningKey = testSigningKey()
	k := New(st, exec, nil, nil, nil, cfg, nil)

	// Grant alice call permission on inner so contractor sub-call passes ACL.
	_ = st.GrantACL(ctx, &ACLEntry{SubjectUserID: alice.ID, ActionID: inner.ID, Permission: PermCall})
	// Grant carol call permission on outer.
	carol := setupUser(t, st, "@carol", 50)
	_ = st.GrantACL(ctx, &ACLEntry{SubjectUserID: carol.ID, ActionID: outer.ID, Permission: PermCall})

	p, root, _ := k.StartProcess(ctx, carol.ID, 50)

	_, err := k.Call(ctx, CallRequest{
		SubjectID: carol.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/outer", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	// Carol's process should be debited only 50 (outer price), not 150.
	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Available != 0 {
		t.Errorf("caller process.available: got %d, want 0 (outer price only)", proc.Available)
	}

	// VAT model: inner taxable=100, fee=20, net=80. Bob: 500+80=580.
	bobUser, _ := st.ReadUser(ctx, bob.ID)
	if bobUser.Available != 580 {
		t.Errorf("inner action owner available: got %d, want 580", bobUser.Available)
	}

	// VAT model: outer gross=50, sub_cost=100 → taxable=0, fee=0, net=50. Alice: 1000-100+50=950.
	aliceUser, _ := st.ReadUser(ctx, alice.ID)
	if aliceUser.Available != 950 {
		t.Errorf("outer action owner available: got %d, want 950", aliceUser.Available)
	}
}

func TestContractorOwnerInsufficientBalanceFails(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 0) // Alice has no balance for sub-calls.
	bob := setupUser(t, st, "@bob", 0)

	inner := &Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "/inner",
		Kind: KindWasm, Source: "inner", Active: true, Price: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, inner)
	outer := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/outer",
		Kind: KindWasm, Source: "outer", Active: true, Price: 50,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outer)

	exec := &contractorExec{targetUser: bob.ID, targetAction: "/inner"}
	k := newTestKernelWithScripts(st, exec)
	_ = st.GrantACL(ctx, &ACLEntry{SubjectUserID: alice.ID, ActionID: inner.ID, Permission: PermCall})

	carol := setupUser(t, st, "@carol", 50)
	_ = st.GrantACL(ctx, &ACLEntry{SubjectUserID: carol.ID, ActionID: outer.ID, Permission: PermCall})
	p, root, _ := k.StartProcess(ctx, carol.ID, 50)

	_, err := k.Call(ctx, CallRequest{
		SubjectID: carol.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/outer", Args: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error when action owner has insufficient balance")
	}

	// Carol must be fully refunded.
	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Available != 50 {
		t.Errorf("caller process.available after failure: got %d, want 50 (full refund)", proc.Available)
	}
	if proc.Locked != 0 {
		t.Errorf("caller process.locked after failure: got %d, want 0", proc.Locked)
	}
}

func TestDirectCallHasNilCausedByTraceID(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 0)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/svc",
		Kind: KindWasm, Active: true, Price: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	p, root, _ := k.StartProcess(ctx, alice.ID, 0)

	reply, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := st.ReadTrace(ctx, reply.TraceID)
	if tr.CausedByTraceID != nil {
		t.Errorf("direct call trace.CausedByTraceID should be nil, got %q", *tr.CausedByTraceID)
	}
}

func TestContractorEphemeralRootHasCausedByTraceID(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 500)
	bob := setupUser(t, st, "@bob", 0)

	inner := &Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "/inner",
		Kind: KindWasm, Source: "inner", Active: true, Price: 0,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, inner)
	outer := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/outer",
		Kind: KindWasm, Source: "outer", Active: true, Price: 0,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, outer)
	_ = st.GrantACL(ctx, &ACLEntry{SubjectUserID: alice.ID, ActionID: inner.ID, Permission: PermCall})

	exec := &contractorExec{targetUser: bob.ID, targetAction: "/inner"}
	k := newTestKernelWithScripts(st, exec)

	p, root, _ := k.StartProcess(ctx, alice.ID, 0)
	reply, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/outer", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	outerCallTraceID := reply.TraceID

	// Find all traces — locate the ephemeral process root (parent == self, not in alice's original process).
	allProcs, _ := st.ListAllProcesses(ctx, 10, 0)
	var epID string
	for _, proc := range allProcs {
		if proc.ID != p.ID {
			epID = proc.ID
			break
		}
	}
	if epID == "" {
		t.Fatal("ephemeral process not found")
	}

	traces, _ := st.ListTraces(ctx, epID)
	var epRoot *Trace
	for _, tr := range traces {
		if tr.ParentTraceID == tr.ID { // self-referential root
			epRoot = tr
			break
		}
	}
	if epRoot == nil {
		t.Fatal("ephemeral root trace not found")
	}

	// Ephemeral root must carry FOLLOWS_FROM to the outer call trace.
	if epRoot.CausedByTraceID == nil {
		t.Fatal("ephemeral root trace.CausedByTraceID should not be nil")
	}
	if *epRoot.CausedByTraceID != outerCallTraceID {
		t.Errorf("ephemeral root caused_by: got %q, want %q", *epRoot.CausedByTraceID, outerCallTraceID)
	}

	// The child call trace within the ephemeral process must NOT have CausedByTraceID.
	for _, tr := range traces {
		if tr.ID == epRoot.ID {
			continue
		}
		if tr.CausedByTraceID != nil {
			t.Errorf("child call trace.CausedByTraceID should be nil, got %q", *tr.CausedByTraceID)
		}
	}
}

func TestDirectCallWithCausedByTraceIDRejected(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 0)
	a := setupAction(t, st, alice.ID, "/svc", 0)
	a.Kind = KindWasm
	a.Active = true
	_ = st.UpdateAction(ctx, a)
	p, root, _ := k.StartProcess(ctx, alice.ID, 0)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:       alice.ID,
		ProcessID:       p.ID,
		ParentTraceID:   root.ID,
		CausedByTraceID: root.ID, // must be empty for direct calls
		TargetUserID:    alice.ID,
		ActionName:      "/svc",
		Args:            map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error when direct call supplies CausedByTraceID")
	}
}

func TestCausedByEqualsParentRejected(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 0)
	a := setupAction(t, st, alice.ID, "/svc", 0)
	a.Kind = KindWasm
	a.Active = true
	_ = st.UpdateAction(ctx, a)
	p, root, _ := k.StartProcess(ctx, alice.ID, 0)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:       alice.ID,
		ProcessID:       p.ID,
		ParentTraceID:   root.ID,
		CausedByTraceID: root.ID, // same as parent — FOLLOWS_FROM must differ from CHILD_OF
		EventID:         "fake-event-id",
		TargetUserID:    alice.ID,
		ActionName:      "/svc",
		Args:            map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error when CausedByTraceID equals ParentTraceID")
	}
}

// ---- Accounting (ComputeFee is defined in call.go) ----

func TestComputeFee(t *testing.T) {
	tests := []struct {
		taxable, gross, feeBPS, wantNet, wantFee int64
	}{
		// taxable == gross (no sub-calls): full fee applies
		{0, 0, 2000, 0, 0},
		{100, 100, 2000, 80, 20},
		{1, 1, 2000, 0, 1},
		{5, 5, 2000, 4, 1},
		{1000, 1000, 2000, 800, 200},
		{1, 1, 0, 1, 0},
		{100, 100, 0, 100, 0},
		{100, 100, 10000, 0, 100},
		// VAT: taxable < gross (sub-calls consumed some gross)
		{0, 50, 2000, 50, 0},   // outer action in contractor test: taxable=0, no fee
		{30, 100, 2000, 94, 6}, // partial sub-cost: taxable=30, fee=ceil(30*0.2)=6, net=94
	}
	for _, tc := range tests {
		net, fee := ComputeFee(tc.taxable, tc.gross, tc.feeBPS)
		if net != tc.wantNet || fee != tc.wantFee {
			t.Errorf("ComputeFee(%d, %d, %d) = (%d, %d), want (%d, %d)",
				tc.taxable, tc.gross, tc.feeBPS, net, fee, tc.wantNet, tc.wantFee)
		}
		if tc.gross > 0 && net+fee != tc.gross {
			t.Errorf("invariant broken: gross=%d net=%d fee=%d", tc.gross, net, fee)
		}
	}
}

func TestComputeFeeInvariant(t *testing.T) {
	for gross := int64(0); gross <= 10000; gross++ {
		net, fee := ComputeFee(gross, gross, 2000)
		if net+fee != gross {
			t.Fatalf("gross=%d: net(%d)+fee(%d) != gross", gross, net, fee)
		}
		if net < 0 || fee < 0 {
			t.Fatalf("gross=%d: negative component net=%d fee=%d", gross, net, fee)
		}
	}
}

// failingCommitStore wraps a fakeStore and makes CommitCall always fail after
// the first call, to verify no transaction is committed on settlement failure.
type failingCommitStore struct {
	*fakeStore
	calls int
}

func (f *failingCommitStore) CommitCall(ctx context.Context, tx *Transaction, receipt *Receipt, processID, targetUserID, feeRecipientID string, net, fee int64, stats *Stats, eventID, idempotencyRecordID string) error {
	f.calls++
	if f.calls > 0 {
		return ErrInternal.Wrap("injected commit failure")
	}
	return f.fakeStore.CommitCall(ctx, tx, receipt, processID, targetUserID, feeRecipientID, net, fee, stats, eventID, idempotencyRecordID)
}

func TestCommitCallAtomicOnFailure(t *testing.T) {
	base := newFakeStore()
	failing := &failingCommitStore{fakeStore: base}
	k := newTestKernel(failing)
	ctx := context.Background()

	caller := setupUser(t, base, "@caller", 1000)
	actionOwner := setupUser(t, base, "@owner", 0)
	a := setupAction(t, base, actionOwner.ID, "/echo", 100)
	base.GrantACL(ctx, &ACLEntry{SubjectUserID: caller.ID, ActionID: a.ID, Permission: PermCall})

	p, root, _ := k.StartProcess(ctx, caller.ID, 500)

	_, err := k.Call(ctx, CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID,
		ParentTraceID: root.ID, TargetUserID: actionOwner.ID,
		ActionName: "/echo", Args: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error from injected commit failure")
	}

	// Caller must be fully refunded — process.available back to 500.
	proc, _ := base.ReadProcess(ctx, p.ID)
	if proc.Available != 500 {
		t.Errorf("caller process.available after commit failure: got %d, want 500", proc.Available)
	}
	if proc.Locked != 0 {
		t.Errorf("caller process.locked after commit failure: got %d, want 0", proc.Locked)
	}

	// No transaction must exist in the store.
	txs, _ := base.ListTransactions(ctx, TxFilter{ProcessID: p.ID})
	for _, tx := range txs {
		if tx.Status == TxSuccess {
			t.Errorf("found committed success transaction despite commit failure: %s", tx.ID)
		}
	}
}

// ---- Trace validation precondition tests ----

func TestCallInvalidParentTraceDoesNotLockFunds(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := setupAction(t, st, alice.ID, "/svc", 100)
	st.GrantACL(ctx, &ACLEntry{SubjectUserID: alice.ID, ActionID: a.ID, Permission: PermCall})

	p, _, _ := k.StartProcess(ctx, alice.ID, 500)

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: "nonexistent-trace-id",
		TargetUserID:  alice.ID,
		ActionName:    "/svc",
		Args:          map[string]any{},
	})
	if !errors.Is(err, ErrInvalidInput) {
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

func TestCallCrossProcessParentTraceDoesNotLockFunds(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := setupAction(t, st, alice.ID, "/svc", 100)
	st.GrantACL(ctx, &ACLEntry{SubjectUserID: alice.ID, ActionID: a.ID, Permission: PermCall})

	p, _, _ := k.StartProcess(ctx, alice.ID, 500)
	other, otherRoot, _ := k.StartProcess(ctx, alice.ID, 0)
	_ = other

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     alice.ID,
		ProcessID:     p.ID,
		ParentTraceID: otherRoot.ID, // belongs to a different process
		TargetUserID:  alice.ID,
		ActionName:    "/svc",
		Args:          map[string]any{},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for cross-process trace, got %v", err)
	}

	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Available != 500 {
		t.Errorf("process.available: got %d, want 500", proc.Available)
	}
	if proc.Locked != 0 {
		t.Errorf("process.locked: got %d, want 0", proc.Locked)
	}
}

// ---- CommitFailedCall settlement error tests ----

type failingCommitFailedCallStore struct {
	*fakeStore
}

func (f *failingCommitFailedCallStore) CommitFailedCall(_ context.Context, _ *Transaction, _ *Receipt, _ string, _ int64, _ *Stats, _ string) error {
	return ErrInternal.Wrap("injected CommitFailedCall failure")
}

func TestCommitFailedCallSettlementError(t *testing.T) {
	base := newFakeStore()
	failing := &failingCommitFailedCallStore{fakeStore: base}
	k := newTestKernelWithScripts(failing, &fakeScriptExec{err: ErrExecutionFailed.Wrap("boom")})
	ctx := context.Background()

	alice := setupUser(t, base, "@alice", 1000)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/risky",
		Kind: KindWasm, Active: true, Price: 100,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = base.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, alice.ID, 500)
	_, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID,
		ParentTraceID: root.ID, TargetUserID: alice.ID,
		ActionName: "/risky", Args: map[string]any{},
	})

	// When CommitFailedCall fails, Call must return ErrInternal (not the original exec error).
	if !errors.Is(err, ErrInternal) {
		t.Errorf("expected ErrInternal when CommitFailedCall fails, got %v", err)
	}
}

// ---- /llm/chat native action tests ----

type fakeChatter struct {
	reply ChatMessage
}

func (f *fakeChatter) Chat(_ context.Context, _ []ChatMessage) (ChatMessage, error) {
	return f.reply, nil
}

func newTestKernelWithChatter(st Store, c Chatter) *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = "test-issuer-id"
	cfg.SigningKey = testSigningKey()
	return New(st, nil, nil, nil, c, cfg, nil)
}

func TestCallLLMChat(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	fc := &fakeChatter{reply: ChatMessage{Role: "assistant", Content: "hello there"}}
	k := newTestKernelWithChatter(st, fc)

	owner := setupUser(t, st, "@sys", 1000)
	chatAction := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "/llm/chat",
		Kind:         KindNative,
		Active:       true,
		Price:        0,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, chatAction)

	p, root, _ := k.StartProcess(ctx, owner.ID, 0)
	reply, err := k.Call(ctx, CallRequest{
		SubjectID:     owner.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  owner.ID,
		ActionName:    "/llm/chat",
		Args: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Call /llm/chat: %v", err)
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
	st := newFakeStore()
	k := newTestKernel(st) // no chatter

	owner := setupUser(t, st, "@sys", 1000)
	chatAction := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "/llm/chat",
		Kind:         KindNative,
		Active:       true,
		Price:        0,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, chatAction)

	p, root, _ := k.StartProcess(ctx, owner.ID, 0)
	_, err := k.Call(ctx, CallRequest{
		SubjectID:     owner.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  owner.ID,
		ActionName:    "/llm/chat",
		Args:          map[string]any{"messages": []any{}},
	})
	if err == nil {
		t.Fatal("expected error when no chatter configured")
	}
}

func TestCallSuspendedSubjectRejected(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 1000)
	target := setupUser(t, st, "@bob", 0)
	_ = setupAction(t, st, target.ID, "/echo", 0)

	p, root, _ := k.StartProcess(ctx, owner.ID, 100)

	if err := st.SuspendUser(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}

	_, err := k.Call(ctx, CallRequest{
		SubjectID:     owner.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  target.ID,
		ActionName:    "/echo",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error for suspended subject")
	}
	var ke *KernelError
	if !errors.As(err, &ke) || ke.Code != "unauthenticated" {
		t.Errorf("expected unauthenticated error, got %v", err)
	}
}

// ---- Fee recipient enforcement ----

func TestCallWithFeeAndNoRecipientRejected(t *testing.T) {
	st := newFakeStore()
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.FeeBPS = 2000 // 20% fee — no FeeRecipientID set
	cfg.IssuerUserID = "test-issuer-id"
	cfg.SigningKey = testSigningKey()
	exec := &fakeScriptExec{result: `{"ok":true}`}
	k := New(st, exec, nil, nil, nil, cfg, nil)
	ctx := context.Background()

	owner := setupUser(t, st, "@owner", 1000)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/paid",
		Kind: KindWasm, Active: true, Price: 100, Public: true,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, owner.ID, 500)
	_, err := k.Call(ctx, CallRequest{
		SubjectID:     owner.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  owner.ID,
		ActionName:    "/paid",
		Args:          map[string]any{},
	})
	if !errors.Is(err, ErrInvalidState) {
		t.Errorf("expected ErrInvalidState when fee > 0 and FeeRecipientID is empty, got %v", err)
	}

	// Funds must be fully refunded — process.available back to 500.
	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Available != 500 {
		t.Errorf("process.available after fee pre-check failure: got %d, want 500", proc.Available)
	}
	if proc.Locked != 0 {
		t.Errorf("process.locked after fee pre-check failure: got %d, want 0", proc.Locked)
	}
}
