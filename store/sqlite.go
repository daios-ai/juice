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

const userCols = `id,handle,email,password_hash,available,locked,suspended_at,denied_at,public_key,remote_base_url,created_at,updated_at`

func (s *DB) CreateUser(ctx context.Context, u *kernel.User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id,handle,email,password_hash,available,locked,suspended_at,denied_at,public_key,remote_base_url,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Handle, u.Email, u.PasswordHash, u.Available, u.Locked,
		nullTimeToStr(u.SuspendedAt), nullTimeToStr(u.DeniedAt),
		nullStr(u.PublicKey), nullStr(u.RemoteBaseURL),
		timeToStr(u.CreatedAt), timeToStr(u.UpdatedAt),
	)
	if err != nil {
		return dbErr(err, "create user")
	}
	return nil
}

func (s *DB) ReadUser(ctx context.Context, id string) (*kernel.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (s *DB) ReadUserByHandle(ctx context.Context, handle string) (*kernel.User, error) {
	// Canonicalize at the single read funnel so "x" and "@x" resolve to the same row,
	// regardless of caller (login, federation, CLI, HTTP all reach here).
	handle = kernel.NormalizeHandle(handle)
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE handle=?`, handle))
}

func (s *DB) ReadUserByPublicKey(ctx context.Context, publicKey string) (*kernel.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE public_key=?`, publicKey))
}

func (s *DB) UpdateRemoteBaseURL(ctx context.Context, userID, baseURL string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET remote_base_url=?, updated_at=? WHERE id=?`,
		nullStr(baseURL), timeToStr(time.Now().UTC()), userID,
	)
	return dbErr(err, "update remote base url")
}

func (s *DB) ReadRemoteKernelByBaseURL(ctx context.Context, baseURL string) (*kernel.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE remote_base_url=? AND remote_base_url!=''`, baseURL))
}

func (s *DB) UpdateRemoteProxySourceURLs(ctx context.Context, ownerUserID, oldBase, newBase string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE actions SET source=REPLACE(source,?,?) WHERE owner_user_id=? AND kind='remote_proxy'`,
		oldBase, newBase, ownerUserID,
	)
	return dbErr(err, "update remote proxy source urls")
}

func (s *DB) DenyUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET denied_at=datetime('now') WHERE id=?`, id)
	return dbErr(err, "deny user")
}

func (s *DB) UndenyUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET denied_at=NULL WHERE id=?`, id)
	return dbErr(err, "undeny user")
}

func (s *DB) DeactivateActionsOwnedBy(ctx context.Context, ownerUserID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE actions SET active=FALSE WHERE owner_user_id=? AND deleted_at IS NULL`, ownerUserID)
	return dbErr(err, "deactivate actions by owner")
}

