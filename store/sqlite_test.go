package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrationsAreFileBackedAndRecorded(t *testing.T) {
	db := openTestDB(t)

	files, err := migrationFileNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 1 {
		t.Fatalf("expected at least 1 migration file, got %d", len(files))
	}
	for _, file := range files {
		version := filepath.Base(file[:len(file)-len(filepath.Ext(file))])
		applied, err := db.migrationApplied(version)
		if err != nil {
			t.Fatalf("migrationApplied(%s): %v", version, err)
		}
		if !applied {
			t.Fatalf("migration %s was not recorded", version)
		}
	}

	for _, tc := range []struct {
		table  string
		column string
	}{
		{"actions", "embed_vec"},
		{"action_stats", "rating_count"},
		{"traces", "action_id"},
		{"traces", "caller_user_id"},
		{"traces", "idempotency_key"},
		{"traces", "dispatch_json"},
		{"steps", "completion_trace_id"},
		{"actions", "auth_json"},
	} {
		if !db.columnExists(tc.table, tc.column) {
			t.Fatalf("expected %s.%s to exist after migrations", tc.table, tc.column)
		}
	}
}

func newUser(handle string, balance int64) *kernel.User {
	return &kernel.User{
		ID:           uuid.New().String(),
		Handle:       handle,
		Email:        handle + "@test.com",
		PasswordHash: "hash",
		Available:    balance,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
}

func newAction(ownerID, name string, price int64, active bool) *kernel.Action {
	return &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Name:        name,
		Kind:        kernel.KindHTTP,
		Active:      active,
		Price:       price,
		Source:      "http://localhost/test",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
}

func newProcess(ownerID string) *kernel.Process {
	return &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Available:   0,
		Locked:      0,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
}

// ---- User CRUD ----

func TestUserCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("@alice", 0)
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	got, err := db.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Handle != u.Handle {
		t.Errorf("handle: got %q, want %q", got.Handle, u.Handle)
	}

	got2, err := db.ReadUserByHandle(ctx, "@alice")
	if err != nil {
		t.Fatal(err)
	}
	if got2.ID != u.ID {
		t.Errorf("id via handle: got %q, want %q", got2.ID, u.ID)
	}

	_, err = db.ReadUser(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for missing user")
	}
}

// ---- Action CRUD ----

func TestActionCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@owner", 0)
	_ = db.CreateUser(ctx, owner)

	a := newAction(owner.ID, "/hello", 10, false)
	if err := db.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	got, err := db.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != a.Name {
		t.Errorf("name: got %q, want %q", got.Name, a.Name)
	}

	got.Active = true
	if err := db.UpdateAction(ctx, got); err != nil {
		t.Fatal(err)
	}
	updated, _ := db.ReadAction(ctx, a.ID)
	if !updated.Active {
		t.Error("expected action to be active after update")
	}

	if err := db.DeleteAction(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	_, err = db.ReadAction(ctx, a.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestDeleteActionSoftDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@hd-owner", 0)
	_ = db.CreateUser(ctx, owner)

	a := newAction(owner.ID, "/hd-svc", 0, true)
	if err := db.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAction(ctx, a.ID); err != nil {
		t.Fatalf("DeleteAction: %v", err)
	}

	// Row is preserved (soft delete: deleted_at is set, not physically removed).
	var deletedAt *string
	if err := db.db.QueryRow("SELECT deleted_at FROM actions WHERE id=?", a.ID).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if deletedAt == nil {
		t.Error("expected deleted_at to be set after soft delete")
	}

	// ReadAction returns not found (deleted action is hidden from lookup).
	if _, err := db.ReadAction(ctx, a.ID); err == nil {
		t.Error("expected error reading deleted action, got nil")
	}

	// Deleted action is absent from owner listing.
	actions, err := db.ListActionsByOwner(ctx, owner.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range actions {
		if listed.ID == a.ID {
			t.Error("deleted action should not appear in ListActionsByOwner")
		}
	}
}

func TestListActions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@owner", 0)
	_ = db.CreateUser(ctx, owner)

	active := newAction(owner.ID, "/active", 0, true)
	active.Public = true
	inactive := newAction(owner.ID, "/inactive", 0, false)
	inactive.Public = true
	private := newAction(owner.ID, "/private", 0, true)
	_ = db.CreateAction(ctx, active)
	_ = db.CreateAction(ctx, inactive)
	_ = db.CreateAction(ctx, private)

	all, err := db.ListAllActions(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("ListAllActions: got %d, want 3", len(all))
	}

	publicActive, err := db.ListPublicActions(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(publicActive) != 1 || publicActive[0].Name != "/active" {
		t.Errorf("ListPublicActions: unexpected result")
	}
}

// ---- Fund operations ----

func TestCreateProcessDeductsFunds(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 1000)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)

	if err := db.CreateProcess(ctx, p, user.ID, 400); err != nil {
		t.Fatal(err)
	}

	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Available != 400 {
		t.Errorf("process.available: got %d, want 400", proc.Available)
	}

	u, _ := db.ReadUser(ctx, user.ID)
	if u.Available != 600 {
		t.Errorf("user.available: got %d, want 600", u.Available)
	}
	if u.Locked != 400 {
		t.Errorf("user.locked: got %d, want 400", u.Locked)
	}

	// Insufficient funds should fail.
	p2 := newProcess(user.ID)
	if err := db.CreateProcess(ctx, p2, user.ID, 9999); err == nil {
		t.Error("expected error for insufficient funds")
	}
}

func TestBeginRootCallAndSubcall(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 500)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	if err := db.CreateProcess(ctx, p, user.ID, 500); err != nil {
		t.Fatal(err)
	}

	// BeginRootCall locks price from process.available into process.locked,
	// and creates the root trace with available=price.
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 500); err != nil {
		t.Fatal(err)
	}
	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Available != 0 || proc.Locked != 500 {
		t.Errorf("after BeginRootCall: available=%d locked=%d, want 0/500", proc.Available, proc.Locked)
	}

	// BeginSubcall locks price from root.available into root.locked,
	// and creates the child trace with available=price.
	child := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ParentTraceID: nullStr(root.ID), CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, root.ID, child, 200); err != nil {
		t.Fatal(err)
	}
	rootRead, _ := db.ReadTrace(ctx, root.ID)
	if rootRead.Available != 300 || rootRead.Locked != 200 {
		t.Errorf("root after subcall: available=%d locked=%d, want 300/200", rootRead.Available, rootRead.Locked)
	}

	// BeginSubcall for more than available should fail.
	child2 := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ParentTraceID: nullStr(root.ID), CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, root.ID, child2, 400); err == nil {
		t.Error("expected error subcalling more than available")
	}
}

func TestCommitCall(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	payer := newUser("@payer", 1000)
	target := newUser("@target", 0)
	fee := newUser("@fee", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, target)
	_ = db.CreateUser(ctx, fee)

	p := newProcess(payer.ID)
	if err := db.CreateProcess(ctx, p, payer.ID, 1000); err != nil {
		t.Fatal(err)
	}
	// Root call: lock 100 from process.
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 100); err != nil {
		t.Fatal(err)
	}

	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID, ParentTraceID: "",
		OwnerUserID: payer.ID, CallerUserID: payer.ID, TargetUserID: target.ID,
		ActionID: "a1", Status: kernel.TxSuccess, Gross: 100, Net: 80, Fee: 20,
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: payer.ID, TxID: tx.ID, TraceID: root.ID, ActionID: "a1",
		ArgsHash: "ah1", ReplyHash: "rh1", Status: kernel.TxSuccess,
		Gross: 100, Net: 80, Fee: 20, CreatedAt: time.Now().UTC(),
	}
	// Root call: callerWalletID=p.ID, callerWalletKind=CallerProcess
	if err := db.CommitCall(ctx, tx, receipt, root.ID, p.ID, kernel.CallerProcess, target.ID, fee.ID, 80, 20, nil, "", ""); err != nil {
		t.Fatal(err)
	}

	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Locked != 0 {
		t.Errorf("process.locked after commit: got %d, want 0", proc.Locked)
	}

	tgt, _ := db.ReadUser(ctx, target.ID)
	if tgt.Available != 80 {
		t.Errorf("target.available: got %d, want 80", tgt.Available)
	}

	feeU, _ := db.ReadUser(ctx, fee.ID)
	if feeU.Available != 20 {
		t.Errorf("fee.available: got %d, want 20", feeU.Available)
	}

	// Transaction must be recorded atomically.
	stored, err := db.ReadTransaction(ctx, tx.ID)
	if err != nil || stored.Status != kernel.TxSuccess {
		t.Errorf("transaction not committed: %v", err)
	}
}

