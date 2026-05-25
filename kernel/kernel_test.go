package kernel

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/daios/juice/log"
	"github.com/google/uuid"
)

func newTestKernel(st Store) *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.FeeBPS = 2000
	return New(st, nil, nil, cfg, log.Default())
}

func newTestKernelWithScripts(st Store, exec ScriptExecutor) *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.FeeBPS = 2000
	return New(st, exec, nil, cfg, log.Default())
}

// setupUser creates a user with the given handle and a starting balance.
func setupUser(t *testing.T, st *fakeStore, handle string, balance int64) *User {
	t.Helper()
	hash, err := HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	u := &User{
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

// setupAction creates an active action owned by the given user.
func setupAction(t *testing.T, st *fakeStore, ownerID, name string, price int64) *Action {
	t.Helper()
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Name:        name,
		Kind:        KindNative, // native so no execution; tests override
		Active:      true,
		Price:       price,
		Source:      "native",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func setupProcess(t *testing.T, k *Kernel, ownerID string, funds int64) (*Process, *Trace) {
	t.Helper()
	p, tr, err := k.StartProcess(context.Background(), ownerID, funds)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	return p, tr
}

// ---- User tests ----

func TestCreateUser(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	u, err := k.CreateUser(ctx, CreateUserRequest{
		Handle:   "@alice",
		Email:    "alice@example.com",
		Password: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle != "@alice" {
		t.Errorf("handle: got %q, want %q", u.Handle, "@alice")
	}
	if u.PasswordHash == "secret" {
		t.Error("password should be hashed")
	}
}

func TestLogin(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	_, err := k.CreateUser(ctx, CreateUserRequest{
		Handle:   "@bob",
		Email:    "bob@example.com",
		Password: "mypass",
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := k.Login(ctx, "@bob", "mypass")
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Error("expected non-empty token")
	}

	subjectID, err := k.VerifyToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := k.ReadUserByHandle(ctx, "@bob")
	if subjectID != u.ID {
		t.Errorf("token subject: got %q, want %q", subjectID, u.ID)
	}

	// Wrong password.
	if _, err := k.Login(ctx, "@bob", "wrong"); err == nil {
		t.Error("expected error for wrong password")
	}
}

// ---- Process tests ----

func TestStartAndEndProcess(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@owner", 1000)

	p, root, err := k.StartProcess(ctx, owner.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available != 500 {
		t.Errorf("process.available: got %d, want 500", p.Available)
	}
	if root.ParentTraceID != root.ID {
		t.Error("root trace must have ParentTraceID == ID")
	}

	// Owner balance reduced by funded amount.
	u, _ := st.ReadUser(ctx, owner.ID)
	if u.Available != 500 {
		t.Errorf("owner balance after funding: got %d, want 500", u.Available)
	}

	// End process — funds return to owner.
	if err := k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	u, _ = st.ReadUser(ctx, owner.ID)
	if u.Available != 1000 {
		t.Errorf("owner balance after end: got %d, want 1000", u.Available)
	}

	// Closed process cannot be ended again.
	if err := k.EndProcess(ctx, owner.ID, p.ID); err == nil {
		t.Error("expected error ending closed process")
	}
}

// ---- Call invariant tests ----

func TestCallClosedProcessFails(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 1000)
	target := setupUser(t, st, "@bob", 0)
	a := setupAction(t, st, target.ID, "/echo", 0)
	_ = a

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
		Active:      false, // inactive
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
	a := setupAction(t, st, bob.ID, "/private", 0)
	_ = a

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

	// Grant call permission.
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

	// Revoke and try again.
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
	target := setupAction(t, st, alice.ID, "/expensive", 200) // price > process balance
	_ = target

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

	after := len(st.transactions)
	if after-before != 1 {
		t.Errorf("expected exactly 1 new transaction, got %d", after-before)
	}
}

func TestCallCreatesChildTrace(t *testing.T) {
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
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
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

	// Child trace must differ from root and have root as parent.
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
	errExec := &fakeScriptExec{err: ErrExecutionFailed.Wrap("boom")}
	k := newTestKernelWithScripts(st, errExec)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "/risky",
		Kind:        KindWasm,
		Active:      true,
		Price:       100,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
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

	// Process should have full 500 available (funds unlocked after failure).
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
	// Use KindWasm so the fakeScriptExec handles execution (KindNative cannot be called directly).
	for _, name := range []string{"/a", "/b", "/c"} {
		_ = st.CreateAction(ctx, &Action{
			ID: uuid.New().String(), OwnerUserID: alice.ID, Name: name,
			Kind: KindWasm, Active: true, Price: 0,
			Source:    "fake",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
	}

	p, root, _ := k.StartProcess(ctx, alice.ID, 0)

	// Level 1
	replyA, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/a", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Level 2
	replyB, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: replyA.TraceID,
		TargetUserID: alice.ID, ActionName: "/b", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Level 3
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

	// All four IDs (root + 3 children) must be unique.
	seen := map[string]bool{root.ID: true}
	for _, id := range []string{replyA.TraceID, replyB.TraceID, replyC.TraceID} {
		if seen[id] {
			t.Errorf("duplicate trace ID %q", id)
		}
		seen[id] = true
	}
}

func TestProcessAvailablePlusLockedInvariant(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 2000)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "/svc",
		Kind:        KindWasm,
		Active:      true,
		Price:       100,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	checkInvariant := func(tag string, wantSum int64) {
		t.Helper()
		proc, err := st.ReadProcess(ctx, p.ID)
		if err != nil {
			t.Fatalf("%s: ReadProcess: %v", tag, err)
		}
		if proc.Locked < 0 {
			t.Errorf("%s: locked is negative: %d", tag, proc.Locked)
		}
		if proc.Available < 0 {
			t.Errorf("%s: available is negative: %d", tag, proc.Available)
		}
		if wantSum >= 0 && proc.Available+proc.Locked != wantSum {
			t.Errorf("%s: available(%d)+locked(%d)=%d, want %d",
				tag, proc.Available, proc.Locked, proc.Available+proc.Locked, wantSum)
		}
	}

	checkInvariant("initial", 500)

	// During a successful call the lock should be 0 after settlement.
	_, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// After settlement locked must be 0; available reduced by the fee.
	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Locked != 0 {
		t.Errorf("after call: locked must be 0, got %d", proc.Locked)
	}

	// Fund: available increases, locked stays 0.
	if err := k.FundProcess(ctx, alice.ID, p.ID, 300); err != nil {
		t.Fatal(err)
	}
	checkInvariant("after fund", -1) // sum varies; just check no negatives

	proc, _ = st.ReadProcess(ctx, p.ID)
	if proc.Locked != 0 {
		t.Errorf("after fund: locked must be 0, got %d", proc.Locked)
	}

	// End: all available returned to owner, both fields become 0.
	if err := k.EndProcess(ctx, alice.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	checkInvariant("after end", 0)
}

// fakeScriptExec is a ScriptExecutor for tests.
type fakeScriptExec struct {
	result string
	err    error
}

func (f *fakeScriptExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (f *fakeScriptExec) Execute(_ context.Context, _ []byte, input []byte, _ HostFunctions) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.result != "" {
		return []byte(f.result), nil
	}
	return input, nil
}

// ---- Accounting ----

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

// ---- Stats ----

func TestIncrementalMean(t *testing.T) {
	var mean float64
	for i, v := range []float64{10, 20, 30} {
		mean = IncrementalMean(mean, int64(i), v)
	}
	if math.Abs(mean-20) > 1e-9 {
		t.Errorf("expected mean 20, got %f", mean)
	}
}

func TestUpdateStats(t *testing.T) {
	s := DefaultStats("action-1")
	now := time.Now()

	success := &Transaction{
		Status:    TxSuccess,
		Gross:     100,
		StartedAt: now.Add(-500 * time.Millisecond),
		EndedAt:   now,
	}
	UpdateStats(s, success, 0.5)

	if s.Uses != 1 {
		t.Errorf("uses: got %d, want 1", s.Uses)
	}
	if s.Successes != 1 || s.Failures != 0 {
		t.Errorf("successes=%d failures=%d, want 1/0", s.Successes, s.Failures)
	}
	if math.Abs(float64(s.PriceMean)-100) > 1e-6 {
		t.Errorf("price_mean: got %f, want 100", s.PriceMean)
	}
	if math.Abs(s.LatencyMean-0.5) > 1e-6 {
		t.Errorf("latency_mean: got %f, want 0.5", s.LatencyMean)
	}

	UpdateStats(s, &Transaction{Status: TxFailure, StartedAt: now, EndedAt: now}, 0.2)
	if s.Uses != 2 || s.Failures != 1 {
		t.Errorf("after failure: uses=%d failures=%d, want 2/1", s.Uses, s.Failures)
	}
	if math.Abs(float64(s.PriceMean)-100) > 1e-6 {
		t.Error("price_mean should not change on failure")
	}
}

// ---- Events ----

func setupActiveWasmAction(t *testing.T, st *fakeStore, ownerID, name string) *Action {
	t.Helper()
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Name:        name,
		Kind:        KindWasm,
		Active:      true,
		Price:       0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCreateListener(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 500)
	source := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, owner.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, owner.ID, 100)

	l, err := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID:    owner.ID,
		SourceUserID:   source.ID,
		EventName:      "ping",
		ProcessID:      p.ID,
		TraceID:        root.ID,
		TargetActionID: a.ID,
	})
	if err != nil {
		t.Fatalf("CreateListener: %v", err)
	}
	if l.ID == "" || !l.Active {
		t.Error("expected active listener with ID")
	}
}

func TestCreateListenerClosedProcessFails(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 100)
	source := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, owner.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, owner.ID, 100)
	_ = k.EndProcess(ctx, owner.ID, p.ID)

	_, err := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID:    owner.ID,
		SourceUserID:   source.ID,
		EventName:      "ping",
		ProcessID:      p.ID,
		TraceID:        root.ID,
		TargetActionID: a.ID,
	})
	if err == nil {
		t.Error("expected error creating listener on closed process")
	}
}

func TestDeleteListener(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 100)
	source := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, owner.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, owner.ID, 100)

	l, _ := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID: owner.ID, SourceUserID: source.ID, EventName: "ping",
		ProcessID: p.ID, TraceID: root.ID, TargetActionID: a.ID,
	})

	if err := k.DeleteListener(ctx, owner.ID, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteListener(ctx, source.ID, l.ID); err == nil {
		t.Error("expected authorization error")
	}
}