func (s *DB) DenyPeerCascade(ctx context.Context, userID string) error {
	return s.withTx(ctx, "deny peer cascade", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET denied_at=datetime('now') WHERE id=?`, userID); err != nil {
			return dbErr(err, "deny user")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET active=FALSE WHERE owner_user_id=? AND deleted_at IS NULL`, userID); err != nil {
			return dbErr(err, "deactivate actions")
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, price, parent_trace_id FROM steps WHERE required_caller_user_id=? AND status='waiting'`, userID)
		if err != nil {
			return dbErr(err, "query steps")
		}
		type stepRef struct {
			id            string
			price         int64
			parentTraceID *string
		}
		var steps []stepRef
		for rows.Next() {
			var sr stepRef
			if err := rows.Scan(&sr.id, &sr.price, &sr.parentTraceID); err != nil {
				rows.Close()
				return dbErr(err, "scan step")
			}
			steps = append(steps, sr)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return dbErr(err, "steps rows err")
		}
		for _, sr := range steps {
			if _, err := tx.ExecContext(ctx, `UPDATE steps SET status='cancelled' WHERE id=?`, sr.id); err != nil {
				return dbErr(err, "cancel step")
			}
			if sr.parentTraceID != nil && sr.price > 0 {
				if _, err := tx.ExecContext(ctx,
					`UPDATE traces SET available=available+?, locked=locked-? WHERE id=?`,
					sr.price, sr.price, *sr.parentTraceID); err != nil {
					return dbErr(err, "refund trace")
				}
			}
		}
		return nil
	})
}

func (s *DB) CancelAndRefundStepsForCaller(ctx context.Context, callerUserID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "cancel steps for caller: begin tx")
	}
	defer tx.Rollback() //nolint

	rows, err := tx.QueryContext(ctx,
		`SELECT id, price, parent_trace_id FROM steps WHERE required_caller_user_id=? AND status='waiting'`,
		callerUserID)
	if err != nil {
		return dbErr(err, "cancel steps for caller: query")
	}
	type stepRef struct {
		id            string
		price         int64
		parentTraceID *string
	}
	var steps []stepRef
	for rows.Next() {
		var sr stepRef
		if err := rows.Scan(&sr.id, &sr.price, &sr.parentTraceID); err != nil {
			rows.Close()
			return dbErr(err, "cancel steps for caller: scan")
		}
		steps = append(steps, sr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return dbErr(err, "cancel steps for caller: rows err")
	}

	for _, sr := range steps {
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET status='cancelled' WHERE id=?`, sr.id); err != nil {
			return dbErr(err, "cancel steps for caller: cancel step")
		}
		if sr.parentTraceID != nil && sr.price > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE traces SET available=available+?, locked=locked-? WHERE id=?`,
				sr.price, sr.price, *sr.parentTraceID); err != nil {
				return dbErr(err, "cancel steps for caller: refund trace")
			}
		}
	}
	return dbErr(tx.Commit(), "cancel steps for caller: commit")
}

func (s *DB) UpsertStatTag(ctx context.Context, tag *kernel.StatTag) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO stat_tags (action_id, key, value, source, updated_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(action_id,key,source) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		tag.ActionID, tag.Key, tag.Value, tag.Source, timeToStr(tag.UpdatedAt))
	return dbErr(err, "upsert stat tag")
}

func (s *DB) ListStatTagsByAction(ctx context.Context, actionID string) ([]*kernel.StatTag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT action_id, key, value, source, updated_at FROM stat_tags WHERE action_id=?`, actionID)
	if err != nil {
		return nil, dbErr(err, "list stat tags")
	}
	return queryList(rows, "list stat tags", func(scan func(...any) error) (*kernel.StatTag, error) {
		var t kernel.StatTag
		var updatedAt string
		if err := scan(&t.ActionID, &t.Key, &t.Value, &t.Source, &updatedAt); err != nil {
			return nil, err
		}
		t.UpdatedAt = strToTime(updatedAt)
		return &t, nil
	})
}

