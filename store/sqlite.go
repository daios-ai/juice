// Package store provides the SQLite implementation of kernel.Store.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/daios-ai/juice/kernel"
	sqlite "modernc.org/sqlite"
)

const driverName = "sqlite"

// Migration policy: store/migrations/*.sql is the canonical schema history.
// Keep future schema changes in numbered SQL files and let this runner apply
// them; do not add ad hoc migration SQL to Go.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// DB implements kernel.Store using SQLite.
type DB struct {
	db *sql.DB
}

// Open opens (or creates) a SQLite database at path and runs migrations.
func Open(path string) (*DB, error) {
	// modernc.org/sqlite ignores mattn-style params (_journal_mode, _busy_timeout, …);
	// pragmas must use its _pragma=NAME(VALUE) form or they silently have no effect.
	// _txlock=immediate makes write transactions take the write lock at BEGIN so that
	// busy_timeout retries on contention instead of dead-locking — required for safe
	// concurrent access from a running server and a CLI process on the same DB file.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer
	s := &DB{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close shuts down the database connection.
func (s *DB) Close() error {
	return s.db.Close()
}

// migrate applies file-backed SQL migrations in order.
// The schema history was folded into one baseline once every live database had reached
// legacySentinel, the last incremental migration. Three states are supported, and nothing else:
// a database that already records the baseline, an empty one, and one sitting exactly at the end
// of the old chain — which is normalized and stamped, in one transaction, on its next boot.
// createSchemaMigrations is the runner's own ledger table — the one schema object the baseline
// file does not carry, because it must exist before any migration can be recorded.
const createSchemaMigrations = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`

const (
	baselineVersion = "001_baseline"
	legacySentinel  = "042_discovery_effect"
	legacyChainLen  = 42
)

func (s *DB) migrate() error {
	if _, err := s.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	if _, err := s.db.Exec(createSchemaMigrations); err != nil {
		return err
	}
	if err := s.reconcileBaseline(); err != nil {
		return err
	}
	files, err := migrationFileNames()
	if err != nil {
		return err
	}
	for _, file := range files {
		version := strings.TrimSuffix(path.Base(file), ".sql")
		applied, err := s.migrationApplied(version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		sqlBytes, err := migrationFS.ReadFile(file)
		if err != nil {
			return err
		}
		if err := s.applyMigration(version, string(sqlBytes)); err != nil {
			return err
		}
	}
	return nil
}

// reconcileBaseline decides which of the three supported states this database is in, and rejects
// every other one rather than guessing. A database at the end of the legacy chain is brought to
// the baseline atomically: the audit proving no legacy grant token survives, the column drop that
// makes an upgraded schema identical to a fresh one, and the stamp all commit together or not at
// all. The audit — not the recorded version — is what proves the completed backfill, because the
// backfill ran at startup AFTER the last migration was recorded.
func (s *DB) reconcileBaseline() error {
	var stamped int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, baselineVersion).Scan(&stamped); err != nil {
		return err
	}
	if stamped > 0 {
		return nil // already on the baseline; later migrations apply normally
	}
	var recorded int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		return err
	}
	if recorded == 0 {
		// Fresh only when there is no application schema: an empty ledger over existing tables is a
		// damaged or foreign database, and creating the baseline over it would fail mid-way.
		var tables int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_migrations'`).Scan(&tables); err != nil {
			return err
		}
		if tables > 0 {
			return fmt.Errorf("store: database has tables but no migration history; refusing to baseline over an unknown schema")
		}
		return nil // the runner creates the baseline below
	}
	var sentinel int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, legacySentinel).Scan(&sentinel); err != nil {
		return err
	}
	if sentinel == 0 || recorded != legacyChainLen {
		return fmt.Errorf("store: unsupported migration state (%d recorded, %s %s); upgrade through the previous release first",
			recorded, legacySentinel, map[bool]string{true: "present", false: "absent"}[sentinel > 0])
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var legacyTokens int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM grants WHERE refresh_token IS NOT NULL`).Scan(&legacyTokens); err != nil {
		return fmt.Errorf("store: legacy grant audit failed: %w", err)
	}
	if legacyTokens > 0 {
		return fmt.Errorf("store: %d grant(s) still hold a legacy token; run the previous release once to complete the connection backfill", legacyTokens)
	}
	if _, err := tx.Exec(`ALTER TABLE grants DROP COLUMN refresh_token`); err != nil {
		return fmt.Errorf("store: normalizing grants: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		baselineVersion, timeToStr(time.Now().UTC())); err != nil {
		return err
	}
	return tx.Commit()
}

func migrationFileNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, path.Join("migrations", e.Name()))
	}
	sort.Strings(files)
	return files, nil
}

func (s *DB) migrationApplied(version string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, version).Scan(&n)
	return n > 0, err
}

func (s *DB) applyMigration(version, sqlText string) error {
	// Disable FK enforcement for the duration of this migration so that table
	// reconstruction (DROP + CREATE) works even when child rows exist.
	s.db.Exec(`PRAGMA foreign_keys=OFF`)
	defer s.db.Exec(`PRAGMA foreign_keys=ON`)

	var txStmts []string
	for _, stmt := range splitSQLStatements(sqlText) {
		if strings.HasPrefix(strings.ToUpper(stmt), "PRAGMA ") {
			if _, err := s.db.Exec(stmt); err != nil {
				return fmt.Errorf("%s: %w", version, err)
			}
			continue
		}
		txStmts = append(txStmts, stmt)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range txStmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", version, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, timeToStr(time.Now().UTC())); err != nil {
		return err
	}
	return tx.Commit()
}

func splitSQLStatements(sqlText string) []string {
	var lines []string
	for _, line := range strings.Split(sqlText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		lines = append(lines, line)
	}
	parts := strings.Split(strings.Join(lines, "\n"), ";")
	stmts := make([]string, 0, len(parts))
	for _, p := range parts {
		stmt := strings.TrimSpace(p)
		if stmt == "" || strings.HasPrefix(stmt, "--") {
			continue
		}
		stmts = append(stmts, stmt)
	}
	return stmts
}

// ---- time helpers ----

const timeLayout = time.RFC3339Nano

func timeToStr(t time.Time) string { return t.UTC().Format(timeLayout) }
func strToTime(s string) time.Time {
	if t, err := time.Parse(timeLayout, s); err == nil {
		return t
	}
	// Legacy rows written with SQLite datetime('now') use "2006-01-02 15:04:05" (UTC, no zone).
	t, _ := time.Parse("2006-01-02 15:04:05", s)
	return t
}

func nullTimeToStr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := timeToStr(*t)
	return &s
}
func strToNullTime(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t := strToTime(*s)
	return &t
}

// strVal dereferences a nullable string pointer, returning "" for nil.
func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- Accounts ----

const userCols = `id,handle,description,password_hash,available,locked,suspended_at,kernel_public_key,recovery_public_key,rail_address,created_at,updated_at`

func (s *DB) CreateUser(ctx context.Context, u *kernel.Account) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (id,handle,description,password_hash,available,locked,suspended_at,kernel_public_key,recovery_public_key,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, nullStr(u.Handle), u.Description, u.PasswordHash, u.Available, u.Locked,
		nullTimeToStr(u.SuspendedAt),
		nullStr(u.KernelPublicKey), nullStr(u.RecoveryPublicKey),
		timeToStr(u.CreatedAt), timeToStr(u.UpdatedAt),
	)
	if err != nil {
		return dbErr(err, "create account")
	}
	return nil
}

func (s *DB) ReadUser(ctx context.Context, id string) (*kernel.Account, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM accounts WHERE id=?`, id))
}

func (s *DB) ReadUserByHandle(ctx context.Context, handle string) (*kernel.Account, error) {
	// Canonicalize at the single read funnel so every caller (login, federation, CLI, HTTP)
	// resolves the same row. A kernel account has no handle, so it is unreachable here by
	// construction — the user and kernel namespaces are disjoint (§13).
	handle = kernel.NormalizeHandle(handle)
	if handle == "" {
		return nil, kernel.ErrNotFound.Wrap("account not found")
	}
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM accounts WHERE handle=?`, handle))
}

// ReadAccountByKernelKey returns the account settling for a remote kernel, or ErrNotFound.
func (s *DB) ReadAccountByKernelKey(ctx context.Context, publicKey string) (*kernel.Account, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM accounts WHERE kernel_public_key=?`, publicKey))
}

// DeactivateImportedIfHash deactivates a remote_proxy action only while it still carries the given
// contract hash (§13 rule C): a stale dispatch's rejection settling after a re-resolve, which has
// already replaced the hash, is a no-op and spares the refreshed row. Not finding the row is fine.
func (s *DB) DeactivateImportedIfHash(ctx context.Context, actionID, expectedHash string, updatedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE actions SET active=FALSE, updated_at=? WHERE id=? AND artifact_hash=? AND kind='remote_proxy' AND deleted_at IS NULL`,
		updatedAt.Format(timeLayout), actionID, expectedHash)
	return dbErr(err, "deactivate imported by hash")
}

// ListPurgeablePeers returns peer accounts (kernel_public_key set) idle past cutoff at zero balance (§13).
// last_active = max(created_at, latest transaction naming the peer, latest deposit/withdrawal to
// the peer, latest gossip mention of the peer's key). Timestamps are compared via julianday() so
// the variable-width RFC3339Nano text (timeLayout) can't misorder near a second boundary. The
// comparison is `<=`: julianday() returns a float64 whose resolution near today's epoch is only
// ~tens of microseconds, so last_active and a cutoff a hair later can round equal; since seeds
// always precede the cutoff and julianday is monotonic, `<=` is deterministic where `<` flaked.
// A peer with any waiting/running step addressed to it or to one of its actions is still in use and skipped.
func (s *DB) ListPurgeablePeers(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT u.id FROM accounts u
WHERE u.kernel_public_key IS NOT NULL AND u.kernel_public_key != ''
  AND u.suspended_at IS NULL
  AND u.available = 0 AND u.locked = 0
  AND max(
        julianday(u.created_at),
        COALESCE((SELECT MAX(julianday(ended_at)) FROM transactions
                    WHERE owner_user_id=u.id OR caller_user_id=u.id OR target_user_id=u.id), julianday(u.created_at)),
        COALESCE((SELECT MAX(julianday(created_at)) FROM ledger WHERE from_user_id=u.id OR to_user_id=u.id), julianday(u.created_at)),
        COALESCE((SELECT MAX(julianday(updated_at)) FROM kernels WHERE public_key=u.kernel_public_key), julianday(u.created_at))
      ) <= julianday(?)
  AND NOT EXISTS (
        SELECT 1 FROM steps s
         WHERE s.status IN ('waiting','running')
           AND (s.required_caller_user_id=u.id
                OR s.action_id IN (SELECT id FROM actions WHERE owner_user_id=u.id)))
ORDER BY u.id`, timeToStr(cutoff))
	if err != nil {
		return nil, dbErr(err, "list purgeable peers")
	}
	return queryList(rows, "list purgeable peers", scanID)
}

// PurgePeerCascade deletes a purged peer's derived data and anonymizes the user row (§13 Retention).
// It removes the peer's proxy actions and their stats/stat_tags, the peer's steps and any steps
// bound to its actions, and its kernel row; it first clears the account→kernel link so the identity is
// forgotten (a later re-resolve starts fresh). The transaction/receipt ledger is left intact —
// its party ids carry no foreign key, so a now-dangling peer id is harmless and local counterparties'
// history stays reconstructible (§11). Deletes run children-before-parents so the RESTRICT foreign
// keys (steps→actions, stats→actions) never block.
func (s *DB) PurgePeerCascade(ctx context.Context, userID string) error {
	return s.withTx(ctx, "purge peer cascade", func(tx *sql.Tx) error {
		var pubKey sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT kernel_public_key FROM accounts WHERE id=?`, userID).Scan(&pubKey); err != nil {
			return dbErr(err, "read peer key")
		}
		const owned = `SELECT id FROM actions WHERE owner_user_id=?`
		if _, err := tx.ExecContext(ctx, `DELETE FROM action_stats WHERE action_id IN (`+owned+`)`, userID); err != nil {
			return dbErr(err, "delete action_stats")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM steps WHERE required_caller_user_id=? OR action_id IN (`+owned+`)`, userID, userID); err != nil {
			return dbErr(err, "delete steps")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM actions WHERE owner_user_id=?`, userID); err != nil {
			return dbErr(err, "delete actions")
		}
		// Clear the account→kernel link BEFORE deleting the kernel row: the foreign key is
		// restrictive on purpose, so the delete would otherwise fail (§13 Retention).
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET kernel_public_key=NULL, updated_at=? WHERE id=?`,
			timeToStr(time.Now().UTC()), userID); err != nil {
			return dbErr(err, "anonymize peer")
		}
		if pubKey.Valid && pubKey.String != "" {
			// Regenerable discovery/evidence caches purge with the peer (§13).
			if err := deleteDiscoveryCache(ctx, tx, pubKey.String); err != nil {
				return err
			}
		}
		return nil
	})
}

// deleteDiscoveryCache removes one kernel's regenerable discovery cache within tx: its discovery
// docs and their FTS mirror, its evidence rows (as issuer and as subject), and its kernels
// row. Shared by peer purge (§13 Retention) and stale non-peer eviction; order is free — no FK links
// these tables. Doc keys are "<kernel_public_key>/…", so the FTS delete is prefix-scoped by key —
// by range, not LIKE, since '_' in a base64url key would be a single-char wildcard and could match
// another kernel's rows ('0' is the ASCII successor of '/').
func deleteDiscoveryCache(ctx context.Context, tx *sql.Tx, pubKey string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM discovery_fts WHERE doc_key >= ? || '/' AND doc_key < ? || '0'`, pubKey, pubKey); err != nil {
		return dbErr(err, "delete discovery_fts")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM discovery_docs WHERE kernel_public_key=?`, pubKey); err != nil {
		return dbErr(err, "delete discovery_docs")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM evidence WHERE issuer_public_key=? OR subject_kernel_public_key=?`, pubKey, pubKey); err != nil {
		return dbErr(err, "delete evidence")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kernels WHERE public_key=?`, pubKey); err != nil {
		return dbErr(err, "delete kernel")
	}
	return nil
}

// PurgeStaleDiscovery evicts directory-only discovered kernels — those stale past cutoff and not
// backed by a peer user row — with their whole discovery cache (§13 Retention). Peer-backed kernels
// are left to PurgePeerCascade. One transaction; returns the count evicted.
func (s *DB) PurgeStaleDiscovery(ctx context.Context, cutoff time.Time) (int, error) {
	var evicted int
	err := s.withTx(ctx, "purge stale discovery", func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT public_key FROM kernels
			  WHERE updated_at <= ?
			    AND public_key NOT IN (SELECT kernel_public_key FROM accounts WHERE kernel_public_key IS NOT NULL)`,
			timeToStr(cutoff))
		if err != nil {
			return dbErr(err, "list stale discovery")
		}
		var keys []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return dbErr(err, "scan stale discovery")
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return dbErr(err, "stale discovery rows")
		}
		for _, k := range keys {
			if err := deleteDiscoveryCache(ctx, tx, k); err != nil {
				return err
			}
		}
		evicted = len(keys)
		return nil
	})
	return evicted, err
}

func scanUserFn(scan func(...any) error) (*kernel.Account, error) {
	var u kernel.Account
	var createdAt, updatedAt string
	var handle, suspendedAt, kernelPublicKey, recoveryPublicKey, railAddress *string
	if err := scan(&u.ID, &handle, &u.Description, &u.PasswordHash,
		&u.Available, &u.Locked, &suspendedAt, &kernelPublicKey, &recoveryPublicKey, &railAddress,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	u.RailAddress = strVal(railAddress)
	u.Handle = strVal(handle)
	u.SuspendedAt = strToNullTime(suspendedAt)
	u.KernelPublicKey = strVal(kernelPublicKey)
	u.RecoveryPublicKey = strVal(recoveryPublicKey)
	u.CreatedAt = strToTime(createdAt)
	u.UpdatedAt = strToTime(updatedAt)
	return &u, nil
}

func (s *DB) scanUser(row *sql.Row) (*kernel.Account, error) {
	u, err := scanUserFn(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("account not found")
	}
	if err != nil {
		return nil, dbErr(err, "read account")
	}
	return u, nil
}

// ListUsers returns live local user accounts. A live local user has a handle: kernel accounts hold
// none and belong to the peer roster (§14), and a purged peer leaves a handleless, keyless row that
// stays as a ledger anchor (§13 Retention) but is history, not a user.
func (s *DB) ListUsers(ctx context.Context, limit, offset int) ([]*kernel.Account, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userCols+` FROM accounts WHERE kernel_public_key IS NULL AND handle IS NOT NULL
		 ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list accounts")
	}
	return queryList(rows, "list accounts", scanUserFn)
}

func (s *DB) SuspendUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET suspended_at=? WHERE id=?`, timeToStr(time.Now().UTC()), id)
	return dbErr(err, "suspend account")
}

func (s *DB) UnsuspendUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET suspended_at=NULL WHERE id=?`, id)
	return dbErr(err, "unsuspend user")
}

// RecordKernelContact writes the contact display cache (§13), keyed by public key: one observation,
// one write. A successful contact advances last_seen and, when the peer reported one, refreshes our
// cached credit there; a failed one advances last_contact_failed_at. Each timestamp only moves
// forward and neither is ever cleared, so a slow observation cannot overwrite newer truth — a reader
// compares the two. Comparison goes through julianday(), since timeLayout is variable-width and
// misorders lexically near a second boundary (see ListPurgeablePeers). COALESCE keeps the prior
// credit when none was reported. A plain UPDATE: an unknown key is a no-op, because observing a
// kernel creates nothing (§13). updated_at is intentionally untouched — contact is a display cache,
// not peer activity for retention, or a zombie would be immortal.
func (s *DB) RecordKernelContact(ctx context.Context, publicKey string, ok bool, at time.Time, credit *int64) error {
	col := "last_contact_failed_at"
	if ok {
		col = "last_seen"
	}
	var cr any
	if ok && credit != nil {
		cr = *credit
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE kernels
		    SET `+col+` = CASE WHEN `+col+` IS NULL OR julianday(`+col+`) < julianday(?) THEN ? ELSE `+col+` END,
		        peer_credit = COALESCE(?, peer_credit)
		  WHERE public_key = ?`,
		timeToStr(at), timeToStr(at), cr, publicKey)
	return dbErr(err, "record kernel contact")
}

func (s *DB) UpdateUser(ctx context.Context, u *kernel.Account) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET description=?, password_hash=?, updated_at=? WHERE id=?`,
		u.Description, u.PasswordHash, timeToStr(u.UpdatedAt), u.ID,
	)
	return dbErr(err, "update user")
}

