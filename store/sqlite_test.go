package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// The schema history is one baseline (store/migrations/001_baseline.sql). These five cases pin
// the whole contract of that cut: what a fresh database gets, what a database at the end of the
// old chain gets, and the three states the runner must refuse rather than guess at.

// legacyChainDB builds a database that looks exactly like one left by the previous release: the
// full 42-row migration ledger and a grants table still carrying the legacy refresh_token column.
// It is built RAW — never through Open — because Open is the thing under test: the fixture must
// reach the runner in the pre-baseline state, not one the runner has already reconciled.
func legacyChainDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw := rawDB(t, path)
	if _, err := raw.Exec(createSchemaMigrations); err != nil {
		t.Fatal(err)
	}
	body, err := migrationFS.ReadFile("migrations/001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range splitSQLStatements(string(body)) {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed baseline: %v", err)
		}
	}
	if _, err := raw.Exec(`ALTER TABLE grants ADD COLUMN refresh_token TEXT`); err != nil {
		t.Fatal(err)
	}
	names := []string{"042_discovery_effect"}
	for i := 1; i <= 41; i++ {
		names = append(names, fmt.Sprintf("%03d_step", i))
	}
	for _, v := range names {
		if _, err := raw.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			v, timeToStr(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	return path
}

// preValueMigrationDB builds a database at exactly the state before 043 — the baseline applied and
// stamped, nothing after — so the value-channel migration reaches the runner with real rows to guard.
// Built RAW, never through Open, because Open is what is under test. seed runs against that state.
func preValueMigrationDB(t *testing.T, seed func(*sql.DB)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pre043.db")
	raw := rawDB(t, path)
	if _, err := raw.Exec(createSchemaMigrations); err != nil {
		t.Fatal(err)
	}
	body, err := migrationFS.ReadFile("migrations/001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range splitSQLStatements(string(body)) {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed baseline: %v", err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		baselineVersion, timeToStr(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO "accounts" (id,handle,available,locked,created_at,updated_at)
		VALUES ('u1','alice',0,500,?,?)`, timeToStr(time.Now().UTC()), timeToStr(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	seed(raw)
	raw.Close()
	return path
}

// TestValueMigrationRefusesToStrandFunds: the value channel became local, and 043 drops the records
// the cross-kernel legs used. Locked funds must never go with them, so the migration aborts — the
// kernel refuses to start — while an unresolved payment reserve or a cross-kernel value lock exists,
// leaving the operator to drain it first. A database with neither upgrades.
func TestValueMigrationRefusesToStrandFunds(t *testing.T) {
	now := timeToStr(time.Now().UTC())
	for _, tc := range []struct {
		name    string
		seed    func(*sql.DB)
		wantErr bool
	}{
		{"unresolved payment reserve", func(raw *sql.DB) {
			if _, err := raw.Exec(`INSERT INTO pending_transfers
				(id,buyer_id,peer_key,step_id,input_hash,input,idempotency_key,beneficiary,amount,remote_max,reserve,status,created_at,updated_at)
				VALUES ('pt1','u1','peerkey','s1','h','{}','idem','benef',100,105,110,'pending',?,?)`, now, now); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"quarantined payment reserve", func(raw *sql.DB) {
			if _, err := raw.Exec(`INSERT INTO pending_transfers
				(id,buyer_id,peer_key,step_id,input_hash,input,idempotency_key,beneficiary,amount,remote_max,reserve,status,created_at,updated_at)
				VALUES ('pt2','u1','peerkey','s2','h','{}','idem2','benef',100,105,110,'quarantined',?,?)`, now, now); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"unsettled cross-kernel value lock", func(raw *sql.DB) {
			if _, err := raw.Exec(`INSERT INTO processes (id,owner_user_id,available,locked,status,created_at)
				VALUES ('p1','u1',0,0,'open',?)`, now); err != nil {
				t.Fatal(err)
			}
			// reserve > value: the difference was cross-kernel value fees, and no transaction settled it.
			if _, err := raw.Exec(`INSERT INTO "traces" (id,process_id,caller_user_id,available,locked,value,value_reserve,created_at)
				VALUES ('t1','p1','u1',0,0,100,110,?)`, now); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"fee-free outbound value lock", func(raw *sql.DB) {
			// remote_bps and import_bps may both be 0, so an outbound reserve equals the value exactly:
			// the amounts are indistinguishable from a local transfer and only the shape gives it away
			// (no local beneficiary). Post-migration nothing would deliver it, so it must be drained.
			if _, err := raw.Exec(`INSERT INTO processes (id,owner_user_id,available,locked,status,created_at)
				VALUES ('p3','u1',0,0,'open',?)`, now); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO "traces" (id,process_id,caller_user_id,available,locked,value,value_reserve,value_to,created_at)
				VALUES ('t3','p3','u1',0,0,100,100,'',?)`, now); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"peer-funded value lock", func(raw *sql.DB) {
			// An inbound transfer funded from a peer's row, again with zero fees. A peer funds no
			// transfer now, so this lock has no settlement path.
			if _, err := raw.Exec(`INSERT INTO kernels (public_key,first_seen,updated_at) VALUES ('peerkey',?,?)`,
				now, now); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO "accounts" (id,kernel_public_key,available,locked,created_at,updated_at)
				VALUES ('peer1','peerkey',0,100,?,?)`, now, now); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO processes (id,owner_user_id,available,locked,status,created_at)
				VALUES ('p4','peer1',0,0,'open',?)`, now); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO "traces" (id,process_id,caller_user_id,available,locked,value,value_reserve,value_to,created_at)
				VALUES ('t4','p4','peer1',0,0,100,100,'u1',?)`, now); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"settled local value", func(raw *sql.DB) {
			if _, err := raw.Exec(`INSERT INTO processes (id,owner_user_id,available,locked,status,created_at)
				VALUES ('p2','u1',0,0,'open',?)`, now); err != nil {
				t.Fatal(err)
			}
			// A local lock is exactly the delivered value, so it releases identically without the column.
			if _, err := raw.Exec(`INSERT INTO "traces" (id,process_id,caller_user_id,available,locked,value,value_reserve,value_to,created_at)
				VALUES ('t2','p2','u1',0,0,100,100,'u1',?)`, now); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := preValueMigrationDB(t, tc.seed)
			db, err := Open(path)
			if db != nil {
				defer db.Close()
			}
			if tc.wantErr && err == nil {
				t.Fatal("migration applied while funds were still locked: money would be stranded")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("migration refused a drainable database: %v", err)
			}
		})
	}
}

// TestValueLedgerBackfill: deliveries that settled before the journal existed are reconstructed from
// the receipts that signed them, so an old transfer reads like a new one. A receipt naming a party
// this kernel never held an account for is skipped rather than aborting the upgrade — the ledger's
// keys name accounts, and value could once cross a kernel boundary.
func TestValueLedgerBackfill(t *testing.T) {
	now := timeToStr(time.Now().UTC())
	path := preValueMigrationDB(t, func(raw *sql.DB) {
		if _, err := raw.Exec(`INSERT INTO "accounts" (id,handle,available,locked,created_at,updated_at)
			VALUES ('u2','bob',0,0,?,?)`, now, now); err != nil {
			t.Fatal(err)
		}
		insTx := `INSERT INTO transactions (id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,target_user_id,action_id,status,started_at,ended_at)
			VALUES (?,'p','t','','u1',?,'sys','a1',?,?,?)`
		ins := `INSERT INTO receipts (id,issuer_user_id,tx_id,trace_id,action_id,caller_user_id,args_hash,reply_hash,status,gross,net,fee,charge,premium,value,value_to,started_at,created_at,signature)
			VALUES (?,'u1',?,?,'a1',?,'ah','rh',?,0,0,0,0,0,?,?,?,?,'sig')`
		for _, r := range []struct {
			id, txID, caller, status, valueTo string
			value                             int64
		}{
			{"r1", "tx1", "u1", "success", "u2", 300},         // delivered locally: backfilled
			{"r2", "tx2", "u1", "failure", "u2", 50},          // never delivered
			{"r3", "tx3", "u1", "success", "u2", 0},           // ordinary call, no value
			{"r4", "tx4", "u1", "success", "gone-abroad", 70}, // beneficiary was never an account here
		} {
			if _, err := raw.Exec(insTx, r.txID, r.caller, r.status, now, now); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(ins, r.id, r.txID, r.txID, r.caller, r.status, r.value, r.valueTo, now, now); err != nil {
				t.Fatal(err)
			}
		}
	})
	db := openAt(t, path)
	ctx := context.Background()

	entries, err := db.ListLedgerByUser(ctx, "u2", 10, 0)
	if err != nil {
		t.Fatalf("ListLedgerByUser: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("backfilled %d entries for the beneficiary, want exactly the delivered one", len(entries))
	}
	e := entries[0]
	if e.FromUserID != "u1" || e.ToUserID != "u2" || e.Amount != 300 || e.Reason != "tx1" {
		t.Errorf("entry = from %s to %s amount %d reason %q, want u1→u2 300 tx1",
			e.FromUserID, e.ToUserID, e.Amount, e.Reason)
	}
	// Derived from the transaction, exactly as the live write derives it, so re-running the migration
	// inserts nothing and one delivery can never be journalled twice.
	if e.ID != "tv_tx1" {
		t.Errorf("entry id = %q, want tv_tx1", e.ID)
	}
}

// rawDB opens a connection that bypasses the migration runner entirely.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open(driverName, path+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func openAt(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func schemaOf(t *testing.T, db *DB) string {
	t.Helper()
	rows, err := db.db.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

// A fresh database is created directly from the baseline and records it.
func TestBaselineCreatesFreshSchema(t *testing.T) {
	db := openTestDB(t)
	applied, err := db.migrationApplied(baselineVersion)
	if err != nil || !applied {
		t.Fatalf("fresh database must record %s: applied=%v err=%v", baselineVersion, applied, err)
	}
	if _, err := db.db.Exec(`SELECT refresh_token FROM grants`); err == nil {
		t.Error("a fresh grants table must not carry the legacy refresh_token column")
	}
}

// A database at the end of the old chain is normalized and stamped, and its schema then matches a
// fresh one exactly — an upgraded kernel and a new one run on the same bytes.
func TestBaselineUpgradesLegacyChainToAnIdenticalSchema(t *testing.T) {
	path := legacyChainDB(t)
	upgraded := openAt(t, path)
	applied, err := db2applied(upgraded)
	if err != nil || !applied {
		t.Fatalf("legacy database must be stamped: applied=%v err=%v", applied, err)
	}
	if _, err := upgraded.db.Exec(`SELECT refresh_token FROM grants`); err == nil {
		t.Error("normalization must drop the legacy refresh_token column")
	}
	if got, want := schemaOf(t, upgraded), schemaOf(t, openTestDB(t)); got != want {
		t.Errorf("upgraded schema differs from a fresh one:\n--- upgraded ---\n%s\n--- fresh ---\n%s", got, want)
	}
	rows, err := upgraded.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("PRAGMA foreign_key_check reported violations after the upgrade")
	}
	// Idempotent: a second boot is state 1 and changes nothing.
	upgraded.Close()
	again := openAt(t, path)
	if got, want := schemaOf(t, again), schemaOf(t, openTestDB(t)); got != want {
		t.Error("a second boot must be a no-op")
	}
}

func db2applied(db *DB) (bool, error) { return db.migrationApplied(baselineVersion) }

// A grant still holding a legacy token means the previous release's backfill never completed.
// Refuse rather than drop the column — that would destroy a recoverable credential.
func TestBaselineRefusesUnbackfilledLegacyToken(t *testing.T) {
	path := legacyChainDB(t)
	raw := rawDB(t, path)
	u, a := uuid.New().String(), uuid.New().String()
	now := timeToStr(time.Now().UTC())
	if _, err := raw.Exec(`INSERT INTO accounts (id,handle,description,password_hash,available,locked,created_at,updated_at) VALUES (?,?,'','',0,0,?,?)`,
		u, "legacy-holder", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO actions (id,owner_user_id,name,kind,active,visibility,price,description,created_at,updated_at) VALUES (?,?,?,'http',1,'private',0,'',?,?)`,
		a, u, "svc", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`INSERT INTO grants (id,grantor_user_id,action_id,connection_id,refresh_token,created_at) VALUES (?,?,?,NULL,?,?)`,
		uuid.New().String(), u, a, "sealed", now); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "legacy token") {
		t.Fatalf("expected a refusal naming the legacy token, got %v", err)
	}
}

