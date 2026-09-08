package kernel

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
)

// Economy is every money rule the kernel applies, in one immutable value injected at construction.
// Nothing here reads a store, a clock, or a config file: given the same numbers it always returns
// the same numbers, so the whole economy is testable as arithmetic and replaceable as a unit.
// Semantics are P7, P10 and D14; what is here is the arithmetic and why it is written this way.
type Economy struct {
	FeeBPS      int64 // domestic fee on a local layer's margin
	RemoteBPS   int64 // r: advertised markup for serving a foreign call
	ImportBPS   int64 // retained markup for importing one
	Lottery     int64 // L: ticket face value; 0 ⇒ every obligation is paid exactly
	LotteryMax  int64 // the world's ceiling on L, which no kernel here may quote above
	CreditLimit int64 // E_max: the ceiling on unpaid delivered service
}

// DefaultEconomy is the shipped money rules: a domestic fee of 20%, serving and import markups of
// 5% each, and no lottery — every obligation paid exactly, which is what a kernel with no rail
// configuration can honour. LotteryMax comes from the world and has no default.
func DefaultEconomy() Economy {
	return Economy{FeeBPS: 2000, RemoteBPS: 500, ImportBPS: 500}
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
