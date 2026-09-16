// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// ReceiptHash is SHA-256(CanonicalJSON(the full stored receipt, signature included)) hex — the ONE
// definition of a receipt's portable identity, used on both sides of the remote-receipt evidence
// join (§13). A rating hashes its receipt with this; an EvidenceReceipt's RemoteReceiptHash uses it;
// a receiver joins A's RemoteReceiptHash to B's ReceiptHash byte-for-byte. It must never be conflated
// with tx.RemoteReceiptHash, which hashes the RAW wire bytes for settlement-integrity and would not
// match a re-canonicalized hash (Go marshal order ≠ JCS order).
func ReceiptHash(r *Receipt) (string, error) {
	b, err := CanonicalJSON(r)
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize receipt: %v", err)
	}
	return sha256Hex(string(b)), nil
}

// receiptHashFromJSON canonicalizes a stored receipt JSON string and hashes it, giving the same
// value receiptHash would for the parsed receipt — used to hash a stored remote receipt (§13).
func receiptHashFromJSON(receiptJSON string) (string, error) {
	if receiptJSON == "" {
		return "", nil
	}
	var r Receipt
	if err := json.Unmarshal([]byte(receiptJSON), &r); err != nil {
		return "", ErrInternal.Wrapf("decode receipt: %v", err)
	}
	return ReceiptHash(&r)
}

// actionBasePrice is the seller's manifest price (mp) snapshotted on a proxy row. Every funding
// boundary heals the snapshot first (ensureBasePrice), so mp is never reverse-calculated from the
// rounded local total, which cannot recover it exactly.
func actionBasePrice(a *Action) int64 {
	if a.BasePrice != nil {
		return *a.BasePrice
	}
	return a.Price
}

// TransferEffect is the staged value channel of an effect-bearing action (§13): the delivered Amount
// and the resolved beneficiary Dest. It is produced only for an action whose contract declares an
// effect, and settled deferredly by the kernel at commit; the two money channels — the execution price,
// funded by the trace, and this value, funded from the immediate caller C's own balance — never mix.
type TransferEffect struct {
	Amount int64
	Dest   string
}

// prepareTransferEffect stages the value channel for a call at funding time (§13), or returns nil when
// the action declares no transfer effect. Effect identity comes from the action's contract (actionValue
// keys on a.Effect), never its name.
//
// The value channel is local to one kernel: caller, action, and beneficiary are all accounts here, the
// amount locked from C is exactly the amount delivered, and no fee is levied on it. Value between
// kernels settles on the external rail, not through a call. callerIsPeer is therefore a rejection
// rather than a pricing input: a peer completing a parked step is the one path by which a kernel
// account could otherwise reach an effect-bearing action, since completion re-checks liveness but not
// visibility (§4 binding rule).
//
// It rejects, before any funds move, a peer caller, a non-positive amount, a kernel-qualified target,
// and an unresolvable / peer / suspended beneficiary.
func (k *Kernel) prepareTransferEffect(ctx context.Context, callerIsPeer bool, a *Action, args map[string]any) (*TransferEffect, error) {
	fn := k.actionValue(a)
	if fn == nil {
		return nil, nil
	}
	if callerIsPeer {
		return nil, ErrInvalidInput.Wrap("value transfer is local to a kernel; a peer cannot fund one")
	}
	amount, ref, err := fn(args)
	if err != nil {
		return nil, err
	}
	if amount < 1 {
		return nil, ErrInvalidInput.Wrap("transfer amount must be a positive integer")
	}
	if strings.Contains(ref, "@") {
		return nil, ErrInvalidInput.Wrap("value transfer is local to a kernel; the target must be a bare local handle")
	}
	benef, err := k.ResolveUser(ctx, ref)
	if err != nil || benef == nil {
		return nil, ErrNotFound.Wrapf("transfer beneficiary %q not found", ref)
	}
	if !benef.IsLiveUser() {
		return nil, ErrInvalidInput.Wrap("transfer beneficiary must be a local user account")
	}
	if benef.SuspendedAt != nil {
		return nil, ErrInvalidInput.Wrap("transfer beneficiary is suspended")
	}
	return &TransferEffect{Amount: amount, Dest: benef.ID}, nil
}

// dispatchPayload is the persisted remote-proxy dispatch record, stored on Trace.DispatchJSON
// so a pending remote call can be replayed verbatim by RetryPendingRemoteDispatches after restart.
type dispatchPayload struct {
	Args        map[string]any `json:"args"`
	StepID      string         `json:"step_id"`
	RemotePrice int64          `json:"remote_price"`
	// Gross is the funded local price this dispatch locked. Retry after restart reconstructs the
	// locked amount from it rather than from action.Price, which is derived live from the current
	// import fee and so would move under a policy change (§16 price snapshot).
	Gross int64 `json:"gross,omitempty"`
	// ContractHash is the expected_contract_hash this dispatch bound (§8 If-Match). Read back at
	// settlement to key the hash-conditional proxy deactivation, so a stale dispatch settling after a
	// re-resolve never deactivates the refreshed row (§13).
	ContractHash string `json:"contract_hash,omitempty"`
	// RemoteBPS/ImportBPS are the rates this dispatch was funded under (§13). The funding boundary
	// freezes every pricing input, so settlement, retry, refund, and audit read them here and never
	// from the action row or live config — a catalog price floats with local policy, and a call
	// locked at one rate must settle at that rate.
	RemoteBPS int64 `json:"remote_bps"`
	ImportBPS int64 `json:"import_bps"`
	// Secret is this call's half of the settlement draw and Lottery the face value it was dispatched
	// under (P10). Both are frozen here for the same reason as the rates: a retry after restart must
	// draw with the values the peer was committed to, not with whatever the config now says.
	Secret  string `json:"secret,omitempty"`
	Lottery int64  `json:"lottery,omitempty"`
	// ServingBPS and Nonce are the other direction: what this kernel admitted a foreign call under —
	// the markup it quoted and its own half of the draw, minted before the buyer's secret is known.
	// They are frozen for the same reason and read back the same way, so a settlement that rebuilds
	// the call from nothing — crash recovery, forced closure — signs the receipt the buyer was
	// promised. The two sets are disjoint: a trace is either buying or selling, never both, so
	// ServingBPS is 0 on a dispatch and RemoteBPS is 0 on an admission.
	ServingBPS int64  `json:"serving_bps,omitempty"`
	Nonce      string `json:"nonce,omitempty"`
	// Commitment is the buyer's hash of its own secret, and Reserve the most the call could owe —
	// what admission counted against the credit limit, corrected at commit to what was charged.
	Commitment string `json:"commitment,omitempty"`
	Reserve    int64  `json:"reserve,omitempty"`
}

// marshalServing freezes what an inbound foreign call was admitted under, on the same trace record
// that freezes an outbound one's terms (D19). With the reveal fields the trace itself carries, this
// is the whole of the seller's ticket: there is no second record to keep in step with it.
func marshalServing(remoteBPS, lottery, reserve int64, nonce, commitment string) *string {
	b, _ := json.Marshal(dispatchPayload{ServingBPS: remoteBPS, Lottery: lottery, Reserve: reserve,
		Nonce: nonce, Commitment: commitment})
	out := string(b)
	return &out
}

// actionRemoteBPS is the peer's signed serving markup snapshotted on a proxy row; 0 for a pre-v0.12
// row that never captured one.
func actionRemoteBPS(a *Action) int64 {
	if a == nil || a.RemoteBPS == nil {
		return 0
	}
	return *a.RemoteBPS
}

// pricedStore is the one place a proxy's local price becomes a number.
//
// A remote proxy stores the SELLER's price (§16 Price Snapshot Pattern); the local total is derived
// from it and the current import fee, so changing local policy reprices the catalog with no
// re-resolve. Deriving it at each call site does not work: the kernel reads actions from ~40 places
// and any one of them forgetting would serve a stale price on a money path. So the derivation is
// attached to the store handle itself, at construction, and every read — present and future — gets
// it for free.
//
// Writes are unaffected. For a non-proxy the derivation is identity, and the sole proxy write path
// (importRemoteActionCore) assigns Price unconditionally from the manifest, so a derived value can
// never round-trip into storage.
//
// A derivation that overflows is returned as an error rather than silently falling back to the
// stored total: these rows fund calls.
type pricedStore struct {
	Store
	econ Economy
}

// price derives one action's local total in place.
func (s *pricedStore) price(a *Action) (*Action, error) {
	if a == nil || a.Kind != KindRemoteProxy || a.BasePrice == nil {
		return a, nil // non-proxy, or a pre-041 row whose stored total is still what it charges
	}
	sr, err := s.econ.ServingPrice(*a.BasePrice, actionRemoteBPS(a))
	if err != nil {
		return nil, err
	}
	q, err := s.econ.LocalPrice(sr)
	if err != nil {
		return nil, err
	}
	a.Price = q
	return a, nil
}

func (s *pricedStore) priceOne(a *Action, err error) (*Action, error) {
	if err != nil {
		return a, err
	}
	return s.price(a)
}

func (s *pricedStore) priceMany(as []*Action, err error) ([]*Action, error) {
	if err != nil {
		return as, err
	}
	for _, a := range as {
		if _, perr := s.price(a); perr != nil {
			return nil, perr
		}
	}
	return as, nil
}

func (s *pricedStore) ReadAction(ctx context.Context, id string) (*Action, error) {
	return s.priceOne(s.Store.ReadAction(ctx, id))
}

func (s *pricedStore) ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*Action, error) {
	return s.priceOne(s.Store.ReadActionByOwnerName(ctx, ownerID, name))
}

func (s *pricedStore) ReadActionByOwnerRemoteID(ctx context.Context, ownerID, remoteActionID string) (*Action, error) {
	return s.priceOne(s.Store.ReadActionByOwnerRemoteID(ctx, ownerID, remoteActionID))
}

func (s *pricedStore) ListVisibleActions(ctx context.Context, includeLocal bool, limit, offset int) ([]*Action, error) {
	return s.priceMany(s.Store.ListVisibleActions(ctx, includeLocal, limit, offset))
}

func (s *pricedStore) ListActionsByOwner(ctx context.Context, ownerID string, limit, offset int) ([]*Action, error) {
	return s.priceMany(s.Store.ListActionsByOwner(ctx, ownerID, limit, offset))
}

func (s *pricedStore) ListAllActions(ctx context.Context, limit, offset int) ([]*Action, error) {
	return s.priceMany(s.Store.ListAllActions(ctx, limit, offset))
}

// marshalDispatch serializes a dispatchPayload and returns a pointer suitable for Trace.DispatchJSON.
func marshalDispatch(args map[string]any, stepID string, mp, gross int64, contractHash string, remoteBPS, importBPS int64, secret string, lottery int64) *string {
	b, _ := json.Marshal(dispatchPayload{
		Args: args, StepID: stepID, RemotePrice: mp, Gross: gross, ContractHash: contractHash,
		RemoteBPS: remoteBPS, ImportBPS: importBPS, Secret: secret, Lottery: lottery,
	})
	s := string(b)
	return &s
}

// dispatched reads what a call was funded under, frozen on its dispatch record (§13 price-snapshot):
// settlement and audit never read live config, so a change between dispatch and settlement — of a
// fee rate, or of the lottery — cannot move this call's arithmetic. Every dispatched trace carries a
// full record, since the upgrade that introduced this refused to run while one did not (migration
// 049); a trace with none has not been dispatched, and the zero values it yields are never read.
func dispatched(dispatchJSON *string) (d dispatchPayload) {
	if dispatchJSON != nil {
		_ = json.Unmarshal([]byte(*dispatchJSON), &d)
	}
	return
}

// DispatchSecret reads the buyer's half of the settlement draw out of a stored dispatch record. The
// store assembles pending reveals from the rows that already hold them, and this is the one field it
// cannot read as a column (P10).
func DispatchSecret(dispatchJSON string) string { return dispatched(&dispatchJSON).Secret }

// ServingTerms reads back what a foreign call was admitted under: the markup it was quoted at, this
// kernel's half of its draw, and the face value the buyer named. Every settlement path reads them
// here, so none of them can sign a receipt at a rate the call was never sold at (D19, P10).
func ServingTerms(dispatchJSON *string) (remoteBPS, lottery int64, nonce string) {
	d := dispatched(dispatchJSON)
	return d.ServingBPS, d.Lottery, d.Nonce
}

// ServingReserve is what a foreign call's admission counted against the credit limit, and whether
// the trace is a foreign call at all — a local one froze no nonce and reserved nothing.
func ServingReserve(dispatchJSON *string) (reserve int64, foreign bool) {
	d := dispatched(dispatchJSON)
	return d.Reserve, d.Nonce != ""
}

// PeerStepView is what a remote peer may see of a step parked for it: the request, not the
// requester. Deliberately NOT the local step view — that one carries the creating action's name,
// the process owner's handle, and raw local ids, and a user identity crossing a kernel boundary is
// precisely what §13's encapsulation forbids. Each field is here because the completer needs it:
// partial_args is the payload channel (§14 has sys/message put its body there) and allowed_input is
// §14's substitute for reading a target action that may be private. One type serves both ends of the
// protocol — the serving kernel builds it, the buying kernel decodes it — so neither side can drift.
type PeerStepView struct {
	ID string `json:"id"`
	// RequiredCaller names which principal on the RECEIVING kernel the step is addressed to, when
	// it is addressed to one of its users rather than to the kernel itself. It is that kernel's own
	// id, so it discloses nothing of the parking kernel: it lets the receiver route the step to the
	// user who may complete it, which completion already demands (step_auth).
	RequiredCaller string          `json:"required_caller,omitempty"`
	PartialArgs    json.RawMessage `json:"partial_args,omitempty"`
	AllowedInput   map[string]any  `json:"allowed_input,omitempty"`
	Price          int64           `json:"price"`
	CreatedAt      time.Time       `json:"created_at"`
}

// PeerStepList is one page of a peer's answer: the steps, and whether more are waiting than the
// page could carry (P8) — a bounded page with no continuation, so the flag is the whole signal.
type PeerStepList struct {
	Steps     []PeerStepView `json:"steps"`
	Truncated bool           `json:"truncated,omitempty"`
}

// NewPeerStepView projects one waiting step into the peer-facing shape (§13).
func (k *Kernel) NewPeerStepView(s *Step, action *Action) *PeerStepView {
	v := &PeerStepView{ID: s.ID, PartialArgs: s.PartialArgs, Price: s.Price, CreatedAt: s.CreatedAt}
	if s.RequiredCallerRemoteID != nil {
		v.RequiredCaller = *s.RequiredCallerRemoteID
	}
	if action != nil {
		v.AllowedInput = DeriveAllowedSchema(action.InputSchema, s.PartialArgs)
	}
	return v
}

