package kernel

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ---- The rail interface (D23) ----

// Rail is the witness of external money. It adds no fact and hides none: everything it reports is
// final on the outside world, so the ledger can commit against it. The kernel holds this interface
// and never imports a chain library; rail/ implements it, once for the manual world where the
// operator's own records are the finalized facts, and once over juice-rail.
type Rail interface {
	// Ready reports whether the rail has been fully verified — chain, token, decimals, venue. Until
	// it returns nil no money verb runs and no pass scans: an unverified token misstates every
	// amount. The manual rail is always ready.
	Ready(ctx context.Context) error
	// Address is where this kernel is paid; empty where the world has no addresses.
	Address() string
	// Destination resolves a party's registered address into the destination a payment goes to.
	// A chain rail refuses an unregistered party; the manual rail pays the account itself.
	Destination(registered string) (string, error)
	// Pay presents a payment, idempotently under id. It signs nothing when fuel is short: it says so,
	// and the kernel records its intention to refill before asking for one, so no refill can exist
	// that the ledger does not know about.
	Pay(ctx context.Context, id, to string, amount int64) (RailOutcome, error)
	// Refill buys fuel, leaving reserve untouched — the rail's own rule refuses otherwise. It reports
	// the authorized maximum, which is locked until the exact cost is known. A purchase already made
	// but not yet through is finished rather than duplicated; an empty refill with no error means
	// the rail is busy with something else and the fuel waits its turn.
	Refill(ctx context.Context, reserve int64) (RailRefill, error)
	// FindRefill returns a refill the rail holds that the ledger does not, so one signed just before
	// a crash is adopted rather than lost. known reports whether the ledger already holds an
	// operation, which is where the search stops: everything older has been seen.
	FindRefill(ctx context.Context, known func(id string) bool) (RailRefill, bool, error)
	// Outcome reports where a payment stands. Only a finalized fact is confirmed or failed.
	Outcome(ctx context.Context, id string) (RailStatus, RailFact, error)
	// RefillCost is the exact amount a finalized refill consumed. Pending until it is final.
	RefillCost(ctx context.Context, refillID string) (int64, RailStatus, error)
	// ScanDeposits returns every finalized payment received at or after sinceBlock — not merely the
	// ones newly seen. The rail records a payment and advances its own cursor before the kernel
	// books it, so anything less would lose a payment to a crash between those two writes.
	ScanDeposits(ctx context.Context, sinceBlock uint64) ([]RailDeposit, error)
	// Witness resolves an operator-supplied reference into the finalized payment it names.
	Witness(ctx context.Context, ref string, amount int64) (RailDeposit, error)
	// FinalizedBalances reads holdings at a settled block, the only balance the audit may use. ok is
	// false where the world has no chain to read.
	FinalizedBalances(ctx context.Context) (token int64, gas string, block uint64, ok bool, err error)
	// DepositsScannedTo is how far payments have been observed; false before any scan has run.
	DepositsScannedTo() (uint64, bool, error)
	// Sign proves control of this kernel's own rail address. Empty where there are no addresses.
	Sign(msg []byte) (string, error)
	// Verify checks a signature by address over msg and returns the address in canonical form —
	// the only form the kernel stores or compares, so a differently-cased duplicate cannot exist.
	Verify(msg []byte, address, sig string) (string, error)
}

// RailStatus is where an external movement stands. Only finalized facts are confirmed or failed.
type RailStatus string

const (
	RailUnknown   RailStatus = "unknown"   // no record; safe to present again from scratch
	RailPending   RailStatus = "pending"   // may still execute; never present it again
	RailConfirmed RailStatus = "confirmed" // finalized and executed
	RailFailed    RailStatus = "failed"    // finalized without executing
)

// RailOutcome is what presenting a payment produced: it went out, or fuel must be bought first, or
// nothing was signed and Blocked says why. All empty means another operation is still in flight
// and the payment waits its turn.
type RailOutcome struct {
	TxHash     string
	NeedRefill bool
	Blocked    string
}

// RailRefill is one fuel purchase: its id on the rail and the most it may consume.
type RailRefill struct {
	ID  string
	Max int64
}

// RailFact is a settled outcome. Executed is false when the payment finalized without moving money.
type RailFact struct {
	TxHash string
	Block  uint64
	Exec   bool
}

// RailDeposit is one finalized payment in. Key identifies the fact itself, so booking it twice is a
// no-op no matter which path found it.
type RailDeposit struct {
	Key    string
	TxHash string
	From   string
	Amount int64
	Block  uint64
}

// ---- Rows (D23) ----