func TestEndProcess(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 1000)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	if err := db.CreateProcess(ctx, p, user.ID, 600); err != nil {
		t.Fatal(err)
	}

	if err := db.EndProcess(ctx, p.ID); err != nil {
		t.Fatal(err)
	}

	// Funds returned to owner; locked must be zero.
	u, _ := db.ReadUser(ctx, user.ID)
	if u.Available != 1000 {
		t.Errorf("user available after end: got %d, want 1000", u.Available)
	}
	if u.Locked != 0 {
		t.Errorf("user locked after end: got %d, want 0", u.Locked)
	}

	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Status != kernel.ProcessClosed {
		t.Errorf("process status: got %q, want closed", proc.Status)
	}
	if proc.Available != 0 || proc.Locked != 0 {
		t.Errorf("process funds after end: available=%d locked=%d, want 0/0", proc.Available, proc.Locked)
	}
}

func TestEndProcessWithLockedFundsForceCloseSucceeds(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice-locked", 500)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	if err := db.CreateProcess(ctx, p, user.ID, 500); err != nil {
		t.Fatal(err)
	}
	// Lock 200 via BeginRootCall — simulates an in-flight root call.
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 200); err != nil {
		t.Fatal(err)
	}

	// store.EndProcess no longer guards on locked > 0; the kernel settles traces before calling it.
	err := db.EndProcess(ctx, p.ID)
	if err != nil {
		t.Fatalf("EndProcess should succeed even with locked funds; got: %v", err)
	}
}


// TestEndProcessWithRunningStep verifies Fix B: EndProcess atomically drains completion
// traces for running steps, cancels those steps, and returns their funds to the owner.
func TestEndProcessWithRunningStep(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@ep-running", 1000)
	_ = db.CreateUser(ctx, user)
	caller := newUser("@ep-running-caller", 0)
	_ = db.CreateUser(ctx, caller)

	p := newProcess(user.ID)
	if err := db.CreateProcess(ctx, p, user.ID, 100); err != nil {
		t.Fatal(err)
	}

	// BeginRootCall with price=50 — root trace gets available=50; process.locked=50.
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 50); err != nil {
		t.Fatal(err)
	}

	act := newAction(user.ID, "ep-act", 50, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}

	ptID := root.ID
	step := &kernel.Step{
		ID:                   uuid.New().String(),
		ProcessID:            p.ID,
		ParentTraceID:        &ptID,
		RequiredCallerUserID: caller.ID,
		NextActionID:         act.ID,
		Price:                50,
		Status:               kernel.StepWaiting,
		CreatedAt:            time.Now().UTC(),
	}
	if err := db.CreateStep(ctx, step); err != nil {
		t.Fatal(err)
	}

	// BeginStepCall: releases root.locked, creates completion trace with available=50.
	ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginStepCall(ctx, step.ID, ct); err != nil {
		t.Fatal(err)
	}

	// Verify pre-conditions.
	ct1, _ := db.ReadTrace(ctx, ct.ID)
	if ct1.Available != 50 {
		t.Fatalf("pre: completion_trace.available=%d, want 50", ct1.Available)
	}

	if err := db.EndProcess(ctx, p.ID); err != nil {
		t.Fatalf("EndProcess: %v", err)
	}

	// Step must be cancelled.
	s, _ := db.ReadStep(ctx, step.ID)
	if s.Status != kernel.StepCancelled {
		t.Errorf("step.status=%s, want cancelled", s.Status)
	}

	// Completion trace must be drained.
	ct2, _ := db.ReadTrace(ctx, ct.ID)
	if ct2.Available != 0 {
		t.Errorf("completion_trace.available=%d after EndProcess, want 0", ct2.Available)
	}

	// Process must be closed with no funds.
	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Status != kernel.ProcessClosed {
		t.Errorf("process.status=%s, want closed", proc.Status)
	}
	if proc.Available != 0 || proc.Locked != 0 {
		t.Errorf("process funds after close: available=%d locked=%d, want 0/0", proc.Available, proc.Locked)
	}

	// User must be fully restored: all 100 returned.
	u, _ := db.ReadUser(ctx, user.ID)
	if u.Available != 1000 {
		t.Errorf("user.available=%d, want 1000 (full restoration)", u.Available)
	}
	if u.Locked != 0 {
		t.Errorf("user.locked=%d, want 0", u.Locked)
	}
}

// TestEndProcessDoesNotDoubleCountCompletedStep verifies that a step which was
// successfully completed before EndProcess is called is left as 'done' and its
// funds are not double-counted in the refund.
func TestEndProcessDoesNotDoubleCountCompletedStep(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@ep-done", 1000)
	_ = db.CreateUser(ctx, user)
	caller := newUser("@ep-done-caller", 0)
	_ = db.CreateUser(ctx, caller)

	p := newProcess(user.ID)
	if err := db.CreateProcess(ctx, p, user.ID, 100); err != nil {
		t.Fatal(err)
	}

	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 50); err != nil {
		t.Fatal(err)
	}

	act := newAction(user.ID, "ep-done-act", 50, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}

	ptID := root.ID
	step := &kernel.Step{
		ID:                   uuid.New().String(),
		ProcessID:            p.ID,
		ParentTraceID:        &ptID,
		RequiredCallerUserID: caller.ID,
		NextActionID:         act.ID,
		Price:                50,
		Status:               kernel.StepWaiting,
		CreatedAt:            time.Now().UTC(),
	}
	if err := db.CreateStep(ctx, step); err != nil {
		t.Fatal(err)
	}

	ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginStepCall(ctx, step.ID, ct); err != nil {
		t.Fatal(err)
	}

	// Simulate successful call completion: step is done, tx_id is set, trace is consumed.
	fakeTxID := uuid.New().String()
	_, err := db.db.ExecContext(ctx,
		`UPDATE steps SET status='done', tx_id=? WHERE id=?`, fakeTxID, step.ID)
	if err != nil {
		t.Fatalf("mark step done: %v", err)
	}
	_, err = db.db.ExecContext(ctx,
		`UPDATE traces SET available=0 WHERE id=?`, ct.ID)
	if err != nil {
		t.Fatalf("drain completion trace: %v", err)
	}

	if err := db.EndProcess(ctx, p.ID); err != nil {
		t.Fatalf("EndProcess: %v", err)
	}

	// Step must still be 'done', not re-cancelled by Fix B.
	s, _ := db.ReadStep(ctx, step.ID)
	if s.Status != kernel.StepDone {
		t.Errorf("step.status=%s, want done (Fix B must not re-cancel completed steps)", s.Status)
	}

	// No negative balances — funds must not be double-counted.
	u, _ := db.ReadUser(ctx, user.ID)
	if u.Available < 0 {
		t.Errorf("user.available=%d, must not go negative (double-counted refund)", u.Available)
	}
	if u.Locked < 0 {
		t.Errorf("user.locked=%d, must not go negative", u.Locked)
	}
}

// ---- Stats ----

