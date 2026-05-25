// Package store provides the SQLite implementation of kernel.Store.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/daios/juice/kernel"
	_ "modernc.org/sqlite"
)

const driverName = "sqlite"

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

// migrate applies the embedded DDL idempotently.
func (s *DB) migrate() error {
	_, err := s.db.Exec(schema001)
	return err
}

// ---- time helpers ----

const timeLayout = time.RFC3339Nano

func timeToStr(t time.Time) string  { return t.UTC().Format(timeLayout) }
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

// ---- Users ----

func (s *DB) CreateUser(ctx context.Context, u *kernel.User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id,handle,email,password_hash,available,locked,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		u.ID, u.Handle, u.Email, u.PasswordHash, u.Available, u.Locked,
		timeToStr(u.CreatedAt), timeToStr(u.UpdatedAt),
	)
	if err != nil {
		return dbErr(err, "create user")
	}
	return nil
}

func (s *DB) ReadUser(ctx context.Context, id string) (*kernel.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id,handle,email,password_hash,available,locked,created_at,updated_at
		 FROM users WHERE id=?`, id))
}

func (s *DB) ReadUserByHandle(ctx context.Context, handle string) (*kernel.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`SELECT id,handle,email,password_hash,available,locked,created_at,updated_at
		 FROM users WHERE handle=?`, handle))
}

func (s *DB) scanUser(row *sql.Row) (*kernel.User, error) {
	var u kernel.User
	var createdAt, updatedAt string
	err := row.Scan(&u.ID, &u.Handle, &u.Email, &u.PasswordHash,
		&u.Available, &u.Locked, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("user not found")
	}
	if err != nil {
		return nil, dbErr(err, "read user")
	}
	u.CreatedAt = strToTime(createdAt)
	u.UpdatedAt = strToTime(updatedAt)
	return &u, nil
}

// ---- Actions ----

func (s *DB) CreateAction(ctx context.Context, a *kernel.Action) error {
	inJSON, _ := json.Marshal(a.InputSchema)
	outJSON, _ := json.Marshal(a.OutputSchema)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO actions
		 (id,owner_user_id,name,kind,active,price,description,input_schema,output_schema,source,artifact_hash,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerUserID, a.Name, string(a.Kind), boolInt(a.Active), a.Price,
		a.Description, string(inJSON), string(outJSON), a.Source, a.ArtifactHash,
		timeToStr(a.CreatedAt), timeToStr(a.UpdatedAt),
	)
	return dbErr(err, "create action")
}

func (s *DB) ReadAction(ctx context.Context, id string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT id,owner_user_id,name,kind,active,price,description,input_schema,output_schema,source,artifact_hash,created_at,updated_at
		 FROM actions WHERE id=?`, id))
}

func (s *DB) ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*kernel.Action, error) {
	return s.scanAction(s.db.QueryRowContext(ctx,
		`SELECT id,owner_user_id,name,kind,active,price,description,input_schema,output_schema,source,artifact_hash,created_at,updated_at
		 FROM actions WHERE owner_user_id=? AND name=?`, ownerID, name))
}

func (s *DB) UpdateAction(ctx context.Context, a *kernel.Action) error {
	inJSON, _ := json.Marshal(a.InputSchema)
	outJSON, _ := json.Marshal(a.OutputSchema)
	_, err := s.db.ExecContext(ctx,
		`UPDATE actions SET kind=?,active=?,price=?,description=?,input_schema=?,output_schema=?,
		 source=?,artifact_hash=?,updated_at=? WHERE id=?`,
		string(a.Kind), boolInt(a.Active), a.Price, a.Description,
		string(inJSON), string(outJSON), a.Source, a.ArtifactHash,
		timeToStr(a.UpdatedAt), a.ID,
	)
	return dbErr(err, "update action")
}

