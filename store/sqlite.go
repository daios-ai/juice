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
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	_ "modernc.org/sqlite"
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
	dsn := path + "?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000"
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
func (s *DB) migrate() error {
	if _, err := s.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
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
		if version == "004_events_queue" && s.columnExists("events", "consumed_at") {
			if err := s.markMigrationApplied(version); err != nil {
				return err
			}
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
			if ignorableMigrationError(err) {
				continue
			}
			return fmt.Errorf("%s: %w", version, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, timeToStr(time.Now().UTC())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DB) markMigrationApplied(version string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, timeToStr(time.Now().UTC()))
	return err
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

// isDuplicateColumn reports whether the SQLite error is a duplicate column error.
func isDuplicateColumn(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate column name") || strings.Contains(msg, "already exists")
}

func ignorableMigrationError(err error) bool {
	return isDuplicateColumn(err)
}

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

// ---- time helpers ----

const timeLayout = time.RFC3339Nano

func timeToStr(t time.Time) string { return t.UTC().Format(timeLayout) }
func strToTime(s string) time.Time {
	t, _ := time.Parse(timeLayout, s)
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

// ---- Users ----

func (s *DB) CreateUser(ctx context.Context, u *kernel.User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id,handle,email,password_hash,available,locked,suspended_at,public_key,remote_base_url,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Handle, u.Email, u.PasswordHash, u.Available, u.Locked,
		nullTimeToStr(u.SuspendedAt), nullStr(u.PublicKey), nullStr(u.RemoteBaseURL),
		timeToStr(u.CreatedAt), timeToStr(u.UpdatedAt),
	)
	if err != nil {
		return dbErr(err, "create user")
	}
	return nil
}

func (s *DB) ReadUser(ctx context.Context, id string) (*kernel.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id,handle,email,password_hash,available,locked,suspended_at,public_key,remote_base_url,created_at,updated_at
		 FROM users WHERE id=?`, id))
}

func (s *DB) ReadUserByHandle(ctx context.Context, handle string) (*kernel.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id,handle,email,password_hash,available,locked,suspended_at,public_key,remote_base_url,created_at,updated_at
		 FROM users WHERE handle=?`, handle))
}

func (s *DB) ReadUserByPublicKey(ctx context.Context, publicKey string) (*kernel.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id,handle,email,password_hash,available,locked,suspended_at,public_key,remote_base_url,created_at,updated_at
		 FROM users WHERE public_key=?`, publicKey))
}

func (s *DB) UpdateRemoteBaseURL(ctx context.Context, userID, baseURL string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET remote_base_url=?, updated_at=? WHERE id=?`,
		nullStr(baseURL), timeToStr(time.Now().UTC()), userID,
	)
	return dbErr(err, "update remote base url")
}

func (s *DB) scanUser(row *sql.Row) (*kernel.User, error) {
	var u kernel.User
	var createdAt, updatedAt string
	var suspendedAt, publicKey, remoteBaseURL *string
	err := row.Scan(&u.ID, &u.Handle, &u.Email, &u.PasswordHash,
		&u.Available, &u.Locked, &suspendedAt, &publicKey, &remoteBaseURL, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("user not found")
	}
	if err != nil {
		return nil, dbErr(err, "read user")
	}
	u.SuspendedAt = strToNullTime(suspendedAt)
	u.PublicKey = strVal(publicKey)
	u.RemoteBaseURL = strVal(remoteBaseURL)
	u.CreatedAt = strToTime(createdAt)
	u.UpdatedAt = strToTime(updatedAt)
	return &u, nil
}

func (s *DB) ListUsers(ctx context.Context, limit, offset int) ([]*kernel.User, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,handle,email,password_hash,available,locked,suspended_at,public_key,remote_base_url,created_at,updated_at
		 FROM users ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list users")
	}
	defer rows.Close()
	var out []*kernel.User
	for rows.Next() {
		var u kernel.User
		var createdAt, updatedAt string
		var suspendedAt, publicKey, remoteBaseURL *string
		if err := rows.Scan(&u.ID, &u.Handle, &u.Email, &u.PasswordHash,
			&u.Available, &u.Locked, &suspendedAt, &publicKey, &remoteBaseURL, &createdAt, &updatedAt); err != nil {
			return nil, dbErr(err, "scan user")
		}
		u.SuspendedAt = strToNullTime(suspendedAt)
		u.PublicKey = strVal(publicKey)
		u.RemoteBaseURL = strVal(remoteBaseURL)
		u.CreatedAt = strToTime(createdAt)
		u.UpdatedAt = strToTime(updatedAt)
		out = append(out, &u)
	}
	return out, rows.Err()
}

func (s *DB) SuspendUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET suspended_at=datetime('now') WHERE id=?`, id)
	return dbErr(err, "suspend user")
}

func (s *DB) UnsuspendUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET suspended_at=NULL WHERE id=?`, id)
	return dbErr(err, "unsuspend user")
}

// ---- Actions ----

func (s *DB) CreateAction(ctx context.Context, a *kernel.Action) error {
	inJSON, _ := json.Marshal(a.InputSchema)
	outJSON, _ := json.Marshal(a.OutputSchema)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO actions
		 (id,owner_user_id,name,kind,active,public,price,description,input_schema,output_schema,source,artifact_hash,wasm_artifact,remote_action_id,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerUserID, a.Name, string(a.Kind), boolInt(a.Active), boolInt(a.Public), a.Price,
		a.Description, string(inJSON), string(outJSON), a.Source, a.ArtifactHash, a.WasmArtifact, a.RemoteActionID,
		timeToStr(a.CreatedAt), timeToStr(a.UpdatedAt),
	)
	return dbErr(err, "create action")
}