// Rail transfer kinds. Every external movement is one row, so the money in transit and the money
// nobody has claimed are the same query.
const (
	RailKindDeposit = "deposit" // a payment in, held until its sender is known
	RailKindPayout  = "payout"  // a user's withdrawal
	RailKindRefill  = "refill"  // the fuel lock
	// RailKindObligation is a won draw on its way to the peer it is owed to. It is kept apart from a
	// withdrawal because the two fail differently: a withdrawal that the rail rejects gives the money
	// back and the user may ask again, while a debt that fails to pay is still a debt — the money
	// stays committed and the payment is presented again (P10).
	RailKindObligation = "obligation"
)

// Rail transfer statuses.
const (
	RailStatusPending   = "pending"
	RailStatusSubmitted = "submitted"
	RailStatusRefilling = "refilling"
	RailStatusBlocked   = "blocked"
	RailStatusConfirmed = "confirmed"
	RailStatusFailed    = "failed"
	RailStatusAnnounced = "announced"
	RailStatusHeld      = "held"
	RailStatusCredited  = "credited"
)

// RailTransfer is one external movement. Amount is what moves on the rail; Credit is what it is
// worth to Party, which differ only where the operator adds to a payment out of its own balance.
// Destination is snapshotted when the row reserves, so nothing — a restart, an address change — can
// redirect a payment already authorized.
type RailTransfer struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Party       string     `json:"party"`
	Amount      int64      `json:"amount"`
	Credit      int64      `json:"credit"`
	Destination string     `json:"destination,omitempty"`
	Status      string     `json:"status"`
	TxHash      string     `json:"tx_hash,omitempty"`
	RefillID    string     `json:"refill_id,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Attempt     int64      `json:"attempt,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	FinalizedAt *time.Time `json:"finalized_at,omitempty"`
}

// RailOp is the name this payment is presented to the rail under. The row keeps one identity for the
// life of the debt — it is what the reveal and the seller know it by — but a rail operation that has
// been signed and settled has spent its place in the account's sequence, so a fresh attempt must ask
// under a fresh name or the rail will decline to act at all.
func (r *RailTransfer) RailOp() string {
	if r.Attempt == 0 {
		return r.ID
	}
	return fmt.Sprintf("%s#%d", r.ID, r.Attempt)
}

// Open reports whether the row still has work to do. A payment for a ticket whose money is final
// still has to reach the seller: until the reveal is acknowledged, the obligation is closed on one
// side only.
func (r *RailTransfer) Open() bool {
	switch r.Status {
	case RailStatusFailed, RailStatusCredited, RailStatusAnnounced, RailStatusConfirmed:
		return false
	}
	return true
}

// RailPosition is the kernel's own account of external money, read from the ledger alone. Vault is
// what has crossed in less what has crossed out, so under conservation it equals what backs the
// credits outstanding.
type RailPosition struct {
	Liabilities    int64 `json:"liabilities"`
	Vault          int64 `json:"vault"`
	PendingPayouts int64 `json:"pending_payouts"`
	HeldDeposits   int64 `json:"held_deposits"`
	RefillLocks    int64 `json:"refill_locks"`
	SysAvailable   int64 `json:"sys_available"`
}

// Gap is the solvency identity: zero when every credit outstanding is backed by cash that actually
// crossed in. There is no receivables term — the ledger is cash-backed, and what strangers owe for
// delivered work is exposure, which the operator carries and never counts as backing. A non-zero gap
// names a broken record, never a policy choice.
func (p RailPosition) Gap() int64 { return p.Liabilities - p.Vault }

// Reserve is what is not the operator's to spend: everything owed to somebody else. It is what the
// kernel hands the rail as the floor a refill may not dip below.
func (p RailPosition) Reserve() int64 {
	r := p.Liabilities - p.SysAvailable
	if r < 0 {
		return 0
	}
	return r
}

// SetRail installs the witness of external money. Called once at startup, before serving.
func (k *Kernel) SetRail(r Rail) { k.rail = r }

// railOrFail returns the rail, or the error a money verb should give when there is none configured.
func (k *Kernel) railOrFail(ctx context.Context) (Rail, error) {
	if k.rail == nil {
		return nil, ErrInvalidState.Wrap("no rail is configured")
	}
	if err := k.rail.Ready(ctx); err != nil {
		return nil, ErrRailStopped.Wrapf("rail is not ready: %v", err)
	}
	return k.rail, nil
}

// ---- The halt (D23) ----

