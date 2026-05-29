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
	if err == nil {
		return false
	}
	msg := err.Error()
	return isDuplicateColumn(err) || strings.Contains(msg, "no such column")
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

func (s *DB) UpdateUser(ctx context.Context, u *kernel.User) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET handle=?,email=?,public_key=?,remote_base_url=?,updated_at=? WHERE id=?`,
		u.Handle, u.Email, nullStr(u.PublicKey), nullStr(u.RemoteBaseURL),
		timeToStr(u.UpdatedAt), u.ID,
	)
	return dbErr(err, "update user")
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
		 (id,owner_user_id,name,kind,active,public,price,description,input_schema,output_schema,source,artifact_hash,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerUserID, a.Name, string(a.Kind), boolInt(a.Active), boolInt(a.Public), a.Price,
		a.Description, string(inJSON), string(outJSON), a.Source, a.ArtifactHash,
		timeToStr(a.CreatedAt), timeToStr(a.UpdatedAt),
	)
	return dbErr(err, "create action")
}

func (s *DB) ReadAction(ctx context.Context, id string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT id,owner_user_id,name,kind,active,public,price,description,input_schema,output_schema,source,artifact_hash,created_at,updated_at,deleted_at
		 FROM actions WHERE id=? AND deleted_at IS NULL`, id))
}

func (s *DB) ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT id,owner_user_id,name,kind,active,public,price,description,input_schema,output_schema,source,artifact_hash,created_at,updated_at,deleted_at
		 FROM actions WHERE owner_user_id=? AND name=? AND deleted_at IS NULL`, ownerID, name))
}

func (s *DB) UpdateAction(ctx context.Context, a *kernel.Action) error {
	inJSON, _ := json.Marshal(a.InputSchema)
	outJSON, _ := json.Marshal(a.OutputSchema)
	_, err := s.db.ExecContext(ctx,
		`UPDATE actions SET kind=?,active=?,public=?,price=?,description=?,input_schema=?,output_schema=?,
		 source=?,artifact_hash=?,updated_at=? WHERE id=?`,
		string(a.Kind), boolInt(a.Active), boolInt(a.Public), a.Price, a.Description,
		string(inJSON), string(outJSON), a.Source, a.ArtifactHash,
		timeToStr(a.UpdatedAt), a.ID,
	)
	return dbErr(err, "update action")
}

func (s *DB) DeleteAction(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin delete action")
	}
	defer tx.Rollback()
	now := timeToStr(time.Now().UTC())
	if _, err := tx.ExecContext(ctx,
		`UPDATE actions SET active=0, deleted_at=? WHERE id=?`, now, id); err != nil {
		return dbErr(err, "delete action: soft delete")
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM acl_entries WHERE action_id=?`, id); err != nil {
		return dbErr(err, "delete action: purge acl")
	}
	return dbErr(tx.Commit(), "delete action: commit")
}

func (s *DB) ListActions(ctx context.Context, activeOnly bool, limit, offset int) ([]*kernel.Action, error) {
	q := `SELECT id,owner_user_id,name,kind,active,public,price,description,input_schema,output_schema,source,artifact_hash,created_at,updated_at,deleted_at FROM actions WHERE deleted_at IS NULL`
	args := []any{}
	if activeOnly {
		q += ` AND active=1 AND public=1`
	}
	q += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, dbErr(err, "list actions")
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
		`SELECT id,owner_user_id,name,kind,active,public,price,description,input_schema,output_schema,source,artifact_hash,created_at,updated_at,deleted_at
		 FROM actions WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
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

func (s *DB) UpdateActionEmbedding(ctx context.Context, actionID string, vec []float32) error {
	vecJSON, err := json.Marshal(vec)
	if err != nil {
		return dbErr(err, "marshal embedding")
	}
	_, err = s.db.ExecContext(ctx, `UPDATE actions SET embed_vec=? WHERE id=?`, string(vecJSON), actionID)
	return dbErr(err, "update action embedding")
}

func (s *DB) ListActionEmbeddings(ctx context.Context, limit int) (map[string][]float32, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, embed_vec FROM actions WHERE active=1 AND embed_vec IS NOT NULL LIMIT ?`, limit)
	if err != nil {
		return nil, dbErr(err, "list action embeddings")
	}
	defer rows.Close()
	out := make(map[string][]float32)
	for rows.Next() {
		var id, vecJSON string
		if err := rows.Scan(&id, &vecJSON); err != nil {
			return nil, dbErr(err, "scan action embedding")
		}
		var vec []float32
		if err := json.Unmarshal([]byte(vecJSON), &vec); err != nil {
			continue
		}
		out[id] = vec
	}
	return out, rows.Err()
}