func TestEmitEvent(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"fired":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	l, err := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID: alice.ID, SourceUserID: bob.ID, EventName: "greet",
		ProcessID: p.ID, TraceID: root.ID, TargetActionID: a.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	txIDs, err := k.EmitEvent(ctx, bob.ID, "greet", map[string]any{"msg": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(txIDs) != 1 {
		t.Errorf("expected 1 tx from emit, got %d", len(txIDs))
	}

	queued, err := k.PollListener(ctx, alice.ID, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0] != txIDs[0] {
		t.Errorf("poll: got %v, want [%s]", queued, txIDs[0])
	}
}

func TestEmitInactiveListenerNotFired(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	bob := setupUser(t, st, "@bob", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	l, _ := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID: alice.ID, SourceUserID: bob.ID, EventName: "greet",
		ProcessID: p.ID, TraceID: root.ID, TargetActionID: a.ID,
	})
	_ = k.DeleteListener(ctx, alice.ID, l.ID)

	txIDs, _ := k.EmitEvent(ctx, bob.ID, "greet", nil)
	if len(txIDs) != 0 {
		t.Errorf("expected 0 fired for inactive listener, got %d", len(txIDs))
	}
}

func TestPollListenerUnauthorized(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 100)
	source := setupUser(t, st, "@bob", 0)
	stranger := setupUser(t, st, "@carol", 0)
	a := setupActiveWasmAction(t, st, alice.ID, "/handler")
	p, root, _ := k.StartProcess(ctx, alice.ID, 100)
	l, _ := k.CreateListener(ctx, CreateListenerRequest{
		OwnerUserID: alice.ID, SourceUserID: source.ID, EventName: "x",
		ProcessID: p.ID, TraceID: root.ID, TargetActionID: a.ID,
	})

	if _, err := k.PollListener(ctx, stranger.ID, l.ID); err == nil {
		t.Error("expected unauthorized error for stranger polling")
	}
	if _, err := k.PollListener(ctx, source.ID, l.ID); err != nil {
		t.Errorf("source user should be able to poll: %v", err)
	}
}