// RailStop reports whether outgoing rail work is halted, and why. It is derived from the rows
// themselves rather than stored: a halt cannot then survive the condition that caused it, nor lift
// while another payment is still refused.
func (k *Kernel) RailStop(ctx context.Context) (reason string, since time.Time, err error) {
	rows, err := k.store.ListRailTransfers(ctx, "", "", RailStatusBlocked, 1, 0)
	if err != nil || len(rows) == 0 {
		return "", time.Time{}, err
	}
	return rows[0].Reason, rows[0].CreatedAt, nil
}

// requireRailRunning refuses an outgoing money verb while anything is blocked. Deposits, paid work,
// and reads are deliberately unaffected: the fees they earn are what clear the halt.
func (k *Kernel) requireRailRunning(ctx context.Context) error {
	reason, since, err := k.RailStop(ctx)
	if err != nil {
		return err
	}
	if reason != "" {
		return ErrRailStopped.Wrapf("outgoing payments are stopped since %s: %s",
			since.UTC().Format(time.RFC3339), reason)
	}
	return nil
}

// ---- Deposits: money in (U3) ----

// Deposit credits a user against a finalized payment from outside. ref names that payment — the
// operator's own record of one where the world has no chain, a transaction where it has. Nothing is
// credited without it: a crossing that named no fact could not be checked against anything, and a
// repeat of it would mint money. Superuser only.
func (k *Kernel) Deposit(ctx context.Context, operatorID, targetUserID string, amount int64, reason, ref string) (*LedgerEntry, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("deposit.start", "target_user_id", targetUserID, "amount", amount)
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		logger.Warn("deposit.failed", "target_user_id", targetUserID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	if ref == "" {
		return nil, ErrInvalidInput.Wrap("a deposit must name the payment it records (--ref)")
	}
	rail, err := k.railOrFail(ctx)
	if err != nil {
		return nil, err
	}
	target, err := k.store.ReadUser(ctx, targetUserID)
	if err != nil {
		return nil, err
	}
	// Only a live user is ever credited. A peer account is identity, never a wallet (D14), and what
	// a peer owes closes when it pays: the payment observed on a chain, or the buyer's own signed
	// reveal where the world has no addresses (P10, D23). The next line would refuse a peer anyway,
	// since a peer holds no handle — this one exists to say why, and to keep the refusal here at the
	// money boundary rather than resting on whatever resolved the name.
	if target.IsPeer() {
		return nil, ErrInvalidInput.Wrapf("%s is a peer: a peer holds no money here, and what it owes closes when it pays",
			k.KernelName(ctx, target.KernelPublicKey))
	}
	if !target.IsLiveUser() {
		return nil, ErrNotFound.Wrapf("account %s is not a live user here", targetUserID)
	}

	fact, err := rail.Witness(ctx, ref, amount)
	if err != nil {
		return nil, err
	}
	if amount > 0 && fact.Amount != amount {
		return nil, ErrInvalidInput.Wrapf("payment %s is %d, not %d", ref, fact.Amount, amount)
	}
	// Every payment enters held and is reconciled — obligations first — before anyone is handed it
	// by name, so an operator's attribution is the fallback for money no obligation claims, never a
	// way past the rules that decide whose it is. The deposit is built from the witnessed fact
	// alone, never from what anybody claimed about it.
	row := heldDeposit(fact, reason)
	if _, err := k.store.CreateRailDeposit(ctx, k.cfg.FeeRecipientID, row, ""); err != nil {
		logger.Warn("deposit.failed", "target_user_id", targetUserID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	if _, err := k.store.ReconcileDeposits(ctx, k.cfg.FeeRecipientID, 100); err != nil {
		return nil, err
	}
	e, err := k.store.CreateRailDeposit(ctx, k.cfg.FeeRecipientID, row, targetUserID)
	if err != nil {
		return nil, err
	}
	logger.Info("deposit.created", "deposit_id", row.ID, "target_user_id", targetUserID, "amount", amount,
		"status", "success", "duration_ms", time.Since(start).Milliseconds())
	return e, nil
}

// AttributionKey names the ledger entry that delivered one deposit to its owner: the deposit's own
// fact, prefixed. One definition, so the store writes it and the kernel can find it exactly.
func AttributionKey(depositID string) string { return "attr:" + depositID }

// heldDeposit is one witnessed payment as a row. Every payment enters held, whoever noticed it —
// the operator, the deposit scan, or a reveal that is itself the payment (D23) — because what
// decides whose it is is reconciliation and nothing else.
func heldDeposit(fact RailDeposit, reason string) *RailTransfer {
	return &RailTransfer{ID: fact.Key, Kind: RailKindDeposit, Party: fact.From, Amount: fact.Amount,
		Credit: fact.Amount, TxHash: fact.TxHash, Status: RailStatusHeld, Reason: reason,
		CreatedAt: time.Now().UTC()}
}

// ListHeldDeposits is money that has arrived and nobody has claimed: waiting on the one decision
// only the ledger authority can make, which sender it belongs to.
func (k *Kernel) ListHeldDeposits(ctx context.Context, operatorID string) ([]*RailTransfer, error) {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return nil, err
	}
	return k.store.ListRailTransfers(ctx, RailKindDeposit, "", RailStatusHeld, 200, 0)
}

// ListOwed is what buyers have said they paid and this kernel has not yet seen the money for — the
// other half of what waits on the operator, and on a world whose finalized facts are its own records
// the half only the operator can close.
func (k *Kernel) ListOwed(ctx context.Context, operatorID string) ([]*Owed, error) {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return nil, err
	}
	return k.store.ListOwed(ctx, 200)
}

// ---- Withdrawals: money out (U51) ----

// Withdraw sends a user's own credits back out. id is the caller's, and is the row: presenting it
// again returns what was already created rather than paying twice, which is what makes a lost reply
// safe to retry. The destination is resolved and stored now, so nothing that happens later can
// redirect the payment.
func (k *Kernel) Withdraw(ctx context.Context, callerID, id string, amount int64, reason string) (*RailTransfer, error) {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrInvalidInput.Wrap("withdrawal id must be a uuid the caller mints")
	}
	if amount <= 0 {
		return nil, ErrInvalidInput.Wrap("amount must be positive")
	}
	if existing, err := k.store.ReadRailTransfer(ctx, id); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.Kind != RailKindPayout || existing.Party != u.ID || existing.Amount != amount {
			return nil, ErrInvalidInput.Wrapf("withdrawal %s was already made on other terms", id)
		}
		return existing, nil
	}
	if err := k.requireRailRunning(ctx); err != nil {
		return nil, err
	}
	rail, err := k.railOrFail(ctx)
	if err != nil {
		return nil, err
	}
	dest, err := rail.Destination(u.RailAddress)
	if err != nil {
		return nil, err
	}
	row := &RailTransfer{ID: id, Kind: RailKindPayout, Party: u.ID, Amount: amount, Credit: amount,
		Destination: dest, Status: RailStatusPending, Reason: reason, CreatedAt: time.Now().UTC()}
	if err := k.store.ReserveRailTransfer(ctx, k.cfg.FeeRecipientID, row); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("withdrawal.created", "withdrawal_id", id, "target_user_id", u.ID, "amount", amount)
	// Outgoing rail work is one thing at a time: the reserve a fuel purchase must leave untouched is
	// read from the books, and it holds only while nothing else is drawing on them.
	k.railMu.Lock()
	k.driveRailTransfer(ctx, row)
	k.railMu.Unlock()
	return row, nil
}