func TestStats(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@owner", 0)
	_ = db.CreateUser(ctx, owner)
	a := newAction(owner.ID, "/svc", 0, true)
	_ = db.CreateAction(ctx, a)

	stats, err := db.ReadStats(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats != nil {
		t.Error("expected nil stats before upsert")
	}

	s := &kernel.Stats{
		ActionID:   a.ID,
		Uses:       5,
		Successes:  4,
		Failures:   1,
		LastUsedAt: time.Now().UTC(),
	}
	if err := db.UpsertStats(ctx, s); err != nil {
		t.Fatal(err)
	}

	got, err := db.ReadStats(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Uses != 5 {
		t.Errorf("uses: got %d, want 5", got.Uses)
	}
}

func TestCommitCallIncrementalStats(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	payer := newUser("@payer-inc", 1000)
	target := newUser("@target-inc", 0)
	fee := newUser("@fee-inc", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, target)
	_ = db.CreateUser(ctx, fee)

	a := newAction(payer.ID, "/inc-svc", 100, true)
	_ = db.CreateAction(ctx, a)

	// Each call uses its own process so auto-close on the first doesn't block the second.
	makeProcess := func() *kernel.Process {
		pr := newProcess(payer.ID)
		if err := db.CreateProcess(ctx, pr, payer.ID, 500); err != nil {
			t.Fatalf("CreateProcess: %v", err)
		}
		return pr
	}
	beginRootTrace := func(pr *kernel.Process, price int64) *kernel.Trace {
		tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: pr.ID, CreatedAt: time.Now().UTC()}
		if err := db.BeginRootCall(ctx, pr.ID, tr, price); err != nil {
			t.Fatalf("BeginRootCall: %v", err)
		}
		return tr
	}
	makeTx := func(id, processID, traceID string, gross int64) *kernel.Transaction {
		return &kernel.Transaction{
			ID: id, ProcessID: processID, TraceID: traceID, ParentTraceID: "",
			OwnerUserID: payer.ID, CallerUserID: payer.ID, TargetUserID: target.ID,
			ActionID: a.ID, Status: kernel.TxSuccess, Gross: gross, Net: gross * 8 / 10, Fee: gross * 2 / 10,
			StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
		}
	}
	makeReceipt := func(id, txID, traceID string, gross int64) *kernel.Receipt {
		return &kernel.Receipt{
			ID: id, IssuerUserID: payer.ID, TxID: txID, TraceID: traceID, ActionID: a.ID,
			ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxSuccess,
			Gross: gross, Net: gross * 8 / 10, Fee: gross * 2 / 10, CreatedAt: time.Now().UTC(),
		}
	}

	p1 := makeProcess()
	tr1 := beginRootTrace(p1, 100)
	tx1 := makeTx(uuid.New().String(), p1.ID, tr1.ID, 100)
	rc1 := makeReceipt(uuid.New().String(), tx1.ID, tr1.ID, 100)
	stats1 := &kernel.Stats{ActionID: a.ID, Uses: 1, Successes: 1, LatencyEstimate: 0.1, LastUsedAt: time.Now().UTC()}
	if err := db.CommitCall(ctx, tx1, rc1, tr1.ID, p1.ID, kernel.CallerProcess, target.ID, fee.ID, tx1.Net, tx1.Fee, stats1, "", ""); err != nil {
		t.Fatalf("CommitCall #1: %v", err)
	}

	p2 := makeProcess()
	tr2 := beginRootTrace(p2, 50)
	tx2 := makeTx(uuid.New().String(), p2.ID, tr2.ID, 50)
	rc2 := makeReceipt(uuid.New().String(), tx2.ID, tr2.ID, 50)
	stats2 := &kernel.Stats{ActionID: a.ID, Uses: 1, Successes: 1, LatencyEstimate: 0.3, LastUsedAt: time.Now().UTC()}
	if err := db.CommitCall(ctx, tx2, rc2, tr2.ID, p2.ID, kernel.CallerProcess, target.ID, fee.ID, tx2.Net, tx2.Fee, stats2, "", ""); err != nil {
		t.Fatalf("CommitCall #2: %v", err)
	}

	got, err := db.ReadStats(ctx, a.ID)
	if err != nil {
		t.Fatalf("ReadStats: %v", err)
	}
	if got.Uses != 2 || got.Successes != 2 {
		t.Errorf("uses=%d successes=%d, want 2/2", got.Uses, got.Successes)
	}
	// Mean of [0.1, 0.3] = 0.2
	if math.Abs(got.LatencyEstimate-0.2) > 1e-6 {
		t.Errorf("latency_estimate: got %f, want 0.2", got.LatencyEstimate)
	}
	// rating_count must not be reset to 0 (stays at 0 since no ratings, but must not error)
	if got.RatingCount != 0 {
		t.Errorf("rating_count: got %d, want 0", got.RatingCount)
	}
}

// ---- ListTraces ----

func TestListTraces(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 200)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	if err := db.CreateProcess(ctx, p, user.ID, 200); err != nil {
		t.Fatal(err)
	}

	// Create root trace then a subcall.
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 200); err != nil {
		t.Fatalf("BeginRootCall: %v", err)
	}

	child := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ParentTraceID: nullStr(root.ID),
		CreatedAt:     time.Now().UTC(),
	}
	if err := db.BeginSubcall(ctx, root.ID, child, 100); err != nil {
		t.Fatalf("BeginSubcall: %v", err)
	}

	traces, err := db.ListTraces(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 2 {
		t.Errorf("expected 2 traces, got %d", len(traces))
	}
}

// ---- Auth codes ----

func TestAuthCodeFlow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@dave", 0)
	_ = db.CreateUser(ctx, user)

	ac := &kernel.AuthCode{
		Code:          "testcode123",
		UserID:        user.ID,
		CodeChallenge: "challenge",
		RedirectURI:   "http://localhost:9999/cb",
		ExpiresAt:     time.Now().UTC().Add(10 * time.Minute),
	}
	if err := db.CreateAuthCode(ctx, ac); err != nil {
		t.Fatal(err)
	}

	// Consume once — should succeed.
	got, err := db.ConsumeAuthCode(ctx, "testcode123")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != user.ID {
		t.Errorf("user ID: got %q, want %q", got.UserID, user.ID)
	}

	// Consume again — should fail (already used).
	_, err = db.ConsumeAuthCode(ctx, "testcode123")
	if err == nil {
		t.Error("expected error consuming used auth code")
	}

	// Non-existent code should fail.
	_, err = db.ConsumeAuthCode(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent code")
	}
}

// ---- Refresh tokens ----

func TestRefreshTokenRotation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@eve", 0)
	_ = db.CreateUser(ctx, user)

	rt := &kernel.RefreshToken{
		Token:     "initial-refresh-token",
		UserID:    user.ID,
		ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateRefreshToken(ctx, rt); err != nil {
		t.Fatal(err)
	}

	// Rotate — should get a new token.
	newRT, err := db.RotateRefreshToken(ctx, "initial-refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	if newRT.Token == "initial-refresh-token" {
		t.Error("rotated token should differ from old token")
	}
	if newRT.UserID != user.ID {
		t.Errorf("user ID: got %q, want %q", newRT.UserID, user.ID)
	}

	// Old token is now revoked.
	_, err = db.RotateRefreshToken(ctx, "initial-refresh-token")
	if err == nil {
		t.Error("expected error rotating already-revoked token")
	}

	// New token can be rotated.
	newest, err := db.RotateRefreshToken(ctx, newRT.Token)
	if err != nil {
		t.Fatalf("expected new token to be rotatable: %v", err)
	}
	if newest.Token == newRT.Token {
		t.Error("second rotation should produce a fresh token")
	}
}

// ---- Transactions ----

func TestTransactionCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@owner", 100)
	target := newUser("@target", 0)
	feeUser := newUser("@fee-crud", 0)
	_ = db.CreateUser(ctx, owner)
	_ = db.CreateUser(ctx, target)
	_ = db.CreateUser(ctx, feeUser)

	a := newAction(owner.ID, "/svc", 100, true)
	_ = db.CreateAction(ctx, a)

	p := newProcess(owner.ID)
	if err := db.CreateProcess(ctx, p, owner.ID, 100); err != nil {
		t.Fatal(err)
	}
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 100); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	tx := &kernel.Transaction{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		TraceID:       root.ID,
		ParentTraceID: "",
		OwnerUserID:   owner.ID,
		CallerUserID:  owner.ID,
		TargetUserID:  target.ID,
		ActionID:      a.ID,
		Status:        kernel.TxSuccess,
		Gross:         100,
		Net:           80,
		Fee:           20,
		StartedAt:     now,
		EndedAt:       now,
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: owner.ID, TxID: tx.ID, TraceID: root.ID, ActionID: a.ID,
		ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxSuccess,
		Gross: 100, Net: 80, Fee: 20, CreatedAt: now,
	}
	if err := db.CommitCall(ctx, tx, receipt, root.ID, p.ID, kernel.CallerProcess, target.ID, feeUser.ID, 80, 20, nil, "", ""); err != nil {
		t.Fatal(err)
	}

	got, err := db.ReadTransaction(ctx, tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Gross != 100 || got.Net != 80 || got.Fee != 20 {
		t.Errorf("transaction amounts: gross=%d net=%d fee=%d", got.Gross, got.Net, got.Fee)
	}
	if got.Gross != got.Net+got.Fee {
		t.Errorf("invariant broken: gross=%d != net=%d + fee=%d", got.Gross, got.Net, got.Fee)
	}

	// List by owner.
	txs, err := db.ListTransactions(ctx, kernel.TxFilter{OwnerUserID: owner.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 1 {
		t.Errorf("list: got %d, want 1", len(txs))
	}
}

func TestListProcesses(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("@lp-owner", 1000)
	other := newUser("@lp-other", 0)
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}

	// Create two processes for u and one for other.
	for i, ownerID := range []string{u.ID, u.ID, other.ID} {
		p := &kernel.Process{
			ID:          uuid.New().String(),
			OwnerUserID: ownerID,
			Status:      kernel.ProcessOpen,
			CreatedAt:   time.Now().UTC(),
		}
		_ = i
		if err := db.CreateProcess(ctx, p, ownerID, 0); err != nil {
			t.Fatalf("CreateProcess %d: %v", i, err)
		}
	}

	got, err := db.ListProcesses(ctx, u.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("ListProcesses: got %d, want 2", len(got))
	}
	for _, p := range got {
		if p.OwnerUserID != u.ID {
			t.Errorf("ListProcesses: unexpected owner %s", p.OwnerUserID)
		}
	}
}


func TestRevokeRefreshToken(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("@rt-user", 0)
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	tok := &kernel.RefreshToken{
		Token:     "test-token-abc",
		UserID:    u.ID,
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateRefreshToken(ctx, tok); err != nil {
		t.Fatal(err)
	}

	// Revoking a valid token succeeds.
	if err := db.RevokeRefreshToken(ctx, tok.Token); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}

	// Revoking again returns an error.
	if err := db.RevokeRefreshToken(ctx, tok.Token); err == nil {
		t.Error("expected error revoking already-revoked token")
	}

	// Rotating a revoked token fails.
	if _, err := db.RotateRefreshToken(ctx, tok.Token); err == nil {
		t.Error("expected error rotating revoked token")
	}
}

// ---- Receipt tests ----

func TestCreateReadReceipt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	issuer := newUser("@issuer", 0)
	_ = db.CreateUser(ctx, issuer)

	tx := &kernel.Transaction{
		ID:            uuid.New().String(),
		OwnerUserID:   issuer.ID,
		CallerUserID: issuer.ID,
		TargetUserID:  issuer.ID,
		ActionID:      uuid.New().String(),
		Status:        kernel.TxSuccess,
		StartedAt:     time.Now().UTC(),
		EndedAt:       time.Now().UTC(),
	}
	_ = db.createTransaction(ctx, tx)

	r := &kernel.Receipt{
		ID:           uuid.New().String(),
		IssuerUserID: issuer.ID,
		TxID:         tx.ID,
		TraceID:      uuid.New().String(),
		ActionID:     tx.ActionID,
		ArgsHash:     "abc123",
		ReplyHash:    "def456",
		Status:       kernel.TxSuccess,
		Gross:        0,
		Net:          0,
		Fee:          0,
		Reason:       "",
		CreatedAt:    time.Now().UTC(),
		Signature:    "sig",
	}
	if err := db.createReceipt(ctx, r); err != nil {
		t.Fatalf("CreateReceipt: %v", err)
	}

	got, err := db.ReadReceiptByTxID(ctx, tx.ID)
	if err != nil {
		t.Fatalf("ReadReceiptByTxID: %v", err)
	}
	if got.ID != r.ID {
		t.Errorf("receipt.ID: got %q, want %q", got.ID, r.ID)
	}
	if got.IssuerUserID != issuer.ID {
		t.Errorf("receipt.IssuerUserID: got %q, want %q", got.IssuerUserID, issuer.ID)
	}
	if got.ArgsHash != "abc123" {
		t.Errorf("receipt.ArgsHash: got %q, want %q", got.ArgsHash, "abc123")
	}
}

// ---- Rating tests ----

func TestCreateRatingDirect(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	rater := newUser("@rater", 0)
	_ = db.CreateUser(ctx, rater)

	tx := &kernel.Transaction{
		ID:            uuid.New().String(),
		OwnerUserID:   rater.ID,
		CallerUserID: rater.ID,
		TargetUserID:  rater.ID,
		ActionID:      uuid.New().String(),
		Status:        kernel.TxSuccess,
		StartedAt:     time.Now().UTC(),
		EndedAt:       time.Now().UTC(),
	}
	_ = db.createTransaction(ctx, tx)

	note := "store test note"
	rating := &kernel.Rating{
		ID:          uuid.New().String(),
		RatedTxID:   tx.ID,
		RaterUserID: rater.ID,
		Rating:      1.0,
		Note:        &note,
		CreatedAt:   time.Now().UTC(),
		Signature:   "",
	}
	if err := db.createRating(ctx, rating); err != nil {
		t.Fatalf("CreateRating: %v", err)
	}

	got, err := db.ReadRatingByTxID(ctx, tx.ID)
	if err != nil {
		t.Fatalf("ReadRatingByTxID: %v", err)
	}
	if got.Rating != 1.0 {
		t.Errorf("rating.Rating: got %f, want 1.0", got.Rating)
	}
	if got.RaterUserID != rater.ID {
		t.Errorf("rating.RaterUserID: got %q, want %q", got.RaterUserID, rater.ID)
	}
	if got.Note == nil || *got.Note != note {
		t.Errorf("rating.Note: got %v, want %q", got.Note, note)
	}

	// Duplicate rating must fail.
	dup := &kernel.Rating{
		ID:          uuid.New().String(),
		RatedTxID:   tx.ID,
		RaterUserID: rater.ID,
		Rating:      0.0,
		CreatedAt:   time.Now().UTC(),
	}
	if err := db.createRating(ctx, dup); err == nil {
		t.Error("expected error for duplicate rating")
	}
}