// actionCols is the canonical column list for action SELECT statements.
// Must stay in sync with scanAction/scanActionRow/finishAction.
const actionCols = `a.id,a.owner_user_id,COALESCE(u.handle,''),a.name,a.kind,a.active,a.public,a.price,a.description,a.input_schema,a.output_schema,a.source,a.artifact_hash,a.wasm_artifact,a.remote_action_id,a.created_at,a.updated_at,a.deleted_at`

func (s *DB) ReadAction(ctx context.Context, id string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN users u ON u.id=a.owner_user_id WHERE a.id=? AND a.deleted_at IS NULL`, id))
}

func (s *DB) ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN users u ON u.id=a.owner_user_id WHERE a.owner_user_id=? AND a.name=? AND a.deleted_at IS NULL`, ownerID, name))
}

func (s *DB) updateActionTx(ctx context.Context, tx *sql.Tx, a *kernel.Action) error {
	inJSON, _ := json.Marshal(a.InputSchema)
	outJSON, _ := json.Marshal(a.OutputSchema)
	_, err := tx.ExecContext(ctx,
		`UPDATE actions SET kind=?,active=?,public=?,price=?,description=?,input_schema=?,output_schema=?,
		 source=?,artifact_hash=?,wasm_artifact=?,updated_at=? WHERE id=?`,
		string(a.Kind), boolInt(a.Active), boolInt(a.Public), a.Price, a.Description,
		string(inJSON), string(outJSON), a.Source, a.ArtifactHash, a.WasmArtifact,
		timeToStr(a.UpdatedAt), a.ID,
	)
	return dbErr(err, "update action")
}

func (s *DB) UpdateAction(ctx context.Context, a *kernel.Action) error {
	return s.withTx(ctx, "update action", func(tx *sql.Tx) error {
		return s.updateActionTx(ctx, tx, a)
	})
}

func (s *DB) UpdateActionAndResetStats(ctx context.Context, a *kernel.Action) error {
	return s.withTx(ctx, "update action and reset stats", func(tx *sql.Tx) error {
		if err := s.updateActionTx(ctx, tx, a); err != nil {
			return err
		}
		zeroTime := timeToStr(time.Time{})
		_, err := tx.ExecContext(ctx,
			`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,price_mean,latency_mean,rating_mean,last_used_at)
			 VALUES (?,0,0,0,0,0,0,0,?)
			 ON CONFLICT(action_id) DO UPDATE SET
			   uses=0,successes=0,failures=0,rating_count=0,
			   price_mean=0,latency_mean=0,rating_mean=0,last_used_at=excluded.last_used_at`,
			a.ID, zeroTime,
		)
		return dbErr(err, "update action and reset stats: reset stats")
	})
}

func (s *DB) DeleteAction(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions SET deleted_at=? WHERE id=? AND deleted_at IS NULL`,
		timeToStr(time.Now().UTC()), id)
	return dbErr(err, "delete action")
}

func (s *DB) ListPublicActions(ctx context.Context, limit, offset int) ([]*kernel.Action, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN users u ON u.id=a.owner_user_id
		 WHERE a.active=1 AND a.public=1 AND a.deleted_at IS NULL
		 ORDER BY a.created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list public actions")
	}
	defer rows.Close()

	var out []*kernel.Action
	for rows.Next() {
		a, err := s.scanActionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *DB) ListActionsByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*kernel.Action, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN users u ON u.id=a.owner_user_id
		 WHERE a.owner_user_id=? AND a.deleted_at IS NULL
		 ORDER BY a.created_at DESC LIMIT ? OFFSET ?`, ownerID, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list actions by owner")
	}
	defer rows.Close()
	var out []*kernel.Action
	for rows.Next() {
		a, err := s.scanActionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *DB) ListAllActions(ctx context.Context, limit, offset int) ([]*kernel.Action, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN users u ON u.id=a.owner_user_id WHERE a.deleted_at IS NULL ORDER BY a.created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list all actions")
	}
	defer rows.Close()
	var out []*kernel.Action
	for rows.Next() {
		a, err := s.scanActionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *DB) ListActionsByOwnerOpenAPISpec(ctx context.Context, ownerID, specURL string) ([]*kernel.Action, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN users u ON u.id=a.owner_user_id
		 WHERE a.owner_user_id=?
		   AND a.deleted_at IS NULL
		   AND json_valid(a.source)=1
		   AND json_extract(a.source,'$.type')='openapi'
		   AND json_extract(a.source,'$.spec_url')=?`,
		ownerID, specURL)
	if err != nil {
		return nil, dbErr(err, "list actions by openapi spec")
	}
	defer rows.Close()
	var out []*kernel.Action
	for rows.Next() {
		a, err := s.scanActionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *DB) scanAction(row *sql.Row) (*kernel.Action, error) {
	var a kernel.Action
	var kind, inJSON, outJSON, createdAt, updatedAt string
	var deletedAt sql.NullString
	var active, public int
	err := row.Scan(&a.ID, &a.OwnerUserID, &a.OwnerHandle, &a.Name, &kind, &active, &public, &a.Price,
		&a.Description, &inJSON, &outJSON, &a.Source, &a.ArtifactHash, &a.WasmArtifact, &a.RemoteActionID,
		&createdAt, &updatedAt, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("action not found")
	}
	if err != nil {
		return nil, dbErr(err, "read action")
	}
	return finishAction(&a, kind, active, public, inJSON, outJSON, createdAt, updatedAt, deletedAt)
}

func (s *DB) scanActionRow(rows *sql.Rows) (*kernel.Action, error) {
	var a kernel.Action
	var kind, inJSON, outJSON, createdAt, updatedAt string
	var deletedAt sql.NullString
	var active, public int
	err := rows.Scan(&a.ID, &a.OwnerUserID, &a.OwnerHandle, &a.Name, &kind, &active, &public, &a.Price,
		&a.Description, &inJSON, &outJSON, &a.Source, &a.ArtifactHash, &a.WasmArtifact, &a.RemoteActionID,
		&createdAt, &updatedAt, &deletedAt)
	if err != nil {
		return nil, dbErr(err, "scan action")
	}
	return finishAction(&a, kind, active, public, inJSON, outJSON, createdAt, updatedAt, deletedAt)
}

func (s *DB) ReadActionByOwnerRemoteID(ctx context.Context, ownerID, remoteActionID string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN users u ON u.id=a.owner_user_id WHERE a.owner_user_id=? AND a.remote_action_id=? AND a.remote_action_id!='' AND a.deleted_at IS NULL`,
		ownerID, remoteActionID))
}