// ListWithdrawals returns a page of the caller's own payouts.
func (k *Kernel) ListWithdrawals(ctx context.Context, callerID string, limit, offset int) ([]*RailTransfer, error) {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return k.store.ListRailTransfers(ctx, RailKindPayout, u.ID, "", limit, offset)
}

// ---- Addresses (D23) ----

// RailAddressMessage is what a user signs to prove an address is theirs. It names this kernel and
// this account, so a signature captured here proves nothing anywhere else. Exported because the
// client builds the same bytes for the wallet to sign: one definition, or the two would drift.
func RailAddressMessage(kernelKey, userID, address string) []byte {
	return []byte("juice address registration\nkernel: " + kernelKey + "\nuser: " + userID + "\naddress: " + address)
}

// railIdentityMessage is what a kernel signs with its rail key to prove the address it advertises is
// its own. Without it a kernel could name a third party's address and claim their payment.
func railIdentityMessage(kernelKey, network, address string) []byte {
	return []byte("juice kernel rail address\nkernel: " + kernelKey + "\nnetwork: " + network + "\naddress: " + address)
}

// SetRailAddress registers where a user is paid, against a signature proving they control it. The
// canonical form is stored, so one address cannot be registered twice under different spellings.
// Registering also attributes anything that address has already paid in: attribution is a function
// of the address, not of when the kernel learned it.
func (k *Kernel) SetRailAddress(ctx context.Context, callerID, address, signature string) (*Account, []*LedgerEntry, error) {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, nil, err
	}
	rail, err := k.railOrFail(ctx)
	if err != nil {
		return nil, nil, err
	}
	self, err := k.store.GetConfig(ctx, "signing_public_key")
	if err != nil {
		return nil, nil, err
	}
	canonical, err := rail.Verify(RailAddressMessage(self, u.ID, address), address, signature)
	if err != nil {
		return nil, nil, err
	}
	if err := k.store.SetRailAddress(ctx, u.ID, canonical, time.Now().UTC()); err != nil {
		return nil, nil, err
	}
	// Money already here from this sender is now theirs. Deciding that is reconciliation's job and
	// nobody else's, so registering an address runs it rather than repeating the decision: the same
	// rule then governs a payment that arrives before the address as one that arrives after.
	credits, err := k.store.ReconcileDeposits(ctx, k.cfg.FeeRecipientID, 200)
	if err != nil {
		return nil, nil, err
	}
	var attributed []*LedgerEntry
	for _, e := range credits {
		if e.ToUserID == u.ID {
			attributed = append(attributed, e)
		}
	}
	u.RailAddress = canonical
	k.log.With(ctx).Info("rail.address.registered", "caller_user_id", u.ID, "attributed", len(attributed))
	return u, attributed, nil
}