// ---- Feedback ----

func TestRecursiveFeedbackSingleCall(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 1000)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/svc",
		Kind: KindWasm, Active: true, Price: 50,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

	reply, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	fb, err := k.RecursiveFeedback(ctx, p.ID, reply.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if fb.RecursiveCost != 50 {
		t.Errorf("recursive cost: got %d, want 50", fb.RecursiveCost)
	}
	if fb.RecursiveLatency < 0 {
		t.Errorf("recursive latency should be non-negative: %f", fb.RecursiveLatency)
	}
}

func TestRecursiveFeedbackNoCalls(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 100)
	p, root, _ := k.StartProcess(ctx, alice.ID, 100)

	fb, err := k.RecursiveFeedback(ctx, p.ID, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fb.RecursiveCost != 0 {
		t.Errorf("expected 0 cost for empty trace, got %d", fb.RecursiveCost)
	}
}

func TestCollectSubtree(t *testing.T) {
	children := map[string][]string{"root": {"a", "b"}, "a": {"c"}}
	sub := collectSubtree("root", children)
	for _, id := range []string{"root", "a", "b", "c"} {
		if !sub[id] {
			t.Errorf("expected %q in subtree", id)
		}
	}
	if sub["nonexistent"] {
		t.Error("unexpected node in subtree")
	}
}