// callerWalletFor returns the (walletID, walletKind) that funds a call:
//   - step completion → CallerStep (no wallet id; BeginStepCall already released the lock)
//   - subcall (has parent trace) → CallerTrace
//   - root call (no parent trace) → CallerProcess
func callerWalletFor(stepID, processID string, parentTraceID *string) (id, kind string) {
	switch {
	case stepID != "":
		return "", CallerStep
	case parentTraceID != nil:
		return *parentTraceID, CallerTrace
	default:
		return processID, CallerProcess
	}
}

// Signature domains (§12). Every signed Juice payload is bound to exactly one domain, so a
// signature valid in one domain is rejected in every other — disjointness is now constructive
// (an explicit per-domain prefix) rather than emergent from disjoint JCS key-sets. The prefix is
// versioned so a future domain scheme can coexist. The transport-handshake domain is libp2p's
// own and is disjoint from all of these by construction.
const (
	sigDomainReceipt         = "receipt"
	sigDomainRating          = "rating"
	sigDomainManifest        = "manifest"
	sigDomainEvidenceReceipt = "evidence_receipt"
	sigDomainFedCall         = "fed_call"
	sigDomainStepComplete    = "step_complete"
	sigDomainStepList        = "step_list"
	sigDomainStepAuth        = "step_auth"
	sigDomainReveal          = "reveal"
	sigDomainCapability      = "capability"
	sigDomainRecovery        = "recovery"
)

// Network identifies the one network a kernel belongs to for life (D23). Digest is the SHA-256 of
// the JCS of the world file's defining part; it rides inside every signature prefix, so no artifact
// of one network can verify on another. Name and Decimals are display only.
type Network struct {
	Name     string `json:"name"`
	Digest   string `json:"digest"`
	Decimals uint8  `json:"decimals"`
	Symbol   string `json:"symbol"`
	// Token is the contract money is paid in, empty where the world has no chain. A symbol names
	// no token — a chain carries several with one name and one decimals — so the address is what a
	// depositor must be told, and it travels beside the symbol rather than being fetched separately.
	Token string `json:"token"`
}

