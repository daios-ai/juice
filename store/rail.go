package store

import (
	"context"
	"database/sql"

	"time"

	"github.com/google/uuid"

	"github.com/daios-ai/juice/kernel"
)

// ---- Rail (D23) ----
//
// One row per external movement, keyed by the fact that caused it. Money in transit is held on sys
// rather than left available, so the operator cannot spend what is already promised elsewhere, and
// the whole position is one query. Every method here is a single compound commit: the balance move
// and the record that explains it are never two writes.

const railCols = `id,kind,party,amount,credit,destination,status,tx_hash,refill_id,reason,attempt,created_at,finalized_at`

// hold moves amount from an account's available into its locked, refusing to overdraw. Money in
// transit lives there: promised, recorded, and unspendable until the promise resolves.
func hold(ctx context.Context, tx *sql.Tx, id string, amount int64) error {
	if amount == 0 {
		return nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE accounts SET locked=locked+? WHERE id=?`, amount, id)
	if err != nil {
		return dbErr(err, "rail: hold")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return kernel.ErrNotFound.Wrap("rail: account not found")
	}
	return nil
}

// move adds delta to an account's available, refusing to take it negative.
func move(ctx context.Context, tx *sql.Tx, id string, delta int64) error {
	if delta == 0 {
		return nil
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE accounts SET available=available+? WHERE id=? AND (? >= 0 OR available >= ?)`,
		delta, id, delta, -delta)
	if err != nil {
		return dbErr(err, "rail: move")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return kernel.ErrInsufficientFunds.Wrap("insufficient balance")
	}
	return nil
}

// release takes amount out of an account's locked.
func release(ctx context.Context, tx *sql.Tx, id string, amount int64) error {
	if amount == 0 {
		return nil
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE accounts SET locked=locked-? WHERE id=? AND locked >= ?`, amount, id, amount)
	if err != nil {
		return dbErr(err, "rail: release")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return kernel.ErrInvalidState.Wrap("rail: held amount is missing")
	}
	return nil
}

func insertRail(ctx context.Context, tx *sql.Tx, r *kernel.RailTransfer) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO rail_transfers (`+railCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Kind, r.Party, r.Amount, r.Credit, r.Destination, r.Status, r.TxHash,
		r.RefillID, r.Reason, r.Attempt, timeToStr(r.CreatedAt), nullTimeToStr(r.FinalizedAt))
	return dbErr(err, "rail: insert row")
}

func readRailTx(ctx context.Context, tx *sql.Tx, id string) (*kernel.RailTransfer, error) {
	return scanRail(tx.QueryRowContext(ctx, `SELECT `+railCols+` FROM rail_transfers WHERE id=?`, id).Scan)
}

func scanRail(scan func(...any) error) (*kernel.RailTransfer, error) {
	var r kernel.RailTransfer
	var createdAt string
	var finalizedAt *string
	if err := scan(&r.ID, &r.Kind, &r.Party, &r.Amount, &r.Credit, &r.Destination, &r.Status,
		&r.TxHash, &r.RefillID, &r.Reason, &r.Attempt, &createdAt, &finalizedAt); err != nil {
		return nil, err
	}
	r.CreatedAt = strToTime(createdAt)
	r.FinalizedAt = strToNullTime(finalizedAt)
	return &r, nil
}