func (s *DB) scanAction(row *sql.Row) (*kernel.Action, error) {
	var a kernel.Action
	var kind, inJSON, outJSON, createdAt, updatedAt string
	var active, public int
	var deletedAt *string
	err := row.Scan(&a.ID, &a.OwnerUserID, &a.Name, &kind, &active, &public, &a.Price,
		&a.Description, &inJSON, &outJSON, &a.Source, &a.ArtifactHash,
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
	var active, public int
	var deletedAt *string
	err := rows.Scan(&a.ID, &a.OwnerUserID, &a.Name, &kind, &active, &public, &a.Price,
		&a.Description, &inJSON, &outJSON, &a.Source, &a.ArtifactHash,
		&createdAt, &updatedAt, &deletedAt)
	if err != nil {
		return nil, dbErr(err, "scan action")
	}
	return finishAction(&a, kind, active, public, inJSON, outJSON, createdAt, updatedAt, deletedAt)
}

func finishAction(a *kernel.Action, kind string, active, public int, inJSON, outJSON, createdAt, updatedAt string, deletedAt *string) (*kernel.Action, error) {
	a.Kind = kernel.ActionKind(kind)
	a.Active = active != 0
	a.Public = public != 0
	a.CreatedAt = strToTime(createdAt)
	a.UpdatedAt = strToTime(updatedAt)
	if deletedAt != nil {
		t := strToTime(*deletedAt)
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

// ---- ACL ----

func (s *DB) GrantACL(ctx context.Context, e *kernel.ACLEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO acl_entries (subject_user_id,action_id,permission,created_at)
		 VALUES (?,?,?,?)`,
		e.SubjectUserID, e.ActionID, string(e.Permission), timeToStr(e.CreatedAt),
	)
	return dbErr(err, "grant acl")
}

func (s *DB) RevokeACL(ctx context.Context, subjectID, actionID string, perm kernel.Permission) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM acl_entries WHERE subject_user_id=? AND action_id=? AND permission=?`,
		subjectID, actionID, string(perm),
	)
	return dbErr(err, "revoke acl")
}

func (s *DB) CheckACL(ctx context.Context, subjectID, actionID string, perm kernel.Permission) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM acl_entries WHERE subject_user_id=? AND action_id=? AND permission=?`,
		subjectID, actionID, string(perm),
	).Scan(&count)
	if err != nil {
		return false, dbErr(err, "check acl")
	}
	return count > 0, nil
}

// ---- Processes ----

func (s *DB) StartProcess(ctx context.Context, p *kernel.Process, t *kernel.Trace, ownerID string, funds int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin start process")
	}
	defer tx.Rollback()

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

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO traces (id,process_id,parent_trace_id,caused_by_trace_id,cost,latency_ms,created_at) VALUES (?,?,?,?,?,?,?)`,
		t.ID, t.ProcessID, t.ParentTraceID, t.CausedByTraceID, t.Cost, t.LatencyMS, timeToStr(t.CreatedAt),
	); err != nil {
		return dbErr(err, "start process: insert trace")
	}

	return dbErr(tx.Commit(), "start process: commit")
}

func (s *DB) CreateProcess(ctx context.Context, p *kernel.Process) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO processes (id,owner_user_id,available,locked,status,created_at,ended_at)
		 VALUES (?,?,?,?,?,?,?)`,
		p.ID, p.OwnerUserID, p.Available, p.Locked, string(p.Status),
		timeToStr(p.CreatedAt), nullTimeToStr(p.EndedAt),
	)
	return dbErr(err, "create process")
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

func (s *DB) LockFunds(ctx context.Context, processID string, amount int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE processes SET available=available-?, locked=locked+?
		 WHERE id=? AND available>=? AND status='open'`,
		amount, amount, processID, amount,
	)
	if err != nil {
		return dbErr(err, "lock funds")
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return kernel.ErrInsufficientFunds.Wrap("not enough process funds or process closed")
	}
	return nil
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin fund process")
	}
	defer tx.Rollback()

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

	_, err = tx.ExecContext(ctx,
		`UPDATE processes SET available=available+? WHERE id=?`,
		amount, processID,
	)
	if err != nil {
		return dbErr(err, "fund process: credit process")
	}
	return dbErr(tx.Commit(), "fund process commit")
}