// Amount renders base units the way a person reads them, and is the only place that decides how:
// a surface that takes 1.50 and answers 1500000 has two units under one name. The shape is the
// ledger convention — the currency's minor unit is the floor, so a round amount reads 1.50 and not
// 1.500000, while extra digits are kept, because rounding money for display is a lie the
// reconciliation will find. No separators: this text is pasted back into parseAmount.
func (n Network) Amount(v int64) string {
	unit := n.Symbol
	if unit != "" {
		unit = " " + unit
	}
	if n.Decimals == 0 {
		return strconv.FormatInt(v, 10) + unit
	}
	s := strconv.FormatInt(v, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	for len(s) <= int(n.Decimals) {
		s = "0" + s
	}
	whole, frac := s[:len(s)-int(n.Decimals)], s[len(s)-int(n.Decimals):]
	for len(frac) > 2 && strings.HasSuffix(frac, "0") {
		frac = frac[:len(frac)-1]
	}
	return sign + whole + "." + frac + unit
}

// DiscoveryNamespace is the rendezvous string kernels of one network advertise and enumerate. It
// carries the network digest, so worlds cannot meet even when they share a bootstrap node (D23).
func DiscoveryNamespace(n Network) string { return "juice/fed/discovery/1/" + n.Digest }

// payload prepends the versioned network-and-domain tag to the JCS-canonical bytes of v, giving the
// exact byte string signed and verified under domain on this network.
func (n Network) payload(domain string, v any) ([]byte, error) {
	canon, err := CanonicalJSON(v)
	if err != nil {
		return nil, ErrInternal.Wrapf("canonicalize: %v", err)
	}
	prefix := []byte("juice/v1/" + n.Digest + "/" + domain + "\n")
	return append(prefix, canon...), nil
}

// sign signs the prefixed JCS-canonical form of v with key.
func (n Network) sign(key ed25519.PrivateKey, domain string, v any) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	payload, err := n.payload(domain, v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}

// verify checks that sigB64 is a valid Ed25519 signature over the prefixed JCS-canonical form of v.
// One rule serves wire and storage alike: an artifact signed before this network's digest existed is
// reported invalid rather than repaired (U36).
func (n Network) verify(pub ed25519.PublicKey, domain string, v any, sigB64 string) error {
	payload, err := n.payload(domain, v)
	if err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !ed25519.Verify(pub, payload, sig) {
		return ErrUnauthorized.Wrap("signature is invalid")
	}
	return nil
}

// VerifyReceipt audits the signed receipt behind one transaction, offline, against what this
// kernel stored (§11, U36). Available to any party satisfying CanReadTransaction. Every committed
// call has a receipt, so both kinds are answerable: a call this kernel executed is audited against
// its own key, and one a peer executed against the peer's, with the extra facts only a cross-kernel
// call has — the frozen rates, the settlement arithmetic and the draw (P7, P10). Checks that do not
// apply are absent rather than silently true; a signature this network does not accept is reported
// invalid, never re-signed.
func (k *Kernel) VerifyReceipt(ctx context.Context, subjectID, txID string) (*ReceiptVerification, error) {
	tv, err := k.ReadTransaction(ctx, subjectID, txID)
	if err != nil {
		return nil, err
	}
	tx := tv.Transaction
	if tx.RemoteReceiptJSON == "" {
		return k.verifyLocalReceipt(ctx, tx)
	}

	var r Receipt
	if err := json.Unmarshal([]byte(tx.RemoteReceiptJSON), &r); err != nil {
		return nil, ErrInternal.Wrapf("decode remote receipt: %v", err)
	}

	// The rates and the secret come from the dispatch record, which froze them at the funding
	// boundary (§13): the action row's markup and local config both move, so auditing against them
	// would fail a historically-correct settlement after any rate change.
	tr, err := k.store.ReadTrace(ctx, tx.TraceID)
	if err != nil {
		return nil, err
	}
	if tr == nil {
		return nil, ErrNotFound.Wrap("the transaction's trace is missing")
	}
	d := dispatched(tr.DispatchJSON)
	var obligationID string
	if tr.IdempotencyKey != nil {
		obligationID = *tr.IdempotencyKey
	}
	obligation, importFee, arithOK := k.econ.RemoteSettlement(r.Charge, r.Premium, d.RemoteBPS, d.ImportBPS, tx.Status == TxSuccess)

	checks := ReceiptChecks{
		// Hash integrity, then the signature under this kernel's own network prefix.
		"receipt_hash": sha256Hex(tx.RemoteReceiptJSON) == tx.RemoteReceiptHash,
		"signature":    k.cfg.Network.verifyReceiptSignature(&r, tx.RemoteSignerKey) == nil,
		"action_id":    r.ActionID == firstNonEmpty(tx.RemoteActionID, tx.ActionID),
		"status":       r.Status == tx.Status,
		// The execution obligation the buyer owes, and the serving markup inside it. The value
		// channel settles on the caller's own reserve, never through tx.net (§13).
		"charge":  tx.Net == obligation,
		"premium": arithOK,
		// The origin's import fee on the actual obligation, and the ceiling the caller authenticated
		// and locked before dispatch — it can never be charged more than that.
		"settlement_arith":    tx.Fee == importFee,
		"charge_ceiling":      obligation <= tx.Gross,
		"refund_conservation": tx.Refund == tx.Gross-tx.Net-tx.Fee,
		"args_hash":           receiptHashMatches(r.ArgsHash, tx.ArgsJSON),
		// What was actually paid must be what the two halves of the randomness decide (P10). The
		// audit re-derives the outcome from the secret this call froze and the nonce the peer signed,
		// reading nothing back from a ledger of its own. A call that owed nothing has nothing to
		// check; one that owed something and has no ticket to draw with fails, never passes.
		"draw": obligation == 0,
	}
	if obligation > 0 && obligationID != "" {
		paid := int64(0)
		if p, perr := k.store.ReadRailTransfer(ctx, obligationID); perr == nil && p != nil {
			paid = p.Amount
		}
		checks["draw"] = paid == Draw(obligationID, mustHex(d.Secret), mustHex(r.Nonce), obligation, d.Lottery)
	}
	if tx.Status == TxSuccess {
		checks["reply_hash"] = receiptHashMatches(r.ReplyHash, tx.ReplyJSON)
	}

	return &ReceiptVerification{
		TransactionID:         txID,
		Valid:                 checks.allHeld(),
		RemoteKernelHandle:    k.KernelName(ctx, tx.RemoteSignerKey),
		RemoteKernelPublicKey: tx.RemoteSignerKey,
		Checks:                checks,
		Receipt:               &r,
	}, nil
}

// verifyLocalReceipt audits the receipt this kernel signed for a call it executed itself, against
// its own key. Same question, one issuer: there is no peer key, no frozen cross-kernel rate and no
// draw, so those checks do not appear.
func (k *Kernel) verifyLocalReceipt(ctx context.Context, tx *Transaction) (*ReceiptVerification, error) {
	r, err := k.store.ReadReceiptByTxID(ctx, tx.ID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrNotFound.Wrap("transaction has no receipt")
	}
	checks := ReceiptChecks{
		"signature": k.cfg.Network.verifyReceiptSignature(r, k.ourKeyB64()) == nil,
		"action_id": r.ActionID == tx.ActionID,
		"status":    r.Status == tx.Status,
		// P5: charge is the full allocation on success, and what settled beneath it on a failure;
		// the signed split must be the row's, or the row has been moved under the signature.
		"charge":     r.Charge == tx.Gross-tx.Refund,
		"settlement": r.Gross == tx.Gross && r.Net == tx.Net && r.Fee == tx.Fee,
		"args_hash":  receiptHashMatches(r.ArgsHash, tx.ArgsJSON),
	}
	if tx.Status == TxSuccess {
		checks["reply_hash"] = receiptHashMatches(r.ReplyHash, tx.ReplyJSON)
	}
	return &ReceiptVerification{TransactionID: tx.ID, Valid: checks.allHeld(), Checks: checks, Receipt: r}, nil
}

// receiptHashMatches compares a receipt's hash of a payload against the payload this kernel stored.
func receiptHashMatches(want string, payload json.RawMessage) bool {
	h, err := jcsHashStr(string(payload))
	return err == nil && want == h
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ReceiptSigningBytes returns the exact domain-prefixed bytes a receipt signature covers (§12) — the
// serving kernel signs these; the origin verifies them. Exposed so an external signer (or a test)
// reconstructs the identical payload.
func (n Network) ReceiptSigningBytes(r *Receipt) ([]byte, error) {
	cp := *r
	cp.Signature = ""
	return n.payload(sigDomainReceipt, cp)
}

// verifyReceiptSignature checks the receipt's Ed25519 signature against pubKeyB64. Fails
// closed on an empty key: a missing key must never let an unverified receipt pass as valid (§13).
func (n Network) verifyReceiptSignature(r *Receipt, pubKeyB64 string) error {
	if pubKeyB64 == "" {
		return ErrInvalidState.Wrap("peer public key is not configured; cannot verify receipt signature")
	}
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return err
	}
	cp := *r
	cp.Signature = ""
	return n.verify(pub, sigDomainReceipt, cp, r.Signature)
}

// parseAndVerifyRemoteReceipt parses receiptJSON and enforces the settlement preconditions:
// signature, action_id, and args_hash must all match what we requested. Returns ErrTimeout
// (keep-trace-open) on absent, unparseable, invalidly signed, or mismatched receipts so the
// trace is never settled against a receipt that fails the invariants VerifyReceipt audits.
// Settlement must only proceed when this function returns without error.
func (n Network) parseAndVerifyRemoteReceipt(receiptJSON, pubKeyB64, expectedActionID, expectedArgsHash string) (*Receipt, error) {
	if receiptJSON == "" {
		return nil, ErrTimeout.Wrap("remote receipt pending")
	}
	var r Receipt
	if err := json.Unmarshal([]byte(receiptJSON), &r); err != nil {
		return nil, ErrTimeout.Wrap("remote receipt pending")
	}
	if err := n.verifyReceiptSignature(&r, pubKeyB64); err != nil {
		return nil, ErrTimeout.Wrap("remote receipt: invalid signature")
	}
	if expectedActionID != "" && r.ActionID != expectedActionID {
		return nil, ErrTimeout.Wrap("remote receipt: action_id mismatch")
	}
	if expectedArgsHash != "" && r.ArgsHash != expectedArgsHash {
		return nil, ErrTimeout.Wrap("remote receipt: args_hash mismatch")
	}
	return &r, nil
}

// remoteReceiptInvalid returns a non-empty reason when a validly-signed remote receipt breaches the
// §13 settlement invariants (success ⇒ charge = mp and reply_hash matches; failure ⇒ 0 ≤ charge ≤ mp),
// so settlement can quarantine it (charge 0, full refund, no retry) instead of clamp-committing a
// record that would fail VerifyReceipt. An empty string means the receipt is settleable.
func (k *Kernel) remoteReceiptInvalid(r Receipt, mp, rbps int64, replyJSON []byte) string {
	// refresh_proxy is only ever a valid zero-charge pre-execution rejection (§13 rule C). A receipt
	// setting it on a success or any charged failure is malformed and quarantines, so a hostile peer
	// cannot pair a paid receipt with a cache-invalidation signal.
	if r.RefreshProxy && !(r.Status == TxFailure && r.Charge == 0 && r.Premium == 0) {
		return "refresh_proxy on a non-rejection receipt"
	}
	// The value channel is local to a kernel (§13): no call this kernel dispatches carries value, so a
	// remote receipt claiming any is inconsistent with what was sent and quarantines rather than
	// settling — the reserve it would move does not exist here.
	if r.Value != 0 {
		return "remote receipt carries value"
	}
	switch r.Status {
	case TxSuccess:
		if r.Charge != mp {
			return "success charge != mp"
		}
		if h, err := jcsHashStr(string(replyJSON)); err != nil || r.ReplyHash != h {
			return "reply_hash mismatch"
		}
	case TxFailure:
		if r.Charge < 0 || r.Charge > mp {
			return "failure charge out of range"
		}
	default:
		return "unknown status"
	}
	// The execution premium must be the manifest-snapshot rate on the actual charge (§13) — the
	// same calculation settlement commits and the audit re-runs.
	if _, _, ok := k.econ.RemoteSettlement(r.Charge, r.Premium, rbps, 0, false); !ok {
		return "premium != ceil(charge*rbps)"
	}
	// An obligation must be drawable: without the seller's nonce there is no draw, and a buyer that
	// settled anyway would be choosing the outcome alone (P10).
	if r.Charge+r.Premium > 0 && r.Nonce == "" {
		return "receipt carries an obligation but no nonce"
	}
	return ""
}

// remotePendingMaxAge is how long a dispatched call may stay unsettled before it settles locally as
// a failure with a full refund (§13). One definition: the settle path quotes it to the caller as the
// refund-eligibility time, and the retry loop enforces it.
// pendingMeta attaches the durable handle for money reserved on a call still awaiting a remote
// receipt (§13): the process to watch, when the reserve started, and when a refund becomes due.
// `since` is the PENDING call's own start, never the start of whatever is reporting it — a parent
// that failed early would otherwise date its child's reserve from the wrong call.
func (k *Kernel) pendingMeta(err *KernelError, processID string, since time.Time) *KernelError {
	return err.WithMeta("process_id", processID).
		WithMeta("pending_since", since.UTC().Format(time.RFC3339)).
		WithMeta("refund_eligible_at", since.Add(k.remotePendingMaxAge()).UTC().Format(time.RFC3339))
}

func (k *Kernel) remotePendingMaxAge() time.Duration {
	if k.cfg.RemotePendingMaxAge == 0 {
		return 24 * time.Hour
	}
	return k.cfg.RemotePendingMaxAge
}

// settleRemoteCall settles a remote-proxy call after ExecuteFederation returns.
// If the receipt is absent or has an invalid signature, the trace stays open for retry (ErrTimeout).
// Otherwise it commits the obligation, the draw that decides what is paid for it, and the payment
// itself, in one transaction (P7, P10).
func (k *Kernel) settleRemoteCall(ctx context.Context, logger *log.Logger, action *Action, ktx *Transaction, trace *Trace, callerWalletID, callerWalletKind string, req callRequest, target *Account, mp int64, fr FederationResult, latency float64) (*CallReply, error) {
	// The values we dispatched under ride on the trace, so both the direct and the retry settle paths
	// read them from one source (§13). The funding boundary froze them, so a fee change — or a
	// lottery change — between dispatch and settlement cannot move this call's arithmetic.
	d := dispatched(trace.DispatchJSON)
	rbps, importBPS := d.RemoteBPS, d.ImportBPS
	dispatchedHash := d.ContractHash
	// A missing, unparseable, unsigned, or mismatched receipt keeps the trace open for retry.
	// action_id and args_hash are enforced here so settlement is valid by construction.
	expectedArgsHash, _ := jcsHashStr(string(ktx.ArgsJSON))
	rp, err := k.cfg.Network.parseAndVerifyRemoteReceipt(fr.ReceiptJSON, target.KernelPublicKey, action.RemoteActionID, expectedArgsHash)
	if err != nil {
		// The call is parked, not lost: no receipt has settled it, so the allocation stays locked and
		// the process open until one arrives or the pending bound expires (§13). Hand back the durable
		// handle for it — the process to watch and when a refund becomes due — so the caller is not
		// left with money reserved and no way to follow it. The original cause stays the message's
		// head: absent, unparseable, badly signed, and mismatched receipts are different diagnoses.
		if errors.Is(err, ErrTimeout) {
			return nil, k.pendingMeta(ErrTimeout.Wrapf("%v; result confirmation is pending — funds remain reserved on process %s and the call retries automatically", err, trace.ProcessID),
				trace.ProcessID, trace.CreatedAt)
		}
		return nil, err
	}
	r := *rp

	// A validly-signed receipt is the peer's final, deterministic word: idempotent retry returns
	// the same bytes, so a receipt that breaches the §13 settlement invariants can never heal.
	// Settle it terminally as receipt-invalid (charge 0, full refund, raw receipt kept as evidence,
	// no retry) rather than clamp-and-commit a record that would fail our own VerifyReceipt
	// audit. Reconcile the discrepancy out of band (§13). The reply bytes are marshalled once so
	// the hash here is computed over exactly what gets stored.
	var replyJSON []byte
	if r.Status == TxSuccess && fr.Result != nil {
		replyJSON, _ = json.Marshal(fr.Result)
	}
	charge := r.Charge
	var premium int64
	quarantined := false
	if invalid := k.remoteReceiptInvalid(r, mp, rbps, replyJSON); invalid != "" {
		logger.Warn("remote.receipt_invalid", "action", action.Name, "reason", invalid)
		charge = 0
		ktx.Status = TxFailure
		ktx.Reason = reasonRemoteReceiptInvalidPrefix + invalid
		quarantined = true
	} else {
		premium = r.Premium // execution serving markup
		ktx.Status = r.Status
		if r.Status == TxSuccess {
			ktx.ReplyJSON = json.RawMessage(replyJSON)
		}
	}
	// What we owe the peer for this call, and what we retain for importing it — the one calculation
	// the offline audit re-runs against this settlement (P7).
	obligation, importFee, _ := k.econ.RemoteSettlement(charge, premium, rbps, importBPS, ktx.Status == TxSuccess)
	ktx.Net = obligation
	ktx.Fee = importFee
	ktx.RemoteReceiptHash = sha256Hex(fr.ReceiptJSON)
	ktx.RemoteReceiptJSON = fr.ReceiptJSON
	ktx.RemoteSignerKey = target.KernelPublicKey // stored with the receipt it verified (G7)

	stats := k.computeStats(ctx, action.ID, ktx, latency)
	// A rejection is the peer's refusal to execute, at any price: an executed failure that consumed
	// nothing also charges 0, and the inbound handler returns the execution error's HTTP status
	// alongside the real receipt, so a propagated ErrInsufficientFunds arrives as 402 too. tx_id ==
	// our dispatched key is the one marker that separates the two (§6 P4). A quarantined receipt is
	// never a rejection: it also forces charge 0, and the explicit flag is its signal. Computed once
	// here because two decisions read it: how the failure is classified, and whether the cached row
	// survives it.
	var dispatchedKey string
	if trace.IdempotencyKey != nil {
		dispatchedKey = *trace.IdempotencyKey
	}
	rejection := !quarantined && ktx.Status != TxSuccess && isRejectionReceipt(r.TxID, dispatchedKey)

	// Classify a failure BEFORE building the receipt: the commit stores the error body a replaying
	// peer will be served, so it needs this call's code, and buildReceipt copies ktx.Reason — so the
	// reason must be final here or the signed receipt and the transaction would disagree.
	var failErr error
	if ktx.Status != TxSuccess {
		failErr = ErrExecutionFailed.Wrap("remote call failed")
		if rejection {
			switch {
			case fr.HTTPStatus == 402:
				// The remote refuses us credit: its limit is reached, or we owe it for a call it has
				// not been paid for. Operator-actionable, so a client never renders it as the
				// caller's own insufficient_funds.
				failErr = PeerUnfundedError(k.KernelName(ctx, target.KernelPublicKey))
			case r.RefreshProxy:
				// The peer's contract moved under our cached copy. NOT a terms change in the buyer's
				// sense: the contract hash also covers the artifact, kind, and owner id (§6 P6), so a
				// re-implementation at an unchanged price trips this while the quote stays identical.
				// Consent is the pin's job at the funding boundary (§4 precondition 7), which the next
				// call reaches with a freshly resolved row; here we only say what happened.
				failErr = ErrExecutionFailed.Wrap("the provider updated this action; nothing was charged — re-run to refresh").
					WithMeta("retry", "refresh")
			default:
				// The peer will not serve us — suspended caller, or an action it refuses. Distinct
				// from unreachable (retry) and unfunded (top up): a human must resolve it.
				failErr = PeerRefusedError(k.KernelName(ctx, target.KernelPublicKey))
			}
		}
		// The peer authored r.Reason; never adopt it into a record we sign (§6). Its verbatim text
		// stays verifiable in ktx.RemoteReceiptJSON. The quarantine marker above wins.
		if ktx.Reason == "" {
			ktx.Reason = KernelErrorCode(failErr)
		}
	}
	localReceipt, receiptErr := k.buildReceipt(ktx, obligation+importFee, 0, 0, "", "")
	if receiptErr != nil {
		return nil, ErrInternal.Wrap("could not build receipt")
	}

	// The draw (P10). Both kernels compute it from the same three values — our secret, the peer's
	// nonce, and the obligation — so neither can pick the outcome and neither has to trust the
	// other's report of it. A losing ticket pays nothing; a winning one pays the face value; an
	// obligation at or above the face value is paid exactly.
	payout, drawn := k.drawPayment(trace, d, obligation, r.Nonce, k.peerRailAddress(ctx, target.ID))

	// Detach settlement from execution-scoped cancellation so the remote settlement
	// (obligation/duty/refund + audit record) always commits once the signed receipt is in.
	sctx, cancel := settlementContext(ctx)
	defer cancel()
	if err := k.store.CommitRemoteSettlement(sctx, ktx, localReceipt, trace.ID, callerWalletID, callerWalletKind, k.cfg.FeeRecipientID, obligation, importFee, payout, stats, req.IdempotencyRecordID, req.StepID, KernelErrorCode(failErr)); err != nil {
		return nil, ErrInternal.Wrap("could not commit remote settlement")
	}
	k.SettleReady(sctx)

	// Rule C (§8/§13): a settlement outcome proving the cached row wrong invalidates it, so the next
	// call re-resolves. Three such outcomes: a signed refresh_proxy rejection (the contract moved), a
	// quarantined receipt, and any other non-funding rejection — the peer refused to serve this action
	// at all (retired, made private, owner suspended), so the entry is stale whether or not it says so.
	// A funding (402) rejection is exempt: our credit is exhausted, the action is fine.
	// Supervision-side, outside the monetary write set (a failure here never rolls back settlement),
	// and hash-conditional so a stale dispatch settling after a re-resolve spares the refreshed row.
	if r.RefreshProxy || quarantined || (rejection && fr.HTTPStatus != 402) {
		// The dispatch snapshots the hash it bound; absent it (a trace not dispatched through the normal
		// path), guard against the row's current hash so the deactivation still targets this row.
		guardHash := dispatchedHash
		if guardHash == "" {
			guardHash = action.ArtifactHash
		}
		if derr := k.store.DeactivateImportedIfHash(sctx, action.ID, guardHash, time.Now().UTC()); derr != nil {
			logger.Warn("remote.proxy_deactivate_failed", "action", action.Name, "error", derr)
		} else {
			logger.Info("remote.proxy_deactivated", "action", action.Name, "reason", ktx.Reason)
		}
	}

	logger.Info("remote.settled", "action", action.Name, "status", ktx.Status,
		"charge", charge, "premium", premium, "import_fee", importFee, "draw", drawn)

	if ktx.Status == TxSuccess {
		return &CallReply{Result: fr.Result, TxID: ktx.ID, TraceID: trace.ID, ReceiptID: localReceipt.ID}, nil
	}
	// Return the committed local receipt alongside the error so an inbound caller can settle
	// the real charge (a re-proxied remote subcall may have settled with charge > 0).
	return &CallReply{TxID: ktx.ID, TraceID: trace.ID, ReceiptID: localReceipt.ID}, failErr
}

// drawPayment decides what one obligation actually pays and produces the payment to make when the
// draw says pay. It records nothing else: the secret is already frozen on the trace, the nonce is
// signed into the receipt the transaction stores, and the payment is a rail row like any other — so
// what the draw decided can always be re-derived rather than believed.
//
// The payment is keyed by the call's own idempotency key, which is what joins the trace, the reveal
// and the money together, and reserved in the same commit that decided it: there is no moment where
// the books say we owe and nothing is set aside for it.
func (k *Kernel) drawPayment(trace *Trace, d dispatchPayload, obligation int64, nonce, destination string) (*RailTransfer, int64) {
	if obligation <= 0 || trace.IdempotencyKey == nil {
		return nil, 0
	}
	amount := Draw(*trace.IdempotencyKey, mustHex(d.Secret), mustHex(nonce), obligation, d.Lottery)
	if amount == 0 {
		return nil, 0
	}
	return &RailTransfer{
		ID: *trace.IdempotencyKey, Kind: RailKindObligation, Party: trace.CallerUserID,
		Amount: amount, Credit: amount, Destination: destination, Status: RailStatusPending,
		Reason: "obligation " + *trace.IdempotencyKey, CreatedAt: time.Now().UTC(),
	}, amount
}

// RetryPendingRemoteDispatches retries all in-flight remote proxy traces that have an
// idempotency key but no settled transaction. Called once at startup (after Recover) and
// periodically by the serve retry loop (startRemoteRetryLoop) — that loop is what lets a peer
// returning online settle parked calls, and the RemotePendingMaxAge refund fire, without a
// restart. Errors for individual traces are logged and skipped.
func (k *Kernel) RetryPendingRemoteDispatches(ctx context.Context) {
	logger := k.log.With(ctx)
	traces, err := k.store.ListPendingRemoteTraces(ctx)
	if err != nil {
		logger.Error("remote.retry.list_failed", "error", err)
		return
	}
	for _, trace := range traces {
		if err := k.retryRemoteTrace(ctx, logger, trace); err != nil {
			logger.Error("remote.retry.trace_failed", "trace_id", trace.ID, "error", err)
		}
	}
}

// PendingRemoteTraces returns the in-flight remote-proxy traces awaiting a receipt (idempotency key
// set, no settled transaction). Exposed for the serve retry loop, which schedules per-trace retries
// with backoff instead of re-dispatching the whole set every tick.
func (k *Kernel) PendingRemoteTraces(ctx context.Context) ([]*Trace, error) {
	return k.store.ListPendingRemoteTraces(ctx)
}

// RetryRemoteTrace re-issues one pending remote dispatch and settles it if a receipt has arrived,
// or settles it as a terminal failure once RemotePendingMaxAge has elapsed (§13). Idempotent: the
// retry carries the same key, so the remote replays rather than re-executing. The serve loop calls
// this per due trace; it derives its own logger so callers never handle one.
func (k *Kernel) RetryRemoteTrace(ctx context.Context, trace *Trace) error {
	return k.retryRemoteTrace(ctx, k.log.With(ctx), trace)
}

// retryRemoteTrace re-issues one pending remote dispatch and settles it if a receipt arrives.
func (k *Kernel) retryRemoteTrace(ctx context.Context, logger *log.Logger, trace *Trace) error {
	if trace.IdempotencyKey == nil || trace.DispatchJSON == nil {
		return nil
	}
	dispatch := dispatched(trace.DispatchJSON)
	action, err := k.store.ReadAction(ctx, trace.ActionID)
	if err != nil || action == nil {
		return ErrNotFound.Wrap("action not found for retry")
	}
	target, err := k.store.ReadUser(ctx, trace.ActionOwnerID)
	if err != nil {
		return ErrNotFound.Wrap("target not found for retry")
	}
	process, err := k.store.ReadProcess(ctx, trace.ProcessID)
	if err != nil {
		return ErrNotFound.Wrap("process not found for retry")
	}
	fe := k.fedClient
	if fe == nil {
		return ErrInvalidState.Wrap("federation executor not configured")
	}
	mp := dispatch.RemotePrice
	// The locked gross is the proxy's full two-step local price (§13) — what BeginRun/BeginStepCall
	// funded — taken from the dispatch record, which froze it at the funding boundary: the action's
	// price floats with local policy, so re-reading the column would refund a call at a rate it was
	// never locked at.
	q := dispatch.Gross

	callerWalletID, callerWalletKind := callerWalletFor(dispatch.StepID, process.ID, trace.ParentTraceID)

	now := time.Now().UTC()
	ktx := newTraceFailureTx(trace, process, action, q, now)
	if trace.ParentTraceID != nil {
		ktx.ParentTraceID = *trace.ParentTraceID
	}
	ktx.RemoteActionID = action.RemoteActionID
	argsJSON, _ := json.Marshal(dispatch.Args)
	ktx.ArgsJSON = json.RawMessage(argsJSON)

	req := callRequest{StepID: dispatch.StepID}
	// The inbound record rides on the trace, so it survives for every action kind and is found
	// here whether this retry settles on a receipt or hits the max-age bound below.
	if trace.IdempotencyRecordID != nil {
		req.IdempotencyRecordID = *trace.IdempotencyRecordID
	}

	// fr.NotDispatched is deliberately ignored on the retry path: a parked trace's request may
	// already have executed remotely, so §13 forbids fail-fast here — only a signed receipt or the
	// max-pending-age bound below settles it. Never-dispatched fail-fast lives solely in Call (§6).
	fr, _ := fe.ExecuteFederation(ctx, target.KernelPublicKey, action.RemoteActionID, action.ArtifactHash, *trace.IdempotencyKey, commitmentOf(dispatch.Secret), dispatch.Lottery, dispatch.Args)
	if fr.ReceiptJSON != "" {
		_, err = k.settleRemoteCall(ctx, logger, action, ktx, trace, callerWalletID, callerWalletKind, req, target, mp, fr, 0)
		if !errors.Is(err, ErrTimeout) {
			return err // settled, or a real settlement error
		}
		// ErrTimeout here = a received-but-invalid receipt; fall through to the age bound rather
		// than failing on the first malformed response (could be transient transport junk).
	}

	// No settleable receipt yet. Past the bound the §13 idempotency key may be gone on the remote,
	// so settle as a failure with full refund rather than retry forever; otherwise keep retrying.
	if now.Sub(trace.CreatedAt) > k.remotePendingMaxAge() {
		ktx.Status = TxFailure
		logger.Warn("remote.retry.expired", "trace_id", trace.ID, "age_seconds", now.Sub(trace.CreatedAt).Seconds())
		_, _, sErr := k.settleFailedCall(ctx, logger, ktx, trace, callerWalletID, callerWalletKind, req, action, 0, ErrTimeout.Wrap("remote call unsettled past max pending age"))
		return sErr
	}
	return nil
}

// SignFederation signs a federation payload with the platform key and returns
// (signature, timestamp). Returns an error if the signing key is not configured.
func (k *Kernel) SignFederation(action, counterparty, recipient, expectedContractHash, idempotencyKey, argsHash, commitment string, lottery int64) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = k.cfg.Network.SignFederationPayload(k.cfg.SigningKey, action, counterparty, recipient, expectedContractHash, idempotencyKey, ts, argsHash, commitment, lottery)
	return
}

