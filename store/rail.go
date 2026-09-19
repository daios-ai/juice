// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
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
func (s *DB) ListRailTransfers(ctx context.Context, kind, party, status string, limit, offset int) ([]*kernel.RailTransfer, error) {
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
	q += ` ORDER BY created_at LIMIT ? OFFSET ?`
	args = append(args, limit, max(offset, 0))
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

// ---- Obligations and exposure (P10) ----
//
// An obligation is read off the call's own records, never kept as a row of its own: the trace froze
// the terms at admission and carries the reveal, the receipt says what was charged, and the
// idempotency record names it for the peer. One number, exposure, is what this kernel has
// delivered and not been paid for. Every state change is one compound commit.

// unresolved is the condition under which an admitted foreign call still has something coming:
// unrevealed with an obligation (or not yet committed, so it may have one), or revealed and unpaid.
// It is the one definition of an open obligation — what reconciliation reserves, what keeps a peer
// from being purged, and what the operator is shown — bound to whatever the query around it named
// the trace, its transaction and its receipt.
func unresolved(trace, tx, receipt string) string {
	return `(` + trace + `.owed_status = 'announced'
	        OR (` + trace + `.owed_status = '' AND (` + tx + `.id IS NULL
	            OR COALESCE(` + receipt + `.charge + ` + receipt + `.premium, 0) > 0)))`
}

// unresolvedTrace binds that condition for a query reading `FROM traces ot`, joining what it needs.
var unresolvedTrace = ` LEFT JOIN transactions ox ON ox.trace_id = ot.id
	  LEFT JOIN receipts orc ON orc.trace_id = ot.id
	 WHERE ` + unresolved("ot", "ox", "orc")

// reservedDeposit says a deposit d comes from an address some unresolved foreign call named as its
// payer, so it may be that call's money and is nobody else's to take yet. A world with no addresses
// reserves nothing: there the payment is the reveal that names its own obligation (D23).
var reservedDeposit = `d.party <> '' AND EXISTS (SELECT 1 FROM traces ot` + unresolvedTrace + ` AND ot.owed_rail_address = d.party)`

// owedSelect is the projection, from the peer's name for the call to the reveal on its trace. The
// obligation is what the receipt charged plus the markup, so it is zero until the call commits.
// The obligation is named by the call it answers, which admission froze on the trace along with
// the rest of what the call was sold under (D19). Reading it from there rather than from the
// execution lock is what lets the lock be released the moment the call commits: an obligation
// outlives the work, and a lock does not (P4, P10).
const owedSelect = `SELECT json_extract(t.dispatch_json,'$.idempotency_key'), t.caller_user_id,
       t.action_owner_id, t.id,
       t.dispatch_json, x.id IS NOT NULL, COALESCE(r.charge + r.premium, 0),
       t.owed_status, t.owed_amount, t.owed_tx_hash, t.owed_rail_address, t.created_at
  FROM traces t
  LEFT JOIN transactions x ON x.trace_id = t.id
  LEFT JOIN receipts r ON r.trace_id = t.id`

// owedIsAdmitted selects the traces that are obligations at all: an inbound call this kernel
// admitted, which is exactly a trace carrying a frozen request.
const owedIsAdmitted = `COALESCE(json_extract(t.dispatch_json,'$.idempotency_key'),'') <> ''`

func scanOwed(scan func(...any) error) (*kernel.Owed, error) {
	var o kernel.Owed
	var terms sql.NullString
	var createdAt string
	if err := scan(&o.ID, &o.PeerUserID, &o.UserID, &o.TraceID, &terms, &o.Settled, &o.Obligation,
		&o.Status, &o.Amount, &o.TxHash, &o.RailAddr, &createdAt); err != nil {
		return nil, err
	}
	o.Terms = terms.String
	o.CreatedAt = strToTime(createdAt)
	return &o, nil
}

func (s *DB) ReadOwed(ctx context.Context, id, peerUserID string) (*kernel.Owed, error) {
	o, err := scanOwed(s.db.QueryRowContext(ctx,
		owedSelect+` WHERE `+owedIsAdmitted+` AND json_extract(t.dispatch_json,'$.idempotency_key')=? AND t.caller_user_id=?`, id, peerUserID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, dbErr(err, "read obligation")
}

// correctExposureTx is the commit's half of the credit limit: admission counted the most the call
// could owe, and this corrects the counter to what it charged — a failure that still charged for
// settled work owes exactly that, and one that charged nothing gives its whole reservation back.
// Every commit path calls it; a local call froze no serving terms and moves nothing.
func correctExposureTx(ctx context.Context, tx *sql.Tx, traceID string, obligation int64) error {
	var terms sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT dispatch_json FROM traces WHERE id=?`, traceID).Scan(&terms); err != nil {
		return dbErr(err, "correct exposure: read trace")
	}
	reserve, foreign := kernel.ServingReserve(nullStrPtr(terms))
	if !foreign {
		return nil
	}
	return moveExposure(ctx, tx, obligation-reserve)
}

func nullStrPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// ApplyReveal records how a draw came out, in the one shape both outcomes share: an amount, and the
// payment that will carry it. Nothing owed closes the obligation for good and leaves no payment to
// wait for; anything else waits for that payment to arrive. Guarded on the reveal not having been
// applied, so a buyer resending because our reply was lost changes nothing.
//
// Where the world has no addresses the reveal is itself the finalized payment (D23), and the caller
// hands that payment in: it is booked in this same statement, so the fact and the money it makes
// final are one commit and no pass can ever find one without the other.
func (s *DB) ApplyReveal(ctx context.Context, sys, traceID string, amount int64, txHash string, payment *kernel.RailTransfer) error {
	status := kernel.OwedAnnounced
	if amount <= 0 {
		amount, status, txHash, payment = 0, kernel.OwedCancelled, "", nil
	}
	return s.withTx(ctx, "apply reveal", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE traces SET owed_status=?, owed_amount=?, owed_tx_hash=? WHERE id=? AND owed_status=''`,
			status, amount, txHash, traceID)
		if err != nil {
			return dbErr(err, "apply reveal")
		}
		// The guard above is what makes a resend change nothing; booking the payment under it means
		// a resend cannot book a second one either, however often the buyer repeats itself.
		if n, _ := res.RowsAffected(); n == 0 || payment == nil {
			return nil
		}
		return bookDeposit(ctx, tx, sys, payment)
	})
}