func (s *DB) DeleteAction(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM actions WHERE id=?`, id)
	return dbErr(err, "delete action")
}

func (s *DB) ListActions(ctx context.Context, activeOnly bool, limit, offset int) ([]*kernel.Action, error) {
	q := `SELECT id,owner_user_id,name,kind,active,price,description,input_schema,output_schema,source,artifact_hash,created_at,updated_at FROM actions`
	args := []any{}
	if activeOnly {
		q += ` WHERE active=1`
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

func (s *DB) scanAction(row *sql.Row) (*kernel.Action, error) {
	var a kernel.Action
	var kind, inJSON, outJSON, createdAt, updatedAt string
	var active int
	err := row.Scan(&a.ID, &a.OwnerUserID, &a.Name, &kind, &active, &a.Price,
		&a.Description, &inJSON, &outJSON, &a.Source, &a.ArtifactHash,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("action not found")
	}
	if err != nil {
		return nil, dbErr(err, "read action")
	}
	return finishAction(&a, kind, active, inJSON, outJSON, createdAt, updatedAt)
}

func (s *DB) scanActionRow(rows *sql.Rows) (*kernel.Action, error) {
	var a kernel.Action
	var kind, inJSON, outJSON, createdAt, updatedAt string
	var active int
	err := rows.Scan(&a.ID, &a.OwnerUserID, &a.Name, &kind, &active, &a.Price,
		&a.Description, &inJSON, &outJSON, &a.Source, &a.ArtifactHash,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, dbErr(err, "scan action")
	}
	return finishAction(&a, kind, active, inJSON, outJSON, createdAt, updatedAt)
}

func finishAction(a *kernel.Action, kind string, active int, inJSON, outJSON, createdAt, updatedAt string) (*kernel.Action, error) {
	a.Kind = kernel.ActionKind(kind)
	a.Active = active != 0
	a.CreatedAt = strToTime(createdAt)
	a.UpdatedAt = strToTime(updatedAt)
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

func (s *DB) SettleCall(ctx context.Context, processID, targetUserID, feeRecipientID string, net, fee int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbErr(err, "begin settle")
	}
	defer tx.Rollback()

	gross := net + fee

	// Debit process.locked and owner.locked.
	p, err := tx.QueryContext(ctx, `SELECT owner_user_id FROM processes WHERE id=?`, processID)
	if err != nil {
		return dbErr(err, "settle: read process owner")
	}
	var ownerID string
	if p.Next() {
		_ = p.Scan(&ownerID)
	}
	p.Close()

	if gross > 0 {
		_, err = tx.ExecContext(ctx,
			`UPDATE processes SET locked=locked-? WHERE id=?`, gross, processID)
		if err != nil {
			return dbErr(err, "settle: debit process locked")
		}
		if ownerID != "" {
			_, err = tx.ExecContext(ctx,
				`UPDATE users SET locked=locked-? WHERE id=?`, gross, ownerID)
			if err != nil {
				return dbErr(err, "settle: debit owner locked")
			}
		}
	}

	// Credit target.
	if net > 0 {
		_, err = tx.ExecContext(ctx,
			`UPDATE users SET available=available+? WHERE id=?`, net, targetUserID)
		if err != nil {
			return dbErr(err, "settle: credit target")
		}
	}

	// Credit fee recipient.
	if fee > 0 && feeRecipientID != "" {
		_, err = tx.ExecContext(ctx,
			`UPDATE users SET available=available+? WHERE id=?`, fee, feeRecipientID)
		if err != nil {
			return dbErr(err, "settle: credit fee recipient")
		}
	}

	return dbErr(tx.Commit(), "settle commit")
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
			total, locked, ownerID)
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
		`INSERT INTO traces (id,process_id,parent_trace_id,created_at) VALUES (?,?,?,?)`,
		t.ID, t.ProcessID, t.ParentTraceID, timeToStr(t.CreatedAt),
	)
	return dbErr(err, "create trace")
}

func (s *DB) ReadTrace(ctx context.Context, id string) (*kernel.Trace, error) {
	var t kernel.Trace
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,process_id,parent_trace_id,created_at FROM traces WHERE id=?`, id,
	).Scan(&t.ID, &t.ProcessID, &t.ParentTraceID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("trace not found")
	}
	if err != nil {
		return nil, dbErr(err, "read trace")
	}
	t.CreatedAt = strToTime(createdAt)
	return &t, nil
}