func TestCreateRatingAndUpdateStats(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@owner", 1000)
	_ = db.CreateUser(ctx, owner)
	action := newAction(owner.ID, "/a", 10, true)
	_ = db.CreateAction(ctx, action)

	// Seed stats so rating_count starts at 0.
	if err := db.UpsertStats(ctx, &kernel.Stats{
		ActionID: action.ID,
		LastUsedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertStats: %v", err)
	}

	rater := newUser("@rater", 0)
	_ = db.CreateUser(ctx, rater)
	tx := &kernel.Transaction{
		ID:            uuid.New().String(),
		OwnerUserID:   rater.ID,
		CallerUserID: rater.ID,
		TargetUserID:  owner.ID,
		ActionID:      action.ID,
		Status:        kernel.TxSuccess,
		StartedAt:     time.Now().UTC(),
		EndedAt:       time.Now().UTC(),
	}
	_ = db.createTransaction(ctx, tx)

	r := &kernel.Rating{
		ID:          uuid.New().String(),
		RatedTxID:   tx.ID,
		RaterUserID: rater.ID,
		Rating:      1.0,
		CreatedAt:   time.Now().UTC(),
	}
	if err := db.CreateRatingAndUpdateStats(ctx, r, action.ID, 1.0); err != nil {
		t.Fatalf("CreateRatingAndUpdateStats: %v", err)
	}

	// Rating row must exist.
	got, err := db.ReadRatingByTxID(ctx, tx.ID)
	if err != nil {
		t.Fatalf("ReadRatingByTxID: %v", err)
	}
	if got.Rating != 1.0 {
		t.Errorf("rating: got %f, want 1.0", got.Rating)
	}

	// Stats must reflect the rating.
	stats, err := db.ReadStats(ctx, action.ID)
	if err != nil {
		t.Fatalf("ReadStats: %v", err)
	}
	if stats.RatingCount != 1 {
		t.Errorf("RatingCount: got %d, want 1", stats.RatingCount)
	}
	if stats.RatingEstimate != 1.0 {
		t.Errorf("RatingEstimate: got %f, want 1.0", stats.RatingEstimate)
	}

	// Second rating (0) must update the running mean atomically.
	rater2 := newUser("@rater2", 0)
	_ = db.CreateUser(ctx, rater2)
	tx2 := &kernel.Transaction{
		ID:            uuid.New().String(),
		OwnerUserID:   rater2.ID,
		CallerUserID: rater2.ID,
		TargetUserID:  owner.ID,
		ActionID:      action.ID,
		Status:        kernel.TxSuccess,
		StartedAt:     time.Now().UTC(),
		EndedAt:       time.Now().UTC(),
	}
	_ = db.createTransaction(ctx, tx2)
	r2 := &kernel.Rating{
		ID:          uuid.New().String(),
		RatedTxID:   tx2.ID,
		RaterUserID: rater2.ID,
		Rating:      0.0,
		CreatedAt:   time.Now().UTC(),
	}
	if err := db.CreateRatingAndUpdateStats(ctx, r2, action.ID, 0.0); err != nil {
		t.Fatalf("CreateRatingAndUpdateStats second: %v", err)
	}
	stats2, _ := db.ReadStats(ctx, action.ID)
	if stats2.RatingCount != 2 {
		t.Errorf("RatingCount after second: got %d, want 2", stats2.RatingCount)
	}
	if stats2.RatingEstimate != 0.5 {
		t.Errorf("RatingEstimate after second: got %f, want 0.5", stats2.RatingEstimate)
	}
}

func TestListRatings(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@list-owner", 0)
	_ = db.CreateUser(ctx, owner)
	action := newAction(owner.ID, "/list-a", 0, true)
	_ = db.CreateAction(ctx, action)

	rater := newUser("@list-rater", 0)
	_ = db.CreateUser(ctx, rater)

	makeTxAndRating := func(id string, rating float64, offset time.Duration) {
		tx := &kernel.Transaction{
			ID:            id,
			OwnerUserID:   rater.ID,
			CallerUserID: rater.ID,
			TargetUserID:  owner.ID,
			ActionID:      action.ID,
			Status:        kernel.TxSuccess,
			StartedAt:     time.Now().UTC(),
			EndedAt:       time.Now().UTC(),
		}
		_ = db.createTransaction(ctx, tx)
		r := &kernel.Rating{
			ID:          "r-" + id,
			RatedTxID:   id,
			RaterUserID: rater.ID,
			Rating:      rating,
			CreatedAt:   time.Now().UTC().Add(offset),
		}
		_ = db.createRating(ctx, r)
	}
	makeTxAndRating("tx-list-1", 1.0, 0)
	makeTxAndRating("tx-list-2", 0.0, time.Second)

	// List all: should return 2 in DESC order.
	all, err := db.ListRatings(ctx, action.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListRatings: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 ratings, got %d", len(all))
	}
	if all[0].RatedTxID != "tx-list-2" {
		t.Errorf("first result should be newest (tx-list-2), got %s", all[0].RatedTxID)
	}

	// Offset skips the first.
	page2, _ := db.ListRatings(ctx, action.ID, 10, 1)
	if len(page2) != 1 || page2[0].RatedTxID != "tx-list-1" {
		t.Errorf("offset=1 should return tx-list-1, got %v", page2)
	}

	// Wrong action ID returns empty.
	none, _ := db.ListRatings(ctx, "no-such-action", 10, 0)
	if len(none) != 0 {
		t.Errorf("expected no ratings for unknown action, got %d", len(none))
	}
}

func TestListTransactionsByParty(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@tx-party-owner", 0)   // seller
	caller := newUser("@tx-party-caller", 0) // buyer
	other := newUser("@tx-party-other", 0)   // non-party
	_ = db.CreateUser(ctx, owner)
	_ = db.CreateUser(ctx, caller)
	_ = db.CreateUser(ctx, other)
	action := newAction(owner.ID, "/party-tx", 0, true)
	_ = db.CreateAction(ctx, action)

	p := newProcess(caller.ID)
	if err := db.CreateProcess(ctx, p, caller.ID, 0); err != nil {
		t.Fatal(err)
	}

	mkTx := func(id string, offset time.Duration) {
		tx := &kernel.Transaction{
			ID:            id,
			ProcessID:     p.ID,
			TraceID:       id + "-tr",
			ParentTraceID: id + "-tr",
			OwnerUserID:   caller.ID,
			CallerUserID: caller.ID,
			TargetUserID:  owner.ID,
			ActionID:      action.ID,
			Status:        kernel.TxSuccess,
			StartedAt:     time.Now().UTC().Add(offset),
			EndedAt:       time.Now().UTC().Add(offset),
		}
		_ = db.createTransaction(ctx, tx)
	}
	mkTx("party-tx-1", 0)
	mkTx("party-tx-2", time.Second)

	// Seller (action owner) sees both, newest first.
	asSeller, err := db.ListTransactions(ctx, kernel.TxFilter{PartyUserID: owner.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListTransactions seller: %v", err)
	}
	if len(asSeller) != 2 || asSeller[0].ID != "party-tx-2" {
		t.Fatalf("seller should see 2 txs newest-first, got %v", asSeller)
	}

	// Buyer (process owner) sees both too.
	asBuyer, _ := db.ListTransactions(ctx, kernel.TxFilter{PartyUserID: caller.ID, Limit: 10})
	if len(asBuyer) != 2 {
		t.Fatalf("buyer should see 2 txs, got %d", len(asBuyer))
	}

	// A non-party sees none.
	none, _ := db.ListTransactions(ctx, kernel.TxFilter{PartyUserID: other.ID, Limit: 10})
	if len(none) != 0 {
		t.Errorf("non-party should see 0 txs, got %d", len(none))
	}

	// Caller distinct from process owner: caller_user_id should also grant access.
	distinctCaller := newUser("@tx-party-distinct-caller", 0)
	_ = db.CreateUser(ctx, distinctCaller)
	txDistinct := &kernel.Transaction{
		ID:            "party-tx-distinct",
		ProcessID:     p.ID,
		TraceID:       "party-tx-distinct-tr",
		ParentTraceID: "party-tx-distinct-tr",
		OwnerUserID:   caller.ID,    // process owner
		CallerUserID: distinctCaller.ID, // distinct call caller
		TargetUserID:  owner.ID,
		ActionID:      action.ID,
		Status:        kernel.TxSuccess,
		StartedAt:     time.Now().UTC(),
		EndedAt:       time.Now().UTC(),
	}
	_ = db.createTransaction(ctx, txDistinct)
	asDistinctCaller, _ := db.ListTransactions(ctx, kernel.TxFilter{PartyUserID: distinctCaller.ID, Limit: 10})
	if len(asDistinctCaller) != 1 || asDistinctCaller[0].ID != "party-tx-distinct" {
		t.Errorf("distinct caller should see their transaction, got %d txs", len(asDistinctCaller))
	}
}

// ---- ReadUserByPublicKey tests ----

func TestReadUserByPublicKey(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("@remote", 0)
	u.PublicKey = "ed25519pubkeyABC"
	u.RemoteBaseURL = "https://remote.example.com"
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	got, err := db.ReadUserByPublicKey(ctx, "ed25519pubkeyABC")
	if err != nil {
		t.Fatalf("ReadUserByPublicKey: %v", err)
	}
	if got.Handle != "@remote" {
		t.Errorf("handle: got %q, want @remote", got.Handle)
	}
	if got.RemoteBaseURL != "https://remote.example.com" {
		t.Errorf("RemoteBaseURL: got %q", got.RemoteBaseURL)
	}

	// Unknown key returns ErrNotFound.
	if _, err := db.ReadUserByPublicKey(ctx, "unknown-key"); err == nil {
		t.Error("expected error for unknown public key")
	}
}

// ---- Idempotency record tests ----

func TestIdempotencyStateMachine(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cp := newUser("@cp-sm", 0)
	_ = db.CreateUser(ctx, cp)

	now := time.Now().UTC()
	rec := &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     "sm-key-1",
		CounterpartyUserID: cp.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}

	// InsertPendingIdempotencyRecord succeeds on first call.
	if err := db.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatalf("InsertPendingIdempotencyRecord: %v", err)
	}

	// Status is "pending".
	got, err := db.ReadIdempotencyRecord(ctx, "sm-key-1", cp.ID)
	if err != nil {
		t.Fatalf("ReadIdempotencyRecord: %v", err)
	}
	if got.Status != "pending" {
		t.Errorf("status: want pending, got %q", got.Status)
	}

	// Duplicate insert returns unique constraint error.
	dup := &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     "sm-key-1",
		CounterpartyUserID: cp.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	if err := db.InsertPendingIdempotencyRecord(ctx, dup); err == nil {
		t.Error("expected unique constraint error on duplicate pending insert")
	}

	// completeIdempotencyRecord transitions to "complete" with result JSON.
	if err := db.completeIdempotencyRecord(ctx, rec.ID, `{"answer":42}`, ""); err != nil {
		t.Fatalf("completeIdempotencyRecord: %v", err)
	}
	got2, _ := db.ReadIdempotencyRecord(ctx, "sm-key-1", cp.ID)
	if got2.Status != "complete" {
		t.Errorf("status after complete: want complete, got %q", got2.Status)
	}
	if got2.ResultJSON != `{"answer":42}` {
		t.Errorf("result_json: got %q", got2.ResultJSON)
	}

	// DeleteIdempotencyRecord removes the record so retry is possible.
	rec2 := &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     "sm-key-2",
		CounterpartyUserID: cp.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	_ = db.InsertPendingIdempotencyRecord(ctx, rec2)
	if err := db.DeleteIdempotencyRecord(ctx, rec2.ID); err != nil {
		t.Fatalf("DeleteIdempotencyRecord: %v", err)
	}
	if _, err := db.ReadIdempotencyRecord(ctx, "sm-key-2", cp.ID); err == nil {
		t.Error("expected ErrNotFound after delete")
	}
	// Re-insert is possible after delete.
	rec2b := &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     "sm-key-2",
		CounterpartyUserID: cp.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	if err := db.InsertPendingIdempotencyRecord(ctx, rec2b); err != nil {
		t.Errorf("re-insert after delete should succeed: %v", err)
	}
}