// RailIdentity is this kernel's own address and the proof it controls it, for gossip (P9).
func (k *Kernel) RailIdentity(ctx context.Context) (address, proof string) {
	if k.rail == nil {
		return "", ""
	}
	address = k.rail.Address()
	if address == "" {
		return "", ""
	}
	self, err := k.store.GetConfig(ctx, "signing_public_key")
	if err != nil {
		return "", ""
	}
	proof, err = k.rail.Sign(railIdentityMessage(self, k.cfg.Network.Digest, address))
	if err != nil {
		return "", ""
	}
	return address, proof
}

// verifyRailIdentity checks a peer's advertised address. On a world without addresses both must be
// empty; on one with them the signature must prove control, or the peer could name a stranger's.
// verifyRailIdentity checks a peer's claim to a rail address and returns the address in the form
// the rail itself reports senders in — the form a payment from it will carry, and so the only form
// worth freezing against one.
func (k *Kernel) verifyRailIdentity(peerKey, address, proof string) (string, error) {
	if k.rail == nil {
		return address, nil
	}
	return k.rail.Verify(railIdentityMessage(peerKey, k.cfg.Network.Digest, address), address, proof)
}

// ---- The worker (D23) ----

// RailPass is one turn of the rail worker: re-drive everything still open, observe payments in,
// close any obligation whose payment has arrived, tell the sellers still waiting how their draws
// came out, and audit. It does nothing at all until the rail is verified, since an unverified token
// misstates every amount.
func (k *Kernel) RailPass(ctx context.Context) {
	if k.rail == nil {
		return
	}
	logger := k.log.With(ctx)
	if err := k.rail.Ready(ctx); err != nil {
		logger.Warn("rail.unready", "error", err.Error())
		return
	}
	k.railMu.Lock()
	defer k.railMu.Unlock()

	open, err := k.store.ListOpenRailTransfers(ctx, 500)
	if err != nil {
		logger.Warn("rail.list_failed", "error", err.Error())
		return
	}
	for _, row := range open {
		k.driveRailTransfer(ctx, row)
	}
	k.scanRailDeposits(ctx)
	k.reconcileDeposits(ctx)
	// A reveal the peer never acknowledged is simply sent again from the row it is recorded on: a
	// losing draw the seller never heard about would leave it owed forever (P10).
	k.RevealPending(ctx)

	rep, err := k.railReport(ctx)
	if err != nil {
		return
	}
	if rep.Gap != 0 {
		logger.Error("rail.audit.gap", "gap", rep.Gap, "liabilities", rep.Position.Liabilities,
			"vault", rep.Position.Vault)
	}
	if rep.CustodyChecked && !rep.CustodyOK {
		logger.Error("rail.audit.custody", "difference", rep.Custody, "vault", rep.Position.Vault,
			"refill_locks", rep.Position.RefillLocks)
	}
}

// driveRailTransfer advances an outgoing row as far as the rail allows right now. Every transition
// is its own store commit, so a crash between two of them resumes rather than repeats; looping only
// means finishing what can be finished, which is why a payment the operator confirms by recording it
// closes immediately rather than waiting for the next pass.
func (k *Kernel) driveRailTransfer(ctx context.Context, row *RailTransfer) {
	for i := 0; i < len(railDriveSteps); i++ {
		before := row.Status
		k.driveRailStep(ctx, row)
		fresh, err := k.store.ReadRailTransfer(ctx, row.ID)
		if err != nil || fresh == nil {
			return
		}
		*row = *fresh
		if row.Status == before || !row.Open() {
			return
		}
	}
}