// CreateRailDeposit books a payment in. The crossing credits the operator and holds it; when the
// sender is known the same commit delivers the credit to its owner and keeps the remainder. A
// replayed fact returns what was written and moves nothing, which is what makes an operator's retry
// and a re-observed payment equally safe. The same fact on other terms is refused, since the
// stateless manual rail cannot refuse it itself.
func (s *DB) CreateRailDeposit(ctx context.Context, sys string, row *kernel.RailTransfer, toUserID string) (*kernel.LedgerEntry, error) {
	var out *kernel.LedgerEntry
	err := s.withTx(ctx, "rail deposit", func(tx *sql.Tx) error {
		existing, err := readRailTx(ctx, tx, row.ID)
		if err != nil && err != sql.ErrNoRows {
			return dbErr(err, "rail: read deposit")
		}
		// A payment some unresolved foreign obligation named its payer for may be that obligation's
		// money: nobody may be handed it by name until the reveal has said whose it is. The same
		// rule reconciliation applies, so the operator cannot route around it.
		if toUserID != "" && row.Party != "" {
			var reserved bool
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM rail_transfers d WHERE d.id=? AND `+reservedDeposit+`)`, row.ID).Scan(&reserved); err != nil {
				return dbErr(err, "rail: reservation")
			}
			if reserved {
				return kernel.ErrInvalidState.Wrapf("payment %s may settle a peer's obligation and waits for its reveal", row.ID)
			}
		}
		if existing != nil {
			if existing.Amount != row.Amount || existing.Party != row.Party {
				return kernel.ErrInvalidInput.Wrapf("payment %s was already recorded on other terms", row.ID)
			}
			*row = *existing
			// A payment already booked but still held is exactly what the operator's attribution
			// names: deliver it now, in this commit, rather than answering with nothing.
			if toUserID != "" && existing.Status == kernel.RailStatusHeld {
				out, err = deliverDeposit(ctx, tx, existing, sys, toUserID, row.Credit)
				return err
			}
			out, err = readLedgerByExternalKey(ctx, tx, kernel.AttributionKey(row.ID))
			return err
		}
		if err := bookDeposit(ctx, tx, sys, row); err != nil {
			return err
		}
		if toUserID == "" {
			return nil
		}
		out, err = deliverDeposit(ctx, tx, row, sys, toUserID, row.Credit)
		return err
	})
	return out, err
}

// bookDeposit is the crossing: credits enter the ledger here and nowhere else, against a finalized
// fact. The money is the operator's and held — unspendable — until reconciliation says whose it is.
func bookDeposit(ctx context.Context, tx *sql.Tx, sys string, row *kernel.RailTransfer) error {
	if err := hold(ctx, tx, sys, row.Amount); err != nil {
		return err
	}
	if err := insertLedgerRow(ctx, tx, &kernel.LedgerEntry{
		ID: uuid.NewString(), OperatorUserID: sys, ToUserID: sys, Amount: row.Amount,
		Reason: row.Reason, ExternalKey: row.ID, CreatedAt: row.CreatedAt}); err != nil {
		return err
	}
	return insertRail(ctx, tx, row)
}

// deliverDeposit hands a held payment to its owner: the hold ends, the owner is credited, and what
// is left over is the operator's own. One ledger entry records the delivery, so both parties read
// the same movement from their own ledger.
func deliverDeposit(ctx context.Context, tx *sql.Tx, row *kernel.RailTransfer, sys, toUserID string, credit int64) (*kernel.LedgerEntry, error) {
	if credit > row.Amount {
		return nil, kernel.ErrInvalidInput.Wrap("rail: credit exceeds the payment")
	}
	if err := release(ctx, tx, sys, row.Amount); err != nil {
		return nil, err
	}
	if err := move(ctx, tx, toUserID, credit); err != nil {
		return nil, err
	}
	if err := move(ctx, tx, sys, row.Amount-credit); err != nil {
		return nil, err
	}
	e := &kernel.LedgerEntry{ID: uuid.NewString(), OperatorUserID: sys, FromUserID: sys,
		ToUserID: toUserID, Amount: credit, Reason: row.Reason, ExternalKey: kernel.AttributionKey(row.ID),
		CreatedAt: time.Now().UTC()}
	if credit > 0 {
		if err := insertLedgerRow(ctx, tx, e); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE rail_transfers SET status=?, finalized_at=? WHERE id=?`,
		kernel.RailStatusCredited, timeToStr(time.Now().UTC()), row.ID); err != nil {
		return nil, dbErr(err, "rail: credit deposit")
	}
	return e, nil
}

// ReserveRailTransfer opens an outgoing payment. From this commit the money is nobody's to spend:
// the party is debited, whatever the operator adds is taken from its own balance, and the whole
// amount is held until the payment resolves one way or the other.
func (s *DB) ReserveRailTransfer(ctx context.Context, sys string, row *kernel.RailTransfer) error {
	return s.withTx(ctx, "rail reserve", func(tx *sql.Tx) error {
		return reserveRailTx(ctx, tx, sys, row, row.Party, row.Credit, row.CreatedAt)
	})
}

// reserveRailTx is that reservation inside an open transaction, so a settlement that decides a
// payment books it in the same commit that decided it — there is no moment where the books say a
// draw was won and no money has been set aside for it.
func reserveRailTx(ctx context.Context, tx *sql.Tx, sys string, row *kernel.RailTransfer, party string, credit int64, at time.Time) error {
	if _, err := readRailTx(ctx, tx, row.ID); err == nil {
		return kernel.ErrInvalidInput.Wrapf("%s was already presented", row.ID)
	} else if err != sql.ErrNoRows {
		return dbErr(err, "rail: read row")
	}
	if err := move(ctx, tx, party, -credit); err != nil {
		return err
	}
	if err := move(ctx, tx, sys, -(row.Amount - credit)); err != nil {
		return err
	}
	if err := hold(ctx, tx, sys, row.Amount); err != nil {
		return err
	}
	if credit > 0 {
		if err := insertLedgerRow(ctx, tx, &kernel.LedgerEntry{
			ID: uuid.NewString(), OperatorUserID: sys, FromUserID: party, ToUserID: sys,
			Amount: credit, Reason: row.Reason, ExternalKey: "res:" + row.ID,
			CreatedAt: at}); err != nil {
			return err
		}
	}
	return insertRail(ctx, tx, row)
}

// RecordRailOutcome stores what presenting the payment produced. A refill, when one was bought
// instead, is locked from the operator's balance in the same commit, so the authorization and the
// money set aside for it can never disagree.
// RetryRailTransfer starts a fresh attempt at a payment whose last one was signed and settled
// against it. The money stays committed — the debt did not go away because the rail refused it — and
// only the name it is presented under changes, so the row, the obligation and the seller's view of
// them are untouched (D23, P10).
func (s *DB) RetryRailTransfer(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE rail_transfers SET status=?, attempt=attempt+1, tx_hash='' WHERE id=?`,
		kernel.RailStatusPending, id)
	return dbErr(err, "rail: retry")
}

func (s *DB) RecordRailOutcome(ctx context.Context, sys, id, status, txHash, reason string, refill *kernel.RailTransfer) error {
	return s.withTx(ctx, "rail outcome", func(tx *sql.Tx) error {
		set := `UPDATE rail_transfers SET status=?, reason=?`
		args := []any{status, reason}
		if txHash != "" {
			set += `, tx_hash=?`
			args = append(args, txHash)
		}
		if refill != nil {
			set += `, refill_id=?`
			args = append(args, refill.ID)
		}
		args = append(args, id)
		if _, err := tx.ExecContext(ctx, set+` WHERE id=?`, args...); err != nil {
			return dbErr(err, "rail: record outcome")
		}
		if refill == nil {
			return nil
		}
		if _, err := readRailTx(ctx, tx, refill.ID); err == nil {
			return nil // already locked; a re-presented outcome must not lock twice
		} else if err != sql.ErrNoRows {
			return dbErr(err, "rail: read refill")
		}
		// Fuel is the operator's cost alone, never user backing: the authorized maximum leaves the
		// operator's own balance now and is settled against the exact cost at booking.
		if err := move(ctx, tx, sys, -refill.Amount); err != nil {
			return err
		}
		if err := hold(ctx, tx, sys, refill.Amount); err != nil {
			return err
		}
		return insertRail(ctx, tx, refill)
	})
}

// FinalizeRailTransfer closes a confirmed payment: the hold ends and the money crosses out of the
// ledger under the transaction that carried it, which is also the key that makes this idempotent.
func (s *DB) FinalizeRailTransfer(ctx context.Context, sys, id, txHash string, at time.Time) error {
	return s.withTx(ctx, "rail finalize", func(tx *sql.Tx) error {
		row, err := readRailTx(ctx, tx, id)
		if err == sql.ErrNoRows {
			return kernel.ErrNotFound.Wrapf("no rail transfer %s", id)
		}
		if err != nil {
			return dbErr(err, "rail: read row")
		}
		if row.Status == kernel.RailStatusConfirmed || row.Status == kernel.RailStatusFailed {
			return nil
		}
		if err := release(ctx, tx, sys, row.Amount); err != nil {
			return err
		}
		if err := insertLedgerRow(ctx, tx, &kernel.LedgerEntry{
			ID: uuid.NewString(), OperatorUserID: sys, FromUserID: sys, Amount: row.Amount,
			Reason: row.Reason, ExternalKey: "rail:" + txHash, CreatedAt: at}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE rail_transfers SET status=?, tx_hash=?, finalized_at=? WHERE id=?`,
			kernel.RailStatusConfirmed, txHash, timeToStr(at), id)
		return dbErr(err, "rail: finalize")
	})
}