func finishAction(a *kernel.Action, kind string, active, public int, inJSON, outJSON, createdAt, updatedAt string, deletedAt sql.NullString) (*kernel.Action, error) {
	a.Kind = kernel.ActionKind(kind)
	a.Active = active != 0
	a.Public = public != 0
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

func (s *DB) StartProcess(ctx context.Context, p *kernel.Process, t *kernel.Trace, ownerID string, funds int64) error {
	return s.withTx(ctx, "start process", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO processes (id,owner_user_id,available,locked,status,created_at,ended_at) VALUES (?,?,?,?,?,?,?)`,
			p.ID, p.OwnerUserID, 0, 0, string(p.Status), timeToStr(p.CreatedAt), nullTimeToStr(p.EndedAt),
		); err != nil {
			return dbErr(err, "start process: insert process")
		}
		if funds > 0 {
			res, err := tx.ExecContext(ctx,
				`UPDATE users SET available=available-?, locked=locked+? WHERE id=? AND available>=?`,
				funds, funds, ownerID, funds,
			)
			if err != nil {
				return dbErr(err, "start process: deduct user")
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return kernel.ErrInsufficientFunds.Wrap("insufficient user balance")
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE processes SET available=? WHERE id=?`, funds, p.ID,
			); err != nil {
				return dbErr(err, "start process: credit process")
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO traces (id,process_id,parent_trace_id,action_owner_id,cost,latency_ms,created_at) VALUES (?,?,?,?,?,?,?)`,
			t.ID, t.ProcessID, nullStrPtr(t.ParentTraceID), t.ActionOwnerID, t.Cost, t.LatencyMS, timeToStr(t.CreatedAt),
		)
		return dbErr(err, "start process: insert trace")
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

// BeginCall atomically locks price credits in the process and inserts the child trace.
// Either both succeed or neither does, preserving the transition invariant.
func (s *DB) BeginCall(ctx context.Context, processID string, t *kernel.Trace, price int64) error {
	return s.withTx(ctx, "begin call", func(tx *sql.Tx) error {
		if price > 0 {
			res, err := tx.ExecContext(ctx,
				`UPDATE processes SET available=available-?, locked=locked+?
				 WHERE id=? AND available>=? AND status='open'`,
				price, price, processID, price,
			)
			if err != nil {
				return dbErr(err, "begin call: lock funds")
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return kernel.ErrInsufficientFunds.Wrap("not enough process funds or process closed")
			}
		} else {
			// price == 0: atomically verify the process is still open to close the
			// TOCTOU window between the precondition read in Call() and this transition.
			res, err := tx.ExecContext(ctx,
				`UPDATE processes SET available=available WHERE id=? AND status='open'`,
				processID,
			)
			if err != nil {
				return dbErr(err, "begin call: verify process open")
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return kernel.ErrInvalidState.Wrap("process is closed")
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO traces (id,process_id,parent_trace_id,action_owner_id,cost,latency_ms,created_at) VALUES (?,?,?,?,?,?,?)`,
			t.ID, t.ProcessID, nullStrPtr(t.ParentTraceID), t.ActionOwnerID, t.Cost, t.LatencyMS, timeToStr(t.CreatedAt),
		)
		return dbErr(err, "begin call: create trace")
	})
}