// railDriveSteps bounds one drive: pending → submitted → confirmed, or through a refill and back.
var railDriveSteps = [5]struct{}{}

// driveRailStep advances the row by exactly one transition.
func (k *Kernel) driveRailStep(ctx context.Context, row *RailTransfer) {
	logger := k.log.With(ctx)
	switch row.Status {
	case RailStatusPending, RailStatusBlocked:
		out, err := k.rail.Pay(ctx, row.RailOp(), row.Destination, row.Amount)
		if err != nil {
			logger.Warn("rail.pay.failed", "rail_transfer_id", row.ID, "error", err.Error())
			return
		}
		switch {
		case out.Blocked != "":
			_ = k.store.RecordRailOutcome(ctx, k.cfg.FeeRecipientID, row.ID, RailStatusBlocked, "", out.Blocked, nil)
			logger.Warn("rail.blocked", "rail_transfer_id", row.ID, "reason", out.Blocked)
		case out.NeedRefill:
			// The intention is recorded before the rail signs anything, so a crash between the two
			// leaves a row that says a refill may exist, and the next pass goes looking for it. If
			// that record cannot be made, nothing is signed: an unrecorded purchase is money the
			// books would never see.
			if err := k.store.RecordRailOutcome(ctx, k.cfg.FeeRecipientID, row.ID, RailStatusRefilling, "", "", nil); err != nil {
				logger.Warn("rail.refill.intent_unrecorded", "rail_transfer_id", row.ID, "error", err.Error())
				return
			}
			k.refillFor(ctx, row)
		case out.TxHash != "":
			_ = k.store.RecordRailOutcome(ctx, k.cfg.FeeRecipientID, row.ID, RailStatusSubmitted, out.TxHash, "", nil)
		}
	case RailStatusSubmitted:
		st, fact, err := k.rail.Outcome(ctx, row.RailOp())
		if err != nil {
			return
		}
		switch st {
		case RailConfirmed:
			if err := k.store.FinalizeRailTransfer(ctx, k.cfg.FeeRecipientID, row.ID, fact.TxHash, time.Now().UTC()); err != nil {
				return
			}
			logger.Info("rail.confirmed", "rail_transfer_id", row.ID, "tx_hash", fact.TxHash)
		case RailFailed:
			// A debt that failed to pay is still a debt: the money stays committed rather than being
			// given back, and the payment is presented again. The attempt that reverted is spent —
			// asking under it again produces nothing at all — so the next one asks under a fresh
			// name while the row, the obligation and the seller's view of them stay as they were.
			// Only a withdrawal, which the user asked for and can ask for again, gives its money back.
			if row.Kind == RailKindObligation {
				_ = k.store.RetryRailTransfer(ctx, row.ID)
				logger.Warn("rail.obligation.retry", "rail_transfer_id", row.ID, "reverted_tx", fact.TxHash)
				return
			}
			if _, err := k.store.CompensateRailTransfer(ctx, k.cfg.FeeRecipientID, row.ID, time.Now().UTC()); err == nil {
				logger.Warn("rail.failed", "rail_transfer_id", row.ID)
			}
		}
	case RailStatusRefilling:
		fuel, err := k.store.ReadRailTransfer(ctx, row.RefillID)
		if err != nil {
			return
		}
		if fuel == nil || fuel.RefillID == "" {
			k.refillFor(ctx, row)
			return
		}
		cost, st, err := k.rail.RefillCost(ctx, fuel.RefillID)
		if err != nil {
			return
		}
		if st == RailPending {
			// A purchase can be durable and still not be on its way: the broadcast can fail after
			// the rail has recorded it, and the rail then refuses every later operation until it
			// resolves. Presenting it again is idempotent, so this either finishes the broadcast or
			// changes nothing while it waits for finality.
			k.refillFor(ctx, row)
			return
		}
		if err := k.store.BookRefill(ctx, k.cfg.FeeRecipientID, fuel.ID, cost, st == RailConfirmed, time.Now().UTC()); err != nil {
			return
		}
		logger.Info("rail.refill.booked", "refill_id", fuel.RefillID, "cost", cost)
		// The payment the refill made room for was never presented; present it now.
		_ = k.store.RecordRailOutcome(ctx, k.cfg.FeeRecipientID, row.ID, RailStatusPending, "", "", nil)
	}
}