// RenameUser changes a user's handle. The UNIQUE constraint is the backstop against a
// concurrent collision the kernel's pre-check missed (§12).
func (s *DB) RenameUser(ctx context.Context, id, handle string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET handle=?, updated_at=? WHERE id=?`, handle, timeToStr(time.Now().UTC()), id)
	return dbErr(err, "rename user")
}

// ---- Actions ----

func (s *DB) CreateAction(ctx context.Context, a *kernel.Action) error {
	// Honor the schema's DEFAULT 'private': a zero-value visibility persists as private (the
	// old public=0 semantics), so the CHECK constraint never sees an empty string.
	if a.Visibility == "" {
		a.Visibility = kernel.VisibilityPrivate
	}
	inJSON, _ := json.Marshal(a.InputSchema)
	outJSON, _ := json.Marshal(a.OutputSchema)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO actions
		 (id,owner_user_id,name,kind,active,visibility,price,description,input_schema,output_schema,source,artifact_hash,wasm_artifact,remote_action_id,remote_owner_id,remote_bps,base_price,effect,auth_json,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerUserID, a.Name, string(a.Kind), boolInt(a.Active), string(a.Visibility), a.Price,
		a.Description, string(inJSON), string(outJSON), a.Source, a.ArtifactHash, a.WasmArtifact, a.RemoteActionID,
		a.RemoteOwnerID, a.RemoteBPS, a.BasePrice, nullStr(a.Effect), a.AuthJSON, timeToStr(a.CreatedAt), timeToStr(a.UpdatedAt),
	)
	return dbErr(err, "create action")
}

// actionCols is the canonical column list for action SELECT statements.
// Must stay in sync with scanAction/scanActionFn/finishAction.
const actionCols = `a.id,a.owner_user_id,COALESCE(u.handle,''),(u.suspended_at IS NOT NULL),a.name,a.kind,a.active,a.visibility,a.price,a.description,a.input_schema,a.output_schema,a.source,a.artifact_hash,a.wasm_artifact,a.remote_action_id,COALESCE(a.remote_owner_id,''),a.remote_bps,a.base_price,COALESCE(a.effect,''),a.auth_json,a.created_at,a.updated_at,a.deleted_at`

func (s *DB) ReadAction(ctx context.Context, id string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN accounts u ON u.id=a.owner_user_id WHERE a.id=? AND a.deleted_at IS NULL`, id))
}

func (s *DB) ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN accounts u ON u.id=a.owner_user_id WHERE a.owner_user_id=? AND a.name=? AND a.deleted_at IS NULL`, ownerID, name))
}

func (s *DB) updateActionTx(ctx context.Context, tx *sql.Tx, a *kernel.Action) error {
	inJSON, _ := json.Marshal(a.InputSchema)
	outJSON, _ := json.Marshal(a.OutputSchema)
	_, err := tx.ExecContext(ctx,
		`UPDATE actions SET kind=?,active=?,visibility=?,price=?,description=?,input_schema=?,output_schema=?,
		 source=?,artifact_hash=?,wasm_artifact=?,remote_owner_id=?,remote_bps=?,base_price=?,effect=?,auth_json=?,updated_at=? WHERE id=?`,
		string(a.Kind), boolInt(a.Active), string(a.Visibility), a.Price, a.Description,
		string(inJSON), string(outJSON), a.Source, a.ArtifactHash, a.WasmArtifact,
		a.RemoteOwnerID, a.RemoteBPS, a.BasePrice, nullStr(a.Effect), a.AuthJSON, timeToStr(a.UpdatedAt), a.ID,
	)
	return dbErr(err, "update action")
}

func (s *DB) UpdateAction(ctx context.Context, a *kernel.Action) error {
	return s.withTx(ctx, "update action", func(tx *sql.Tx) error {
		return s.updateActionTx(ctx, tx, a)
	})
}

// resetStatsTx zeros an action's stats row inside tx, creating it when absent.
func resetStatsTx(ctx context.Context, tx *sql.Tx, actionID string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,latency_estimate,rating_estimate,last_used_at)
		 VALUES (?,0,0,0,0,0,0,?)
		 ON CONFLICT(action_id) DO UPDATE SET
		   uses=0,successes=0,failures=0,rating_count=0,
		   latency_estimate=0,rating_estimate=0,last_used_at=excluded.last_used_at`,
		actionID, timeToStr(time.Time{}),
	)
	return dbErr(err, "reset stats")
}

// deleteGrantsForActionTx is the transaction-local form of DeleteGrantsForAction, so a lifecycle
// commit revokes consent in the same transaction that changes the action (§5).
func deleteGrantsForActionTx(ctx context.Context, tx *sql.Tx, actionID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM grants WHERE action_id=?`, actionID)
	return dbErr(err, "delete grants for action")
}

func (s *DB) UpdateActionLifecycle(ctx context.Context, a *kernel.Action, resetStats, revokeGrants bool) error {
	return s.withTx(ctx, "update action lifecycle", func(tx *sql.Tx) error {
		if err := s.updateActionTx(ctx, tx, a); err != nil {
			return err
		}
		if resetStats {
			if err := resetStatsTx(ctx, tx, a.ID); err != nil {
				return err
			}
		}
		if revokeGrants {
			if err := deleteGrantsForActionTx(ctx, tx, a.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *DB) deleteActionTx(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE actions SET deleted_at=? WHERE id=? AND deleted_at IS NULL`,
		timeToStr(time.Now().UTC()), id)
	return dbErr(err, "delete action")
}

func (s *DB) DeleteAction(ctx context.Context, id string) error {
	return s.withTx(ctx, "delete action", func(tx *sql.Tx) error {
		return s.deleteActionTx(ctx, tx, id)
	})
}

func (s *DB) DeleteActionAndGrants(ctx context.Context, id string) error {
	return s.withTx(ctx, "delete action and grants", func(tx *sql.Tx) error {
		if err := s.deleteActionTx(ctx, tx, id); err != nil {
			return err
		}
		return deleteGrantsForActionTx(ctx, tx, id)
	})
}

func (s *DB) ListVisibleActions(ctx context.Context, includeLocal bool, limit, offset int) ([]*kernel.Action, error) {
	// Public always; local only when the caller is local (§4/§14). Peers never reach this via a
	// session, so includeLocal is safe to key on session presence upstream.
	visFilter := `a.visibility='public'`
	if includeLocal {
		visFilter = `a.visibility IN ('public','local')`
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN accounts u ON u.id=a.owner_user_id
		 WHERE a.active=1 AND `+visFilter+` AND a.deleted_at IS NULL AND u.suspended_at IS NULL
		 ORDER BY a.created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list visible actions")
	}
	return queryList(rows, "list visible actions", scanActionFn)
}

func (s *DB) ListActionsByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*kernel.Action, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN accounts u ON u.id=a.owner_user_id
		 WHERE a.owner_user_id=? AND a.deleted_at IS NULL
		 ORDER BY a.created_at DESC LIMIT ? OFFSET ?`, ownerID, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list actions by owner")
	}
	return queryList(rows, "list actions by owner", scanActionFn)
}

func (s *DB) ListAllActions(ctx context.Context, limit, offset int) ([]*kernel.Action, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN accounts u ON u.id=a.owner_user_id WHERE a.deleted_at IS NULL ORDER BY a.created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list all actions")
	}
	return queryList(rows, "list all actions", scanActionFn)
}

func (s *DB) ListNativeActions(ctx context.Context) ([]*kernel.Action, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN accounts u ON u.id=a.owner_user_id
		 WHERE a.kind='native' AND a.deleted_at IS NULL ORDER BY a.name`)
	if err != nil {
		return nil, dbErr(err, "list native actions")
	}
	return queryList(rows, "list native actions", scanActionFn)
}

func scanActionFn(scan func(...any) error) (*kernel.Action, error) {
	var a kernel.Action
	var kind, visibility, inJSON, outJSON, createdAt, updatedAt string
	var deletedAt sql.NullString
	var remoteBPS, basePrice sql.NullInt64
	var active, ownerSuspended int
	if err := scan(&a.ID, &a.OwnerUserID, &a.OwnerHandle, &ownerSuspended, &a.Name, &kind, &active, &visibility, &a.Price,
		&a.Description, &inJSON, &outJSON, &a.Source, &a.ArtifactHash, &a.WasmArtifact, &a.RemoteActionID,
		&a.RemoteOwnerID, &remoteBPS, &basePrice, &a.Effect, &a.AuthJSON, &createdAt, &updatedAt, &deletedAt); err != nil {
		return nil, err
	}
	if remoteBPS.Valid {
		v := remoteBPS.Int64
		a.RemoteBPS = &v
	}
	if basePrice.Valid {
		v := basePrice.Int64
		a.BasePrice = &v
	}
	a.OwnerSuspended = ownerSuspended != 0
	return finishAction(&a, kind, visibility, active, inJSON, outJSON, createdAt, updatedAt, deletedAt)
}

func (s *DB) scanAction(row *sql.Row) (*kernel.Action, error) {
	a, err := scanActionFn(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("action not found")
	}
	if err != nil {
		return nil, dbErr(err, "read action")
	}
	return a, nil
}

func (s *DB) ReadActionByOwnerRemoteID(ctx context.Context, ownerID, remoteActionID string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN accounts u ON u.id=a.owner_user_id WHERE a.owner_user_id=? AND a.remote_action_id=? AND a.remote_action_id!='' AND a.deleted_at IS NULL`,
		ownerID, remoteActionID))
}

func finishAction(a *kernel.Action, kind, visibility string, active int, inJSON, outJSON, createdAt, updatedAt string, deletedAt sql.NullString) (*kernel.Action, error) {
	a.Kind = kernel.ActionKind(kind)
	a.Active = active != 0
	a.Visibility = kernel.ActionVisibility(visibility)
	a.CreatedAt = strToTime(createdAt)
	a.UpdatedAt = strToTime(updatedAt)
	if deletedAt.Valid {
		t := strToTime(deletedAt.String)
		a.DeletedAt = &t
	}
	if err := json.Unmarshal([]byte(inJSON), &a.InputSchema); err != nil {
		a.InputSchema = map[string]any{}
	}
	if err := json.Unmarshal([]byte(outJSON), &a.OutputSchema); err != nil {
		a.OutputSchema = map[string]any{}
	}
	return a, nil
}

// ---- Processes ----

// BeginRun atomically debits price from owner.available→locked, creates the process
// with available=0/locked=price, and creates the root trace with available=price.
// lockReserveTx atomically debits `amount` from userID.available into userID.locked, guarded by the
// §13 admission rule (factored so both the execution reserve and the value-transfer reserve use it):
//
//	ordinary user (kernel_public_key IS NULL): must be prepaid (available ≥ amount); the exposure clause is
//	  short-circuited true.
//	peer (kernel_public_key set): admitted when the draw does not increase this peer's own debt
//	  (max(0,amount−available) ≤ max(0,−available)), or when the projected global gross receivables
//	  (Σ over OTHER peer rows of max(0,−available) + this peer's post-debit debt) stay ≤ exposureMax.
//	  Own row's current debt is excluded and replaced by its projected value — the cap is on the whole
//	  book (Sybil-proof); SQLite serializes writers, so G ≤ X holds as an invariant.
//
// A zero amount is a no-op. Returns ErrInsufficientFunds when the guard rejects (0 rows).
func lockReserveTx(ctx context.Context, tx *sql.Tx, userID string, amount, exposureMax int64) error {
	if amount == 0 {
		return nil
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE accounts SET available=available-?, locked=locked+? WHERE id=?
		   AND (kernel_public_key IS NOT NULL OR available >= ?)
		   AND (kernel_public_key IS NULL OR ? = 0
		     OR MAX(0, ? - available) <= MAX(0, -available)
		     OR ((SELECT COALESCE(SUM(MAX(0,-available)),0) FROM accounts WHERE kernel_public_key IS NOT NULL AND id <> ?)
		         + MAX(0, ? - available)) <= ?)`,
		amount, amount, userID, amount, amount, amount, userID, amount, exposureMax,
	)
	if err != nil {
		return dbErr(err, "lock reserve: deduct user")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return kernel.ErrInsufficientFunds.Wrap("insufficient user balance")
	}
	return nil
}

func (s *DB) BeginRun(ctx context.Context, p *kernel.Process, t *kernel.Trace, ownerID string, price, premiumReserve, exposureMax int64) error {
	return s.withTx(ctx, "begin run", func(tx *sql.Tx) error {
		// Execution reserve on the process owner P (price + serving-markup reserve).
		if err := lockReserveTx(ctx, tx, ownerID, price+premiumReserve, exposureMax); err != nil {
			return err
		}
		// Transfer value on the immediate caller C (t.CallerUserID) from C's OWN balance, atomic with
		// the execution reserve (§13). For a root call C == P; a composed subcall differs. C is always
		// a local ordinary account — a peer funds no transfer — so it prepays (exposureMax 0).
		if err := lockReserveTx(ctx, tx, t.CallerUserID, t.Value, 0); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO processes (id,owner_user_id,available,locked,status,created_at,ended_at) VALUES (?,?,0,?,?,?,?)`,
			p.ID, p.OwnerUserID, price, string(p.Status), timeToStr(p.CreatedAt), nullTimeToStr(p.EndedAt),
		); err != nil {
			return dbErr(err, "begin run: insert process")
		}
		return insertTraceTx(ctx, tx, t, t.ParentTraceID, price)
	})
}

func (s *DB) ReadProcess(ctx context.Context, id string) (*kernel.Process, error) {
	var p kernel.Process
	var status, createdAt string
	var endedAt *string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,owner_user_id,available,locked,status,created_at,ended_at FROM processes WHERE id=?`, id,
	).Scan(&p.ID, &p.OwnerUserID, &p.Available, &p.Locked, &status, &createdAt, &endedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("process not found")
	}
	if err != nil {
		return nil, dbErr(err, "read process")
	}
	p.Status = kernel.ProcessStatus(status)
	p.CreatedAt = strToTime(createdAt)
	p.EndedAt = strToNullTime(endedAt)
	return &p, nil
}

func insertTraceTx(ctx context.Context, tx *sql.Tx, t *kernel.Trace, parentTraceID *string, price int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO traces (id,process_id,parent_trace_id,action_owner_id,action_id,caller_user_id,available,locked,idempotency_key,dispatch_json,idempotency_record_id,premium_bps,premium_parked,value,value_to,created_at)
		 VALUES (?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,?)`,
		t.ID, t.ProcessID, parentTraceID, t.ActionOwnerID, t.ActionID, t.CallerUserID, price, t.IdempotencyKey, t.DispatchJSON, t.IdempotencyRecordID, t.PremiumBPS, t.PremiumParked, t.Value, nullStr(t.ValueTo), timeToStr(t.CreatedAt),
	)
	return dbErr(err, "insert trace")
}

// BeginSubcall atomically deducts price from parent_trace.available into parent_trace.locked
// and creates the child trace with available=price.
func (s *DB) BeginSubcall(ctx context.Context, parentTraceID string, t *kernel.Trace, price int64) error {
	return s.withTx(ctx, "begin subcall", func(tx *sql.Tx) error {
		// Execution reserve: the subcall's price comes from the parent trace's budget (process funds).
		res, err := tx.ExecContext(ctx,
			`UPDATE traces SET available=available-?, locked=locked+?
			 WHERE id=? AND available>=?`,
			price, price, parentTraceID, price,
		)
		if err != nil {
			return dbErr(err, "begin subcall: lock parent trace funds")
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return kernel.ErrInsufficientFunds.Wrap("parent trace has insufficient available funds")
		}
		// Transfer value on the immediate caller C from C's OWN balance — a composed transfer (an action
		// subcalls sys/transfer) pays the value from the composing action owner, not the process budget.
		if err := lockReserveTx(ctx, tx, t.CallerUserID, t.Value, 0); err != nil {
			return err
		}
		return insertTraceTx(ctx, tx, t, &parentTraceID, price)
	})
}

// BeginStepCall atomically moves step.price from the step's parent_trace.locked back into
// parent_trace.available (consuming the park), claims the step waiting→running, and creates
// a new trace with available=step.price funded from the released lock.
func (s *DB) BeginStepCall(ctx context.Context, stepID string, t *kernel.Trace, exposureMax int64) error {
	return s.withTx(ctx, "begin step call", func(tx *sql.Tx) error {
		// Read step to get price and parent_trace_id.
		var price int64
		var parentTraceID *string
		err := tx.QueryRowContext(ctx,
			`SELECT price, parent_trace_id FROM steps WHERE id=? AND status='waiting'`,
			stepID,
		).Scan(&price, &parentTraceID)
		if errors.Is(err, sql.ErrNoRows) {
			return kernel.ErrInvalidState.Wrap("step not waiting").Because(kernel.ErrStepNotClaimed)
		}
		if err != nil {
			return dbErr(err, "begin step call: read step")
		}
		// The step's price was previously parked from parent_trace.locked;
		// release the lock (parent keeps the park; it flows into the new trace's available).
		// Guard locked>=price like BeginSubcall so a broken park invariant surfaces as a typed
		// kernel error rather than a raw CHECK(locked>=0) constraint failure.
		if parentTraceID != nil {
			res, lerr := tx.ExecContext(ctx,
				`UPDATE traces SET locked=locked-? WHERE id=? AND locked>=?`,
				price, *parentTraceID, price,
			)
			if lerr != nil {
				return dbErr(lerr, "begin step call: release parent trace lock")
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return kernel.ErrInvalidState.Wrap("step park invariant violated: parent trace locked < step price")
			}
		}
		// Value transfer (§13): the completer C funds the delivered value from its OWN balance, atomic
		// with claiming the step — the step's execution price stays creator-parked above. C is local (a
		// peer funds no transfer), so it prepays.
		if err = lockReserveTx(ctx, tx, t.CallerUserID, t.Value, 0); err != nil {
			return err
		}
		// Insert the completion trace first so the FK on steps.completion_trace_id is satisfied.
		if err = insertTraceTx(ctx, tx, t, parentTraceID, price); err != nil {
			return err
		}
		// Claim the step and record which trace will complete it.
		res, err := tx.ExecContext(ctx,
			`UPDATE steps SET status='running', completion_trace_id=? WHERE id=? AND status='waiting'`,
			t.ID, stepID)
		if err != nil {
			return dbErr(err, "begin step call: claim step")
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return kernel.ErrInvalidState.Wrap("step already claimed").Because(kernel.ErrStepNotClaimed)
		}
		return nil
	})
}

// insertAuditRows inserts the transaction record and its mandatory receipt into an open SQLite transaction.
func (s *DB) insertAuditRows(ctx context.Context, tx *sql.Tx, ktx *kernel.Transaction, receipt *kernel.Receipt, label string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO transactions
		 (id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,target_user_id,
		  action_id,action_name,remote_action_id,args_json,reply_json,status,gross,net,fee,refund,reason,remote_receipt_hash,remote_receipt_json,started_at,ended_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ktx.ID, ktx.ProcessID, ktx.TraceID, ktx.ParentTraceID,
		ktx.OwnerUserID, ktx.CallerUserID, ktx.TargetUserID, ktx.ActionID, ktx.ActionName, ktx.RemoteActionID,
		rawJSONStr(ktx.ArgsJSON), rawJSONStr(ktx.ReplyJSON), string(ktx.Status),
		ktx.Gross, ktx.Net, ktx.Fee, ktx.Refund, ktx.Reason, nullStr(ktx.RemoteReceiptHash), ktx.RemoteReceiptJSON,
		timeToStr(ktx.StartedAt), timeToStr(ktx.EndedAt),
	); err != nil {
		return dbErr(err, label+": insert transaction")
	}
	if receipt == nil {
		return dbErr(fmt.Errorf("receipt is required"), label)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO receipts (id,issuer_user_id,tx_id,trace_id,action_id,caller_user_id,process_id,
		                       args_hash,reply_hash,status,gross,net,fee,charge,premium,value,value_to,reason,started_at,created_at,signature)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.ID, receipt.IssuerUserID, receipt.TxID, receipt.TraceID, receipt.ActionID,
		receipt.CallerUserID, receipt.ProcessID,
		receipt.ArgsHash, receipt.ReplyHash, string(receipt.Status),
		receipt.Gross, receipt.Net, receipt.Fee, receipt.Charge, receipt.Premium, receipt.Value, nullStr(receipt.ValueTo), receipt.Reason,
		timeToStr(receipt.StartedAt), timeToStr(receipt.CreatedAt), receipt.Signature,
	); err != nil {
		return dbErr(err, label+": insert receipt")
	}
	return nil
}

// upsertActionStats updates the incremental success or failure counters for an action.
// rating_count/rating_estimate are excluded — owned by UpdateRating.
func (s *DB) upsertActionStats(ctx context.Context, tx *sql.Tx, stats *kernel.Stats, success bool, label string) error {
	if stats == nil {
		return nil
	}
	var err error
	if success {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,latency_estimate,rating_estimate,last_used_at)
			 VALUES (?,1,1,0,0,?,0,?)
			 ON CONFLICT(action_id) DO UPDATE SET
			   uses=uses+1,
			   successes=successes+1,
			   latency_estimate=latency_estimate+(excluded.latency_estimate-latency_estimate)/(uses+1),
			   last_used_at=excluded.last_used_at`,
			stats.ActionID, stats.LatencyEstimate, timeToStr(stats.LastUsedAt),
		)
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,latency_estimate,rating_estimate,last_used_at)
			 VALUES (?,1,0,1,0,?,0,?)
			 ON CONFLICT(action_id) DO UPDATE SET
			   uses=uses+1,
			   failures=failures+1,
			   latency_estimate=latency_estimate+(excluded.latency_estimate-latency_estimate)/(uses+1),
			   last_used_at=excluded.last_used_at`,
			stats.ActionID, stats.LatencyEstimate, timeToStr(stats.LastUsedAt),
		)
	}
	return dbErr(err, label+": upsert stats")
}

// completeStepTx transitions a step to done and records the tx_id atomically.
// Requires exactly one row to be affected; returns ErrInvalidState if not (B2 fix).
func (s *DB) completeStepTx(ctx context.Context, tx *sql.Tx, stepID, txID, label string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE steps SET status='done', tx_id=? WHERE id=? AND status='running'`,
		txID, stepID,
	)
	if err != nil {
		return dbErr(err, label+": complete step")
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return kernel.ErrInvalidState.Wrap("step transition to done affected unexpected rows")
	}
	return nil
}

