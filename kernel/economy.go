// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"time"

	"github.com/google/uuid"
)

// Economy is every money rule the kernel applies, in one immutable value injected at construction.
// Nothing here reads a store, a clock, or a config file: given the same numbers it always returns
// the same numbers, so the whole economy is testable as arithmetic and replaceable as a unit.
// Semantics are P7, P10 and D14; what is here is the arithmetic and why it is written this way.
type Economy struct {
	FeeBPS      int64 // domestic fee on a local layer's margin
	RemoteBPS   int64 // r: advertised markup for serving a foreign call
	ImportBPS   int64 // retained markup for importing one
	Lottery     int64 // L: this kernel's ticket face value; 0 ⇒ every obligation is paid exactly
	LotteryMax  int64 // the largest ticket this kernel will accept from a buyer
	CreditLimit int64 // E_max: the ceiling on unpaid delivered service
}

// DefaultEconomy is the shipped money rules, whole: a domestic fee of 20%, serving and import
// markups of 5% each, a ticket of one unit drawn against a maximum of five, and five hundred of
// unpaid delivered service. The two ticket figures are this kernel's own, not the world's — every
// shipped world counts in millionths, so one number means the same amount on all of them.
func DefaultEconomy() Economy {
	return Economy{FeeBPS: 2000, RemoteBPS: 500, ImportBPS: 500,
		Lottery: 1_000_000, LotteryMax: 5_000_000, CreditLimit: 500_000_000}
}

// Fee splits a taxable amount into what the provider keeps and what the operator takes.
// Invariant: net + fee == taxable.
func (e Economy) Fee(taxable int64) (net, fee int64) {
	fee = markup(taxable, e.FeeBPS)
	return taxable - fee, fee
}

// Premium is the serving kernel's markup on a charge; ImportFee what the origin retains on what it
// pays a peer. Both take their rate as a parameter rather than reading the field beside them: a
// settlement applies the rate frozen on its call's record, which a later config change must not move.
func (e Economy) Premium(charge, remoteBPS int64) int64 { return markup(charge, remoteBPS) }

func (e Economy) ImportFee(amount, importBPS int64) int64 { return markup(amount, importBPS) }

// RemoteSettlement is what one signed receipt obliges, at the rates its dispatch froze (P7): the
// obligation owed to the peer, the fee the origin retains on it, and whether the markup the seller
// wrote is the one those rates produce. Settlement and the offline audit both go through here, so
// what is committed and what verification expects agree by construction rather than by inspection.
// The rates are parameters, never the live fields beside them: a settlement applies the terms its
// own call was sold at, which a later config change must not move.
func (e Economy) RemoteSettlement(charge, premium, remoteBPS, importBPS int64, success bool) (obligation, importFee int64, premiumOK bool) {
	premiumOK = premium == e.Premium(charge, remoteBPS)
	obligation = charge + premium
	if success {
		importFee = e.ImportFee(obligation, importBPS)
	}
	return obligation, importFee, premiumOK
}

// ServingPrice is D_max: the most a foreign buyer can owe for one call at manifest price mp, at the
// seller's advertised rate. It is the quoted cross-kernel maximum, before the origin's import fee.
func (e Economy) ServingPrice(mp, remoteBPS int64) (int64, error) {
	return markUp(mp, remoteBPS)
}

// LocalPrice is q: what the buying kernel charges its own user, the serving price plus the import
// fee it retains. The caller can never be charged more than this.
func (e Economy) LocalPrice(servingPrice int64) (int64, error) {
	return markUp(servingPrice, e.ImportBPS)
}

// Stake is what a call holds from its immediate caller's own balance so a winning ticket is funded
// when it lands: the whole face value whenever a draw is possible at all.
//
// It does not depend on the advertised price. A call quoted at or above the face value still settles
// at whatever it actually charged, and a failure charges only what settled beneath it, so an
// obligation below the face value can arise from any call and win the whole of it. Sizing the stake
// from the quote would leave exactly those calls unable to pay.
func (e Economy) Stake(servingPrice int64) int64 {
	if servingPrice <= 0 {
		return 0 // a free call owes nothing to draw for
	}
	return e.Lottery // zero when no lottery is configured, and every obligation is paid exactly
}