// ---- Transactions ----

func (s *DB) CreateTransaction(ctx context.Context, tx *kernel.Transaction) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO transactions
		 (id,process_id,trace_id,parent_trace_id,owner_user_id,subject_user_id,target_user_id,
		  action_id,args_json,reply_json,status,gross,net,fee,reason,started_at,ended_at,rating)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		tx.ID, tx.ProcessID, tx.TraceID, tx.ParentTraceID,
		tx.OwnerUserID, tx.SubjectUserID, tx.TargetUserID, tx.ActionID,
		tx.ArgsJSON, tx.ReplyJSON, string(tx.Status),
		tx.Gross, tx.Net, tx.Fee, tx.Reason,
		timeToStr(tx.StartedAt), timeToStr(tx.EndedAt), tx.Rating,
	)
	return dbErr(err, "create transaction")
}

func (s *DB) UpdateTransaction(ctx context.Context, tx *kernel.Transaction) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE transactions SET reply_json=?,status=?,gross=?,net=?,fee=?,reason=?,ended_at=?,rating=?
		 WHERE id=?`,
		tx.ReplyJSON, string(tx.Status), tx.Gross, tx.Net, tx.Fee,
		tx.Reason, timeToStr(tx.EndedAt), tx.Rating, tx.ID,
	)
	return dbErr(err, "update transaction")
}

func (s *DB) ReadTransaction(ctx context.Context, id string) (*kernel.Transaction, error) {
	var tx kernel.Transaction
	var status, startedAt, endedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,process_id,trace_id,parent_trace_id,owner_user_id,subject_user_id,target_user_id,
		        action_id,args_json,reply_json,status,gross,net,fee,reason,started_at,ended_at,rating
		 FROM transactions WHERE id=?`, id,
	).Scan(&tx.ID, &tx.ProcessID, &tx.TraceID, &tx.ParentTraceID,
		&tx.OwnerUserID, &tx.SubjectUserID, &tx.TargetUserID, &tx.ActionID,
		&tx.ArgsJSON, &tx.ReplyJSON, &status,
		&tx.Gross, &tx.Net, &tx.Fee, &tx.Reason,
		&startedAt, &endedAt, &tx.Rating)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.ErrNotFound.Wrap("transaction not found")
	}
	if err != nil {
		return nil, dbErr(err, "read transaction")
	}
	tx.Status = kernel.TxStatus(status)
	tx.StartedAt = strToTime(startedAt)
	tx.EndedAt = strToTime(endedAt)
	return &tx, nil
}