// SignStep signs a step-completion payload with the platform key and returns
// (signature, timestamp). recipient is the peer being addressed. Returns an error if the signing
// key is not configured.
func (k *Kernel) SignStep(stepID, counterparty, recipient, idempotencyKey, inputHash string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = k.cfg.Network.SignStepPayload(k.cfg.SigningKey, stepID, counterparty, recipient, idempotencyKey, ts, inputHash)
	return
}

// SignStepList signs a step-list request with the platform key and returns
// (signature, timestamp). recipient is the peer being addressed.
func (k *Kernel) SignStepList(counterparty, recipient, forUserID string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = k.cfg.Network.SignStepListPayload(k.cfg.SigningKey, counterparty, recipient, ts, forUserID)
	return
}

// ---- Outbound step protocol (§13) ----

// StepIdempotencyKey derives the cross-kernel key for one step completion. It is DERIVED, never
// minted per attempt: a retry after a network failure presents the same key and recovers the stored
// outcome, since a step completion has no local trace to persist one on (unlike a remote-proxy call).
// recipient is the serving kernel's key. Exported because the serving side recomputes it to verify
// the requester's key. The trailing empty component is a reserved slot in the derivation: keeping it
// makes every key byte-identical to those already in flight, so a completion retried across an
// upgrade recovers its stored outcome instead of re-executing under a fresh key.
func StepIdempotencyKey(recipient, stepID, inputHash string) string {
	return sha256Hex("juice/fed/step/1|" + recipient + "|" + stepID + "|" + inputHash + "|")
}

// normalizeStepInput renders completion input as exactly the bytes the transport will send, and
// hashes those. Marshaling the outer request compacts and HTML-escapes an embedded raw message, so
// hashing the caller's raw body would sign bytes the peer never sees; marshaling a RawMessage is
// idempotent, so this is a fixed point — hash and send the same slice.
func normalizeStepInput(raw json.RawMessage) ([]byte, string, error) {
	input := []byte(raw)
	if len(input) == 0 {
		input = []byte("{}")
	}
	input, err := json.Marshal(json.RawMessage(input))
	if err != nil {
		return nil, "", ErrInvalidInput.Wrap("input must be valid JSON")
	}
	return input, sha256Hex(string(input)), nil
}

// selfKey is this kernel's own base64url public key — the counterparty it signs as.
func (k *Kernel) selfKey(ctx context.Context) string {
	key, _ := k.store.GetConfig(ctx, "signing_public_key")
	return key
}

// stepReply unwraps a peer's step response into a decoded body, mapping transport and protocol
// failures to typed errors. The §13 dispatch distinction is preserved exactly as the call path
// keeps it: only a provably-never-sent request is unreachable; anything else may already have
// executed there, so it is a timeout the caller recovers by retrying under the same derived key.
func stepReply(status int, body []byte, notDispatched bool, err error, peerKey string) (map[string]any, error) {
	if err != nil || notDispatched {
		if notDispatched {
			return nil, PeerUnreachableError(peerKey)
		}
		return nil, ErrTimeout.Wrapf(
			"no reply from %s; the request may have executed there — retry to recover its result", peerKey).WithMeta("peer", peerKey)
	}
	var decoded map[string]any
	if json.Unmarshal(body, &decoded) != nil {
		return nil, ErrExecutionFailed.Wrap("malformed peer response")
	}
	if status >= 300 {
		msg, _ := decoded["error"].(string)
		if msg == "" {
			msg = "peer rejected the step request"
		}
		code, _ := decoded["code"].(string)
		return nil, ErrorFromCode(code).Wrap(msg)
	}
	return decoded, nil
}

// PeerStepsAwaitingUs lists the steps a peer holds for this kernel (§13), for admin inspect and for
// the payment-descriptor lookup below. One bounded fetch: the queue is a handful of pending
// cross-kernel approvals, not a corpus.
func (k *Kernel) PeerStepsAwaitingUs(ctx context.Context, peerKey, forUserID string) (*PeerStepList, error) {
	if k.fedClient == nil {
		return nil, ErrInvalidState.Wrap("federation transport not running")
	}
	self := k.selfKey(ctx)
	sig, ts, err := k.SignStepList(self, peerKey, forUserID)
	if err != nil {
		return nil, err
	}
	status, body, notDispatched, err := k.fedClient.ListPeerSteps(ctx, peerKey, ts, sig, forUserID)
	reply, err := stepReply(status, body, notDispatched, err, peerKey)
	if err != nil {
		return nil, err
	}
	// Values, not pointers: the list is peer-controlled, and a reply of {"steps":[null]} would
	// otherwise decode to a nil element that every reader must remember to guard. Decoding into
	// values makes the malformed entry a zero one, which matches no step id and carries no payment.
	b, _ := json.Marshal(reply)
	var list PeerStepList
	if json.Unmarshal(b, &list) != nil {
		return nil, ErrExecutionFailed.Wrap("malformed peer step list")
	}
	return &list, nil
}

// completePeerStepRaw signs and dispatches one completion under an ALREADY-DERIVED idempotency key:
// a retry must present the key its first attempt used, so the key is an argument, never re-derived
// here. forUserID, when non-empty, attaches the home-kernel step_auth attestation naming that stable
// local id, so a remote-user-addressed step is completed as that specific user (§13); empty is a
// kernel-level completion for a kernel-addressed step.
func (k *Kernel) completePeerStepRaw(ctx context.Context, peerKey, stepID string, input []byte, inputHash, idempotencyKey, forUserID string) (map[string]any, error) {
	if k.fedClient == nil {
		return nil, ErrInvalidState.Wrap("federation transport not running")
	}
	self := k.selfKey(ctx)
	sig, ts, err := k.SignStep(stepID, self, peerKey, idempotencyKey, inputHash)
	if err != nil {
		return nil, err
	}
	var attestation, attestTS string
	superuser := false
	if forUserID != "" {
		superuser = k.IsSuperuser(ctx, forUserID)
		if attestation, attestTS, err = k.SignStepAuth(self, peerKey, forUserID, stepID, superuser); err != nil {
			return nil, err
		}
	}
	status, body, notDispatched, err := k.fedClient.CompletePeerStep(ctx, peerKey, ts, sig, stepID,
		idempotencyKey, input, forUserID, attestation, attestTS, superuser)
	return stepReply(status, body, notDispatched, err, peerKey)
}

// CompletePeerStep resumes a step a peer parked for this kernel over /juice/fed/step/1 (§13). No money
// moves here: the step's price was parked on the serving kernel at creation and completion never checks
// funds (§10), so the requester creates no local trace or transaction and a timeout pins nothing. The
// completion settles wholly on the serving kernel under §6.
func (k *Kernel) CompletePeerStep(ctx context.Context, peerKey, stepID string, rawInput json.RawMessage, forUserID string) (map[string]any, error) {
	input, inputHash, err := normalizeStepInput(rawInput)
	if err != nil {
		return nil, err
	}
	return k.completePeerStepRaw(ctx, peerKey, stepID, input, inputHash,
		StepIdempotencyKey(peerKey, stepID, inputHash), forUserID)
}

// ---- Peer operations ----

// ResolveKernelKey maps a kernel qualifier (from an owner@kernel/name reference) to the kernel's
// public key and, when one exists locally, its account. Resolution order is strictly: a bound local
// petname → a raw base64url key → not found. A kernel's self-asserted nickname NEVER resolves a
// reference (§13), so no kernel can capture a name by gossiping a label first. The account may be
// nil for a key with no financial relationship here yet.
func (k *Kernel) ResolveKernelKey(ctx context.Context, ident string) (peerKey string, mount *Account, err error) {
	ident = strings.TrimSpace(ident)
	if rk, err := k.store.ReadKernelByPetname(ctx, ident); err == nil && rk != nil {
		acct, _ := k.store.ReadAccountByKernelKey(ctx, rk.PublicKey) // nil until money is involved
		return rk.PublicKey, acct, nil
	}
	if looksLikeKey(ident) {
		acct, _ := k.store.ReadAccountByKernelKey(ctx, ident)
		return ident, acct, nil
	}
	return "", nil, ErrNotFound.Wrapf("kernel %q is not a known petname or key", ident)
}

// ResolveRequiredCaller resolves a step's required-caller reference (§10, §13). A bare/local ref
// returns (localUserID, ""). A kernel-qualified ref user@kernel returns (peer proxy userID, stable
// remote user_id): the proxy user is the local accounting/routing account, the remote id addresses
// the completer beneath its mutable handle, so completion demands a step_auth attestation naming it.
// A raw-key qualifier is mounted on demand (best-effort alias); the remote user is resolved to its
// stable id over /juice/fed/resolve/1.
// RequiredCaller is who a step is parked for: the local account that funds and routes it, and, when
// that account is a peer, the principal on that peer — its stable id, which authorises completion,
// and the handle it went by when the step was made, which only ever displays it. The two travelled
// as separate strings that had to agree; one value carries them and the display name for free.
type RequiredCaller struct {
	UserID   string // local account: the user, or the peer's proxy account
	RemoteID string // the completer's stable id on that peer (P8); empty when local
	Handle   string // that principal's handle when the step was made; display only, may go stale
}

func (k *Kernel) ResolveRequiredCaller(ctx context.Context, ref string) (rc RequiredCaller, err error) {
	ref = strings.TrimSpace(ref)
	owner, kernelAlias, hasKernel := strings.Cut(ref, "@")
	if !hasKernel {
		u, uerr := k.ResolveUser(ctx, ref)
		if uerr != nil || !u.IsLive() {
			// A tombstone resolves but can never complete: the step would park its price forever.
			return rc, ErrNotFound.Wrapf("required caller %q not found", ref)
		}
		return RequiredCaller{UserID: u.ID}, nil
	}
	// A sigil-prefixed "@bob" cuts to an empty owner; reject it rather than treat it as a
	// kernel-qualified ref with no owner (handles are bare, §14).
	if owner == "" || kernelAlias == "" {
		return rc, ErrInvalidInput.Wrapf("required caller %q must be owner@kernel", ref)
	}
	peerKey, mount, kerr := k.ResolveKernelKey(ctx, kernelAlias)
	if kerr != nil {
		return rc, kerr
	}
	resolver := k.fedClient
	if resolver == nil {
		return rc, ErrNotFound.Wrap("remote resolution unavailable")
	}
	remoteUserID, remoteHandle, rerr := resolver.ResolveRemoteUser(ctx, peerKey, owner)
	if rerr != nil {
		return rc, rerr
	}
	// An empty id is not a principal. Accepted, it would address the step to the peer kernel
	// itself — operator scope, decided by a remote reply — and strand the user it was meant for.
	if remoteUserID == "" {
		return rc, ErrInvalidInput.Wrapf("peer resolved %q to no user id", ref)
	}
	// First meaningful use (§13): a verified remote-user resolve is our own outbound act, so a
	// petname is bound here too — on the petname being unbound, not on the account being absent
	// (see lazyResolveRemote). Best-effort — a missing name never blocks the step.
	if _, berr := k.BindPetname(ctx, peerKey, "", false); berr != nil {
		k.log.With(ctx).Warn("kernel.petname.bind_failed", "public_key", peerKey, "error", berr.Error())
	}
	if mount == nil {
		if mount, err = k.EnsureKernelAccount(ctx, peerKey); err != nil {
			return rc, err
		}
	}
	return RequiredCaller{UserID: mount.ID, RemoteID: remoteUserID, Handle: NormalizeHandle(remoteHandle)}, nil
}