func TestListActionsByOwnerOpenAPISpec(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@oapi-owner", 0)
	other := newUser("@oapi-other", 0)
	_ = db.CreateUser(ctx, owner)
	_ = db.CreateUser(ctx, other)

	specURL := "https://spec.example.com/api.json"
	specURL2 := "https://spec.example.com/api2.json"

	makeSrc := func(su, key string) string {
		src := kernel.OpenAPISource{
			Type: "openapi", SpecURL: su, OperationKey: key,
			BaseURL: "https://api.example.com", Method: "GET", Path: "/" + key,
		}
		b, _ := json.Marshal(src)
		return string(b)
	}

	// Action matching owner + specURL.
	a1 := newAction(owner.ID, "@oapi-owner/op1", 0, false)
	a1.Source = makeSrc(specURL, "op1")
	_ = db.CreateAction(ctx, a1)

	// Action matching owner + specURL2 (different spec; must not appear).
	a2 := newAction(owner.ID, "@oapi-owner/op2", 0, false)
	a2.Source = makeSrc(specURL2, "op2")
	_ = db.CreateAction(ctx, a2)

	// Action owned by other user for specURL (must not appear).
	a3 := newAction(other.ID, "@oapi-other/op1", 0, false)
	a3.Source = makeSrc(specURL, "op1")
	_ = db.CreateAction(ctx, a3)

	// Plain HTTP action with no OpenAPI source (must not appear).
	a4 := newAction(owner.ID, "@oapi-owner/plain", 0, false)
	_ = db.CreateAction(ctx, a4)

	got, err := db.ListActionsByOwnerOpenAPISpec(ctx, owner.ID, specURL)
	if err != nil {
		t.Fatalf("ListActionsByOwnerOpenAPISpec: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 action, got %d", len(got))
	}
	if got[0].ID != a1.ID {
		t.Errorf("id: got %q, want %q", got[0].ID, a1.ID)
	}
	_ = a2
	_ = a3
	_ = a4
}

func TestInitFirstBootConfigPreservesExisting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("@sys", 0)
	configs := map[string]string{"signing_key": "original-value"}
	if err := db.InitFirstBoot(ctx, u, configs); err != nil {
		t.Fatalf("InitFirstBoot: %v", err)
	}

	// Simulate a manual config update after first boot.
	if err := db.SetConfig(ctx, "signing_key", "updated-value"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	// Calling InitFirstBoot again must not clobber the updated value.
	if err := db.InitFirstBoot(ctx, u, configs); err != nil {
		t.Fatalf("InitFirstBoot second call: %v", err)
	}

	got, err := db.GetConfig(ctx, "signing_key")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if got != "updated-value" {
		t.Errorf("signing_key: got %q, want %q", got, "updated-value")
	}
}

// ---- Fee destruction prevention ----

func TestCommitCallFeeDestructionRejected(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	payer := newUser("@payer-fd", 1000)
	target := newUser("@target-fd", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, target)

	p := newProcess(payer.ID)
	if err := db.CreateProcess(ctx, p, payer.ID, 1000); err != nil {
		t.Fatal(err)
	}
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 100); err != nil {
		t.Fatal(err)
	}

	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID, ParentTraceID: "",
		OwnerUserID: payer.ID, CallerUserID: payer.ID, TargetUserID: target.ID,
		ActionID: "a1", Status: kernel.TxSuccess, Gross: 100, Net: 80, Fee: 20,
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: payer.ID, TxID: tx.ID, TraceID: root.ID,
		ActionID: "a1", ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxSuccess,
		Gross: 100, Net: 80, Fee: 20, CreatedAt: time.Now().UTC(),
	}

	// fee=20 with empty feeRecipientID must be rejected.
	err := db.CommitCall(ctx, tx, receipt, root.ID, p.ID, kernel.CallerProcess, target.ID, "", 80, 20, nil, "", "")
	if err == nil {
		t.Fatal("expected error when fee > 0 and feeRecipientID is empty, got nil")
	}
}

// ---- Idempotency atomicity in CommitCall / CommitFailedCall ----

func newIdempotencyRecord(cpID string) *kernel.IdempotencyRecord {
	now := time.Now().UTC()
	return &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     uuid.New().String(),
		CounterpartyUserID: cpID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
}

func TestCommitCallCompletesIdempotencyRecordAtomically(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	payer := newUser("@payer-idem", 1000)
	target := newUser("@target-idem", 0)
	fee := newUser("@fee-idem", 0)
	cp := newUser("@cp-idem", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, target)
	_ = db.CreateUser(ctx, fee)
	_ = db.CreateUser(ctx, cp)

	p := newProcess(payer.ID)
	if err := db.CreateProcess(ctx, p, payer.ID, 1000); err != nil {
		t.Fatal(err)
	}
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 100); err != nil {
		t.Fatal(err)
	}

	rec := newIdempotencyRecord(cp.ID)
	if err := db.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatalf("InsertPendingIdempotencyRecord: %v", err)
	}

	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID, ParentTraceID: "",
		OwnerUserID: payer.ID, CallerUserID: payer.ID, TargetUserID: target.ID,
		ActionID: "a1", Status: kernel.TxSuccess, Gross: 100, Net: 100, Fee: 0,
		ReplyJSON: json.RawMessage(`{"ok":true}`),
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: payer.ID, TxID: tx.ID, TraceID: root.ID,
		ActionID: "a1", ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxSuccess,
		Gross: 100, Net: 100, Fee: 0, CreatedAt: time.Now().UTC(),
	}

	if err := db.CommitCall(ctx, tx, receipt, root.ID, p.ID, kernel.CallerProcess, target.ID, "", 100, 0, nil, rec.ID, ""); err != nil {
		t.Fatalf("CommitCall: %v", err)
	}

	got, err := db.ReadIdempotencyRecord(ctx, rec.IdempotencyKey, cp.ID)
	if err != nil {
		t.Fatalf("ReadIdempotencyRecord: %v", err)
	}
	if got.Status != "complete" {
		t.Errorf("status: want complete, got %q", got.Status)
	}
	if got.ResultJSON != `{"ok":true}` {
		t.Errorf("result_json: got %q, want {\"ok\":true}", got.ResultJSON)
	}
	var storedReceipt kernel.Receipt
	if err := json.Unmarshal([]byte(got.ReceiptJSON), &storedReceipt); err != nil {
		t.Errorf("receipt_json is not valid receipt JSON: %v", err)
	}
}

func TestCommitFailedCallCompletesIdempotencyRecordAtomically(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	payer := newUser("@payer-idem2", 1000)
	cp := newUser("@cp-idem2", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, cp)

	p := newProcess(payer.ID)
	if err := db.CreateProcess(ctx, p, payer.ID, 1000); err != nil {
		t.Fatal(err)
	}
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 100); err != nil {
		t.Fatal(err)
	}

	rec := newIdempotencyRecord(cp.ID)
	if err := db.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatalf("InsertPendingIdempotencyRecord: %v", err)
	}

	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID, ParentTraceID: "",
		OwnerUserID: payer.ID, CallerUserID: payer.ID, TargetUserID: payer.ID,
		ActionID: "a1", Status: kernel.TxFailure, Gross: 0, Net: 0, Fee: 0,
		Reason:    "execution failed",
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: payer.ID, TxID: tx.ID, TraceID: root.ID,
		ActionID: "a1", ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxFailure,
		Gross: 0, Net: 0, Fee: 0, CreatedAt: time.Now().UTC(),
	}

	// Root call: callerWalletID=p.ID, callerWalletKind=CallerProcess
	buildFn := func(_ int64) (*kernel.Receipt, error) { return receipt, nil }
	if err := db.CommitFailedCall(ctx, tx, buildFn, root.ID, p.ID, kernel.CallerProcess, 100, nil, rec.ID, "execution_failed", ""); err != nil {
		t.Fatalf("CommitFailedCall: %v", err)
	}

	got, err := db.ReadIdempotencyRecord(ctx, rec.IdempotencyKey, cp.ID)
	if err != nil {
		t.Fatalf("ReadIdempotencyRecord: %v", err)
	}
	if got.Status != "complete" {
		t.Errorf("status: want complete, got %q", got.Status)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(got.ResultJSON), &result); err != nil {
		t.Errorf("result_json is not valid JSON: %v", err)
	}
	if result["error"] != "execution failed" {
		t.Errorf("result_json[\"error\"]: got %q, want \"execution failed\"", result["error"])
	}
}