func (s *DB) ListTransactions(ctx context.Context, f kernel.TxFilter) ([]*kernel.Transaction, error) {
	q := `SELECT id,process_id,trace_id,parent_trace_id,owner_user_id,subject_user_id,target_user_id,
	             action_id,args_json,reply_json,status,gross,net,fee,reason,started_at,ended_at,rating
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
		if err := rows.Scan(&tx.ID, &tx.ProcessID, &tx.TraceID, &tx.ParentTraceID,
			&tx.OwnerUserID, &tx.SubjectUserID, &tx.TargetUserID, &tx.ActionID,
			&tx.ArgsJSON, &tx.ReplyJSON, &status,
			&tx.Gross, &tx.Net, &tx.Fee, &tx.Reason,
			&startedAt, &endedAt, &tx.Rating); err != nil {
			return nil, dbErr(err, "scan transaction")
		}
		tx.Status = kernel.TxStatus(status)
		tx.StartedAt = strToTime(startedAt)
		tx.EndedAt = strToTime(endedAt)
		out = append(out, &tx)
	}
	return out, rows.Err()
}

// ---- Stats ----

func (s *DB) ReadStats(ctx context.Context, actionID string) (*kernel.Stats, error) {
	var st kernel.Stats
	var lastUsed string
	err := s.db.QueryRowContext(ctx,
		`SELECT action_id,uses,successes,failures,price_mean,latency_mean,rating_mean,last_used_at
		 FROM action_stats WHERE action_id=?`, actionID,
	).Scan(&st.ActionID, &st.Uses, &st.Successes, &st.Failures,
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
		`INSERT INTO action_stats (action_id,uses,successes,failures,price_mean,latency_mean,rating_mean,last_used_at)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(action_id) DO UPDATE SET
		   uses=excluded.uses, successes=excluded.successes, failures=excluded.failures,
		   price_mean=excluded.price_mean, latency_mean=excluded.latency_mean,
		   rating_mean=excluded.rating_mean, last_used_at=excluded.last_used_at`,
		st.ActionID, st.Uses, st.Successes, st.Failures,
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
		`INSERT INTO listeners (id,owner_user_id,source_user_id,event_name,process_id,trace_id,target_action_id,active,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		l.ID, l.OwnerUserID, l.SourceUserID, l.EventName, l.ProcessID, l.TraceID,
		l.TargetActionID, boolInt(l.Active), timeToStr(l.CreatedAt),
	)
	return dbErr(err, "create listener")
}

func (s *DB) ReadListener(ctx context.Context, id string) (*kernel.Listener, error) {
	var l kernel.Listener
	var active int
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,owner_user_id,source_user_id,event_name,process_id,trace_id,target_action_id,active,created_at
		 FROM listeners WHERE id=?`, id,
	).Scan(&l.ID, &l.OwnerUserID, &l.SourceUserID, &l.EventName, &l.ProcessID,
		&l.TraceID, &l.TargetActionID, &active, &createdAt)
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
		`SELECT id,owner_user_id,source_user_id,event_name,process_id,trace_id,target_action_id,active,created_at
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
			&l.ProcessID, &l.TraceID, &l.TargetActionID, &active, &createdAt); err != nil {
			return nil, dbErr(err, "scan listener")
		}
		l.Active = active != 0
		l.CreatedAt = strToTime(createdAt)
		out = append(out, &l)
	}
	return out, rows.Err()
}

func (s *DB) AppendEvent(ctx context.Context, listenerID, txID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (listener_id,tx_id) VALUES (?,?)`, listenerID, txID)
	return dbErr(err, "append event")
}

func (s *DB) ReadEvents(ctx context.Context, listenerID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tx_id FROM events WHERE listener_id=? ORDER BY id`, listenerID)
	if err != nil {
		return nil, dbErr(err, "read events")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var txID string
		if err := rows.Scan(&txID); err != nil {
			return nil, err
		}
		out = append(out, txID)
	}
	return out, rows.Err()
}

// ---- Traces (by process) ----