// Draw decides what an obligation d pays under a lottery of face value L: 0, L, or d exactly.
//
// Each party supplies half the randomness — the buyer a secret committed to before the work, the
// seller a nonce minted after — so neither can pick the outcome, and both recompute this from the
// same three values and must agree. The modulo is biased by at most L/2^64, far below the whole-unit
// rounding d already carries, so rejection sampling would remove an error orders of magnitude under
// the one the model accepts.
func Draw(ticketID string, secret, nonce []byte, d, lottery int64) int64 {
	if d <= 0 {
		return 0
	}
	if lottery <= 0 || d >= lottery {
		return d
	}
	h := sha256.New()
	h.Write([]byte(ticketID))
	h.Write(secret)
	h.Write(nonce)
	if binary.BigEndian.Uint64(h.Sum(nil)[:8])%uint64(lottery) < uint64(d) {
		return lottery
	}
	return 0
}

// markup is ceil(base·bps/10000), the one rounding every rate here applies. It is computed as
// (base/10000)·bps + ceil((base%10000)·bps/10000) rather than base·bps so it never overflows:
// amounts are peer-supplied, and the naive product wraps into a negative fee on a large one.
func markup(base, bps int64) int64 {
	if base <= 0 || bps <= 0 {
		return 0
	}
	return (base/10000)*bps + ceilDiv((base%10000)*bps, 10000)
}

// markUp applies one layer: base + ceil(base·bps/10000). The result is a price rather than a slice
// of one, so it can overflow at the sum and reports that rather than wrapping.
func markUp(base, bps int64) (int64, error) {
	if base < 0 || bps < 0 || bps > 10000 {
		return 0, ErrInvalidInput.Wrapf("price %d and markup %d bps are out of range", base, bps)
	}
	m := markup(base, bps)
	if base > math.MaxInt64-m {
		return 0, ErrInvalidInput.Wrapf("marked-up price of %d at %d bps overflows", base, bps)
	}
	return base + m, nil
}

// ceilDiv returns ceil(a/b) using integer arithmetic.
func ceilDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}

// Owed is one cross-kernel obligation as the side that is owed sees it: a projection over the
// call's own records, never a row of its own. The trace froze the terms and carries the reveal; the
// receipt says what was charged; the idempotency record names it for the peer. A separate row could
// only fall out of step with them. The buyer's side is the mirror: its trace, its receipt, and the
// rail row carrying a winning payment.
type Owed struct {
	ID         string    `json:"id"` // the call's idempotency key — the name both kernels share
	PeerUserID string    `json:"-"`  // the buyer's account here
	UserID     string    `json:"-"`  // the provider who is owed
	TraceID    string    `json:"-"`
	Settled    bool      `json:"-"`          // the call has committed, so the obligation is known
	Obligation int64     `json:"obligation"` // charge plus premium from the receipt, once settled
	Terms      string    `json:"-"`          // the trace's frozen record (D19)
	Amount     int64     `json:"amount"`     // what the draw decided moves, set at the reveal
	TxHash     string    `json:"tx_hash"`    // the payment the buyer named
	RailAddr   string    `json:"-"`          // the buyer's proven sender, frozen at admission
	Status     string    `json:"status"`     // empty until the buyer reveals
	CreatedAt  time.Time `json:"created_at"`
}

// Reveal states, on the trace. Empty is the buyer not having said yet. The draw's outcome is read
// from Amount rather than stored: nothing is a losing draw, the obligation an exact one, anything
// else a win.
const (
	OwedAnnounced = "announced" // the buyer named a payment; the money has not arrived
	OwedCancelled = "cancelled" // terminal: the draw lost and nothing is owed
	OwedCredited  = "credited"  // terminal: the money arrived
)

// Outcome names what the draw decided, for a log line or an operator's reading.
func (r *Owed) Outcome() string {
	switch {
	case r.Amount == 0:
		return "cancel"
	case r.Amount == r.Obligation:
		return "exact"
	default:
		return "pay"
	}
}

// PendingReveal is one call whose seller has still to be told how its draw came out, assembled by
// the store from the records that already hold it.
type PendingReveal struct {
	ID      string // the call's idempotency key
	TraceID string
	PeerKey string
	Secret  string
	TxHash  string // empty for a losing draw, which owes no payment
}

// newSecret mints 32 bytes of randomness as hex. A party's half of the draw rides on the trace's
// frozen record, so a restart mid-flight still holds it and it is never stored twice.
func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", ErrInternal.Wrap("could not generate randomness")
	}
	return hex.EncodeToString(b), nil
}