func (s *DB) CommitCall(ctx context.Context, ktx *kernel.Transaction, receipt *kernel.Receipt, processID, targetUserID, feeRecipientID string, net, fee int64, stats *kernel.Stats) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin commit call")
	}
	defer tx.Rollback()

	// Insert transaction record (immutable — no rating column written).
	_, err = tx.ExecContext(ctx,
		`INSERT INTO transactions
		 (id,process_id,trace_id,parent_trace_id,owner_user_id,subject_user_id,target_user_id,
		  action_id,args_json,reply_json,status,gross,net,fee,reason,remote_receipt_hash,started_at,ended_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ktx.ID, ktx.ProcessID, ktx.TraceID, ktx.ParentTraceID,
		ktx.OwnerUserID, ktx.SubjectUserID, ktx.TargetUserID, ktx.ActionID,
		ktx.ArgsJSON, ktx.ReplyJSON, string(ktx.Status),
		ktx.Gross, ktx.Net, ktx.Fee, ktx.Reason, nullStr(ktx.RemoteReceiptHash),
		timeToStr(ktx.StartedAt), timeToStr(ktx.EndedAt),
	)
	if err != nil {
		return dbErr(err, "commit call: insert transaction")
	}

	// Receipt is mandatory.
	if receipt == nil {
		return dbErr(fmt.Errorf("receipt is required"), "commit call")
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO receipts (id,issuer_user_id,tx_id,trace_id,action_id,args_hash,reply_hash,status,gross,net,fee,reason,created_at,signature)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.ID, receipt.IssuerUserID, receipt.TxID, receipt.TraceID, receipt.ActionID,
		receipt.ArgsHash, receipt.ReplyHash, string(receipt.Status),
		receipt.Gross, receipt.Net, receipt.Fee, receipt.Reason,
		timeToStr(receipt.CreatedAt), receipt.Signature,
	); err != nil {
		return dbErr(err, "commit call: insert receipt")
	}

	gross := net + fee
	if gross > 0 {
		// Debit process.locked and owner.locked.
		var ownerID string
		row := tx.QueryRowContext(ctx, `SELECT owner_user_id FROM processes WHERE id=?`, processID)
		_ = row.Scan(&ownerID)

		if _, err = tx.ExecContext(ctx,
			`UPDATE processes SET locked=locked-? WHERE id=?`, gross, processID); err != nil {
			return dbErr(err, "commit call: debit process locked")
		}
		if ownerID != "" {
			if _, err = tx.ExecContext(ctx,
				`UPDATE users SET locked=locked-? WHERE id=?`, gross, ownerID); err != nil {
				return dbErr(err, "commit call: debit owner locked")
			}
		}
	}

	// Credit target.
	if net > 0 {
		if _, err = tx.ExecContext(ctx,
			`UPDATE users SET available=available+? WHERE id=?`, net, targetUserID); err != nil {
			return dbErr(err, "commit call: credit target")
		}
	}

	// Credit fee recipient.
	if fee > 0 && feeRecipientID != "" {
		if _, err = tx.ExecContext(ctx,
			`UPDATE users SET available=available+? WHERE id=?`, fee, feeRecipientID); err != nil {
			return dbErr(err, "commit call: credit fee recipient")
		}
	}

	// Update trace cost and latency for all ancestor traces atomically.
	if _, err = tx.ExecContext(ctx, `
WITH RECURSIVE ancestors(id, parent_id) AS (
    SELECT id, parent_trace_id FROM traces WHERE id=?
    UNION ALL
    SELECT t.id, t.parent_trace_id FROM traces t
    JOIN ancestors a ON t.id=a.parent_id AND a.id!=a.parent_id
)
UPDATE traces SET
    cost=cost+?,
    latency_ms=MAX(latency_ms, CAST((julianday(?)-julianday(created_at))*86400000 AS INTEGER))
WHERE id IN (SELECT id FROM ancestors)`,
		ktx.TraceID, ktx.Gross, timeToStr(ktx.EndedAt),
	); err != nil {
		return dbErr(err, "commit call: update trace cost")
	}

	// Upsert action stats.
	if stats != nil {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,price_mean,latency_mean,rating_mean,last_used_at)
			 VALUES (?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(action_id) DO UPDATE SET
			   uses=excluded.uses, successes=excluded.successes, failures=excluded.failures,
			   rating_count=excluded.rating_count, price_mean=excluded.price_mean,
			   latency_mean=excluded.latency_mean, rating_mean=excluded.rating_mean,
			   last_used_at=excluded.last_used_at`,
			stats.ActionID, stats.Uses, stats.Successes, stats.Failures, stats.RatingCount,
			stats.PriceMean, stats.LatencyMean, stats.RatingMean, timeToStr(stats.LastUsedAt),
		); err != nil {
			return dbErr(err, "commit call: upsert stats")
		}
	}

	return dbErr(tx.Commit(), "commit call: commit")
}

func (s *DB) CommitFailedCall(ctx context.Context, ktx *kernel.Transaction, receipt *kernel.Receipt, processID string, gross int64, stats *kernel.Stats) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin commit failed call")
	}
	defer tx.Rollback()

	if gross > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE processes SET available=available+?, locked=locked-? WHERE id=?`,
			gross, gross, processID,
		); err != nil {
			return dbErr(err, "commit failed call: refund process")
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO transactions
		 (id,process_id,trace_id,parent_trace_id,owner_user_id,subject_user_id,target_user_id,
		  action_id,args_json,reply_json,status,gross,net,fee,reason,remote_receipt_hash,started_at,ended_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ktx.ID, ktx.ProcessID, ktx.TraceID, ktx.ParentTraceID,
		ktx.OwnerUserID, ktx.SubjectUserID, ktx.TargetUserID, ktx.ActionID,
		ktx.ArgsJSON, ktx.ReplyJSON, string(ktx.Status),
		ktx.Gross, ktx.Net, ktx.Fee, ktx.Reason, nullStr(ktx.RemoteReceiptHash),
		timeToStr(ktx.StartedAt), timeToStr(ktx.EndedAt),
	); err != nil {
		return dbErr(err, "commit failed call: insert transaction")
	}

	// Receipt is mandatory.
	if receipt == nil {
		return dbErr(fmt.Errorf("receipt is required"), "commit failed call")
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO receipts (id,issuer_user_id,tx_id,trace_id,action_id,args_hash,reply_hash,status,gross,net,fee,reason,created_at,signature)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.ID, receipt.IssuerUserID, receipt.TxID, receipt.TraceID, receipt.ActionID,
		receipt.ArgsHash, receipt.ReplyHash, string(receipt.Status),
		receipt.Gross, receipt.Net, receipt.Fee, receipt.Reason,
		timeToStr(receipt.CreatedAt), receipt.Signature,
	); err != nil {
		return dbErr(err, "commit failed call: insert receipt")
	}

	// Update trace latency (cost delta is 0 for failures).
	if _, err := tx.ExecContext(ctx, `
WITH RECURSIVE ancestors(id, parent_id) AS (
    SELECT id, parent_trace_id FROM traces WHERE id=?
    UNION ALL
    SELECT t.id, t.parent_trace_id FROM traces t
    JOIN ancestors a ON t.id=a.parent_id AND a.id!=a.parent_id
)
UPDATE traces SET
    latency_ms=MAX(latency_ms, CAST((julianday(?)-julianday(created_at))*86400000 AS INTEGER))