// A ledger that is neither empty nor exactly the old chain is an unknown state: refuse, rather
// than stamp a baseline over a schema we cannot vouch for.
func TestBaselineRefusesPartialHistory(t *testing.T) {
	path := legacyChainDB(t)
	raw := rawDB(t, path)
	if _, err := raw.Exec(`DELETE FROM schema_migrations WHERE version=?`, "042_discovery_effect"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "unsupported migration state") {
		t.Fatalf("expected an unsupported-state refusal, got %v", err)
	}
}

// An empty ledger over existing tables is a damaged or foreign database; creating the baseline
// over it would fail half-way through.
func TestBaselineRefusesTablesWithoutHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "damaged.db")
	db := openAt(t, path)
	if _, err := db.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "unknown schema") {
		t.Fatalf("expected a refusal to baseline over an unknown schema, got %v", err)
	}
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

func newUser(handle string, balance int64) *kernel.Account {
	return &kernel.Account{
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
func newPeer(t *testing.T, db *DB, handle, key string, available, locked int64, createdAt time.Time) *kernel.Account {
	t.Helper()
	u := newUser(handle, available)
	// A kernel account holds no session credential at all (§3 CHECK): no handle, no password, no
	// recovery key. It is named by its kernel's petname and authenticates by federation signature.
	u.Handle, u.PasswordHash, u.RecoveryPublicKey = "", "", ""
	u.Locked = locked
	u.KernelPublicKey = key
	u.CreatedAt = createdAt
	u.UpdatedAt = createdAt
	// The kernel row must exist first — accounts.kernel_public_key is a restrictive foreign key.
	if err := db.UpsertKernel(context.Background(), key, handle, "", "", "", createdAt); err != nil {
		t.Fatalf("upsert kernel %s: %v", handle, err)
	}
	if _, err := db.BindPetname(context.Background(), key, handle, false); err != nil {
		t.Fatalf("bind petname %s: %v", handle, err)
	}
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

// TestDBErrClassifiesUniqueness: dbErr is the single funnel for every store error, so it is where a
// caller's own uniqueness collision is separated from a broken internal invariant. A collision on a
// caller-supplied key (a handle, an action owner/name) is ErrInvalidInput and carries no SQL or
// table text; a kernel-minted key (transactions.trace_id) and every CHECK/FK violation stay
// ErrInternal, because reaching those means a bug, not bad input. Unlisted keys fail closed.
func TestDBErrClassifiesUniqueness(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	alice := newUser("alice", 0)
	if err := db.CreateUser(ctx, alice); err != nil {
		t.Fatal(err)
	}

	// Caller-supplied keys: the caller picked the colliding value and can pick another.
	handleErr := db.CreateUser(ctx, newUser("alice", 0))
	if !errors.Is(handleErr, kernel.ErrInvalidInput) {
		t.Errorf("duplicate handle: got %v, want ErrInvalidInput", handleErr)
	}
	for _, leak := range []string{"constraint", "accounts.", "UNIQUE", "2067"} {
		if strings.Contains(handleErr.Error(), leak) {
			t.Errorf("duplicate handle message leaks %q: %v", leak, handleErr)
		}
	}
	if err := db.CreateAction(ctx, newAction(alice.ID, "dup", 0, true)); err != nil {
		t.Fatal(err)
	}
	if nameErr := db.CreateAction(ctx, newAction(alice.ID, "dup", 0, true)); !errors.Is(nameErr, kernel.ErrInvalidInput) {
		t.Errorf("duplicate owner/name: got %v, want ErrInvalidInput", nameErr)
	}

	// A kernel-minted key: one receipt per transaction is a settlement invariant (§11), so a second
	// insert on the same tx is double-settlement — a 500, never the caller's fault. (The sibling
	// invariant, one transaction per trace, has NO enforced index: migration 008's
	// "CREATE UNIQUE INDEX IF NOT EXISTS idx_transactions_trace" is a no-op because 001 already
	// created a non-unique index of that name. Pre-existing, unrelated to this classifier.)
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO transactions (id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,
		   target_user_id,action_id,status,started_at,ended_at)
		 VALUES ('t1','p1','trace-1','',?,?,?,'a','success','','')`,
		alice.ID, alice.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO receipts (id,issuer_user_id,tx_id,trace_id,action_id,status,created_at)
		 VALUES ('r1',?,'t1','trace-1','a','success','')`, alice.ID); err != nil {
		t.Fatal(err)
	}
	_, rawErr := db.db.ExecContext(ctx,
		`INSERT INTO receipts (id,issuer_user_id,tx_id,trace_id,action_id,status,created_at)
		 VALUES ('r2',?,'t1','trace-1','a','success','')`, alice.ID)
	if got := dbErr(rawErr, "insert receipt"); !errors.Is(got, kernel.ErrInternal) {
		t.Errorf("duplicate receipt tx_id: got %v, want ErrInternal", got)
	}

	// A CHECK violation is an invariant breach too (the guarded UPDATE is the real path, §6).
	_, checkErr := db.db.ExecContext(ctx, `UPDATE accounts SET available=-1 WHERE id=?`, alice.ID)
	if got := dbErr(checkErr, "debit"); !errors.Is(got, kernel.ErrInternal) {
		t.Errorf("CHECK violation: got %v, want ErrInternal", got)
	}
}

// ---- User CRUD ----

func TestUserCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("alice", 0)
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

	// ReadUserByHandle canonicalizes its argument, so both "alice" and "alice" resolve.
	for _, h := range []string{"alice", "alice"} {
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
	if err := db.RenameUser(ctx, u.ID, "alice2"); err != nil {
		t.Fatalf("RenameUser: %v", err)
	}
	if got, err := db.ReadUserByHandle(ctx, "alice2"); err != nil || got.ID != u.ID {
		t.Errorf("renamed handle does not resolve: %v", err)
	}
	if _, err := db.ReadUserByHandle(ctx, "alice"); err == nil {
		t.Error("old handle should be free after rename")
	}
	// The UNIQUE constraint is the backstop against a colliding rename.
	other := newUser("carol", 0)
	if err := db.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := db.RenameUser(ctx, other.ID, "alice2"); err == nil {
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

	a := newUser("alice", 0)
	if err := db.CreateUser(ctx, a); err != nil {
		t.Fatalf("create @alice: %v", err)
	}

	// The rebuild kept handle uniqueness.
	if err := db.CreateUser(ctx, newUser("alice", 0)); err == nil {
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
	u := newUser("stamp", 0)
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

// ---- Action CRUD ----

func TestActionCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("owner", 0)
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

	owner := newUser("hd-owner", 0)
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

	owner := newUser("owner", 0)
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

	owner := newUser("owner", 0)
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

	user := newUser("alice", 1000)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}

	if err := db.BeginRun(ctx, p, tr, user.ID, 400, 0, 0); err != nil {
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
	if err := db.BeginRun(ctx, p2, tr2, user.ID, 9999, 0, 0); err == nil {
		t.Error("expected error for insufficient funds")
	}
}

// TestBeginRunGlobalExposure exercises the §13 admission guard: ordinary accounts stay prepaid, a
// peer may draw negative only within the GLOBAL cap X, and — critically — a second peer identity
// cannot use exposure the first already consumed (Sybil-proof: one X across all peers).
func TestBeginRunGlobalExposure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const X = 100
	now := time.Now().UTC()
	run := func(u *kernel.Account, price, exposureMax int64) error {
		p := newProcess(u.ID)
		tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: now}
		return db.BeginRun(ctx, p, tr, u.ID, price, 0, exposureMax)
	}

	// Ordinary account with no balance cannot run a priced action (prepaid-only), regardless of X.
	local := newUser("local", 0)
	_ = db.CreateUser(ctx, local)
	if err := run(local, 1, X); err == nil {
		t.Error("ordinary account with no balance ran a priced action")
	}

	// A peer may draw negative up to the global cap X.
	peerA := newPeer(t, db, "peerA", "keyA", 0, 0, now)
	if err := run(peerA, 60, X); err != nil {
		t.Fatalf("peer within X should run: %v", err)
	}
	if u, _ := db.ReadUser(ctx, peerA.ID); u.Available != -60 {
		t.Errorf("peerA available: got %d, want -60", u.Available)
	}

	// Sybil: a second peer identity cannot use the exposure the first consumed. Global gross is 60;
	// a 60 draw would push it to 120 > X=100, so it is refused — proving X is not per-peer.
	peerB := newPeer(t, db, "peerB", "keyB", 0, 0, now)
	if err := run(peerB, 60, X); err == nil {
		t.Error("second peer identity jointly exceeded the global cap X (Sybil)")
	}
	// A draw that keeps global gross ≤ X is admitted (60 + 40 = 100).
	if err := run(peerB, 40, X); err != nil {
		t.Fatalf("second peer within remaining headroom should run: %v", err)
	}

	// X=0 ⇒ prepaid-only: a peer with no balance cannot draw negative.
	peerC := newPeer(t, db, "peerC", "keyC", 0, 0, now)
	if err := run(peerC, 1, 0); err == nil {
		t.Error("X=0 should forbid any peer credit draw")
	}
	// A prepaid peer is immune to X: with balance ≥ price it always runs.
	peerD := newPeer(t, db, "peerD", "keyD", 50, 0, now)
	if err := run(peerD, 50, 0); err != nil {
		t.Fatalf("prepaid peer should run regardless of X: %v", err)
	}
}

// TestCommitSettlement verifies the §13 rail-anchored residual-settlement store ops: a pay outcome
// moves no balance (the debt stays, pending), a clear outcome extinguishes it, the cash finalization
// is the only non-conservative move, insufficient debtor reserve rolls back with no partial writes,
// and every path is idempotent.
func TestCommitSettlement(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sys := newUser("sys", 100) // operator reserve (accumulated fees) absorbs write-off variance
	if err := db.CreateUser(ctx, sys); err != nil {
		t.Fatal(err)
	}
	const d, Q = int64(3), int64(10)

	// (6) Clear outcome (creditor: peer owes us d): clear +d on the row, sys absorbs −d, no cash.
	pc := newPeer(t, db, "peerClear", "pkClear", -d, 0, now)
	sys0, _ := db.ReadUser(ctx, sys.ID)
	if _, err := db.CommitSettlement(ctx, "sid-clear", pc.ID, sys.ID, d, -d, d, `{"outcome":"clear"}`); err != nil {
		t.Fatal(err)
	}
	if u, _ := db.ReadUser(ctx, pc.ID); u.Available != 0 {
		t.Errorf("clear: peer row got %d, want 0", u.Available)
	}
	if u, _ := db.ReadUser(ctx, sys.ID); u.Available != sys0.Available-d {
		t.Errorf("clear: sys delta got %d, want %d", u.Available-sys0.Available, -d)
	}

	// (1) Pay outcome: NO balance change — the debt stays, and G is unchanged. Record stored (replay).
	pp := newPeer(t, db, "peerPay", "pkPay", -d, 0, now)
	if _, err := db.CommitSettlement(ctx, "sid-pay", pp.ID, sys.ID, 0, 0, d, `{"outcome":"pay"}`); err != nil {
		t.Fatal(err)
	}
	if u, _ := db.ReadUser(ctx, pp.ID); u.Available != -d {
		t.Errorf("pay: peer row moved to %d, want unchanged %d", u.Available, -d)
	}
	if pending, _ := db.HasPendingSettlement(ctx, pp.ID); !pending {
		t.Error("pay: expected HasPendingSettlement true")
	}
	// (5) Replay returns the first record with no second application (anti-grinding).
	if stored, _ := db.CommitSettlement(ctx, "sid-pay", pp.ID, sys.ID, 0, 0, d, `{"outcome":"clear"}`); stored != `{"outcome":"pay"}` {
		t.Errorf("pay replay: got %q, want the stored pay record", stored)
	}

	// (3) The rail closes it: the debt stays on the books until a transfer row for that settlement
	// reaches a final state, which is the only thing that can end a paid outcome now.
	if err := db.CreateRailTransfer(ctx, &kernel.RailTransfer{
		ID: "sid-pay", Kind: kernel.RailKindClaim, Party: pp.ID, Amount: Q, Credit: d,
		Status: kernel.RailStatusAnnounced, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if pending, _ := db.HasPendingSettlement(ctx, pp.ID); !pending {
		t.Error("a settlement whose payment has not arrived is still pending")
	}
	if err := db.MarkRailTransfer(ctx, "sid-pay", kernel.RailStatusCredited); err != nil {
		t.Fatal(err)
	}
	if pending, _ := db.HasPendingSettlement(ctx, pp.ID); pending {
		t.Error("a settlement the rail has closed is no longer pending")
	}

	poor := newUser("poorSys", 0)
	_ = db.CreateUser(ctx, poor)

	// Creditor clear against an empty reserve refuses the same way: ErrInsufficientFunds, no
	// message SQL, full rollback, and the same commit succeeds once the reserve exists.
	pcr := newPeer(t, db, "peerClearPoor", "pkClearPoor", -d, 0, now)
	_, cerr := db.CommitSettlement(ctx, "sid-poor", pcr.ID, poor.ID, d, -d, d, `{"outcome":"clear"}`)
	if !errors.Is(cerr, kernel.ErrInsufficientFunds) {
		t.Errorf("clear with empty creditor reserve: got %v, want ErrInsufficientFunds", cerr)
	}
	if cerr != nil && strings.Contains(cerr.Error(), "CHECK") {
		t.Errorf("reserve refusal leaks constraint text: %v", cerr)
	}
	if u, _ := db.ReadUser(ctx, pcr.ID); u.Available != -d {
		t.Errorf("failed clear left a partial write on the row: got %d, want %d", u.Available, -d)
	}
	if u, _ := db.ReadUser(ctx, poor.ID); u.Available != 0 {
		t.Errorf("failed clear moved the poor reserve: got %d, want 0", u.Available)
	}
	if err := db.CreateLedgerEntry(ctx, &kernel.LedgerEntry{ID: uuid.New().String(), OperatorUserID: sys.ID, ToUserID: poor.ID, Amount: d, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitSettlement(ctx, "sid-poor", pcr.ID, poor.ID, d, -d, d, `{"outcome":"clear"}`); err != nil {
		t.Errorf("clear after funding the reserve should succeed: %v", err)
	}
	if u, _ := db.ReadUser(ctx, pcr.ID); u.Available != 0 {
		t.Errorf("funded clear: peer row got %d, want 0", u.Available)
	}

	// A draw that came out payable holds the debt until it is paid: no other outcome for that peer
	// commits meanwhile, whatever it says, and the rule lives in the commit itself so that two draws
	// finishing together cannot both slip past it.
	ph := newPeer(t, db, "peerHeld", "pkHeld", -d, 0, now)
	if _, err := db.CommitSettlement(ctx, "sid-h1", ph.ID, sys.ID, 0, 0, d, `{"outcome":"pay"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitSettlement(ctx, "sid-h2", ph.ID, sys.ID, d, -d, d, `{"outcome":"clear"}`); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("a clear while a payable draw stands: got %v, want ErrInvalidState", err)
	}
	if u, _ := db.ReadUser(ctx, ph.ID); u.Available != -d {
		t.Errorf("the held debt moved: %d, want %d", u.Available, -d)
	}

	// Two settlements drawn on one debt: the first clears it, and the second finds nothing left to
	// clear. Without that the row would be credited twice for one debt and the creditor would lose
	// it twice over.
	pdd := newPeer(t, db, "peerTwice", "pkTwice", -d, 0, now)
	if _, err := db.CommitSettlement(ctx, "sid-t1", pdd.ID, sys.ID, d, -d, d, `{"outcome":"clear"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitSettlement(ctx, "sid-t2", pdd.ID, sys.ID, d, -d, d, `{"outcome":"clear"}`); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("clearing a debt that is already gone: got %v, want ErrInvalidState", err)
	}
	if u, _ := db.ReadUser(ctx, pdd.ID); u.Available != 0 {
		t.Errorf("one debt cleared twice left the row at %d, want 0", u.Available)
	}

	// The conservation guard rejects a non-conservative outcome record.
	if _, err := db.CommitSettlement(ctx, "sid-bad", pc.ID, sys.ID, d, 0, d, `{"outcome":"clear"}`); err == nil {
		t.Error("CommitSettlement must reject dClear+variance != 0")
	}
}

func TestBeginRunAndSubcall(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("alice", 500)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}

	// BeginRun atomically creates process+root trace and debits user.
	if err := db.BeginRun(ctx, p, root, user.ID, 500, 0, 0); err != nil {
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

	payer := newUser("payer", 1000)
	target := newUser("target", 0)
	fee := newUser("fee", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, target)
	_ = db.CreateUser(ctx, fee)

	p := newProcess(payer.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, payer.ID, 100, 0, 0); err != nil {
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

// TestTransferEffectFundsFromCaller is the core value-wallet regression (§13): a composed transfer
// (an action subcalls sys/transfer) funds the delivered VALUE from the immediate caller C's own
// balance — NOT the process budget (owner P) nor the parent trace. A subcall trace carrying a value
// reserve locks it from C at BeginSubcall and settles it to the beneficiary at CommitCall, leaving P
// and the parent trace's execution budget untouched.
func TestTransferEffectFundsFromCaller(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	P := newUser("proc-owner", 1000) // process owner: funds execution only
	C := newUser("caller", 500)      // immediate caller: funds the value
	B := newUser("beneficiary", 0)
	sys := newUser("sys", 0)
	for _, u := range []*kernel.Account{P, C, B, sys} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	// Process owned by P with a funded parent trace (execution budget 200).
	p := newProcess(P.ID)
	parent := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CallerUserID: P.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, parent, P.ID, 200, 0, 0); err != nil {
		t.Fatal(err)
	}

	// Composed subcall: caller C subcalls a value-bearing action delivering 100 to B. Execution price
	// 0 (funded from the parent trace); the value reserve 100 is locked from C's own balance.
	sub := &kernel.Trace{
		ID: uuid.New().String(), ProcessID: p.ID, CallerUserID: C.ID,
		Value: 100, ValueTo: B.ID, CreatedAt: time.Now().UTC(),
	}
	if err := db.BeginSubcall(ctx, parent.ID, sub, 0); err != nil {
		t.Fatal(err)
	}
	// Reserve is locked from C, not P or the parent trace.
	if cu, _ := db.ReadUser(ctx, C.ID); cu.Available != 400 || cu.Locked != 100 {
		t.Fatalf("after admission C: got available=%d locked=%d, want 400/100", cu.Available, cu.Locked)
	}
	if pu, _ := db.ReadUser(ctx, P.ID); pu.Available != 800 {
		t.Errorf("P.available disturbed by value reserve: got %d, want 800 (200 execution only)", pu.Available)
	}

	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: sub.ID, ParentTraceID: parent.ID,
		OwnerUserID: P.ID, CallerUserID: C.ID, TargetUserID: sys.ID, ActionID: "a1",
		Status: kernel.TxSuccess, Gross: 0, Net: 0, Fee: 0, StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: P.ID, TxID: tx.ID, TraceID: sub.ID, ActionID: "a1",
		ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxSuccess, Value: 100, ValueTo: B.ID, CreatedAt: time.Now().UTC(),
	}
	if err := db.CommitCall(ctx, tx, receipt, sub.ID, parent.ID, kernel.CallerTrace, sys.ID, sys.ID, 0, 0, nil, "", ""); err != nil {
		t.Fatal(err)
	}

	// Value delivered to B from C; C down exactly 100; P's execution budget intact; local caller pays
	// no value premium (sys unchanged).
	if cu, _ := db.ReadUser(ctx, C.ID); cu.Available != 400 || cu.Locked != 0 {
		t.Errorf("after settle C: got available=%d locked=%d, want 400/0", cu.Available, cu.Locked)
	}
	if bu, _ := db.ReadUser(ctx, B.ID); bu.Available != 100 {
		t.Errorf("beneficiary credited: got %d, want 100", bu.Available)
	}
	if su, _ := db.ReadUser(ctx, sys.ID); su.Available != 0 {
		t.Errorf("local transfer must be untaxed: sys got %d, want 0", su.Available)
	}
	if pu, _ := db.ReadUser(ctx, P.ID); pu.Available != 800 || pu.Locked != 200 {
		t.Errorf("P untouched by value channel: got available=%d locked=%d, want 800/200", pu.Available, pu.Locked)
	}

	// The delivery is journalled where every balance movement between two users is. Without it the
	// beneficiary — no party to the transaction (§11) — would see credit arrive with nothing to read.
	entries, err := db.ListLedgerByUser(ctx, B.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListLedgerByUser: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("beneficiary ledger: got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.FromUserID != C.ID || e.ToUserID != B.ID || e.Amount != 100 {
		t.Errorf("entry = from %s to %s amount %d, want from C to B amount 100", e.FromUserID, e.ToUserID, e.Amount)
	}
	if e.OperatorUserID != C.ID {
		t.Errorf("authorizer = %s, want the immediate caller C (%s)", e.OperatorUserID, C.ID)
	}
	if e.Reason != tx.ID {
		t.Errorf("reason = %q, want the settling transaction id %q", e.Reason, tx.ID)
	}
	if !e.CreatedAt.Equal(receipt.CreatedAt) {
		t.Errorf("created_at = %v, want the receipt's settlement time %v", e.CreatedAt, receipt.CreatedAt)
	}
	// The sender sees the same one entry, so both parties can reconstruct the movement.
	if sent, _ := db.ListLedgerByUser(ctx, C.ID, 10, 0); len(sent) != 1 || sent[0].ID != e.ID {
		t.Errorf("sender ledger: got %d entries, want the same one", len(sent))
	}
}

// TestTransferEffectRefundedOnFailure: a failed composed transfer returns the whole value reserve to
// the caller C — nothing delivered, no premium taken (§13, all-or-nothing).
func TestTransferEffectRefundedOnFailure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	P := newUser("proc-owner", 1000)
	C := newUser("caller", 500)
	B := newUser("beneficiary", 0)
	sys := newUser("sys", 0)
	for _, u := range []*kernel.Account{P, C, B, sys} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	p := newProcess(P.ID)
	parent := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CallerUserID: P.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, parent, P.ID, 200, 0, 0); err != nil {
		t.Fatal(err)
	}
	sub := &kernel.Trace{
		ID: uuid.New().String(), ProcessID: p.ID, CallerUserID: C.ID,
		Value: 100, ValueTo: B.ID, CreatedAt: time.Now().UTC(),
	}
	if err := db.BeginSubcall(ctx, parent.ID, sub, 0); err != nil {
		t.Fatal(err)
	}
	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: sub.ID, ParentTraceID: parent.ID,
		OwnerUserID: P.ID, CallerUserID: C.ID, ActionID: "a1", Status: kernel.TxFailure,
		Gross: 0, Reason: "boom", StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	buildReceipt := func(refund int64) (*kernel.Receipt, error) {
		return &kernel.Receipt{
			ID: uuid.New().String(), IssuerUserID: P.ID, TxID: tx.ID, TraceID: sub.ID, ActionID: "a1",
			ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxFailure, CreatedAt: time.Now().UTC(),
		}, nil
	}
	if err := db.CommitFailedCall(ctx, tx, buildReceipt, sub.ID, parent.ID, kernel.CallerTrace, sys.ID, 0, nil, "", "execution_failed", ""); err != nil {
		t.Fatal(err)
	}
	if cu, _ := db.ReadUser(ctx, C.ID); cu.Available != 500 || cu.Locked != 0 {
		t.Errorf("failed transfer must refund C in full: got available=%d locked=%d, want 500/0", cu.Available, cu.Locked)
	}
	if bu, _ := db.ReadUser(ctx, B.ID); bu.Available != 0 {
		t.Errorf("failed transfer delivered value: beneficiary got %d, want 0", bu.Available)
	}
	// Nothing moved between the two, so the journal records nothing: the ledger holds completed
	// movements, and the refund returns C's own reserve to C.
	if entries, _ := db.ListLedgerByUser(ctx, B.ID, 10, 0); len(entries) != 0 {
		t.Errorf("failed transfer wrote %d ledger entries, want 0", len(entries))
	}
	if entries, _ := db.ListLedgerByUser(ctx, C.ID, 10, 0); len(entries) != 0 {
		t.Errorf("failed transfer wrote %d ledger entries for the sender, want 0", len(entries))
	}
}

// CommitFailedCall (the recovery settle op) fully restores the balance.
func TestPremiumReserveReleasedFromTrace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sys := newUser("sys", 0)
	_ = db.CreateUser(ctx, sys)
	const price, reserve = int64(100), int64(5)
	peer := newPeer(t, db, "peer", "pk", price+reserve, 0, now)

	p := newProcess(peer.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, PremiumBPS: 500, PremiumParked: reserve, CreatedAt: now}
	if err := db.BeginRun(ctx, p, root, peer.ID, price, reserve, 0); err != nil {
		t.Fatal(err)
	}
	if u, _ := db.ReadUser(ctx, peer.ID); u.Available != 0 || u.Locked != price+reserve {
		t.Fatalf("after BeginRun: available=%d locked=%d, want 0/%d", u.Available, u.Locked, price+reserve)
	}

	// Settle as a failure with a full refund — mirrors recoverTrace, which passes NO premium reserve
	// (it rebuilds the request fresh); the store must read the reserve from the trace row alone.
	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID, OwnerUserID: peer.ID,
		CallerUserID: peer.ID, TargetUserID: peer.ID, ActionID: "dummy",
		Status: kernel.TxFailure, Gross: price, Reason: "recovered", ArgsJSON: []byte("{}"), ReplyJSON: []byte("null"),
		StartedAt: now, EndedAt: now,
	}
	buildFn := func(refund int64) (*kernel.Receipt, error) {
		charge := price - refund // full refund ⇒ charge 0 ⇒ premium 0
		return &kernel.Receipt{ID: uuid.New().String(), IssuerUserID: sys.ID, TxID: tx.ID, TraceID: root.ID, ActionID: "dummy", Status: kernel.TxFailure, Charge: charge, CreatedAt: now}, nil
	}
	if err := db.CommitFailedCall(ctx, tx, buildFn, root.ID, p.ID, kernel.CallerProcess, sys.ID, price, nil, "", "recovered", ""); err != nil {
		t.Fatal(err)
	}
	// Reserve fully released: locked back to 0, available restored — no leak.
	if u, _ := db.ReadUser(ctx, peer.ID); u.Locked != 0 || u.Available != price+reserve {
		t.Errorf("after recovery: available=%d locked=%d, want %d/0 (reserve leaked in locked?)", u.Available, u.Locked, price+reserve)
	}
}

// TestCommitFailedCallRecordsRefund: a failed local call records what actually came back on the
// transaction the payer audits (§3 D4). The local law is not the remote identity gross−net−fee — a
// failure charges no fee or net, yet settled descendants stay paid — so the field carries the
// unspent allocation, which is the whole gross when nothing settled beneath it.
func TestCommitFailedCallRecordsRefund(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("refund-alice", 1000)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 70, 0, 0); err != nil {
		t.Fatal(err)
	}
	failTx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID,
		OwnerUserID: user.ID, CallerUserID: user.ID, TargetUserID: user.ID,
		ActionID: "dummy", Status: kernel.TxFailure, Gross: 70, Reason: "execution_failed",
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	buildReceipt := func(refund int64) (*kernel.Receipt, error) {
		return &kernel.Receipt{
			ID: uuid.New().String(), IssuerUserID: user.ID, TxID: failTx.ID, TraceID: root.ID,
			ActionID: "dummy", Status: kernel.TxFailure, Gross: 70, Charge: 70 - refund,
			CreatedAt: time.Now().UTC(),
		}, nil
	}
	if err := db.CommitFailedCall(ctx, failTx, buildReceipt, root.ID, p.ID, kernel.CallerProcess, "", 70, nil, "", "execution_failed", ""); err != nil {
		t.Fatal(err)
	}

	// Read it back: the audit record, not the in-memory struct, is what the payer sees.
	stored, err := db.ReadTransaction(ctx, failTx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Refund != 70 {
		t.Errorf("failed call refund = %d, want 70 (the whole allocation came back)", stored.Refund)
	}
	if stored.Net != 0 || stored.Fee != 0 {
		t.Errorf("a failure pays nothing: net=%d fee=%d", stored.Net, stored.Fee)
	}
	// The reported refund must agree with the money that actually moved.
	u, _ := db.ReadUser(ctx, user.ID)
	if u.Available != 1000 {
		t.Errorf("owner available = %d, want the full 1000 back", u.Available)
	}
}

// TestCommitFailedCallRefundExcludesSettledDescendants: the local law is NOT gross−net−fee. A parent
// that fails after a child already settled keeps that child paid (U13), and the parent's own net and
// fee stay zero — so the remote identity would report the whole gross as returned. `refund` must be
// what the caller actually got back: the allocation still unspent when the rollup ran.
func TestCommitFailedCallRefundExcludesSettledDescendants(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	sys := newUser("refund-sys", 0)
	_ = db.CreateUser(ctx, sys)
	owner := newUser("refund-owner", 1000)
	_ = db.CreateUser(ctx, owner)
	provider := newUser("refund-provider", 0)
	_ = db.CreateUser(ctx, provider)

	// A root funded with 70 spends 30 on a child that settles successfully, then fails.
	p := newProcess(owner.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, owner.ID, 70, 0, 0); err != nil {
		t.Fatal(err)
	}
	child := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ActionOwnerID: provider.ID,
		CallerUserID: owner.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginSubcall(ctx, root.ID, child, 30); err != nil {
		t.Fatal(err)
	}
	okTx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: child.ID, ParentTraceID: root.ID,
		OwnerUserID: owner.ID, CallerUserID: owner.ID, TargetUserID: provider.ID, ActionID: "child",
		Status: kernel.TxSuccess, Gross: 30, Net: 24, Fee: 6,
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	okReceipt := &kernel.Receipt{ID: uuid.New().String(), IssuerUserID: sys.ID, TxID: okTx.ID,
		TraceID: child.ID, ActionID: "child", Status: kernel.TxSuccess, CreatedAt: time.Now().UTC()}
	if err := db.CommitCall(ctx, okTx, okReceipt, child.ID, root.ID, kernel.CallerTrace,
		provider.ID, sys.ID, 24, 6, nil, "", ""); err != nil {
		t.Fatal(err)
	}

	failTx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID,
		OwnerUserID: owner.ID, CallerUserID: owner.ID, TargetUserID: owner.ID, ActionID: "root",
		Status: kernel.TxFailure, Gross: 70, Reason: "execution_failed",
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	buildReceipt := func(refund int64) (*kernel.Receipt, error) {
		return &kernel.Receipt{ID: uuid.New().String(), IssuerUserID: sys.ID, TxID: failTx.ID,
			TraceID: root.ID, ActionID: "root", Status: kernel.TxFailure, Gross: 70,
			Charge: 70 - refund, CreatedAt: time.Now().UTC()}, nil
	}
	if err := db.CommitFailedCall(ctx, failTx, buildReceipt, root.ID, p.ID, kernel.CallerProcess,
		sys.ID, 70, nil, "", "execution_failed", ""); err != nil {
		t.Fatal(err)
	}

	stored, err := db.ReadTransaction(ctx, failTx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Refund != 40 {
		t.Errorf("refund = %d, want 40 (gross 70 − the settled child's 30)", stored.Refund)
	}
	if stored.Gross-stored.Net-stored.Fee == stored.Refund {
		t.Error("refund must not equal gross−net−fee here; that identity is the remote one (P7)")
	}
	// The child stays paid and the owner is out exactly what the child cost.
	if pu, _ := db.ReadUser(ctx, provider.ID); pu.Available != 24 {
		t.Errorf("settled descendant must stay paid: provider has %d, want 24", pu.Available)
	}
	if u, _ := db.ReadUser(ctx, owner.ID); u.Available != 970 {
		t.Errorf("owner available = %d, want 970 (1000 − the settled 30)", u.Available)
	}
}

func TestEndProcess(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("alice", 1000)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 600, 0, 0); err != nil {
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
	if err := db.CommitFailedCall(ctx, failTx, buildReceipt, root.ID, p.ID, kernel.CallerProcess, "", 600, nil, "", "failure", ""); err != nil {
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

	user := newUser("alice-locked", 500)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	// BeginRun creates process (locked=500) + root trace (available=500).
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 500, 0, 0); err != nil {
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

	user := newUser("ep-running", 1000)
	_ = db.CreateUser(ctx, user)
	caller := newUser("ep-running-caller", 0)
	_ = db.CreateUser(ctx, caller)

	p := newProcess(user.ID)
	// BeginRun: user.locked=50, root trace.available=50.
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 50, 0, 0); err != nil {
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

	user := newUser("ep-done", 1000)
	_ = db.CreateUser(ctx, user)
	caller := newUser("ep-done-caller", 0)
	_ = db.CreateUser(ctx, caller)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 50, 0, 0); err != nil {
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
	if err := db.BeginStepCall(ctx, step.ID, ct, 0); err != nil {
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

	user := newUser("bsc-guard", 1000)
	_ = db.CreateUser(ctx, user)
	caller := newUser("bsc-guard-caller", 0)
	_ = db.CreateUser(ctx, caller)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 50, 0, 0); err != nil {
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
	err := db.BeginStepCall(ctx, step.ID, ct, 0)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("BeginStepCall with broken park: got %v, want ErrInvalidState", err)
	}
}

// ---- Stats ----

func TestStats(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("owner", 0)
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

	payer := newUser("payer-inc", 1000)
	target := newUser("target-inc", 0)
	fee := newUser("fee-inc", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, target)
	_ = db.CreateUser(ctx, fee)

	a := newAction(payer.ID, "/inc-svc", 100, true)
	_ = db.CreateAction(ctx, a)

	// Each call uses its own process so auto-close on the first doesn't block the second.
	makeRun := func(price int64) (*kernel.Process, *kernel.Trace) {
		pr := newProcess(payer.ID)
		tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: pr.ID, CreatedAt: time.Now().UTC()}
		if err := db.BeginRun(ctx, pr, tr, payer.ID, price, 0, 0); err != nil {
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

	user := newUser("alice", 200)
	_ = db.CreateUser(ctx, user)
	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 200, 0, 0); err != nil {
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

	user := newUser("dave", 0)
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

	user := newUser("eve", 0)
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

	owner := newUser("owner", 100)
	target := newUser("target", 0)
	feeUser := newUser("fee-crud", 0)
	_ = db.CreateUser(ctx, owner)
	_ = db.CreateUser(ctx, target)
	_ = db.CreateUser(ctx, feeUser)

	a := newAction(owner.ID, "/svc", 100, true)
	_ = db.CreateAction(ctx, a)

	p := newProcess(owner.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, owner.ID, 100, 0, 0); err != nil {
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

	u := newUser("lp-owner", 1000)
	other := newUser("lp-other", 0)
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
		if err := db.BeginRun(ctx, p, tr, ownerID, 0, 0, 0); err != nil {
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

	u := newUser("rt-user", 0)
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

	issuer := newUser("issuer", 0)
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

	rater := newUser("rater", 0)
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

	owner := newUser("owner", 1000)
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

	rater := newUser("rater", 0)
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
	rater2 := newUser("rater2", 0)
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

	owner := newUser("list-owner", 0)
	_ = db.CreateUser(ctx, owner)
	action := newAction(owner.ID, "/list-a", 0, true)
	_ = db.CreateAction(ctx, action)

	rater := newUser("list-rater", 0)
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

	user := newUser("ls-owner", 1000)
	_ = db.CreateUser(ctx, user)
	caller := newUser("ls-caller", 0)
	_ = db.CreateUser(ctx, caller)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 100, 0, 0); err != nil {
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

	owner := newUser("tx-party-owner", 0)   // seller
	caller := newUser("tx-party-caller", 0) // buyer
	other := newUser("tx-party-other", 0)   // non-party
	_ = db.CreateUser(ctx, owner)
	_ = db.CreateUser(ctx, caller)
	_ = db.CreateUser(ctx, other)
	action := newAction(owner.ID, "/party-tx", 0, true)
	_ = db.CreateAction(ctx, action)

	p := newProcess(caller.ID)
	{
		tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		if err := db.BeginRun(ctx, p, tr, caller.ID, 0, 0, 0); err != nil {
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
	distinctCaller := newUser("tx-party-distinct-caller", 0)
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

func TestReadAccountByKernelKey(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	newPeer(t, db, "remote", "ed25519pubkeyABC", 0, 0, time.Now().UTC())

	got, err := db.ReadAccountByKernelKey(ctx, "ed25519pubkeyABC")
	if err != nil {
		t.Fatalf("ReadAccountByKernelKey: %v", err)
	}
	if got.Handle != "" {
		t.Errorf("a kernel account holds no handle, got %q", got.Handle)
	}
	if got.KernelPublicKey != "ed25519pubkeyABC" {
		t.Errorf("KernelPublicKey: got %q", got.KernelPublicKey)
	}
	// Its name lives in the other namespace: the kernel's petname (§13).
	rk, err := db.ReadKernelByPetname(ctx, "remote")
	if err != nil || rk == nil || rk.PublicKey != "ed25519pubkeyABC" {
		t.Errorf("petname must resolve to the kernel: %+v, %v", rk, err)
	}

	// Unknown key returns ErrNotFound.
	if _, err := db.ReadAccountByKernelKey(ctx, "unknown-key"); err == nil {
		t.Error("expected error for unknown public key")
	}
}

// ---- Idempotency record tests ----

func TestIdempotencyStateMachine(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cp := newUser("cp-sm", 0)
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

// TestUpdateActionLifecycle: one commit carries the row, the stats reset, and the grant
// revocation, in every combination — an update can need both effects at once (§5).
func TestUpdateActionLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("lifecycle-owner", 0)
	_ = db.CreateUser(ctx, user)

	setup := func(name string) *kernel.Action {
		a := newAction(user.ID, name, 5, true)
		if err := db.CreateAction(ctx, a); err != nil {
			t.Fatal(err)
		}
		st := kernel.DefaultStats(a.ID)
		st.Uses, st.Successes = 3, 3
		if err := db.UpsertStats(ctx, st); err != nil {
			t.Fatal(err)
		}
		g := &kernel.Grant{ID: uuid.New().String(), GrantorUserID: user.ID, ActionID: a.ID, CreatedAt: time.Now().UTC()}
		if err := db.CreateOrReplaceGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
		return a
	}

	for _, tc := range []struct {
		name                     string
		resetStats, revokeGrants bool
	}{
		{"neither", false, false},
		{"stats only", true, false},
		{"grants only", false, true},
		{"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := setup("svc-" + strings.ReplaceAll(tc.name, " ", "-"))
			a.Description = "moved"
			if err := db.UpdateActionLifecycle(ctx, a, tc.resetStats, tc.revokeGrants); err != nil {
				t.Fatalf("UpdateActionLifecycle: %v", err)
			}
			got, _ := db.ReadAction(ctx, a.ID)
			if got.Description != "moved" {
				t.Errorf("row not written: description = %q", got.Description)
			}
			st, _ := db.ReadStats(ctx, a.ID)
			if tc.resetStats && st.Uses != 0 {
				t.Errorf("stats not reset: uses = %d", st.Uses)
			}
			if !tc.resetStats && st.Uses != 3 {
				t.Errorf("stats reset when they should stand: uses = %d", st.Uses)
			}
			_, err := db.ReadGrant(ctx, user.ID, a.ID)
			if tc.revokeGrants && !errors.Is(err, kernel.ErrNotFound) {
				t.Errorf("grant survived revocation: %v", err)
			}
			if !tc.revokeGrants && err != nil {
				t.Errorf("grant revoked when it should stand: %v", err)
			}
		})
	}
}

// TestDeleteActionAndGrants: a deleted action can never be called again, so no consent outlives it,
// and both go in one commit.
func TestDeleteActionAndGrants(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("delete-owner", 0)
	_ = db.CreateUser(ctx, user)
	a := newAction(user.ID, "doomed", 0, true)
	_ = db.CreateAction(ctx, a)
	g := &kernel.Grant{ID: uuid.New().String(), GrantorUserID: user.ID, ActionID: a.ID, CreatedAt: time.Now().UTC()}
	if err := db.CreateOrReplaceGrant(ctx, g); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteActionAndGrants(ctx, a.ID); err != nil {
		t.Fatalf("DeleteActionAndGrants: %v", err)
	}
	if got, _ := db.ReadAction(ctx, a.ID); got != nil {
		t.Error("action still readable after delete")
	}
	if _, err := db.ReadGrant(ctx, user.ID, a.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("grant survived the action it consented to: %v", err)
	}
}

func TestInitFirstBootConfigPreservesExisting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("sys", 0)
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

	payer := newUser("payer-fd", 1000)
	target := newUser("target-fd", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, target)

	p := newProcess(payer.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, payer.ID, 100, 0, 0); err != nil {
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

	payer := newUser("payer-idem", 1000)
	target := newUser("target-idem", 0)
	fee := newUser("fee-idem", 0)
	cp := newUser("cp-idem", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, target)
	_ = db.CreateUser(ctx, fee)
	_ = db.CreateUser(ctx, cp)

	p := newProcess(payer.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, payer.ID, 100, 0, 0); err != nil {
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

	payer := newUser("payer-idem2", 1000)
	cp := newUser("cp-idem2", 0)
	_ = db.CreateUser(ctx, payer)
	_ = db.CreateUser(ctx, cp)

	p := newProcess(payer.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, payer.ID, 100, 0, 0); err != nil {
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
	if err := db.CommitFailedCall(ctx, tx, buildFn, root.ID, p.ID, kernel.CallerProcess, "", 100, nil, rec.ID, "execution_failed", ""); err != nil {
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

	owner := newUser("owner-emb", 0)
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
	peer := newPeer(t, db, "key-peer", "somepubkey", 0, 0, time.Now().UTC())

	found, err := db.ReadAccountByKernelKey(ctx, "somepubkey")
	if err != nil {
		t.Fatalf("ReadAccountByKernelKey: %v", err)
	}
	if found.ID != peer.ID {
		t.Errorf("expected peer ID %s, got %s", peer.ID, found.ID)
	}
	if found.KernelPublicKey == "" || found.PasswordHash != "" {
		t.Errorf("kernel account should hold a key and no password, got key=%q hash=%q", found.KernelPublicKey, found.PasswordHash)
	}
}

func TestUpdateActionAndResetStats(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("stats-owner", 0)
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
	if err := db.UpdateActionLifecycle(ctx, a, true, false); err != nil {
		t.Fatalf("UpdateActionLifecycle: %v", err)
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

func TestDeactivateImportedIfHash(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("peer-owner", 0)
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	proxy := newAction(owner.ID, "peer/act", 10, true)
	proxy.Kind = kernel.KindRemoteProxy
	proxy.ArtifactHash = "hash-v1"
	if err := db.CreateAction(ctx, proxy); err != nil {
		t.Fatal(err)
	}

	// A stale-hash rejection (a re-resolve already moved the row to a new hash) is a no-op: the
	// refreshed row stays active (§13 rule C).
	if err := db.DeactivateImportedIfHash(ctx, proxy.ID, "hash-v0", time.Now().UTC()); err != nil {
		t.Fatalf("DeactivateImportedIfHash(stale): %v", err)
	}
	if r, _ := db.ReadAction(ctx, proxy.ID); !r.Active {
		t.Fatal("stale-hash deactivation must be a no-op")
	}
	// The dispatched hash still matches: deactivate.
	if err := db.DeactivateImportedIfHash(ctx, proxy.ID, "hash-v1", time.Now().UTC()); err != nil {
		t.Fatalf("DeactivateImportedIfHash(current): %v", err)
	}
	if r, _ := db.ReadAction(ctx, proxy.ID); r.Active {
		t.Fatal("current-hash deactivation must clear active")
	}
}

func TestBeginRunIsAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("beginrun-alice", 200)
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
	if err := db.BeginRun(ctx, p, tr, user.ID, 100, 0, 0); err != nil {
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
	if err := db.BeginRun(ctx, p2, tr2, user.ID, 9999, 0, 0); !errors.Is(err, kernel.ErrInsufficientFunds) {
		t.Fatalf("expected ErrInsufficientFunds, got %v", err)
	}
	if _, readErr := db.ReadProcess(ctx, p2.ID); !errors.Is(readErr, kernel.ErrNotFound) {
		t.Error("process should not exist after failed BeginRun")
	}
}

func TestListOrphanRunningStepsDistinguishesSettled(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("orphan-settled", 1000)
	_ = db.CreateUser(ctx, user)
	act := newAction(user.ID, "orphan-settled-act", 100, true)
	_ = db.CreateAction(ctx, act)

	// mkSetup: create process → root trace → step → completion trace via BeginStepCall.
	mkSetup := func(price int64) (*kernel.Step, *kernel.Trace) {
		p := newProcess(user.ID)
		root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		_ = db.BeginRun(ctx, p, root, user.ID, price, 0, 0)
		ptID := root.ID
		step := &kernel.Step{
			ID: uuid.New().String(), ParentTraceID: &ptID,
			RequiredCallerUserID: user.ID, ActionID: act.ID,
			Price: price, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
		}
		_ = db.CreateStep(ctx, step)
		ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
		_ = db.BeginStepCall(ctx, step.ID, ct, 0)
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

	user := newUser("unsettled-order", 200)
	_ = db.CreateUser(ctx, user)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: user.ID, CallerUserID: user.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 200, 0, 0); err != nil {
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

	user := newUser("unsettled-children", 300)
	_ = db.CreateUser(ctx, user)
	feeUser := newUser("fee-uc", 0)
	_ = db.CreateUser(ctx, feeUser)
	act := newAction(user.ID, "uc-act", 0, true)
	_ = db.CreateAction(ctx, act)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID,
		ActionOwnerID: user.ID, CallerUserID: user.ID, ActionID: act.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, user.ID, 200, 0, 0); err != nil {
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

	user := newUser("repark-desc-tx", 200)
	_ = db.CreateUser(ctx, user)
	feeUser := newUser("fee-rdtx", 0)
	_ = db.CreateUser(ctx, feeUser)
	act := newAction(user.ID, "repark-desc-tx-act", 100, true)
	_ = db.CreateAction(ctx, act)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	_ = db.BeginRun(ctx, p, root, user.ID, 100, 0, 0)
	ptID := root.ID
	step := &kernel.Step{
		ID: uuid.New().String(), ParentTraceID: &ptID,
		RequiredCallerUserID: user.ID, ActionID: act.ID,
		Price: 100, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
	}
	_ = db.CreateStep(ctx, step)
	ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	_ = db.BeginStepCall(ctx, step.ID, ct, 0)

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

	user := newUser("repark-nonempty", 200)
	_ = db.CreateUser(ctx, user)
	act := newAction(user.ID, "repark-nonempty-act", 100, true)
	_ = db.CreateAction(ctx, act)

	p := newProcess(user.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	_ = db.BeginRun(ctx, p, root, user.ID, 100, 0, 0)
	ptID := root.ID
	step := &kernel.Step{
		ID: uuid.New().String(), ParentTraceID: &ptID,
		RequiredCallerUserID: user.ID, ActionID: act.ID,
		Price: 100, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
	}
	_ = db.CreateStep(ctx, step)
	ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	_ = db.BeginStepCall(ctx, step.ID, ct, 0)

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

// ---- Peer retention purge (§13) ----

func TestPurgePeerCascade(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	peer := newPeer(t, db, "peerP", "peerkeyAAA", 0, 0, time.Now().UTC())

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
	}

	// A discovered_kernels row about the peer (must be deleted) and one about another kernel (must
	// survive — it is information about a different peer).
	now := time.Now().UTC()
	if err := db.UpsertKernel(ctx, "peerkeyAAA", "peerP", "", "", "", now); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertKernel(ctx, "otherkeyBBB", "other", "", "", "", now); err != nil {
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
	if n := count(`SELECT COUNT(*) FROM kernels WHERE public_key=?`, "peerkeyAAA"); n != 0 {
		t.Errorf("discovered_kernels(peer) after purge = %d, want 0", n)
	}
	if n := count(`SELECT COUNT(*) FROM kernels WHERE public_key=?`, "otherkeyBBB"); n != 1 {
		t.Errorf("discovered_kernels(other) after purge = %d, want 1 (preserved)", n)
	}
	if _, err := db.ReadTransaction(ctx, txID); err != nil {
		t.Errorf("ledger transaction must survive purge: %v", err)
	}
	u, err := db.ReadUser(ctx, peer.ID)
	if err != nil {
		t.Fatalf("peer user must remain as ledger anchor: %v", err)
	}
	if u.KernelPublicKey != "" {
		t.Errorf("account→kernel link must be cleared, got %q", u.KernelPublicKey)
	}
}

// TestPurgeStaleDiscovery evicts directory-only discovered kernels stale past the cutoff, with their
// discovery docs, FTS mirror, and evidence — while sparing a fresh one and a peer-backed one (§13).
func TestPurgeStaleDiscovery(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	fresh := time.Now().UTC()
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)

	// A stale never-peer kernel with a discovery doc and an evidence row (both must be evicted).
	if err := db.UpsertKernel(ctx, "staleKey", "stale", "", "", "", old); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceDiscoveryDocs(ctx, "staleKey", []*kernel.DiscoveryDoc{{KernelPublicKey: "staleKey", ActionID: "sa1", Name: "svc", Description: "d", ObservedAt: old}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEvidence(ctx, &kernel.EvidenceRow{IssuerPublicKey: "staleKey", ReceiptHash: "rh1", SubjectKernelPublicKey: "staleKey", SubjectActionID: "sa1", EvidenceReceiptJSON: "{}", ReceiptCreatedAt: old, EffectiveAt: old, ObservedAt: old}); err != nil {
		t.Fatal(err)
	}
	// A fresh never-peer kernel survives.
	if err := db.UpsertKernel(ctx, "freshKey", "fresh", "", "", "", fresh); err != nil {
		t.Fatal(err)
	}
	// A stale but peer-backed kernel survives here (peer retention governs it, not this sweep).
	newPeer(t, db, "peerP", "peerKey", 0, 0, old)
	if err := db.UpsertKernel(ctx, "peerKey", "peerP", "", "", "", old); err != nil {
		t.Fatal(err)
	}

	n, err := db.PurgeStaleDiscovery(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeStaleDiscovery: %v", err)
	}
	if n != 1 {
		t.Errorf("evicted = %d, want 1", n)
	}
	count := func(q string, args ...any) int {
		var c int
		if err := db.db.QueryRowContext(ctx, q, args...).Scan(&c); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return c
	}
	if c := count(`SELECT COUNT(*) FROM kernels WHERE public_key=?`, "staleKey"); c != 0 {
		t.Errorf("stale discovered_kernels = %d, want 0", c)
	}
	if c := count(`SELECT COUNT(*) FROM discovery_docs WHERE kernel_public_key=?`, "staleKey"); c != 0 {
		t.Errorf("stale discovery_docs = %d, want 0", c)
	}
	if c := count(`SELECT COUNT(*) FROM evidence WHERE issuer_public_key=?`, "staleKey"); c != 0 {
		t.Errorf("stale evidence = %d, want 0", c)
	}
	if c := count(`SELECT COUNT(*) FROM kernels WHERE public_key IN ('freshKey','peerKey')`); c != 2 {
		t.Errorf("fresh + peer-backed survivors = %d, want 2", c)
	}
}

func TestListPurgeablePeers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)

	idle := newPeer(t, db, "idle", "k-idle", 0, 0, old) // the only purgeable peer

	newPeer(t, db, "funded", "k-funded", 100, 0, old) // excluded: holds value (available)
	newPeer(t, db, "locked", "k-locked", 0, 50, old)  // excluded: holds value (locked)
	newPeer(t, db, "recent", "k-recent", 0, 0, now)   // excluded: created within the window

	// excluded: a recent gossip mention keeps it live
	newPeer(t, db, "gossip", "k-gossip", 0, 0, old)
	if err := db.UpsertKernel(ctx, "k-gossip", "gossip", "", "", "", now); err != nil {
		t.Fatal(err)
	}

	// excluded: a recent transaction names the peer, though its balance is zero
	txp := newPeer(t, db, "txp", "k-txp", 0, 0, old)
	insertTx(t, db, txp.ID, txp.ID, "some-target", "some-action", now)

	// excluded: a waiting step is addressed to the peer as required caller
	stepp := newPeer(t, db, "stepp", "k-stepp", 0, 0, old)
	owner := newUser("sowner", 0)
	_ = db.CreateUser(ctx, owner)
	act := newAction(owner.ID, "approve", 0, true)
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateStep(ctx, &kernel.Step{ID: uuid.New().String(), RequiredCallerUserID: stepp.ID, ActionID: act.ID, Price: 0, Status: kernel.StepWaiting, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	// excluded: a local (non-peer) account, even though old and zero-balance
	local := newUser("local", 0)
	local.CreatedAt = old
	_ = db.CreateUser(ctx, local)

	// The §13 contact cache must NOT count as activity: a fresh last_seen on the idle peer keeps
	// it purgeable, or answering gossip would immortalize a zombie peer.
	if err := db.RecordKernelContact(ctx, idle.KernelPublicKey, true, now, nil); err != nil {
		t.Fatalf("RecordKernelContact: %v", err)
	}

	ids, err := db.ListPurgeablePeers(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListPurgeablePeers: %v", err)
	}
	if len(ids) != 1 || ids[0] != idle.ID {
		t.Fatalf("purgeable = %v, want exactly [%s (@idle)]", ids, idle.ID)
	}
}

// TestRecordKernelContact round-trips the §13 contact cache: success and failure land on their own
// columns, each only moves forward, a nil credit keeps the prior one, and an unknown key writes nothing.
func TestRecordKernelContact(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	peer := newPeer(t, db, "synced", "k-synced", 0, 0, now)
	key := peer.KernelPublicKey

	credit := int64(900)
	if err := db.RecordKernelContact(ctx, key, true, now, &credit); err != nil {
		t.Fatalf("RecordKernelContact: %v", err)
	}
	got, _ := db.ReadKernel(ctx, key)
	if got.LastSeen == nil {
		t.Error("expected last_seen set")
	}
	if got.LastContactFailedAt != nil {
		t.Error("a success must not touch last_contact_failed_at")
	}
	if got.PeerCredit == nil || *got.PeerCredit != 900 {
		t.Errorf("peer_credit = %v, want 900", got.PeerCredit)
	}

	// A nil credit advances last_seen but keeps the prior value (COALESCE).
	later := now.Add(time.Hour)
	if err := db.RecordKernelContact(ctx, key, true, later, nil); err != nil {
		t.Fatalf("RecordKernelContact nil credit: %v", err)
	}
	got, _ = db.ReadKernel(ctx, key)
	if got.PeerCredit == nil || *got.PeerCredit != 900 {
		t.Errorf("nil credit must keep prior 900, got %v", got.PeerCredit)
	}
	if !got.LastSeen.Equal(later) {
		t.Errorf("last_seen = %v, want %v", got.LastSeen, later)
	}

	// Neither timestamp ever moves backwards, so a slow observation cannot overwrite newer truth.
	// Sub-second spacing is the case lexical text ordering gets wrong, hence julianday().
	stale := later.Add(-500 * time.Millisecond)
	if err := db.RecordKernelContact(ctx, key, true, stale, nil); err != nil {
		t.Fatalf("stale success: %v", err)
	}
	got, _ = db.ReadKernel(ctx, key)
	if !got.LastSeen.Equal(later) {
		t.Errorf("stale success moved last_seen to %v, want %v held", got.LastSeen, later)
	}

	// A failure lands on its own column and leaves the success untouched: a reader compares them.
	failedAt := later.Add(time.Minute)
	if err := db.RecordKernelContact(ctx, key, false, failedAt, nil); err != nil {
		t.Fatalf("failure: %v", err)
	}
	got, _ = db.ReadKernel(ctx, key)
	if got.LastContactFailedAt == nil || !got.LastContactFailedAt.Equal(failedAt) {
		t.Errorf("last_contact_failed_at = %v, want %v", got.LastContactFailedAt, failedAt)
	}
	if !got.LastSeen.Equal(later) {
		t.Errorf("failure moved last_seen to %v, want %v held", got.LastSeen, later)
	}
	if err := db.RecordKernelContact(ctx, key, false, failedAt.Add(-time.Second), nil); err != nil {
		t.Fatalf("stale failure: %v", err)
	}
	got, _ = db.ReadKernel(ctx, key)
	if !got.LastContactFailedAt.Equal(failedAt) {
		t.Errorf("stale failure moved last_contact_failed_at to %v", got.LastContactFailedAt)
	}

	// Observing a kernel this one has never met creates nothing (§13): no row, no error.
	if err := db.RecordKernelContact(ctx, "k-unknown-kernel", false, now, nil); err != nil {
		t.Fatalf("unknown key must be a silent no-op: %v", err)
	}
	if rk, _ := db.ReadKernel(ctx, "k-unknown-kernel"); rk != nil {
		t.Error("contact created a kernel row for an unknown key")
	}
}

// ---- Grants (delegated upstream OAuth, §8) ----

func TestGrantCRUDAndUpsert(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	user := newUser("grantor", 0)
	_ = db.CreateUser(ctx, user)
	a := newAction(user.ID, "/oauth-svc", 0, true)
	_ = db.CreateAction(ctx, a)

	g := &kernel.Grant{ID: uuid.New().String(), GrantorUserID: user.ID, ActionID: a.ID, CreatedAt: time.Now().UTC()}
	if err := db.CreateOrReplaceGrant(ctx, g); err != nil {
		t.Fatalf("CreateOrReplaceGrant: %v", err)
	}
	got, err := db.ReadGrant(ctx, user.ID, a.ID)
	if err != nil {
		t.Fatalf("ReadGrant: %v", err)
	}
	if got.ID != g.ID {
		t.Errorf("read back id = %q, want %q", got.ID, g.ID)
	}

	// Re-consent overwrites in place: still one row, new id. A grant holds no secret of its own —
	// the credential lives on the Connection it points at (§8).
	g2 := &kernel.Grant{ID: uuid.New().String(), GrantorUserID: user.ID, ActionID: a.ID, CreatedAt: time.Now().UTC()}
	if err := db.CreateOrReplaceGrant(ctx, g2); err != nil {
		t.Fatalf("re-consent: %v", err)
	}
	list, _ := db.ListGrantsByUser(ctx, user.ID)
	if len(list) != 1 {
		t.Fatalf("grant count = %d, want 1 (upsert)", len(list))
	}
	if list[0].ID != g2.ID {
		t.Errorf("after upsert id = %q, want %q", list[0].ID, g2.ID)
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

	u1 := newUser("g1", 0)
	u2 := newUser("g2", 0)
	_ = db.CreateUser(ctx, u1)
	_ = db.CreateUser(ctx, u2)
	a := newAction(u1.ID, "/multi", 0, true)
	_ = db.CreateAction(ctx, a)

	for _, u := range []*kernel.Account{u1, u2} {
		_ = db.CreateOrReplaceGrant(ctx, &kernel.Grant{ID: uuid.New().String(), GrantorUserID: u.ID, ActionID: a.ID, CreatedAt: time.Now().UTC()})
	}
	if err := db.DeleteGrantsForAction(ctx, a.ID); err != nil {
		t.Fatalf("DeleteGrantsForAction: %v", err)
	}
	for _, u := range []*kernel.Account{u1, u2} {
		if _, err := db.ReadGrant(ctx, u.ID, a.ID); !errors.Is(err, kernel.ErrNotFound) {
			t.Errorf("grant for %s survived action-wide delete", u.Handle)
		}
	}
}

func TestConnectionCRUDAndCascade(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := newUser("conn", 0)
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

func TestListNativeActions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("sys", 0)
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
	sys := newUser("sys", 0)
	alice := newUser("alice", 100)
	bob := newUser("bob", 0)
	for _, u := range []*kernel.Account{sys, alice, bob} {
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

// ListStepsAwaitingCaller must be scoped in SQL and oldest-first: the federation step list (§13)
// relies on it, and filtering ListSteps' disjunction in Go after its row cap discarded exactly the
// steps a peer could complete.
func TestListStepsAwaitingCaller(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("owner", 1000)
	assignee := newUser("assignee", 0)
	other := newUser("other", 0)
	for _, u := range []*kernel.Account{owner, assignee, other} {
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
		if err := db.BeginRun(ctx, p, root, processOwner, 0, 0, 0); err != nil {
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

	got, err := db.ListStepsAwaitingCaller(ctx, assignee.ID, 200)
	if err != nil {
		t.Fatalf("ListStepsAwaitingCaller: %v", err)
	}
	if len(got) != 1 || got[0].ID != mine {
		t.Fatalf("expected only the assignee's own waiting step, got %d rows", len(got))
	}

	// Oldest first: the longest-stranded step is what an operator needs to see.
	second := mkStep(owner.ID, assignee.ID)
	got, _ = db.ListStepsAwaitingCaller(ctx, assignee.ID, 200)
	if len(got) != 2 || got[0].ID != mine || got[1].ID != second {
		t.Errorf("expected oldest-first ordering, got %d rows in unexpected order", len(got))
	}

}

// Scenario (design review): CommitRemoteSettlement completes the idempotency record with
// ktx.ReplyJSON, which settleRemoteCall sets only on SUCCESS. A remote FAILURE therefore stores
// result_json "null", so a replaying peer reads no "error" key and gets HTTP 200 — a settled
// failure replaying as success, which §15 forbids. Written from the scenario before the fix.
func TestCommitRemoteSettlementStoresFailureResult(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("rs-owner", 1000)
	proxy := newUser("rs-proxy", 0)
	sys := newUser("rs-sys", 0)
	for _, u := range []*kernel.Account{owner, proxy, sys} {
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
	if err := db.BeginRun(ctx, p, root, owner.ID, 10, 0, 0); err != nil {
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

// TestListReceiptsForGossip pins the evidence sender (§13): a committed call to the kernel's own
// active public action is gossip-eligible execution evidence with the caller named as counterparty
// when the caller is a peer. A NULL-effect ordinary action must be included (regression: a naive
// `effect != 'transfer'` filter drops NULL-effect rows).
func TestListReceiptsForGossip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	owner := newUser("gprov", 0)
	_ = db.CreateUser(ctx, owner)
	// A peer caller (public_key set) so the evidence names it as counterparty.
	peer := newPeer(t, db, "gpeer", "peerKeyXYZ", 100, 0, time.Now().UTC())
	fee := newUser("gfee", 0)
	_ = db.CreateUser(ctx, fee)

	act := newAction(owner.ID, "greet", 10, true)
	act.Kind = kernel.KindHTTP
	act.Visibility = kernel.VisibilityPublic
	act.Effect = "" // stored as NULL-ish ordinary action
	if err := db.CreateAction(ctx, act); err != nil {
		t.Fatal(err)
	}

	p := newProcess(peer.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, peer.ID, 10, 0, 0); err != nil {
		t.Fatal(err)
	}
	tx := &kernel.Transaction{
		ID: uuid.New().String(), ProcessID: p.ID, TraceID: root.ID,
		OwnerUserID: peer.ID, CallerUserID: peer.ID, TargetUserID: owner.ID,
		ActionID: act.ID, Status: kernel.TxSuccess, Gross: 10, Net: 8, Fee: 2,
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
	}
	receipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: owner.ID, TxID: tx.ID, TraceID: root.ID, ActionID: act.ID,
		ArgsHash: "ah", ReplyHash: "rh", Status: kernel.TxSuccess, Gross: 10, Net: 8, Fee: 2,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CommitCall(ctx, tx, receipt, root.ID, p.ID, kernel.CallerProcess, owner.ID, fee.ID, 8, 2, nil, "", ""); err != nil {
		t.Fatal(err)
	}

	rows, err := db.ListReceiptsForGossip(ctx, "", 100)
	if err != nil {
		t.Fatalf("ListReceiptsForGossip: %v", err)
	}
	var found *kernel.GossipReceiptRow
	for _, r := range rows {
		if r.Receipt != nil && r.Receipt.ActionID == act.ID {
			found = r
		}
	}
	if found == nil {
		t.Fatalf("a NULL-effect public action's receipt must be gossip-eligible; got %d rows", len(rows))
	}
	if found.CounterpartyKernelPublicKey != "peerKeyXYZ" {
		t.Errorf("counterparty = %q, want the peer caller's key", found.CounterpartyKernelPublicKey)
	}
	if found.SubjectKernelPublicKey != "" {
		t.Errorf("own-action subject kernel must be empty (kernel fills its own key), got %q", found.SubjectKernelPublicKey)
	}
	if found.Cursor == "" {
		t.Error("gossip row must carry a cursor high-watermark")
	}
	// A leg-(a) own-execution row has no outbound idempotency key; the field is populated only for a
	// leg-(b) receipt-backed proxy row, where the kernel uses it to drop signed rejections (§13).
	if found.IdempotencyKey != "" {
		t.Errorf("own-execution row must have an empty IdempotencyKey, got %q", found.IdempotencyKey)
	}
}

// ---- Account/kernel split: schema invariants, roster, migration (§3, §13, §14) ----

// TestKernelAccountCredentialSeparation: the split is enforced by the schema, not by convention — a
// kernel account can never hold a session credential, so a peer can never authenticate as a user.
func TestKernelAccountCredentialSeparation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const key = "credsepkey"
	if err := db.UpsertKernel(ctx, key, "credsep", "", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	base := func() *kernel.Account {
		return &kernel.Account{ID: uuid.New().String(), KernelPublicKey: key,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	}
	for _, tc := range []struct {
		name  string
		mutet func(*kernel.Account)
	}{
		{"handle", func(a *kernel.Account) { a.Handle = "named" }},
		{"password", func(a *kernel.Account) { a.PasswordHash = "hash" }},
		{"recovery key", func(a *kernel.Account) { a.RecoveryPublicKey = "reckey" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base()
			tc.mutet(a)
			if err := db.CreateUser(ctx, a); err == nil {
				t.Errorf("a kernel account with a %s must be rejected", tc.name)
			}
		})
	}
	if err := db.CreateUser(ctx, base()); err != nil {
		t.Errorf("a credentialless kernel account must be accepted: %v", err)
	}
}

// TestKernelDeleteBlockedByAccount: the account→kernel foreign key is restrictive on purpose, so a
// code path that deletes a kernel outside the purge fails loudly instead of orphaning a ledger
// principal. The purge clears the link first, which is why it succeeds.
func TestKernelDeleteBlockedByAccount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	peer := newPeer(t, db, "fk-peer", "fkpeerkey", 0, 0, time.Now().UTC())

	if _, err := db.db.ExecContext(ctx, `DELETE FROM kernels WHERE public_key=?`, "fkpeerkey"); err == nil {
		t.Fatal("deleting a kernel with a linked account must be rejected")
	}
	if err := db.PurgePeerCascade(ctx, peer.ID); err != nil {
		t.Fatalf("PurgePeerCascade: %v", err)
	}
	if rk, _ := db.ReadKernel(ctx, "fkpeerkey"); rk != nil {
		t.Error("purge must remove the kernel row once the link is cleared")
	}
}

// TestSuspendedKernelAccountSurvivesRetention: a suspension must outlive idleness, or the peer
// returns unsuspended after the sweep and the moderation decision quietly evaporates (§13).
func TestSuspendedKernelAccountSurvivesRetention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)

	idle := newPeer(t, db, "ret-idle", "retidlekey", 0, 0, old)
	banned := newPeer(t, db, "ret-banned", "retbannedkey", 0, 0, old)
	if err := db.SuspendUser(ctx, banned.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := db.ListPurgeablePeers(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListPurgeablePeers: %v", err)
	}
	if len(ids) != 1 || ids[0] != idle.ID {
		t.Fatalf("purgeable = %v, want only the unsuspended idle peer %s", ids, idle.ID)
	}
}

// TestListKernelsRoster: the whole `admin peers` view comes from one query — counterparties and
// discovery-only kernels merged, self excluded, suspended hidden unless asked for (§14).
func TestListKernelsRoster(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	newPeer(t, db, "titan", "rosterK1", 5, 0, now)
	banned := newPeer(t, db, "banned", "rosterK4", 0, 0, now)
	if err := db.SuspendUser(ctx, banned.ID); err != nil {
		t.Fatal(err)
	}
	// Discovery-only: a kernel row with a nickname and no account or petname.
	if err := db.UpsertKernel(ctx, "rosterK3", "minibox", "", "", "", now); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertKernel(ctx, "SELF", "me", "", "", "", now); err != nil {
		t.Fatal(err)
	}

	byKey := func(includeSuspended bool) map[string]*kernel.RemoteKernelView {
		views, err := db.ListKernels(ctx, "SELF", includeSuspended, 0, 0)
		if err != nil {
			t.Fatalf("ListKernels: %v", err)
		}
		m := map[string]*kernel.RemoteKernelView{}
		for _, v := range views {
			m[v.PublicKey] = v
		}
		return m
	}

	def := byKey(false)
	if _, ok := def["SELF"]; ok {
		t.Error("the roster must exclude this kernel")
	}
	if _, ok := def["rosterK4"]; ok {
		t.Error("a suspended counterparty must be hidden by default")
	}
	if v := def["rosterK1"]; v == nil || !v.HasAccount || v.Available != 5 || v.Petname != "titan" {
		t.Errorf("counterparty row = %+v, want an account with balance 5 and petname titan", v)
	}
	if v := def["rosterK3"]; v == nil || v.HasAccount || v.Nickname != "minibox" || v.Petname != "" {
		t.Errorf("discovery-only row = %+v, want no account and an unbound nickname", v)
	}
	if all := byKey(true); all["rosterK4"] == nil {
		t.Error("--all must include suspended counterparties")
	}
}

// TestUpsertKernelPreservesNarrowPaths: observation carries nickname/about only. The evidence cursor
// and the sync cache each advance on their own path, after their own work commits — otherwise a
// failure between observation and evidence persistence would skip a page forever (§13).
func TestUpsertKernelPreservesNarrowPaths(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const key = "narrowkey"

	if err := db.UpsertKernel(ctx, key, "nick", "about", "", "", now); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGossipCursor(ctx, key, "cursor-1"); err != nil {
		t.Fatal(err)
	}
	credit := int64(42)
	if err := db.RecordKernelContact(ctx, key, true, now, &credit); err != nil {
		t.Fatal(err)
	}
	// A later observation (e.g. the next gossip pass, or a minimal row from an inbound call)
	// must not reset any of it.
	if err := db.UpsertKernel(ctx, key, "", "", "", "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rk, err := db.ReadKernel(ctx, key)
	if err != nil || rk == nil {
		t.Fatalf("ReadKernel: %v", err)
	}
	if rk.GossipCursor != "cursor-1" {
		t.Errorf("cursor = %q, want cursor-1 (observation must not touch it)", rk.GossipCursor)
	}
	if rk.LastSeen == nil || rk.PeerCredit == nil || *rk.PeerCredit != 42 {
		t.Errorf("sync cache lost: last_seen=%v credit=%v", rk.LastSeen, rk.PeerCredit)
	}
	if rk.Nickname != "nick" || rk.About != "about" {
		t.Errorf("an empty observation must preserve prior metadata, got %q/%q", rk.Nickname, rk.About)
	}
}

// TestMigration037Integrity checks what the migration promises: account ids survive (so every
// captured transaction party still resolves), no child table still points at the dropped `users`
// table, and the schema is referentially clean. SQLite writes the rewritten name quoted, so the
// sqlite_master check must look for both forms.
func TestSchemaAccountIntegrity(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	var dangling int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE sql LIKE '%REFERENCES users%' OR sql LIKE '%REFERENCES "users"%'`).
		Scan(&dangling); err != nil {
		t.Fatal(err)
	}
	if dangling != 0 {
		t.Errorf("%d schema objects still reference the dropped users table", dangling)
	}
	var children int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE sql LIKE '%REFERENCES "accounts"%' OR sql LIKE '%REFERENCES accounts%'`).
		Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children == 0 {
		t.Error("child tables must reference accounts after the rename")
	}
	// No legacy row may hold both a password and a kernel key — the CHECK would have failed the
	// migration, and no production path creates one.
	var mixed int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM accounts WHERE kernel_public_key IS NOT NULL AND (password_hash != '' OR handle IS NOT NULL)`).
		Scan(&mixed); err != nil {
		t.Fatal(err)
	}
	if mixed != 0 {
		t.Errorf("%d kernel accounts hold a session credential", mixed)
	}

	// A child insert still works, proving the rewritten foreign keys resolve.
	u := newUser("fk-child", 0)
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAction(ctx, newAction(u.ID, "child-act", 0, false)); err != nil {
		t.Fatalf("insert into a child table after migration: %v", err)
	}
	rows, err := db.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("PRAGMA foreign_key_check reported violations")
	}
}

// TestPurgedPeerIsHistoryNotAUser: retention keeps the credentialless row as the ledger anchor §13
// requires, but a handleless, keyless account is history — it must not surface as a live local user.
func TestPurgedPeerIsHistoryNotAUser(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	live := newUser("still-here", 0)
	if err := db.CreateUser(ctx, live); err != nil {
		t.Fatal(err)
	}
	peer := newPeer(t, db, "gone-peer", "gonekey", 0, 0, time.Now().UTC())
	if err := db.PurgePeerCascade(ctx, peer.ID); err != nil {
		t.Fatalf("PurgePeerCascade: %v", err)
	}
	// The anchor survives for the ledger…
	if _, err := db.ReadUser(ctx, peer.ID); err != nil {
		t.Fatalf("the tombstone must remain readable as a ledger anchor: %v", err)
	}
	// …but never as a user.
	users, err := db.ListUsers(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.ID == peer.ID {
			t.Error("a purged peer must not appear in the user list")
		}
	}
	if len(users) != 1 || users[0].ID != live.ID {
		t.Errorf("user list = %d rows, want only the live local user", len(users))
	}
}

// TestDiscoveryDocServingPrice: serving_price round-trips through the replace-all rebuild, the
// column rejects a negative value (§13), and migration 039 leaves no falsely-free legacy row —
// a fresh DB starts with an empty cache, so nothing can be read back at a defaulted price.
func TestDiscoveryDocServingPrice(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	var pre int
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM discovery_docs`).Scan(&pre); err != nil {
		t.Fatal(err)
	}
	if pre != 0 {
		t.Errorf("migration 039 must leave the discovery cache empty, got %d rows", pre)
	}

	docs := []*kernel.DiscoveryDoc{
		{KernelPublicKey: "pk", ActionID: "a1", Name: "paid", Description: "d", ServingPrice: 105, ObservedAt: now},
		{KernelPublicKey: "pk", ActionID: "a2", Name: "free", Description: "d", ServingPrice: 0, ObservedAt: now},
	}
	if err := db.ReplaceDiscoveryDocs(ctx, "pk", docs); err != nil {
		t.Fatalf("ReplaceDiscoveryDocs: %v", err)
	}
	got, err := db.ListDiscoveryDocs(ctx)
	if err != nil {
		t.Fatalf("ListDiscoveryDocs: %v", err)
	}
	prices := map[string]int64{}
	for _, d := range got {
		prices[d.ActionID] = d.ServingPrice
	}
	if prices["a1"] != 105 || prices["a2"] != 0 {
		t.Errorf("serving_price round-trip = %v, want a1=105 a2=0", prices)
	}

	// A negative serving price is rejected by the column CHECK, not silently stored.
	err = db.ReplaceDiscoveryDocs(ctx, "pk", []*kernel.DiscoveryDoc{
		{KernelPublicKey: "pk", ActionID: "a3", Name: "bad", ServingPrice: -1, ObservedAt: now},
	})
	if err == nil {
		t.Error("a negative serving_price must be rejected by the CHECK constraint")
	}
}

// TestMigration046DropsDiscoveryKind: the discovery cache became action-only, so 046 rebuilds it
// without the kind discriminator and the user-doc-only user_id column. The cache is regenerable
// (§13), so the rebuild is a truncation: old rows of both kinds and their FTS mirror go, and the
// replace/search round-trip works against the new shape.
func TestMigration046DropsDiscoveryKind(t *testing.T) {
	now := timeToStr(time.Now().UTC())
	path := preValueMigrationDB(t, func(raw *sql.DB) {
		for _, ins := range []string{
			`INSERT INTO discovery_docs (kernel_public_key,kind,user_id,handle,description,action_id,name,input_schema,output_schema,observed_at,serving_price,effect)
			 VALUES ('pk','user','u1','prov','a provider','','','{}','{}','` + now + `',0,'')`,
			`INSERT INTO discovery_docs (kernel_public_key,kind,user_id,handle,description,action_id,name,input_schema,output_schema,observed_at,serving_price,effect)
			 VALUES ('pk','action','u1','prov','forecast','a1','weather','{}','{}','` + now + `',10,'')`,
			`INSERT INTO discovery_fts (doc_key,text) VALUES ('pk/user/u1/','prov a provider')`,
			`INSERT INTO discovery_fts (doc_key,text) VALUES ('pk/action/u1/a1','prov weather forecast')`,
		} {
			if _, err := raw.Exec(ins); err != nil {
				t.Fatal(err)
			}
		}
	})
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	var docRows, ftsRows int
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM discovery_docs`).Scan(&docRows); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM discovery_fts`).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if docRows != 0 || ftsRows != 0 {
		t.Errorf("046 must truncate the cache: %d docs, %d fts rows", docRows, ftsRows)
	}
	cols := map[string]bool{}
	rows, err := db.db.QueryContext(ctx, `PRAGMA table_info(discovery_docs)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	for _, gone := range []string{"kind", "user_id", "effect"} {
		if cols[gone] {
			t.Errorf("column %q survived the 046 rebuild", gone)
		}
	}

	// Repopulation path: a replace and a lexical search work against the new shape.
	if err := db.ReplaceDiscoveryDocs(ctx, "pk", []*kernel.DiscoveryDoc{
		{KernelPublicKey: "pk", Handle: "prov", ActionID: "a1", Name: "weather",
			Description: "forecast", ServingPrice: 10, ObservedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("ReplaceDiscoveryDocs: %v", err)
	}
	keys, err := db.SearchDiscoveryLexical(ctx, "weather", 10)
	if err != nil {
		t.Fatalf("SearchDiscoveryLexical: %v", err)
	}
	if len(keys) != 1 || keys[0] != "pk/a1" {
		t.Errorf("post-046 search = %v, want [pk/a1]", keys)
	}
}

// TestDiscoveryFTSDeleteIsNotWildcarded: doc keys are prefix-scoped by kernel key, and base64url
// keys may contain '_' — a LIKE single-char wildcard. The prefix delete must be an exact range, so
// clearing one kernel can never take an underscore-cousin's rows with it.
func TestDiscoveryFTSDeleteIsNotWildcarded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Under LIKE 'kernel_A/%', '_' would match the 'X' in kernelXA.
	const k1, k2 = "kernel_A", "kernelXA"
	seed := func(key, name string) {
		if err := db.ReplaceDiscoveryDocs(ctx, key, []*kernel.DiscoveryDoc{
			{KernelPublicKey: key, Handle: "prov", ActionID: "a1", Name: name,
				Description: name, ServingPrice: 1, ObservedAt: now},
		}); err != nil {
			t.Fatalf("ReplaceDiscoveryDocs(%s): %v", key, err)
		}
	}
	seed(k1, "alpha")
	seed(k2, "beta")

	// Replace-all for k1 must not clear k2's FTS row.
	seed(k1, "gamma")
	if keys, err := db.SearchDiscoveryLexical(ctx, "beta", 10); err != nil || len(keys) != 1 || keys[0] != k2+"/a1" {
		t.Fatalf("k2's fts row lost to k1's replace: keys=%v err=%v", keys, err)
	}

	// Stale eviction of k1 (never a peer) must not clear k2's rows either.
	old := now.Add(-100 * 24 * time.Hour)
	if err := db.UpsertKernel(ctx, k1, "one", "", "", "", old); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertKernel(ctx, k2, "two", "", "", "", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PurgeStaleDiscovery(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("PurgeStaleDiscovery: %v", err)
	}
	if keys, err := db.SearchDiscoveryLexical(ctx, "beta", 10); err != nil || len(keys) != 1 || keys[0] != k2+"/a1" {
		t.Fatalf("k2's fts row lost to k1's purge: keys=%v err=%v", keys, err)
	}
	if keys, _ := db.SearchDiscoveryLexical(ctx, "gamma", 10); len(keys) != 0 {
		t.Fatalf("k1's fts rows survived its purge: %v", keys)
	}
}

// TestPriceSnapshotColumnsRoundTrip: the two 041 snapshot columns are nullable and survive a
// round-trip. NULL is meaningful — it marks a row imported or parked before the change, which heals
// rather than being reverse-calculated (§16).
func TestPriceSnapshotColumnsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner := newUser("owner041", 0)
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}

	base := int64(100)
	withPrice := newAction(owner.ID, "bob/greet", 111, true)
	withPrice.Kind = kernel.KindRemoteProxy
	withPrice.BasePrice = &base
	legacy := newAction(owner.ID, "bob/wave", 111, true)
	legacy.Kind = kernel.KindRemoteProxy
	for _, a := range []*kernel.Action{withPrice, legacy} {
		if err := db.CreateAction(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.ReadAction(ctx, withPrice.ID)
	if err != nil || got.BasePrice == nil || *got.BasePrice != 100 {
		t.Errorf("base_price round-trip = %v (err %v), want 100", got.BasePrice, err)
	}
	if l, _ := db.ReadAction(ctx, legacy.ID); l.BasePrice != nil {
		t.Errorf("legacy row base_price = %v, want nil", l.BasePrice)
	}

	// steps.import_bps behaves the same way.
	p := newProcess(owner.ID)
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, root, owner.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	ptID := root.ID
	ibps := int64(500)
	step := &kernel.Step{
		ID: uuid.New().String(), ParentTraceID: &ptID, RequiredCallerUserID: owner.ID,
		ActionID: withPrice.ID, Price: 0,
		ImportBPS: &ibps, Status: kernel.StepWaiting, CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateStep(ctx, step); err != nil {
		t.Fatal(err)
	}
	back, err := db.ReadStep(ctx, step.ID)
	if err != nil || back.ImportBPS == nil || *back.ImportBPS != 500 {
		t.Errorf("step import_bps round-trip = %v (err %v), want 500", back.ImportBPS, err)
	}
}

// A settlement closed under the retired cash record is history the rail table must carry: the debt
// is gone, the payment is final, and the money that crossed the rail has to keep counting in the
// solvency audit. 047 converts both sides — a creditor's credited claim, a debtor's announced
// settlement — and gives the cash record the shape of the crossing it always was.
func TestRailMigrationConvertsRetiredCashRecords(t *testing.T) {
	now := timeToStr(time.Now().UTC())
	path := preValueMigrationDB(t, func(raw *sql.DB) {
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := raw.Exec(q, args...); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		exec(`INSERT INTO config (key,value) VALUES ('signing_public_key','ourkey')`)
		for _, p := range []struct{ id, key string }{{"peerA", "keyA"}, {"peerB", "keyB"}} {
			exec(`INSERT INTO kernels (public_key,first_seen,updated_at) VALUES (?,?,?)`, p.key, now, now)
			exec(`INSERT INTO "accounts" (id,kernel_public_key,available,locked,created_at,updated_at)
			      VALUES (?,?,0,0,?,?)`, p.id, p.key, now, now)
		}
		// We are the creditor of sidA (peerA paid us Q=100 against a debt of 30) and the debtor of
		// sidB (we paid peerB Q=120 against a debt of 40).
		for _, s := range []struct {
			sid, party, creditor string
			debt, cash           int64
		}{
			{"sidA", "peerA", "ourkey", 30, 100},
			{"sidB", "peerB", "otherkey", 40, 120},
		} {
			record := fmt.Sprintf(`{"settlement_id":%q,"creditor":%q,"outcome":"pay"}`, s.sid, s.creditor)
			exec(`INSERT INTO ledger (id,operator_user_id,to_user_id,amount,reason,external_key,created_at)
			      VALUES (?,?,?,?,?,?,?)`, "st_"+s.sid, "u1", s.party, s.debt, record, s.sid, now)
			exec(`INSERT INTO ledger (id,operator_user_id,to_user_id,amount,reason,external_key,created_at)
			      VALUES (?,?,?,?,?,?,?)`, "st_"+s.sid+".cash", "u1", s.party, s.cash, record, s.sid+".cash", now)
		}
	})

	db := openAt(t, path)
	ctx := context.Background()
	for _, want := range []*kernel.RailTransfer{
		{ID: "sidA", Kind: kernel.RailKindClaim, Party: "peerA", Amount: 100, Credit: 30, Status: kernel.RailStatusCredited},
		{ID: "sidB", Kind: kernel.RailKindSettlement, Party: "peerB", Amount: 120, Credit: 40, Status: kernel.RailStatusAnnounced},
	} {
		got, err := db.ReadRailTransfer(ctx, want.ID)
		if err != nil || got == nil {
			t.Fatalf("%s was not converted: %v", want.ID, err)
		}
		if got.Kind != want.Kind || got.Party != want.Party || got.Amount != want.Amount ||
			got.Credit != want.Credit || got.Status != want.Status {
			t.Errorf("%s converted to %+v, want %+v", want.ID, got, want)
		}
		// Nothing is left for the worker to drive: both sides are finished history.
		if got.Open() {
			t.Errorf("%s is still open after conversion", want.ID)
		}
		if pending, _ := db.HasPendingSettlement(ctx, got.Party); pending {
			t.Errorf("%s still reads as an unsettled debt", want.ID)
		}
	}
	// The cash rows now read as crossings in the direction the money went, so the vault counts them:
	// 100 in from the payment we received, 120 out for the one we made.
	pos, err := db.RailPosition(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if pos.Vault != -20 {
		t.Errorf("vault after conversion = %d, want -20 (100 received, 120 paid)", pos.Vault)
	}
}