// finalizeTx executes the shared tail of both commit paths: audit rows, trace latency,
// stats, optional idempotency completion, optional step completion, and commit.
func (s *DB) finalizeTx(ctx context.Context, tx *sql.Tx, ktx *kernel.Transaction, receipt *kernel.Receipt, stats *kernel.Stats, idempotencyRecordID, idempotencyResultJSON, stepID, label string) error {
	if err := s.insertAuditRows(ctx, tx, ktx, receipt, label); err != nil {
		return err
	}
	if err := s.upsertActionStats(ctx, tx, stats, ktx.Status == kernel.TxSuccess, label); err != nil {
		return err
	}
	if idempotencyRecordID != "" {
		receiptBytes, _ := json.Marshal(receipt)
		if _, err := tx.ExecContext(ctx,
			`UPDATE idempotency_records SET status='complete', result_json=?, receipt_json=? WHERE id=?`,
			idempotencyResultJSON, string(receiptBytes), idempotencyRecordID,
		); err != nil {
			return dbErr(err, label+": complete idempotency record")
		}
	}
	if stepID != "" {
		if err := s.completeStepTx(ctx, tx, stepID, ktx.ID, label); err != nil {
			return err
		}
	}
	return nil
}

// closeProcessTx closes a process within an existing transaction if it is quiescent:
// no waiting/running steps and no traces without a committed transaction.
// If not quiescent it is a no-op. Returns any remaining process.available to the owner.
func (s *DB) closeProcessTx(ctx context.Context, tx *sql.Tx, processID string) error {
	// Count open steps.
	var openSteps int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM steps s JOIN traces t ON s.parent_trace_id=t.id WHERE t.process_id=? AND s.status IN ('waiting','running')`,
		processID,
	).Scan(&openSteps); err != nil {
		return dbErr(err, "close process: count open steps")
	}
	if openSteps > 0 {
		return nil
	}
	// Count traces with no committed transaction (in-flight calls).
	var orphanTraces int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM traces t
		 WHERE t.process_id=?
		 AND NOT EXISTS (SELECT 1 FROM transactions WHERE trace_id=t.id)`,
		processID,
	).Scan(&orphanTraces); err != nil {
		return dbErr(err, "close process: count orphan traces")
	}
	if orphanTraces > 0 {
		return nil
	}
	// Quiescent: close and return remaining available to owner.
	var ownerID string
	var available int64
	err := tx.QueryRowContext(ctx,
		`SELECT owner_user_id, available FROM processes WHERE id=? AND status='open'`,
		processID,
	).Scan(&ownerID, &available)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already closed
	}
	if err != nil {
		return dbErr(err, "close process: read")
	}
	if available > 0 {
		if _, err = tx.ExecContext(ctx,
			`UPDATE accounts SET available=available+?, locked=locked-? WHERE id=?`,
			available, available, ownerID); err != nil {
			return dbErr(err, "close process: return available to owner")
		}
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE processes SET status='closed', available=0, locked=0, ended_at=? WHERE id=?`,
		timeToStr(time.Now().UTC()), processID)
	return dbErr(err, "close process: close")
}

// cancelStepSubtree cancels all waiting and running steps whose parent_trace_id is anywhere
// in the subtree rooted at traceID, and returns the sum of their parked prices.
// This must run inside an existing transaction.
func (s *DB) cancelStepSubtree(ctx context.Context, tx *sql.Tx, traceID string) (int64, error) {
	// Collect all trace IDs in the subtree (including traceID itself).
	var total int64
	err := tx.QueryRowContext(ctx, `
WITH RECURSIVE sub(id) AS (
    SELECT ? AS id
    UNION ALL
    SELECT t.id FROM traces t JOIN sub s ON t.parent_trace_id=s.id
)
SELECT COALESCE(SUM(price),0) FROM steps
WHERE parent_trace_id IN (SELECT id FROM sub)
  AND status IN ('waiting','running')`, traceID).Scan(&total)
	if err != nil {
		return 0, dbErr(err, "cancel step subtree: sum prices")
	}
	_, err = tx.ExecContext(ctx, `
WITH RECURSIVE sub(id) AS (
    SELECT ? AS id
    UNION ALL
    SELECT t.id FROM traces t JOIN sub s ON t.parent_trace_id=s.id
)
UPDATE steps SET status='cancelled'
WHERE parent_trace_id IN (SELECT id FROM sub)
  AND status IN ('waiting','running')`, traceID)
	if err != nil {
		return 0, dbErr(err, "cancel step subtree: cancel steps")
	}
	return total, nil
}

// CommitCall settles a successful call using trace-level wallet semantics:
//   - zeroes trace.available
//   - releases ktx.Gross (full allocated amount) from the caller wallet lock
//   - decrements owner.locked by taxable (= net+fee, what actually settled)
//   - credits net to targetUserID, fee to feeRecipientID
//
// callerWalletKind controls the lock release: CallerProcess (process.locked),
// CallerTrace (parent trace.locked), or CallerStep (no lock to release; BeginStepCall
// already consumed it — refund would go to process on failure).
// applyPremiumLegs releases the serving-markup reserve the peer owner parked in its locked balance at
// admission (§13). It is read from the trace snapshot (premium_parked) rather than caller arguments, so
// EVERY settlement path — commit, failure, crash recovery, forced closure — releases it without the
// in-memory request (a no-op when nothing is parked). The whole parked amount leaves owner.locked; the
// actual `premium` goes to sys and the unused remainder refunds to the owner. The value-transfer channel
// is settled separately on the caller's own reserve (settleTransferReserve), never here.
func applyPremiumLegs(ctx context.Context, tx *sql.Tx, traceID, ownerID, sysID string, premium int64) error {
	var reserve int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(premium_parked,0) FROM traces WHERE id=?`, traceID,
	).Scan(&reserve); err != nil {
		return dbErr(err, "premium legs: read reserve")
	}
	if reserve == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET locked=locked-? WHERE id=?`, reserve, ownerID); err != nil {
		return dbErr(err, "premium legs: release owner reserve")
	}
	if refund := reserve - premium; refund > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET available=available+? WHERE id=?`, refund, ownerID); err != nil {
			return dbErr(err, "premium legs: refund unused reserve")
		}
	}
	if premium > 0 {
		if sysID == "" {
			return fmt.Errorf("premium legs: premium %d > 0 but sysID is empty: funds would be destroyed", premium)
		}
		res, err := tx.ExecContext(ctx, `UPDATE accounts SET available=available+? WHERE id=?`, premium, sysID)
		if err != nil {
			return dbErr(err, "premium legs: credit sys premium")
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("premium legs: sys recipient %q not found: funds would be destroyed", sysID)
		}
	}
	return nil
}

// releaseTransferValue disposes of the value a transfer locked on the caller C (§13). The value channel
// is local and untaxed, so exactly what was locked is what moves: the whole amount leaves C.locked and
// is credited to `toID` — the beneficiary at commit, or C itself on failure. Delivery is all-or-nothing;
// there is no remainder to refund and no fee to levy. Both legs are guarded (RowsAffected==1) so a
// missing account fails closed rather than destroying funds. A zero value is a no-op.
func releaseTransferValue(ctx context.Context, tx *sql.Tx, callerC string, value int64, toID string) error {
	if value == 0 {
		return nil
	}
	if toID == "" {
		return fmt.Errorf("transfer settle: value %d > 0 but no recipient: funds would be destroyed", value)
	}
	res, err := tx.ExecContext(ctx, `UPDATE accounts SET locked=locked-? WHERE id=?`, value, callerC)
	if err != nil {
		return dbErr(err, "transfer settle: release caller lock")
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("transfer settle: caller %q not found: funds would be destroyed", callerC)
	}
	res, err = tx.ExecContext(ctx, `UPDATE accounts SET available=available+? WHERE id=?`, value, toID)
	if err != nil {
		return dbErr(err, "transfer settle: credit recipient")
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("transfer settle: recipient %q not found: funds would be destroyed", toID)
	}
	return nil
}