WHERE id IN (SELECT id FROM ancestors)`,
		ktx.TraceID, timeToStr(ktx.EndedAt),
	); err != nil {
		return dbErr(err, "commit failed call: update trace latency")
	}

	// Upsert action stats.
	if stats != nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,price_mean,latency_mean,rating_mean,last_used_at)
			 VALUES (?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(action_id) DO UPDATE SET
			   uses=excluded.uses, successes=excluded.successes, failures=excluded.failures,
			   rating_count=excluded.rating_count, price_mean=excluded.price_mean,
			   latency_mean=excluded.latency_mean, rating_mean=excluded.rating_mean,
			   last_used_at=excluded.last_used_at`,
			stats.ActionID, stats.Uses, stats.Successes, stats.Failures, stats.RatingCount,
			stats.PriceMean, stats.LatencyMean, stats.RatingMean, timeToStr(stats.LastUsedAt),
		); err != nil {
			return dbErr(err, "commit failed call: upsert stats")
		}
	}

	return dbErr(tx.Commit(), "commit failed call: commit")
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

func (s *DB) EndProcess(ctx context.Context, processID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin end process")
	}
	defer tx.Rollback()

	var ownerID string
	var available, locked int64
	err = tx.QueryRowContext(ctx,
		`SELECT owner_user_id, available, locked FROM processes WHERE id=? AND status='open'`,
		processID,
	).Scan(&ownerID, &available, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return kernel.ErrInvalidState.Wrap("process not open")
	}
	if err != nil {
		return dbErr(err, "end process: read")
	}

	total := available + locked
	now := timeToStr(time.Now().UTC())

	// Return all funds to owner.
	if total > 0 {
		_, err = tx.ExecContext(ctx,
			`UPDATE users SET available=available+?, locked=locked-? WHERE id=?`,
			total, total, ownerID)
		if err != nil {
			return dbErr(err, "end process: return funds")
		}
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE processes SET status='closed', available=0, locked=0, ended_at=? WHERE id=?`,
		now, processID)
	if err != nil {
		return dbErr(err, "end process: close")
	}

	return dbErr(tx.Commit(), "end process commit")
}

// ---- Traces ----

func (s *DB) CreateTrace(ctx context.Context, t *kernel.Trace) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO traces (id,process_id,parent_trace_id,caused_by_trace_id,cost,latency_ms,created_at) VALUES (?,?,?,?,?,?,?)`,
		t.ID, t.ProcessID, t.ParentTraceID, t.CausedByTraceID, t.Cost, t.LatencyMS, timeToStr(t.CreatedAt),
	)
	return dbErr(err, "create trace")
}

func (s *DB) ReadTrace(ctx context.Context, id string) (*kernel.Trace, error) {
	var t kernel.Trace
	var createdAt string
	var causedBy sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id,process_id,parent_trace_id,caused_by_trace_id,cost,latency_ms,created_at FROM traces WHERE id=?`, id,
	).Scan(&t.ID, &t.ProcessID, &t.ParentTraceID, &causedBy, &t.Cost, &t.LatencyMS, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("trace not found")
	}
	if err != nil {
		return nil, dbErr(err, "read trace")
	}
	t.CreatedAt = strToTime(createdAt)
	if causedBy.Valid {
		t.CausedByTraceID = &causedBy.String
	}
	return &t, nil
}

func (s *DB) ReadRootTrace(ctx context.Context, processID string) (*kernel.Trace, error) {
	var t kernel.Trace
	var createdAt string
	var causedBy sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id,process_id,parent_trace_id,caused_by_trace_id,cost,latency_ms,created_at
		 FROM traces WHERE process_id=? AND parent_trace_id=id LIMIT 1`, processID,
	).Scan(&t.ID, &t.ProcessID, &t.ParentTraceID, &causedBy, &t.Cost, &t.LatencyMS, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("root trace not found for process")
	}
	if err != nil {
		return nil, dbErr(err, "read root trace")
	}
	t.CreatedAt = strToTime(createdAt)
	if causedBy.Valid {
		t.CausedByTraceID = &causedBy.String
	}
	return &t, nil
}

// ---- Transactions ----

func (s *DB) CreateTransaction(ctx context.Context, tx *kernel.Transaction) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO transactions
		 (id,process_id,trace_id,parent_trace_id,owner_user_id,subject_user_id,target_user_id,
		  action_id,args_json,reply_json,status,gross,net,fee,reason,remote_receipt_hash,started_at,ended_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		tx.ID, tx.ProcessID, tx.TraceID, tx.ParentTraceID,
		tx.OwnerUserID, tx.SubjectUserID, tx.TargetUserID, tx.ActionID,
		tx.ArgsJSON, tx.ReplyJSON, string(tx.Status),
		tx.Gross, tx.Net, tx.Fee, tx.Reason, nullStr(tx.RemoteReceiptHash),
		timeToStr(tx.StartedAt), timeToStr(tx.EndedAt),
	)
	return dbErr(err, "create transaction")
}

