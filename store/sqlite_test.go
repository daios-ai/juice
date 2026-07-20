package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"path"
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
		{"ledger", "from_user_id"},
		{"ledger", "to_user_id"},
		{"ledger", "external_key"},
	} {
		if !db.columnExists(tc.table, tc.column) {
			t.Fatalf("expected %s.%s to exist after migrations", tc.table, tc.column)
		}
	}
	// The from/to ledger replaced the per-direction tables and then the adjustments table.
	if db.columnExists("deposits", "id") || db.columnExists("withdrawals", "id") || db.columnExists("adjustments", "id") {
		t.Fatal("deposits/withdrawals/adjustments tables should be dropped after the ledger migration")
	}
}

// columnExists reports whether table has a column with the given name (test-only schema check).
func (s *DB) columnExists(table, column string) bool {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt any
		_ = rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk)
		if name == column {
			return true
		}
	}
	return false
}

func newUser(handle string, balance int64) *kernel.User {
	return &kernel.User{
		ID:           uuid.New().String(),
		Handle:       handle,
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

// newPeer builds and inserts a proxy/peer user (public_key set) with explicit balances and
// creation time, for §13 retention-purge tests.
func newPeer(t *testing.T, db *DB, handle, key string, available, locked int64, createdAt time.Time) *kernel.User {
	t.Helper()
	u := newUser(handle, available)
	u.Locked = locked
	u.PublicKey = key
	u.CreatedAt = createdAt
	u.UpdatedAt = createdAt
	if err := db.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("create peer %s: %v", handle, err)
	}
	return u
}

// insertTx inserts a bare transaction row naming the given users, for ledger/activity assertions.
// Transactions carry no foreign key on their user/process/trace columns, so arbitrary ids are fine.
func insertTx(t *testing.T, db *DB, ownerID, callerID, targetID, actionID string, endedAt time.Time) string {
	t.Helper()
	id := uuid.New().String()
	_, err := db.db.ExecContext(context.Background(),
		`INSERT INTO transactions
		   (id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,target_user_id,
		    action_id,status,gross,net,fee,started_at,ended_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "p-"+id, "t-"+id, "", ownerID, callerID, targetID, actionID,
		"success", 0, 0, 0, timeToStr(endedAt), timeToStr(endedAt))
	if err != nil {
		t.Fatalf("insert tx: %v", err)
	}
	return id
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

	// ReadUserByHandle canonicalizes its argument, so both "@alice" and "alice" resolve.
	for _, h := range []string{"@alice", "alice"} {
		got2, err := db.ReadUserByHandle(ctx, h)
		if err != nil {
			t.Fatalf("ReadUserByHandle(%q): %v", h, err)
		}
		if got2.ID != u.ID {
			t.Errorf("id via handle %q: got %q, want %q", h, got2.ID, u.ID)
		}
	}

	_, err = db.ReadUser(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for missing user")
	}

	// RenameUser moves the handle: the old one frees, the new one resolves.
	if err := db.RenameUser(ctx, u.ID, "@alice2"); err != nil {
		t.Fatalf("RenameUser: %v", err)
	}
	if got, err := db.ReadUserByHandle(ctx, "@alice2"); err != nil || got.ID != u.ID {
		t.Errorf("renamed handle does not resolve: %v", err)
	}
	if _, err := db.ReadUserByHandle(ctx, "@alice"); err == nil {
		t.Error("old handle should be free after rename")
	}
	// The UNIQUE constraint is the backstop against a colliding rename.
	other := newUser("@carol", 0)
	if err := db.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := db.RenameUser(ctx, other.ID, "@alice2"); err == nil {
		t.Error("rename onto a taken handle should fail on the UNIQUE constraint")
	}
}

// TestUsersTableConstraints proves the users table (rebuilt through migrations 021 and 022, which
// dropped email and denied_at) still enforces handle uniqueness and anchors child foreign keys, and
// no longer carries a denied_at column.
func TestUsersTableConstraints(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Migration 022 dropped denied_at: the column must be gone.
	if _, err := db.db.ExecContext(ctx, `SELECT denied_at FROM users LIMIT 0`); err == nil {
		t.Error("denied_at column should no longer exist after migration 022")
	}

	a := newUser("@alice", 0)
	if err := db.CreateUser(ctx, a); err != nil {
		t.Fatalf("create @alice: %v", err)
	}

	// The rebuild kept handle uniqueness.
	if err := db.CreateUser(ctx, newUser("@alice", 0)); err == nil {
		t.Error("duplicate handle should still fail (handle UNIQUE preserved)")
	}

	// The rebuilt users table still anchors child foreign keys: an action owned by @alice, then
	// PRAGMA foreign_key_check must report no violations.
	if err := db.CreateAction(ctx, newAction(a.ID, "act", 0, true)); err != nil {
		t.Fatalf("create action: %v", err)
	}
	rows, err := db.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("foreign_key_check reported a violation after the users rebuild")
	}
}

// TestSuspendStampRealTime guards the write/read timestamp round-trip: SuspendUser must store a
// real, recent time — not the zero value that a datetime('now')/RFC3339Nano format mismatch used to
// produce (displayed "00000").
func TestSuspendStampRealTime(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := newUser("@stamp", 0)
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-2 * time.Second)

	if err := db.SuspendUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SuspendedAt == nil || got.SuspendedAt.IsZero() || got.SuspendedAt.Before(before) {
		t.Fatalf("suspended_at should be a real, recent time, got %v", got.SuspendedAt)
	}
}

// TestStrToTimeAcceptsLegacyLayout guards the read-side heal: values already stored by the old
// SQLite datetime('now') path ("YYYY-MM-DD HH:MM:SS", UTC) must still parse, not zero out.
func TestStrToTimeAcceptsLegacyLayout(t *testing.T) {
	got := strToTime("2026-07-13 12:34:56")
	want := time.Date(2026, 7, 13, 12, 34, 56, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("legacy SQLite-layout timestamp parsed to %v, want %v", got, want)
	}
	// The primary RFC3339Nano path still parses.
	if strToTime("2026-07-13T12:34:56.5Z").IsZero() {
		t.Error("RFC3339Nano timestamp should parse on the primary path")
	}
}

// TestMigration021DropsEmailPreservesRows exercises the real upgrade path: it applies every
// migration strictly before 021, seeds a user (with the then-required email column, via raw SQL)
// plus a child action under the old schema, then applies 021 and asserts the pre-existing rows
// survive the users-table rebuild with their FK graph intact — the one thing a fresh-DB test cannot
// cover — while email is dropped and description defaults to "".
func TestMigration021DropsEmailPreservesRows(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "up.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	raw, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1) // mirror Open(): the FK-off pragma and the rebuild tx share one conn
	defer raw.Close()
	s := &DB{db: raw}
	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	files, err := migrationFileNames()
	if err != nil {
		t.Fatal(err)
	}
	apply := func(file string) {
		version := strings.TrimSuffix(path.Base(file), ".sql")
		b, err := migrationFS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.applyMigration(version, string(b)); err != nil {
			t.Fatalf("apply %s: %v", version, err)
		}
	}
	var held []string // 021 and everything after it: applied after seeding under the old schema
	for _, f := range files {
		if path.Base(f) >= "021_" {
			held = append(held, f)
			continue
		}
		apply(f)
	}
	if len(held) == 0 || !strings.HasPrefix(path.Base(held[0]), "021_") {
		t.Fatal("migration 021 not found")
	}

	// Seed under the pre-021 schema, which still has the email column (NOT NULL). CreateUser no
	// longer writes email, so seed via raw SQL to match the old column set.
	ctx := context.Background()
	uid := uuid.New().String()
	now := timeToStr(time.Now().UTC())
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO users (id,handle,email,password_hash,available,locked,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		uid, "@old", "old@example.com", "hash", int64(42), int64(0), now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := s.CreateAction(ctx, newAction(uid, "act", 0, true)); err != nil {
		t.Fatalf("seed action: %v", err)
	}

	// Apply 021 (rebuild dropping email, adding description + recovery_public_key) and everything
	// after it (022 drops denied_at), in order.
	for _, f := range held {
		apply(f)
	}

	// The pre-existing user survived the rebuild with its data; description defaults to "".
	got, err := s.ReadUser(ctx, uid)
	if err != nil || got.Handle != "@old" || got.Available != 42 {
		t.Fatalf("user lost or altered by rebuild: err=%v got=%+v", err, got)
	}
	if got.Description != "" || got.RecoveryPublicKey != "" {
		t.Errorf("new columns should default empty, got description=%q recovery=%q", got.Description, got.RecoveryPublicKey)
	}
	// The child action's FK still resolves — no dangling references after DROP/RENAME.
	fkRows, err := raw.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer fkRows.Close()
	if fkRows.Next() {
		t.Error("foreign_key_check reported a violation after the upgrade")
	}
	// The email column is gone: selecting it must error.
	if _, err := raw.QueryContext(ctx, `SELECT email FROM users`); err == nil {
		t.Error("email column should not exist after migration 021")
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
	active.Visibility = kernel.VisibilityPublic
	inactive := newAction(owner.ID, "/inactive", 0, false)
	inactive.Visibility = kernel.VisibilityPublic
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

	publicActive, err := db.ListVisibleActions(ctx, false, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(publicActive) != 1 || publicActive[0].Name != "/active" {
		t.Errorf("ListPublicActions: unexpected result")
	}
}

// TestListPublicActionsExcludesSuspendedOwner proves the suspended-owner JOIN both hides the
// action from the public listing and surfaces OwnerSuspended on a direct read (§12).
func TestListPublicActionsExcludesSuspendedOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@owner", 0)
	_ = db.CreateUser(ctx, owner)
	a := newAction(owner.ID, "/svc", 0, true)
	a.Visibility = kernel.VisibilityPublic
	_ = db.CreateAction(ctx, a)

	before, err := db.ListVisibleActions(ctx, false, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].OwnerSuspended {
		t.Fatalf("active owner: want 1 action with OwnerSuspended=false, got %d", len(before))
	}

	if err := db.SuspendUser(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	after, err := db.ListVisibleActions(ctx, false, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("suspended owner: want 0 public actions, got %d", len(after))
	}

	got, err := db.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.OwnerSuspended {
		t.Error("ReadAction: OwnerSuspended should be true for suspended owner")
	}
}

// ---- Fund operations ----

func TestBeginRunDeductsFunds(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 1000)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}

	if err := db.BeginRun(ctx, p, tr, user.ID, 400); err != nil {
		t.Fatal(err)
	}

	// Process holds available=0 (funds are in root trace); trace holds available=400.
	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Available != 0 || proc.Locked != 400 {
		t.Errorf("process after BeginRun: available=%d locked=%d, want 0/400", proc.Available, proc.Locked)
	}
	root, _ := db.ReadTrace(ctx, tr.ID)
	if root.Available != 400 {
		t.Errorf("trace.available after BeginRun: got %d, want 400", root.Available)
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
	tr2 := &kernel.Trace{ID: uuid.New().String(), ProcessID: p2.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p2, tr2, user.ID, 9999); err == nil {
		t.Error("expected error for insufficient funds")
	}
}

func TestBeginRunAndSubcall(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice", 500)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}

	// BeginRun atomically creates process+root trace and debits user.
	if err := db.BeginRun(ctx, p, root, user.ID, 500); err != nil {
		t.Fatal(err)
	}
	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Available != 0 || proc.Locked != 500 {
		t.Errorf("after BeginRun: process available=%d locked=%d, want 0/500", proc.Available, proc.Locked)
	}
	rootRead, _ := db.ReadTrace(ctx, root.ID)
	if rootRead.Available != 500 {
		t.Errorf("after BeginRun: trace.available=%d, want 500", rootRead.Available)
	}

	// BeginSubcall locks price from root.available into root.locked,
	// and creates the child trace with available=price.
	child := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ParentTraceID: nullStr(root.ID), CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, root.ID, child, 200); err != nil {
		t.Fatal(err)
	}
	rootRead, _ = db.ReadTrace(ctx, root.ID)
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
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, payer.ID, 100); err != nil {
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
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 600); err != nil {
		t.Fatal(err)
	}

	// Settle the root trace as failure to route funds back to process.available.
	// This mirrors the production path: the kernel settles traces before calling EndProcess.
	failTx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID,
		OwnerUserID: user.ID, CallerUserID: user.ID, TargetUserID: user.ID,
		ActionID: "dummy", Status: kernel.TxFailure, Gross: 600, Reason: "test",
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	buildReceipt := func(refund int64) (*kernel.Receipt, error) {
		return &kernel.Receipt{
			ID: uuid.New().String(), IssuerUserID: user.ID, TxID: failTx.ID, TraceID: root.ID,
			ActionID: "dummy", Status: kernel.TxFailure, Gross: 600, Charge: 600 - refund,
			CreatedAt: time.Now().UTC(),
		}, nil
	}
	// CallerProcess: root call's caller wallet is the process itself.
	if err := db.CommitFailedCall(ctx, failTx, buildReceipt, root.ID, p.ID, kernel.CallerProcess, 600, nil, "", "failure", ""); err != nil {
		t.Fatal(err)
	}

	// CommitFailedCall + closeProcessTx should have closed the process automatically (quiescent).
	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Status != kernel.ProcessClosed {
		t.Errorf("process status: got %q, want closed", proc.Status)
	}
	if proc.Available != 0 || proc.Locked != 0 {
		t.Errorf("process funds after end: available=%d locked=%d, want 0/0", proc.Available, proc.Locked)
	}

	// Funds returned to owner.
	u, _ := db.ReadUser(ctx, user.ID)
	if u.Available != 1000 {
		t.Errorf("user available after end: got %d, want 1000", u.Available)
	}
	if u.Locked != 0 {
		t.Errorf("user locked after end: got %d, want 0", u.Locked)
	}
}

func TestEndProcessWithLockedFundsForceCloseSucceeds(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@alice-locked", 500)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	// BeginRun creates process (locked=500) + root trace (available=500).
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 500); err != nil {
		t.Fatal(err)
	}

	// store.EndProcess is called by the kernel after settling traces; here we test it directly
	// on a process that still has an in-flight root trace (locked > 0). It must not error.
	err := db.EndProcess(ctx, p.ID)
	if err != nil {
		t.Fatalf("EndProcess should succeed even with locked funds; got: %v", err)
	}
}

// TestEndProcessCancelsWaitingStep verifies the store.EndProcess primitive: it cancels
// waiting steps, returns their parked prices to the owner, and closes the process.
// Running step-completion traces are settled as failed calls by kernel.EndProcess before
// this primitive runs (see kernel TestEndProcessFailsRunningStep), so this method only
// handles waiting steps and remaining available.
func TestEndProcessCancelsWaitingStep(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@ep-running", 1000)
	_ = db.CreateUser(ctx, user)
	caller := newUser("@ep-running-caller", 0)
	_ = db.CreateUser(ctx, caller)

	p := newProcess(user.ID)
	// BeginRun: user.locked=50, root trace.available=50.
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 50); err != nil {
		t.Fatal(err)
	}

	act := newAction(user.ID, "ep-act", 50, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}

	// CreateStep parks the price from the root trace; the step stays waiting.
	ptID := root.ID
	step := &kernel.Step{
		ID:                   uuid.New().String(),
		ParentTraceID:        &ptID,
		RequiredCallerUserID: caller.ID,
		ActionID:             act.ID,
		Price:                50,
		Status:               kernel.StepWaiting,
		CreatedAt:            time.Now().UTC(),
	}
	if err := db.CreateStep(ctx, step); err != nil {
		t.Fatal(err)
	}

	if err := db.EndProcess(ctx, p.ID); err != nil {
		t.Fatalf("EndProcess: %v", err)
	}

	// Waiting step must be cancelled (parked price returned).
	s, _ := db.ReadStep(ctx, step.ID)
	if s.Status != kernel.StepCancelled {
		t.Errorf("step.status=%s, want cancelled", s.Status)
	}

	// Process must be closed with no funds.
	proc, _ := db.ReadProcess(ctx, p.ID)
	if proc.Status != kernel.ProcessClosed {
		t.Errorf("process.status=%s, want closed", proc.Status)
	}
	if proc.Available != 0 || proc.Locked != 0 {
		t.Errorf("process funds after close: available=%d locked=%d, want 0/0", proc.Available, proc.Locked)
	}

	// User must be fully restored.
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
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 50); err != nil {
		t.Fatal(err)
	}

	act := newAction(user.ID, "ep-done-act", 50, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}

	ptID := root.ID
	step := &kernel.Step{
		ID:                   uuid.New().String(),
		ParentTraceID:        &ptID,
		RequiredCallerUserID: caller.ID,
		ActionID:             act.ID,
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

// TestBeginStepCallGuardsParkInvariant verifies BeginStepCall returns a typed ErrInvalidState
// (not a raw CHECK constraint failure) if the parent trace's locked is below the step price —
// i.e. the park invariant is broken. Mirrors BeginSubcall's guarded-update pattern.
func TestBeginStepCallGuardsParkInvariant(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@bsc-guard", 1000)
	_ = db.CreateUser(ctx, user)
	caller := newUser("@bsc-guard-caller", 0)
	_ = db.CreateUser(ctx, caller)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 50); err != nil {
		t.Fatal(err)
	}
	act := newAction(user.ID, "bsc-guard-act", 50, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	ptID := root.ID
	step := &kernel.Step{
		ID: uuid.New().String(), ParentTraceID: &ptID, RequiredCallerUserID: caller.ID,
		ActionID: act.ID, Price: 50, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateStep(ctx, step); err != nil {
		t.Fatal(err)
	}

	// Corrupt the park: drop the parent trace's locked below the step price.
	if _, err := db.db.ExecContext(ctx, `UPDATE traces SET locked=0 WHERE id=?`, root.ID); err != nil {
		t.Fatalf("corrupt locked: %v", err)
	}

	ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	err := db.BeginStepCall(ctx, step.ID, ct)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("BeginStepCall with broken park: got %v, want ErrInvalidState", err)
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
	makeRun := func(price int64) (*kernel.Process, *kernel.Trace) {
		pr := newProcess(payer.ID)
		tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: pr.ID, CreatedAt: time.Now().UTC()}
		if err := db.BeginRun(ctx, pr, tr, payer.ID, price); err != nil {
			t.Fatalf("BeginRun: %v", err)
		}
		return pr, tr
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

	p1, tr1 := makeRun(100)
	tx1 := makeTx(uuid.New().String(), p1.ID, tr1.ID, 100)
	rc1 := makeReceipt(uuid.New().String(), tx1.ID, tr1.ID, 100)
	stats1 := &kernel.Stats{ActionID: a.ID, Uses: 1, Successes: 1, LatencyEstimate: 0.1, LastUsedAt: time.Now().UTC()}
	if err := db.CommitCall(ctx, tx1, rc1, tr1.ID, p1.ID, kernel.CallerProcess, target.ID, fee.ID, tx1.Net, tx1.Fee, stats1, "", ""); err != nil {
		t.Fatalf("CommitCall #1: %v", err)
	}

	p2, tr2 := makeRun(50)
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
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 200); err != nil {
		t.Fatalf("BeginRun: %v", err)
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
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, owner.ID, 100); err != nil {
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
		tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		_ = i
		if err := db.BeginRun(ctx, p, tr, ownerID, 0); err != nil {
			t.Fatalf("BeginRun %d: %v", i, err)
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
		ID:           uuid.New().String(),
		OwnerUserID:  issuer.ID,
		CallerUserID: issuer.ID,
		TargetUserID: issuer.ID,
		ActionID:     uuid.New().String(),
		Status:       kernel.TxSuccess,
		StartedAt:    time.Now().UTC(),
		EndedAt:      time.Now().UTC(),
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
		ID:           uuid.New().String(),
		OwnerUserID:  rater.ID,
		CallerUserID: rater.ID,
		TargetUserID: rater.ID,
		ActionID:     uuid.New().String(),
		Status:       kernel.TxSuccess,
		StartedAt:    time.Now().UTC(),
		EndedAt:      time.Now().UTC(),
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
		ActionID:   action.ID,
		LastUsedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertStats: %v", err)
	}

	rater := newUser("@rater", 0)
	_ = db.CreateUser(ctx, rater)
	tx := &kernel.Transaction{
		ID:           uuid.New().String(),
		OwnerUserID:  rater.ID,
		CallerUserID: rater.ID,
		TargetUserID: owner.ID,
		ActionID:     action.ID,
		Status:       kernel.TxSuccess,
		StartedAt:    time.Now().UTC(),
		EndedAt:      time.Now().UTC(),
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
		ID:           uuid.New().String(),
		OwnerUserID:  rater2.ID,
		CallerUserID: rater2.ID,
		TargetUserID: owner.ID,
		ActionID:     action.ID,
		Status:       kernel.TxSuccess,
		StartedAt:    time.Now().UTC(),
		EndedAt:      time.Now().UTC(),
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
			ID:           id,
			OwnerUserID:  rater.ID,
			CallerUserID: rater.ID,
			TargetUserID: owner.ID,
			ActionID:     action.ID,
			Status:       kernel.TxSuccess,
			StartedAt:    time.Now().UTC(),
			EndedAt:      time.Now().UTC(),
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

func TestListStepsPagination(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@ls-owner", 1000)
	_ = db.CreateUser(ctx, user)
	caller := newUser("@ls-caller", 0)
	_ = db.CreateUser(ctx, caller)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 100); err != nil {
		t.Fatal(err)
	}
	act := newAction(user.ID, "ls-act", 10, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}

	// Park three waiting steps from the root trace (3 * 10 = 30 <= 100).
	ptID := root.ID
	for i := 0; i < 3; i++ {
		step := &kernel.Step{
			ID:                   uuid.New().String(),
			ParentTraceID:        &ptID,
			RequiredCallerUserID: caller.ID,
			ActionID:             act.ID,
			Price:                10,
			Status:               kernel.StepWaiting,
			CreatedAt:            time.Now().UTC().Add(time.Duration(i) * time.Second),
		}
		if err := db.CreateStep(ctx, step); err != nil {
			t.Fatal(err)
		}
	}

	// The process owner sees all three; limit bounds the page.
	page1, err := db.ListSteps(ctx, user.ID, "", "", false, 2, 0)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("limit=2: want 2 steps, got %d", len(page1))
	}

	// Offset skips the first page.
	page2, _ := db.ListSteps(ctx, user.ID, "", "", false, 2, 2)
	if len(page2) != 1 {
		t.Fatalf("limit=2 offset=2: want 1 step, got %d", len(page2))
	}

	// A non-positive limit falls back to the default (50), returning all three.
	all, _ := db.ListSteps(ctx, user.ID, "", "", false, 0, 0)
	if len(all) != 3 {
		t.Fatalf("limit=0 fallback: want all 3 steps, got %d", len(all))
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
	{
		tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		if err := db.BeginRun(ctx, p, tr, caller.ID, 0); err != nil {
			t.Fatal(err)
		}
	}

	mkTx := func(id string, offset time.Duration) {
		tx := &kernel.Transaction{
			ID:            id,
			ProcessID:     p.ID,
			TraceID:       id + "-tr",
			ParentTraceID: id + "-tr",
			OwnerUserID:   caller.ID,
			CallerUserID:  caller.ID,
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
		OwnerUserID:   caller.ID,         // process owner
		CallerUserID:  distinctCaller.ID, // distinct call caller
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
	if got.PublicKey != "ed25519pubkeyABC" {
		t.Errorf("PublicKey: got %q", got.PublicKey)
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
		src := kernel.HTTPSource{
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
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, payer.ID, 100); err != nil {
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
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, payer.ID, 100); err != nil {
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
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, payer.ID, 100); err != nil {
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
		Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	inactive := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/inactive",
		Kind: kernel.KindHTTP, Active: false, Visibility: kernel.VisibilityPublic,
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

func TestProxyUserIdentifiedByPublicKey(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// A key-only account: a public key, no password (that credential combination is what makes it
	// a peer). Same CreateUser insert as any account.
	peer := newUser("@key-peer", 0)
	peer.PublicKey = "somepubkey"
	peer.PasswordHash = ""
	if err := db.CreateUser(ctx, peer); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	found, err := db.ReadUserByPublicKey(ctx, "somepubkey")
	if err != nil {
		t.Fatalf("ReadUserByPublicKey: %v", err)
	}
	if found.ID != peer.ID {
		t.Errorf("expected peer ID %s, got %s", peer.ID, found.ID)
	}
	if found.PublicKey == "" || found.PasswordHash != "" {
		t.Errorf("key-only account should have public_key set and empty password, got key=%q hash=%q", found.PublicKey, found.PasswordHash)
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
		ActionID:   a1.ID,
		Uses:       3,
		Successes:  3,
		LastUsedAt: now,
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
		root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		_ = db.BeginRun(ctx, p, root, user.ID, price)
		ptID := root.ID
		step := &kernel.Step{
			ID: uuid.New().String(), ParentTraceID: &ptID,
			RequiredCallerUserID: user.ID, ActionID: act.ID,
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

// TestListUnsettledTracesChildFirst verifies Fix 1: ListUnsettledTracesForProcess returns
// children before parents so EndProcess settles deepest traces first.
func TestListUnsettledTracesChildFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@unsettled-order", 200)
	_ = db.CreateUser(ctx, user)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: user.ID, CallerUserID: user.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 200); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}

	child := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: user.ID, CallerUserID: user.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, root.ID, child, 50); err != nil {
		t.Fatalf("BeginSubcall: %v", err)
	}

	traces, err := db.ListUnsettledTracesForProcess(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListUnsettledTracesForProcess: %v", err)
	}
	if len(traces) != 2 {
		t.Fatalf("expected 2 unsettled traces, got %d", len(traces))
	}
	if traces[0].ID != child.ID {
		t.Errorf("first trace should be child %q (deepest-first), got %q", child.ID, traces[0].ID)
	}
	if traces[1].ID != root.ID {
		t.Errorf("second trace should be root %q, got %q", root.ID, traces[1].ID)
	}
}

// TestListDirectUnsettledChildren verifies that ListDirectUnsettledChildren returns only
// direct children of the given parent trace that have no committed transaction, and that
// a child with a committed transaction is excluded.
func TestListDirectUnsettledChildren(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@unsettled-children", 300)
	_ = db.CreateUser(ctx, user)
	feeUser := newUser("@fee-uc", 0)
	_ = db.CreateUser(ctx, feeUser)
	act := newAction(user.ID, "uc-act", 0, true)
	_ = db.CreateAction(ctx, act)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: user.ID, CallerUserID: user.ID, ActionID: act.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 200); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}

	// child1: unsettled subcall
	child1 := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: user.ID, CallerUserID: user.ID, ActionID: act.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, root.ID, child1, 50); err != nil {
		t.Fatalf("BeginSubcall child1: %v", err)
	}

	// child2: settled subcall (has a committed transaction)
	child2 := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: user.ID, CallerUserID: user.ID, ActionID: act.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, root.ID, child2, 50); err != nil {
		t.Fatalf("BeginSubcall child2: %v", err)
	}
	tx2 := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: child2.ID,
		OwnerUserID: user.ID, CallerUserID: user.ID, TargetUserID: user.ID,
		ActionID: act.ID, Status: kernel.TxSuccess, Gross: 50, Net: 40, Fee: 10,
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	rc2 := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: user.ID, TxID: tx2.ID, TraceID: child2.ID,
		ActionID: act.ID, Status: kernel.TxSuccess, Gross: 50, Net: 40, Fee: 10,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CommitCall(ctx, tx2, rc2, child2.ID, root.ID, kernel.CallerTrace, user.ID, feeUser.ID, 40, 10, nil, "", ""); err != nil {
		t.Fatalf("CommitCall child2: %v", err)
	}

	// grandchild of child1: should NOT appear (not a direct child of root)
	grandchild := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: user.ID, CallerUserID: user.ID, ActionID: act.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, child1.ID, grandchild, 20); err != nil {
		t.Fatalf("BeginSubcall grandchild: %v", err)
	}

	children, err := db.ListDirectUnsettledChildren(ctx, root.ID)
	if err != nil {
		t.Fatalf("ListDirectUnsettledChildren: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("expected 1 unsettled direct child, got %d", len(children))
	}
	if children[0].ID != child1.ID {
		t.Errorf("expected child1 %q, got %q", child1.ID, children[0].ID)
	}
}

// TestResetStepAndReparkWithDescendantTransaction verifies that ResetStepAndRepark rejects
// a re-park when the completion trace has a committed descendant transaction, even if the
// trace's own available/locked look correct.
func TestResetStepAndReparkWithDescendantTransaction(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@repark-desc-tx", 200)
	_ = db.CreateUser(ctx, user)
	feeUser := newUser("@fee-rdtx", 0)
	_ = db.CreateUser(ctx, feeUser)
	act := newAction(user.ID, "repark-desc-tx-act", 100, true)
	_ = db.CreateAction(ctx, act)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	_ = db.BeginRun(ctx, p, root, user.ID, 100)
	ptID := root.ID
	step := &kernel.Step{
		ID: uuid.New().String(), ParentTraceID: &ptID,
		RequiredCallerUserID: user.ID, ActionID: act.ID,
		Price: 100, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
	}
	_ = db.CreateStep(ctx, step)
	ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	_ = db.BeginStepCall(ctx, step.ID, ct)

	// Create a descendant subcall of the completion trace and commit a transaction for it.
	sub := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, ct.ID, sub, 50); err != nil {
		t.Fatalf("BeginSubcall: %v", err)
	}
	// Settle the subcall so ct.available and ct.locked look normal, but a descendant tx exists.
	subTx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: sub.ID,
		OwnerUserID: user.ID, CallerUserID: user.ID, TargetUserID: user.ID,
		ActionID: act.ID, Status: kernel.TxSuccess, Gross: 50, Net: 40, Fee: 10,
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	subRc := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: user.ID, TxID: subTx.ID, TraceID: sub.ID,
		ActionID: act.ID, Status: kernel.TxSuccess, Gross: 50, Net: 40, Fee: 10,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CommitCall(ctx, subTx, subRc, sub.ID, ct.ID, kernel.CallerTrace, user.ID, feeUser.ID, 40, 10, nil, "", ""); err != nil {
		t.Fatalf("CommitCall sub: %v", err)
	}

	// ct.available == price and ct.locked == 0 at this point (subcall settled and released lock),
	// but there IS a committed descendant transaction. Re-park must be rejected.
	err := db.ResetStepAndRepark(ctx, step.ID)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState for completion trace with descendant tx, got %v", err)
	}
	got, _ := db.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepRunning {
		t.Errorf("step.status after failed re-park: got %s, want running", got.Status)
	}
}

// TestResetStepAndReparkNonEmptyTrace verifies Fix 2: ResetStepAndRepark returns
// ErrInvalidState when the completion trace has committed downstream work.
func TestResetStepAndReparkNonEmptyTrace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@repark-nonempty", 200)
	_ = db.CreateUser(ctx, user)
	act := newAction(user.ID, "repark-nonempty-act", 100, true)
	_ = db.CreateAction(ctx, act)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	_ = db.BeginRun(ctx, p, root, user.ID, 100)
	ptID := root.ID
	step := &kernel.Step{
		ID: uuid.New().String(), ParentTraceID: &ptID,
		RequiredCallerUserID: user.ID, ActionID: act.ID,
		Price: 100, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
	}
	_ = db.CreateStep(ctx, step)
	ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	_ = db.BeginStepCall(ctx, step.ID, ct)

	// Make the completion trace non-empty: lock funds via a subcall.
	sub := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, ct.ID, sub, 50); err != nil {
		t.Fatalf("BeginSubcall: %v", err)
	}

	err := db.ResetStepAndRepark(ctx, step.ID)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState for non-empty completion trace, got %v", err)
	}

	// Step must still be running (re-park was aborted).
	got, _ := db.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepRunning {
		t.Errorf("step.status after failed re-park: got %s, want running", got.Status)
	}
}

// TestMigration011RewritesBareURLSources verifies the http-source unification
// migration converts legacy bare-URL kind=http sources into structured HTTPSource
// JSON while leaving already-structured (OpenAPI) sources untouched.
func TestMigration011RewritesBareURLSources(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@mig-owner", 0)
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}

	// Legacy manual action: bare URL string.
	bare := newAction(owner.ID, "/legacy", 0, false)
	bare.Source = "https://api.example.com/hook"
	if err := db.CreateAction(ctx, bare); err != nil {
		t.Fatal(err)
	}
	// OpenAPI action: already-structured JSON.
	oapi := newAction(owner.ID, "/imported", 0, false)
	oapi.Source = `{"type":"openapi","base_url":"https://api.example.com","method":"GET","path":"/items"}`
	if err := db.CreateAction(ctx, oapi); err != nil {
		t.Fatal(err)
	}

	// Re-apply migration 011 (idempotent over already-structured rows).
	sqlBytes, err := migrationFS.ReadFile("migrations/011_http_source_structured.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	gotBare, _ := db.ReadAction(ctx, bare.ID)
	var s kernel.HTTPSource
	if err := json.Unmarshal([]byte(gotBare.Source), &s); err != nil {
		t.Fatalf("legacy source not rewritten to JSON: %v (%s)", err, gotBare.Source)
	}
	if s.Type != "http" || s.Method != "POST" || s.BaseURL != "https://api.example.com/hook" {
		t.Errorf("rewritten source unexpected: %+v", s)
	}

	gotOapi, _ := db.ReadAction(ctx, oapi.ID)
	if gotOapi.Source != oapi.Source {
		t.Errorf("openapi source must be untouched: got %s", gotOapi.Source)
	}
}

// ---- Peer retention purge (§13) ----

func TestPurgePeerCascade(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	peer := newPeer(t, db, "@peerP", "peerkeyAAA", 0, 0, time.Now().UTC())

	var actIDs []string
	for _, n := range []string{"svc-1", "svc-2"} {
		a := newAction(peer.ID, n, 10, true)
		if err := db.CreateAction(ctx, a); err != nil {
			t.Fatalf("create action: %v", err)
		}
		actIDs = append(actIDs, a.ID)
		if err := db.UpsertStats(ctx, &kernel.Stats{ActionID: a.ID, Uses: 5, Successes: 5, LastUsedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("upsert stats: %v", err)
		}
		if err := db.UpsertStatTag(ctx, &kernel.StatTag{ActionID: a.ID, Key: "gossip_uses", Value: "5", Source: "introX", UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("upsert stat tag: %v", err)
		}
	}

	// A discovered_kernels row about the peer (must be deleted) and one about another kernel that
	// the peer merely introduced (must survive — it is information about a different peer).
	now := time.Now().UTC()
	if err := db.CreateOrUpdateDiscoveredKernel(ctx, &kernel.DiscoveredKernel{PublicKey: "peerkeyAAA", IntroducedBy: "someIntro", Handle: "@peerP", StatsJSON: json.RawMessage("{}"), FirstSeen: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOrUpdateDiscoveredKernel(ctx, &kernel.DiscoveredKernel{PublicKey: "otherkeyBBB", IntroducedBy: "peerkeyAAA", Handle: "@other", StatsJSON: json.RawMessage("{}"), FirstSeen: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	// Ledger: a transaction crediting the peer as target, referencing a peer action that gets
	// deleted. It must survive the purge with its now-dangling action id intact (§11).
	txID := insertTx(t, db, "some-local-owner", "some-local-owner", peer.ID, actIDs[0], now)

	if err := db.PurgePeerCascade(ctx, peer.ID); err != nil {
		t.Fatalf("PurgePeerCascade: %v", err)
	}

	count := func(q string, args ...any) int {
		var n int
		if err := db.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM actions WHERE owner_user_id=?`, peer.ID); n != 0 {
		t.Errorf("peer actions after purge = %d, want 0", n)
	}
	if n := count(`SELECT COUNT(*) FROM action_stats WHERE action_id IN (?,?)`, actIDs[0], actIDs[1]); n != 0 {
		t.Errorf("action_stats after purge = %d, want 0", n)
	}
	if n := count(`SELECT COUNT(*) FROM stat_tags WHERE action_id IN (?,?)`, actIDs[0], actIDs[1]); n != 0 {
		t.Errorf("stat_tags after purge = %d, want 0", n)
	}
	if n := count(`SELECT COUNT(*) FROM discovered_kernels WHERE public_key=?`, "peerkeyAAA"); n != 0 {
		t.Errorf("discovered_kernels(peer) after purge = %d, want 0", n)
	}
	if n := count(`SELECT COUNT(*) FROM discovered_kernels WHERE public_key=?`, "otherkeyBBB"); n != 1 {
		t.Errorf("discovered_kernels(other) after purge = %d, want 1 (preserved)", n)
	}
	if _, err := db.ReadTransaction(ctx, txID); err != nil {
		t.Errorf("ledger transaction must survive purge: %v", err)
	}
	u, err := db.ReadUser(ctx, peer.ID)
	if err != nil {
		t.Fatalf("peer user must remain as ledger anchor: %v", err)
	}
	if u.PublicKey != "" {
		t.Errorf("peer public_key must be cleared, got %q", u.PublicKey)
	}
}

func TestListPurgeablePeers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)

	idle := newPeer(t, db, "@idle", "k-idle", 0, 0, old) // the only purgeable peer

	newPeer(t, db, "@funded", "k-funded", 100, 0, old) // excluded: holds value (available)
	newPeer(t, db, "@locked", "k-locked", 0, 50, old)  // excluded: holds value (locked)
	newPeer(t, db, "@recent", "k-recent", 0, 0, now)   // excluded: created within the window

	// excluded: a recent gossip mention keeps it live
	newPeer(t, db, "@gossip", "k-gossip", 0, 0, old)
	if err := db.CreateOrUpdateDiscoveredKernel(ctx, &kernel.DiscoveredKernel{PublicKey: "k-gossip", IntroducedBy: "x", Handle: "@gossip", StatsJSON: json.RawMessage("{}"), FirstSeen: old, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	// excluded: a recent transaction names the peer, though its balance is zero
	txp := newPeer(t, db, "@txp", "k-txp", 0, 0, old)
	insertTx(t, db, txp.ID, txp.ID, "some-target", "some-action", now)

	// excluded: a waiting step is addressed to the peer as required caller
	stepp := newPeer(t, db, "@stepp", "k-stepp", 0, 0, old)
	owner := newUser("@sowner", 0)
	_ = db.CreateUser(ctx, owner)
	act := newAction(owner.ID, "approve", 0, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateStep(ctx, &kernel.Step{ID: uuid.New().String(), RequiredCallerUserID: stepp.ID, ActionID: act.ID, Price: 0, Status: kernel.StepWaiting, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	// excluded: a local (non-peer) account, even though old and zero-balance
	local := newUser("@local", 0)
	local.CreatedAt = old
	_ = db.CreateUser(ctx, local)

	// The §13 sync cache must NOT count as activity: a fresh peer_last_seen on the idle peer keeps
	// it purgeable, or answering gossip would immortalize a zombie peer.
	if err := db.UpdatePeerSync(ctx, idle.ID, now, nil); err != nil {
		t.Fatalf("UpdatePeerSync: %v", err)
	}

	ids, err := db.ListPurgeablePeers(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListPurgeablePeers: %v", err)
	}
	if len(ids) != 1 || ids[0] != idle.ID {
		t.Fatalf("purgeable = %v, want exactly [%s (@idle)]", ids, idle.ID)
	}
}

// TestUpdatePeerSync round-trips the §13 friend-sync cache and checks the COALESCE keep-prior rule.
func TestUpdatePeerSync(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	peer := newPeer(t, db, "@synced", "k-synced", 0, 0, now)

	credit := int64(900)
	if err := db.UpdatePeerSync(ctx, peer.ID, now, &credit); err != nil {
		t.Fatalf("UpdatePeerSync: %v", err)
	}
	got, _ := db.ReadUser(ctx, peer.ID)
	if got.PeerLastSeen == nil {
		t.Error("expected peer_last_seen set")
	}
	if got.PeerCredit == nil || *got.PeerCredit != 900 {
		t.Errorf("peer_credit = %v, want 900", got.PeerCredit)
	}

	// A nil credit refreshes last_seen but keeps the prior value (COALESCE).
	later := now.Add(time.Hour)
	if err := db.UpdatePeerSync(ctx, peer.ID, later, nil); err != nil {
		t.Fatalf("UpdatePeerSync nil: %v", err)
	}
	got, _ = db.ReadUser(ctx, peer.ID)
	if got.PeerCredit == nil || *got.PeerCredit != 900 {
		t.Errorf("nil credit must keep prior 900, got %v", got.PeerCredit)
	}
}

// ---- Grants (delegated upstream OAuth, §8) ----

func TestGrantCRUDAndUpsert(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("@grantor", 0)
	_ = db.CreateUser(ctx, user)
	a := newAction(user.ID, "/oauth-svc", 0, true)
	_ = db.CreateAction(ctx, a)

	g := &kernel.Grant{ID: uuid.New().String(), GrantorUserID: user.ID, ActionID: a.ID, RefreshToken: "sealed-1", CreatedAt: time.Now().UTC()}
	if err := db.CreateOrReplaceGrant(ctx, g); err != nil {
		t.Fatalf("CreateOrReplaceGrant: %v", err)
	}
	got, err := db.ReadGrant(ctx, user.ID, a.ID)
	if err != nil {
		t.Fatalf("ReadGrant: %v", err)
	}
	if got.RefreshToken != "sealed-1" {
		t.Errorf("refresh token = %q, want sealed-1", got.RefreshToken)
	}

	// Re-consent overwrites in place: still one row, new token, new id.
	g2 := &kernel.Grant{ID: uuid.New().String(), GrantorUserID: user.ID, ActionID: a.ID, RefreshToken: "sealed-2", CreatedAt: time.Now().UTC()}
	if err := db.CreateOrReplaceGrant(ctx, g2); err != nil {
		t.Fatalf("re-consent: %v", err)
	}
	list, _ := db.ListGrantsByUser(ctx, user.ID)
	if len(list) != 1 {
		t.Fatalf("grant count = %d, want 1 (upsert)", len(list))
	}
	if list[0].RefreshToken != "sealed-2" {
		t.Errorf("after upsert token = %q, want sealed-2", list[0].RefreshToken)
	}

	if err := db.DeleteGrant(ctx, user.ID, a.ID); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	if _, err := db.ReadGrant(ctx, user.ID, a.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("after delete: got %v, want ErrNotFound", err)
	}
	if err := db.DeleteGrant(ctx, user.ID, a.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("delete absent grant: got %v, want ErrNotFound", err)
	}
}

func TestDeleteGrantsForAction(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u1 := newUser("@g1", 0)
	u2 := newUser("@g2", 0)
	_ = db.CreateUser(ctx, u1)
	_ = db.CreateUser(ctx, u2)
	a := newAction(u1.ID, "/multi", 0, true)
	_ = db.CreateAction(ctx, a)

	for _, u := range []*kernel.User{u1, u2} {
		_ = db.CreateOrReplaceGrant(ctx, &kernel.Grant{ID: uuid.New().String(), GrantorUserID: u.ID, ActionID: a.ID, RefreshToken: "s", CreatedAt: time.Now().UTC()})
	}
	if err := db.DeleteGrantsForAction(ctx, a.ID); err != nil {
		t.Fatalf("DeleteGrantsForAction: %v", err)
	}
	for _, u := range []*kernel.User{u1, u2} {
		if _, err := db.ReadGrant(ctx, u.ID, a.ID); !errors.Is(err, kernel.ErrNotFound) {
			t.Errorf("grant for %s survived action-wide delete", u.Handle)
		}
	}
}

func TestConnectionCRUDAndCascade(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("@conn", 0)
	_ = db.CreateUser(ctx, u)
	a1 := newAction(u.ID, "inbox/send", 0, true)
	a2 := newAction(u.ID, "inbox/read", 0, true)
	_ = db.CreateAction(ctx, a1)
	_ = db.CreateAction(ctx, a2)

	c := &kernel.Connection{
		ID: uuid.New().String(), UserID: u.ID, ProviderKey: "bearer:api.test.com",
		SealedSecret: "sealed-1", ScopesJSON: "", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.CreateOrUpdateConnection(ctx, c); err != nil {
		t.Fatalf("CreateOrUpdateConnection: %v", err)
	}

	// Upsert on (user, provider_key) keeps id + created_at, refreshes secret + scopes.
	c2 := &kernel.Connection{
		ID: uuid.New().String(), UserID: u.ID, ProviderKey: "bearer:api.test.com",
		SealedSecret: "sealed-2", ScopesJSON: `["read"]`, CreatedAt: time.Now().UTC().Add(time.Hour), UpdatedAt: time.Now().UTC().Add(time.Hour),
	}
	if err := db.CreateOrUpdateConnection(ctx, c2); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}
	got, err := db.ReadConnectionByUserProvider(ctx, u.ID, "bearer:api.test.com")
	if err != nil {
		t.Fatalf("ReadConnectionByUserProvider: %v", err)
	}
	if got.ID != c.ID {
		t.Errorf("id changed on upsert: got %s want %s (created_at must be preserved)", got.ID, c.ID)
	}
	if got.SealedSecret != "sealed-2" || got.ScopesJSON != `["read"]` {
		t.Errorf("upsert did not refresh secret/scopes: %+v", got)
	}
	if !got.CreatedAt.Equal(c.CreatedAt) {
		// created_at is preserved from the original row, not overwritten by the upsert.
		t.Errorf("created_at = %v, want original %v", got.CreatedAt, c.CreatedAt)
	}

	// Rotation replaces the secret only.
	if err := db.UpdateConnectionSecret(ctx, got.ID, "sealed-3"); err != nil {
		t.Fatalf("UpdateConnectionSecret: %v", err)
	}
	if r, _ := db.ReadConnection(ctx, got.ID); r.SealedSecret != "sealed-3" {
		t.Errorf("after rotation secret = %q, want sealed-3", r.SealedSecret)
	}

	// Two grants point at the connection; cascade removes both and the connection.
	for _, a := range []*kernel.Action{a1, a2} {
		_ = db.CreateOrReplaceGrant(ctx, &kernel.Grant{ID: uuid.New().String(), GrantorUserID: u.ID, ActionID: a.ID, ConnectionID: got.ID, CreatedAt: time.Now().UTC()})
	}
	if list, _ := db.ListConnectionsByUser(ctx, u.ID); len(list) != 1 {
		t.Fatalf("connection count = %d, want 1", len(list))
	}
	if err := db.DeleteConnectionCascade(ctx, got.ID); err != nil {
		t.Fatalf("DeleteConnectionCascade: %v", err)
	}
	if _, err := db.ReadConnection(ctx, got.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("connection survived cascade: %v", err)
	}
	for _, a := range []*kernel.Action{a1, a2} {
		if _, err := db.ReadGrant(ctx, u.ID, a.ID); !errors.Is(err, kernel.ErrNotFound) {
			t.Errorf("grant on %s survived connection cascade", a.Name)
		}
	}
	if err := db.DeleteConnectionCascade(ctx, got.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("cascade absent connection: got %v, want ErrNotFound", err)
	}
}

func TestLegacyTokenBackfillHelpers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("@legacy", 0)
	_ = db.CreateUser(ctx, u)
	a := newAction(u.ID, "svc", 0, true)
	_ = db.CreateAction(ctx, a)

	// A legacy grant carries a sealed token and no connection.
	g := &kernel.Grant{ID: uuid.New().String(), GrantorUserID: u.ID, ActionID: a.ID, RefreshToken: "legacy-sealed", CreatedAt: time.Now().UTC()}
	_ = db.CreateOrReplaceGrant(ctx, g)

	legacy, err := db.ListLegacyTokenGrants(ctx)
	if err != nil {
		t.Fatalf("ListLegacyTokenGrants: %v", err)
	}
	if len(legacy) != 1 || legacy[0].RefreshToken != "legacy-sealed" || legacy[0].ConnectionID != "" {
		t.Fatalf("legacy grants = %+v, want one unlinked token", legacy)
	}

	c := &kernel.Connection{ID: uuid.New().String(), UserID: u.ID, ProviderKey: "oauth:t|c", SealedSecret: "resealed", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_ = db.CreateOrUpdateConnection(ctx, c)
	if err := db.LinkGrantConnection(ctx, g.ID, c.ID); err != nil {
		t.Fatalf("LinkGrantConnection: %v", err)
	}

	// After linking, the grant points at the connection and holds no token; the backfill
	// predicate is now empty (idempotent: a second pass finds nothing).
	got, _ := db.ReadGrant(ctx, u.ID, a.ID)
	if got.ConnectionID != c.ID || got.RefreshToken != "" {
		t.Errorf("after link: connection=%q token=%q, want linked and empty", got.ConnectionID, got.RefreshToken)
	}
	if again, _ := db.ListLegacyTokenGrants(ctx); len(again) != 0 {
		t.Errorf("legacy predicate not cleared after link: %d rows", len(again))
	}
}

func TestListNativeActions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@sys", 0)
	_ = db.CreateUser(ctx, owner)

	// A live native, an http action (wrong kind), and a soft-deleted native.
	live := newAction(owner.ID, "time", 0, true)
	live.Kind = kernel.KindNative
	_ = db.CreateAction(ctx, live)

	httpAct := newAction(owner.ID, "weather", 0, true) // kind=http
	_ = db.CreateAction(ctx, httpAct)

	deleted := newAction(owner.ID, "make", 0, true)
	deleted.Kind = kernel.KindNative
	_ = db.CreateAction(ctx, deleted)
	if err := db.DeleteAction(ctx, deleted.ID); err != nil {
		t.Fatal(err)
	}

	natives, err := db.ListNativeActions(ctx)
	if err != nil {
		t.Fatalf("ListNativeActions: %v", err)
	}
	if len(natives) != 1 {
		t.Fatalf("want 1 live native, got %d: %v", len(natives), natives)
	}
	if natives[0].Name != "time" || natives[0].Kind != kernel.KindNative {
		t.Errorf("unexpected native: name=%q kind=%q", natives[0].Name, natives[0].Kind)
	}
}

func TestCreateLedgerEntry(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sys := newUser("@sys", 0)
	alice := newUser("@alice", 100)
	bob := newUser("@bob", 0)
	for _, u := range []*kernel.User{sys, alice, bob} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	mustEntry := func(e *kernel.LedgerEntry) {
		t.Helper()
		if err := db.CreateLedgerEntry(ctx, e); err != nil {
			t.Fatalf("create ledger entry: %v", err)
		}
	}
	avail := func(id string) int64 {
		u, err := db.ReadUser(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return u.Available
	}

	// Credit-only (deposit): only to is set.
	mustEntry(&kernel.LedgerEntry{ID: uuid.New().String(), OperatorUserID: sys.ID, ToUserID: alice.ID, Amount: 50, CreatedAt: time.Now().UTC()})
	if avail(alice.ID) != 150 {
		t.Errorf("alice after credit: got %d, want 150", avail(alice.ID))
	}
	// Debit-only (withdraw): only from is set.
	mustEntry(&kernel.LedgerEntry{ID: uuid.New().String(), OperatorUserID: sys.ID, FromUserID: alice.ID, Amount: 20, CreatedAt: time.Now().UTC()})
	if avail(alice.ID) != 130 {
		t.Errorf("alice after debit: got %d, want 130", avail(alice.ID))
	}
	// Both (transfer): from and to set, one commit.
	mustEntry(&kernel.LedgerEntry{ID: uuid.New().String(), OperatorUserID: alice.ID, FromUserID: alice.ID, ToUserID: bob.ID, Amount: 30, CreatedAt: time.Now().UTC()})
	if avail(alice.ID) != 100 || avail(bob.ID) != 30 {
		t.Errorf("after transfer: alice=%d bob=%d, want 100/30", avail(alice.ID), avail(bob.ID))
	}
	// Insufficient funds on the debit leg returns ErrInsufficientFunds and moves nothing.
	err := db.CreateLedgerEntry(ctx, &kernel.LedgerEntry{ID: uuid.New().String(), OperatorUserID: alice.ID, FromUserID: alice.ID, ToUserID: bob.ID, Amount: 1000, CreatedAt: time.Now().UTC()})
	if !errors.Is(err, kernel.ErrInsufficientFunds) {
		t.Errorf("overdraw: got %v, want ErrInsufficientFunds", err)
	}
	if avail(alice.ID) != 100 || avail(bob.ID) != 30 {
		t.Errorf("after failed transfer: alice=%d bob=%d, want 100/30 (unchanged)", avail(alice.ID), avail(bob.ID))
	}

	// ListLedgerByUser: alice is party to the credit, debit, and transfer = 3 committed rows;
	// the failed transfer rolled back and wrote nothing. Bob only to the transfer = 1.
	aliceEntries, err := db.ListLedgerByUser(ctx, alice.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceEntries) != 3 {
		t.Errorf("alice ledger entries: got %d, want 3", len(aliceEntries))
	}
	bobEntries, err := db.ListLedgerByUser(ctx, bob.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobEntries) != 1 {
		t.Errorf("bob ledger entries: got %d, want 1", len(bobEntries))
	}

	// Pagination bounds the result set (alice has 3 committed entries).
	if got, _ := db.ListLedgerByUser(ctx, alice.ID, 1, 0); len(got) != 1 {
		t.Errorf("limit=1: got %d entries, want 1", len(got))
	}
	if got, _ := db.ListLedgerByUser(ctx, alice.ID, 2, 1); len(got) != 2 {
		t.Errorf("limit=2 offset=1: got %d entries, want 2 of 3", len(got))
	}
}

// Step gates back the @sys/step/join native (§9). They are native-owned state — not part of the
// kernel.Store interface — so they are exercised directly here.
func TestStepGate_IncrementAndDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// A gate hangs off a real step (FK + ON DELETE CASCADE), so a cancelled or purged step takes
	// its barrier with it rather than leaving an orphan row.
	user := newUser("@gate-user", 100)
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: user.ID, Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 10); err != nil {
		t.Fatal(err)
	}
	act := newAction(user.ID, "gate-act", 10, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	ptID := root.ID
	step := &kernel.Step{
		ID: uuid.New().String(), ParentTraceID: &ptID, RequiredCallerUserID: user.ID,
		ActionID: act.ID, Price: 10, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateStep(ctx, step); err != nil {
		t.Fatal(err)
	}
	stepID := step.ID

	have, need, err := db.IncrementStepGate(ctx, stepID, 3)
	if err != nil {
		t.Fatalf("first increment: %v", err)
	}
	if have != 1 || need != 3 {
		t.Fatalf("first increment: have=%d need=%d, want 1/3", have, need)
	}

	if have, _, err = db.IncrementStepGate(ctx, stepID, 3); err != nil || have != 2 {
		t.Fatalf("second increment: have=%d err=%v, want 2", have, err)
	}

	// The threshold is fixed by the first contribution: a contributor naming a different one is a
	// workflow bug, not a race, so it is rejected rather than silently re-basing the barrier.
	if _, _, err := db.IncrementStepGate(ctx, stepID, 5); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("need mismatch: expected ErrInvalidInput, got %v", err)
	}

	if err := db.DeleteStepGate(ctx, stepID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if have, _, err = db.IncrementStepGate(ctx, stepID, 2); err != nil || have != 1 {
		t.Errorf("after delete the gate reopens fresh: have=%d err=%v, want 1", have, err)
	}
	// Deleting a gate that was never opened is a no-op, so a losing join never errors on cleanup.
	if err := db.DeleteStepGate(ctx, "never-opened"); err != nil {
		t.Errorf("delete of an absent gate should be a no-op, got %v", err)
	}
}

// ListStepsAwaitingCaller must be scoped in SQL and oldest-first: the federation step list (§13)
// relies on it, and filtering ListSteps' disjunction in Go after its row cap discarded exactly the
// steps a peer could complete.
func TestListStepsAwaitingCaller(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@owner", 1000)
	assignee := newUser("@assignee", 0)
	other := newUser("@other", 0)
	for _, u := range []*kernel.User{owner, assignee, other} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	act := newAction(owner.ID, "await-act", 0, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	mkStep := func(processOwner, requiredCaller string) string {
		t.Helper()
		p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: processOwner, Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
		root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		if err := db.BeginRun(ctx, p, root, processOwner, 0); err != nil {
			t.Fatal(err)
		}
		ptID := root.ID
		st := &kernel.Step{
			ID: uuid.New().String(), ParentTraceID: &ptID, RequiredCallerUserID: requiredCaller,
			ActionID: act.ID, Price: 0, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
		}
		if err := db.CreateStep(ctx, st); err != nil {
			t.Fatal(err)
		}
		return st.ID
	}

	// The assignee's own step comes first in time; 60 steps in processes it owns follow. Under the
	// old "cap then filter in Go" shape those 60 would fill the page and hide this one.
	mine := mkStep(owner.ID, assignee.ID)
	for i := 0; i < 60; i++ {
		mkStep(assignee.ID, other.ID)
	}

	got, _, err := db.ListStepsAwaitingCaller(ctx, assignee.ID, 200, "")
	if err != nil {
		t.Fatalf("ListStepsAwaitingCaller: %v", err)
	}
	if len(got) != 1 || got[0].ID != mine {
		t.Fatalf("expected only the assignee's own waiting step, got %d rows", len(got))
	}

	// Oldest first: the longest-stranded step is what an operator needs to see.
	second := mkStep(owner.ID, assignee.ID)
	got, _, _ = db.ListStepsAwaitingCaller(ctx, assignee.ID, 200, "")
	if len(got) != 2 || got[0].ID != mine || got[1].ID != second {
		t.Errorf("expected oldest-first ordering, got %d rows in unexpected order", len(got))
	}

	// Keyset paging: page 1 of size 1, then follow the cursor to reach the second row.
	first, cur, err := db.ListStepsAwaitingCaller(ctx, assignee.ID, 1, "")
	if err != nil || len(first) != 1 || first[0].ID != mine {
		t.Fatalf("page 1: got %v err=%v", first, err)
	}
	if paged, _, _ := db.ListStepsAwaitingCaller(ctx, assignee.ID, 1, cur); len(paged) != 1 || paged[0].ID != second {
		t.Errorf("cursor paging failed, got %v", paged)
	}
}

// seedWaitingStep creates owner+assignee, an action, a funded process and root trace, and returns
// a maker for waiting steps on that trace. Four paging/gate tests built this by hand.
func seedWaitingStep(t *testing.T, db *DB, prefix string) (assigneeID string, mk func() string) {
	t.Helper()
	ctx := context.Background()
	owner := newUser("@"+prefix+"-owner", 1000)
	assignee := newUser("@"+prefix+"-assignee", 0)
	for _, u := range []*kernel.User{owner, assignee} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	act := newAction(owner.ID, prefix+"-act", 0, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	return assignee.ID, func() string {
		t.Helper()
		p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: owner.ID, Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
		root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		if err := db.BeginRun(ctx, p, root, owner.ID, 0); err != nil {
			t.Fatal(err)
		}
		ptID := root.ID
		st := &kernel.Step{
			ID: uuid.New().String(), ParentTraceID: &ptID, RequiredCallerUserID: assignee.ID,
			ActionID: act.ID, Price: 0, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
		}
		if err := db.CreateStep(ctx, st); err != nil {
			t.Fatal(err)
		}
		return st.ID
	}
}

// Scenario (review finding 7): offset pagination over a LIVE query skips a step. A step settled
// between pages shifts the window, so one still-waiting step is never listed — under a listing
// that looks complete. Written from the failure scenario before the fix; must fail on offsets.
func TestListStepsAwaitingCallerNoSkipWhenAStepSettlesMidPage(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	assignee, mk := seedWaitingStep(t, db, "page")

	const total = 6
	created := make([]string, total)
	for i := range created {
		created[i] = mk()
	}

	page1, cursor, err := db.ListStepsAwaitingCaller(ctx, assignee, 3, "")
	if err != nil || len(page1) != 3 {
		t.Fatalf("page 1: got %d rows err=%v, want 3", len(page1), err)
	}
	// A step from page 1 settles before page 2 is fetched — an offset window would shift.
	if err := db.ExecForTest(ctx, `UPDATE steps SET status='done' WHERE id=?`, page1[0].ID); err != nil {
		t.Fatal(err)
	}
	page2, _, err := db.ListStepsAwaitingCaller(ctx, assignee, 3, cursor)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}

	seen := map[string]bool{}
	for _, s := range append(append([]*kernel.Step{}, page1...), page2...) {
		if seen[s.ID] {
			t.Errorf("step %s listed twice", s.ID)
		}
		seen[s.ID] = true
	}
	for _, id := range created[1:] {
		if !seen[id] {
			t.Errorf("still-waiting step %s was skipped across the page boundary", id)
		}
	}
}

// A cursor must be stable when two steps share a created_at instant: the id tiebreak is what
// keeps the comparator a strict total order.
func TestListStepsAwaitingCallerTiebreaksOnID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	assignee, mk := seedWaitingStep(t, db, "tie")

	// Force every step onto one instant: without the id tiebreak the cursor is not a total order.
	ids := []string{mk(), mk(), mk(), mk()}
	same := timeToStr(time.Now().UTC())
	for _, id := range ids {
		if err := db.ExecForTest(ctx, `UPDATE steps SET created_at=? WHERE id=?`, same, id); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < len(ids); page++ {
		got, next, err := db.ListStepsAwaitingCaller(ctx, assignee, 2, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(got) == 0 {
			break
		}
		for _, s := range got {
			if seen[s.ID] {
				t.Fatalf("step %s listed twice across identical timestamps", s.ID)
			}
			seen[s.ID] = true
		}
		cursor = next
	}
	if len(seen) != len(ids) {
		t.Errorf("listed %d of %d steps sharing one instant", len(seen), len(ids))
	}
}

func TestListStepsAwaitingCallerRejectsMalformedCursor(t *testing.T) {
	db := openTestDB(t)
	// A malformed cursor must be an error, never a silent restart from the first page: a client
	// accumulating pages would otherwise duplicate everything it had already collected.
	if _, _, err := db.ListStepsAwaitingCaller(context.Background(), "u1", 10, "not-a-cursor"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for a malformed cursor, got %v", err)
	}
}

// Scenario (design review): CommitRemoteSettlement completes the idempotency record with
// ktx.ReplyJSON, which settleRemoteCall sets only on SUCCESS. A remote FAILURE therefore stores
// result_json "null", so a replaying peer reads no "error" key and gets HTTP 200 — a settled
// failure replaying as success, which §15 forbids. Written from the scenario before the fix.
func TestCommitRemoteSettlementStoresFailureResult(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("@rs-owner", 1000)
	proxy := newUser("@rs-proxy", 0)
	sys := newUser("@rs-sys", 0)
	for _, u := range []*kernel.User{owner, proxy, sys} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	act := newAction(proxy.ID, "rs-act", 10, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: owner.ID, Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ActionOwnerID: proxy.ID, ActionID: act.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, owner.ID, 10); err != nil {
		t.Fatal(err)
	}
	rec := &kernel.IdempotencyRecord{
		ID: uuid.New().String(), IdempotencyKey: "k-rs", CounterpartyUserID: proxy.ID,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := db.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// A remote FAILURE settlement: ReplyJSON is unset, exactly as settleRemoteCall leaves it.
	now := time.Now().UTC()
	ktx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID,
		OwnerUserID: owner.ID, CallerUserID: owner.ID, TargetUserID: proxy.ID,
		ActionID: act.ID, ActionName: act.Name, Status: kernel.TxFailure,
		Gross: 10, Reason: "remote call failed", StartedAt: now, EndedAt: now,
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: sys.ID, TxID: ktx.ID, TraceID: root.ID,
		ActionID: act.ID, CallerUserID: owner.ID, ProcessID: p.ID,
		Status: "failure", Gross: 10, StartedAt: now, CreatedAt: now,
	}
	if err := db.CommitRemoteSettlement(ctx, ktx, receipt, root.ID, p.ID, kernel.CallerProcess,
		proxy.ID, sys.ID, 0, 0, &kernel.Stats{ActionID: act.ID}, rec.ID, "", kernel.ErrExecutionFailed.Code); err != nil {
		t.Fatalf("CommitRemoteSettlement: %v", err)
	}

	got, err := db.ReadIdempotencyRecord(ctx, "k-rs", proxy.ID)
	if err != nil {
		t.Fatalf("GetIdempotencyRecord: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(got.ResultJSON), &result); err != nil {
		t.Fatalf("stored result_json %q is not an object: %v", got.ResultJSON, err)
	}
	if result["error"] == nil {
		t.Errorf("a settled remote FAILURE must store an error body so a replay cannot report success; got %q", got.ResultJSON)
	}
	if result["code"] != kernel.ErrExecutionFailed.Code {
		t.Errorf("stored code = %v, want %q", result["code"], kernel.ErrExecutionFailed.Code)
	}
}