func (s *DB) ListStatsByOwner(ctx context.Context, ownerUserID string) ([]*kernel.Stats, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.action_id, s.uses, s.successes, s.failures, s.rating_count,
		        s.latency_estimate, s.rating_estimate, s.last_used_at
		 FROM action_stats s
		 JOIN actions a ON a.id = s.action_id
		 WHERE a.owner_user_id = ? AND s.uses > 0 AND a.deleted_at IS NULL`,
		ownerUserID)
	if err != nil {
		return nil, dbErr(err, "list stats by owner")
	}
	return queryList(rows, "list stats by owner", func(scan func(...any) error) (*kernel.Stats, error) {
		var st kernel.Stats
		var lastUsedAt string
		if err := scan(&st.ActionID, &st.Uses, &st.Successes, &st.Failures,
			&st.RatingCount, &st.LatencyEstimate, &st.RatingEstimate, &lastUsedAt); err != nil {
			return nil, err
		}
		st.LastUsedAt = strToTime(lastUsedAt)
		return &st, nil
	})
}

func (s *DB) CreateProxyUser(ctx context.Context, u *kernel.User) error {
	// Use handle+"@remote" as a unique placeholder email for proxy users.
	proxyEmail := u.Handle + "@remote"
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id,handle,email,password_hash,available,locked,suspended_at,denied_at,public_key,remote_base_url,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Handle, proxyEmail, "", 0, 0,
		nil, nil,
		nullStr(u.PublicKey), nullStr(u.RemoteBaseURL),
		timeToStr(u.CreatedAt), timeToStr(u.UpdatedAt),
	)
	return dbErr(err, "create proxy user")
}

func scanUserFn(scan func(...any) error) (*kernel.User, error) {
	var u kernel.User
	var createdAt, updatedAt string
	var suspendedAt, deniedAt, publicKey, remoteBaseURL *string
	if err := scan(&u.ID, &u.Handle, &u.Email, &u.PasswordHash,
		&u.Available, &u.Locked, &suspendedAt, &deniedAt, &publicKey, &remoteBaseURL, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	u.SuspendedAt = strToNullTime(suspendedAt)
	u.DeniedAt = strToNullTime(deniedAt)
	u.PublicKey = strVal(publicKey)
	u.RemoteBaseURL = strVal(remoteBaseURL)
	u.CreatedAt = strToTime(createdAt)
	u.UpdatedAt = strToTime(updatedAt)
	return &u, nil
}

func (s *DB) scanUser(row *sql.Row) (*kernel.User, error) {
	u, err := scanUserFn(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("user not found")
	}
	if err != nil {
		return nil, dbErr(err, "read user")
	}
	return u, nil
}

func (s *DB) ListUsers(ctx context.Context, limit, offset int) ([]*kernel.User, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userCols+` FROM users ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list users")
	}
	return queryList(rows, "list users", scanUserFn)
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

func (s *DB) UpdateUser(ctx context.Context, u *kernel.User) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET email=?, password_hash=?, updated_at=? WHERE id=?`,
		u.Email, u.PasswordHash, timeToStr(u.UpdatedAt), u.ID,
	)
	return dbErr(err, "update user")
}

// ---- Actions ----

func (s *DB) CreateAction(ctx context.Context, a *kernel.Action) error {
	inJSON, _ := json.Marshal(a.InputSchema)
	outJSON, _ := json.Marshal(a.OutputSchema)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO actions
		 (id,owner_user_id,name,kind,active,public,price,description,input_schema,output_schema,source,artifact_hash,wasm_artifact,remote_action_id,auth_json,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerUserID, a.Name, string(a.Kind), boolInt(a.Active), boolInt(a.Public), a.Price,
		a.Description, string(inJSON), string(outJSON), a.Source, a.ArtifactHash, a.WasmArtifact, a.RemoteActionID,
		a.AuthJSON, timeToStr(a.CreatedAt), timeToStr(a.UpdatedAt),
	)
	return dbErr(err, "create action")
}

// actionCols is the canonical column list for action SELECT statements.
// Must stay in sync with scanAction/scanActionFn/finishAction.
const actionCols = `a.id,a.owner_user_id,COALESCE(u.handle,''),a.name,a.kind,a.active,a.public,a.price,a.description,a.input_schema,a.output_schema,a.source,a.artifact_hash,a.wasm_artifact,a.remote_action_id,a.auth_json,a.created_at,a.updated_at,a.deleted_at`

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
		 source=?,artifact_hash=?,wasm_artifact=?,auth_json=?,updated_at=? WHERE id=?`,
		string(a.Kind), boolInt(a.Active), boolInt(a.Public), a.Price, a.Description,
		string(inJSON), string(outJSON), a.Source, a.ArtifactHash, a.WasmArtifact,
		a.AuthJSON, timeToStr(a.UpdatedAt), a.ID,
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
			`INSERT INTO action_stats (action_id,uses,successes,failures,rating_count,latency_estimate,rating_estimate,last_used_at)
			 VALUES (?,0,0,0,0,0,0,?)
			 ON CONFLICT(action_id) DO UPDATE SET
			   uses=0,successes=0,failures=0,rating_count=0,
			   latency_estimate=0,rating_estimate=0,last_used_at=excluded.last_used_at`,
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
	return queryList(rows, "list public actions", scanActionFn)
}