func (s *DB) ReadTransaction(ctx context.Context, id string) (*kernel.Transaction, error) {
	var tx kernel.Transaction
	var status, startedAt, endedAt string
	var remoteReceiptHash *string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,process_id,trace_id,parent_trace_id,owner_user_id,subject_user_id,target_user_id,
		        action_id,args_json,reply_json,status,gross,net,fee,reason,remote_receipt_hash,started_at,ended_at
		 FROM transactions WHERE id=?`, id,
	).Scan(&tx.ID, &tx.ProcessID, &tx.TraceID, &tx.ParentTraceID,
		&tx.OwnerUserID, &tx.SubjectUserID, &tx.TargetUserID, &tx.ActionID,
		&tx.ArgsJSON, &tx.ReplyJSON, &status,
		&tx.Gross, &tx.Net, &tx.Fee, &tx.Reason, &remoteReceiptHash,
		&startedAt, &endedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("transaction not found")
	}
	if err != nil {
		return nil, dbErr(err, "read transaction")
	}
	tx.Status = kernel.TxStatus(status)
	tx.RemoteReceiptHash = strVal(remoteReceiptHash)
	tx.StartedAt = strToTime(startedAt)
	tx.EndedAt = strToTime(endedAt)
	return &tx, nil
}

func (s *DB) ListTransactions(ctx context.Context, f kernel.TxFilter) ([]*kernel.Transaction, error) {
	q := `SELECT id,process_id,trace_id,parent_trace_id,owner_user_id,subject_user_id,target_user_id,
	             action_id,args_json,reply_json,status,gross,net,fee,reason,remote_receipt_hash,started_at,ended_at
	      FROM transactions WHERE 1=1`
	args := []any{}
	if f.OwnerUserID != "" {
		q += ` AND owner_user_id=?`
		args = append(args, f.OwnerUserID)
	}
	if f.SubjectUserID != "" {
		q += ` AND subject_user_id=?`
		args = append(args, f.SubjectUserID)
	}
	if f.TargetUserID != "" {
		q += ` AND target_user_id=?`
		args = append(args, f.TargetUserID)
	}
	if f.ProcessID != "" {
		q += ` AND process_id=?`
		args = append(args, f.ProcessID)
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
		var tx kernel.Transaction
		var status, startedAt, endedAt string
		var remoteReceiptHash *string
		if err := rows.Scan(&tx.ID, &tx.ProcessID, &tx.TraceID, &tx.ParentTraceID,
			&tx.OwnerUserID, &tx.SubjectUserID, &tx.TargetUserID, &tx.ActionID,
			&tx.ArgsJSON, &tx.ReplyJSON, &status,
			&tx.Gross, &tx.Net, &tx.Fee, &tx.Reason, &remoteReceiptHash,
			&startedAt, &endedAt); err != nil {
			return nil, dbErr(err, "scan transaction")
		}
		tx.Status = kernel.TxStatus(status)
		tx.RemoteReceiptHash = strVal(remoteReceiptHash)
		tx.StartedAt = strToTime(startedAt)
		tx.EndedAt = strToTime(endedAt)
		out = append(out, &tx)
	}
	return out, rows.Err()
}

func (s *DB) ListAllTransactions(ctx context.Context, limit, offset int) ([]*kernel.Transaction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,process_id,trace_id,parent_trace_id,owner_user_id,subject_user_id,target_user_id,
		        action_id,args_json,reply_json,status,gross,net,fee,reason,remote_receipt_hash,started_at,ended_at
		 FROM transactions ORDER BY started_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list all transactions")
	}
	defer rows.Close()
	var out []*kernel.Transaction
	for rows.Next() {
		var tx kernel.Transaction
		var status, startedAt, endedAt string
		var remoteReceiptHash *string
		if err := rows.Scan(&tx.ID, &tx.ProcessID, &tx.TraceID, &tx.ParentTraceID,
			&tx.OwnerUserID, &tx.SubjectUserID, &tx.TargetUserID, &tx.ActionID,
			&tx.ArgsJSON, &tx.ReplyJSON, &status,
			&tx.Gross, &tx.Net, &tx.Fee, &tx.Reason, &remoteReceiptHash,
			&startedAt, &endedAt); err != nil {
			return nil, dbErr(err, "scan transaction")
		}
		tx.Status = kernel.TxStatus(status)
		tx.RemoteReceiptHash = strVal(remoteReceiptHash)
		tx.StartedAt = strToTime(startedAt)
		tx.EndedAt = strToTime(endedAt)
		out = append(out, &tx)
	}
	return out, rows.Err()
}

func (s *DB) UpdateTraceCostLatency(ctx context.Context, traceID string, grossDelta int64, endedAt time.Time) error {
	// Walk up the trace tree from traceID to the root, updating cost and latency_ms.
	cur := traceID
	for {
		t, err := s.ReadTrace(ctx, cur)
		if err != nil {
			return err
		}
		// Compute latency as ms from trace creation to endedAt.
		latencyMS := endedAt.Sub(t.CreatedAt).Milliseconds()

		_, err = s.db.ExecContext(ctx,
			`UPDATE traces SET cost=cost+?, latency_ms=MAX(latency_ms,?) WHERE id=?`,
			grossDelta, latencyMS, cur,
		)
		if err != nil {
			return dbErr(err, "update trace cost latency")
		}

		// Stop at root (parent_trace_id == id).
		if t.ParentTraceID == cur {
			break
		}
		cur = t.ParentTraceID
	}
	return nil
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

func (s *DB) UpsertStatTag(ctx context.Context, tag *kernel.StatTag) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO stat_tags (action_id,key,value,source,updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(action_id,key,source) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		tag.ActionID, tag.Key, tag.Value, tag.Source, timeToStr(tag.UpdatedAt),
	)
	return dbErr(err, "upsert stat tag")
}

// ---- Listeners & Events ----

func (s *DB) CreateListener(ctx context.Context, l *kernel.Listener) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO listeners (id,owner_user_id,source_user_id,event_name,target_action_id,active,created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		l.ID, l.OwnerUserID, l.SourceUserID, l.EventName,
		l.TargetActionID, boolInt(l.Active), timeToStr(l.CreatedAt),
	)
	return dbErr(err, "create listener")
}

func (s *DB) ReadListener(ctx context.Context, id string) (*kernel.Listener, error) {
	var l kernel.Listener
	var active int
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,owner_user_id,source_user_id,event_name,target_action_id,active,created_at
		 FROM listeners WHERE id=?`, id,
	).Scan(&l.ID, &l.OwnerUserID, &l.SourceUserID, &l.EventName,
		&l.TargetActionID, &active, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("listener not found")
	}
	if err != nil {
		return nil, dbErr(err, "read listener")
	}
	l.Active = active != 0
	l.CreatedAt = strToTime(createdAt)
	return &l, nil
}