// CompensateRailTransfer undoes a reservation whose payment finalized without moving money. The
// original entries stand: what happened is recorded by a new one, so the history never rewrites.
func (s *DB) CompensateRailTransfer(ctx context.Context, sys, id string, at time.Time) (*kernel.LedgerEntry, error) {
	var out *kernel.LedgerEntry
	err := s.withTx(ctx, "rail compensate", func(tx *sql.Tx) error {
		if e, err := readLedgerByExternalKey(ctx, tx, "comp:"+id); err != nil {
			return err
		} else if e != nil {
			out = e
			return nil
		}
		row, err := readRailTx(ctx, tx, id)
		if err == sql.ErrNoRows {
			return kernel.ErrNotFound.Wrapf("no rail transfer %s", id)
		}
		if err != nil {
			return dbErr(err, "rail: read row")
		}
		if err := release(ctx, tx, sys, row.Amount); err != nil {
			return err
		}
		if err := move(ctx, tx, row.Party, row.Credit); err != nil {
			return err
		}
		if err := move(ctx, tx, sys, row.Amount-row.Credit); err != nil {
			return err
		}
		out = &kernel.LedgerEntry{ID: uuid.NewString(), OperatorUserID: sys, FromUserID: sys,
			ToUserID: row.Party, Amount: row.Credit, Reason: "payment failed",
			ExternalKey: "comp:" + id, CreatedAt: at}
		if row.Credit > 0 {
			if err := insertLedgerRow(ctx, tx, out); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE rail_transfers SET status=?, finalized_at=? WHERE id=?`,
			kernel.RailStatusFailed, timeToStr(at), id)
		return dbErr(err, "rail: compensate")
	})
	return out, err
}

// BookRefill closes a fuel purchase against what it actually consumed, releasing the rest of the
// authorized maximum. The maximum was only ever authority: booking it would put a number in the
// ledger that no receipt supports.
func (s *DB) BookRefill(ctx context.Context, sys, id string, cost int64, executed bool, at time.Time) error {
	return s.withTx(ctx, "rail book refill", func(tx *sql.Tx) error {
		row, err := readRailTx(ctx, tx, id)
		if err == sql.ErrNoRows {
			return kernel.ErrNotFound.Wrapf("no refill %s", id)
		}
		if err != nil {
			return dbErr(err, "rail: read refill")
		}
		if row.Status != kernel.RailStatusPending {
			return nil
		}
		if cost > row.Amount {
			return kernel.ErrInvalidState.Wrapf("refill %s consumed more than it was authorized", id)
		}
		if err := release(ctx, tx, sys, row.Amount); err != nil {
			return err
		}
		if err := move(ctx, tx, sys, row.Amount-cost); err != nil {
			return err
		}
		if cost > 0 {
			if err := insertLedgerRow(ctx, tx, &kernel.LedgerEntry{
				ID: uuid.NewString(), OperatorUserID: sys, FromUserID: sys, Amount: cost,
				Reason: "fuel", ExternalKey: "rail:refill:" + row.RefillID, CreatedAt: at}); err != nil {
				return err
			}
		}
		status := kernel.RailStatusConfirmed
		if !executed {
			status = kernel.RailStatusFailed
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE rail_transfers SET status=?, finalized_at=? WHERE id=?`, status, timeToStr(at), id)
		return dbErr(err, "rail: book refill")
	})
}