func TestUpsertAndListEmbeddings(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@owner-emb", 0)
	_ = db.CreateUser(ctx, owner)

	active := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/active",
		Kind: kernel.KindHTTP, Active: true, Public: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	inactive := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/inactive",
		Kind: kernel.KindHTTP, Active: false, Public: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = db.CreateAction(ctx, active)
	_ = db.CreateAction(ctx, inactive)

	vec := []float32{0.1, 0.2, 0.3}
	if err := db.UpsertEmbedding(ctx, active.ID, vec); err != nil {
		t.Fatalf("UpsertEmbedding active: %v", err)
	}
	if err := db.UpsertEmbedding(ctx, inactive.ID, vec); err != nil {
		t.Fatalf("UpsertEmbedding inactive: %v", err)
	}

	embeddings, err := db.ListEmbeddings(ctx)
	if err != nil {
		t.Fatalf("ListEmbeddings: %v", err)
	}
	if _, ok := embeddings[active.ID]; !ok {
		t.Error("active action embedding missing from ListEmbeddings")
	}
	if _, ok := embeddings[inactive.ID]; ok {
		t.Error("inactive action embedding must not appear in ListEmbeddings (active=0)")
	}

	// UpsertEmbedding is idempotent.
	vec2 := []float32{0.4, 0.5, 0.6}
	if err := db.UpsertEmbedding(ctx, active.ID, vec2); err != nil {
		t.Fatalf("UpsertEmbedding overwrite: %v", err)
	}
	embeddings, _ = db.ListEmbeddings(ctx)
	got := embeddings[active.ID]
	if len(got) != len(vec2) || got[0] != vec2[0] {
		t.Errorf("UpsertEmbedding overwrite: got %v, want %v", got, vec2)
	}
}

// ---- Federation store methods ----

func TestUpdateRemoteProxySourceURLs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@proxy-owner", 0)
	owner.PublicKey = "validkey"
	owner.RemoteBaseURL = "https://old.example.com"
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "act",
		Kind: kernel.KindRemoteProxy, Active: false, Price: 0,
		Source:       "https://old.example.com/v1/federation/call?action=@owner/act&counterparty=abc",
		InputSchema:  map[string]any{}, OutputSchema: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	if err := db.UpdateRemoteProxySourceURLs(ctx, owner.ID, "https://old.example.com", "https://new.example.com"); err != nil {
		t.Fatalf("UpdateRemoteProxySourceURLs: %v", err)
	}

	updated, err := db.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updated.Source, "old.example.com") {
		t.Errorf("old base URL still in source: %s", updated.Source)
	}
	if !strings.Contains(updated.Source, "new.example.com") {
		t.Errorf("new base URL not in source: %s", updated.Source)
	}
}

func TestReadRemoteKernelByBaseURL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	peer := newUser("@rburl-peer", 0)
	peer.PublicKey = "somepubkey"
	peer.RemoteBaseURL = "https://rburl.example.com"
	if err := db.CreateUser(ctx, peer); err != nil {
		t.Fatal(err)
	}

	found, err := db.ReadRemoteKernelByBaseURL(ctx, "https://rburl.example.com")
	if err != nil {
		t.Fatalf("ReadRemoteKernelByBaseURL: %v", err)
	}
	if found.ID != peer.ID {
		t.Errorf("expected peer ID %s, got %s", peer.ID, found.ID)
	}

	_, err = db.ReadRemoteKernelByBaseURL(ctx, "https://notfound.example.com")
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown base URL, got %v", err)
	}
}

func TestUpdateActionAndResetStats(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@stats-owner", 0)
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	a := newAction(owner.ID, "my-action", 5, true)
	if err := db.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Seed non-zero stats.
	if err := db.UpsertStats(ctx, &kernel.Stats{
		ActionID:  a.ID,
		Uses:      10,
		Successes: 8,
		Failures:  2,
	}); err != nil {
		t.Fatal(err)
	}

	// Deactivate and reset stats atomically.
	a.Active = false
	a.Description = "updated description"
	a.UpdatedAt = a.UpdatedAt.Add(1)
	if err := db.UpdateActionAndResetStats(ctx, a); err != nil {
		t.Fatalf("UpdateActionAndResetStats: %v", err)
	}

	// Action must reflect the update.
	got, err := db.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("ReadAction: %v", err)
	}
	if got.Active {
		t.Error("expected action to be inactive after update")
	}
	if got.Description != "updated description" {
		t.Errorf("unexpected description: %q", got.Description)
	}

	// Stats must be zeroed.
	stats, err := db.ReadStats(ctx, a.ID)
	if err != nil {
		t.Fatalf("ReadStats: %v", err)
	}
	if stats.Uses != 0 || stats.Successes != 0 || stats.Failures != 0 {
		t.Errorf("expected zeroed stats after reset, got uses=%d successes=%d failures=%d",
			stats.Uses, stats.Successes, stats.Failures)
	}
}

// ---- Test-only store helpers ----
// These low-level helpers exist only in test builds to keep test setup simple.
// Production code uses the higher-level atomic methods (CommitCall, CommitFailedCall, etc.).

// createTransaction inserts a transaction record directly. Used only to set up
// data for tests that exercise receipt, rating, and listing logic independent of
// the full call flow.
func (s *DB) createTransaction(ctx context.Context, tx *kernel.Transaction) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO transactions
		 (id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,target_user_id,
		  action_id,args_json,reply_json,status,gross,net,fee,reason,remote_receipt_hash,remote_receipt_json,started_at,ended_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		tx.ID, tx.ProcessID, tx.TraceID, tx.ParentTraceID,
		tx.OwnerUserID, tx.CallerUserID, tx.TargetUserID, tx.ActionID,
		rawJSONStr(tx.ArgsJSON), rawJSONStr(tx.ReplyJSON), string(tx.Status),
		tx.Gross, tx.Net, tx.Fee, tx.Reason, nullStr(tx.RemoteReceiptHash), tx.RemoteReceiptJSON,
		timeToStr(tx.StartedAt), timeToStr(tx.EndedAt),
	)
	return dbErr(err, "create transaction")
}

func startProc(t *testing.T, db *DB, ctx context.Context, p *kernel.Process) {
	t.Helper()
	if err := db.CreateProcess(ctx, p, p.OwnerUserID, 0); err != nil {
		t.Fatalf("startProc: %v", err)
	}
}

func (s *DB) createReceipt(ctx context.Context, r *kernel.Receipt) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO receipts (id,issuer_user_id,tx_id,trace_id,action_id,caller_user_id,process_id,
		                       args_hash,reply_hash,status,gross,net,fee,reason,started_at,created_at,signature)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.IssuerUserID, r.TxID, r.TraceID, r.ActionID,
		r.CallerUserID, r.ProcessID,
		r.ArgsHash, r.ReplyHash, string(r.Status),
		r.Gross, r.Net, r.Fee, r.Reason,
		timeToStr(r.StartedAt), timeToStr(r.CreatedAt), r.Signature,
	)
	return dbErr(err, "create receipt")
}

func (s *DB) createRating(ctx context.Context, r *kernel.Rating) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ratings (id,rated_tx_id,rated_receipt_id,rater_user_id,rating,note,created_at,signature)
		 VALUES (?,?,?,?,?,?,?,?)`,
		r.ID, r.RatedTxID, r.RatedReceiptID, r.RaterUserID, r.Rating, r.Note,
		timeToStr(r.CreatedAt), r.Signature,
	)
	return dbErr(err, "create rating")
}

func (s *DB) completeIdempotencyRecord(ctx context.Context, id, resultJSON, receiptJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE idempotency_records SET status='complete', result_json=?, receipt_json=? WHERE id=?`,
		resultJSON, receiptJSON, id,
	)
	return dbErr(err, "complete idempotency record")
}