// transferValueOf reads a trace's value snapshot: the amount locked on the caller and the beneficiary
// it is owed to. Reading it from the trace rather than the in-memory request is what lets every
// settlement path release the lock identically (§13).
func transferValueOf(ctx context.Context, tx *sql.Tx, traceID string) (callerC string, value int64, valueTo string, err error) {
	var to sql.NullString
	if err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(value,0), caller_user_id, value_to FROM traces WHERE id=?`, traceID,
	).Scan(&value, &callerC, &to); err != nil {
		return "", 0, "", dbErr(err, "transfer: read trace value")
	}
	return callerC, value, to.String, nil
}

// commitTraceTransferEffect delivers a trace's transfer value to its beneficiary (§13) and journals
// the movement. The transaction records the execution channel (gross/net/fee); the delivered value is
// a balance movement between two users, so it is recorded where every such movement is — one ledger
// entry, written in this same commit, naming C as authorizer and source and carrying the settling
// transaction's id as its reason. Without it the beneficiary — no party to the transaction — would
// see credit arrive with no readable record (§3 U5, U15). The id derives from the transaction (like
// a settlement's), so one delivery can never be journalled twice.
func commitTraceTransferEffect(ctx context.Context, tx *sql.Tx, traceID, txID string, at time.Time) error {
	callerC, value, valueTo, err := transferValueOf(ctx, tx, traceID)
	if err != nil {
		return err
	}
	if err := releaseTransferValue(ctx, tx, callerC, value, valueTo); err != nil {
		return err
	}
	if value == 0 {
		return nil
	}
	return insertLedgerRow(ctx, tx, &kernel.LedgerEntry{
		ID: "tv_" + txID, OperatorUserID: callerC, FromUserID: callerC, ToUserID: valueTo,
		Amount: value, Reason: txID, CreatedAt: at,
	})
}

// refundTransferEffect returns a trace's transfer value to the caller C — the disposition for a failed
// or interrupted transfer, where delivery is all-or-nothing (§13).
func refundTransferEffect(ctx context.Context, tx *sql.Tx, traceID string) error {
	callerC, value, _, err := transferValueOf(ctx, tx, traceID)
	if err != nil {
		return err
	}
	return releaseTransferValue(ctx, tx, callerC, value, callerC)
}

func (s *DB) CommitCall(ctx context.Context, ktx *kernel.Transaction, receipt *kernel.Receipt, traceID, callerWalletID, callerWalletKind, targetUserID, feeRecipientID string, net, fee int64, stats *kernel.Stats, idempotencyRecordID, stepID string) error {
	return s.withTx(ctx, "commit call", func(tx *sql.Tx) error {
		taxable := net + fee
		// Zero out trace.available (taxable flows out; the rest was consumed by subcalls/steps).
		if _, err := tx.ExecContext(ctx,
			`UPDATE traces SET available=0 WHERE id=?`, traceID); err != nil {
			return dbErr(err, "commit call: zero trace available")
		}
		// Release the full gross from the caller's lock row (per callerWalletKind, see doc above).
		// Each settled subcall already released its own gross from this row, so it now holds
		// exactly this call's gross.
		if ktx.Gross > 0 {
			switch callerWalletKind {
			case kernel.CallerProcess:
				if _, err := tx.ExecContext(ctx,
					`UPDATE processes SET locked=locked-? WHERE id=?`, ktx.Gross, callerWalletID); err != nil {
					return dbErr(err, "commit call: release process lock")
				}
			case kernel.CallerTrace:
				if _, err := tx.ExecContext(ctx,
					`UPDATE traces SET locked=locked-? WHERE id=?`, ktx.Gross, callerWalletID); err != nil {
					return dbErr(err, "commit call: release parent trace lock")
				}
				// CallerStep: BeginStepCall already released the lock; nothing to do here.
			}
		}
		// Decrement owner.locked by taxable (only the portion that settles to target/sys).
		if taxable > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE accounts SET locked=locked-? WHERE id=?`, taxable, ktx.OwnerUserID); err != nil {
				return dbErr(err, "commit call: debit owner locked")
			}
		}
		if net > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE accounts SET available=available+? WHERE id=?`, net, targetUserID); err != nil {
				return dbErr(err, "commit call: credit target")
			}
		}
		if fee > 0 {
			if feeRecipientID == "" {
				return fmt.Errorf("commit call: fee %d > 0 but feeRecipientID is empty: funds would be destroyed", fee)
			}
			res, feeErr := tx.ExecContext(ctx,
				`UPDATE accounts SET available=available+? WHERE id=?`, fee, feeRecipientID)
			if feeErr != nil {
				return dbErr(feeErr, "commit call: credit fee recipient")
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return fmt.Errorf("commit call: fee recipient %q not found: funds would be destroyed", feeRecipientID)
			}
		}
		// Serving-markup premium (§13): release the execution reserve snapshotted on the trace, credit
		// receipt.Premium to sys, refund the remainder. No-op for local calls/subcalls.
		if err := applyPremiumLegs(ctx, tx, traceID, ktx.OwnerUserID, feeRecipientID, receipt.Premium); err != nil {
			return err
		}
		// Value channel (§13): a local/inbound transfer credits its beneficiary from the caller C's own
		// reserve, untaxed and on a different wallet than the execution premium above. No-op otherwise.
		if err := commitTraceTransferEffect(ctx, tx, traceID, ktx.ID, receipt.CreatedAt); err != nil {
			return err
		}
		if err := s.finalizeTx(ctx, tx, ktx, receipt, stats, idempotencyRecordID, rawJSONStr(ktx.ReplyJSON), stepID, "commit call"); err != nil {
			return err
		}
		return s.closeProcessTx(ctx, tx, ktx.ProcessID)
	})
}

// CommitFailedCall settles a failed call:
//   - cancels all outstanding steps in the trace's subtree, summing their parked prices
//   - total refund = trace.available + step prices
//   - refunds total to caller wallet (process or parent trace); CallerStep → process.available
func (s *DB) CommitFailedCall(ctx context.Context, ktx *kernel.Transaction, buildReceipt func(refund int64) (*kernel.Receipt, error), traceID, callerWalletID, callerWalletKind, feeRecipientID string, gross int64, stats *kernel.Stats, idempotencyRecordID, errorCode, stepID string) error {
	return s.withTx(ctx, "commit failed call", func(tx *sql.Tx) error {
		// Read trace.available before zeroing.
		var traceAvailable int64
		if err := tx.QueryRowContext(ctx,
			`SELECT available FROM traces WHERE id=?`, traceID,
		).Scan(&traceAvailable); err != nil {
			return dbErr(err, "commit failed call: read trace available")
		}
		// Cancel subtree steps and collect their parked prices.
		stepPrices, err := s.cancelStepSubtree(ctx, tx, traceID)
		if err != nil {
			return err
		}
		refund := traceAvailable + stepPrices
		// Record what actually came back, on the same row the payer audits (§3 D4). The local law is
		// not the remote identity gross−net−fee: a failed call charges no fee or net, yet already
		// settled descendants stay paid, so the returned amount is the unspent allocation plus the
		// parked prices of the steps this rollup cancels.
		ktx.Refund = refund
		// Build and sign the receipt inside the transaction so that charge (gross − refund)
		// is guaranteed to match what is committed — no TOCTOU window.
		receipt, err := buildReceipt(refund)
		if err != nil {
			return err
		}
		// Zero trace.available.
		if _, err = tx.ExecContext(ctx,
			`UPDATE traces SET available=0 WHERE id=?`, traceID); err != nil {
			return dbErr(err, "commit failed call: zero trace available")
		}
		// Return refund to caller wallet and release the gross lock.
		switch callerWalletKind {
		case kernel.CallerProcess:
			if _, err = tx.ExecContext(ctx,
				`UPDATE processes SET available=available+?, locked=locked-? WHERE id=?`,
				refund, gross, callerWalletID); err != nil {
				return dbErr(err, "commit failed call: refund process")
			}
		case kernel.CallerTrace:
			if _, err = tx.ExecContext(ctx,
				`UPDATE traces SET available=available+?, locked=locked-? WHERE id=?`,
				refund, gross, callerWalletID); err != nil {
				return dbErr(err, "commit failed call: refund parent trace")
			}
		case kernel.CallerStep:
			// BeginStepCall already released the parent trace lock. Return refund to process.
			if refund > 0 {
				if _, err = tx.ExecContext(ctx,
					`UPDATE processes SET available=available+? WHERE id=?`,
					refund, ktx.ProcessID); err != nil {
					return dbErr(err, "commit failed call: refund step to process")
				}
			}
		}
		// NOTE: user.locked is NOT decremented here. Permanent outflows were already
		// debited by CommitCall for each successful subcall. The refunded amount will be
		// returned to user.available (via EndProcess or caller wallet propagation) and
		// user.locked will be decremented then. Touching it here would double-count.
		// The serving-markup premium reserve is a separate parked amount (not part of the process
		// budget or the refund flow above), so it is released here in full — receipt.Premium (on the
		// actual failed charge) to sys, the remainder back to the peer owner. No-op for local calls.
		if err := applyPremiumLegs(ctx, tx, traceID, ktx.OwnerUserID, feeRecipientID, receipt.Premium); err != nil {
			return err
		}
		// Value channel (§13): a failed/never-dispatched transfer delivers nothing, so the whole value
		// reserve returns to the caller C. No-op for non-transfer calls.
		if err := refundTransferEffect(ctx, tx, traceID); err != nil {
			return err
		}
		errResult, _ := json.Marshal(map[string]string{"error": ktx.Reason, "code": errorCode})
		if err := s.finalizeTx(ctx, tx, ktx, receipt, stats, idempotencyRecordID, string(errResult), stepID, "commit failed call"); err != nil {
			return err
		}
		return s.closeProcessTx(ctx, tx, ktx.ProcessID)
	})
}

// CommitRemoteSettlement settles an outbound remote-proxy call with economics distinct from local
// calls (§13): the gross q (= the two-step local price sr + ceil(sr·import_bps)) was locked; paid
// (= the peer's charge + serving premium) flows to the proxy user as the bilateral payable, the
// origin's import fee to @sys, and the remainder (refund = q−paid−importFee) returns to the caller
// wallet. Unlike CommitCall, taxable = paid+importFee (not gross), so the refund must be explicit.
func (s *DB) CommitRemoteSettlement(ctx context.Context, ktx *kernel.Transaction, receipt *kernel.Receipt, traceID, callerWalletID, callerWalletKind, proxyUserID, feeRecipientID string, paid, importFee int64, stats *kernel.Stats, idempotencyRecordID, stepID, errorCode string) error {
	return s.withTx(ctx, "commit remote settlement", func(tx *sql.Tx) error {
		q := ktx.Gross // full locked amount (two-step local price)
		taxable := paid + importFee
		refund := q - paid - importFee
		ktx.Refund = refund
		// Zero trace.available.
		if _, err := tx.ExecContext(ctx, `UPDATE traces SET available=0 WHERE id=?`, traceID); err != nil {
			return dbErr(err, "commit remote settlement: zero trace available")
		}
		// Release caller wallet lock and return refund.
		switch callerWalletKind {
		case kernel.CallerProcess:
			if _, err := tx.ExecContext(ctx,
				`UPDATE processes SET locked=locked-?, available=available+? WHERE id=?`,
				q, refund, callerWalletID); err != nil {
				return dbErr(err, "commit remote settlement: release process lock")
			}
		case kernel.CallerTrace:
			if _, err := tx.ExecContext(ctx,
				`UPDATE traces SET locked=locked-?, available=available+? WHERE id=?`,
				q, refund, callerWalletID); err != nil {
				return dbErr(err, "commit remote settlement: release parent trace lock")
			}
		case kernel.CallerStep:
			// BeginStepCall already released the parent trace lock.
			if refund > 0 {
				if _, err := tx.ExecContext(ctx,
					`UPDATE processes SET available=available+? WHERE id=?`,
					refund, ktx.ProcessID); err != nil {
					return dbErr(err, "commit remote settlement: refund step to process")
				}
			}
		}
		// Decrement owner.locked by taxable (permanently committed portion).
		if taxable > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE accounts SET locked=locked-? WHERE id=?`, taxable, ktx.OwnerUserID); err != nil {
				return dbErr(err, "commit remote settlement: debit owner locked")
			}
		}
		// Pay paid (charge + serving premium) to the proxy user — the bilateral payable to the peer.
		if paid > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE accounts SET available=available+? WHERE id=?`, paid, proxyUserID); err != nil {
				return dbErr(err, "commit remote settlement: credit proxy user")
			}
		}
		// Retain the import fee locally on the origin's @sys.
		if importFee > 0 {
			if feeRecipientID == "" {
				return fmt.Errorf("commit remote settlement: importFee %d > 0 but feeRecipientID is empty", importFee)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE accounts SET available=available+? WHERE id=?`, importFee, feeRecipientID); err != nil {
				return dbErr(err, "commit remote settlement: credit fee recipient")
			}
		}
		// A remote FAILURE has no ReplyJSON (settleRemoteCall sets it only on success), so storing
		// it verbatim would complete the record with "null" — and a replaying peer, finding no
		// "error" key, would read a settled failure as a 200 success. Store the same error body a
		// local failure stores, so both replay through one rule (§13).
		idemResult := rawJSONStr(ktx.ReplyJSON)
		if ktx.Status != kernel.TxSuccess {
			errResult, _ := json.Marshal(map[string]string{"error": ktx.Reason, "code": errorCode})
			idemResult = string(errResult)
		}
		if err := s.finalizeTx(ctx, tx, ktx, receipt, stats, idempotencyRecordID, idemResult, stepID, "commit remote settlement"); err != nil {
			return err
		}
		return s.closeProcessTx(ctx, tx, ktx.ProcessID)
	})
}

// scanProcessFn is the queryList adapter for process lists.
func scanProcessFn(scan func(...any) error) (*kernel.Process, error) {
	var p kernel.Process
	var status, createdAt string
	var endedAt *string
	if err := scan(&p.ID, &p.OwnerUserID, &p.Available, &p.Locked, &status, &createdAt, &endedAt); err != nil {
		return nil, err
	}
	p.Status = kernel.ProcessStatus(status)
	p.CreatedAt = strToTime(createdAt)
	p.EndedAt = strToNullTime(endedAt)
	return &p, nil
}

func (s *DB) ListProcesses(ctx context.Context, ownerID string, limit, offset int) ([]*kernel.Process, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,owner_user_id,available,locked,status,created_at,ended_at
		 FROM processes WHERE owner_user_id=? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		ownerID, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list processes")
	}
	defer rows.Close()
	return queryList(rows, "list processes", scanProcessFn)
}

func (s *DB) ListAllProcesses(ctx context.Context, limit, offset int) ([]*kernel.Process, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,owner_user_id,available,locked,status,created_at,ended_at
		 FROM processes ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list all processes")
	}
	defer rows.Close()
	return queryList(rows, "list processes", scanProcessFn)
}

func (s *DB) EndProcess(ctx context.Context, processID string) error {
	return s.withTx(ctx, "end process", func(tx *sql.Tx) error {
		var ownerID string
		var available, locked int64
		err := tx.QueryRowContext(ctx,
			`SELECT owner_user_id, available, locked FROM processes WHERE id=? AND status='open'`,
			processID,
		).Scan(&ownerID, &available, &locked)
		if errors.Is(err, sql.ErrNoRows) {
			return kernel.ErrInvalidState.Wrap("process not open")
		}
		if err != nil {
			return dbErr(err, "end process: read")
		}
		// Running step-completion traces are settled as failed calls (transaction + receipt)
		// by kernel.EndProcess via recoverTrace before this runs, so by now no step is in
		// the running state. This method only cancels waiting steps and returns funds.
		// Cancel all waiting steps and collect parked prices to return to owner.
		var parkedTotal int64
		if err = tx.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(s.price),0) FROM steps s JOIN traces t ON s.parent_trace_id=t.id WHERE t.process_id=? AND s.status='waiting'`,
			processID,
		).Scan(&parkedTotal); err != nil {
			return dbErr(err, "end process: sum parked prices")
		}
		if parkedTotal > 0 {
			// Release parked prices: remove from parent trace locks, return to user.
			// We cancel the steps in bulk; the trace.locked decrements must also happen.
			rows, err2 := tx.QueryContext(ctx,
				`SELECT s.parent_trace_id, SUM(s.price) FROM steps s JOIN traces t ON s.parent_trace_id=t.id WHERE t.process_id=? AND s.status='waiting' GROUP BY s.parent_trace_id`,
				processID)
			if err2 != nil {
				return dbErr(err2, "end process: group parked by trace")
			}
			var traceParks []struct {
				traceID string
				amount  int64
			}
			for rows.Next() {
				var traceID *string
				var amount int64
				if err3 := rows.Scan(&traceID, &amount); err3 != nil {
					rows.Close()
					return dbErr(err3, "end process: scan trace park")
				}
				if traceID != nil {
					traceParks = append(traceParks, struct {
						traceID string
						amount  int64
					}{*traceID, amount})
				}
			}
			rows.Close()
			for _, tp := range traceParks {
				if _, err2 = tx.ExecContext(ctx,
					`UPDATE traces SET locked=locked-? WHERE id=?`, tp.amount, tp.traceID); err2 != nil {
					return dbErr(err2, "end process: release trace lock")
				}
			}
			// Return parked prices to owner as available.
			if _, err2 = tx.ExecContext(ctx,
				`UPDATE accounts SET available=available+?, locked=locked-? WHERE id=?`,
				parkedTotal, parkedTotal, ownerID); err2 != nil {
				return dbErr(err2, "end process: return parked prices to owner")
			}
		}
		if _, err = tx.ExecContext(ctx,
			`UPDATE steps SET status='cancelled' WHERE id IN (SELECT s.id FROM steps s JOIN traces t ON s.parent_trace_id=t.id WHERE t.process_id=? AND s.status='waiting')`,
			processID); err != nil {
			return dbErr(err, "end process: cancel waiting steps")
		}
		now := timeToStr(time.Now().UTC())
		returnAmount := available
		if returnAmount > 0 {
			if _, err = tx.ExecContext(ctx,
				`UPDATE accounts SET available=available+?, locked=locked-? WHERE id=?`,
				returnAmount, returnAmount, ownerID); err != nil {
				return dbErr(err, "end process: return available to owner")
			}
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE processes SET status='closed', available=0, locked=0, ended_at=? WHERE id=?`,
			now, processID)
		return dbErr(err, "end process: close")
	})
}

// ---- Traces ----

const traceCols = `id,process_id,parent_trace_id,action_owner_id,action_id,caller_user_id,available,locked,idempotency_key,dispatch_json,idempotency_record_id,premium_bps,premium_parked,value,value_to,created_at`

func scanTrace(t *kernel.Trace, scanFn func(...any) error) error {
	var createdAt string
	var parentID, idempotencyKey, dispatchJSON, recordID, valueTo sql.NullString
	var premiumBPS, premiumParked, value sql.NullInt64
	err := scanFn(&t.ID, &t.ProcessID, &parentID, &t.ActionOwnerID, &t.ActionID, &t.CallerUserID,
		&t.Available, &t.Locked, &idempotencyKey, &dispatchJSON, &recordID, &premiumBPS, &premiumParked, &value, &valueTo, &createdAt)
	if err != nil {
		return err
	}
	t.CreatedAt = strToTime(createdAt)
	t.PremiumBPS = premiumBPS.Int64
	t.PremiumParked = premiumParked.Int64
	t.Value = value.Int64
	t.ValueTo = valueTo.String
	if parentID.Valid {
		t.ParentTraceID = &parentID.String
	}
	if recordID.Valid {
		t.IdempotencyRecordID = &recordID.String
	}
	if idempotencyKey.Valid {
		t.IdempotencyKey = &idempotencyKey.String
	}
	if dispatchJSON.Valid {
		t.DispatchJSON = &dispatchJSON.String
	}
	return nil
}

// scanTracePtr is the queryList adapter for trace lists.
func scanTracePtr(scan func(...any) error) (*kernel.Trace, error) {
	var t kernel.Trace
	if err := scanTrace(&t, scan); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *DB) ReadTrace(ctx context.Context, id string) (*kernel.Trace, error) {
	var t kernel.Trace
	err := scanTrace(&t, s.db.QueryRowContext(ctx,
		`SELECT `+traceCols+` FROM traces WHERE id=?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("trace not found")
	}
	if err != nil {
		return nil, dbErr(err, "read trace")
	}
	return &t, nil
}

func (s *DB) ReadRootTrace(ctx context.Context, processID string) (*kernel.Trace, error) {
	var t kernel.Trace
	err := scanTrace(&t, s.db.QueryRowContext(ctx,
		`SELECT `+traceCols+` FROM traces WHERE process_id=? AND parent_trace_id IS NULL LIMIT 1`, processID,
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("root trace not found for process")
	}
	if err != nil {
		return nil, dbErr(err, "read root trace")
	}
	return &t, nil
}

// TraceHasTransaction reports whether a transaction row exists for the trace (settled).
func (s *DB) TraceHasTransaction(ctx context.Context, traceID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM transactions WHERE trace_id=?)`, traceID).Scan(&exists)
	if err != nil {
		return false, dbErr(err, "trace has transaction")
	}
	return exists, nil
}

// ---- Transactions ----

const txColumns = `id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,target_user_id,` +
	`action_id,action_name,remote_action_id,args_json,reply_json,status,gross,net,fee,refund,reason,remote_receipt_hash,remote_receipt_json,started_at,ended_at`

// scanTx scans one transaction row using the provided scan function.
// scan must be called with exactly the destinations expected by txColumns.
func scanTx(scan func(...any) error) (kernel.Transaction, error) {
	var tx kernel.Transaction
	var status, startedAt, endedAt, argsJSON, replyJSON string
	var remoteReceiptHash *string
	if err := scan(&tx.ID, &tx.ProcessID, &tx.TraceID, &tx.ParentTraceID,
		&tx.OwnerUserID, &tx.CallerUserID, &tx.TargetUserID, &tx.ActionID, &tx.ActionName, &tx.RemoteActionID,
		&argsJSON, &replyJSON, &status,
		&tx.Gross, &tx.Net, &tx.Fee, &tx.Refund, &tx.Reason, &remoteReceiptHash, &tx.RemoteReceiptJSON,
		&startedAt, &endedAt); err != nil {
		return tx, err
	}
	tx.ArgsJSON = strToRawJSON(argsJSON)
	tx.ReplyJSON = strToRawJSON(replyJSON)
	tx.Status = kernel.TxStatus(status)
	tx.RemoteReceiptHash = strVal(remoteReceiptHash)
	tx.StartedAt = strToTime(startedAt)
	tx.EndedAt = strToTime(endedAt)
	return tx, nil
}

// scanTxPtr is the queryList adapter for transaction lists.
func scanTxPtr(scan func(...any) error) (*kernel.Transaction, error) {
	tx, err := scanTx(scan)
	if err != nil {
		return nil, err
	}
	return &tx, nil
}

func (s *DB) ReadTransaction(ctx context.Context, id string) (*kernel.Transaction, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+txColumns+` FROM transactions WHERE id=?`, id)
	tx, err := scanTx(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("transaction not found")
	}
	if err != nil {
		return nil, dbErr(err, "read transaction")
	}
	return &tx, nil
}

func (s *DB) ListTransactions(ctx context.Context, f kernel.TxFilter) ([]*kernel.Transaction, error) {
	q := `SELECT ` + txColumns + ` FROM transactions WHERE 1=1`
	args := []any{}
	if f.OwnerUserID != "" {
		q += ` AND owner_user_id=?`
		args = append(args, f.OwnerUserID)
	}
	if f.CallerUserID != "" {
		q += ` AND caller_user_id=?`
		args = append(args, f.CallerUserID)
	}
	if f.TargetUserID != "" {
		q += ` AND target_user_id=?`
		args = append(args, f.TargetUserID)
	}
	if f.ProcessID != "" {
		q += ` AND process_id=?`
		args = append(args, f.ProcessID)
	}
	if f.PartyUserID != "" {
		q += ` AND (owner_user_id=? OR caller_user_id=? OR target_user_id=?)`
		args = append(args, f.PartyUserID, f.PartyUserID, f.PartyUserID)
	}
	q += ` ORDER BY started_at DESC`
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	q += ` LIMIT ? OFFSET ?`
	args = append(args, limit, f.Offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, dbErr(err, "list transactions")
	}
	return queryList(rows, "list transactions", scanTxPtr)
}