// commitmentOf is the hash a buyer publishes before the work, binding it to a secret it cannot
// change afterwards. Empty for a call that owes nothing.
func commitmentOf(secret string) string {
	b, err := hex.DecodeString(secret)
	if secret == "" || err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mustHex decodes hex, yielding nil on garbage — which then fails the commitment check.
func mustHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

// prepareDispatch readies a trace to buy across a kernel boundary: the key both sides will name the
// obligation by, the buyer's half of the draw, and the stake that funds a winning ticket. Every
// pricing input is frozen on the record here, so a retry after a restart draws with exactly the
// values the peer was committed to (P10, D19). It refuses first if the obligation could not be paid.
func (k *Kernel) prepareDispatch(ctx context.Context, t *Trace, a *Action, args map[string]any, stepID string, gross, importBPS int64) error {
	mp, rbps := actionBasePrice(a), actionRemoteBPS(a)
	dmax, err := k.econ.ServingPrice(mp, rbps)
	if err != nil {
		return err
	}
	if err := k.payableOnThisRail(ctx, a.OwnerUserID, dmax); err != nil {
		return err
	}
	// A call that can owe nothing has nothing to draw for, and carries no ticket terms (P4).
	var secret string
	lottery := k.econ.Lottery
	if dmax > 0 {
		if secret, err = newSecret(); err != nil {
			return err
		}
	} else {
		lottery = 0
	}
	key := uuid.New().String()
	t.IdempotencyKey = &key
	t.Ticket = k.econ.Stake(dmax)
	t.DispatchJSON = marshalDispatch(args, stepID, mp, gross, a.ArtifactHash, rbps, importBPS, secret, lottery)
	return nil
}

// payableOnThisRail refuses a paid cross-kernel call whose obligation this kernel could not pay: a
// peer whose address is unknown or unproven could never collect, and taking on the debt anyway would
// leave the seller owed with no way to be paid. A free call owes nothing and is always allowed,
// which is what keeps a cold resolve and a price-0 action working before any address is known.
func (k *Kernel) payableOnThisRail(ctx context.Context, peerAccountID string, obligation int64) error {
	if obligation <= 0 || k.rail == nil || k.rail.Address() == "" {
		return nil // nothing owed, or a world with no addresses: the operator's record is the payment
	}
	if k.peerRailAddress(ctx, peerAccountID) != "" {
		return nil
	}
	peer, _ := k.store.ReadUser(ctx, peerAccountID)
	name := peerAccountID
	if peer != nil && peer.KernelPublicKey != "" {
		name = k.KernelName(ctx, peer.KernelPublicKey)
	}
	return ErrPeerUnreachable.Wrapf("%s has not proved where it is paid, so this call could not be settled", name).
		WithMeta("peer", name)
}

// BuyerTerms is what a buyer fixes on a cross-kernel call before the work is done (P4, P10): its
// commitment to the draw, the face value it dispatches under, and where a winning ticket will be
// paid from. The address is proven and frozen at admission rather than learned later, so the seller
// knows every obligation's payer before any payment can land — a payment that arrives ahead of its
// reveal is then never mistaken for somebody else's.
type BuyerTerms struct {
	Commitment  string
	Lottery     int64
	RailAddress string
	RailProof   string
}

// RevealPayload is what a buyer signs to tell the seller how a draw came out (P10). The secret makes
// the outcome checkable — the seller holds its commitment already — and the recipient binds the
// message to one kernel, so a captured reveal cannot be replayed at a third.
type RevealPayload struct {
	Counterparty string `json:"counterparty"`
	Recipient    string `json:"recipient"`
	Secret       string `json:"secret"`
	TicketID     string `json:"ticket_id"`
	Timestamp    string `json:"timestamp"`
	TxHash       string `json:"tx_hash,omitempty"`
}

// HandleReveal is the seller's side: the buyer says how the draw came out, and this checks it.
// Everything is recomputed rather than believed, so a buyer cannot report a loss on a ticket that
// won; a repeat of a reveal already applied returns the stored row, which makes a lost reply safe
// to resend.
func (k *Kernel) HandleReveal(ctx context.Context, peerKey string, p RevealPayload, signature string) (*Owed, error) {
	peer, err := k.store.ReadAccountByKernelKey(ctx, peerKey)
	if err != nil {
		return nil, err
	}
	if peer == nil {
		return nil, ErrNotFound.Wrap("no obligation for this peer")
	}
	r, err := k.store.ReadOwed(ctx, p.TicketID, peer.ID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrNotFound.Wrapf("obligation %s not found", p.TicketID)
	}
	pub, err := decodeRemotePublicKey(peerKey)
	if err != nil {
		return nil, err
	}
	if err := k.cfg.Network.verify(pub, sigDomainReveal, p, signature); err != nil {
		return nil, err
	}
	if !r.Settled {
		return nil, ErrInvalidState.Wrapf("obligation %s has not settled yet", p.TicketID)
	}
	// A reveal that repeats what was already applied is answered from the record: the buyer may be
	// resending because our reply was lost, and re-deciding could contradict what we already said.
	if r.Status != "" {
		return r, nil
	}
	// The draw runs on the terms the call was admitted under, frozen on its trace (D19).
	_, lottery, nonce := ServingTerms(&r.Terms)
	if commitmentOf(p.Secret) != dispatched(&r.Terms).Commitment {
		return nil, ErrUnauthorized.Wrap("the secret does not match the commitment")
	}
	amount := Draw(r.ID, mustHex(p.Secret), mustHex(nonce), r.Obligation, lottery)
	// A paying draw must name its payment; where it comes from was proven and frozen when the call
	// was admitted, so an address change afterwards cannot retarget a payment this obligation is
	// already waiting for. A losing draw owes nothing and names nothing, and is accepted however
	// late it arrives: a deadline past which a loss became a win would turn any outage into a
	// payment neither party's draw called for.
	if amount > 0 && p.TxHash == "" {
		return nil, ErrInvalidInput.Wrap("a paying reveal must name its payment")
	}
	// Where the world has no addresses there is nothing outside to observe, so this signed reveal is
	// itself the finalized payment (D23): it is recorded with the reveal that names it, and the
	// ordinary reconciliation then closes the obligation exactly as it closes a scanned one.
	var payment *RailTransfer
	if amount > 0 && k.rail != nil && k.rail.Address() == "" {
		fact, ferr := k.rail.Witness(ctx, p.TxHash, amount)
		if ferr != nil {
			return nil, ferr
		}
		payment = heldDeposit(fact, "obligation "+r.ID)
	}
	if err := k.store.ApplyReveal(ctx, k.cfg.FeeRecipientID, r.TraceID, amount, p.TxHash, payment); err != nil {
		return nil, err
	}
	if payment != nil {
		k.reconcileDeposits(ctx)
	}
	r.Amount, r.TxHash = amount, p.TxHash
	r.Status = OwedAnnounced
	if amount == 0 {
		r.Status, r.TxHash = OwedCancelled, ""
	}
	k.log.With(ctx).Info("owed."+r.Status, "obligation_id", r.ID, "owed", r.Obligation, "amount", amount)
	return r, nil
}

// RevealPending tells every seller still waiting how its obligation came out, and keeps telling them
// until each has heard: a losing draw the seller never learns about leaves it owed forever. The
// store returns only the calls revealable now, so nothing blocks the queue behind it.
func (k *Kernel) RevealPending(ctx context.Context) {
	pending, err := k.store.ListPendingReveals(ctx, 200)
	if err != nil {
		return
	}
	for _, d := range pending {
		if err := k.Reveal(ctx, d); err != nil {
			k.log.With(ctx).Warn("owed.reveal_failed", "obligation_id", d.ID, "error", err)
		}
	}
}

// Reveal tells one seller how our own draw came out, and marks the call told. The rail worker drives
// it, so a reveal lost to a dead connection is simply sent again from the same records.
func (k *Kernel) Reveal(ctx context.Context, d *PendingReveal) error {
	fe := k.fedClient
	if fe == nil {
		return ErrInvalidState.Wrap("federation client is not configured")
	}
	p := RevealPayload{
		Counterparty: k.ourKeyB64(),
		Recipient:    d.PeerKey,
		Secret:       d.Secret,
		TicketID:     d.ID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		TxHash:       d.TxHash,
	}
	sig, err := k.cfg.Network.sign(k.cfg.SigningKey, sigDomainReveal, p)
	if err != nil {
		return err
	}
	if err := fe.Reveal(ctx, d.PeerKey, p, sig); err != nil {
		return err
	}
	return k.store.MarkRevealed(ctx, d.TraceID)
}

// Economy is the money rules this kernel runs under, for the operator's own view of them.
func (k *Kernel) Economy() Economy { return k.econ }

// Exposure is what this kernel has delivered to foreign buyers and not been paid for (D14).
func (k *Kernel) Exposure(ctx context.Context, operatorID string) (int64, error) {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return 0, err
	}
	return k.store.Exposure(ctx)
}