func (s *DB) RefundFunds(ctx context.Context, processID string, amount int64) error {
	if amount == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE processes SET available=available+?, locked=locked-? WHERE id=?`,
		amount, amount, processID,
	)
	return dbErr(err, "refund funds")
}

func (s *DB) FundProcess(ctx context.Context, userID, processID string, amount int64) error {
	return s.withTx(ctx, "fund process", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET available=available-?, locked=locked+? WHERE id=? AND available>=?`,
			amount, amount, userID, amount,
		)
		if err != nil {
			return dbErr(err, "fund process: deduct user")
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return kernel.ErrInsufficientFunds.Wrap("insufficient user balance")
		}
		res, err = tx.ExecContext(ctx,
			`UPDATE processes SET available=available+? WHERE id=? AND status='open'`,
			amount, processID,
		)
		if err != nil {
			return dbErr(err, "fund process: credit process")
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return kernel.ErrInvalidState.Wrap("process is not open")
		}
		return nil
	})
}

// insertAuditRows inserts the transaction record and its mandatory receipt into an open SQLite transaction.
func (s *DB) insertAuditRows(ctx context.Context, tx *sql.Tx, ktx *kernel.Transaction, receipt *kernel.Receipt, label string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO transactions
		 (id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,target_user_id,
		  action_id,action_name,remote_action_id,args_json,reply_json,status,gross,net,fee,reason,remote_receipt_hash,remote_receipt_json,started_at,ended_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ktx.ID, ktx.ProcessID, ktx.TraceID, ktx.ParentTraceID,
		ktx.OwnerUserID, ktx.CallerUserID, ktx.TargetUserID, ktx.ActionID, ktx.ActionName, ktx.RemoteActionID,
		rawJSONStr(ktx.ArgsJSON), rawJSONStr(ktx.ReplyJSON), string(ktx.Status),
		ktx.Gross, ktx.Net, ktx.Fee, ktx.Reason, nullStr(ktx.RemoteReceiptHash), ktx.RemoteReceiptJSON,
		timeToStr(ktx.StartedAt), timeToStr(ktx.EndedAt),
	); err != nil {
		return dbErr(err, label+": insert transaction")
	}
	if receipt == nil {
		return dbErr(fmt.Errorf("receipt is required"), label)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO receipts (id,issuer_user_id,tx_id,trace_id,action_id,caller_user_id,process_id,
		                       args_hash,reply_hash,status,gross,net,fee,reason,started_at,created_at,signature)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.ID, receipt.IssuerUserID, receipt.TxID, receipt.TraceID, receipt.ActionID,
		receipt.CallerUserID, receipt.ProcessID,
		receipt.ArgsHash, receipt.ReplyHash, string(receipt.Status),
		receipt.Gross, receipt.Net, receipt.Fee, receipt.Reason,
		timeToStr(receipt.StartedAt), timeToStr(receipt.CreatedAt), receipt.Signature,
	); err != nil {
		return dbErr(err, label+": insert receipt")
	}
	return nil
}

// updateAncestorTraces updates cost and latency for all ancestor traces via a recursive CTE.
// costDelta is 0 for failures (latency-only update).
func (s *DB) updateAncestorTraces(ctx context.Context, tx *sql.Tx, traceID string, costDelta int64, endedAt time.Time, label string) error {
	_, err := tx.ExecContext(ctx, `
WITH RECURSIVE ancestors(id, parent_id) AS (
    SELECT id, parent_trace_id FROM traces WHERE id=?
    UNION ALL
    SELECT t.id, t.parent_trace_id FROM traces t
    JOIN ancestors a ON t.id=a.parent_id AND a.parent_id IS NOT NULL AND a.id!=a.parent_id
)
UPDATE traces SET
    cost=cost+?,
    latency_ms=MAX(latency_ms, CAST((julianday(?)-julianday(created_at))*86400000 AS INTEGER))
WHERE id IN (SELECT id FROM ancestors)`,
		traceID, costDelta, timeToStr(endedAt),
	)
	return dbErr(err, label+": update trace ancestors")
}

