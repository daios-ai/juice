package store

import (
	"context"
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
	_ = db.CreateProcess(ctx, p)

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

func TestLockAndRefundFunds(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 500)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	_ = db.CreateProcess(ctx, p)
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
	_ = db.CreateProcess(ctx, p)
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
	if err := db.CommitCall(ctx, tx, receipt, p.ID, target.ID, fee.ID, 80, 20, nil); err != nil {
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
	_ = db.CreateProcess(ctx, p)
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

// ---- ListTraces ----

func TestListTraces(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 0)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	_ = db.CreateProcess(ctx, p)

	root := &kernel.Trace{
		ID:        uuid.New().String(),
		ProcessID: p.ID,
		CreatedAt: time.Now().UTC(),
	}
	root.ParentTraceID = root.ID
	_ = db.CreateTrace(ctx, root)

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
	_ = db.CreateProcess(ctx, p)

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
	if err := db.CreateTransaction(ctx, tx); err != nil {
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
	_ = db.CreateTransaction(ctx, tx)

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
	if err := db.CreateReceipt(ctx, r); err != nil {
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

func TestCreateRating(t *testing.T) {
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
	_ = db.CreateTransaction(ctx, tx)

	rating := &kernel.Rating{
		ID:          uuid.New().String(),
		RatedTxID:   tx.ID,
		RaterUserID: rater.ID,
		Rating:      1.0,
		CreatedAt:   time.Now().UTC(),
		Signature:   "",
	}
	if err := db.CreateRating(ctx, rating); err != nil {
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
	if err := db.CreateRating(ctx, dup); err == nil {
		t.Error("expected error for duplicate rating")
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

func TestCreateReadIdempotencyRecord(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	counterparty := newUser("@cp", 0)
	_ = db.CreateUser(ctx, counterparty)

	now := time.Now().UTC()
	r := &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     "key-abc-123",
		CounterpartyUserID: counterparty.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	if err := db.CreateIdempotencyRecord(ctx, r); err != nil {
		t.Fatalf("CreateIdempotencyRecord: %v", err)
	}

	got, err := db.ReadIdempotencyRecord(ctx, "key-abc-123", counterparty.ID)
	if err != nil {
		t.Fatalf("ReadIdempotencyRecord: %v", err)
	}
	if got.ID != r.ID {
		t.Errorf("record.ID: got %q, want %q", got.ID, r.ID)
	}

	// Unknown key returns ErrNotFound.
	if _, err := db.ReadIdempotencyRecord(ctx, "no-such-key", counterparty.ID); err == nil {
		t.Error("expected error for unknown idempotency key")
	}

	// Second INSERT with same key+counterparty is silently ignored (INSERT OR IGNORE).
	dup := &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     "key-abc-123",
		CounterpartyUserID: counterparty.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	if err := db.CreateIdempotencyRecord(ctx, dup); err != nil {
		t.Fatalf("duplicate idempotency insert should not error: %v", err)
	}
	// Confirm original record is still returned (not the duplicate ID).
	got2, _ := db.ReadIdempotencyRecord(ctx, "key-abc-123", counterparty.ID)
	if got2.ID != r.ID {
		t.Errorf("expected original ID after duplicate insert, got %q", got2.ID)
	}
}