// BindRefill ties the lock taken before the rail signed to the purchase it made, and settles the lock
// at the purchase's own maximum: what was held above it goes back to the operator. Binding the same
// purchase again changes nothing.
func (s *DB) BindRefill(ctx context.Context, sys, id, refillID string, max int64) error {
	return s.withTx(ctx, "rail bind refill", func(tx *sql.Tx) error {
		row, err := readRailTx(ctx, tx, id)
		if err == sql.ErrNoRows {
			return kernel.ErrNotFound.Wrapf("no refill %s", id)
		}
		if err != nil {
			return dbErr(err, "rail: read refill")
		}
		if row.RefillID == refillID && row.Amount == max {
			return nil
		}
		if row.RefillID != "" && row.RefillID != refillID {
			return kernel.ErrInvalidState.Wrapf("refill %s is bound to another purchase", id)
		}
		if delta := max - row.Amount; delta < 0 {
			if err := release(ctx, tx, sys, -delta); err != nil {
				return err
			}
			if err := move(ctx, tx, sys, -delta); err != nil {
				return err
			}
		} else if delta > 0 {
			if err := move(ctx, tx, sys, -delta); err != nil {
				return err
			}
			if err := hold(ctx, tx, sys, delta); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE rail_transfers SET refill_id=?, amount=? WHERE id=?`, refillID, max, id)
		return dbErr(err, "rail: bind refill")
	})
}

// ReleaseRefill gives a lock back that bought nothing: the rail was busy or refused before signing,
// so there is no purchase for the row to stand for, and it is removed rather than left as history.
func (s *DB) ReleaseRefill(ctx context.Context, sys, id string) error {
	return s.withTx(ctx, "rail release refill", func(tx *sql.Tx) error {
		row, err := readRailTx(ctx, tx, id)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return dbErr(err, "rail: read refill")
		}
		if row.RefillID != "" {
			return kernel.ErrInvalidState.Wrapf("refill %s stands for a purchase and cannot be released", id)
		}
		if err := release(ctx, tx, sys, row.Amount); err != nil {
			return err
		}
		if err := move(ctx, tx, sys, row.Amount); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM rail_transfers WHERE id=?`, id)
		return dbErr(err, "rail: release refill")
	})
}