func TestDeactivateActionsOwnedBy(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@peer-owner", 0)
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	other := newUser("@other-owner", 0)
	if err := db.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}

	a1 := newAction(owner.ID, "act1", 10, true)
	a2 := newAction(owner.ID, "act2", 5, true)
	a3 := newAction(other.ID, "act3", 3, true)
	for _, a := range []*kernel.Action{a1, a2, a3} {
		if err := db.CreateAction(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.DeactivateActionsOwnedBy(ctx, owner.ID); err != nil {
		t.Fatalf("DeactivateActionsOwnedBy: %v", err)
	}
	r1, _ := db.ReadAction(ctx, a1.ID)
	r2, _ := db.ReadAction(ctx, a2.ID)
	r3, _ := db.ReadAction(ctx, a3.ID)
	if r1.Active || r2.Active {
		t.Error("owner's actions should be inactive after DeactivateActionsOwnedBy")
	}
	if !r3.Active {
		t.Error("other owner's action should remain active")
	}
}

func TestCancelAndRefundStepsForCaller(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Set up user, process, root trace, and an action for the step.
	user := newUser("@step-owner", 500)
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	caller := newUser("@step-caller", 0)
	if err := db.CreateUser(ctx, caller); err != nil {
		t.Fatal(err)
	}

	p := newProcess(user.ID)
	if err := db.CreateProcess(ctx, p, user.ID, 500); err != nil {
		t.Fatal(err)
	}
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRootCall(ctx, p.ID, root, 500); err != nil {
		t.Fatal(err)
	}

	act := newAction(caller.ID, "step-act", 0, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	ptID := root.ID
	step := &kernel.Step{
		ID:                   uuid.New().String(),
		ProcessID:            p.ID,
		ParentTraceID:        &ptID,
		RequiredCallerUserID: caller.ID,
		NextActionID:         act.ID,
		Price:                100,
		Status:               kernel.StepWaiting,
		CreatedAt:            time.Now().UTC(),
	}
	if err := db.CreateStep(ctx, step); err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// Trace should have available=400, locked=100 after step park.
	tr, _ := db.ReadTrace(ctx, root.ID)
	if tr.Available != 400 || tr.Locked != 100 {
		t.Errorf("after CreateStep: trace available=%d locked=%d, want 400/100", tr.Available, tr.Locked)
	}

	// Cancel steps for the caller — should cancel the step and refund price to parent trace.
	if err := db.CancelAndRefundStepsForCaller(ctx, caller.ID); err != nil {
		t.Fatalf("CancelAndRefundStepsForCaller: %v", err)
	}

	// Trace available should be restored to 500.
	tr, _ = db.ReadTrace(ctx, root.ID)
	if tr.Available != 500 || tr.Locked != 0 {
		t.Errorf("after cancel: trace available=%d locked=%d, want 500/0", tr.Available, tr.Locked)
	}

	// Step status should be cancelled.
	s, err := db.ReadStep(ctx, step.ID)
	if err != nil {
		t.Fatalf("ReadStep: %v", err)
	}
	if s.Status != kernel.StepCancelled {
		t.Errorf("step status = %s, want cancelled", s.Status)
	}
}

func TestListStatsByOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@stats-peer", 0)
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}

	a1 := newAction(owner.ID, "used-act", 10, true)
	a2 := newAction(owner.ID, "unused-act", 5, true)
	for _, a := range []*kernel.Action{a1, a2} {
		if err := db.CreateAction(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	// Upsert stats: a1 has uses=3, a2 has uses=0 (default).
	now := time.Now().UTC()
	if err := db.UpsertStats(ctx, &kernel.Stats{
		ActionID:    a1.ID,
		Uses:        3,
		Successes:   3,
		LastUsedAt:  now,
	}); err != nil {
		t.Fatalf("UpsertStats: %v", err)
	}

	results, err := db.ListStatsByOwner(ctx, owner.ID)
	if err != nil {
		t.Fatalf("ListStatsByOwner: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (uses>0 only), got %d", len(results))
	}
	if results[0].ActionID != a1.ID {
		t.Errorf("expected action %s, got %s", a1.ID, results[0].ActionID)
	}
	if results[0].Uses != 3 {
		t.Errorf("expected Uses=3, got %d", results[0].Uses)
	}
}

func TestBeginRunIsAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@beginrun-alice", 200)
	_ = db.CreateUser(ctx, user)
	action := newAction(user.ID, "beginrun-action", 100, true)
	_ = db.CreateAction(ctx, action)

	p := newProcess(user.ID)
	tr := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: user.ID,
		ActionID:      action.ID,
		CallerUserID:  user.ID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := db.BeginRun(ctx, p, tr, user.ID, 100); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}

	u, _ := db.ReadUser(ctx, user.ID)
	if u.Available != 100 {
		t.Errorf("user.available: got %d, want 100", u.Available)
	}
	if u.Locked != 100 {
		t.Errorf("user.locked: got %d, want 100", u.Locked)
	}

	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Available != 0 || proc.Locked != 100 {
		t.Errorf("process: available=%d locked=%d, want 0/100", proc.Available, proc.Locked)
	}

	gotTrace, _ := db.ReadTrace(ctx, tr.ID)
	if gotTrace.Available != 100 {
		t.Errorf("trace.available: got %d, want 100", gotTrace.Available)
	}

	// Insufficient funds: no process or trace should be created.
	p2 := newProcess(user.ID)
	tr2 := &kernel.Trace{
		ID: uuid.New().String(), ProcessID: p2.ID,
		ActionOwnerID: user.ID, ActionID: action.ID, CallerUserID: user.ID,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.BeginRun(ctx, p2, tr2, user.ID, 9999); !errors.Is(err, kernel.ErrInsufficientFunds) {
		t.Fatalf("expected ErrInsufficientFunds, got %v", err)
	}
	if _, readErr := db.ReadProcess(ctx, p2.ID); !errors.Is(readErr, kernel.ErrNotFound) {
		t.Error("process should not exist after failed BeginRun")
	}
}

func TestListOrphanRunningStepsDistinguishesSettled(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@orphan-settled", 1000)
	_ = db.CreateUser(ctx, user)
	act := newAction(user.ID, "orphan-settled-act", 100, true)
	_ = db.CreateAction(ctx, act)

	// mkSetup: create process → root trace → step → completion trace via BeginStepCall.
	mkSetup := func(price int64) (*kernel.Step, *kernel.Trace) {
		p := newProcess(user.ID)
		_ = db.CreateProcess(ctx, p, user.ID, price)
		root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		_ = db.BeginRootCall(ctx, p.ID, root, price)
		ptID := root.ID
		step := &kernel.Step{
			ID: uuid.New().String(), ProcessID: p.ID, ParentTraceID: &ptID,
			RequiredCallerUserID: user.ID, NextActionID: act.ID,
			Price: price, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
		}
		_ = db.CreateStep(ctx, step)
		ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		_ = db.BeginStepCall(ctx, step.ID, ct)
		return step, ct
	}

	// ct1 is an empty completion trace (HasSettled should be false).
	_, ct1 := mkSetup(100)
	// ct2 has a subcall that locked funds (HasSettled should be true).
	_, ct2 := mkSetup(200)
	ctID2 := ct2.ID
	child := &kernel.Trace{ID: uuid.New().String(), ProcessID: ct2.ProcessID, ParentTraceID: &ctID2, CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, ct2.ID, child, 50); err != nil {
		t.Fatalf("BeginSubcall: %v", err)
	}

	rows, err := db.ListOrphanRunningSteps(ctx)
	if err != nil {
		t.Fatalf("ListOrphanRunningSteps: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	byTrace := make(map[string]kernel.OrphanRunningStep)
	for _, r := range rows {
		byTrace[r.CompletionTraceID] = r
	}

	r1, ok1 := byTrace[ct1.ID]
	if !ok1 {
		t.Fatal("missing row for empty completion trace")
	}
	if r1.HasSettled {
		t.Error("empty completion trace: HasSettled should be false")
	}

	r2, ok2 := byTrace[ct2.ID]
	if !ok2 {
		t.Fatal("missing row for completion trace with locked funds")
	}
	if !r2.HasSettled {
		t.Error("completion trace with locked funds: HasSettled should be true")
	}
}