func (s *DB) UpdateListener(ctx context.Context, l *kernel.Listener) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE listeners SET active=? WHERE id=?`, boolInt(l.Active), l.ID)
	return dbErr(err, "update listener")
}

func (s *DB) ListListeners(ctx context.Context, sourceUserID, eventName string) ([]*kernel.Listener, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,owner_user_id,source_user_id,event_name,target_action_id,active,created_at
		 FROM listeners WHERE source_user_id=? AND event_name=? AND active=1`,
		sourceUserID, eventName,
	)
	if err != nil {
		return nil, dbErr(err, "list listeners")
	}
	defer rows.Close()

	var out []*kernel.Listener
	for rows.Next() {
		var l kernel.Listener
		var active int
		var createdAt string
		if err := rows.Scan(&l.ID, &l.OwnerUserID, &l.SourceUserID, &l.EventName,
			&l.TargetActionID, &active, &createdAt); err != nil {
			return nil, dbErr(err, "scan listener")
		}
		l.Active = active != 0
		l.CreatedAt = strToTime(createdAt)
		out = append(out, &l)
	}
	return out, rows.Err()
}

func (s *DB) ListListenersByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*kernel.Listener, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,owner_user_id,source_user_id,event_name,target_action_id,active,created_at
		 FROM listeners WHERE owner_user_id=? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		ownerID, limit, offset,
	)
	if err != nil {
		return nil, dbErr(err, "list listeners by owner")
	}
	defer rows.Close()
	var out []*kernel.Listener
	for rows.Next() {
		var l kernel.Listener
		var active int
		var createdAt string
		if err := rows.Scan(&l.ID, &l.OwnerUserID, &l.SourceUserID, &l.EventName,
			&l.TargetActionID, &active, &createdAt); err != nil {
			return nil, dbErr(err, "scan listener")
		}
		l.Active = active != 0
		l.CreatedAt = strToTime(createdAt)
		out = append(out, &l)
	}
	return out, rows.Err()
}

func (s *DB) CreateEvent(ctx context.Context, e *kernel.Event) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (id,listener_id,args_json,causing_trace_id,created_at)
		 VALUES (?,?,?,?,?)`,
		e.ID, e.ListenerID, e.ArgsJSON, nullStr(e.CausingTraceID), timeToStr(e.CreatedAt),
	)
	return dbErr(err, "create event")
}

func (s *DB) ReadEvent(ctx context.Context, id string) (*kernel.Event, error) {
	var e kernel.Event
	var causingTraceID, consumedAt, txID *string
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,listener_id,args_json,causing_trace_id,consumed_at,tx_id,created_at
		 FROM events WHERE id=?`, id,
	).Scan(&e.ID, &e.ListenerID, &e.ArgsJSON, &causingTraceID, &consumedAt, &txID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("event not found")
	}
	if err != nil {
		return nil, dbErr(err, "read event")
	}
	if causingTraceID != nil {
		e.CausingTraceID = *causingTraceID
	}
	e.ConsumedAt = strToNullTime(consumedAt)
	e.TxID = txID
	e.CreatedAt = strToTime(createdAt)
	return &e, nil
}

func (s *DB) ListPendingEvents(ctx context.Context, listenerID string) ([]*kernel.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,listener_id,args_json,causing_trace_id,created_at
		 FROM events WHERE listener_id=? AND consumed_at IS NULL ORDER BY created_at`, listenerID)
	if err != nil {
		return nil, dbErr(err, "list pending events")
	}
	defer rows.Close()
	var out []*kernel.Event
	for rows.Next() {
		var e kernel.Event
		var causingTraceID *string
		var createdAt string
		if err := rows.Scan(&e.ID, &e.ListenerID, &e.ArgsJSON, &causingTraceID, &createdAt); err != nil {
			return nil, dbErr(err, "scan event")
		}
		if causingTraceID != nil {
			e.CausingTraceID = *causingTraceID
		}
		e.CreatedAt = strToTime(createdAt)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *DB) LockEvent(ctx context.Context, eventID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE events SET consumed_at=datetime('now') WHERE id=? AND consumed_at IS NULL`, eventID)
	if err != nil {
		return dbErr(err, "lock event")
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return kernel.ErrInvalidState.Wrap("event already consumed or in-flight")
	}
	return nil
}

func (s *DB) SettleEvent(ctx context.Context, eventID, txID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE events SET tx_id=? WHERE id=?`, txID, eventID)
	return dbErr(err, "settle event")
}

func (s *DB) UnlockEvent(ctx context.Context, eventID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE events SET consumed_at=NULL WHERE id=? AND tx_id IS NULL`, eventID)
	return dbErr(err, "unlock event")
}

