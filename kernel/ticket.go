package kernel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

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

// LotteryMax is the largest ticket this network permits, from its world file. A foreign call quoting
// more is refused: the face value is what its own draw pays.
func (k *Kernel) LotteryMax() int64 { return k.econ.LotteryMax }