// refillFor gets the fuel a payment is waiting on, keeping the books ahead of the rail throughout.
// The rail names a purchase only once it has made it, so its maximum cannot be locked first; what
// can be locked first is everything the operator could spend. That lock is taken before the rail is
// asked, so the reserve it is told to keep — what is not the operator's — holds until the purchase
// is known, and is then settled at the purchase's own maximum. A purchase a crash left unbound is
// adopted from what the rail holds rather than bought again; a lock that bought nothing is returned.
func (k *Kernel) refillFor(ctx context.Context, row *RailTransfer) {
	logger := k.log.With(ctx)
	sys := k.cfg.FeeRecipientID
	fuel, err := k.store.ReadRailTransfer(ctx, row.RefillID)
	if err != nil {
		return
	}
	pos, err := k.store.RailPosition(ctx, sys)
	if err != nil {
		return
	}
	reserve := pos.Reserve()
	if fuel == nil {
		if pos.SysAvailable <= 0 {
			_ = k.store.RecordRailOutcome(ctx, sys, row.ID, RailStatusBlocked, "", "the operator holds nothing to buy fuel with", nil)
			logger.Warn("rail.refill.failed", "rail_transfer_id", row.ID, "error", "operator balance is empty")
			return
		}
		fuel = &RailTransfer{ID: row.ID + ":fuel", Kind: RailKindRefill, Amount: pos.SysAvailable,
			Status: RailStatusPending, CreatedAt: time.Now().UTC()}
		if err := k.store.RecordRailOutcome(ctx, sys, row.ID, RailStatusRefilling, "", "", fuel); err != nil {
			logger.Warn("rail.refill.unlocked", "rail_transfer_id", row.ID, "error", err.Error())
			return
		}
	} else if fuel.RefillID == "" {
		// The lock was taken by an earlier pass: the reserve is what it was then, with the lock
		// itself counted as the operator's, since it is.
		reserve = RailPosition{Liabilities: pos.Liabilities, SysAvailable: pos.SysAvailable + fuel.Amount}.Reserve()
	}
	known := func(id string) bool {
		bound, err := k.store.ReadRailTransferByRefill(ctx, id)
		return err == nil && bound != nil
	}
	r, found, err := k.rail.FindRefill(ctx, known)
	if err != nil {
		return
	}
	if !found {
		if r, err = k.rail.Refill(ctx, reserve); err != nil {
			// Fuel the operator cannot buy stops outgoing money, and the halt is the blocked row:
			// without it the payment would sit refilling forever with nothing to show the operator.
			_ = k.store.ReleaseRefill(ctx, sys, fuel.ID)
			_ = k.store.RecordRailOutcome(ctx, sys, row.ID, RailStatusBlocked, "", err.Error(), nil)
			logger.Warn("rail.refill.failed", "rail_transfer_id", row.ID, "error", err.Error())
			return
		}
		if r.ID == "" {
			// The rail is busy with another operation; the fuel waits its turn, and the operator's
			// money is not held for a purchase nobody is making.
			if fuel.RefillID == "" {
				_ = k.store.ReleaseRefill(ctx, sys, fuel.ID)
			}
			return
		}
	}
	if err := k.store.BindRefill(ctx, sys, fuel.ID, r.ID, r.Max); err != nil {
		// The purchase exists whatever the books say; a lock the operator cannot fund is an alarm.
		logger.Error("rail.refill.unbound", "refill_id", r.ID, "max", r.Max, "error", err.Error())
		return
	}
	if fuel.RefillID != r.ID {
		logger.Info("rail.refill.recorded", "refill_id", r.ID, "max", r.Max)
	}
}

// scanRailDeposits books every finalized payment in, held. Who it belongs to is not decided here:
// reconciliation decides it, obligations first, because a peer's payment can arrive from an address
// a local account also registered and crediting that account on sight would leave the seller unpaid.
// Booking is keyed by the fact, so re-observing one moves nothing; the kernel's own mark advances
// only after a pass has booked what it saw.
func (k *Kernel) scanRailDeposits(ctx context.Context) {
	logger := k.log.With(ctx)
	from, _ := k.store.GetConfig(ctx, railBookedBlockKey)
	var since uint64
	if from != "" {
		_, _ = fmt.Sscanf(from, "%d", &since)
	}
	deposits, err := k.rail.ScanDeposits(ctx, since)
	if err != nil {
		logger.Warn("rail.scan.failed", "error", err.Error())
		return
	}
	var highest uint64 = since
	for _, d := range deposits {
		if _, err := k.store.CreateRailDeposit(ctx, k.cfg.FeeRecipientID, heldDeposit(d, ""), ""); err != nil {
			logger.Warn("rail.deposit.failed", "key", d.Key, "error", err.Error())
			return
		}
		if d.Block > highest {
			highest = d.Block
		}
	}
	if highest > since {
		_ = k.store.SetConfig(ctx, railBookedBlockKey, fmt.Sprintf("%d", highest))
	}
}