// ReadRailTransferByRefill returns the lock a purchase of the rail's is bound to, or nil.
func (s *DB) ReadRailTransferByRefill(ctx context.Context, refillID string) (*kernel.RailTransfer, error) {
	r, err := scanRail(s.db.QueryRowContext(ctx,
		`SELECT `+railCols+` FROM rail_transfers WHERE kind='refill' AND refill_id=?`, refillID).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, dbErr(err, "rail: read refill by purchase")
}

// ReadRailTransfer returns one row, or nil when the fact is unknown here.
func (s *DB) ReadRailTransfer(ctx context.Context, id string) (*kernel.RailTransfer, error) {
	r, err := scanRail(s.db.QueryRowContext(ctx, `SELECT `+railCols+` FROM rail_transfers WHERE id=?`, id).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, dbErr(err, "rail: read row")
}

// ListRailTransfers filters by kind, party and status, oldest first; an empty filter matches all.
// This is the reading question — what happened — so a finished payment is included.
func (s *DB) ListRailTransfers(ctx context.Context, kind, party, status string, limit int) ([]*kernel.RailTransfer, error) {
	q := `SELECT ` + railCols + ` FROM rail_transfers WHERE 1=1`
	args := []any{}
	for col, v := range map[string]string{"kind": kind, "party": party, "status": status} {
		if v != "" {
			q += ` AND ` + col + `=?`
			args = append(args, v)
		}
	}
	if limit <= 0 {
		limit = 100
	}
	q += ` ORDER BY created_at LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, dbErr(err, "rail: list transfers")
	}
	return queryList(rows, "rail: list transfers", scanRail)
}

// ListOpenRailTransfers returns the rows the worker still has to drive, oldest first. Only payments
// out: a held payment in waits on the operator, and a stream of tiny unclaimed ones must not be able
// to push a real withdrawal out of the worker's sight.
func (s *DB) ListOpenRailTransfers(ctx context.Context, limit int) ([]*kernel.RailTransfer, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+railCols+` FROM rail_transfers
		  WHERE kind IN ('payout','obligation')
		    AND status NOT IN ('confirmed','failed','credited','announced')
		  ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, dbErr(err, "rail: list open transfers")
	}
	return queryList(rows, "rail: list open transfers", scanRail)
}

// RailPosition sums the ledger into the operator's account of external money. The vault is derived
// from the crossings themselves — the entries with one side outside the kernel — so it says what the
// books believe is held, which is exactly what the custody audit compares against the rail.
func (s *DB) RailPosition(ctx context.Context, sys string) (*kernel.RailPosition, error) {
	var p kernel.RailPosition
	err := s.db.QueryRowContext(ctx, `
	  SELECT
	    (SELECT COALESCE(SUM(MAX(0, available+locked)),0) FROM accounts),
	    (SELECT COALESCE(SUM(CASE WHEN from_user_id IS NULL THEN amount ELSE -amount END),0)
	       FROM ledger WHERE (from_user_id IS NULL) <> (to_user_id IS NULL) AND id NOT LIKE 'st_%'),
	    (SELECT COALESCE(SUM(amount),0) FROM rail_transfers
	       WHERE kind IN ('payout','obligation') AND status IN ('pending','submitted','refilling','blocked')),
	    (SELECT COALESCE(SUM(amount),0) FROM rail_transfers WHERE kind='deposit' AND status='held'),
	    (SELECT COALESCE(SUM(amount),0) FROM rail_transfers WHERE kind='refill' AND status='pending'),
	    (SELECT COALESCE(available,0) FROM accounts WHERE id=?)`, sys).
		Scan(&p.Liabilities, &p.Vault, &p.PendingPayouts, &p.HeldDeposits,
			&p.RefillLocks, &p.SysAvailable)
	return &p, dbErr(err, "rail: position")
}

// SetRailAddress records where an account is paid. The unique index is the backstop: one address
// belongs to one account, whoever registers it first.
func (s *DB) SetRailAddress(ctx context.Context, userID, address string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET rail_address=?, updated_at=? WHERE id=?`, address, timeToStr(at), userID)
	return dbErr(err, "rail: set address")
}