// ---- Stats ----

func (s *DB) ReadStats(ctx context.Context, actionID string) (*kernel.Stats, error) {
	var st kernel.Stats
	var lastUsed string
	err := s.db.QueryRowContext(ctx,
		`SELECT action_id,uses,successes,failures,rating_count,latency_estimate,rating_estimate,last_used_at
		 FROM action_stats WHERE action_id=?`, actionID,
	).Scan(&st.ActionID, &st.Uses, &st.Successes, &st.Failures, &st.RatingCount,
		&st.LatencyEstimate, &st.RatingEstimate, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // no stats yet is not an error
	}
	if err != nil {
		return nil, dbErr(err, "read stats")
	}
	st.LastUsedAt = strToTime(lastUsed)
	return &st, nil
}

func (s *DB) UpsertStats(ctx context.Context, st *kernel.Stats) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,latency_estimate,rating_estimate,last_used_at)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(action_id) DO UPDATE SET
		   uses=excluded.uses, successes=excluded.successes, failures=excluded.failures,
		   rating_count=excluded.rating_count,
		   latency_estimate=excluded.latency_estimate, rating_estimate=excluded.rating_estimate,
		   last_used_at=excluded.last_used_at`,
		st.ActionID, st.Uses, st.Successes, st.Failures, st.RatingCount,
		st.LatencyEstimate, st.RatingEstimate, timeToStr(st.LastUsedAt),
	)
	return dbErr(err, "upsert stats")
}

// ---- Steps ----

// CreateStep atomically inserts the step and parks step.price from the parent trace's
// available into its locked. Returns ErrInsufficientFunds if parent_trace.available < price.
func (s *DB) CreateStep(ctx context.Context, step *kernel.Step) error {
	return s.withTx(ctx, "create step", func(tx *sql.Tx) error {
		if step.ParentTraceID != nil && step.Price > 0 {
			res, err := tx.ExecContext(ctx,
				`UPDATE traces SET available=available-?, locked=locked+?
				 WHERE id=? AND available>=?`,
				step.Price, step.Price, *step.ParentTraceID, step.Price,
			)
			if err != nil {
				return dbErr(err, "create step: park price")
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return kernel.ErrInsufficientFunds.Wrap("parent trace has insufficient available funds for step")
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO steps (id,parent_trace_id,required_caller_user_id,required_caller_remote_id,action_id,
			                    partial_args,price,import_bps,status,created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			step.ID, step.ParentTraceID, step.RequiredCallerUserID, step.RequiredCallerRemoteID,
			step.ActionID, rawJSONStr(step.PartialArgs),
			step.Price, step.ImportBPS, string(step.Status), timeToStr(step.CreatedAt),
		)
		return dbErr(err, "create step: insert")
	})
}

const stepCols = `id,parent_trace_id,required_caller_user_id,required_caller_remote_id,action_id,partial_args,price,import_bps,status,tx_id,completion_trace_id,created_at`

func scanStep(step *kernel.Step, scanFn func(...any) error) error {
	var parentTraceID, txID, completionTraceID, remoteID *string
	var createdAt, partialArgs, status string
	var importBPS sql.NullInt64
	if err := scanFn(&step.ID, &parentTraceID, &step.RequiredCallerUserID, &remoteID,
		&step.ActionID, &partialArgs, &step.Price, &importBPS, &status, &txID, &completionTraceID, &createdAt); err != nil {
		return err
	}
	if importBPS.Valid {
		v := importBPS.Int64
		step.ImportBPS = &v
	}
	step.RequiredCallerRemoteID = remoteID
	step.ParentTraceID = parentTraceID
	step.PartialArgs = strToRawJSON(partialArgs)
	step.Status = kernel.StepStatus(status)
	step.TxID = txID
	step.CompletionTraceID = completionTraceID
	step.CreatedAt = strToTime(createdAt)
	return nil
}

func (s *DB) ReadStep(ctx context.Context, id string) (*kernel.Step, error) {
	var step kernel.Step
	err := scanStep(&step, s.db.QueryRowContext(ctx,
		`SELECT `+stepCols+` FROM steps WHERE id=?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("step not found")
	}
	if err != nil {
		return nil, dbErr(err, "read step")
	}
	return &step, nil
}

func (s *DB) ListSteps(ctx context.Context, callerUserID, processID, status string, isSuperuser bool, limit, offset int) ([]*kernel.Step, error) {
	superInt := 0
	if isSuperuser {
		superInt = 1
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+stepCols+`
		 FROM steps
		 WHERE (parent_trace_id IN (
		            SELECT id FROM traces WHERE process_id IN (
		                SELECT id FROM processes WHERE owner_user_id=?))
		        OR required_caller_user_id=?
		        OR ?)
		   AND (?='' OR parent_trace_id IN (SELECT id FROM traces WHERE process_id=?))
		   AND (?='' OR status=?)
		 ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		callerUserID, callerUserID, superInt,
		processID, processID,
		status, status,
		limit, offset,
	)
	if err != nil {
		return nil, dbErr(err, "list steps")
	}
	return queryList(rows, "list steps", func(scan func(...any) error) (*kernel.Step, error) {
		var step kernel.Step
		if err := scanStep(&step, scan); err != nil {
			return nil, err
		}
		return &step, nil
	})
}

// scanOrphanRunningStep is the queryList adapter shared by the orphan-running-step lists.
func scanOrphanRunningStep(scan func(...any) error) (kernel.OrphanRunningStep, error) {
	var row kernel.OrphanRunningStep
	var hasSettled int
	err := scan(&row.StepID, &row.CompletionTraceID, &row.Price, &row.ParentTraceID,
		&row.TraceAvailable, &row.TraceLocked, &hasSettled)
	row.HasSettled = hasSettled == 1
	return row, err
}

// ListOrphanRunningSteps returns running steps that have a completion trace but no tx,
// with enough detail to decide between re-parking (empty trace) or settling as failed.
// HasSettled is true when the completion trace has locked funds or committed subcall transactions.
func (s *DB) ListOrphanRunningSteps(ctx context.Context) ([]kernel.OrphanRunningStep, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT st.id, st.completion_trace_id, st.price, st.parent_trace_id,
		       t.available, t.locked,
		       CASE WHEN t.locked > 0 OR EXISTS (
		           WITH RECURSIVE sub(id) AS (
		               SELECT st.completion_trace_id
		               UNION ALL
		               SELECT ch.id FROM traces ch JOIN sub ON ch.parent_trace_id = sub.id
		           )
		           SELECT 1 FROM transactions tx WHERE tx.trace_id IN (SELECT id FROM sub)
		       ) THEN 1 ELSE 0 END AS has_settled
		FROM steps st
		JOIN traces t ON t.id = st.completion_trace_id
		WHERE st.status = 'running' AND st.tx_id IS NULL AND st.completion_trace_id IS NOT NULL`)
	if err != nil {
		return nil, dbErr(err, "list orphan running steps")
	}
	return queryList(rows, "list orphan running steps", scanOrphanRunningStep)
}

// ListOrphanRunningStepsForProcess is ListOrphanRunningSteps scoped to a single process.
// Used by EndProcess to map each running step-completion trace back to its step so the
// completion call can be failed (transaction + receipt) rather than drained.
func (s *DB) ListOrphanRunningStepsForProcess(ctx context.Context, processID string) ([]kernel.OrphanRunningStep, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT st.id, st.completion_trace_id, st.price, st.parent_trace_id,
		       t.available, t.locked,
		       CASE WHEN t.locked > 0 OR EXISTS (
		           WITH RECURSIVE sub(id) AS (
		               SELECT st.completion_trace_id
		               UNION ALL
		               SELECT ch.id FROM traces ch JOIN sub ON ch.parent_trace_id = sub.id
		           )
		           SELECT 1 FROM transactions tx WHERE tx.trace_id IN (SELECT id FROM sub)
		       ) THEN 1 ELSE 0 END AS has_settled
		FROM steps st
		JOIN traces t ON t.id = st.completion_trace_id
		WHERE st.status = 'running' AND st.tx_id IS NULL AND st.completion_trace_id IS NOT NULL
		  AND t.process_id = ?`, processID)
	if err != nil {
		return nil, dbErr(err, "list orphan running steps for process")
	}
	return queryList(rows, "list orphan running steps for process", scanOrphanRunningStep)
}

// ResetStepAndRepark re-parks the step: it moves the completion trace's available funds back into
// the parent trace's locked position (the original park), deletes the empty completion trace,
// clears completion_trace_id, and resets the step to waiting. This prevents double-completion minting.
func (s *DB) ResetStepAndRepark(ctx context.Context, stepID string) error {
	return s.withTx(ctx, "reset step and repark", func(tx *sql.Tx) error {
		var price int64
		var completionTraceID, parentTraceID *string
		err := tx.QueryRowContext(ctx,
			`SELECT price, completion_trace_id, parent_trace_id FROM steps WHERE id=? AND status='running' AND tx_id IS NULL`,
			stepID,
		).Scan(&price, &completionTraceID, &parentTraceID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // nothing to do
		}
		if err != nil {
			return dbErr(err, "reset step and repark: read step")
		}
		if completionTraceID != nil && price > 0 && parentTraceID != nil {
			// Verify the completion trace is truly empty before operating on it.
			// available must equal price (nothing committed downstream) and locked must be 0.
			var traceAvailable, traceLocked int64
			if err = tx.QueryRowContext(ctx,
				`SELECT available, locked FROM traces WHERE id=?`, *completionTraceID,
			).Scan(&traceAvailable, &traceLocked); err != nil {
				return dbErr(err, "reset step and repark: read trace")
			}
			if traceAvailable != price || traceLocked != 0 {
				return kernel.ErrInvalidState.Wrap("completion trace is not empty; cannot re-park")
			}
			// Also reject if any descendant transaction exists: a committed subcall means
			// the trace was not truly empty even if available/locked look right.
			var descTxCount int64
			if err = tx.QueryRowContext(ctx, `
WITH RECURSIVE sub(id) AS (
    SELECT ?
    UNION ALL
    SELECT t.id FROM traces t JOIN sub s ON t.parent_trace_id=s.id
)
SELECT COUNT(*) FROM transactions WHERE trace_id IN (SELECT id FROM sub)`,
				*completionTraceID).Scan(&descTxCount); err != nil {
				return dbErr(err, "reset step and repark: check descendant transactions")
			}
			if descTxCount > 0 {
				return kernel.ErrInvalidState.Wrap("completion trace has descendant transactions; cannot re-park")
			}
			// Move funds from completion trace's available back to parent trace's locked.
			if _, err = tx.ExecContext(ctx,
				`UPDATE traces SET available=available-? WHERE id=?`, price, *completionTraceID); err != nil {
				return dbErr(err, "reset step and repark: drain completion trace")
			}
			if _, err = tx.ExecContext(ctx,
				`UPDATE traces SET locked=locked+? WHERE id=?`, price, *parentTraceID); err != nil {
				return dbErr(err, "reset step and repark: repark to parent trace")
			}
			// Delete the empty completion trace.
			if _, err = tx.ExecContext(ctx, `DELETE FROM traces WHERE id=?`, *completionTraceID); err != nil {
				return dbErr(err, "reset step and repark: delete completion trace")
			}
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE steps SET status='waiting', completion_trace_id=NULL WHERE id=?`, stepID)
		return dbErr(err, "reset step and repark: reset step")
	})
}

func (s *DB) ResetRunningSteps(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE steps SET status='waiting' WHERE status='running' AND tx_id IS NULL`)
	return dbErr(err, "reset running steps")
}

// ListStepsAwaitingCaller returns the waiting steps a given user is the required caller of,
// oldest first. Deliberately narrow: ListSteps' visibility predicate is a disjunction that also
// matches every step inside a process the caller owns, so filtering it in Go after the query's
// row cap can discard the whole page — and for a peer, the steps it can actually complete are
// exactly the ones its own inbound calls would crowd out (§13). Oldest-first because the longest
// stranded are the ones an operator needs to see. No superuser widening: this answers "what awaits
// me", which is never wider than one user.
// Steps a caller is the required completer of, oldest first. Deliberately narrow: ListSteps'
// visibility predicate is a disjunction that also matches every step inside a process the caller
// owns, so filtering it in Go after the query's row cap can discard the whole page — and for a
// peer, the steps it can actually complete are exactly the ones its own inbound calls would crowd
// out (§13). Oldest-first because the longest stranded are the ones an operator needs to see.
// No superuser widening: this answers "what awaits me", which is never wider than one user.
func (s *DB) ListStepsAwaitingCaller(ctx context.Context, requiredCallerUserID string, limit int) ([]*kernel.Step, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+stepCols+`
		 FROM steps
		 WHERE required_caller_user_id=? AND status='waiting'
		 ORDER BY created_at ASC, id ASC LIMIT ?`,
		requiredCallerUserID, limit)
	if err != nil {
		return nil, dbErr(err, "list steps awaiting caller")
	}
	return queryList(rows, "list steps awaiting caller", func(scan func(...any) error) (*kernel.Step, error) {
		var step kernel.Step
		if err := scanStep(&step, scan); err != nil {
			return nil, err
		}
		return &step, nil
	})
}

// nullStr converts an empty string to nil for nullable TEXT columns.
func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// rawJSONStr returns the string form of a json.RawMessage, defaulting to "null" when empty.
func rawJSONStr(r json.RawMessage) string {
	if len(r) == 0 {
		return "null"
	}
	return string(r)
}

// strToRawJSON converts a DB string to json.RawMessage, defaulting to null when empty.
func strToRawJSON(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}

// ---- Traces (by process) ----

func (s *DB) ListTraces(ctx context.Context, processID string) ([]*kernel.Trace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+traceCols+` FROM traces WHERE process_id=?`, processID)
	if err != nil {
		return nil, dbErr(err, "list traces")
	}
	return queryList(rows, "list traces", scanTracePtr)
}

func (s *DB) ListOrphanTraces(ctx context.Context) ([]*kernel.Trace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+traceCols+` FROM traces t
		 WHERE idempotency_key IS NULL
		 AND NOT EXISTS (SELECT 1 FROM transactions tx WHERE tx.trace_id=t.id)
		 ORDER BY (
		   WITH RECURSIVE depth(id, d) AS (
		     SELECT t.id, 0
		     UNION ALL
		     SELECT p.id, d+1 FROM traces p JOIN depth ON depth.id=p.parent_trace_id
		   )
		   SELECT MAX(d) FROM depth
		 ) ASC`)
	if err != nil {
		// Fallback: simpler ordering without depth CTE for SQLite versions that struggle.
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+traceCols+` FROM traces t
			 WHERE idempotency_key IS NULL
			 AND NOT EXISTS (SELECT 1 FROM transactions tx WHERE tx.trace_id=t.id)
			 ORDER BY created_at DESC`)
		if err != nil {
			return nil, dbErr(err, "list orphan traces")
		}
	}
	return queryList(rows, "list orphan traces", scanTracePtr)
}

func (s *DB) ListPendingRemoteTraces(ctx context.Context) ([]*kernel.Trace, error) {
	// The correlation must be qualified (transactions.trace_id=traces.id): an unqualified `id`
	// binds to transactions.id, making the predicate always false and every trace look pending.
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+traceCols+` FROM traces
		 WHERE idempotency_key IS NOT NULL
		 AND NOT EXISTS (SELECT 1 FROM transactions WHERE transactions.trace_id=traces.id)`)
	if err != nil {
		return nil, dbErr(err, "list pending remote traces")
	}
	return queryList(rows, "list pending remote traces", scanTracePtr)
}

func (s *DB) ListDirectUnsettledChildren(ctx context.Context, parentTraceID string) ([]*kernel.Trace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+traceCols+` FROM traces t
		 WHERE t.parent_trace_id=?
		 AND NOT EXISTS (SELECT 1 FROM transactions tx WHERE tx.trace_id=t.id)`,
		parentTraceID)
	if err != nil {
		return nil, dbErr(err, "list direct unsettled children")
	}
	return queryList(rows, "list direct unsettled children", scanTracePtr)
}

func (s *DB) ListUnsettledTracesForProcess(ctx context.Context, processID string) ([]*kernel.Trace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+traceCols+` FROM traces t
		 WHERE t.process_id=?
		 AND NOT EXISTS (SELECT 1 FROM transactions tx WHERE tx.trace_id=t.id)
		 ORDER BY (
		   WITH RECURSIVE depth(id, d) AS (
		     SELECT t.id, 0
		     UNION ALL
		     SELECT p.id, d+1 FROM traces p JOIN depth ON depth.id=p.parent_trace_id
		   )
		   SELECT MAX(d) FROM depth
		 ) ASC`, processID)
	if err != nil {
		// Fallback: simpler ordering without depth CTE.
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+traceCols+` FROM traces t
			 WHERE t.process_id=?
			 AND NOT EXISTS (SELECT 1 FROM transactions tx WHERE tx.trace_id=t.id)
			 ORDER BY created_at DESC`, processID)
		if err != nil {
			return nil, dbErr(err, "list unsettled traces for process")
		}
	}
	return queryList(rows, "list unsettled traces for process", scanTracePtr)
}

// ---- Auth codes ----

func (s *DB) CreateAuthCode(ctx context.Context, c *kernel.AuthCode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_codes (code,user_id,code_challenge,redirect_uri,expires_at,used)
		 VALUES (?,?,?,?,?,0)`,
		c.Code, c.UserID, c.CodeChallenge, c.RedirectURI, timeToStr(c.ExpiresAt),
	)
	return dbErr(err, "create auth code")
}