func (s *DB) PurgeListenerEvents(ctx context.Context, listenerID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM events WHERE listener_id=? AND consumed_at IS NULL`, listenerID)
	return dbErr(err, "purge listener events")
}

func (s *DB) DeleteListenerWithEvents(ctx context.Context, listenerID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin delete listener")
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE listeners SET active=0 WHERE id=?`, listenerID); err != nil {
		return dbErr(err, "delete listener: deactivate")
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE listener_id=? AND consumed_at IS NULL`, listenerID); err != nil {
		return dbErr(err, "delete listener: purge events")
	}
	return dbErr(tx.Commit(), "delete listener: commit")
}

func (s *DB) ResetInFlightEvents(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE events SET consumed_at=NULL WHERE consumed_at IS NOT NULL AND tx_id IS NULL`)
	return dbErr(err, "reset in-flight events")
}

// nullStr converts an empty string to nil for nullable TEXT columns.
func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---- Traces (by process) ----

func (s *DB) ListTraces(ctx context.Context, processID string) ([]*kernel.Trace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,process_id,parent_trace_id,caused_by_trace_id,cost,latency_ms,created_at FROM traces WHERE process_id=?`, processID)
	if err != nil {
		return nil, dbErr(err, "list traces")
	}
	defer rows.Close()
	var out []*kernel.Trace
	for rows.Next() {
		var t kernel.Trace
		var createdAt string
		var causedBy sql.NullString
		if err := rows.Scan(&t.ID, &t.ProcessID, &t.ParentTraceID, &causedBy, &t.Cost, &t.LatencyMS, &createdAt); err != nil {
			return nil, dbErr(err, "scan trace")
		}
		t.CreatedAt = strToTime(createdAt)
		if causedBy.Valid {
			t.CausedByTraceID = &causedBy.String
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, dbErr(err, "begin consume auth code")
	}
	defer tx.Rollback()

	var ac kernel.AuthCode
	var expiresAt string
	var used int
	err = tx.QueryRowContext(ctx,
		`SELECT code,user_id,code_challenge,redirect_uri,expires_at,used FROM auth_codes WHERE code=?`, code,
	).Scan(&ac.Code, &ac.UserID, &ac.CodeChallenge, &ac.RedirectURI, &expiresAt, &used)
	if errors.Is(err, sql.ErrNoRows) || used != 0 {
		return nil, kernel.ErrUnauthenticated.Wrap("invalid or used auth code")
	}
	if err != nil {
		return nil, dbErr(err, "read auth code")
	}
	ac.ExpiresAt = strToTime(expiresAt)
	if ac.ExpiresAt.Before(time.Now()) {
		return nil, kernel.ErrUnauthenticated.Wrap("auth code expired")
	}

	_, err = tx.ExecContext(ctx, `UPDATE auth_codes SET used=1 WHERE code=?`, code)
	if err != nil {
		return nil, dbErr(err, "mark auth code used")
	}
	return &ac, dbErr(tx.Commit(), "consume auth code commit")
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, dbErr(err, "begin rotate refresh token")
	}
	defer tx.Rollback()

	var userID, expiresAt string
	var revoked int
	err = tx.QueryRowContext(ctx,
		`SELECT user_id,expires_at,revoked FROM refresh_tokens WHERE token=?`, oldToken,
	).Scan(&userID, &expiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) || revoked != 0 {
		return nil, kernel.ErrUnauthenticated.Wrap("invalid or revoked refresh token")
	}
	if err != nil {
		return nil, dbErr(err, "read refresh token")
	}
	if strToTime(expiresAt).Before(time.Now()) {
		return nil, kernel.ErrUnauthenticated.Wrap("refresh token expired")
	}

	// Revoke the old token.
	if _, err = tx.ExecContext(ctx, `UPDATE refresh_tokens SET revoked=1 WHERE token=?`, oldToken); err != nil {
		return nil, dbErr(err, "revoke old refresh token")
	}

	// Issue a new one with a fresh random token.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, kernel.ErrInternal.Wrap("failed to generate refresh token")
	}
	now := time.Now().UTC()
	newTok := &kernel.RefreshToken{
		Token:     base64.RawURLEncoding.EncodeToString(raw),
		UserID:    userID,
		ExpiresAt: now.Add(30 * 24 * time.Hour),
		CreatedAt: now,
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO refresh_tokens (token,user_id,expires_at,revoked,created_at) VALUES (?,?,?,0,?)`,
		newTok.Token, newTok.UserID, timeToStr(newTok.ExpiresAt), timeToStr(newTok.CreatedAt),
	); err != nil {
		return nil, dbErr(err, "insert new refresh token")
	}
	return newTok, dbErr(tx.Commit(), "rotate refresh token commit")
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

func (s *DB) InitSuperuser(ctx context.Context, u *kernel.User, configKey, configValue string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin init superuser")
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO users (id,handle,email,password_hash,available,locked,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		u.ID, u.Handle, u.Email, u.PasswordHash,
		u.Available, u.Locked, timeToStr(u.CreatedAt), timeToStr(u.UpdatedAt),
	)
	if err != nil {
		return dbErr(err, "init superuser: insert user")
	}

	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO config (key,value) VALUES (?,?)`, configKey, configValue)
	if err != nil {
		return dbErr(err, "init superuser: set config")
	}

	return dbErr(tx.Commit(), "init superuser: commit")
}

func (s *DB) InitFirstBoot(ctx context.Context, u *kernel.User, configs map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin init first boot")
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO users (id,handle,email,password_hash,available,locked,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		u.ID, u.Handle, u.Email, u.PasswordHash,
		u.Available, u.Locked, timeToStr(u.CreatedAt), timeToStr(u.UpdatedAt),
	)
	if err != nil {
		return dbErr(err, "init first boot: insert user")
	}

	for k, v := range configs {
		if _, err = tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO config (key,value) VALUES (?,?)`, k, v); err != nil {
			return dbErr(err, "init first boot: set config "+k)
		}
	}

	return dbErr(tx.Commit(), "init first boot: commit")
}

