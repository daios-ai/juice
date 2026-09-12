package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/daios-ai/juice/kernel"
)

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
const owedSelect = `SELECT k.idempotency_key, k.counterparty_user_id, t.action_owner_id, t.id,
       t.dispatch_json, x.id IS NOT NULL, COALESCE(r.charge + r.premium, 0),
       t.owed_status, t.owed_amount, t.owed_tx_hash, t.owed_rail_address, t.created_at
  FROM idempotency_records k
  JOIN traces t ON t.idempotency_record_id = k.id
  LEFT JOIN transactions x ON x.trace_id = t.id
  LEFT JOIN receipts r ON r.trace_id = t.id`

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
		owedSelect+` WHERE k.idempotency_key=? AND k.counterparty_user_id=?`, id, peerUserID).Scan)
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
		owedSelect+` WHERE `+unresolved("t", "x", "r")+` ORDER BY t.created_at LIMIT ?`, limit)
	if err != nil {
		return nil, dbErr(err, "list what is owed")
	}
	return queryList(rows, "list what is owed", scanOwed)
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
		  ORDER BY t.created_at LIMIT ?`, limit)
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