func (s *DB) ConsumeAuthCode(ctx context.Context, code string) (*kernel.AuthCode, error) {
	var ac kernel.AuthCode
	if err := s.withTx(ctx, "consume auth code", func(tx *sql.Tx) error {
		var expiresAt string
		var used int
		err := tx.QueryRowContext(ctx,
			`SELECT code,user_id,code_challenge,redirect_uri,expires_at,used FROM auth_codes WHERE code=?`, code,
		).Scan(&ac.Code, &ac.UserID, &ac.CodeChallenge, &ac.RedirectURI, &expiresAt, &used)
		if errors.Is(err, sql.ErrNoRows) || used != 0 {
			return kernel.ErrUnauthenticated.Wrap("invalid or used auth code")
		}
		if err != nil {
			return dbErr(err, "read auth code")
		}
		ac.ExpiresAt = strToTime(expiresAt)
		if ac.ExpiresAt.Before(time.Now()) {
			return kernel.ErrUnauthenticated.Wrap("auth code expired")
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_codes SET used=1 WHERE code=?`, code)
		return dbErr(err, "mark auth code used")
	}); err != nil {
		return nil, err
	}
	return &ac, nil
}

// ---- Refresh tokens ----

func (s *DB) CreateRefreshToken(ctx context.Context, t *kernel.RefreshToken) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO refresh_tokens (token,user_id,expires_at,revoked,created_at)
		 VALUES (?,?,?,0,?)`,
		t.Token, t.UserID, timeToStr(t.ExpiresAt), timeToStr(t.CreatedAt),
	)
	return dbErr(err, "create refresh token")
}

func (s *DB) RotateRefreshToken(ctx context.Context, oldToken string) (*kernel.RefreshToken, error) {
	var newTok *kernel.RefreshToken
	if err := s.withTx(ctx, "rotate refresh token", func(tx *sql.Tx) error {
		var userID, expiresAt string
		var revoked int
		err := tx.QueryRowContext(ctx,
			`SELECT user_id,expires_at,revoked FROM refresh_tokens WHERE token=?`, oldToken,
		).Scan(&userID, &expiresAt, &revoked)
		if errors.Is(err, sql.ErrNoRows) || revoked != 0 {
			return kernel.ErrUnauthenticated.Wrap("invalid or revoked refresh token")
		}
		if err != nil {
			return dbErr(err, "read refresh token")
		}
		if strToTime(expiresAt).Before(time.Now()) {
			return kernel.ErrUnauthenticated.Wrap("refresh token expired")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE refresh_tokens SET revoked=1 WHERE token=?`, oldToken); err != nil {
			return dbErr(err, "revoke old refresh token")
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return kernel.ErrInternal.Wrap("failed to generate refresh token")
		}
		now := time.Now().UTC()
		newTok = &kernel.RefreshToken{
			Token:     base64.RawURLEncoding.EncodeToString(raw),
			UserID:    userID,
			ExpiresAt: now.Add(30 * 24 * time.Hour),
			CreatedAt: now,
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO refresh_tokens (token,user_id,expires_at,revoked,created_at) VALUES (?,?,?,0,?)`,
			newTok.Token, newTok.UserID, timeToStr(newTok.ExpiresAt), timeToStr(newTok.CreatedAt),
		)
		return dbErr(err, "insert new refresh token")
	}); err != nil {
		return nil, err
	}
	return newTok, nil
}

func (s *DB) RevokeRefreshToken(ctx context.Context, token string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked=1 WHERE token=? AND revoked=0`, token)
	if err != nil {
		return dbErr(err, "revoke refresh token")
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return kernel.ErrUnauthenticated.Wrap("invalid or already revoked refresh token")
	}
	return nil
}

// ---- Grants (delegated upstream OAuth, §8) ----

// CreateOrReplaceGrant upserts on (grantor_user_id, action_id): a re-consent overwrites the
// row's id, connection_id, and created_at, so a user holds at most one grant per action. A
// grant holds no secret of its own: it points at the Connection whose credential it consents to.
func (s *DB) CreateOrReplaceGrant(ctx context.Context, g *kernel.Grant) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO grants (id,grantor_user_id,action_id,connection_id,created_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(grantor_user_id,action_id) DO UPDATE SET
		   id=excluded.id, connection_id=excluded.connection_id, created_at=excluded.created_at`,
		g.ID, g.GrantorUserID, g.ActionID, nullStr(g.ConnectionID), timeToStr(g.CreatedAt),
	)
	return dbErr(err, "create grant")
}

func scanGrant(scan func(...any) error) (*kernel.Grant, error) {
	var g kernel.Grant
	var connID sql.NullString
	var createdAt string
	if err := scan(&g.ID, &g.GrantorUserID, &g.ActionID, &connID, &createdAt); err != nil {
		return nil, err
	}
	g.ConnectionID = connID.String
	g.CreatedAt = strToTime(createdAt)
	return &g, nil
}

func (s *DB) ReadGrant(ctx context.Context, grantorUserID, actionID string) (*kernel.Grant, error) {
	g, err := scanGrant(func(dest ...any) error {
		return s.db.QueryRowContext(ctx,
			`SELECT id,grantor_user_id,action_id,connection_id,created_at
			 FROM grants WHERE grantor_user_id=? AND action_id=?`, grantorUserID, actionID).Scan(dest...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("grant not found")
	}
	if err != nil {
		return nil, dbErr(err, "read grant")
	}
	return g, nil
}

func (s *DB) ListGrantsByUser(ctx context.Context, grantorUserID string) ([]*kernel.Grant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,grantor_user_id,action_id,connection_id,created_at
		 FROM grants WHERE grantor_user_id=? ORDER BY created_at DESC`, grantorUserID)
	if err != nil {
		return nil, dbErr(err, "list grants")
	}
	defer rows.Close()
	return queryList(rows, "list grants", scanGrant)
}

func (s *DB) DeleteGrant(ctx context.Context, grantorUserID, actionID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM grants WHERE grantor_user_id=? AND action_id=?`, grantorUserID, actionID)
	if err != nil {
		return dbErr(err, "delete grant")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return kernel.ErrNotFound.Wrap("grant not found")
	}
	return nil
}

func (s *DB) DeleteGrantsForAction(ctx context.Context, actionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM grants WHERE action_id=?`, actionID)
	return dbErr(err, "delete grants for action")
}

// ---- Connections (shared upstream credential, §8) ----

func scanConnection(scan func(...any) error) (*kernel.Connection, error) {
	var c kernel.Connection
	var scopes sql.NullString
	var createdAt, updatedAt string
	if err := scan(&c.ID, &c.UserID, &c.ProviderKey, &c.SealedSecret, &scopes, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	c.ScopesJSON = scopes.String
	c.CreatedAt = strToTime(createdAt)
	c.UpdatedAt = strToTime(updatedAt)
	return &c, nil
}

const connectionCols = `id,user_id,provider_key,sealed_secret,scopes_json,created_at,updated_at`

// CreateOrUpdateConnection upserts on (user_id, provider_key): a conflict keeps the existing id
// and created_at (so the AAD the caller sealed with stays valid) and refreshes only the secret,
// scopes, and updated_at.
func (s *DB) CreateOrUpdateConnection(ctx context.Context, c *kernel.Connection) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO connections (`+connectionCols+`)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(user_id,provider_key) DO UPDATE SET
		   sealed_secret=excluded.sealed_secret, scopes_json=excluded.scopes_json, updated_at=excluded.updated_at`,
		c.ID, c.UserID, c.ProviderKey, c.SealedSecret, nullStr(c.ScopesJSON), timeToStr(c.CreatedAt), timeToStr(c.UpdatedAt),
	)
	return dbErr(err, "create connection")
}

func (s *DB) ReadConnection(ctx context.Context, id string) (*kernel.Connection, error) {
	c, err := scanConnection(func(dest ...any) error {
		return s.db.QueryRowContext(ctx, `SELECT `+connectionCols+` FROM connections WHERE id=?`, id).Scan(dest...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("connection not found")
	}
	if err != nil {
		return nil, dbErr(err, "read connection")
	}
	return c, nil
}

func (s *DB) ReadConnectionByUserProvider(ctx context.Context, userID, providerKey string) (*kernel.Connection, error) {
	c, err := scanConnection(func(dest ...any) error {
		return s.db.QueryRowContext(ctx,
			`SELECT `+connectionCols+` FROM connections WHERE user_id=? AND provider_key=?`, userID, providerKey).Scan(dest...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("connection not found")
	}
	if err != nil {
		return nil, dbErr(err, "read connection by provider")
	}
	return c, nil
}

func (s *DB) ListConnectionsByUser(ctx context.Context, userID string) ([]*kernel.Connection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+connectionCols+` FROM connections WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, dbErr(err, "list connections")
	}
	defer rows.Close()
	return queryList(rows, "list connections", scanConnection)
}

// UpdateConnectionSecret replaces the sealed secret (provider refresh-token rotation), leaving
// every grant that points at the connection untouched.
func (s *DB) UpdateConnectionSecret(ctx context.Context, id, sealedSecret string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE connections SET sealed_secret=?, updated_at=? WHERE id=?`, sealedSecret, timeToStr(time.Now().UTC()), id)
	return dbErr(err, "update connection secret")
}

// DeleteConnectionCascade removes a connection and every grant that points at it, atomically
// (provider invalid_grant / disconnect --account).
func (s *DB) DeleteConnectionCascade(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "delete connection")
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM grants WHERE connection_id=?`, id); err != nil {
		return dbErr(err, "delete connection grants")
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM connections WHERE id=?`, id)
	if err != nil {
		return dbErr(err, "delete connection")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return kernel.ErrNotFound.Wrap("connection not found")
	}
	return dbErr(tx.Commit(), "delete connection")
}

func (s *DB) GetConfig(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM config WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", kernel.ErrNotFound.Wrapf("config key %q not found", key)
	}
	if err != nil {
		return "", dbErr(err, "get config")
	}
	return value, nil
}

func (s *DB) SetConfig(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO config (key,value) VALUES (?,?)`, key, value)
	return dbErr(err, "set config")
}

func (s *DB) InitFirstBoot(ctx context.Context, u *kernel.Account, configs map[string]string) error {
	return s.withTx(ctx, "init first boot", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO accounts (id,handle,description,password_hash,available,locked,recovery_public_key,created_at,updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			u.ID, u.Handle, u.Description, u.PasswordHash,
			u.Available, u.Locked, nullStr(u.RecoveryPublicKey), timeToStr(u.CreatedAt), timeToStr(u.UpdatedAt),
		); err != nil {
			return dbErr(err, "init first boot: insert user")
		}
		for k, v := range configs {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO config (key,value) VALUES (?,?)`, k, v); err != nil {
				return dbErr(err, "init first boot: set config "+k)
			}
		}
		return nil
	})
}

// ---- Ledger ----

func (s *DB) CreateLedgerEntry(ctx context.Context, e *kernel.LedgerEntry) error {
	return s.withTx(ctx, "ledger", func(tx *sql.Tx) error {
		// Idempotent replay: an existing external_key returns the recorded entry
		// before any balance change, so a debit replay never re-evaluates the guard below.
		if e.ExternalKey != "" {
			existing, err := readLedgerByExternalKey(ctx, tx, e.ExternalKey)
			if err != nil {
				return err
			}
			if existing != nil {
				*e = *existing
				return nil
			}
		}
		if e.FromUserID != "" {
			res, err := tx.ExecContext(ctx,
				`UPDATE accounts SET available=available-? WHERE id=? AND available>=?`,
				e.Amount, e.FromUserID, e.Amount,
			)
			if err != nil {
				return dbErr(err, "ledger: debit source")
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return kernel.ErrInsufficientFunds.Wrap("insufficient balance")
			}
		}
		if e.ToUserID != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE accounts SET available=available+? WHERE id=?`, e.Amount, e.ToUserID,
			); err != nil {
				return dbErr(err, "ledger: credit destination")
			}
		}
		return insertLedgerRow(ctx, tx, e)
	})
}

// insertLedgerRow writes one ledger record inside an open transaction — the single INSERT behind
// every entry class (deposit/withdraw/transfer, settlement record, transfer-effect delivery). It
// inserts what it is given and decides nothing: the nullable columns go through nullStr, so an
// empty external_key lands as SQL NULL rather than colliding on the unique index.
func insertLedgerRow(ctx context.Context, tx *sql.Tx, e *kernel.LedgerEntry) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO ledger (id,operator_user_id,from_user_id,to_user_id,amount,reason,external_key,created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		e.ID, e.OperatorUserID, nullStr(e.FromUserID), nullStr(e.ToUserID), e.Amount, e.Reason,
		nullStr(e.ExternalKey), timeToStr(e.CreatedAt),
	)
	return dbErr(err, "ledger: insert record")
}

// ListLedgerByUser returns ledger entries where userID is the source or destination,
// most recent first, bounded by limit/offset (a non-positive limit defaults to 100).
func (s *DB) ListLedgerByUser(ctx context.Context, userID string, limit, offset int) ([]*kernel.LedgerEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,operator_user_id,COALESCE(from_user_id,''),COALESCE(to_user_id,''),amount,reason,COALESCE(external_key,''),created_at
		 FROM ledger WHERE from_user_id=? OR to_user_id=? ORDER BY created_at DESC LIMIT ? OFFSET ?`, userID, userID, limit, offset,
	)
	if err != nil {
		return nil, dbErr(err, "list ledger by user")
	}
	return queryList(rows, "list ledger by user", func(scan func(...any) error) (*kernel.LedgerEntry, error) {
		var e kernel.LedgerEntry
		var createdAt string
		if err := scan(&e.ID, &e.OperatorUserID, &e.FromUserID, &e.ToUserID, &e.Amount, &e.Reason, &e.ExternalKey, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt = strToTime(createdAt)
		return &e, nil
	})
}

func readLedgerByExternalKey(ctx context.Context, tx *sql.Tx, externalKey string) (*kernel.LedgerEntry, error) {
	var e kernel.LedgerEntry
	var createdAt string
	err := tx.QueryRowContext(ctx,
		`SELECT id,operator_user_id,COALESCE(from_user_id,''),COALESCE(to_user_id,''),amount,reason,COALESCE(external_key,''),created_at
		 FROM ledger WHERE external_key=?`, externalKey,
	).Scan(&e.ID, &e.OperatorUserID, &e.FromUserID, &e.ToUserID, &e.Amount, &e.Reason, &e.ExternalKey, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, dbErr(err, "read ledger by external_key")
	}
	e.CreatedAt = strToTime(createdAt)
	return &e, nil
}

// CommitSettlement records one finish outcome (§13) and its idempotent ledger row: a clear applies
// ±d/∓d and extinguishes the debt; a pending pay applies no legs and leaves the debt on the row
// until the rail closes it. Every outcome is internally conservative — the money that crosses the
// rail is booked by the rail table, never here. settlementID is the idempotency key: a replay
// returns the stored record and changes no balance (anti-grinding). A negative sys variance is
// guarded like lockReserveTx: a reserve short of it refuses ErrInsufficientFunds and rolls the whole
// transaction back, leaving the settlement pending with no partial writes.
func (s *DB) CommitSettlement(ctx context.Context, settlementID, rowUserID, sysID string, dClear, variance, debt int64, recordJSON string) (string, error) {
	if dClear+variance != 0 {
		return "", fmt.Errorf("commit settlement: non-conservative outcome (dClear=%d variance=%d)", dClear, variance)
	}
	var stored string
	err := s.withTx(ctx, "commit settlement", func(tx *sql.Tx) error {
		existing, err := readLedgerByExternalKey(ctx, tx, settlementID)
		if err != nil {
			return err
		}
		if existing != nil {
			stored = existing.Reason
			return nil
		}
		// One debt draws once, and this is where that holds: the check and the outcome are one
		// transaction, so two draws finishing at once cannot both pass it. Anything checked before
		// the transaction is only a courtesy to the debtor.
		if pending, err := hasPendingSettlement(ctx, tx, rowUserID); err != nil {
			return err
		} else if pending {
			return kernel.ErrInvalidState.Wrap("a settlement of this debt is already awaiting payment")
		}
		if dClear != 0 {
			// The debt must still be outstanding to be cleared, and by at least what is being
			// cleared. Two settlements drawn on one position would otherwise both forgive it, and
			// one debt would be paid for twice.
			res, err := tx.ExecContext(ctx,
				`UPDATE accounts SET available=available+? WHERE id=?
				   AND ((?>0 AND available<=-?) OR (?<0 AND available>=-?))`,
				dClear, rowUserID, dClear, dClear, dClear, dClear)
			if err != nil {
				return dbErr(err, "commit settlement: clear row")
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return kernel.ErrInvalidState.Wrap("the debt this settlement clears is no longer outstanding")
			}
		}
		if variance != 0 {
			res, err := tx.ExecContext(ctx, `UPDATE accounts SET available=available+? WHERE id=? AND available >= ?`, variance, sysID, -variance)
			if err != nil {
				return dbErr(err, "commit settlement: sys variance")
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return kernel.ErrInsufficientFunds.Wrap("operator reserve below settlement variance")
			}
		}
		// to_user_id names the settled peer/proxy row (non-null from/to CHECK + attribution); the
		// signed balance change is applied above; reason carries the record JSON.
		return insertLedgerRow(ctx, tx, &kernel.LedgerEntry{
			ID: "st_" + settlementID, OperatorUserID: sysID, ToUserID: rowUserID,
			Amount: debt, Reason: recordJSON, ExternalKey: settlementID, CreatedAt: time.Now().UTC(),
		})
	})
	return stored, err
}

// ReadSettlementRecord returns the stored final record for settlementID, or "" if none exists.
func (s *DB) ReadSettlementRecord(ctx context.Context, settlementID string) (string, error) {
	var reason string
	err := s.db.QueryRowContext(ctx, `SELECT reason FROM ledger WHERE external_key=?`, settlementID).Scan(&reason)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return reason, dbErr(err, "read settlement record")
}

// HasPendingSettlement reports whether peerID has a paid outcome the rail has not closed (§13): a
// settlement audit row with outcome "pay" whose money has not finished moving. What finishes it
// differs by side, and each side asks about its own row: the creditor's claim is closed when the
// payment has been credited, the debtor's settlement when the payment is final. The serving side
// gates obligation-increasing calls on this; the debtor side reports it instead of re-flipping.
func (s *DB) HasPendingSettlement(ctx context.Context, peerID string) (bool, error) {
	return hasPendingSettlement(ctx, s.db, peerID)
}

// hasPendingSettlement is the predicate itself, runnable inside a transaction so that a commit can
// require it and act on it in one step.
func hasPendingSettlement(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, peerID string) (bool, error) {
	var exists int
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM ledger l
		   WHERE l.to_user_id=? AND l.id LIKE 'st_%' AND l.external_key IS NOT NULL
		     AND l.reason LIKE '%"outcome":"pay"%'
		     AND NOT EXISTS (SELECT 1 FROM rail_transfers r
		                      WHERE r.id = l.external_key
		                        AND (r.status = 'credited'
		                             OR (r.kind = 'settlement' AND r.status IN ('confirmed','announced')))))`,
		peerID).Scan(&exists)
	return exists == 1, dbErr(err, "has pending settlement")
}

// GrossReceivables returns Σ over peer rows of max(0, −available) (§13).
func (s *DB) GrossReceivables(ctx context.Context) (int64, error) {
	var g int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(MAX(0,-available)),0) FROM accounts WHERE kernel_public_key IS NOT NULL`).Scan(&g)
	return g, dbErr(err, "gross receivables")
}