// ReconcileDeposits is the one path every payment this kernel observes takes, in one transaction,
// and returns the credits it wrote. Obligations come first: the match is the join itself, so no
// caller can bypass the rules that make a payment an obligation's — a held deposit, from the sender
// the buyer proved, carrying the transaction it named, for the amount the draw decided. On a world
// with no addresses both sides of the sender test are empty, which is the right answer there. One
// payment closes at most one obligation, because crediting it marks that deposit spent.
//
// The seller is credited the whole payment. The draw pays the face value or nothing and its expected
// value is the obligation, so a kernel keeping the difference would take a position in its users'
// trades and pay its sellers less than they are owed on average.
//
// Whatever no obligation claimed is then an ordinary payment to whoever registered the address it
// came from — unless some foreign call was admitted with that address as its payer and is not yet
// resolved: unrevealed, or revealed and unpaid. A buyer's winning payment can land before its reveal
// arrives, from an address a local account also registered, so such a deposit stays held until the
// reveal decides whose it is. The payer was proven at admission, so this holds for a buyer this
// kernel had never heard of before its call. No deadline: a late losing reveal is deliberately valid.
func (s *DB) ReconcileDeposits(ctx context.Context, sysID string, limit int) ([]*kernel.LedgerEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	var closed []*kernel.LedgerEntry
	err := s.withTx(ctx, "reconcile deposits", func(tx *sql.Tx) error {
		type match struct{ trace, seller, deposit, owner string }
		collect := func(query string, args ...any) ([]match, error) {
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, dbErr(err, "reconcile deposits: find")
			}
			defer rows.Close()
			var out []match
			for rows.Next() {
				var m match
				if err := rows.Scan(&m.trace, &m.seller, &m.deposit, &m.owner); err != nil {
					return nil, dbErr(err, "reconcile deposits: scan")
				}
				out = append(out, m)
			}
			return out, dbErr(rows.Err(), "reconcile deposits: read")
		}
		paid, err := collect(
			`SELECT t.id, t.action_owner_id, d.id, t.action_owner_id
			   FROM traces t
			   JOIN rail_transfers d
			     ON d.kind = 'deposit' AND d.status = 'held'
			    AND d.party = t.owed_rail_address AND d.tx_hash = t.owed_tx_hash AND d.amount = t.owed_amount
			  WHERE t.owed_status = ? AND t.owed_tx_hash <> ''
			  ORDER BY t.created_at LIMIT ?`, kernel.OwedAnnounced, limit)
		if err != nil {
			return err
		}
		spent, done := map[string]bool{}, map[string]bool{}
		for _, m := range paid {
			if spent[m.deposit] || done[m.trace] {
				continue // one payment settles one obligation, and one obligation takes one payment
			}
			row, err := readRailTx(ctx, tx, m.deposit)
			if err != nil {
				return dbErr(err, "reconcile deposits: read payment")
			}
			e, err := deliverDeposit(ctx, tx, row, sysID, m.seller, row.Amount)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE traces SET owed_status=? WHERE id=? AND owed_status=?`,
				kernel.OwedCredited, m.trace, kernel.OwedAnnounced); err != nil {
				return dbErr(err, "reconcile deposits: close")
			}
			if err := moveExposure(ctx, tx, -row.Amount); err != nil {
				return err
			}
			spent[m.deposit], done[m.trace] = true, true
			closed = append(closed, e)
		}
		known, err := collect(
			`SELECT '', '', d.id, a.id FROM rail_transfers d
			   JOIN accounts a ON a.rail_address = d.party
			  WHERE d.kind = 'deposit' AND d.status = 'held' AND d.party <> ''
			    AND a.kernel_public_key IS NULL AND a.suspended_at IS NULL AND a.password_hash <> ''
			    AND NOT (`+reservedDeposit+`)
			  ORDER BY d.created_at LIMIT ?`, limit)
		if err != nil {
			return err
		}
		for _, m := range known {
			if spent[m.deposit] {
				continue
			}
			row, err := readRailTx(ctx, tx, m.deposit)
			if err != nil {
				return dbErr(err, "reconcile deposits: read payment")
			}
			e, err := deliverDeposit(ctx, tx, row, sysID, m.owner, row.Amount)
			if err != nil {
				return err
			}
			spent[m.deposit] = true
			closed = append(closed, e)
		}
		return nil
	})
	return closed, err
}

// ListOwed is every obligation this kernel is still waiting to be paid for, oldest first: the work
// its exposure is made of, itemised. A buyer that has not yet said how the draw came out is on it
// with an empty status, since what bounds such a buyer is the credit limit and what acts on one is
// the operator.
func (s *DB) ListOwed(ctx context.Context, limit int) ([]*kernel.Owed, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		owedSelect+` WHERE `+owedIsAdmitted+` AND `+unresolved("t", "x", "r")+` ORDER BY t.created_at LIMIT ?`, limit)
	if err != nil {
		return nil, dbErr(err, "list what is owed")
	}
	return queryList(rows, "list what is owed", scanOwed)
}

// PeersWithUnresolvedMoney is every peer some money is waiting on, in either direction, as keys
// alone: one that owes this kernel for work delivered, one that has not heard how a draw it is
// owed came out, and one holding a call of ours whose answer decides a reserve. The scheduler
// needs identities, not the financial records behind them, so this is one distinct-key query
// rather than three paged listings and a read per row — and being unpaged, it cannot silently
// omit the peer whose obligation happens to sort hundred-and-first (§13, P10).
func (s *DB) PeersWithUnresolvedMoney(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT key FROM (
  -- a peer that owes us for work we delivered
  SELECT COALESCE(a.kernel_public_key,'') AS key
    FROM traces t
    JOIN accounts a ON a.id = t.caller_user_id
    LEFT JOIN transactions x ON x.trace_id = t.id
    LEFT JOIN receipts r ON r.trace_id = t.id
   WHERE `+owedIsAdmitted+` AND `+unresolved("t", "x", "r")+`
  UNION
  -- a peer that has not heard how a draw it is owed came out
  SELECT COALESCE(a.kernel_public_key,'')
    FROM traces t
    JOIN transactions x ON x.trace_id = t.id
    JOIN accounts a ON a.id = x.target_user_id
   WHERE t.revealed = 0 AND t.idempotency_key IS NOT NULL AND x.net > 0
  UNION
  -- a peer holding a call of ours whose answer decides money already reserved
  SELECT COALESCE(ow.kernel_public_key,'')
    FROM traces t
    JOIN actions act ON act.id = t.action_id
    LEFT JOIN accounts ow ON ow.id = act.owner_user_id
   WHERE t.idempotency_key IS NOT NULL
     AND NOT EXISTS (SELECT 1 FROM transactions x2 WHERE x2.trace_id = t.id)
) WHERE key <> ''`)
	if err != nil {
		return nil, dbErr(err, "list peers money is waiting on")
	}
	return queryList(rows, "list peers money is waiting on", func(scan func(...any) error) (string, error) {
		var key string
		err := scan(&key)
		return key, err
	})
}

func (s *DB) Exposure(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT value FROM exposure WHERE id=1`).Scan(&v)
	return v, dbErr(err, "read exposure")
}

// ListPendingReveals returns the calls whose seller has still to be told how the draw came out,
// assembled from the records that already hold it: the trace's frozen secret and idempotency key,
// the peer the proxy belongs to, and — when the draw won — the payment, offered only once it is
// confirmed, since a seller told of a payment that never lands would be owed forever.
//
// Only calls that actually owe something qualify: the transaction's net is the obligation, so a
// rejection, a free call, a zero-charge failure and a quarantined receipt are all excluded. None of
// them left the seller anything to reveal against, so revealing one could only fail forever and,
// being oldest, would starve the reveals behind it. Only actionable rows come back for the same
// reason, so a payment still in flight never sits at the head of the queue either.
func (s *DB) ListPendingReveals(ctx context.Context, limit int) ([]*kernel.PendingReveal, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.idempotency_key, t.id, a.kernel_public_key, t.dispatch_json, COALESCE(p.tx_hash,'')
		   FROM traces t
		   JOIN transactions x ON x.trace_id = t.id
		   JOIN accounts a ON a.id = x.target_user_id
		   LEFT JOIN rail_transfers p ON p.id = t.idempotency_key AND p.kind = 'obligation'
		  WHERE t.revealed = 0 AND t.idempotency_key IS NOT NULL
		    AND a.kernel_public_key IS NOT NULL AND x.net > 0
		    AND (p.id IS NULL OR p.status = 'confirmed')
		  ORDER BY COALESCE(t.reveal_failed_at,'') ASC, t.created_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, dbErr(err, "list pending reveals")
	}
	return queryList(rows, "list pending reveals", func(scan func(...any) error) (*kernel.PendingReveal, error) {
		var d kernel.PendingReveal
		var dispatchJSON sql.NullString
		if err := scan(&d.ID, &d.TraceID, &d.PeerKey, &dispatchJSON, &d.TxHash); err != nil {
			return nil, err
		}
		if dispatchJSON.Valid {
			d.Secret = kernel.DispatchSecret(dispatchJSON.String)
		}
		return &d, nil
	})
}

// MarkRevealFailed records that this reveal could not be delivered, which moves it behind every
// reveal not yet tried. Order alone rotates the queue: no attempt counter, no backoff state, and a
// peer that comes back is reached on the next pass like any other (P10).
func (s *DB) MarkRevealFailed(ctx context.Context, traceID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE traces SET reveal_failed_at=? WHERE id=?`, timeToStr(at), traceID)
	return dbErr(err, "mark reveal failed")
}

// MarkRevealed records that the seller has acknowledged how the draw came out, so the worker stops
// telling it. The payment's own row closes in the same commit — being told is exactly what it was
// still open for — and the guard makes that a no-op for a draw that owed no payment.
func (s *DB) MarkRevealed(ctx context.Context, traceID string) error {
	return s.withTx(ctx, "mark revealed", func(tx *sql.Tx) error {
		var key sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT idempotency_key FROM traces WHERE id=?`, traceID).Scan(&key); err != nil {
			return dbErr(err, "mark revealed: read trace")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE traces SET revealed=1 WHERE id=?`, traceID); err != nil {
			return dbErr(err, "mark revealed")
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE rail_transfers SET status=? WHERE id=? AND kind=? AND status=?`,
			kernel.RailStatusAnnounced, key.String, kernel.RailKindObligation, kernel.RailStatusConfirmed)
		return dbErr(err, "mark revealed: close payment")
	})
}