// upsertActionStats updates the incremental success or failure counters for an action.
// rating_count/rating_mean are excluded — owned by UpdateRating.
func (s *DB) upsertActionStats(ctx context.Context, tx *sql.Tx, stats *kernel.Stats, success bool, label string) error {
	if stats == nil {
		return nil
	}
	var err error
	if success {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,price_mean,latency_mean,rating_mean,last_used_at)
			 VALUES (?,1,1,0,0,?,?,0,?)
			 ON CONFLICT(action_id) DO UPDATE SET
			   uses=uses+1,
			   successes=successes+1,
			   price_mean=price_mean+(excluded.price_mean-price_mean)/(successes+1),
			   latency_mean=latency_mean+(excluded.latency_mean-latency_mean)/(uses+1),
			   last_used_at=excluded.last_used_at`,
			stats.ActionID, stats.PriceMean, stats.LatencyMean, timeToStr(stats.LastUsedAt),
		)
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,price_mean,latency_mean,rating_mean,last_used_at)
			 VALUES (?,1,0,1,0,0,?,0,?)
			 ON CONFLICT(action_id) DO UPDATE SET
			   uses=uses+1,
			   failures=failures+1,
			   latency_mean=latency_mean+(excluded.latency_mean-latency_mean)/(uses+1),
			   last_used_at=excluded.last_used_at`,
			stats.ActionID, stats.LatencyMean, timeToStr(stats.LastUsedAt),
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

// finalizeTx executes the shared tail of both commit paths: audit rows, trace metrics,
// stats, optional idempotency completion, optional step completion, and commit.
func (s *DB) finalizeTx(ctx context.Context, tx *sql.Tx, ktx *kernel.Transaction, receipt *kernel.Receipt, stats *kernel.Stats, idempotencyRecordID, idempotencyResultJSON, stepID, label string) error {
	if err := s.insertAuditRows(ctx, tx, ktx, receipt, label); err != nil {
		return err
	}
	if err := s.updateAncestorTraces(ctx, tx, ktx.TraceID, ktx.Gross, ktx.EndedAt, label); err != nil {
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

func (s *DB) CommitCall(ctx context.Context, ktx *kernel.Transaction, receipt *kernel.Receipt, processID, targetUserID, feeRecipientID string, net, fee int64, stats *kernel.Stats, idempotencyRecordID, stepID string) error {
	return s.withTx(ctx, "commit call", func(tx *sql.Tx) error {
		gross := net + fee
		if gross > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE processes SET locked=locked-? WHERE id=?`, gross, processID); err != nil {
				return dbErr(err, "commit call: debit process locked")
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET locked=locked-? WHERE id=?`, gross, ktx.OwnerUserID); err != nil {
				return dbErr(err, "commit call: debit owner locked")
			}
		}
		if net > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET available=available+? WHERE id=?`, net, targetUserID); err != nil {
				return dbErr(err, "commit call: credit target")
			}
		}
		// Credit fee recipient — hard-fail if unset or nonexistent to prevent fund destruction.
		if fee > 0 {
			if feeRecipientID == "" {
				return fmt.Errorf("commit call: fee %d > 0 but feeRecipientID is empty: funds would be destroyed", fee)
			}
			res, feeErr := tx.ExecContext(ctx,
				`UPDATE users SET available=available+? WHERE id=?`, fee, feeRecipientID)
			if feeErr != nil {
				return dbErr(feeErr, "commit call: credit fee recipient")
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return fmt.Errorf("commit call: fee recipient %q not found: funds would be destroyed", feeRecipientID)
			}
		}
		return s.finalizeTx(ctx, tx, ktx, receipt, stats, idempotencyRecordID, rawJSONStr(ktx.ReplyJSON), stepID, "commit call")
	})
}

func (s *DB) CommitFailedCall(ctx context.Context, ktx *kernel.Transaction, receipt *kernel.Receipt, processID string, gross int64, stats *kernel.Stats, idempotencyRecordID, errorCode, stepID string) error {
	return s.withTx(ctx, "commit failed call", func(tx *sql.Tx) error {
		if gross > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE processes SET available=available+?, locked=locked-? WHERE id=?`,
				gross, gross, processID,
			); err != nil {
				return dbErr(err, "commit failed call: refund process")
			}
		}
		errResult, _ := json.Marshal(map[string]string{"error": ktx.Reason, "code": errorCode})
		return s.finalizeTx(ctx, tx, ktx, receipt, stats, idempotencyRecordID, string(errResult), stepID, "commit failed call")
	})
}

func scanProcessRows(rows *sql.Rows) ([]*kernel.Process, error) {
	var out []*kernel.Process
	for rows.Next() {
		var p kernel.Process
		var status, createdAt string
		var endedAt *string
		if err := rows.Scan(&p.ID, &p.OwnerUserID, &p.Available, &p.Locked, &status, &createdAt, &endedAt); err != nil {
			return nil, dbErr(err, "scan process")
		}
		p.Status = kernel.ProcessStatus(status)
		p.CreatedAt = strToTime(createdAt)
		p.EndedAt = strToNullTime(endedAt)
		out = append(out, &p)
	}
	return out, rows.Err()
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
	return scanProcessRows(rows)
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
	return scanProcessRows(rows)
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
		if locked > 0 {
			return kernel.ErrInvalidState.Wrap("process has locked funds")
		}
		now := timeToStr(time.Now().UTC())
		if available > 0 {
			if _, err = tx.ExecContext(ctx,
				`UPDATE users SET available=available+?, locked=locked-? WHERE id=?`,
				available, available, ownerID); err != nil {
				return dbErr(err, "end process: return funds")
			}
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE processes SET status='closed', available=0, locked=0, ended_at=? WHERE id=?`,
			now, processID)
		return dbErr(err, "end process: close")
	})
}

// ---- Traces ----

func (s *DB) ReadTrace(ctx context.Context, id string) (*kernel.Trace, error) {
	var t kernel.Trace
	var createdAt string
	var parentID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id,process_id,parent_trace_id,action_owner_id,cost,latency_ms,created_at FROM traces WHERE id=?`, id,
	).Scan(&t.ID, &t.ProcessID, &parentID, &t.ActionOwnerID, &t.Cost, &t.LatencyMS, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("trace not found")
	}
	if err != nil {
		return nil, dbErr(err, "read trace")
	}
	t.CreatedAt = strToTime(createdAt)
	if parentID.Valid {
		t.ParentTraceID = &parentID.String
	}
	return &t, nil
}