func (s *DB) ListTraces(ctx context.Context, processID string) ([]*kernel.Trace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,process_id,parent_trace_id,created_at FROM traces WHERE process_id=?`, processID)
	if err != nil {
		return nil, dbErr(err, "list traces")
	}
	defer rows.Close()
	var out []*kernel.Trace
	for rows.Next() {
		var t kernel.Trace
		var createdAt string
		if err := rows.Scan(&t.ID, &t.ProcessID, &t.ParentTraceID, &createdAt); err != nil {
			return nil, dbErr(err, "scan trace")
		}
		t.CreatedAt = strToTime(createdAt)
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

// schema001 is the initial migration DDL.
const schema001 = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    handle        TEXT NOT NULL UNIQUE,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    available     INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked        INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS actions (
    id            TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL REFERENCES users(id),
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('http','wasm','native')),
    active        INTEGER NOT NULL DEFAULT 0,
    price         INTEGER NOT NULL DEFAULT 0 CHECK (price >= 0),
    description   TEXT NOT NULL DEFAULT '',
    input_schema  TEXT NOT NULL DEFAULT '{}',
    output_schema TEXT NOT NULL DEFAULT '{}',
    source        TEXT NOT NULL DEFAULT '',
    artifact_hash TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (owner_user_id, name)
);

CREATE TABLE IF NOT EXISTS acl_entries (
    subject_user_id TEXT NOT NULL REFERENCES users(id),
    action_id       TEXT NOT NULL REFERENCES actions(id) ON DELETE CASCADE,
    permission      TEXT NOT NULL CHECK (permission IN ('read','call','admin')),
    created_at      TEXT NOT NULL,
    PRIMARY KEY (subject_user_id, action_id, permission)
);

CREATE TABLE IF NOT EXISTS processes (
    id            TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL REFERENCES users(id),
    available     INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked        INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
    created_at    TEXT NOT NULL,
    ended_at      TEXT
);

CREATE TABLE IF NOT EXISTS traces (
    id              TEXT PRIMARY KEY,
    process_id      TEXT NOT NULL REFERENCES processes(id),
    parent_trace_id TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS transactions (
    id              TEXT PRIMARY KEY,
    process_id      TEXT NOT NULL,
    trace_id        TEXT NOT NULL,
    parent_trace_id TEXT NOT NULL,
    owner_user_id   TEXT NOT NULL,
    subject_user_id TEXT NOT NULL,
    target_user_id  TEXT NOT NULL,
    action_id       TEXT NOT NULL,
    args_json       TEXT NOT NULL DEFAULT '',
    reply_json      TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('success','failure')),
    gross           INTEGER NOT NULL DEFAULT 0 CHECK (gross >= 0),
    net             INTEGER NOT NULL DEFAULT 0 CHECK (net >= 0),
    fee             INTEGER NOT NULL DEFAULT 0 CHECK (fee >= 0),
    reason          TEXT NOT NULL DEFAULT '',
    started_at      TEXT NOT NULL,
    ended_at        TEXT NOT NULL,
    rating          REAL
);

CREATE TABLE IF NOT EXISTS action_stats (
    action_id    TEXT PRIMARY KEY REFERENCES actions(id) ON DELETE CASCADE,
    uses         INTEGER NOT NULL DEFAULT 0,
    successes    INTEGER NOT NULL DEFAULT 0,
    failures     INTEGER NOT NULL DEFAULT 0,
    price_mean   REAL NOT NULL DEFAULT 0,
    latency_mean REAL NOT NULL DEFAULT 0,
    rating_mean  REAL NOT NULL DEFAULT 0,
    last_used_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS stat_tags (
    action_id  TEXT NOT NULL REFERENCES actions(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (action_id, key, source)
);

CREATE TABLE IF NOT EXISTS listeners (
    id               TEXT PRIMARY KEY,
    owner_user_id    TEXT NOT NULL REFERENCES users(id),
    source_user_id   TEXT NOT NULL REFERENCES users(id),
    event_name       TEXT NOT NULL,
    process_id       TEXT NOT NULL REFERENCES processes(id),
    trace_id         TEXT NOT NULL,
    target_action_id TEXT NOT NULL REFERENCES actions(id),
    active           INTEGER NOT NULL DEFAULT 1,
    created_at       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    listener_id TEXT NOT NULL REFERENCES listeners(id),
    tx_id       TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS auth_codes (
    code           TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id),
    code_challenge TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL,
    used           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS refresh_tokens (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id),
    expires_at TEXT NOT NULL,
    revoked    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_actions_owner        ON actions(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_acl_action           ON acl_entries(action_id);
CREATE INDEX IF NOT EXISTS idx_transactions_owner   ON transactions(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_transactions_process ON transactions(process_id);
CREATE INDEX IF NOT EXISTS idx_transactions_trace   ON transactions(trace_id);
CREATE INDEX IF NOT EXISTS idx_listeners_source     ON listeners(source_user_id, event_name);
CREATE INDEX IF NOT EXISTS idx_events_listener      ON events(listener_id);
CREATE INDEX IF NOT EXISTS idx_traces_process       ON traces(process_id);
`
