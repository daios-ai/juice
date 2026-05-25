package kernel

import (
	"context"
	"errors"
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

func TestCallInactiveActionDeniedForNonOwner(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 1000)
	target := setupUser(t, st, "@bob", 0)

	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: target.ID,
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
		TargetUserID:  target.ID,
		ActionName:    "/svc",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected error calling inactive action as non-owner")
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

// ---- Accounting (ComputeFee is defined in call.go) ----

func TestComputeFee(t *testing.T) {
	tests := []struct {
		gross, feeBPS, wantNet, wantFee int64
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
		net, fee := ComputeFee(tc.gross, tc.feeBPS)
		if net != tc.wantNet || fee != tc.wantFee {
			t.Errorf("ComputeFee(%d, %d) = (%d, %d), want (%d, %d)",
				tc.gross, tc.feeBPS, net, fee, tc.wantNet, tc.wantFee)
		}
		if tc.gross > 0 && net+fee != tc.gross {
			t.Errorf("invariant broken: gross=%d net=%d fee=%d", tc.gross, net, fee)
		}
	}
}

func TestComputeFeeInvariant(t *testing.T) {
	for gross := int64(0); gross <= 10000; gross++ {
		net, fee := ComputeFee(gross, 2000)
		if net+fee != gross {
			t.Fatalf("gross=%d: net(%d)+fee(%d) != gross", gross, net, fee)
		}
		if net < 0 || fee < 0 {
			t.Fatalf("gross=%d: negative component net=%d fee=%d", gross, net, fee)
		}
	}
}