func (s *DB) ListActionsByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*kernel.Action, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionCols+` FROM actions a LEFT JOIN users u ON u.id=a.owner_user_id
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
		`SELECT `+actionCols+` FROM actions a LEFT JOIN users u ON u.id=a.owner_user_id WHERE a.deleted_at IS NULL ORDER BY a.created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, dbErr(err, "list all actions")
	}
	return queryList(rows, "list all actions", scanActionFn)
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
	return queryList(rows, "list actions by openapi spec", scanActionFn)
}

func scanActionFn(scan func(...any) error) (*kernel.Action, error) {
	var a kernel.Action
	var kind, inJSON, outJSON, createdAt, updatedAt string
	var deletedAt sql.NullString
	var active, public int
	if err := scan(&a.ID, &a.OwnerUserID, &a.OwnerHandle, &a.Name, &kind, &active, &public, &a.Price,
		&a.Description, &inJSON, &outJSON, &a.Source, &a.ArtifactHash, &a.WasmArtifact, &a.RemoteActionID,
		&a.AuthJSON, &createdAt, &updatedAt, &deletedAt); err != nil {
		return nil, err
	}
	return finishAction(&a, kind, active, public, inJSON, outJSON, createdAt, updatedAt, deletedAt)
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

// BeginRun atomically debits price from owner.available→locked, creates the process
// with available=0/locked=price, and creates the root trace with available=price.
func (s *DB) BeginRun(ctx context.Context, p *kernel.Process, t *kernel.Trace, ownerID string, price int64) error {
	return s.withTx(ctx, "begin run", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET available=available-?, locked=locked+? WHERE id=? AND available>=?`,
			price, price, ownerID, price,
		)
		if err != nil {
			return dbErr(err, "begin run: deduct user")
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return kernel.ErrInsufficientFunds.Wrap("insufficient user balance")
		}
		if _, err = tx.ExecContext(ctx,
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
		`INSERT INTO traces (id,process_id,parent_trace_id,action_owner_id,action_id,caller_user_id,available,locked,idempotency_key,dispatch_json,created_at)
		 VALUES (?,?,?,?,?,?,?,0,?,?,?)`,
		t.ID, t.ProcessID, parentTraceID, t.ActionOwnerID, t.ActionID, t.CallerUserID, price, t.IdempotencyKey, t.DispatchJSON, timeToStr(t.CreatedAt),
	)
	return dbErr(err, "insert trace")
}

// BeginSubcall atomically deducts price from parent_trace.available into parent_trace.locked
// and creates the child trace with available=price.
func (s *DB) BeginSubcall(ctx context.Context, parentTraceID string, t *kernel.Trace, price int64) error {
	return s.withTx(ctx, "begin subcall", func(tx *sql.Tx) error {
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
		return insertTraceTx(ctx, tx, t, &parentTraceID, price)
	})
}

// BeginStepCall atomically moves step.price from the step's parent_trace.locked back into
// parent_trace.available (consuming the park), claims the step waiting→running, and creates
// a new trace with available=step.price funded from the released lock.
func (s *DB) BeginStepCall(ctx context.Context, stepID string, t *kernel.Trace) error {
	return s.withTx(ctx, "begin step call", func(tx *sql.Tx) error {
		// Read step to get price and parent_trace_id.
		var price int64
		var parentTraceID *string
		err := tx.QueryRowContext(ctx,
			`SELECT price, parent_trace_id FROM steps WHERE id=? AND status='waiting'`,
			stepID,
		).Scan(&price, &parentTraceID)
		if errors.Is(err, sql.ErrNoRows) {
			return kernel.ErrInvalidState.Wrap("step not waiting")
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
			return kernel.ErrInvalidState.Wrap("step already claimed")
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
		                       args_hash,reply_hash,status,gross,net,fee,charge,reason,started_at,created_at,signature)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.ID, receipt.IssuerUserID, receipt.TxID, receipt.TraceID, receipt.ActionID,
		receipt.CallerUserID, receipt.ProcessID,
		receipt.ArgsHash, receipt.ReplyHash, string(receipt.Status),
		receipt.Gross, receipt.Net, receipt.Fee, receipt.Charge, receipt.Reason,
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
			`UPDATE users SET available=available+?, locked=locked-? WHERE id=?`,
			available, available, ownerID); err != nil {
			return dbErr(err, "close process: return available to owner")
		}
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE processes SET status='closed', available=0, locked=0, ended_at=datetime('now') WHERE id=?`,
		processID)
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
				`UPDATE users SET locked=locked-? WHERE id=?`, taxable, ktx.OwnerUserID); err != nil {
				return dbErr(err, "commit call: debit owner locked")
			}
		}
		if net > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET available=available+? WHERE id=?`, net, targetUserID); err != nil {
				return dbErr(err, "commit call: credit target")
			}
		}
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
func (s *DB) CommitFailedCall(ctx context.Context, ktx *kernel.Transaction, buildReceipt func(refund int64) (*kernel.Receipt, error), traceID, callerWalletID, callerWalletKind string, gross int64, stats *kernel.Stats, idempotencyRecordID, errorCode, stepID string) error {
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
		errResult, _ := json.Marshal(map[string]string{"error": ktx.Reason, "code": errorCode})
		if err := s.finalizeTx(ctx, tx, ktx, receipt, stats, idempotencyRecordID, string(errResult), stepID, "commit failed call"); err != nil {
			return err
		}
		return s.closeProcessTx(ctx, tx, ktx.ProcessID)
	})
}

// CommitRemoteSettlement settles a remote-proxy call with economics distinct from local calls:
// the gross (= mp + maxduty) was locked; charge flows to the proxy, duty to @sys, and the
// remainder (refund = gross−charge−duty) is returned to the caller wallet.
// Unlike CommitCall, taxable = charge+duty (not gross), so the refund must be explicit.
func (s *DB) CommitRemoteSettlement(ctx context.Context, ktx *kernel.Transaction, receipt *kernel.Receipt, traceID, callerWalletID, callerWalletKind, proxyUserID, feeRecipientID string, charge, duty int64, stats *kernel.Stats, idempotencyRecordID, stepID string) error {
	return s.withTx(ctx, "commit remote settlement", func(tx *sql.Tx) error {
		q := ktx.Gross // full locked amount (mp + maxduty)
		taxable := charge + duty
		refund := q - charge - duty
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
				`UPDATE users SET locked=locked-? WHERE id=?`, taxable, ktx.OwnerUserID); err != nil {
				return dbErr(err, "commit remote settlement: debit owner locked")
			}
		}
		// Pay charge to proxy user.
		if charge > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET available=available+? WHERE id=?`, charge, proxyUserID); err != nil {
				return dbErr(err, "commit remote settlement: credit proxy user")
			}
		}
		// Pay duty to fee recipient.
		if duty > 0 {
			if feeRecipientID == "" {
				return fmt.Errorf("commit remote settlement: duty %d > 0 but feeRecipientID is empty", duty)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET available=available+? WHERE id=?`, duty, feeRecipientID); err != nil {
				return dbErr(err, "commit remote settlement: credit fee recipient")
			}
		}
		if err := s.finalizeTx(ctx, tx, ktx, receipt, stats, idempotencyRecordID, rawJSONStr(ktx.ReplyJSON), stepID, "commit remote settlement"); err != nil {
			return err
		}
		return s.closeProcessTx(ctx, tx, ktx.ProcessID)
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
				`UPDATE users SET available=available+?, locked=locked-? WHERE id=?`,
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
				`UPDATE users SET available=available+?, locked=locked-? WHERE id=?`,
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

const traceCols = `id,process_id,parent_trace_id,action_owner_id,action_id,caller_user_id,available,locked,idempotency_key,dispatch_json,created_at`

func scanTrace(t *kernel.Trace, scanFn func(...any) error) error {
	var createdAt string
	var parentID, idempotencyKey, dispatchJSON sql.NullString
	err := scanFn(&t.ID, &t.ProcessID, &parentID, &t.ActionOwnerID, &t.ActionID, &t.CallerUserID,
		&t.Available, &t.Locked, &idempotencyKey, &dispatchJSON, &createdAt)
	if err != nil {
		return err
	}
	t.CreatedAt = strToTime(createdAt)
	if parentID.Valid {
		t.ParentTraceID = &parentID.String
	}
	if idempotencyKey.Valid {
		t.IdempotencyKey = &idempotencyKey.String
	}
	if dispatchJSON.Valid {
		t.DispatchJSON = &dispatchJSON.String
	}
	return nil
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
	return queryList(rows, "list transactions", func(scan func(...any) error) (*kernel.Transaction, error) {
		tx, err := scanTx(scan)
		if err != nil {
			return nil, err
		}
		return &tx, nil
	})
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
	return queryList(rows, "list all transactions", func(scan func(...any) error) (*kernel.Transaction, error) {
		tx, err := scanTx(scan)
		if err != nil {
			return nil, err
		}
		return &tx, nil
	})
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
			`INSERT INTO steps (id,parent_trace_id,required_caller_user_id,action_id,
			                    partial_args,price,status,created_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			step.ID, step.ParentTraceID, step.RequiredCallerUserID,
			step.ActionID, rawJSONStr(step.PartialArgs),
			step.Price, string(step.Status), timeToStr(step.CreatedAt),
		)
		return dbErr(err, "create step: insert")
	})
}