// railBookedBlockKey marks how far the kernel has booked, which is its own question and not the
// rail's: the rail records a payment before the kernel commits it.
const railBookedBlockKey = "rail_booked_block"

// reconcileDeposits decides who every payment observed so far belongs to. The store does the whole
// thing in one transaction, because the join that decides a payment is an obligation's — the sender
// the buyer proved, the transaction it named, the amount the draw decided — is the rule itself, and
// a rule applied in Go between two store calls is a rule a later caller can skip.
func (k *Kernel) reconcileDeposits(ctx context.Context) {
	closed, err := k.store.ReconcileDeposits(ctx, k.cfg.FeeRecipientID, 200)
	if err != nil {
		k.log.With(ctx).Warn("rail.reconcile_failed", "error", err)
		return
	}
	if len(closed) > 0 {
		k.log.With(ctx).Info("rail.reconciled", "count", len(closed))
	}
}

// peerRailAddress is where a peer account's kernel proved it is paid from — the sender a payment
// of theirs must carry. Empty when unknown, which matches nothing.
func (k *Kernel) peerRailAddress(ctx context.Context, accountID string) string {
	peer, err := k.store.ReadUser(ctx, accountID)
	if err != nil || peer == nil || peer.KernelPublicKey == "" {
		return ""
	}
	kern, err := k.store.ReadKernel(ctx, peer.KernelPublicKey)
	if err != nil || kern == nil || kern.RailAddress == "" {
		return ""
	}
	return kern.RailAddress
}

// RailHoldings is what the rail itself holds at a block that can no longer change — the only
// balance an audit may compare the books against.
type RailHoldings struct {
	Token int64  `json:"token"`
	Gas   string `json:"gas"`
	Block uint64 `json:"block"`
}

// RailReport is the operator's whole picture of external money (U44).
type RailReport struct {
	Network        Network       `json:"network"`
	Address        string        `json:"rail_address"`
	Finalized      *RailHoldings `json:"finalized,omitempty"`
	Position       RailPosition  `json:"position"`
	Gap            int64         `json:"gap"`
	Custody        int64         `json:"custody_difference"`
	CustodyChecked bool          `json:"custody_checked"`
	CustodyOK      bool          `json:"custody_ok"`
	StopReason     string        `json:"stop_reason,omitempty"`
	StopSince      string        `json:"stop_since,omitempty"`
}

// RailInspect assembles that picture for the operator. Every number here is display: the ledger is
// authoritative, and the rail's own reads are only what the audit compares it against.
func (k *Kernel) RailInspect(ctx context.Context, operatorID string) (*RailReport, error) {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return nil, err
	}
	return k.railReport(ctx)
}

// railReport is the audit itself, which the worker runs every pass and the operator reads on demand.
func (k *Kernel) railReport(ctx context.Context) (*RailReport, error) {
	pos, err := k.store.RailPosition(ctx, k.cfg.FeeRecipientID)
	if err != nil {
		return nil, err
	}
	rep := &RailReport{Network: k.cfg.Network, Position: *pos, Gap: pos.Gap()}
	if reason, since, err := k.RailStop(ctx); err == nil && reason != "" {
		rep.StopReason, rep.StopSince = reason, since.UTC().Format(time.RFC3339)
	}
	if k.rail == nil {
		return rep, nil
	}
	rep.Address = k.rail.Address()
	token, gas, block, ok, err := k.rail.FinalizedBalances(ctx)
	if err != nil || !ok {
		return rep, nil
	}
	rep.Finalized = &RailHoldings{Token: token, Gas: gas, Block: block}
	// A cut is only a cut if every payment up to it has been observed: a holding read ahead of the
	// scan would count money the books have not seen yet as money that vanished.
	scanned, ok, err := k.rail.DepositsScannedTo()
	if err != nil || !ok || scanned < block {
		return rep, nil
	}
	// Nor is it a cut while a payment is in flight: the holdings may already be without money the
	// books still hold, and the difference that produces is timing rather than loss.
	inFlight, err := k.store.ListRailTransfers(ctx, "", "", RailStatusSubmitted, 1, 0)
	if err != nil || len(inFlight) > 0 {
		return rep, nil
	}
	// The vault should match what is actually held, give or take a refill that has finalized but is
	// not yet booked. Anything else is money that left without a record.
	rep.Custody = pos.Vault - token
	rep.CustodyChecked = true
	rep.CustodyOK = rep.Custody >= 0 && rep.Custody <= pos.RefillLocks
	return rep, nil
}