func (s *DB) ReadRootTrace(ctx context.Context, processID string) (*kernel.Trace, error) {
	var t kernel.Trace
	var createdAt string
	var parentID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id,process_id,parent_trace_id,action_owner_id,cost,latency_ms,created_at
		 FROM traces WHERE process_id=? AND parent_trace_id IS NULL LIMIT 1`, processID,
	).Scan(&t.ID, &t.ProcessID, &parentID, &t.ActionOwnerID, &t.Cost, &t.LatencyMS, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("root trace not found for process")
	}
	if err != nil {
		return nil, dbErr(err, "read root trace")
	}
	t.CreatedAt = strToTime(createdAt)
	if parentID.Valid {
		t.ParentTraceID = &parentID.String
	}
	return &t, nil
}

// ---- Transactions ----

const txColumns = `id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,target_user_id,` +
	`action_id,action_name,remote_action_id,args_json,reply_json,status,gross,net,fee,reason,remote_receipt_hash,remote_receipt_json,started_at,ended_at`

// scanTx scans one transaction row using the provided scan function.
// scan must be called with exactly the destinations expected by txColumns.
func scanTx(scan func(...any) error) (kernel.Transaction, error) {
	var tx kernel.Transaction
	var status, startedAt, endedAt, argsJSON, replyJSON string
	var remoteReceiptHash *string
	if err := scan(&tx.ID, &tx.ProcessID, &tx.TraceID, &tx.ParentTraceID,
		&tx.OwnerUserID, &tx.CallerUserID, &tx.TargetUserID, &tx.ActionID, &tx.ActionName, &tx.RemoteActionID,
		&argsJSON, &replyJSON, &status,
		&tx.Gross, &tx.Net, &tx.Fee, &tx.Reason, &remoteReceiptHash, &tx.RemoteReceiptJSON,
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
	defer rows.Close()

	var out []*kernel.Transaction
	for rows.Next() {
		tx, err := scanTx(rows.Scan)
		if err != nil {
			return nil, dbErr(err, "scan transaction")
		}
		out = append(out, &tx)
	}
	return out, rows.Err()
}

func (s *DB) ListAllTransactions(ctx context.Context, limit, offset int) ([]*kernel.Transaction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+txColumns+` FROM transactions ORDER BY started_at DESC LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, dbErr(err, "list all transactions")
	}
	defer rows.Close()
	var out []*kernel.Transaction
	for rows.Next() {
		tx, err := scanTx(rows.Scan)
		if err != nil {
			return nil, dbErr(err, "scan transaction")
		}
		out = append(out, &tx)
	}
	return out, rows.Err()
}

// ---- Stats ----

func (s *DB) ReadStats(ctx context.Context, actionID string) (*kernel.Stats, error) {
	var st kernel.Stats
	var lastUsed string
	err := s.db.QueryRowContext(ctx,
		`SELECT action_id,uses,successes,failures,rating_count,price_mean,latency_mean,rating_mean,last_used_at
		 FROM action_stats WHERE action_id=?`, actionID,
	).Scan(&st.ActionID, &st.Uses, &st.Successes, &st.Failures, &st.RatingCount,
		&st.PriceMean, &st.LatencyMean, &st.RatingMean, &lastUsed)
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
		`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,price_mean,latency_mean,rating_mean,last_used_at)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(action_id) DO UPDATE SET
		   uses=excluded.uses, successes=excluded.successes, failures=excluded.failures,
		   rating_count=excluded.rating_count, price_mean=excluded.price_mean,
		   latency_mean=excluded.latency_mean, rating_mean=excluded.rating_mean,
		   last_used_at=excluded.last_used_at`,
		st.ActionID, st.Uses, st.Successes, st.Failures, st.RatingCount,
		st.PriceMean, st.LatencyMean, st.RatingMean, timeToStr(st.LastUsedAt),
	)
	return dbErr(err, "upsert stats")
}

// ---- Steps ----

func (s *DB) CreateStep(ctx context.Context, step *kernel.Step) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO steps (id,process_id,parent_trace_id,required_caller_user_id,next_action_id,
		                    partial_args,input_schema,status,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		step.ID, step.ProcessID, nullStrPtr(step.ParentTraceID), step.RequiredCallerUserID,
		step.NextActionID, rawJSONStr(step.PartialArgs), rawJSONStr(step.InputSchema),
		string(step.Status), timeToStr(step.CreatedAt),
	)
	return dbErr(err, "create step")
}

func (s *DB) ReadStep(ctx context.Context, id string) (*kernel.Step, error) {
	var step kernel.Step
	var parentTraceID, txID *string
	var createdAt, partialArgs, inputSchema, status string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,process_id,parent_trace_id,required_caller_user_id,next_action_id,
		        partial_args,input_schema,status,tx_id,created_at
		 FROM steps WHERE id=?`, id,
	).Scan(&step.ID, &step.ProcessID, &parentTraceID, &step.RequiredCallerUserID,
		&step.NextActionID, &partialArgs, &inputSchema, &status, &txID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("step not found")
	}
	if err != nil {
		return nil, dbErr(err, "read step")
	}
	step.ParentTraceID = parentTraceID
	step.PartialArgs = strToRawJSON(partialArgs)
	step.InputSchema = strToRawJSON(inputSchema)
	step.Status = kernel.StepStatus(status)
	step.TxID = txID
	step.CreatedAt = strToTime(createdAt)
	return &step, nil
}

func (s *DB) ListSteps(ctx context.Context, callerUserID, processID, status string, isSuperuser bool) ([]*kernel.Step, error) {
	superInt := 0
	if isSuperuser {
		superInt = 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,process_id,parent_trace_id,required_caller_user_id,next_action_id,
		        partial_args,input_schema,status,tx_id,created_at
		 FROM steps
		 WHERE (process_id IN (SELECT id FROM processes WHERE owner_user_id=?)
		        OR required_caller_user_id=?
		        OR ?)
		   AND (?='' OR process_id=?)
		   AND (?='' OR status=?)
		 ORDER BY created_at DESC`,
		callerUserID, callerUserID, superInt,
		processID, processID,
		status, status,
	)
	if err != nil {
		return nil, dbErr(err, "list steps")
	}
	defer rows.Close()
	var out []*kernel.Step
	for rows.Next() {
		var step kernel.Step
		var parentTraceID, txID *string
		var createdAt, partialArgs, inputSchema, stepStatus string
		if err := rows.Scan(&step.ID, &step.ProcessID, &parentTraceID, &step.RequiredCallerUserID,
			&step.NextActionID, &partialArgs, &inputSchema, &stepStatus, &txID, &createdAt); err != nil {
			return nil, dbErr(err, "scan step")
		}
		step.ParentTraceID = parentTraceID
		step.PartialArgs = strToRawJSON(partialArgs)
		step.InputSchema = strToRawJSON(inputSchema)
		step.Status = kernel.StepStatus(stepStatus)
		step.TxID = txID
		step.CreatedAt = strToTime(createdAt)
		out = append(out, &step)
	}
	return out, rows.Err()
}