// ResolvePrincipal resolves a user reference (bare handle or id) to its stable id and current
// handle, for the open /juice/fed/resolve/1 protocol (§13): the caller's home kernel maps a
// friendly `owner@kernel` to the underlying PrincipalID beneath the name. Only a live
// (non-suspended) account resolves; ids are addresses, not secrets.
func (k *Kernel) ResolvePrincipal(ctx context.Context, ref string) (userID, handle string, err error) {
	u, err := k.ResolveUser(ctx, ref)
	// A live local user has a handle. A kernel account has none, and a purged peer's tombstone has
	// neither handle nor key (§13 Retention) — resolving either would answer a peer with a nameless
	// principal, so an id that lands on one is not found.
	if err != nil || u == nil || u.SuspendedAt != nil || u.Handle == "" {
		return "", "", ErrNotFound.Wrapf("user %s not found", ref)
	}
	return u.ID, u.Handle, nil
}

// The three kernel lifecycle operations (§13). They are deliberately separate: observation must not
// name or fund a kernel, naming must not open a billing relationship, and an inbound call or a
// deposit must not let a stranger seed a local name. Each path calls only what it is entitled to.

// ObserveKernel records what a verified gossip pull said about a kernel: its self-asserted nickname
// and about. It binds no petname, opens no account, and never advances the evidence cursor or the
// peer-sync cache — those have their own narrow paths, run only after their own work commits.
func (k *Kernel) ObserveKernel(ctx context.Context, publicKey, nickname, about string) error {
	return k.observeKernel(ctx, publicKey, nickname, about, "", "")
}

// observeKernel records what a verified reply said about a kernel, including where it is paid.
func (k *Kernel) observeKernel(ctx context.Context, publicKey, nickname, about, railAddress, railProof string) error {
	if _, err := decodeRemotePublicKey(publicKey); err != nil {
		return err
	}
	return k.store.UpsertKernel(ctx, publicKey, nickname, about, railAddress, railProof, time.Now().UTC())
}

// BindPetname assigns a kernel's local, resolvable name (§13 Stiegler naming). Automatic binding
// (exact=false, on our own verified outbound act) keeps any existing petname, else seeds from the
// kernel's cached nickname when that is a valid bare name, else the mechanical k-<key8>; collisions
// suffix locally. An explicit operator bind (exact=true) takes the requested name exactly and fails
// if it is occupied — never silently suffixed. Petnames share the handle grammar and validator, so a
// key- or id-shaped name is rejected and the resolver's syntactic disjointness holds (§14).
func (k *Kernel) BindPetname(ctx context.Context, publicKey, desired string, exact bool) (string, error) {
	if _, err := decodeRemotePublicKey(publicKey); err != nil {
		return "", err
	}
	if err := k.store.UpsertKernel(ctx, publicKey, "", "", "", "", time.Now().UTC()); err != nil {
		return "", err
	}
	seed := NormalizeHandle(desired)
	if !exact && seed == "" {
		if rk, err := k.store.ReadKernel(ctx, publicKey); err == nil && rk != nil {
			seed = NormalizeHandle(rk.Nickname)
		}
	}
	if validateHandle(seed) != nil {
		if exact {
			return "", ErrInvalidInput.Wrapf("petname %q is not a valid bare name", desired)
		}
		seed = "k-" + publicKey[:8] // every kernel key is 43 base64url chars
	}
	bound, err := k.store.BindPetname(ctx, publicKey, seed, exact)
	if err != nil {
		return "", err
	}
	k.log.With(ctx).Info("kernel.petname.bound", "petname", bound, "public_key", publicKey)
	return bound, nil
}

// EnsureKernelAccount opens the billing relationship with a kernel: a minimal kernel row if none
// exists (never clearing learned metadata) plus a zero-balance account. It binds no petname — an
// inbound call or a deposit is not our act of naming. Idempotent, including under the race where two
// inbound calls provision the same key concurrently.
func (k *Kernel) EnsureKernelAccount(ctx context.Context, publicKey string) (*Account, error) {
	if _, err := decodeRemotePublicKey(publicKey); err != nil {
		return nil, err
	}
	if existing, err := k.store.ReadAccountByKernelKey(ctx, publicKey); err == nil && existing != nil {
		return existing, nil
	}
	if err := k.store.UpsertKernel(ctx, publicKey, "", "", "", "", time.Now().UTC()); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	// A credentialless account: no handle and no password, authenticating only by federation
	// signature. The kernel it settles for is named by its petname, never by an account handle.
	u := &Account{ID: uuid.New().String(), KernelPublicKey: publicKey, CreatedAt: now, UpdatedAt: now}
	if err := k.store.CreateUser(ctx, u); err != nil {
		// Two provisioning paths can race (an inbound call and the CLI both write this DB); the
		// loser trips idx_accounts_kernel. Treat that as idempotent success and return the winner.
		if winner, rerr := k.store.ReadAccountByKernelKey(ctx, publicKey); rerr == nil && winner != nil {
			return winner, nil
		}
		return nil, err
	}
	k.log.With(ctx).Info("kernel.account.created", "public_key", publicKey)
	return u, nil
}

// SuspendKernel freezes a remote kernel (§13), provisioning its account if it has none so a
// not-yet-transacting kernel can be blocked before its first inbound call — atomically, so no call
// can execute against a briefly-active account. Superuser-only, like every moderation act.
func (k *Kernel) SuspendKernel(ctx context.Context, operatorID, publicKey string) error {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return err
	}
	if _, err := decodeRemotePublicKey(publicKey); err != nil {
		return err
	}
	if err := k.store.SuspendKernelAccount(ctx, publicKey, uuid.New().String(), time.Now().UTC()); err != nil {
		return err
	}
	k.log.With(ctx).Info("kernel.suspended", "public_key", publicKey)
	return nil
}

// RenameKernel is the explicit operator bind (§13): superuser-only, exact, and the only way to name
// a kernel this node has merely discovered. It is `admin rename` over the kernel namespace, the
// sibling of RenameUser over the account namespace.
func (k *Kernel) RenameKernel(ctx context.Context, operatorID, publicKey, petname string) (string, error) {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return "", err
	}
	return k.BindPetname(ctx, publicKey, petname, true)
}

// KernelName renders a kernel for a human: its bound petname, else its public key. It is the one
// rule for every kernel-facing output (§14) — a key always resolves a reference, so a name printed
// for an unbound kernel is still something the operator can retype into the next command.
func (k *Kernel) KernelName(ctx context.Context, publicKey string) string {
	if publicKey == "" {
		return ""
	}
	if rk, err := k.store.ReadKernel(ctx, publicKey); err == nil && rk != nil && rk.Petname != "" {
		return rk.Petname
	}
	return publicKey
}

// ListKernels returns the whole `admin peers` roster (§14): every known kernel, counterparties and
// discovery-only alike, from one store query. selfKey is excluded.
func (k *Kernel) ListKernels(ctx context.Context, includeSuspended bool, limit, offset int) ([]*RemoteKernelView, error) {
	selfKey, _ := k.store.GetConfig(ctx, "signing_public_key")
	return k.store.ListKernels(ctx, selfKey, includeSuspended, limit, offset)
}

// PeerKeys returns the public keys of all counterparties (kernels holding a live account here) —
// the pull set for the discovery loop's peer sync (§13 peer sync).
func (k *Kernel) PeerKeys(ctx context.Context) []string {
	kernels, err := k.ListKernels(ctx, false, 0, 0)
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(kernels))
	for _, p := range kernels {
		if p.HasAccount {
			keys = append(keys, p.PublicKey)
		}
	}
	return keys
}

// tradeGroup is one issuer's rows about one trade, in first-seen order.
type tradeGroup struct{ rows []*EvidenceRow }

// tradeGroups collapses evidence rows onto the trades they name. Rows are unique on the issuer's
// OWN receipt hash, which it mints, but one trade of ours is one receipt of one action: an issuer's
// rows naming the same receipt for the same action are one trade, so one purchase is one use and
// at most one rating. A row that names no trade stands alone under its own hash: it can be shown,
// never linked.
func tradeGroups(rows []*EvidenceRow) []*tradeGroup {
	var out []*tradeGroup
	index := map[[3]string]*tradeGroup{}
	for _, e := range rows {
		key := [3]string{e.IssuerPublicKey, e.SubjectActionID, e.RemoteReceiptHash}
		if e.RemoteReceiptHash == "" {
			key[2] = "own:" + e.ReceiptHash
		}
		g := index[key]
		if g == nil {
			g = &tradeGroup{}
			index[key] = g
			out = append(out, g)
		}
		g.rows = append(g.rows, e)
	}
	return out
}

// rating is the one rating a trade carries: its rows must agree, or the issuer has rated one trade
// two ways — equivocation by another route (D16) — and it carries none. A row outside the rating
// contract is not a rating; an equivocated row contributes nothing.
func (g *tradeGroup) rating() (RatingEvidence, bool) {
	var first *RatingEvidence
	for _, e := range g.rows {
		if e.Equivocated {
			return RatingEvidence{}, false // the issuer has told two stories about this trade
		}
		if e.RatingJSON == "" {
			continue
		}
		var rt RatingEvidence
		if json.Unmarshal([]byte(e.RatingJSON), &rt) != nil || validRating(rt.Rating, rt.Note) != nil {
			return RatingEvidence{}, false // a row outside the contract voids the trade, as the projection does
		}
		if first == nil {
			first = &rt
			continue
		}
		if first.Rating != rt.Rating || !sameNote(first.Note, rt.Note) {
			return RatingEvidence{}, false
		}
	}
	if first == nil {
		return RatingEvidence{}, false
	}
	return *first, true
}

func sameNote(a, b *string) bool {
	text := func(n *string) string {
		if n == nil {
			return ""
		}
		return *n
	}
	return text(a) == text(b) // an absent note and an empty one are the same absence
}

// SubjectEvidence derives the retained-evidence metrics about a subject kernel from the local
// evidence cache (§13), grouped by issuer. Uses/successes/failures/latency count ONLY issuer==subject
// rows (first-party execution evidence, so a remote call is never double-counted). A rating counts
// as trade-backed only when the two-kernel link holds: the rating's issuer evidence names this
// subject, its RemoteReceiptHash equals a subject execution row's ReceiptHash, and that subject row
// names the issuer as counterparty. Ratings that fail the link are surfaced as UnverifiedRatings;
// equivocated rows contribute no rating. This replaces the deleted introducer roster as the
// reputation display; local Stats are never touched by evidence.
func (k *Kernel) SubjectEvidence(ctx context.Context, subjectKernelPublicKey string) ([]*SubjectEvidenceRow, error) {
	rows, err := k.store.ListEvidenceBySubject(ctx, subjectKernelPublicKey)
	if err != nil {
		return nil, err
	}
	// Index the subject's own execution receipts by hash AND action, so a counterparty's row or a
	// rating is confirmed trade-backed only against the receipt of the very action it claims, and
	// only when that receipt names the issuer as counterparty.
	execKey := func(hash, action string) string { return hash + "\x1f" + action }
	counterpartyOf := map[string]string{}
	for _, e := range rows {
		if e.IssuerPublicKey == subjectKernelPublicKey {
			counterpartyOf[execKey(e.ReceiptHash, e.SubjectActionID)] = e.CounterpartyKernelPublicKey
		}
	}
	linked := func(e *EvidenceRow) bool {
		cp, ok := counterpartyOf[execKey(e.RemoteReceiptHash, e.SubjectActionID)]
		return ok && e.RemoteReceiptHash != "" && cp == e.IssuerPublicKey
	}
	agg := map[string]*SubjectEvidenceRow{}
	get := func(issuer, action string) *SubjectEvidenceRow {
		kk := issuer + "\x1f" + action
		if agg[kk] == nil {
			agg[kk] = &SubjectEvidenceRow{IssuerPublicKey: issuer, SubjectActionID: action}
		}
		return agg[kk]
	}
	// One trade is one use and at most one rating (tradeGroups), whatever number of rows an issuer
	// wrote about it. Interaction metrics come from each issuer's OWN evidence: the subject's
	// self-reported executions when issuer == subject (the execution summary), each other issuer's
	// directly-observed calls otherwise (its counterparty-experience row) — two views never summed
	// together (§13), so no call is double-counted. A counterparty's interaction and a rating are
	// trade-backed only through the two-kernel link; a claim without it is shown, never taken on faith.
	for _, g := range tradeGroups(rows) {
		e := g.rows[0]
		row := get(e.IssuerPublicKey, e.SubjectActionID)
		var er EvidenceReceipt
		if json.Unmarshal([]byte(e.EvidenceReceiptJSON), &er) == nil {
			row.Uses++
			if er.Status == TxSuccess {
				row.Successes++
			} else {
				row.Failures++
			}
			if lat := er.CreatedAt.Sub(er.StartedAt).Milliseconds(); lat >= 0 {
				row.AvgLatencyMs += float64(lat)
			}
			if e.IssuerPublicKey != subjectKernelPublicKey && linked(e) {
				row.CorroboratedUses++
			}
		}
		if rt, ok := g.rating(); ok {
			if linked(e) {
				row.RatingCount++
				row.RatingMean += rt.Rating
				if rt.Note != nil && *rt.Note != "" {
					row.Notes = append(row.Notes, *rt.Note)
				}
			} else {
				row.UnverifiedRatings++
			}
		}
	}
	out := make([]*SubjectEvidenceRow, 0, len(agg))
	for _, r := range agg {
		if r.Uses > 0 {
			r.AvgLatencyMs /= float64(r.Uses)
		}
		if r.RatingCount > 0 {
			r.RatingMean /= float64(r.RatingCount)
		}
		out = append(out, r)
	}
	return out, nil
}

// PurgeIdlePeers reaps peers idle past PeerRetention at zero balance (§13 Retention): it deletes
// each such peer's proxy actions, stats, discovery docs, evidence, and discovered_kernels rows and
// forgets the peer identity, keeping the transaction ledger intact. In the same pass it also evicts
// directory-only discovered kernels stale past the same horizon (never-peer cache rows that would
// otherwise accumulate unbounded, §13). Internal maintenance (like RetryPendingRemoteDispatches, no
// superuser gate) — driven by the serve sweep and once at startup. PeerRetention <= 0 disables it.
// Returns the number of peers purged.
func (k *Kernel) PurgeIdlePeers(ctx context.Context) (int, error) {
	if k.cfg.PeerRetention <= 0 {
		return 0, nil
	}
	logger := k.log.With(ctx)
	cutoff := time.Now().UTC().Add(-k.cfg.PeerRetention)
	ids, err := k.store.ListPurgeablePeers(ctx, cutoff)
	if err != nil {
		logger.Error("peer.purge.list_failed", "error", err)
		return 0, err
	}
	purged := 0
	for _, id := range ids {
		if err := k.store.PurgePeerCascade(ctx, id); err != nil {
			logger.Error("peer.purge.failed", "user_id", id, "error", err)
			continue
		}
		purged++
		logger.Info("peer.purged", "user_id", id)
	}
	// Evict directory-only discovered kernels stale past the same horizon, so the discovery cache
	// stays bounded on a busy network (peer-backed kernels are handled by the loop above).
	if evicted, derr := k.store.PurgeStaleDiscovery(ctx, cutoff); derr != nil {
		logger.Error("discovery.purge.failed", "error", derr)
	} else if evicted > 0 {
		logger.Info("discovery.purged", "kernels", evicted)
	}
	return purged, nil
}