// ---- Embeddings ----

func (s *DB) UpsertEmbedding(ctx context.Context, actionID string, vec []float32) error {
	data, err := json.Marshal(vec)
	if err != nil {
		return dbErr(err, "upsert embedding: marshal")
	}
	_, err = s.db.ExecContext(ctx, `UPDATE actions SET embed_vec=? WHERE id=?`, string(data), actionID)
	return dbErr(err, "upsert embedding")
}

func (s *DB) ListEmbeddings(ctx context.Context) (map[string][]float32, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, embed_vec FROM actions
		 WHERE active=1 AND embed_vec IS NOT NULL AND deleted_at IS NULL`)
	if err != nil {
		return nil, dbErr(err, "list embeddings")
	}
	defer rows.Close()
	out := make(map[string][]float32)
	for rows.Next() {
		var id, vecJSON string
		if err := rows.Scan(&id, &vecJSON); err != nil {
			return nil, dbErr(err, "list embeddings: scan")
		}
		var vec []float32
		if err := json.Unmarshal([]byte(vecJSON), &vec); err != nil {
			continue // corrupt entry; skip silently
		}
		out[id] = vec
	}
	return out, rows.Err()
}

// UpsertLookupText replaces an action's lexical-index row (§9). Delete-then-insert keeps it
// idempotent; the row is keyed by the UNINDEXED action_id.
func (s *DB) UpsertLookupText(ctx context.Context, actionID, text string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM actions_fts WHERE action_id=?`, actionID); err != nil {
		return dbErr(err, "upsert lookup text: delete")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions_fts(action_id, text) VALUES (?, ?)`, actionID, text)
	return dbErr(err, "upsert lookup text")
}

// SearchActionsLexical returns up to limit active, non-deleted action IDs matching query, BM25-ranked
// (best first). The raw query is FTS5 *syntax*, so it is tokenized into quoted terms OR'd together —
// user text is never passed through as an expression. A query with no word tokens yields nil.
func (s *DB) SearchActionsLexical(ctx context.Context, query string, limit int) ([]string, error) {
	match := ftsMatchQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT action_id FROM actions_fts
		 WHERE text MATCH ?
		   AND action_id IN (SELECT id FROM actions WHERE active=1 AND deleted_at IS NULL)
		 ORDER BY bm25(actions_fts) LIMIT ?`, match, limit)
	if err != nil {
		return nil, dbErr(err, "lexical search")
	}
	return queryList(rows, "lexical search", scanID)
}

// ftsMatchQuery turns free text into a safe FTS5 MATCH expression: each unicode word token is
// double-quoted (so it is a literal phrase, not an operator) and the tokens are OR'd. Splitting on
// non-alphanumerics means tokens never contain quotes or FTS5 metacharacters. Empty when tokenless.
func ftsMatchQuery(query string) string {
	terms := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	for i, t := range terms {
		terms[i] = `"` + t + `"`
	}
	return strings.Join(terms, " OR ")
}

// ---- Kernels (identity, naming, discovery) ----

// UpsertKernel records an observation of a remote kernel (§13): its self-asserted nickname, its
// about, and the timestamps. It writes NEITHER the petname (assigned locally, only on our own
// outbound act) NOR gossip_cursor/last_seen/peer_credit, each of which has its own narrow path that
// runs only after the corresponding work is verified and committed. Insert-if-absent for everything
// else, so a minimal row created by an inbound call never clears learned metadata.
func (s *DB) UpsertKernel(ctx context.Context, publicKey, nickname, about, railAddress, railProof string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kernels (public_key,nickname,about,rail_address,rail_proof,first_seen,updated_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(public_key) DO UPDATE SET
		   nickname=CASE WHEN excluded.nickname != '' THEN excluded.nickname ELSE kernels.nickname END,
		   about=CASE WHEN excluded.about != '' THEN excluded.about ELSE kernels.about END,
		   rail_address=CASE WHEN excluded.rail_address != '' THEN excluded.rail_address ELSE kernels.rail_address END,
		   rail_proof=CASE WHEN excluded.rail_proof != '' THEN excluded.rail_proof ELSE kernels.rail_proof END,
		   updated_at=excluded.updated_at`,
		publicKey, nickname, about, railAddress, railProof, timeToStr(now), timeToStr(now),
	)
	return dbErr(err, "upsert kernel")
}

// BindPetname assigns a kernel's local petname inside one transaction, so concurrent first use of
// the same key converges on a single name (§13). Automatic binding (exact=false) preserves any
// existing petname, else takes the desired seed, suffixing -2…-99 on collision. An explicit
// operator bind (exact=true) is exact: an occupied petname is an error, never silently suffixed.
// Returns the bound petname.
func (s *DB) BindPetname(ctx context.Context, publicKey, desired string, exact bool) (string, error) {
	var bound string
	err := s.withTx(ctx, "bind petname", func(tx *sql.Tx) error {
		var existing sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT petname FROM kernels WHERE public_key=?`, publicKey).Scan(&existing); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return kernel.ErrNotFound.Wrapf("kernel %s is not known", publicKey)
			}
			return dbErr(err, "read petname")
		}
		if !exact && existing.String != "" {
			bound = existing.String
			return nil
		}
		for i := 0; i < 99; i++ {
			candidate := desired
			if i > 0 {
				candidate = desired + "-" + strconv.Itoa(i+1)
			}
			var owner string
			err := tx.QueryRowContext(ctx, `SELECT public_key FROM kernels WHERE petname=?`, candidate).Scan(&owner)
			switch {
			case errors.Is(err, sql.ErrNoRows) || owner == publicKey:
			case err != nil:
				return dbErr(err, "check petname")
			case exact:
				return kernel.ErrInvalidInput.Wrapf("petname %s is already bound to another kernel", candidate)
			default:
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE kernels SET petname=? WHERE public_key=?`, candidate, publicKey); err != nil {
				return dbErr(err, "bind petname")
			}
			bound = candidate
			return nil
		}
		return kernel.ErrInvalidInput.Wrapf("no free petname for %s (tried 99 variants)", desired)
	})
	return bound, err
}

// SuspendKernelAccount provisions (when absent) and suspends a kernel's account in ONE transaction
// (§13): two calls would let an inbound signed call land between them and execute against a briefly
// active account. Idempotent — re-suspending an already-suspended kernel just rewrites the stamp.
func (s *DB) SuspendKernelAccount(ctx context.Context, publicKey, newAccountID string, now time.Time) error {
	return s.withTx(ctx, "suspend kernel account", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kernels (public_key,nickname,about,first_seen,updated_at) VALUES (?,'','',?,?)
			 ON CONFLICT(public_key) DO NOTHING`,
			publicKey, timeToStr(now), timeToStr(now)); err != nil {
			return dbErr(err, "ensure kernel")
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO accounts (id,description,password_hash,available,locked,suspended_at,kernel_public_key,created_at,updated_at)
			 SELECT ?,'','',0,0,?,?,?,?
			  WHERE NOT EXISTS (SELECT 1 FROM accounts WHERE kernel_public_key=?)`,
			newAccountID, timeToStr(now), publicKey, timeToStr(now), timeToStr(now), publicKey); err != nil {
			return dbErr(err, "ensure kernel account")
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET suspended_at=?, updated_at=? WHERE kernel_public_key=?`,
			timeToStr(now), timeToStr(now), publicKey); err != nil {
			return dbErr(err, "suspend kernel account")
		}
		return nil
	})
}

// ListKernels returns the whole `admin peers` roster in one query (§14): every known kernel, with
// its account state when one exists. selfKey is excluded; suspended counterparties appear only when
// includeSuspended.
func (s *DB) ListKernels(ctx context.Context, selfKey string, includeSuspended bool, limit, offset int) ([]*kernel.RemoteKernelView, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no ceiling
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT k.public_key, COALESCE(k.petname,''), k.nickname, k.about,
		        CASE WHEN a.id IS NOT NULL THEN 1 ELSE 0 END,
		        COALESCE(a.available,0), COALESCE(a.locked,0), a.suspended_at,
		        k.peer_credit, k.last_seen, k.last_contact_failed_at,
		        (SELECT COUNT(*) FROM discovery_docs d WHERE d.kernel_public_key = k.public_key)
		 FROM kernels k LEFT JOIN accounts a ON a.kernel_public_key = k.public_key
		 WHERE k.public_key != ? AND (? OR a.suspended_at IS NULL)
		 ORDER BY k.updated_at DESC, k.public_key
		 LIMIT ? OFFSET ?`, selfKey, includeSuspended, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list kernels")
	}
	return queryList(rows, "list kernels", func(scan func(...any) error) (*kernel.RemoteKernelView, error) {
		var v kernel.RemoteKernelView
		var hasAccount int
		var suspendedAt, lastSeen, failedAt *string
		if err := scan(&v.PublicKey, &v.Petname, &v.Nickname, &v.About, &hasAccount,
			&v.Available, &v.Locked, &suspendedAt, &v.PeerCredit, &lastSeen, &failedAt, &v.Actions); err != nil {
			return nil, err
		}
		v.HasAccount = hasAccount == 1
		v.SuspendedAt = strToNullTime(suspendedAt)
		v.LastSeen = strToNullTime(lastSeen)
		v.LastContactFailedAt = strToNullTime(failedAt)
		return &v, nil
	})
}

func (s *DB) ReadKernel(ctx context.Context, publicKey string) (*kernel.RemoteKernel, error) {
	return s.readKernelBy(ctx, "public_key", publicKey)
}

// ReadKernelByPetname resolves a bound petname to its kernel (§13); nil when no kernel holds it.
func (s *DB) ReadKernelByPetname(ctx context.Context, petname string) (*kernel.RemoteKernel, error) {
	return s.readKernelBy(ctx, "petname", petname)
}

// readKernelBy reads one kernel row by either of its two unique keys — the global one and the local
// one — which is exactly the pair of namespaces a reference can name (§13). Nil when unknown.
func (s *DB) readKernelBy(ctx context.Context, col, val string) (*kernel.RemoteKernel, error) {
	var k kernel.RemoteKernel
	var firstSeen, updatedAt string
	var lastSeen, failedAt *string
	err := s.db.QueryRowContext(ctx,
		`SELECT public_key,COALESCE(petname,''),nickname,about,rail_address,rail_proof,gossip_cursor,last_seen,last_contact_failed_at,peer_credit,first_seen,updated_at
		   FROM kernels WHERE `+col+`=?`, val).
		Scan(&k.PublicKey, &k.Petname, &k.Nickname, &k.About, &k.RailAddress, &k.RailProof, &k.GossipCursor,
			&lastSeen, &failedAt, &k.PeerCredit, &firstSeen, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, dbErr(err, "read kernel")
	}
	k.LastSeen = strToNullTime(lastSeen)
	k.LastContactFailedAt = strToNullTime(failedAt)
	k.FirstSeen = strToTime(firstSeen)
	k.UpdatedAt = strToTime(updatedAt)
	return &k, nil
}

// SetGossipCursor advances the evidence high-watermark, the narrow path run only after a page is
// verified and committed (§13) — never from ordinary observation.
func (s *DB) SetGossipCursor(ctx context.Context, publicKey, cursor string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE kernels SET gossip_cursor=?, updated_at=? WHERE public_key=?`,
		cursor, timeToStr(time.Now().UTC()), publicKey)
	return dbErr(err, "set gossip cursor")
}

// ---- Discovery docs (regenerable lookup cache) ----

// discoveryDocKey is the FTS/join key for a discovery doc: "<kernel>/<action_id>".
func discoveryDocKey(kernelKey, actionID string) string {
	return kernelKey + "/" + actionID
}

func (s *DB) ReplaceDiscoveryDocs(ctx context.Context, kernelPublicKey string, docs []*kernel.DiscoveryDoc) error {
	return s.withTx(ctx, "replace discovery docs", func(tx *sql.Tx) error {
		// Prefix scope by range, not LIKE: '_' in a base64url key would be a single-char wildcard
		// and could match another kernel's rows. '0' is the ASCII successor of '/'.
		if _, err := tx.ExecContext(ctx, `DELETE FROM discovery_fts WHERE doc_key >= ? || '/' AND doc_key < ? || '0'`, kernelPublicKey, kernelPublicKey); err != nil {
			return dbErr(err, "clear discovery_fts")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM discovery_docs WHERE kernel_public_key=?`, kernelPublicKey); err != nil {
			return dbErr(err, "clear discovery_docs")
		}
		for _, d := range docs {
			inJSON, o0 := json.Marshal(d.InputSchema)
			outJSON, o1 := json.Marshal(d.OutputSchema)
			if oo := firstErr(o0, o1); oo != nil {
				return dbErr(oo, "marshal discovery schema")
			}
			var embed any
			if len(d.Embedding) > 0 {
				b, e := json.Marshal(d.Embedding)
				if e != nil {
					return dbErr(e, "marshal discovery embed")
				}
				embed = string(b)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO discovery_docs (kernel_public_key,handle,description,action_id,name,input_schema,output_schema,serving_price,embed_vec,observed_at)
				 VALUES (?,?,?,?,?,?,?,?,?,?)`,
				d.KernelPublicKey, d.Handle, d.Description, d.ActionID, d.Name,
				string(inJSON), string(outJSON), d.ServingPrice, embed, timeToStr(d.ObservedAt)); err != nil {
				return dbErr(err, "insert discovery_doc")
			}
			text := d.Handle + " " + d.Name + " " + d.Description
			if _, err := tx.ExecContext(ctx, `INSERT INTO discovery_fts(doc_key, text) VALUES (?, ?)`,
				discoveryDocKey(d.KernelPublicKey, d.ActionID), text); err != nil {
				return dbErr(err, "insert discovery_fts")
			}
		}
		return nil
	})
}

func (s *DB) ListDiscoveryDocs(ctx context.Context) ([]*kernel.DiscoveryDoc, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kernel_public_key,handle,description,action_id,name,input_schema,output_schema,serving_price,embed_vec,observed_at
		 FROM discovery_docs`)
	if err != nil {
		return nil, dbErr(err, "list discovery docs")
	}
	return queryList(rows, "list discovery docs", func(scan func(...any) error) (*kernel.DiscoveryDoc, error) {
		var d kernel.DiscoveryDoc
		var inJSON, outJSON, observedAt string
		var embed sql.NullString
		if err := scan(&d.KernelPublicKey, &d.Handle, &d.Description, &d.ActionID, &d.Name,
			&inJSON, &outJSON, &d.ServingPrice, &embed, &observedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(inJSON), &d.InputSchema)
		_ = json.Unmarshal([]byte(outJSON), &d.OutputSchema)
		if embed.Valid && embed.String != "" {
			_ = json.Unmarshal([]byte(embed.String), &d.Embedding)
		}
		d.ObservedAt = strToTime(observedAt)
		return &d, nil
	})
}

func (s *DB) SearchDiscoveryLexical(ctx context.Context, query string, limit int) ([]string, error) {
	match := ftsMatchQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT doc_key FROM discovery_fts WHERE text MATCH ? ORDER BY bm25(discovery_fts) LIMIT ?`, match, limit)
	if err != nil {
		return nil, dbErr(err, "discovery lexical search")
	}
	return queryList(rows, "discovery lexical search", scanID)
}

// ---- Evidence cache ----

func (s *DB) UpsertEvidence(ctx context.Context, e *kernel.EvidenceRow) error {
	return s.withTx(ctx, "upsert evidence", func(tx *sql.Tx) error {
		var existingRating string
		var equivocated int
		err := tx.QueryRowContext(ctx,
			`SELECT rating_json, equivocated FROM evidence WHERE issuer_public_key=? AND receipt_hash=?`,
			e.IssuerPublicKey, e.ReceiptHash).Scan(&existingRating, &equivocated)
		switch {
		case err == sql.ErrNoRows:
			// New row.
		case err != nil:
			return dbErr(err, "read existing evidence")
		default:
			// Late-rating transitions (§13). A row already exists for this (issuer, receipt).
			newRating := e.RatingJSON
			finalRating := existingRating
			finalEquivocated := equivocated == 1
			switch {
			case existingRating == "" && newRating != "":
				finalRating = newRating // attach a late rating
			case newRating == "":
				// keep existing rating (a receipt re-gossiped without its rating)
			case existingRating != "" && newRating != "" && existingRating != newRating:
				finalEquivocated = true // two different valid ratings → both excluded
			}
			_, uerr := tx.ExecContext(ctx,
				`UPDATE evidence SET rating_json=?, equivocated=?, observed_at=?,
				   counterparty_kernel_public_key=?, remote_receipt_hash=?, effective_at=?
				 WHERE issuer_public_key=? AND receipt_hash=?`,
				finalRating, boolInt(finalEquivocated), timeToStr(e.ObservedAt),
				e.CounterpartyKernelPublicKey, e.RemoteReceiptHash, timeToStr(e.EffectiveAt),
				e.IssuerPublicKey, e.ReceiptHash)
			return dbErr(uerr, "update evidence")
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO evidence (issuer_public_key,receipt_hash,subject_kernel_public_key,subject_action_id,
			   counterparty_kernel_public_key,evidence_receipt_json,rating_json,remote_receipt_hash,
			   receipt_created_at,effective_at,observed_at,equivocated)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,0)`,
			e.IssuerPublicKey, e.ReceiptHash, e.SubjectKernelPublicKey, e.SubjectActionID,
			e.CounterpartyKernelPublicKey, e.EvidenceReceiptJSON, e.RatingJSON, e.RemoteReceiptHash,
			timeToStr(e.ReceiptCreatedAt), timeToStr(e.EffectiveAt), timeToStr(e.ObservedAt)); err != nil {
			return dbErr(err, "insert evidence")
		}
		// Enforce the E cap per (issuer, subject_kernel, subject_action): keep the newest E rows.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM evidence
			  WHERE issuer_public_key=? AND subject_kernel_public_key=? AND subject_action_id=?
			    AND receipt_hash NOT IN (
			      SELECT receipt_hash FROM evidence
			       WHERE issuer_public_key=? AND subject_kernel_public_key=? AND subject_action_id=?
			       ORDER BY receipt_created_at DESC LIMIT ?)`,
			e.IssuerPublicKey, e.SubjectKernelPublicKey, e.SubjectActionID,
			e.IssuerPublicKey, e.SubjectKernelPublicKey, e.SubjectActionID, kernelEvidenceCap); err != nil {
			return dbErr(err, "evict evidence over cap")
		}
		return nil
	})
}