func (s *DB) ClaimStep(ctx context.Context, stepID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE steps SET status='running' WHERE id=? AND status='waiting'`, stepID)
	if err != nil {
		return dbErr(err, "claim step")
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return kernel.ErrInvalidState.Wrap("step is not waiting")
	}
	return nil
}

func (s *DB) ResetStep(ctx context.Context, stepID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE steps SET status='waiting' WHERE id=? AND status='running' AND tx_id IS NULL`, stepID)
	return dbErr(err, "reset step")
}

func (s *DB) ResetRunningSteps(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE steps SET status='waiting' WHERE status='running' AND tx_id IS NULL`)
	return dbErr(err, "reset running steps")
}

func (s *DB) ResetInFlightCalls(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE processes SET available=available+locked, locked=0 WHERE status='open' AND locked>0`)
	return dbErr(err, "reset in-flight calls")
}

// nullStr converts an empty string to nil for nullable TEXT columns.
func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullStrPtr converts a *string to a SQL-compatible value: nil becomes nil (NULL), non-nil is passed through.
func nullStrPtr(s *string) *string { return s }

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
		`SELECT id,process_id,parent_trace_id,action_owner_id,cost,latency_ms,created_at FROM traces WHERE process_id=?`, processID)
	if err != nil {
		return nil, dbErr(err, "list traces")
	}
	defer rows.Close()
	var out []*kernel.Trace
	for rows.Next() {
		var t kernel.Trace
		var createdAt string
		var parentID sql.NullString
		if err := rows.Scan(&t.ID, &t.ProcessID, &parentID, &t.ActionOwnerID, &t.Cost, &t.LatencyMS, &createdAt); err != nil {
			return nil, dbErr(err, "scan trace")
		}
		t.CreatedAt = strToTime(createdAt)
		if parentID.Valid {
			t.ParentTraceID = &parentID.String
		}
		out = append(out, &t)
	}
	return out, rows.Err()
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

// ---- Config ----

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

func (s *DB) InitFirstBoot(ctx context.Context, u *kernel.User, configs map[string]string) error {
	return s.withTx(ctx, "init first boot", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO users (id,handle,email,password_hash,available,locked,created_at,updated_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			u.ID, u.Handle, u.Email, u.PasswordHash,
			u.Available, u.Locked, timeToStr(u.CreatedAt), timeToStr(u.UpdatedAt),
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

// ---- Deposits ----

func (s *DB) CreateDeposit(ctx context.Context, d *kernel.Deposit) error {
	return s.withTx(ctx, "deposit", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO deposits (id,operator_user_id,target_user_id,amount,reason,created_at)
			 VALUES (?,?,?,?,?,?)`,
			d.ID, d.OperatorUserID, d.TargetUserID, d.Amount, d.Reason, timeToStr(d.CreatedAt),
		); err != nil {
			return dbErr(err, "insert deposit")
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE users SET available=available+? WHERE id=?`, d.Amount, d.TargetUserID)
		return dbErr(err, "deposit: update user balance")
	})
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

// ---- Federation ----

func (s *DB) UpdateRemoteProxySourceURLs(ctx context.Context, ownerUserID, oldBase, newBase string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE actions SET source=REPLACE(source,?,?) WHERE owner_user_id=? AND kind='remote_proxy'`,
		oldBase, newBase, ownerUserID,
	)
	return dbErr(err, "update remote proxy source urls")
}

func (s *DB) ReadRemoteKernelByBaseURL(ctx context.Context, baseURL string) (*kernel.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id,handle,email,password_hash,available,locked,suspended_at,public_key,remote_base_url,created_at,updated_at
		 FROM users WHERE remote_base_url=? AND remote_base_url!=''`, baseURL))
}

