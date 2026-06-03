package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
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
	if len(files) < 8 {
		t.Fatalf("expected at least 8 migration files, got %d", len(files))
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
		{"events", "consumed_at"},
		{"events", "causing_trace_id"},
		{"actions", "embed_vec"},
		{"action_stats", "rating_count"},
		{"traces", "caused_by_trace_id"},
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

	owner := newUser("@sd-owner", 0)
	caller := newUser("@sd-caller", 0)
	_ = db.CreateUser(ctx, owner)
	_ = db.CreateUser(ctx, caller)

	a := newAction(owner.ID, "/sd-svc", 0, true)
	if err := db.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantACL(ctx, &kernel.ACLEntry{
		SubjectUserID: caller.ID,
		ActionID:      a.ID,
		Permission:    kernel.PermCall,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteAction(ctx, a.ID); err != nil {
		t.Fatalf("DeleteAction: %v", err)
	}

	// Row still exists in the database (soft delete preserves it).
	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM actions WHERE id=?", a.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("action row count after soft delete: got %d, want 1", count)
	}

	// ReadAction returns not found (filtered by deleted_at IS NULL).
	_, err := db.ReadAction(ctx, a.ID)
	if err == nil {
		t.Error("expected error reading soft-deleted action, got nil")
	}

	// ListActions excludes the deleted action.
	all, _ := db.ListActions(ctx, false, 100, 0)
	for _, listed := range all {
		if listed.ID == a.ID {
			t.Error("soft-deleted action should not appear in ListActions")
		}
	}

	// ACL entries are purged.
	ok, _ := db.CheckACL(ctx, caller.ID, a.ID, kernel.PermCall)
	if ok {
		t.Error("ACL entry should be removed after soft delete")
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

	all, err := db.ListActions(ctx, false, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("ListActions(all): got %d, want 3", len(all))
	}

	activeOnly, err := db.ListActions(ctx, true, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(activeOnly) != 1 || activeOnly[0].Name != "/active" {
		t.Errorf("ListActions(active): unexpected result")
	}
}

// ---- ACL ----

func TestACL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@owner", 0)
	caller := newUser("@caller", 0)
	_ = db.CreateUser(ctx, owner)
	_ = db.CreateUser(ctx, caller)

	a := newAction(owner.ID, "/svc", 0, true)
	_ = db.CreateAction(ctx, a)

	ok, err := db.CheckACL(ctx, caller.ID, a.ID, kernel.PermCall)
	if err != nil || ok {
		t.Error("expected no ACL before grant")
	}

	if err := db.GrantACL(ctx, &kernel.ACLEntry{
		SubjectUserID: caller.ID,
		ActionID:      a.ID,
		Permission:    kernel.PermCall,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	ok, err = db.CheckACL(ctx, caller.ID, a.ID, kernel.PermCall)
	if err != nil || !ok {
		t.Error("expected ACL after grant")
	}

	if err := db.RevokeACL(ctx, caller.ID, a.ID, kernel.PermCall); err != nil {
		t.Fatal(err)
	}
	ok, err = db.CheckACL(ctx, caller.ID, a.ID, kernel.PermCall)
	if err != nil || ok {
		t.Error("expected no ACL after revoke")
	}
}

// ---- Fund operations ----

func TestFundProcess(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 1000)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	startProc(t, db, ctx, p)

	if err := db.FundProcess(ctx, user.ID, p.ID, 400); err != nil {
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

	// Overfunding should fail.
	if err := db.FundProcess(ctx, user.ID, p.ID, 9999); err == nil {
		t.Error("expected error for overfunding")
	}
}

func TestFundProcessClosedFails(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@closed-fund", 1000)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	startProc(t, db, ctx, p)
	_ = db.FundProcess(ctx, user.ID, p.ID, 200)

	// Close the process.
	if err := db.EndProcess(ctx, p.ID); err != nil {
		t.Fatal(err)
	}

	// FundProcess on a closed process must fail atomically.
	if err := db.FundProcess(ctx, user.ID, p.ID, 100); err == nil {
		t.Error("expected error funding a closed process")
	}

	// User balance must be unchanged (deduction rolled back; EndProcess already returned the 200).
	u, _ := db.ReadUser(ctx, user.ID)
	if u.Available != 1000 {
		t.Errorf("user.available after failed fund: got %d, want 1000", u.Available)
	}
}

func TestLockAndRefundFunds(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 500)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	startProc(t, db, ctx, p)
	_ = db.FundProcess(ctx, user.ID, p.ID, 500)

	if err := db.LockFunds(ctx, p.ID, 200); err != nil {
		t.Fatal(err)
	}
	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Available != 300 || proc.Locked != 200 {
		t.Errorf("after lock: available=%d locked=%d, want 300/200", proc.Available, proc.Locked)
	}

	// Lock more than available should fail.
	if err := db.LockFunds(ctx, p.ID, 400); err == nil {
		t.Error("expected error locking more than available")
	}

	// Refund.
	if err := db.RefundFunds(ctx, p.ID, 200); err != nil {
		t.Fatal(err)
	}
	proc, _ = db.ReadProcess(ctx, p.ID)
	if proc.Available != 500 || proc.Locked != 0 {
		t.Errorf("after refund: available=%d locked=%d, want 500/0", proc.Available, proc.Locked)
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
	startProc(t, db, ctx, p)
	_ = db.FundProcess(ctx, payer.ID, p.ID, 1000)
	_ = db.LockFunds(ctx, p.ID, 100)

	tr := &kernel.Trace{ID: "tr1", ProcessID: p.ID, ParentTraceID: "tr1", CreatedAt: time.Now().UTC()}
	_ = db.CreateTrace(ctx, tr)

	tx := &kernel.Transaction{
		ID: "tx1", ProcessID: p.ID, TraceID: tr.ID, ParentTraceID: tr.ID,
		OwnerUserID: payer.ID, SubjectUserID: payer.ID, TargetUserID: target.ID,
		ActionID: "a1", Status: kernel.TxSuccess, Gross: 100, Net: 80, Fee: 20,
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: "rc1", IssuerUserID: payer.ID, TxID: tx.ID, TraceID: tr.ID, ActionID: "a1",
		ArgsHash: "ah1", ReplyHash: "rh1", Status: kernel.TxSuccess,
		Gross: 100, Net: 80, Fee: 20, CreatedAt: time.Now().UTC(),
	}
	if err := db.CommitCall(ctx, tx, receipt, p.ID, target.ID, fee.ID, 80, 20, nil, "", ""); err != nil {
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
	startProc(t, db, ctx, p)
	_ = db.FundProcess(ctx, user.ID, p.ID, 600)

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

func TestEndProcessWithLockedFunds(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice-locked", 500)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	startProc(t, db, ctx, p)
	_ = db.FundProcess(ctx, user.ID, p.ID, 500)
	// Lock 200 — simulates an in-flight sub-call.
	if err := db.LockFunds(ctx, p.ID, 200); err != nil {
		t.Fatal(err)
	}

	err := db.EndProcess(ctx, p.ID)
	if err == nil {
		t.Fatal("expected error ending process with locked funds, got nil")
	}
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState, got %v", err)
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
		PriceMean:  50,
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

	p := newProcess(payer.ID)
	startProc(t, db, ctx, p)
	_ = db.FundProcess(ctx, payer.ID, p.ID, 1000)
	_ = db.LockFunds(ctx, p.ID, 200) // lock for both calls

	makeTrace := func(id string) *kernel.Trace {
		tr := &kernel.Trace{ID: id, ProcessID: p.ID, ParentTraceID: id, CreatedAt: time.Now().UTC()}
		_ = db.CreateTrace(ctx, tr)
		return tr
	}
	makeTx := func(id, traceID string, gross int64) *kernel.Transaction {
		return &kernel.Transaction{
			ID: id, ProcessID: p.ID, TraceID: traceID, ParentTraceID: traceID,
			OwnerUserID: payer.ID, SubjectUserID: payer.ID, TargetUserID: target.ID,
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

	tr1 := makeTrace("tr-inc-1")
	tx1 := makeTx("tx-inc-1", tr1.ID, 100)
	rc1 := makeReceipt("rc-inc-1", tx1.ID, tr1.ID, 100)
	stats1 := &kernel.Stats{ActionID: a.ID, Uses: 1, Successes: 1, PriceMean: 100, LatencyMean: 0.1, LastUsedAt: time.Now().UTC()}
	if err := db.CommitCall(ctx, tx1, rc1, p.ID, target.ID, fee.ID, tx1.Net, tx1.Fee, stats1, "", ""); err != nil {
		t.Fatalf("CommitCall #1: %v", err)
	}

	tr2 := makeTrace("tr-inc-2")
	tx2 := makeTx("tx-inc-2", tr2.ID, 50) // different gross to verify mean formula
	rc2 := makeReceipt("rc-inc-2", tx2.ID, tr2.ID, 50)
	stats2 := &kernel.Stats{ActionID: a.ID, Uses: 1, Successes: 1, PriceMean: 50, LatencyMean: 0.3, LastUsedAt: time.Now().UTC()}
	if err := db.CommitCall(ctx, tx2, rc2, p.ID, target.ID, fee.ID, tx2.Net, tx2.Fee, stats2, "", ""); err != nil {
		t.Fatalf("CommitCall #2: %v", err)
	}

	got, err := db.ReadStats(ctx, a.ID)
	if err != nil {
		t.Fatalf("ReadStats: %v", err)
	}
	if got.Uses != 2 || got.Successes != 2 {
		t.Errorf("uses=%d successes=%d, want 2/2", got.Uses, got.Successes)
	}
	// Mean of [100, 50] = 75
	if math.Abs(got.PriceMean-75) > 1e-6 {
		t.Errorf("price_mean: got %f, want 75", got.PriceMean)
	}
	// Mean of [0.1, 0.3] = 0.2
	if math.Abs(got.LatencyMean-0.2) > 1e-6 {
		t.Errorf("latency_mean: got %f, want 0.2", got.LatencyMean)
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

	user := newUser("@alice", 0)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	startProc(t, db, ctx, p)

	root, err := db.ReadRootTrace(ctx, p.ID)
	if err != nil {
		t.Fatalf("ReadRootTrace: %v", err)
	}

	child := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		CreatedAt:     time.Now().UTC(),
	}
	_ = db.CreateTrace(ctx, child)

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

	owner := newUser("@owner", 0)
	target := newUser("@target", 0)
	_ = db.CreateUser(ctx, owner)
	_ = db.CreateUser(ctx, target)

	a := newAction(owner.ID, "/svc", 0, true)
	_ = db.CreateAction(ctx, a)

	p := newProcess(owner.ID)
	startProc(t, db, ctx, p)

	tr := &kernel.Trace{
		ID:        uuid.New().String(),
		ProcessID: p.ID,
		CreatedAt: time.Now().UTC(),
	}
	tr.ParentTraceID = tr.ID
	_ = db.CreateTrace(ctx, tr)

	tx := &kernel.Transaction{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		TraceID:       tr.ID,
		ParentTraceID: tr.ID,
		OwnerUserID:   owner.ID,
		SubjectUserID: owner.ID,
		TargetUserID:  target.ID,
		ActionID:      a.ID,
		Status:        kernel.TxSuccess,
		Gross:         100,
		Net:           80,
		Fee:           20,
		StartedAt:     time.Now().UTC(),
		EndedAt:       time.Now().UTC(),
	}
	if err := db.createTransaction(ctx, tx); err != nil {
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
		tr := &kernel.Trace{
			ID:            uuid.New().String(),
			ProcessID:     p.ID,
			ParentTraceID: p.ID,
			CreatedAt:     time.Now().UTC(),
		}
		_ = i
		if err := db.StartProcess(ctx, p, tr, ownerID, 0); err != nil {
			t.Fatalf("StartProcess %d: %v", i, err)
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

func TestListListenersByOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@ll-owner", 0)
	other := newUser("@ll-other", 0)
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}

	act := newAction(owner.ID, "/ll-action", 0, false)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}

	makeListener := func(ownerID, sourceID string) *kernel.Listener {
		return &kernel.Listener{
			ID:             uuid.New().String(),
			OwnerUserID:    ownerID,
			SourceUserID:   sourceID,
			EventName:      "test.event",
			TargetActionID: act.ID,
			Active:         true,
			CreatedAt:      time.Now().UTC(),
		}
	}

	if err := db.CreateListener(ctx, makeListener(owner.ID, other.ID)); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateListener(ctx, makeListener(owner.ID, other.ID)); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateListener(ctx, makeListener(other.ID, owner.ID)); err != nil {
		t.Fatal(err)
	}

	got, err := db.ListListenersByOwner(ctx, owner.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("ListListenersByOwner: got %d, want 2", len(got))
	}
	for _, l := range got {
		if l.OwnerUserID != owner.ID {
			t.Errorf("unexpected owner %s", l.OwnerUserID)
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
		SubjectUserID: issuer.ID,
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
		SubjectUserID: rater.ID,
		TargetUserID:  rater.ID,
		ActionID:      uuid.New().String(),
		Status:        kernel.TxSuccess,
		StartedAt:     time.Now().UTC(),
		EndedAt:       time.Now().UTC(),
	}
	_ = db.createTransaction(ctx, tx)

	rating := &kernel.Rating{
		ID:          uuid.New().String(),
		RatedTxID:   tx.ID,
		RaterUserID: rater.ID,
		Rating:      1.0,
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
		SubjectUserID: rater.ID,
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
	if stats.RatingMean != 1.0 {
		t.Errorf("RatingMean: got %f, want 1.0", stats.RatingMean)
	}

	// Second rating (0) must update the running mean atomically.
	rater2 := newUser("@rater2", 0)
	_ = db.CreateUser(ctx, rater2)
	tx2 := &kernel.Transaction{
		ID:            uuid.New().String(),
		OwnerUserID:   rater2.ID,
		SubjectUserID: rater2.ID,
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
	if stats2.RatingMean != 0.5 {
		t.Errorf("RatingMean after second: got %f, want 0.5", stats2.RatingMean)
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
			SubjectUserID: rater.ID,
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

func TestListReceiptsByAction(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@rcpt-list-owner", 0)
	caller := newUser("@rcpt-list-caller", 0)
	_ = db.CreateUser(ctx, owner)
	_ = db.CreateUser(ctx, caller)
	action := newAction(owner.ID, "/list-rcpt", 0, true)
	_ = db.CreateAction(ctx, action)

	p := newProcess(caller.ID)
	startProc(t, db, ctx, p)

	makeReceipt := func(id string, offset time.Duration) {
		tx := &kernel.Transaction{
			ID:            id,
			ProcessID:     p.ID,
			TraceID:       id + "-tr",
			ParentTraceID: id + "-tr",
			OwnerUserID:   caller.ID,
			SubjectUserID: caller.ID,
			TargetUserID:  owner.ID,
			ActionID:      action.ID,
			Status:        kernel.TxSuccess,
			StartedAt:     time.Now().UTC().Add(offset),
			EndedAt:       time.Now().UTC().Add(offset),
		}
		_ = db.createTransaction(ctx, tx)
		r := &kernel.Receipt{
			ID:           "rc-" + id,
			IssuerUserID: owner.ID,
			TxID:         id,
			TraceID:      id + "-tr",
			ActionID:     action.ID,
			CallerUserID: caller.ID,
			ProcessID:    p.ID,
			ArgsHash:     "ah",
			ReplyHash:    "rh",
			Status:       kernel.TxSuccess,
			StartedAt:    tx.StartedAt,
			CreatedAt:    time.Now().UTC().Add(offset),
		}
		_ = db.createReceipt(ctx, r)
	}
	makeReceipt("rcpt-tx-1", 0)
	makeReceipt("rcpt-tx-2", time.Second)

	// Returns all receipts ordered by started_at DESC.
	all, err := db.ListReceiptsByAction(ctx, action.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListReceiptsByAction: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 receipts, got %d", len(all))
	}
	if all[0].TxID != "rcpt-tx-2" {
		t.Errorf("first result should be newest (rcpt-tx-2), got %s", all[0].TxID)
	}
	if all[0].CallerUserID != caller.ID {
		t.Errorf("CallerUserID: got %q, want %q", all[0].CallerUserID, caller.ID)
	}
	if all[0].ProcessID != p.ID {
		t.Errorf("ProcessID: got %q, want %q", all[0].ProcessID, p.ID)
	}

	// Offset skips the first.
	page2, _ := db.ListReceiptsByAction(ctx, action.ID, 10, 1)
	if len(page2) != 1 || page2[0].TxID != "rcpt-tx-1" {
		t.Errorf("offset=1 should return rcpt-tx-1, got %v", page2)
	}

	// Wrong action ID returns empty.
	none, _ := db.ListReceiptsByAction(ctx, "no-such-action", 10, 0)
	if len(none) != 0 {
		t.Errorf("expected no receipts for unknown action, got %d", len(none))
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
	startProc(t, db, ctx, p)
	_ = db.FundProcess(ctx, payer.ID, p.ID, 1000)
	_ = db.LockFunds(ctx, p.ID, 100)

	tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ParentTraceID: "root", CreatedAt: time.Now().UTC()}
	_ = db.CreateTrace(ctx, tr)

	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: tr.ID, ParentTraceID: tr.ID,
		OwnerUserID: payer.ID, SubjectUserID: payer.ID, TargetUserID: target.ID,
		ActionID: "a1", Status: kernel.TxSuccess, Gross: 100, Net: 80, Fee: 20,
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: payer.ID, TxID: tx.ID, TraceID: tr.ID,
		ActionID: "a1", ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxSuccess,
		Gross: 100, Net: 80, Fee: 20, CreatedAt: time.Now().UTC(),
	}

	// fee=20 with empty feeRecipientID must be rejected.
	err := db.CommitCall(ctx, tx, receipt, p.ID, target.ID, "", 80, 20, nil, "", "")
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
	startProc(t, db, ctx, p)
	_ = db.FundProcess(ctx, payer.ID, p.ID, 1000)
	_ = db.LockFunds(ctx, p.ID, 100)

	tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ParentTraceID: "root", CreatedAt: time.Now().UTC()}
	_ = db.CreateTrace(ctx, tr)

	rec := newIdempotencyRecord(cp.ID)
	if err := db.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatalf("InsertPendingIdempotencyRecord: %v", err)
	}

	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: tr.ID, ParentTraceID: tr.ID,
		OwnerUserID: payer.ID, SubjectUserID: payer.ID, TargetUserID: target.ID,
		ActionID: "a1", Status: kernel.TxSuccess, Gross: 100, Net: 100, Fee: 0,
		ReplyJSON: json.RawMessage(`{"ok":true}`),
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: payer.ID, TxID: tx.ID, TraceID: tr.ID,
		ActionID: "a1", ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxSuccess,
		Gross: 100, Net: 100, Fee: 0, CreatedAt: time.Now().UTC(),
	}

	if err := db.CommitCall(ctx, tx, receipt, p.ID, target.ID, "", 100, 0, nil, "", rec.ID); err != nil {
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
	startProc(t, db, ctx, p)
	_ = db.FundProcess(ctx, payer.ID, p.ID, 1000)
	_ = db.LockFunds(ctx, p.ID, 100)

	tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ParentTraceID: "root", CreatedAt: time.Now().UTC()}
	_ = db.CreateTrace(ctx, tr)

	rec := newIdempotencyRecord(cp.ID)
	if err := db.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatalf("InsertPendingIdempotencyRecord: %v", err)
	}

	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: tr.ID, ParentTraceID: tr.ID,
		OwnerUserID: payer.ID, SubjectUserID: payer.ID, TargetUserID: payer.ID,
		ActionID: "a1", Status: kernel.TxFailure, Gross: 0, Net: 0, Fee: 0,
		Reason:    "execution failed",
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: payer.ID, TxID: tx.ID, TraceID: tr.ID,
		ActionID: "a1", ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxFailure,
		Gross: 0, Net: 0, Fee: 0, CreatedAt: time.Now().UTC(),
	}

	if err := db.CommitFailedCall(ctx, tx, receipt, p.ID, 100, nil, rec.ID, "execution_failed"); err != nil {
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

// ---- Test-only store helpers ----
// These low-level helpers exist only in test builds to keep test setup simple.
// Production code uses the higher-level atomic methods (CommitCall, CommitFailedCall, etc.).

func startProc(t *testing.T, db *DB, ctx context.Context, p *kernel.Process) {
	t.Helper()
	trID := uuid.New().String()
	tr := &kernel.Trace{ID: trID, ProcessID: p.ID, ParentTraceID: trID, CreatedAt: time.Now().UTC()}
	if err := db.StartProcess(ctx, p, tr, p.OwnerUserID, 0); err != nil {
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
		`INSERT INTO ratings (id,rated_tx_id,rated_receipt_id,rater_user_id,rating,created_at,signature)
		 VALUES (?,?,?,?,?,?,?)`,
		r.ID, r.RatedTxID, r.RatedReceiptID, r.RaterUserID, r.Rating,
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