// GetGossip returns this kernel's v0.13 gossip payload (§13): first-party identity, the kernel's
// own signed action manifests, and one page of evidence bundles ordered by effective time after
// cursor. When requesterKey names a known,
// non-suspended peer, the response also carries that peer's credit here (CounterpartyBalance, §13
// peer sync); nil for strangers, suspended keys, and anonymous pulls.
// reasonRemoteReceiptInvalidPrefix marks a quarantined remote-proxy settlement: a validly-signed
// receipt that breached the §13 settlement invariants (settled with charge 0, reserve kept locked,
// reconciled out of band). settleRemoteCall writes it as the transaction reason; gossipRowIsExecuted
// reads it to keep a quarantined receipt out of gossip evidence (§13 gossip-eligibility).
const reasonRemoteReceiptInvalidPrefix = "remote receipt invalid: "

func (k *Kernel) GetGossip(ctx context.Context, requesterKey, cursor string) (*GossipResponse, error) {
	ourKey := k.ourKeyB64()
	handle, _ := k.store.GetConfig(ctx, "kernel_handle")
	// The kernel's self-description is @sys's user description (§13): one primitive, not a config key.
	var about string
	sys, _ := k.store.ReadUserByHandle(ctx, "sys")
	if sys != nil {
		about = sys.Description
	}

	actions, err := k.store.ListVisibleActions(ctx, false, 100, 0)
	if err != nil {
		return nil, err
	}
	var manifests []*ActionManifest
	owners := map[string]*Account{} // one read per owner across the manifest loop
	for _, a := range actions {
		if k.exportable(a) != nil {
			continue
		}
		ow, cached := owners[a.OwnerUserID]
		if !cached {
			ow, _ = k.store.ReadUser(ctx, a.OwnerUserID)
			owners[a.OwnerUserID] = ow
		}
		if ow == nil {
			continue
		}
		m, merr := k.buildManifest(ctx, a, ow)
		if merr != nil || m == nil {
			continue
		}
		manifests = append(manifests, m)
	}

	bundles, nextCursor, err := k.gossipEvidencePage(ctx, ourKey, cursor)
	if err != nil {
		return nil, err
	}

	railAddr, railProof := k.RailIdentity(ctx)
	resp := &GossipResponse{
		PublicKey:       ourKey,
		Handle:          handle,
		About:           about,
		Network:         k.cfg.Network.Name,
		NetworkDigest:   k.cfg.Network.Digest,
		RailAddress:     railAddr,
		RailProof:       railProof,
		ActionManifests: manifests,
		Evidence:        bundles,
		NextCursor:      nextCursor,
	}
	return resp, nil
}

// buildEvidenceReceipt builds the wire-only signed projection of one of this kernel's receipts (§13).
// For an own action the subject is (ourKey, receipt.action_id); for a proxy call the store supplied
// the peer subject in row.Subject*. RemoteReceiptHash is receiptHash of the STORED remote receipt
// (canonical JSON — the one definition shared with the serving kernel's ReceiptHash), never the raw
// tx.RemoteReceiptHash. Signed under the evidence_receipt domain.
func (k *Kernel) buildEvidenceReceipt(ourKey string, row *GossipReceiptRow) (*EvidenceReceipt, error) {
	rh, err := ReceiptHash(row.Receipt)
	if err != nil {
		return nil, err
	}
	subjectKernel := row.SubjectKernelPublicKey
	if subjectKernel == "" {
		subjectKernel = ourKey // own execution evidence
	}
	er := &EvidenceReceipt{
		ReceiptHash:                 rh,
		SubjectKernelPublicKey:      subjectKernel,
		SubjectActionID:             row.SubjectActionID,
		CounterpartyKernelPublicKey: row.CounterpartyKernelPublicKey,
		Status:                      row.Receipt.Status,
		StartedAt:                   row.Receipt.StartedAt,
		CreatedAt:                   row.Receipt.CreatedAt,
	}
	if row.RemoteReceiptJSON != "" {
		h, herr := receiptHashFromJSON(row.RemoteReceiptJSON)
		if herr != nil {
			return nil, herr
		}
		er.RemoteReceiptHash = h
	}
	cp := *er
	cp.Signature = ""
	sig, err := k.cfg.Network.sign(k.cfg.SigningKey, sigDomainEvidenceReceipt, cp)
	if err != nil {
		return nil, err
	}
	er.Signature = sig
	return er, nil
}

// gossipRowIsExecuted reports whether a gossip-candidate receipt records an admitted execution, so it
// is gossip-eligible (§13 leg-(b) exclusions). Own-execution leg-(a) rows carry no remote receipt and
// are always executed. For a receipt-backed leg-(b) proxy row (locally-manufactured settlements are
// already dropped in SQL by their empty remote_receipt_json), two non-executions are excluded: a
// quarantined invalid receipt (our transaction reason carries the quarantine prefix) and a signed
// rejection — the serving kernel sets a rejection receipt's tx_id to the caller's idempotency_key, so
// tx_id == our dispatched key distinguishes any rejection from a genuine execution at any price, 0
// included.
// isRejectionReceipt reports whether a remote receipt records a refusal rather than an execution.
// The serving kernel sets a rejection receipt's tx_id to the caller's idempotency_key (§6 P4), and
// that is the only marker that holds at any price: an executed failure consuming nothing charges 0
// too, and may even carry transport status 402. One definition, used by gossip eligibility and by
// settlement classification alike.
func isRejectionReceipt(receiptTxID, dispatchedIdempotencyKey string) bool {
	return dispatchedIdempotencyKey != "" && receiptTxID == dispatchedIdempotencyKey
}

func gossipRowIsExecuted(row *GossipReceiptRow) bool {
	if row.RemoteReceiptJSON == "" {
		return true // leg (a): own execution
	}
	var rr struct {
		TxID   string   `json:"tx_id"`
		Status TxStatus `json:"status"`
	}
	_ = json.Unmarshal([]byte(row.RemoteReceiptJSON), &rr)
	if isRejectionReceipt(rr.TxID, row.IdempotencyKey) {
		return false
	}
	// Quarantined invalid receipt (§13): we settled it as a failure while keeping the reserve locked.
	// The UNFORGEABLE signal is a status disagreement — the peer claimed success but we recorded a
	// failure (a mispriced/inconsistent success receipt) — which a peer cannot fake to pass a bad
	// receipt off as executed. The reason prefix additionally catches failure-receipt quarantines; a
	// peer setting that exact reason could at most suppress one of its OWN failures, never inflate.
	if row.Receipt != nil {
		if rr.Status == TxSuccess && row.Receipt.Status == TxFailure {
			return false
		}
		if strings.HasPrefix(row.Receipt.Reason, reasonRemoteReceiptInvalidPrefix) {
			return false
		}
	}
	return true
}

// gossipEvidencePage builds one ordered evidence page after cursor, plus the next cursor (§13). A
// non-executed leg-(b) row (rejection or quarantine) is skipped but still advances the cursor past
// it — sender-side gaps are expected and never an endless replay (§13 cursor).
func (k *Kernel) gossipEvidencePage(ctx context.Context, ourKey, cursor string) ([]EvidenceBundle, string, error) {
	rows, err := k.store.ListReceiptsForGossip(ctx, cursor, gossipEvidencePageSize)
	if err != nil {
		return nil, "", err
	}
	bundles := make([]EvidenceBundle, 0, len(rows))
	next := cursor
	for _, row := range rows {
		next = row.Cursor
		if !gossipRowIsExecuted(row) {
			continue
		}
		// A delegated-auth action is never described abroad (§6 P6, §8 D10), so evidence must not
		// name it either: the exclusion is one rule, and gossiping usage of a capability no peer can
		// call or even see would disclose its existence and volume for no consumer. Own-execution
		// rows only — a leg-(b) subject is the peer's action, governed by that peer.
		if row.RemoteReceiptJSON == "" && row.SubjectActionID != "" {
			if a, aerr := k.store.ReadAction(ctx, row.SubjectActionID); aerr == nil && k.isDelegatedAuth(a) {
				continue
			}
		}
		er, berr := k.buildEvidenceReceipt(ourKey, row)
		if berr != nil {
			return nil, "", berr
		}
		b := EvidenceBundle{EvidenceReceipt: er}
		if row.Rating != nil && row.Rating.RatedReceiptHash != "" {
			re, perr := k.projectRating(row.Rating)
			if perr != nil {
				return nil, "", perr
			}
			b.Rating = re
		}
		bundles = append(bundles, b)
	}
	return bundles, next, nil
}

// projectRating drops the identity fields of a locally-created rating and re-signs the projection
// with the platform key under sigDomainRating (§13): the gossiping kernel is always the rater, so
// it holds the key. Signed per serve, never stored.
func (k *Kernel) projectRating(r *Rating) (*RatingEvidence, error) {
	re := &RatingEvidence{
		Rating:           r.Rating,
		Note:             r.Note,
		RatedReceiptHash: r.RatedReceiptHash,
		CreatedAt:        r.CreatedAt,
	}
	sig, err := k.cfg.Network.sign(k.cfg.SigningKey, sigDomainRating, re)
	if err != nil {
		return nil, err
	}
	re.Signature = sig
	return re, nil
}

// ReadKernel returns the kernel row for a public key, or nil if unknown.
func (k *Kernel) ReadKernel(ctx context.Context, publicKey string) (*RemoteKernel, error) {
	return k.store.ReadKernel(ctx, publicKey)
}

// ReadKernelByPetname resolves a bound petname to its kernel; nil when no kernel holds it.
func (k *Kernel) ReadKernelByPetname(ctx context.Context, petname string) (*RemoteKernel, error) {
	return k.store.ReadKernelByPetname(ctx, NormalizeHandle(petname))
}

// DiscoveryDocsForKernel returns the locally-cached discovery docs for one source kernel
// (§13). Regenerable — empty until the next gossip pull.
func (k *Kernel) DiscoveryDocsForKernel(ctx context.Context, publicKey string) ([]*DiscoveryDoc, error) {
	all, err := k.store.ListDiscoveryDocs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*DiscoveryDoc, 0)
	for _, d := range all {
		if d.KernelPublicKey == publicKey {
			out = append(out, d)
		}
	}
	return out, nil
}