// ---- helpers ----

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func dbErr(err error, op string) error {
	if err == nil {
		return nil
	}
	return kernel.ErrInternal.Wrapf("%s: %v", op, err)
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

func (s *DB) ReadReceiptByTxID(ctx context.Context, txID string) (*kernel.Receipt, error) {
	var r kernel.Receipt
	var status, startedAt, createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,issuer_user_id,tx_id,trace_id,action_id,caller_user_id,process_id,
		        args_hash,reply_hash,status,gross,net,fee,reason,started_at,created_at,signature
		 FROM receipts WHERE tx_id=?`, txID,
	).Scan(&r.ID, &r.IssuerUserID, &r.TxID, &r.TraceID, &r.ActionID,
		&r.CallerUserID, &r.ProcessID,
		&r.ArgsHash, &r.ReplyHash, &status,
		&r.Gross, &r.Net, &r.Fee, &r.Reason, &startedAt, &createdAt, &r.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("receipt not found")
	}
	if err != nil {
		return nil, dbErr(err, "read receipt by tx_id")
	}
	r.Status = kernel.TxStatus(status)
	r.StartedAt = strToTime(startedAt)
	r.CreatedAt = strToTime(createdAt)
	return &r, nil
}

func (s *DB) ReadReceipt(ctx context.Context, id string) (*kernel.Receipt, error) {
	var r kernel.Receipt
	var status, startedAt, createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,issuer_user_id,tx_id,trace_id,action_id,caller_user_id,process_id,
		        args_hash,reply_hash,status,gross,net,fee,reason,started_at,created_at,signature
		 FROM receipts WHERE id=?`, id,
	).Scan(&r.ID, &r.IssuerUserID, &r.TxID, &r.TraceID, &r.ActionID,
		&r.CallerUserID, &r.ProcessID,
		&r.ArgsHash, &r.ReplyHash, &status,
		&r.Gross, &r.Net, &r.Fee, &r.Reason, &startedAt, &createdAt, &r.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("receipt not found")
	}
	if err != nil {
		return nil, dbErr(err, "read receipt")
	}
	r.Status = kernel.TxStatus(status)
	r.StartedAt = strToTime(startedAt)
	r.CreatedAt = strToTime(createdAt)
	return &r, nil
}

// ---- Ratings ----

func (s *DB) CreateRatingAndUpdateStats(ctx context.Context, r *kernel.Rating, actionID string, rating float64) error {
	return s.withTx(ctx, "create rating and update stats", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ratings (id,rated_tx_id,rated_receipt_id,rater_user_id,rating,note,created_at,signature)
			 VALUES (?,?,?,?,?,?,?,?)`,
			r.ID, r.RatedTxID, r.RatedReceiptID, r.RaterUserID, r.Rating, r.Note,
			timeToStr(r.CreatedAt), r.Signature,
		); err != nil {
			return dbErr(err, "create rating and update stats: insert rating")
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE action_stats SET
			   rating_count = rating_count + 1,
			   rating_mean  = rating_mean + (? - rating_mean) / (rating_count + 1)
			 WHERE action_id = ?`,
			rating, actionID,
		)
		return dbErr(err, "create rating and update stats: update stats")
	})
}

func (s *DB) ListRatings(ctx context.Context, actionID string, limit, offset int) ([]*kernel.Rating, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.rated_tx_id, r.rated_receipt_id, r.rater_user_id, r.rating, r.note, r.created_at, r.signature
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
	defer rows.Close()
	var result []*kernel.Rating
	for rows.Next() {
		var r kernel.Rating
		var ratedReceiptID *string
		var createdAt string
		if err := rows.Scan(&r.ID, &r.RatedTxID, &ratedReceiptID, &r.RaterUserID, &r.Rating, &r.Note, &createdAt, &r.Signature); err != nil {
			return nil, dbErr(err, "list ratings: scan")
		}
		r.RatedReceiptID = ratedReceiptID
		r.CreatedAt = strToTime(createdAt)
		result = append(result, &r)
	}
	return result, dbErr(rows.Err(), "list ratings: rows")
}

func (s *DB) ReadRatingByTxID(ctx context.Context, txID string) (*kernel.Rating, error) {
	var r kernel.Rating
	var ratedReceiptID *string
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,rated_tx_id,rated_receipt_id,rater_user_id,rating,note,created_at,signature
		 FROM ratings WHERE rated_tx_id=?`, txID,
	).Scan(&r.ID, &r.RatedTxID, &ratedReceiptID, &r.RaterUserID, &r.Rating, &r.Note, &createdAt, &r.Signature)
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
		`INSERT INTO idempotency_records (id,idempotency_key,counterparty_user_id,receipt_id,status,result_json,created_at,expires_at)
		 VALUES (?,?,?,NULL,'pending','',?,?)`,
		r.ID, r.IdempotencyKey, r.CounterpartyUserID,
		timeToStr(r.CreatedAt), timeToStr(r.ExpiresAt),
	)
	return dbErr(err, "insert pending idempotency record")
}

func (s *DB) DeleteIdempotencyRecord(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_records WHERE id=?`, id,
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

func (s *DB) ReadIdempotencyRecord(ctx context.Context, key, counterpartyUserID string) (*kernel.IdempotencyRecord, error) {
	var r kernel.IdempotencyRecord
	var receiptID *string
	var createdAt, expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,idempotency_key,counterparty_user_id,receipt_id,status,result_json,receipt_json,created_at,expires_at
		 FROM idempotency_records
		 WHERE idempotency_key=? AND counterparty_user_id=? AND datetime(expires_at) > datetime('now')`,
		key, counterpartyUserID,
	).Scan(&r.ID, &r.IdempotencyKey, &r.CounterpartyUserID, &receiptID, &r.Status, &r.ResultJSON, &r.ReceiptJSON, &createdAt, &expiresAt)
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