func (s *DB) ListEvidenceBySubject(ctx context.Context, subjectKernelPublicKey string) ([]*kernel.EvidenceRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issuer_public_key,receipt_hash,subject_kernel_public_key,subject_action_id,
		        counterparty_kernel_public_key,evidence_receipt_json,rating_json,remote_receipt_hash,
		        receipt_created_at,effective_at,observed_at,equivocated
		 FROM evidence WHERE subject_kernel_public_key=? ORDER BY receipt_created_at DESC`, subjectKernelPublicKey)
	if err != nil {
		return nil, dbErr(err, "list evidence by subject")
	}
	return queryList(rows, "list evidence by subject", func(scan func(...any) error) (*kernel.EvidenceRow, error) {
		var e kernel.EvidenceRow
		var receiptCreated, effectiveAt, observedAt string
		var equiv int
		if err := scan(&e.IssuerPublicKey, &e.ReceiptHash, &e.SubjectKernelPublicKey, &e.SubjectActionID,
			&e.CounterpartyKernelPublicKey, &e.EvidenceReceiptJSON, &e.RatingJSON, &e.RemoteReceiptHash,
			&receiptCreated, &effectiveAt, &observedAt, &equiv); err != nil {
			return nil, err
		}
		e.ReceiptCreatedAt = strToTime(receiptCreated)
		e.EffectiveAt = strToTime(effectiveAt)
		e.ObservedAt = strToTime(observedAt)
		e.Equivocated = equiv == 1
		return &e, nil
	})
}

// ListReceiptsForGossip returns one ordered page of this kernel's own gossip-eligible receipts after
// cursor (§13). Two legs, unified: (a) receipts of the kernel's own active public non-transfer
// actions (execution evidence — SubjectKernelPublicKey left empty for the kernel to fill with its own
// key; CounterpartyKernelPublicKey set when the caller was a peer); (b) receipt-backed remote_proxy
// calls (subject is the peer owner's key + remote action id; RemoteReceiptJSON is the stored serving
// receipt) — every admitted execution, rated or not. Leg (b) keys on a non-empty remote_receipt_json,
// which only a receipt-settled call carries (a locally-manufactured settlement leaves it empty), and
// carries IdempotencyKey so the kernel can drop signed rejections (tx_id == idempotency_key) and
// quarantined receipts (§13 gossip-eligibility) without re-querying. Ordered ascending by effective
// time (a rating's created_at when rated, else the receipt's), so a late rating re-surfaces its
// bundle. cursor is "<effective_at>\x1f<receipt_id>".
func (s *DB) ListReceiptsForGossip(ctx context.Context, cursor string, limit int) ([]*kernel.GossipReceiptRow, error) {
	var curEff, curID string
	if i := strings.IndexByte(cursor, '\x1f'); i >= 0 {
		curEff, curID = cursor[:i], cursor[i+1:]
	}
	const q = `
SELECT r.id, r.tx_id,
       CASE WHEN a.kind='remote_proxy' THEN COALESCE(ow.kernel_public_key,'') ELSE '' END AS subj_kernel,
       CASE WHEN a.kind='remote_proxy' THEN a.remote_action_id ELSE r.action_id END AS subj_action,
       CASE WHEN a.kind='remote_proxy' THEN '' ELSE COALESCE(ca.kernel_public_key,'') END AS cp_kernel,
       COALESCE(t.remote_receipt_json,'') AS remote_receipt_json,
       COALESCE(tr.idempotency_key,'') AS idem_key,
       COALESCE(rt.created_at, r.created_at) AS eff
FROM receipts r
JOIN transactions t ON t.id = r.tx_id
JOIN actions a ON a.id = r.action_id
LEFT JOIN traces tr ON tr.id = r.trace_id
LEFT JOIN accounts ow ON ow.id = a.owner_user_id
LEFT JOIN accounts ca ON ca.id = t.caller_user_id
LEFT JOIN ratings rt ON rt.rated_tx_id = r.tx_id
WHERE r.value = 0 AND COALESCE(a.effect,'') != 'transfer'
  AND (
        (a.kind IN ('http','wasm','native') AND a.visibility='public' AND a.active=1 AND a.deleted_at IS NULL)
     OR (a.kind='remote_proxy' AND COALESCE(t.remote_receipt_json,'') != '' AND ow.kernel_public_key IS NOT NULL AND ow.kernel_public_key != '')
      )
  AND ( ? = ''
        OR julianday(COALESCE(rt.created_at, r.created_at)) > julianday(?)
        OR (julianday(COALESCE(rt.created_at, r.created_at)) = julianday(?) AND r.id > ?) )
ORDER BY julianday(COALESCE(rt.created_at, r.created_at)) ASC, r.id ASC
LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, curEff, curEff, curEff, curID, limit)
	if err != nil {
		return nil, dbErr(err, "list receipts for gossip")
	}
	type raw struct {
		receiptID, txID, subjKernel, subjAction, cpKernel, remoteReceiptJSON, idemKey, eff string
	}
	var raws []raw
	for rows.Next() {
		var rr raw
		if err := rows.Scan(&rr.receiptID, &rr.txID, &rr.subjKernel, &rr.subjAction, &rr.cpKernel, &rr.remoteReceiptJSON, &rr.idemKey, &rr.eff); err != nil {
			rows.Close()
			return nil, dbErr(err, "scan gossip receipt")
		}
		raws = append(raws, rr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr(err, "gossip receipt rows")
	}
	out := make([]*kernel.GossipReceiptRow, 0, len(raws))
	for _, rr := range raws {
		rec, rerr := s.ReadReceipt(ctx, rr.receiptID)
		if rerr != nil {
			return nil, rerr
		}
		var rating *kernel.Rating
		if rt, rterr := s.ReadRatingByTxID(ctx, rr.txID); rterr == nil {
			rating = rt
		}
		out = append(out, &kernel.GossipReceiptRow{
			Receipt:                     rec,
			SubjectKernelPublicKey:      rr.subjKernel,
			SubjectActionID:             rr.subjAction,
			CounterpartyKernelPublicKey: rr.cpKernel,
			RemoteReceiptJSON:           rr.remoteReceiptJSON,
			IdempotencyKey:              rr.idemKey,
			Rating:                      rating,
			EffectiveAt:                 strToTime(rr.eff),
			Cursor:                      rr.eff + "\x1f" + rr.receiptID,
		})
	}
	return out, nil
}

// ---- helpers ----

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// kernelEvidenceCap (E) is the retained-evidence window per (issuer, subject_kernel, subject_action)
// (§13). Fixed, mirroring kernel.gossipEvidenceCap; kept store-local to avoid a cross-package const.
const kernelEvidenceCap = 200

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// callerUnique lists the unique keys derived from caller input; a collision there is the caller's
// conflict. Every other unique key is kernel-minted, so its collision is a broken invariant and
// stays internal — unlisted keys fail closed. ledger.external_key never reaches the index (§12).
var callerUnique = []string{
	"accounts.handle", "accounts.rail_address", "kernels.petname", "actions.owner_user_id", "ratings.rated_tx_id",
	"connections.user_id", "grants.grantor_user_id",
	"idempotency_records.idempotency_key",
}

// Extended result codes for the two uniqueness failures.
const (
	sqliteConstraintUnique     = 2067
	sqliteConstraintPrimaryKey = 1555
)

func dbErr(err error, op string) error {
	if err == nil {
		return nil
	}
	// Classify BEFORE wrapping: Wrapf drops the cause, so the driver error is unreachable after.
	var se *sqlite.Error
	if errors.As(err, &se) && (se.Code() == sqliteConstraintUnique || se.Code() == sqliteConstraintPrimaryKey) {
		for _, key := range callerUnique {
			if strings.Contains(se.Error(), key) {
				return kernel.ErrInvalidInput.Wrapf("%s: already exists", op)
			}
		}
	}
	return kernel.ErrInternal.Wrapf("%s: %v", op, err)
}

// rowScan adapts a single-row query to the scan-function shape the scanners take.
func rowScan(row *sql.Row) func(...any) error { return row.Scan }

// scanID is the queryList adapter for a single-column id/key list.
func scanID(scan func(...any) error) (string, error) {
	var id string
	err := scan(&id)
	return id, err
}

// queryList collects rows into a slice using the provided scan function.
func queryList[T any](rows *sql.Rows, op string, scan func(func(...any) error) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows.Scan)
		if err != nil {
			return nil, dbErr(err, op)
		}
		out = append(out, v)
	}
	return out, dbErr(rows.Err(), op)
}

// withTx runs fn inside a single SQLite transaction identified by label.
// It begins the transaction, defers rollback, calls fn, and on success commits.
func (s *DB) withTx(ctx context.Context, label string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin "+label)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return dbErr(tx.Commit(), label+": commit")
}

// ---- Receipts ----

const receiptSelectCols = `id,issuer_user_id,tx_id,trace_id,action_id,caller_user_id,process_id,
		        args_hash,reply_hash,status,gross,net,fee,charge,premium,value,COALESCE(value_to,''),reason,started_at,created_at,signature`

func scanReceipt(row *sql.Row, op string) (*kernel.Receipt, error) {
	var r kernel.Receipt
	var status, startedAt, createdAt string
	err := row.Scan(&r.ID, &r.IssuerUserID, &r.TxID, &r.TraceID, &r.ActionID,
		&r.CallerUserID, &r.ProcessID,
		&r.ArgsHash, &r.ReplyHash, &status,
		&r.Gross, &r.Net, &r.Fee, &r.Charge, &r.Premium, &r.Value, &r.ValueTo, &r.Reason, &startedAt, &createdAt, &r.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("receipt not found")
	}
	if err != nil {
		return nil, dbErr(err, op)
	}
	r.Status = kernel.TxStatus(status)
	r.StartedAt = strToTime(startedAt)
	r.CreatedAt = strToTime(createdAt)
	return &r, nil
}

func (s *DB) ReadReceiptByTxID(ctx context.Context, txID string) (*kernel.Receipt, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+receiptSelectCols+` FROM receipts WHERE tx_id=?`, txID)
	return scanReceipt(row, "read receipt by tx_id")
}

func (s *DB) ReadReceipt(ctx context.Context, id string) (*kernel.Receipt, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+receiptSelectCols+` FROM receipts WHERE id=?`, id)
	return scanReceipt(row, "read receipt")
}

// ---- Ratings ----

func (s *DB) CreateRatingAndUpdateStats(ctx context.Context, r *kernel.Rating, actionID string, rating float64) error {
	return s.withTx(ctx, "create rating and update stats", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ratings (id,rated_tx_id,rated_receipt_id,rated_receipt_hash,rater_user_id,rating,note,created_at,signature)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			r.ID, r.RatedTxID, r.RatedReceiptID, r.RatedReceiptHash, r.RaterUserID, r.Rating, r.Note,
			timeToStr(r.CreatedAt), r.Signature,
		); err != nil {
			return dbErr(err, "create rating and update stats: insert rating")
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE action_stats SET
			   rating_count = rating_count + 1,
			   rating_estimate  = rating_estimate + (? - rating_estimate) / (rating_count + 1)
			 WHERE action_id = ?`,
			rating, actionID,
		)
		return dbErr(err, "create rating and update stats: update stats")
	})
}

func (s *DB) ListRatings(ctx context.Context, actionID string, limit, offset int) ([]*kernel.Rating, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.rated_tx_id, r.rated_receipt_id, r.rated_receipt_hash, r.rater_user_id, r.rating, r.note, r.created_at, r.signature
		 FROM ratings r
		 JOIN transactions t ON t.id = r.rated_tx_id
		 WHERE t.action_id = ?
		 ORDER BY r.created_at DESC
		 LIMIT ? OFFSET ?`,
		actionID, limit, offset,
	)
	if err != nil {
		return nil, dbErr(err, "list ratings")
	}
	return queryList(rows, "list ratings", func(scan func(...any) error) (*kernel.Rating, error) {
		var r kernel.Rating
		var ratedReceiptID *string
		var createdAt string
		if err := scan(&r.ID, &r.RatedTxID, &ratedReceiptID, &r.RatedReceiptHash, &r.RaterUserID, &r.Rating, &r.Note, &createdAt, &r.Signature); err != nil {
			return nil, err
		}
		r.RatedReceiptID = ratedReceiptID
		r.CreatedAt = strToTime(createdAt)
		return &r, nil
	})
}

func (s *DB) ReadRatingByTxID(ctx context.Context, txID string) (*kernel.Rating, error) {
	var r kernel.Rating
	var ratedReceiptID *string
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,rated_tx_id,rated_receipt_id,rated_receipt_hash,rater_user_id,rating,note,created_at,signature
		 FROM ratings WHERE rated_tx_id=?`, txID,
	).Scan(&r.ID, &r.RatedTxID, &ratedReceiptID, &r.RatedReceiptHash, &r.RaterUserID, &r.Rating, &r.Note, &createdAt, &r.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("rating not found")
	}
	if err != nil {
		return nil, dbErr(err, "read rating by tx_id")
	}
	r.RatedReceiptID = ratedReceiptID
	r.CreatedAt = strToTime(createdAt)
	return &r, nil
}

// ---- Idempotency ----

func (s *DB) InsertPendingIdempotencyRecord(ctx context.Context, r *kernel.IdempotencyRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO idempotency_records (id,idempotency_key,counterparty_user_id,receipt_id,status,args_json,result_json,created_at,expires_at)
		 VALUES (?,?,?,NULL,'pending',?,'',?,?)`,
		r.ID, r.IdempotencyKey, r.CounterpartyUserID, r.ArgsJSON,
		timeToStr(r.CreatedAt), timeToStr(r.ExpiresAt),
	)
	return dbErr(err, "insert pending idempotency record")
}

// DeleteIdempotencyRecord removes a pending idempotency record.
// Records that have already been completed (by CommitFailedCall or CommitCall)
// are left intact so that replays can return the stored result.
func (s *DB) DeleteIdempotencyRecord(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_records WHERE id=? AND status='pending'`, id,
	)
	return dbErr(err, "delete idempotency record")
}

func (s *DB) CompleteIdempotencyRecordIfPending(ctx context.Context, id, resultJSON, receiptJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE idempotency_records SET status='complete', result_json=?, receipt_json=? WHERE id=? AND status='pending'`,
		resultJSON, receiptJSON, id,
	)
	return dbErr(err, "complete idempotency record if pending")
}

// ---- Recovery challenges ----

// CreateRecoveryChallenge stores a single-use nonce for a password-recovery attempt (§12).
func (s *DB) CreateRecoveryChallenge(ctx context.Context, nonce, userID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO recovery_challenge (nonce,user_id,expires_at) VALUES (?,?,?)`,
		nonce, userID, timeToStr(expiresAt),
	)
	return dbErr(err, "create recovery challenge")
}

// ConsumeRecoveryChallenge atomically deletes an unexpired nonce and returns its user_id. The
// single DELETE ... RETURNING makes consumption single-use — a replay finds no row — so a captured
// recovery request cannot be replayed. Returns ErrNotFound when unknown, already consumed, or expired.
func (s *DB) ConsumeRecoveryChallenge(ctx context.Context, nonce string) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM recovery_challenge WHERE nonce=? AND datetime(expires_at) > datetime('now') RETURNING user_id`,
		nonce,
	).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", kernel.ErrNotFound.Wrap("recovery challenge not found or expired")
	}
	if err != nil {
		return "", dbErr(err, "consume recovery challenge")
	}
	return userID, nil
}

func (s *DB) ReadIdempotencyRecord(ctx context.Context, key, counterpartyUserID string) (*kernel.IdempotencyRecord, error) {
	return s.readIdempotencyRecord(ctx,
		`idempotency_key=? AND counterparty_user_id=? AND datetime(expires_at) > datetime('now')`, key, counterpartyUserID)
}

// ReadIdempotencyRecordByID reads a record whatever its age: recovery settles what it finds, and a
// record older than its expiry still names an inbound call whose caller may be waiting.
func (s *DB) ReadIdempotencyRecordByID(ctx context.Context, id string) (*kernel.IdempotencyRecord, error) {
	return s.readIdempotencyRecord(ctx, `id=?`, id)
}

func (s *DB) readIdempotencyRecord(ctx context.Context, where string, args ...any) (*kernel.IdempotencyRecord, error) {
	var r kernel.IdempotencyRecord
	var receiptID *string
	var createdAt, expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,idempotency_key,counterparty_user_id,receipt_id,status,args_json,result_json,receipt_json,created_at,expires_at
		 FROM idempotency_records WHERE `+where, args...,
	).Scan(&r.ID, &r.IdempotencyKey, &r.CounterpartyUserID, &receiptID, &r.Status, &r.ArgsJSON, &r.ResultJSON, &r.ReceiptJSON, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("idempotency record not found or expired")
	}
	if err != nil {
		return nil, dbErr(err, "read idempotency record")
	}
	r.ReceiptID = receiptID
	r.CreatedAt = strToTime(createdAt)
	r.ExpiresAt = strToTime(expiresAt)
	return &r, nil
}

// ExecForTest runs a raw statement. It exists so tests can construct states the kernel's own API
// deliberately cannot reach — notably breaking the step-park invariant to prove that a corrupted
// ledger is reported as corruption rather than as a lost claim race. Not used in production.
func (s *DB) ExecForTest(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}