// ---- Lookup ----

type fakeEmbedder struct{}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	dims := 8
	vec := make([]float32, dims)
	for i, b := range []byte(text) {
		vec[i%dims] += float32(b)
	}
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	if norm > 0 {
		x := norm
		for i := 0; i < 10; i++ {
			x = (x + norm/x) / 2
		}
		for i := range vec {
			vec[i] /= x
		}
	}
	return vec, nil
}

func newTestKernelWithEmbedder(st Store, emb Embedder) *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.FeeBPS = 2000
	return New(st, nil, emb, cfg, nil)
}

func TestLookupRanking(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{})
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	for _, desc := range []struct{ name, text string }{
		{"/weather", "weather forecast temperature rain"},
		{"/news", "latest news headlines today"},
	} {
		_ = st.CreateAction(ctx, &Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: desc.name,
			Kind: KindHTTP, Active: true, Description: desc.text,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
	}

	results, err := k.Lookup(ctx, LookupRequest{Query: "weather forecast", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].Action.Name != "/weather" {
		t.Errorf("expected /weather first, got %s", results[0].Action.Name)
	}
	for _, r := range results {
		if r.Score < 0 {
			t.Errorf("score should be non-negative: %f", r.Score)
		}
	}
}

func TestLookupRankingWithStats(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{})
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	ids := map[string]string{}
	for _, name := range []string{"/reliable", "/unreliable"} {
		a := &Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: name,
			Kind: KindHTTP, Active: true, Description: "compute data results",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		_ = st.CreateAction(ctx, a)
		ids[name] = a.ID
	}
	_ = st.UpsertStats(ctx, &Stats{ActionID: ids["/reliable"], Uses: 10, Successes: 10, LastUsedAt: time.Now()})
	_ = st.UpsertStats(ctx, &Stats{ActionID: ids["/unreliable"], Uses: 10, Successes: 2, LastUsedAt: time.Now()})

	results, err := k.Lookup(ctx, LookupRequest{Query: "compute data", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatal("expected at least 2 results")
	}
	if results[0].Action.Name != "/reliable" {
		t.Errorf("reliable action should rank first; got %s", results[0].Action.Name)
	}
}

func TestLookupNoEmbedder(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	_, err := k.Lookup(context.Background(), LookupRequest{Query: "test"})
	if err == nil {
		t.Error("expected error when no embedder configured")
	}
}

func TestLookupInactiveActionsExcluded(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{})
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	_ = st.CreateAction(ctx, &Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/hidden",
		Kind: KindHTTP, Active: false, Description: "hidden service do not show",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	results, _ := k.Lookup(ctx, LookupRequest{Query: "hidden service", Limit: 10})
	for _, r := range results {
		if r.Action.Name == "/hidden" {
			t.Error("inactive action should not appear in lookup results")
		}
	}
}

// ---- Schema enforcement in calls ----

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

func (h *hostCallExec) Execute(ctx context.Context, _ []byte, input []byte, host HostFunctions) ([]byte, error) {
	result, err := host.Call(ctx, h.targetUser+"/"+h.targetAction[1:], []byte(`{}`))
	if err != nil {
		return nil, err
	}
	return result, nil
}

// suppress unused import warning from absorbed files
var _ = log.Default