// PeerAction is one action a peer offers, as `admin inspect` reports it. Price is the indicative
// local all-in — the peer's serving price plus this kernel's import fee — the same number and the
// same definition sys/lookup shows, so two surfaces never quote one action differently; a resolve
// re-quotes authoritatively before money moves (§13).
type PeerAction struct {
	ActionID     string         `json:"action_id"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
	Price        int64          `json:"price"`
	Indicative   bool           `json:"indicative"`
}

// indicativePrice adds this kernel's import fee to a peer's serving price, the one definition of the
// number both catalog sources and sys/lookup quote (§13). False when the arithmetic is out of range.
func (k *Kernel) indicativePrice(serving int64) (int64, bool) {
	p, err := k.econ.LocalPrice(serving)
	return p, err == nil
}

// PeerCatalog projects the signed manifests of a live gossip pull. Separate from PeerCatalogCached
// rather than one call switching on a nil slice: a live peer that exports nothing legitimately sends
// no manifests, and an emptiness test would silently answer that with stale cache under source
// "live". The source is the caller's knowledge, so the caller names it.
func (k *Kernel) PeerCatalog(manifests []*ActionManifest) []*PeerAction {
	out := make([]*PeerAction, 0, len(manifests))
	for _, m := range manifests {
		serving, err := k.econ.ServingPrice(m.Price, m.RemoteBPS)
		if err != nil {
			continue // a manifest priced out of range is skipped at ingest too (§6 P6)
		}
		price, ok := k.indicativePrice(serving)
		if !ok {
			continue
		}
		out = append(out, &PeerAction{ActionID: m.ActionID, Name: m.Name, Description: m.Description,
			InputSchema: m.InputSchema, OutputSchema: m.OutputSchema, Price: price, Indicative: true})
	}
	return out
}

// PeerCatalogCached projects the discovery cache into the same shape, for an unreachable peer. A
// cached price is still a real price — rendering the untagged ServingPrice would report every action
// free — and it is the same quantity the live projection carries, so the two differ only in freshness.
func (k *Kernel) PeerCatalogCached(ctx context.Context, publicKey string) ([]*PeerAction, error) {
	docs, err := k.DiscoveryDocsForKernel(ctx, publicKey)
	if err != nil {
		return nil, err
	}
	out := make([]*PeerAction, 0, len(docs))
	for _, d := range docs {
		price, ok := k.indicativePrice(d.ServingPrice)
		if !ok {
			continue
		}
		out = append(out, &PeerAction{ActionID: d.ActionID, Name: d.Name, Description: d.Description,
			InputSchema: d.InputSchema, OutputSchema: d.OutputSchema, Price: price, Indicative: true})
	}
	return out, nil
}

// GossipCursor returns the persisted evidence high-watermark for a peer (§13 peer sync); empty when
// the peer is unknown or has never been pulled.
func (k *Kernel) GossipCursor(ctx context.Context, publicKey string) string {
	if dk, err := k.store.ReadKernel(ctx, publicKey); err == nil && dk != nil {
		return dk.GossipCursor
	}
	return ""
}

// SetGossipCursor advances a peer's persisted evidence high-watermark (§13 peer sync).
func (k *Kernel) SetGossipCursor(ctx context.Context, publicKey, cursor string) error {
	return k.store.SetGossipCursor(ctx, publicKey, cursor)
}

// RecordKernelContact persists one contact observation (§13) keyed by public key: a success advances
// last_seen, a failure advances last_contact_failed_at. A no-op for an unknown key. No moderation
// check: suspension governs whose requests this kernel answers, while reachability is a fact about
// the network that gates nothing — freezing it would only make the operator's view of a suspended
// peer wrong. Display-only cache.
func (k *Kernel) RecordKernelContact(ctx context.Context, publicKey string, ok bool) error {
	return k.store.RecordKernelContact(ctx, publicKey, ok, time.Now().UTC())
}

// CreateSignedRejectionReceipt produces a signed Receipt (status=failure, gross=0) for an inbound
// federation call the receiver refuses before execution. No transaction is created; the receipt is
// signed with the kernel's Ed25519 key so the caller can verify the rejection was authentic. reason
// records why (e.g. "counterparty denied", "insufficient balance", "action inactive") so the caller's
// settled failure is legible rather than always reading "denied". refreshProxy marks a cache fault
// (contract-hash mismatch, non-executable action) so the origin invalidates its cached proxy (§13).
func (k *Kernel) CreateSignedRejectionReceipt(counterpartyID, actionParam, argsHash, idempotencyKey, reason string, refreshProxy bool) (*Receipt, error) {
	if err := k.requireReceiptSigningReady(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	r := &Receipt{
		ID:           uuid.New().String(),
		IssuerUserID: k.cfg.IssuerUserID,
		TxID:         idempotencyKey, // no real TxID; idempotency key identifies this rejection
		ActionID:     actionParam,    // the refused action's id, matching the caller's remote_action_id
		CallerUserID: counterpartyID,
		ArgsHash:     argsHash,
		Status:       TxFailure,
		Gross:        0,
		Net:          0,
		Fee:          0,
		RefreshProxy: refreshProxy,
		Reason:       reason,
		StartedAt:    now,
		CreatedAt:    now,
	}
	sig, err := signReceipt(k.cfg.Network, k.cfg.SigningKey, r)
	if err != nil {
		return nil, err
	}
	r.Signature = sig
	return r, nil
}

// AccumulateGossip ingests one v0.13 gossip page (§13): it refreshes the slim discovered-kernel row,
// rebuilds that kernel's searchable discovery docs from the verified first-party catalog snapshot, and
// stores each verified evidence bundle. It returns the page's NextCursor so the discovery loop can
// persist the peer's evidence high-watermark. Nothing here grants callability — a lookup selection
// still resolves and verifies from the home kernel.
func (k *Kernel) AccumulateGossip(ctx context.Context, gossip *GossipResponse, introducerPublicKey string) (string, error) {
	if gossip.PublicKey == "" {
		return "", ErrInvalidInput.Wrap("gossip missing public_key")
	}
	// Authenticated-key binding (§13): the reply must be signed-by-transport as the key we dialed;
	// a responder cannot claim a different identity and write under it. The caller passes the
	// Noise-authenticated key it dialed as the introducer.
	if introducerPublicKey != "" && gossip.PublicKey != introducerPublicKey {
		return "", ErrInvalidInput.Wrap("gossip public_key does not match the authenticated peer")
	}
	// A verified pull requires a valid (bare) identity (§3, §14); an empty or non-bare handle is a
	// failed pull, not a success — so it never resets attempts and counts toward stub eviction.
	if err := validateHandle(gossip.Handle); err != nil {
		return "", ErrInvalidInput.Wrap("gossip handle invalid")
	}
	// A reply from another world is not ours to accumulate: nothing it carries could verify here,
	// and adopting its catalog would offer actions no call could ever pay for (D23).
	if gossip.NetworkDigest != "" && gossip.NetworkDigest != k.cfg.Network.Digest {
		return "", ErrInvalidInput.Wrapf("peer serves network %q, not ours", gossip.Network)
	}
	// Where the rail has addresses, a kernel must prove it controls the one it advertises: an
	// address merely declared could name a third party's and claim their payment (D23).
	if _, err := k.verifyRailIdentity(gossip.PublicKey, gossip.RailAddress, gossip.RailProof); err != nil {
		return "", ErrInvalidInput.Wrap("peer rail address is unproven")
	}
	now := time.Now().UTC()
	if err := k.observeKernel(ctx, gossip.PublicKey, gossip.Handle, gossip.About, gossip.RailAddress, gossip.RailProof); err != nil {
		return "", err
	}

	// Rebuild discovery docs from the verified catalog snapshot (replace-all per source kernel).
	const maxGossipElements = 10000
	docs := make([]*DiscoveryDoc, 0, len(gossip.ActionManifests))
	for i, m := range gossip.ActionManifests {
		if i >= maxGossipElements {
			break
		}
		if m == nil || k.cfg.Network.VerifyManifestSignature(gossip.PublicKey, m) != nil {
			continue // only verified first-party manifests are indexed
		}
		// The catalog price a browser sees without resolving: the peer's signed serving markup now,
		// the origin's import fee at read time (§13). An unrepresentable one skips the manifest.
		sp, perr := k.econ.ServingPrice(m.Price, m.RemoteBPS)
		if perr != nil {
			continue
		}
		d := &DiscoveryDoc{
			KernelPublicKey: gossip.PublicKey,
			Handle:          m.OwnerHandle,
			Description:     m.Description,
			ActionID:        m.ActionID,
			Name:            m.Name,
			InputSchema:     m.InputSchema,
			OutputSchema:    m.OutputSchema,
			ServingPrice:    sp,
			ObservedAt:      now,
		}
		if k.llm != nil {
			if vec, eerr := k.llm.Embed(ctx, m.Name+" "+m.Description); eerr == nil {
				d.Embedding = vec
			}
		}
		docs = append(docs, d)
	}
	if err := k.store.ReplaceDiscoveryDocs(ctx, gossip.PublicKey, docs); err != nil {
		return "", err
	}

	// Store each verified evidence bundle.
	for i, b := range gossip.Evidence {
		if i >= maxGossipElements {
			break
		}
		if err := k.ingestEvidenceBundle(ctx, gossip.PublicKey, b, now); err != nil {
			k.log.With(ctx).Warn("gossip.evidence.rejected", "issuer", gossip.PublicKey, "error", err)
		}
	}

	return gossip.NextCursor, nil
}

// ingestEvidenceBundle verifies and stores one evidence bundle from issuerKey (§13). It verifies the
// evidence-receipt signature under the evidence_receipt domain against the gossiping key, requires a
// present rating (if any) to be signed by the same key and to hash-match the receipt, drops transfer
// subjects, and upserts under the late-rating/equivocation transitions.
func (k *Kernel) ingestEvidenceBundle(ctx context.Context, issuerKey string, b EvidenceBundle, now time.Time) error {
	er := b.EvidenceReceipt
	if er == nil {
		return ErrInvalidInput.Wrap("evidence bundle missing receipt")
	}
	pub, err := decodeRemotePublicKey(issuerKey)
	if err != nil {
		return err
	}
	cp := *er
	cp.Signature = ""
	if err := k.cfg.Network.verify(pub, sigDomainEvidenceReceipt, cp, er.Signature); err != nil {
		return ErrUnauthorized.Wrap("evidence receipt signature invalid")
	}
	row := &EvidenceRow{
		IssuerPublicKey:             issuerKey,
		ReceiptHash:                 er.ReceiptHash,
		SubjectKernelPublicKey:      er.SubjectKernelPublicKey,
		SubjectActionID:             er.SubjectActionID,
		CounterpartyKernelPublicKey: er.CounterpartyKernelPublicKey,
		RemoteReceiptHash:           er.RemoteReceiptHash,
		ReceiptCreatedAt:            er.CreatedAt,
		EffectiveAt:                 er.CreatedAt,
		ObservedAt:                  now,
	}
	erJSON, _ := json.Marshal(er)
	row.EvidenceReceiptJSON = string(erJSON)
	if b.Rating != nil {
		// Wire-ingress rule: a gossiped rating projection must carry a hash matching the receipt and
		// be signed (v2 domain) by the issuing kernel. It carries no rater/transaction identity (§13).
		if b.Rating.RatedReceiptHash == "" || b.Rating.RatedReceiptHash != er.ReceiptHash {
			return ErrInvalidInput.Wrap("rating does not match its receipt hash")
		}
		rc := *b.Rating
		rc.Signature = ""
		if err := k.cfg.Network.verify(pub, sigDomainRating, rc, b.Rating.Signature); err != nil {
			return ErrUnauthorized.Wrap("rating signature invalid")
		}
		if err := validRating(b.Rating.Rating, b.Rating.Note); err != nil {
			return err
		}
		rJSON, _ := json.Marshal(b.Rating)
		row.RatingJSON = string(rJSON)
		row.EffectiveAt = b.Rating.CreatedAt
	}
	return k.store.UpsertEvidence(ctx, row)
}

func decodeRemotePublicKey(publicKey string) (ed25519.PublicKey, error) {
	key, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil {
		return nil, ErrInvalidInput.Wrap("public_key must be base64url")
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, ErrInvalidInput.Wrap("public_key must be a 32-byte Ed25519 public key")
	}
	return ed25519.PublicKey(key), nil
}

// ---- Federation import (uses reconcileImport from kernel.go) ----

// remoteManifestHash returns the manifest hash for a remote_proxy action: a hex-encoded
// SHA-256 over the manifest contract fields defined in §12.2 (description, input/output
// schemas, price, kind, artifact_hash, and execution identity: action_id, name,
// owner_handle). Stats and updated_at are excluded because they are not contract fields.
func remoteManifestHash(m ActionManifest) string {
	inputJSON, _ := CanonicalJSON(m.InputSchema)
	outputJSON, _ := CanonicalJSON(m.OutputSchema)
	// owner_id (stable identity) and remote_bps (provider pricing) are contract fields; owner_handle
	// is display metadata, so a remote handle rename never re-keys the cache or deactivates a proxy.
	payload, _ := CanonicalJSON(map[string]any{
		"action_id":     m.ActionID,
		"artifact_hash": m.ArtifactHash,
		"description":   m.Description,
		// Reserved: the value channel is local to a kernel, so no manifest declares an effect. The key
		// stays in the payload at its empty value so every hash already cached by a peer keeps matching
		// and no proxy is re-keyed by this removal.
		"effect":        "",
		"input_schema":  string(inputJSON),
		"kind":          string(m.Kind),
		"name":          m.Name,
		"output_schema": string(outputJSON),
		"owner_id":      m.OwnerID,
		"price":         m.Price,
		"remote_bps":    m.RemoteBPS,
	})
	h := sha256.Sum256(payload)
	return hex.EncodeToString(h[:])
}

// ImportPeerAction imports one signed manifest without a superuser gate (its authority is the
// verified manifest signature) and activates it as a local proxy — the §13 subscription-free
// resolve path used by lazy cross-kernel calls. Returns the resulting proxy action.
func (k *Kernel) ImportPeerAction(ctx context.Context, remoteUserID string, m ActionManifest) (*Action, error) {
	res, err := k.importRemoteActionCore(ctx, remoteUserID, m)
	if err != nil {
		return nil, err
	}
	var a *Action
	switch {
	case len(res.Created) > 0:
		a = res.Created[0]
	case len(res.Updated) > 0:
		a = res.Updated[0]
	case len(res.Unchanged) > 0:
		a = res.Unchanged[0]
	default:
		return nil, ErrNotFound.Wrap("import produced no action")
	}
	// Enable and make callable by local users (visibility=local), mirroring the resolve path.
	// The manifest is already signature-verified, so no further gate is needed.
	if !a.Active || a.Visibility != VisibilityLocal {
		a.Active = true
		a.Visibility = VisibilityLocal
		a.UpdatedAt = time.Now().UTC()
		if err := k.store.UpdateAction(ctx, a); err != nil {
			return nil, err
		}
	}
	// Activation is what indexes an action for lookup, but a proxy activates here rather than
	// through SetActive (which rejects kernel-managed rows), so the index write must happen here
	// too. Without it a resolved action is unfindable: the proxy also shadows the discovery row
	// it replaces (§9), so buying an action would remove it from search.
	k.indexForLookup(ctx, a)
	return a, nil
}

func (k *Kernel) importRemoteActionCore(ctx context.Context, remoteUserID string, m ActionManifest) (*ImportResult, error) {
	remoteUser, err := k.store.ReadUser(ctx, remoteUserID)
	if err != nil {
		return nil, err
	}
	if remoteUser.KernelPublicKey == "" {
		return nil, ErrInvalidInput.Wrap("user is not a remote kernel")
	}
	if m.ActionID == "" {
		return nil, ErrInvalidInput.Wrap("manifest missing action_id")
	}
	if err := k.cfg.Network.VerifyManifestSignature(remoteUser.KernelPublicKey, &m); err != nil {
		return nil, err
	}
	switch {
	case m.Name == "":
		return nil, ErrInvalidInput.Wrap("manifest missing name")
	case m.OwnerHandle == "":
		return nil, ErrInvalidInput.Wrap("manifest missing owner_handle")
	case strings.ContainsAny(m.OwnerHandle, "@/"):
		// owner_handle is concatenated into the proxy name/source below; a sigil or slash would
		// corrupt the reference, so a bare handle is required (§3, §14) — not silently rewritten.
		return nil, ErrInvalidInput.Wrap("manifest owner_handle must be a bare handle")
	case m.Description == "":
		return nil, ErrInvalidInput.Wrap("manifest missing description")
	case m.InputSchema == nil:
		return nil, ErrInvalidInput.Wrap("manifest missing input_schema")
	case m.OutputSchema == nil:
		return nil, ErrInvalidInput.Wrap("manifest missing output_schema")
	case string(m.Kind) == "":
		return nil, ErrInvalidInput.Wrap("manifest missing kind")
	case m.Stats == nil:
		return nil, ErrInvalidInput.Wrap("manifest missing stats")
	case m.UpdatedAt.IsZero():
		return nil, ErrInvalidInput.Wrap("manifest missing updated_at")
	}
	if err := ValidateSchema(m.InputSchema); err != nil {
		return nil, ErrInvalidInput.Wrapf("manifest input_schema invalid: %v", err)
	}
	if err := ValidateSchema(m.OutputSchema); err != nil {
		return nil, ErrInvalidInput.Wrapf("manifest output_schema invalid: %v", err)
	}
	if m.Price < 0 {
		return nil, ErrInvalidInput.Wrap("price must be non-negative")
	}
	// For a key-addressed proxy, Source holds only the remote action ref (@owner/name).
	// The peer is identified by remoteUser.KernelPublicKey; the federation transport resolves that
	// key to a live path and supplies this kernel's own key as the signed counterparty (§13).
	source := m.OwnerHandle + "/" + m.Name

	existingByKey := map[string]*Action{}
	if existing, err := k.store.ReadActionByOwnerRemoteID(ctx, remoteUserID, m.ActionID); err == nil {
		existingByKey[m.ActionID] = existing
	}

	contentHash := remoteManifestHash(m)
	// Owner-qualified (rendered remoteowner@mount/name) so same-named actions from different owners
	// on the peer don't collide.
	name := NormalizeHandle(m.OwnerHandle) + "/" + m.Name
	// Proxy price is the two-step markup (§13): sr = mp + ceil(mp·remote_bps/10000) is the
	// serving-kernel markup (a signed manifest field) and the cross-kernel obligation ceiling;
	// q = sr + ceil(sr·import_bps/10000) adds the origin's locally-retained import fee, so the local
	// user sees one authenticated price bounding the whole remote call.
	rbps := m.RemoteBPS
	basePrice := m.Price // the seller's number, kept so the total can be re-derived (§16)
	sr, err := k.econ.ServingPrice(m.Price, rbps)
	if err != nil {
		return nil, err
	}
	proxyPrice, err := k.econ.LocalPrice(sr)
	if err != nil {
		return nil, err
	}
	incoming := []incomingOp{{
		key:  m.ActionID,
		name: name,
		apply: func(a *Action) {
			a.Name = name
			a.Source = source
			a.Price = proxyPrice
			a.Description = m.Description
			a.InputSchema = m.InputSchema
			a.OutputSchema = m.OutputSchema
			a.ArtifactHash = contentHash
			a.RemoteOwnerID = m.OwnerID
			a.RemoteBPS = &rbps
			a.BasePrice = &basePrice
		},
		// Identity and lifecycle only; apply writes the contract fields.
		new: func() *Action {
			now := time.Now().UTC()
			return &Action{
				ID:             uuid.New().String(),
				OwnerUserID:    remoteUserID,
				Kind:           KindRemoteProxy,
				Active:         false,
				Visibility:     VisibilityPrivate, // promoted to local on successful resolve (§8, below)
				RemoteActionID: m.ActionID,
				CreatedAt:      now,
				UpdatedAt:      now,
			}
		},
	}}

	// A manifest is signed as a whole, so its own content hash decides whether the served contract
	// moved — no field-by-field projection is needed on this side.
	plan := classifyImport(existingByKey, incoming, func(ex *Action, _ incomingOp) bool {
		return ex.ArtifactHash != contentHash
	})
	result, err := k.commitProxyImport(ctx, plan)
	if err != nil {
		return nil, err
	}

	if len(result.Created) > 0 {
		k.log.With(ctx).Info("action.imported_remote", "action_id", result.Created[0].ID, "name", result.Created[0].Name, "status", "success")
	} else if len(result.Updated) > 0 {
		k.log.With(ctx).Info("action.reimported_remote", "action_id", result.Updated[0].ID, "name", result.Updated[0].Name, "status", "success")
	} else {
		k.log.With(ctx).Info("action.remote_unchanged", "remote_action_id", m.ActionID, "status", "success")
	}
	return result, nil
}

// GetActionManifest returns a signed manifest for a public active action.
// Manifests are only available for actions that are both active and public.
// exportable reports whether an action may be served abroad: the one export predicate, shared by
// the manifest path and the gossip catalog (§13) so the two cannot drift apart.
func (k *Kernel) exportable(a *Action) error {
	if !a.Active || a.Visibility != VisibilityPublic {
		return ErrUnauthorized.Wrap("manifest only available for public active actions")
	}
	// Delegated-OAuth actions are never advertised: a remote peer's proxy user cannot complete a
	// browser consent, so importing one could only ever produce grant-required failures (§8/§13).
	if k.isDelegatedAuth(a) {
		return ErrUnauthorized.Wrap("delegated-OAuth actions are not served as manifests")
	}
	// Imported (remote_proxy) actions are never re-exported: a kernel serves manifests only for its
	// own actions, so friendship stays non-transitive — reaching a peer's imported action requires
	// friending its true owner directly (§13).
	if a.Kind == KindRemoteProxy {
		return ErrUnauthorized.Wrap("imported (remote-proxy) actions are not re-exported to peers")
	}
	return nil
}

func (k *Kernel) GetActionManifest(ctx context.Context, actionID string) (*ActionManifest, error) {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if err := k.exportable(a); err != nil {
		return nil, err
	}
	owner, err := k.store.ReadUser(ctx, a.OwnerUserID)
	if err != nil {
		return nil, err
	}
	return k.buildManifest(ctx, a, owner)
}

// buildManifest projects an already-exportable action and its owner into a signed manifest, so
// gossip can reuse one owner read across every manifest it serves.
func (k *Kernel) buildManifest(ctx context.Context, a *Action, owner *Account) (*ActionManifest, error) {
	stats, _ := k.store.ReadStats(ctx, a.ID)
	if stats == nil {
		stats = &Stats{ActionID: a.ID}
	}
	m := &ActionManifest{
		ActionID:     a.ID,
		OwnerID:      owner.ID,
		OwnerHandle:  owner.Handle,
		Name:         a.Name,
		RemoteBPS:    k.econ.RemoteBPS,
		Description:  a.Description,
		InputSchema:  a.InputSchema,
		OutputSchema: a.OutputSchema,
		Price:        a.Price,
		Kind:         a.Kind,
		ArtifactHash: a.ArtifactHash,
		UpdatedAt:    a.UpdatedAt,
		Stats:        stats,
	}
	sig, err := k.cfg.Network.SignManifest(k.cfg.SigningKey, m)
	if err != nil {
		return nil, err
	}
	m.Signature = sig
	return m, nil
}

// CurrentContractHash returns the contract hash a peer would cache for this action — the same
// remoteManifestHash the caller stored at resolve time (§8 If-Match). It errors for an action not
// currently servable as a manifest (inactive, non-public, delegated-auth, remote_proxy); the inbound
// handler then skips the explicit precondition and lets the non-executable path sign the rejection.
func (k *Kernel) CurrentContractHash(ctx context.Context, actionID string) (string, error) {
	m, err := k.GetActionManifest(ctx, actionID)
	if err != nil {
		return "", err
	}
	return remoteManifestHash(*m), nil
}

// SignManifest creates a base64url Ed25519 signature over the canonical ActionManifest.
func (n Network) SignManifest(key ed25519.PrivateKey, m *ActionManifest) (string, error) {
	cp := *m
	cp.Signature = ""
	return n.sign(key, sigDomainManifest, cp)
}

// VerifyManifestSignature checks that m.Signature was produced by the private key
// corresponding to pubKeyB64 (base64url Ed25519 public key), and that the monetary fields the
// origin prices from are in range. A signature proves authorship, not sanity: a signed negative
// price or out-of-range markup would otherwise flow into the proxy price. This is the single
// funnel for all three trust boundaries — gossip ingest, authoritative import, and resolve.
func (n Network) VerifyManifestSignature(pubKeyB64 string, m *ActionManifest) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return err
	}
	if m.Price < 0 || m.RemoteBPS < 0 || m.RemoteBPS > 10000 {
		return ErrInvalidInput.Wrapf("manifest price %d / remote_bps %d out of range", m.Price, m.RemoteBPS)
	}
	cp := *m
	cp.Signature = ""
	if err := n.verify(pub, sigDomainManifest, cp, m.Signature); err != nil {
		return ErrUnauthorized.Wrap("manifest signature is invalid")
	}
	return nil
}

// fedCallPayload is the canonical federation call payload signed and verified on both sides:
// JCS({action, args_hash, counterparty, expected_contract_hash, idempotency_key, recipient, timestamp}).
// recipient (the serving kernel's key) binds the request to one kernel (§13 replay defense);
// expected_contract_hash is the caller's cached contract hash (§8 If-Match).
// Signed request payloads (§13). Each type IS one protocol's contract shape: its JSON tags fix the
// key-set, and its signature domain (§12) keeps it from verifying as any other payload. JCS orders
// keys canonically, so these produce byte-identical bytes to the maps they replaced — pinned by the
// golden fixtures in sigfixture_test.go. Adding a payload is one struct plus one domain constant.
type fedCallPayload struct {
	Action               string `json:"action"`
	ArgsHash             string `json:"args_hash"`
	Commitment           string `json:"commitment,omitempty"`
	Counterparty         string `json:"counterparty"`
	ExpectedContractHash string `json:"expected_contract_hash"`
	IdempotencyKey       string `json:"idempotency_key"`
	Lottery              int64  `json:"lottery,omitempty"`
	Recipient            string `json:"recipient"`
	Timestamp            string `json:"timestamp"`
}

type stepCompletePayload struct {
	Counterparty   string `json:"counterparty"`
	IdempotencyKey string `json:"idempotency_key"`
	InputHash      string `json:"input_hash"`
	Recipient      string `json:"recipient"`
	StepID         string `json:"step_id"`
	Timestamp      string `json:"timestamp"`
}

// stepAuthPayload is the home kernel's attestation that its authenticated local user authorized
// completing a step (§13). No "scope" key: the sigDomainStepAuth prefix provides the separation.
type stepAuthPayload struct {
	Counterparty string `json:"counterparty"`
	Recipient    string `json:"recipient"`
	StepID       string `json:"step_id"`
	Timestamp    string `json:"timestamp"`
	UserID       string `json:"user_id"`
	// Superuser is the home kernel's word that this user is its operator: the scope a step
	// addressed to the kernel itself demands, which nothing else could tell the serving side.
	// Omitted when false, so an ordinary user's attestation is byte-identical to before.
	Superuser bool `json:"superuser,omitempty"`
}

// stepListPayload asks a peer which of its parked steps this kernel may complete. UserID names one
// principal on the requesting kernel when the question is asked on a user's behalf, as completion
// already is (step_auth): listing and completing then have the same granularity, so a user can see
// the work addressed to them rather than only its operator. Omitted for a kernel-level ask, whose
// canonical bytes are therefore unchanged.
type stepListPayload struct {
	Counterparty string `json:"counterparty"`
	Recipient    string `json:"recipient"`
	Timestamp    string `json:"timestamp"`
	UserID       string `json:"user_id,omitempty"`
}

// verifyPeerSignature verifies a peer's base64url Ed25519 signature over a payload in its own
// domain. It owns the cryptographic mechanics only; each exported verifier below translates a
// failure into its own typed, protocol-specific error.
func (n Network) verifyPeer(pubKeyB64, domain string, payload any, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	return n.verify(pub, domain, payload, sigB64)
}

// VerifyFederationSignature verifies an Ed25519 signature over the canonical federation call payload.
func (n Network) VerifyFederationSignature(pubKeyB64, action, counterparty, recipient, expectedContractHash, idempotencyKey, timestamp, argsHash, commitment string, lottery int64, sigB64 string) error {
	p := fedCallPayload{Action: action, ArgsHash: argsHash, Commitment: commitment,
		Counterparty: counterparty, ExpectedContractHash: expectedContractHash,
		IdempotencyKey: idempotencyKey, Lottery: lottery, Recipient: recipient, Timestamp: timestamp}
	if err := n.verifyPeer(pubKeyB64, sigDomainFedCall, p, sigB64); err != nil {
		return ErrUnauthenticated.Wrap("federation signature is invalid")
	}
	return nil
}

// SignFederationPayload creates a base64url Ed25519 signature over the canonical federation payload.
func (n Network) SignFederationPayload(key ed25519.PrivateKey, action, counterparty, recipient, expectedContractHash, idempotencyKey, timestamp, argsHash, commitment string, lottery int64) (string, error) {
	return n.sign(key, sigDomainFedCall, fedCallPayload{Action: action, ArgsHash: argsHash,
		Commitment: commitment, Counterparty: counterparty, ExpectedContractHash: expectedContractHash,
		IdempotencyKey: idempotencyKey, Lottery: lottery, Recipient: recipient, Timestamp: timestamp})
}

// SignStepPayload creates a base64url Ed25519 signature over the canonical step-completion payload
// — a key-set disjoint from every other signed Juice payload (§12, §13).
func (n Network) SignStepPayload(key ed25519.PrivateKey, stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash string) (string, error) {
	return n.sign(key, sigDomainStepComplete, stepCompletePayload{Counterparty: counterparty,
		IdempotencyKey: idempotencyKey, InputHash: inputHash, Recipient: recipient,
		StepID: stepID, Timestamp: timestamp})
}

// VerifyStepSignature verifies an Ed25519 signature over the canonical step-completion payload.
// recipient must be the verifying kernel's own public key.
func (n Network) VerifyStepSignature(pubKeyB64, stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash, sigB64 string) error {
	p := stepCompletePayload{Counterparty: counterparty, IdempotencyKey: idempotencyKey,
		InputHash: inputHash, Recipient: recipient, StepID: stepID, Timestamp: timestamp}
	if err := n.verifyPeer(pubKeyB64, sigDomainStepComplete, p, sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step signature is invalid")
	}
	return nil
}

// SignStepAuthPayload signs the home-kernel attestation that its authenticated local user (userID)
// authorized completing step stepID (§13). recipient binds it to the serving kernel, closing
// cross-kernel replay.
func (n Network) SignStepAuthPayload(key ed25519.PrivateKey, counterparty, recipient, userID, stepID, timestamp string, superuser bool) (string, error) {
	return n.sign(key, sigDomainStepAuth, stepAuthPayload{Counterparty: counterparty,
		Recipient: recipient, StepID: stepID, Timestamp: timestamp, UserID: userID, Superuser: superuser})
}

// VerifyStepAuthSignature verifies the attestation. recipient must be the verifying kernel's own key.
func (n Network) VerifyStepAuthSignature(pubKeyB64, counterparty, recipient, userID, stepID, timestamp string, superuser bool, sigB64 string) error {
	p := stepAuthPayload{Counterparty: counterparty, Recipient: recipient, StepID: stepID,
		Timestamp: timestamp, UserID: userID, Superuser: superuser}
	if err := n.verifyPeer(pubKeyB64, sigDomainStepAuth, p, sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step attestation is invalid")
	}
	return nil
}

// SignStepAuth stamps the current time and signs the step_auth attestation with this kernel's
// platform key. counterparty is this kernel's own key; recipient is the serving peer's key.
func (k *Kernel) SignStepAuth(counterparty, recipient, userID, stepID string, superuser bool) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = k.cfg.Network.SignStepAuthPayload(k.cfg.SigningKey, counterparty, recipient, userID, stepID, ts, superuser)
	return
}

// SignStepListPayload creates a base64url Ed25519 signature over the canonical step-list payload.
// The sigDomainStepList prefix keeps this key-set disjoint from every other signed payload (§12).
func (n Network) SignStepListPayload(key ed25519.PrivateKey, counterparty, recipient, timestamp, forUserID string) (string, error) {
	return n.sign(key, sigDomainStepList, stepListPayload{Counterparty: counterparty,
		Recipient: recipient, Timestamp: timestamp, UserID: forUserID})
}

// VerifyStepListSignature verifies an Ed25519 signature over the canonical step-list payload.
// recipient must be the verifying kernel's own public key.
func (n Network) VerifyStepListSignature(pubKeyB64, counterparty, recipient, timestamp, forUserID, sigB64 string) error {
	p := stepListPayload{Counterparty: counterparty, Recipient: recipient, Timestamp: timestamp, UserID: forUserID}
	if err := n.verifyPeer(pubKeyB64, sigDomainStepList, p, sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step signature is invalid")
	}
	return nil
}

// ourKeyB64 is this kernel's own public key in the base64url form every payload names it by.
func (k *Kernel) ourKeyB64() string {
	return base64.RawURLEncoding.EncodeToString(k.cfg.SigningKey.Public().(ed25519.PublicKey))
}