const stepCols = `id,parent_trace_id,required_caller_user_id,action_id,partial_args,price,status,tx_id,completion_trace_id,created_at`

func scanStep(step *kernel.Step, scanFn func(...any) error) error {
	var parentTraceID, txID, completionTraceID *string
	var createdAt, partialArgs, status string
	if err := scanFn(&step.ID, &parentTraceID, &step.RequiredCallerUserID,
		&step.ActionID, &partialArgs, &step.Price, &status, &txID, &completionTraceID, &createdAt); err != nil {
		return err
	}
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

func (s *DB) ListSteps(ctx context.Context, callerUserID, processID, status string, isSuperuser bool) ([]*kernel.Step, error) {
	superInt := 0
	if isSuperuser {
		superInt = 1
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
		 ORDER BY created_at DESC`,
		callerUserID, callerUserID, superInt,
		processID, processID,
		status, status,
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
	return queryList(rows, "list orphan running steps", func(scan func(...any) error) (kernel.OrphanRunningStep, error) {
		var row kernel.OrphanRunningStep
		var hasSettled int
		err := scan(&row.StepID, &row.CompletionTraceID, &row.Price, &row.ParentTraceID,
			&row.TraceAvailable, &row.TraceLocked, &hasSettled)
		row.HasSettled = hasSettled == 1
		return row, err
	})
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
	return queryList(rows, "list orphan running steps for process", func(scan func(...any) error) (kernel.OrphanRunningStep, error) {
		var row kernel.OrphanRunningStep
		var hasSettled int
		err := scan(&row.StepID, &row.CompletionTraceID, &row.Price, &row.ParentTraceID,
			&row.TraceAvailable, &row.TraceLocked, &hasSettled)
		row.HasSettled = hasSettled == 1
		return row, err
	})
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
	return queryList(rows, "list traces", func(scan func(...any) error) (*kernel.Trace, error) {
		var t kernel.Trace
		if err := scanTrace(&t, scan); err != nil {
			return nil, err
		}
		return &t, nil
	})
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
	return queryList(rows, "list orphan traces", func(scan func(...any) error) (*kernel.Trace, error) {
		var t kernel.Trace
		if err := scanTrace(&t, scan); err != nil {
			return nil, err
		}
		return &t, nil
	})
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
	return queryList(rows, "list pending remote traces", func(scan func(...any) error) (*kernel.Trace, error) {
		var t kernel.Trace
		if err := scanTrace(&t, scan); err != nil {
			return nil, err
		}
		return &t, nil
	})
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
	return queryList(rows, "list direct unsettled children", func(scan func(...any) error) (*kernel.Trace, error) {
		var t kernel.Trace
		if err := scanTrace(&t, scan); err != nil {
			return nil, err
		}
		return &t, nil
	})
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
	return queryList(rows, "list unsettled traces for process", func(scan func(...any) error) (*kernel.Trace, error) {
		var t kernel.Trace
		if err := scanTrace(&t, scan); err != nil {
			return nil, err
		}
		return &t, nil
	})
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

// ---- Deposits / Withdrawals ----

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

func (s *DB) CreateWithdrawal(ctx context.Context, w *kernel.Withdrawal) error {
	return s.withTx(ctx, "withdrawal", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET available=available-? WHERE id=? AND available>=?`,
			w.Amount, w.TargetUserID, w.Amount,
		)
		if err != nil {
			return dbErr(err, "withdrawal: debit user")
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return kernel.ErrInsufficientFunds.Wrap("insufficient balance for withdrawal")
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO withdrawals (id,operator_user_id,target_user_id,amount,reason,created_at)
			 VALUES (?,?,?,?,?,?)`,
			w.ID, w.OperatorUserID, w.TargetUserID, w.Amount, w.Reason, timeToStr(w.CreatedAt),
		)
		return dbErr(err, "withdrawal: insert record")
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

// ---- Gossip / Discovered Kernels ----

func (s *DB) CreateOrUpdateDiscoveredKernel(ctx context.Context, k *kernel.DiscoveredKernel) error {
	statsJSON := "{}"
	if len(k.StatsJSON) > 0 {
		statsJSON = string(k.StatsJSON)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO discovered_kernels (public_key,introduced_by,handle,base_url,stats_json,first_seen,updated_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(public_key,introduced_by) DO UPDATE SET
		   handle=excluded.handle, base_url=excluded.base_url,
		   stats_json=excluded.stats_json, updated_at=excluded.updated_at`,
		k.PublicKey, k.IntroducedBy, k.Handle, k.BaseURL, statsJSON,
		timeToStr(k.FirstSeen), timeToStr(k.UpdatedAt),
	)
	return dbErr(err, "create or update discovered kernel")
}

func (s *DB) ListDiscoveredKernels(ctx context.Context) ([]*kernel.DiscoveredKernel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT public_key,introduced_by,handle,base_url,stats_json,first_seen,updated_at
		 FROM discovered_kernels ORDER BY updated_at DESC`)
	if err != nil {
		return nil, dbErr(err, "list discovered kernels")
	}
	return queryList(rows, "list discovered kernels", func(scan func(...any) error) (*kernel.DiscoveredKernel, error) {
		var k kernel.DiscoveredKernel
		var firstSeen, updatedAt, statsJSON string
		if err := scan(&k.PublicKey, &k.IntroducedBy, &k.Handle, &k.BaseURL, &statsJSON, &firstSeen, &updatedAt); err != nil {
			return nil, err
		}
		k.StatsJSON = json.RawMessage(statsJSON)
		k.FirstSeen = strToTime(firstSeen)
		k.UpdatedAt = strToTime(updatedAt)
		return &k, nil
	})
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
		        args_hash,reply_hash,status,gross,net,fee,charge,reason,started_at,created_at,signature`

func scanReceipt(row *sql.Row, op string) (*kernel.Receipt, error) {
	var r kernel.Receipt
	var status, startedAt, createdAt string
	err := row.Scan(&r.ID, &r.IssuerUserID, &r.TxID, &r.TraceID, &r.ActionID,
		&r.CallerUserID, &r.ProcessID,
		&r.ArgsHash, &r.ReplyHash, &status,
		&r.Gross, &r.Net, &r.Fee, &r.Charge, &r.Reason, &startedAt, &createdAt, &r.Signature)
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
			   rating_estimate  = rating_estimate + (? - rating_estimate) / (rating_count + 1)
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
	return queryList(rows, "list ratings", func(scan func(...any) error) (*kernel.Rating, error) {
		var r kernel.Rating
		var ratedReceiptID *string
		var createdAt string
		if err := scan(&r.ID, &r.RatedTxID, &ratedReceiptID, &r.RaterUserID, &r.Rating, &r.Note, &createdAt, &r.Signature); err != nil {
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