// ---- Deposits ----

func (s *DB) CreateDeposit(ctx context.Context, d *kernel.Deposit) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin deposit")
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO deposits (id,operator_user_id,target_user_id,amount,reason,created_at)
		 VALUES (?,?,?,?,?,?)`,
		d.ID, d.OperatorUserID, d.TargetUserID, d.Amount, d.Reason, timeToStr(d.CreatedAt),
	)
	if err != nil {
		return dbErr(err, "insert deposit")
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE users SET available=available+? WHERE id=?`, d.Amount, d.TargetUserID)
	if err != nil {
		return dbErr(err, "deposit: update user balance")
	}

	return dbErr(tx.Commit(), "deposit: commit")
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

// ---- Receipts ----

func (s *DB) CreateReceipt(ctx context.Context, r *kernel.Receipt) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO receipts (id,issuer_user_id,tx_id,trace_id,action_id,args_hash,reply_hash,status,gross,net,fee,reason,created_at,signature)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.IssuerUserID, r.TxID, r.TraceID, r.ActionID,
		r.ArgsHash, r.ReplyHash, string(r.Status),
		r.Gross, r.Net, r.Fee, r.Reason,
		timeToStr(r.CreatedAt), r.Signature,
	)
	return dbErr(err, "create receipt")
}

func (s *DB) ReadReceiptByTxID(ctx context.Context, txID string) (*kernel.Receipt, error) {
	var r kernel.Receipt
	var status, createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,issuer_user_id,tx_id,trace_id,action_id,args_hash,reply_hash,status,gross,net,fee,reason,created_at,signature
		 FROM receipts WHERE tx_id=?`, txID,
	).Scan(&r.ID, &r.IssuerUserID, &r.TxID, &r.TraceID, &r.ActionID,
		&r.ArgsHash, &r.ReplyHash, &status,
		&r.Gross, &r.Net, &r.Fee, &r.Reason, &createdAt, &r.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("receipt not found")
	}
	if err != nil {
		return nil, dbErr(err, "read receipt by tx_id")
	}
	r.Status = kernel.TxStatus(status)
	r.CreatedAt = strToTime(createdAt)
	return &r, nil
}

func (s *DB) ReadReceipt(ctx context.Context, id string) (*kernel.Receipt, error) {
	var r kernel.Receipt
	var status, createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,issuer_user_id,tx_id,trace_id,action_id,args_hash,reply_hash,status,gross,net,fee,reason,created_at,signature
		 FROM receipts WHERE id=?`, id,
	).Scan(&r.ID, &r.IssuerUserID, &r.TxID, &r.TraceID, &r.ActionID,
		&r.ArgsHash, &r.ReplyHash, &status,
		&r.Gross, &r.Net, &r.Fee, &r.Reason, &createdAt, &r.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("receipt not found")
	}
	if err != nil {
		return nil, dbErr(err, "read receipt")
	}
	r.Status = kernel.TxStatus(status)
	r.CreatedAt = strToTime(createdAt)
	return &r, nil
}

// ---- Ratings ----

func (s *DB) CreateRating(ctx context.Context, r *kernel.Rating) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ratings (id,rated_tx_id,rated_receipt_id,rater_user_id,rating,created_at,signature)
		 VALUES (?,?,?,?,?,?,?)`,
		r.ID, r.RatedTxID, r.RatedReceiptID, r.RaterUserID, r.Rating,
		timeToStr(r.CreatedAt), r.Signature,
	)
	return dbErr(err, "create rating")
}

func (s *DB) ReadRatingByTxID(ctx context.Context, txID string) (*kernel.Rating, error) {
	var r kernel.Rating
	var ratedReceiptID *string
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,rated_tx_id,rated_receipt_id,rater_user_id,rating,created_at,signature
		 FROM ratings WHERE rated_tx_id=?`, txID,
	).Scan(&r.ID, &r.RatedTxID, &ratedReceiptID, &r.RaterUserID, &r.Rating, &createdAt, &r.Signature)
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

func (s *DB) CreateIdempotencyRecord(ctx context.Context, r *kernel.IdempotencyRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO idempotency_records (id,idempotency_key,counterparty_user_id,receipt_id,created_at,expires_at)
		 VALUES (?,?,?,?,?,?)`,
		r.ID, r.IdempotencyKey, r.CounterpartyUserID, r.ReceiptID,
		timeToStr(r.CreatedAt), timeToStr(r.ExpiresAt),
	)
	return dbErr(err, "create idempotency record")
}

func (s *DB) ReadIdempotencyRecord(ctx context.Context, key, counterpartyUserID string) (*kernel.IdempotencyRecord, error) {
	var r kernel.IdempotencyRecord
	var receiptID *string
	var createdAt, expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,idempotency_key,counterparty_user_id,receipt_id,created_at,expires_at
		 FROM idempotency_records
		 WHERE idempotency_key=? AND counterparty_user_id=? AND expires_at > datetime('now')`,
		key, counterpartyUserID,
	).Scan(&r.ID, &r.IdempotencyKey, &r.CounterpartyUserID, &receiptID, &createdAt, &expiresAt)
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
