package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

// receiptHash is SHA-256(CanonicalJSON(the full stored receipt, signature included)) hex — the ONE
// definition of a receipt's portable identity, used on both sides of the remote-receipt evidence
// join (§13). A rating hashes its receipt with this; an EvidenceReceipt's RemoteReceiptHash uses it;
// a receiver joins A's RemoteReceiptHash to B's ReceiptHash byte-for-byte. It must never be conflated
// with tx.RemoteReceiptHash, which hashes the RAW wire bytes for settlement-integrity and would not
// match a re-canonicalized hash (Go marshal order ≠ JCS order).
func receiptHash(r *Receipt) (string, error) {
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
	return receiptHash(&r)
}

// markedUpPrice applies one markup layer: base + ceil(base·bps/10000) (§13 pricing). It is the
// forward direction of the two markup layers and serves both — the serving markup
// (remote_bps, signed by the peer) and the origin import fee (import_bps, local policy).
//
// base·bps is computed as (base/10000)·bps + ceil((base%10000)·bps/10000) so the product never
// overflows: with bps ≤ 10000 the first term is ≤ base and the second is < 10^8. Prices are
// peer-supplied and every non-negative int64 price is legal (§3), so the only rejection is a
// result that cannot be represented.
func markedUpPrice(base, bps int64) (int64, error) {
	if base < 0 || bps < 0 || bps > 10000 {
		return 0, ErrInvalidInput.Wrapf("price %d and markup %d bps are out of range", base, bps)
	}
	markup := (base / 10000) * bps
	rem := ceilDiv((base%10000)*bps, 10000)
	if markup > math.MaxInt64-rem {
		return 0, ErrInvalidInput.Wrapf("markup of %d at %d bps overflows", base, bps)
	}
	markup += rem
	if base > math.MaxInt64-markup {
		return 0, ErrInvalidInput.Wrapf("marked-up price of %d at %d bps overflows", base, bps)
	}
	return base + markup, nil
}

// actionBasePrice is the seller's manifest price (mp) snapshotted on a proxy row. Falling back to
// the stored total is only reached for a pre-041 row, which re-resolves before it is next funded.
func actionBasePrice(a *Action) int64 {
	if a == nil {
		return 0
	}
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
	// from the action row or live config — a catalog price now floats with local policy, and a call
	// locked at one rate must settle at that rate. Nullable: 0 is a legitimate rate (a fee-free
	// import), so a pre-041 dispatch that recorded neither must stay distinguishable from one that
	// recorded zero. Nil ⇒ fall back to the old sources.
	RemoteBPS *int64 `json:"remote_bps,omitempty"`
	ImportBPS *int64 `json:"import_bps,omitempty"`
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
	importBPS int64
}

// price derives one action's local total in place.
func (s *pricedStore) price(a *Action) (*Action, error) {
	if a == nil || a.Kind != KindRemoteProxy || a.BasePrice == nil {
		return a, nil // non-proxy, or a pre-041 row whose stored total is still what it charges
	}
	sr, err := markedUpPrice(*a.BasePrice, actionRemoteBPS(a))
	if err != nil {
		return nil, err
	}
	q, err := markedUpPrice(sr, s.importBPS)
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

func (s *pricedStore) ListActionsByOwnerOpenAPISpec(ctx context.Context, ownerID, specURL string) ([]*Action, error) {
	return s.priceMany(s.Store.ListActionsByOwnerOpenAPISpec(ctx, ownerID, specURL))
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
func marshalDispatch(args map[string]any, stepID string, mp, gross int64, contractHash string, remoteBPS, importBPS int64) *string {
	b, _ := json.Marshal(dispatchPayload{
		Args: args, StepID: stepID, RemotePrice: mp, Gross: gross, ContractHash: contractHash,
		RemoteBPS: &remoteBPS, ImportBPS: &importBPS,
	})
	s := string(b)
	return &s
}

// dispatchedRates returns the rates a call was funded under, frozen on its dispatch record (§13
// price-snapshot): settlement and audit never read live config, so a rate change between dispatch
// and settlement cannot move this call's arithmetic. A nil field is a pre-041 row: use the default.
func (k *Kernel) dispatchedRates(dispatchJSON *string, defRemoteBPS, defImportBPS int64) (remoteBPS, importBPS int64, d dispatchPayload) {
	remoteBPS, importBPS = defRemoteBPS, defImportBPS
	if dispatchJSON == nil {
		return
	}
	if json.Unmarshal([]byte(*dispatchJSON), &d) != nil {
		return
	}
	if d.RemoteBPS != nil {
		remoteBPS = *d.RemoteBPS
	}
	if d.ImportBPS != nil {
		importBPS = *d.ImportBPS
	}
	return
}

// PeerStepView is what a remote peer may see of a step parked for it: the request, not the
// requester. Deliberately NOT the local step view — that one carries the creating action's name,
// the process owner's handle, and raw local ids, and a user identity crossing a kernel boundary is
// precisely what §13's encapsulation forbids. Each field is here because the completer needs it:
// partial_args is the payload channel (§14 has sys/message put its body there) and allowed_input is
// §14's substitute for reading a target action that may be private. One type serves both ends of the
// protocol — the serving kernel builds it, the buying kernel decodes it — so neither side can drift.
type PeerStepView struct {
	ID           string          `json:"id"`
	PartialArgs  json.RawMessage `json:"partial_args,omitempty"`
	AllowedInput map[string]any  `json:"allowed_input,omitempty"`
	Price        int64           `json:"price"`
	CreatedAt    time.Time       `json:"created_at"`
}

// NewPeerStepView projects one waiting step into the peer-facing shape (§13).
func (k *Kernel) NewPeerStepView(s *Step, action *Action) *PeerStepView {
	v := &PeerStepView{ID: s.ID, PartialArgs: s.PartialArgs, Price: s.Price, CreatedAt: s.CreatedAt}
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
	sigDomainSettleOpen      = "settle_open"
	sigDomainSettleFinish    = "settle_finish"
	sigDomainSettleReconcile = "settle_reconcile"
	sigDomainSettlementRec   = "settlement_record"
	sigDomainCapability      = "capability"
	sigDomainRecovery        = "recovery"
)

// domainPayload prepends the versioned domain tag to the JCS-canonical bytes of v, giving the
// exact byte string that is signed/verified under domain.
func domainPayload(domain string, v any) ([]byte, error) {
	canon, err := CanonicalJSON(v)
	if err != nil {
		return nil, ErrInternal.Wrapf("canonicalize: %v", err)
	}
	prefix := []byte("juice/v1/" + domain + "\n")
	return append(prefix, canon...), nil
}

// signJCS signs the domain-prefixed JCS-canonical form of v with key.
func signJCS(key ed25519.PrivateKey, domain string, v any) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	payload, err := domainPayload(domain, v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}

// verifyJCS checks that sigB64 is a valid Ed25519 signature over the domain-prefixed JCS-canonical
// form of v. Wire ingress must always pass the payload's own domain; the legacy (undomained)
// fallback for stored historical artifacts lives in verifyJCSStored.
func verifyJCS(pub ed25519.PublicKey, domain string, v any, sigB64 string) error {
	payload, err := domainPayload(domain, v)
	if err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !ed25519.Verify(pub, payload, sig) {
		return ErrUnauthorized.Wrap("signature is invalid")
	}
	return nil
}

// verifyJCSStored verifies a locally STORED signed artifact (a receipt or rating persisted before
// or after the v0.13 domain break), returning the signature_version that matched: 2 for a
// domain-prefixed signature, 1 for a legacy undomained one, 0 (with error) for neither. This
// keeps authentic pre-v0.13 audit records verifiable without re-signing them (§12). It must never
// be used on wire ingress — network traffic is domained-only.
func verifyJCSStored(pub ed25519.PublicKey, domain string, v any, sigB64 string) (int, error) {
	if err := verifyJCS(pub, domain, v, sigB64); err == nil {
		return 2, nil
	}
	canon, err := CanonicalJSON(v)
	if err != nil {
		return 0, ErrInternal.Wrapf("canonicalize: %v", err)
	}
	sig, derr := base64.RawURLEncoding.DecodeString(sigB64)
	if derr == nil && ed25519.Verify(pub, canon, sig) {
		return 1, nil
	}
	return 0, ErrUnauthorized.Wrap("signature is invalid")
}

// VerifyRemoteReceipt verifies the stored remote receipt for a remote-proxy transaction.
// Available to any party satisfying CanReadTransaction. Returns ErrInvalidState for
// non-remote-proxy transactions (no remote receipt stored).
//
// Checks:
//   - ReceiptHash: SHA-256(remote_receipt_json) == stored hash
//   - Signature: Ed25519 over JCS(receipt with Signature="") by peer key
//   - ActionID: receipt.action_id == tx.remote_action_id (or tx.action_id if no remote ID)
//   - Status: receipt.status == tx.status
//   - Charge: tx.net == receipt.charge + receipt.value + receipt.premium (bilateral payable to the peer)
//   - Premium: receipt.premium == ceil((receipt.charge+receipt.value)*remote_bps/10000) for the proxy-row snapshot
//   - SettlementArith: tx.fee == ceil(tx.net*import_bps/10000) for success, 0 for failure
//   - ArgsHash: receipt.args_hash == SHA-256(JCS(tx.args))
//   - ReplyHash: receipt.reply_hash == SHA-256(JCS(tx.result)) on success
func (k *Kernel) VerifyRemoteReceipt(ctx context.Context, subjectID, txID string) (*ReceiptVerification, error) {
	tv, err := k.ReadTransaction(ctx, subjectID, txID)
	if err != nil {
		return nil, err
	}
	tx := tv.Transaction
	if tx.RemoteReceiptJSON == "" {
		return nil, ErrInvalidState.Wrap("transaction has no remote receipt")
	}

	var r Receipt
	if err := json.Unmarshal([]byte(tx.RemoteReceiptJSON), &r); err != nil {
		return nil, ErrInternal.Wrapf("decode remote receipt: %v", err)
	}

	// Resolve the remote kernel's public key. tx.TargetUserID is the proxy user (remote peer).
	owner, err := k.store.ReadUser(ctx, tx.TargetUserID)
	if err != nil {
		return nil, err
	}

	var checks ReceiptChecks

	// 1. Hash integrity.
	checks.ReceiptHash = sha256Hex(tx.RemoteReceiptJSON) == tx.RemoteReceiptHash

	// 2. Signature. A stored remote receipt may predate the v0.13 domain break, so audit it with the
	// legacy fallback and surface which signature version matched (§12).
	sigVer := 0
	if owner.KernelPublicKey != "" {
		if pub, derr := decodeRemotePublicKey(owner.KernelPublicKey); derr == nil {
			cp := r
			cp.Signature = ""
			if v, verr := verifyJCSStored(pub, sigDomainReceipt, cp, r.Signature); verr == nil {
				sigVer = v
			}
		}
	}
	checks.Signature = sigVer > 0

	// 3. ActionID: receipt carries the remote action's ID.
	if tx.RemoteActionID != "" {
		checks.ActionID = r.ActionID == tx.RemoteActionID
	} else {
		checks.ActionID = r.ActionID == tx.ActionID
	}

	// 4. Status consistency.
	checks.Status = r.Status == tx.Status

	// 5. Charge: local tx.net (the EXECUTION obligation paid to the proxy) must equal receipt.charge +
	// receipt.premium. The value channel is settled separately on the caller's own reserve, not through
	// tx.net, so it is audited by its own check below (§13, the un-folded two-channel model).
	checks.Charge = tx.Net == r.Charge+r.Premium

	// 6. Premium: the execution serving markup must equal ceil(charge·remote_bps), and the value serving
	// markup ceil(value·remote_bps), for the manifest-snapshot rate on the proxy row — computed
	// separately (never the folded ceil((charge+value)·…), whose rounding merge is the bug).
	// Both rates come from the dispatch record, which froze them at the funding boundary (§13): the
	// action row's markup and local config both move, so auditing against them would fail a
	// historically-correct settlement after any rate change. Pre-041 traces fall back to the old
	// sources.
	rbps := int64(0)
	if act, aerr := k.store.ReadAction(ctx, tx.ActionID); aerr == nil && act.RemoteBPS != nil {
		rbps = *act.RemoteBPS
	}
	importBPS := k.cfg.ImportBPS
	if tr, terr := k.store.ReadTrace(ctx, tx.TraceID); terr == nil && tr != nil {
		rbps, importBPS, _ = k.dispatchedRates(tr.DispatchJSON, rbps, importBPS)
	}
	checks.Premium = r.Premium == ceilDiv(r.Charge*rbps, 10000)

	// 7. Settlement arithmetic: the origin import fee on the actual obligation (tx.net = paid).
	if tx.Status == TxSuccess {
		checks.SettlementArith = tx.Fee == ceilDiv(tx.Net*importBPS, 10000)
	} else {
		checks.SettlementArith = tx.Fee == 0
	}

	// 7b. Charge ceiling (§13): the caller can never be charged more than the local price it
	// authenticated and locked before dispatch.
	checks.ChargeCeiling = r.Charge+r.Premium <= tx.Gross

	// 7. Refund conservation: stored refund must equal gross − net − fee exactly.
	checks.RefundConservation = tx.Refund == tx.Gross-tx.Net-tx.Fee

	// 8. Args hash.
	if h, hashErr := jcsHashStr(string(tx.ArgsJSON)); hashErr == nil {
		checks.ArgsHash = r.ArgsHash == h
	}

	// 9. Reply hash (only meaningful on success).
	if tx.Status == TxSuccess {
		if h, hashErr := jcsHashStr(string(tx.ReplyJSON)); hashErr == nil {
			checks.ReplyHash = r.ReplyHash == h
		}
	} else {
		checks.ReplyHash = true // not applicable on failure
	}

	valid := checks.ReceiptHash && checks.Signature && checks.ActionID &&
		checks.Status && checks.Charge && checks.Premium && checks.SettlementArith &&
		checks.ChargeCeiling && checks.RefundConservation && checks.ArgsHash && checks.ReplyHash

	return &ReceiptVerification{
		TransactionID:         txID,
		Valid:                 valid,
		RemoteKernelHandle:    owner.Handle,
		RemoteKernelPublicKey: owner.KernelPublicKey,
		SignatureVersion:      sigVer,
		Checks:                checks,
		Receipt:               &r,
	}, nil
}

// ReceiptSigningBytes returns the exact domain-prefixed bytes a receipt signature covers (§12) — the
// serving kernel signs these; the origin verifies them. Exposed so an external signer (or a test)
// reconstructs the identical payload.
func ReceiptSigningBytes(r *Receipt) ([]byte, error) {
	cp := *r
	cp.Signature = ""
	return domainPayload(sigDomainReceipt, cp)
}

// verifyRemoteReceiptSignature checks the receipt's Ed25519 signature against pubKeyB64. Fails
// closed on an empty key: a missing key must never let an unverified receipt pass as valid (§13).
func verifyRemoteReceiptSignature(r *Receipt, pubKeyB64 string) error {
	if pubKeyB64 == "" {
		return ErrInvalidState.Wrap("peer public key is not configured; cannot verify receipt signature")
	}
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return err
	}
	cp := *r
	cp.Signature = ""
	return verifyJCS(pub, sigDomainReceipt, cp, r.Signature)
}

// parseAndVerifyRemoteReceipt parses receiptJSON and enforces the settlement preconditions:
// signature, action_id, and args_hash must all match what we requested. Returns ErrTimeout
// (keep-trace-open) on absent, unparseable, invalidly signed, or mismatched receipts so the
// trace is never settled against a receipt that fails the invariants VerifyRemoteReceipt audits.
// Settlement must only proceed when this function returns without error.
func parseAndVerifyRemoteReceipt(receiptJSON, pubKeyB64, expectedActionID, expectedArgsHash string) (*Receipt, error) {
	if receiptJSON == "" {
		return nil, ErrTimeout.Wrap("remote receipt pending")
	}
	var r Receipt
	if err := json.Unmarshal([]byte(receiptJSON), &r); err != nil {
		return nil, ErrTimeout.Wrap("remote receipt pending")
	}
	if err := verifyRemoteReceiptSignature(&r, pubKeyB64); err != nil {
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
// record that would fail VerifyRemoteReceipt. An empty string means the receipt is settleable.
func remoteReceiptInvalid(r Receipt, mp, rbps int64, replyJSON []byte) string {
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
	// The execution premium must be the manifest-snapshot rate on the actual charge (§13).
	if r.Premium != ceilDiv(r.Charge*rbps, 10000) {
		return "premium != ceil(charge*rbps)"
	}
	return ""
}

// settleRemoteCall settles a remote-proxy call after ExecuteFederation returns.
// If the receipt is absent or has an invalid signature, the trace stays open for retry (ErrTimeout).
// Otherwise it commits CommitRemoteSettlement with the correct charge/duty/refund split.
func (k *Kernel) settleRemoteCall(ctx context.Context, logger *log.Logger, action *Action, ktx *Transaction, trace *Trace, callerWalletID, callerWalletKind string, req callRequest, target *Account, mp int64, fr FederationResult, latency float64) (*CallReply, error) {
	// The value we dispatched (for a value transfer) and the contract hash we dispatched with ride on
	// the trace, so both the direct and the retry settle paths read them from one source (§13).
	// The rates come from the same record: the funding boundary froze them, so a fee change between
	// dispatch and settlement cannot move this call's arithmetic (§13). Nil = pre-041 dispatch, which
	// falls back to the row and live config exactly as before.
	rbps, importBPS, d := k.dispatchedRates(trace.DispatchJSON, actionRemoteBPS(action), k.cfg.ImportBPS)
	dispatchedHash := d.ContractHash
	// A missing, unparseable, unsigned, or mismatched receipt keeps the trace open for retry.
	// action_id and args_hash are enforced here so settlement is valid by construction.
	expectedArgsHash, _ := jcsHashStr(string(ktx.ArgsJSON))
	rp, err := parseAndVerifyRemoteReceipt(fr.ReceiptJSON, target.KernelPublicKey, action.RemoteActionID, expectedArgsHash)
	if err != nil {
		return nil, err
	}
	r := *rp

	// A validly-signed receipt is the peer's final, deterministic word: idempotent retry returns
	// the same bytes, so a receipt that breaches the §13 settlement invariants can never heal.
	// Settle it terminally as receipt-invalid (charge 0, full refund, raw receipt kept as evidence,
	// no retry) rather than clamp-and-commit a record that would fail our own VerifyRemoteReceipt
	// audit. Reconcile the discrepancy out of band (§13). The reply bytes are marshalled once so
	// the hash here is computed over exactly what gets stored.
	var replyJSON []byte
	if r.Status == TxSuccess && fr.Result != nil {
		replyJSON, _ = json.Marshal(fr.Result)
	}
	charge := r.Charge
	var premium, importFee int64
	quarantined := false
	if invalid := remoteReceiptInvalid(r, mp, rbps, replyJSON); invalid != "" {
		logger.Warn("remote.receipt_invalid", "action", action.Name, "reason", invalid)
		charge = 0
		ktx.Status = TxFailure
		ktx.Reason = reasonRemoteReceiptInvalidPrefix + invalid
		quarantined = true
	} else {
		premium = r.Premium // execution serving markup
		ktx.Status = r.Status
		if r.Status == TxSuccess {
			importFee = ceilDiv((charge+premium)*importBPS, 10000) // execution import at the DISPATCHED rate (§13)
			ktx.ReplyJSON = json.RawMessage(replyJSON)
		}
	}
	paid := charge + premium // execution bilateral payable to the peer
	ktx.Net = paid
	ktx.Fee = importFee
	ktx.RemoteReceiptHash = sha256Hex(fr.ReceiptJSON)
	ktx.RemoteReceiptJSON = fr.ReceiptJSON

	stats := k.computeStats(ctx, action.ID, ktx, latency)
	// Classify a failure BEFORE building the receipt: the commit stores the error body a replaying
	// peer will be served, so it needs this call's code, and buildReceipt copies ktx.Reason — so the
	// reason must be final here or the signed receipt and the transaction would disagree. This also
	// keeps one definition of the 402/unfunded predicate, reused for the returned error below.
	var failErr error
	if ktx.Status != TxSuccess {
		failErr = ErrExecutionFailed.Wrap("remote call failed")
		// A signed zero-charge rejection carried on transport status 402 is the remote's structured
		// ErrInsufficientFunds (the inbound handler's 402 mapping): OUR prepaid credit there is
		// exhausted, not the caller's balance. Surface it as the operator-actionable ErrPeerUnfunded
		// so a client never renders it as the caller's own insufficient_funds. Gated on the receipt
		// having validated — a quarantined receipt also forces charge 0, and vs.Quarantine is the
		// explicit flag for that (never infer it from the reason string).
		if fr.HTTPStatus == 402 && charge == 0 && !quarantined {
			failErr = PeerUnfundedError(k.KernelName(ctx, target.KernelPublicKey))
		}
		// The peer authored r.Reason; never adopt it into a record we sign (§6). Its verbatim text
		// stays verifiable in ktx.RemoteReceiptJSON. The quarantine marker above wins.
		if ktx.Reason == "" {
			ktx.Reason = KernelErrorCode(failErr)
		}
	}
	localReceipt, receiptErr := k.buildReceipt(ktx, paid+importFee, 0, 0, "")
	if receiptErr != nil {
		return nil, ErrInternal.Wrap("could not build receipt")
	}

	// Detach settlement from execution-scoped cancellation so the remote settlement
	// (charge/duty/refund + audit record) always commits once the signed receipt is in.
	sctx, cancel := settlementContext(ctx)
	defer cancel()
	if err := k.store.CommitRemoteSettlement(sctx, ktx, localReceipt, trace.ID, callerWalletID, callerWalletKind, target.ID, k.cfg.FeeRecipientID, paid, importFee, stats, req.IdempotencyRecordID, req.StepID, KernelErrorCode(failErr)); err != nil {
		return nil, ErrInternal.Wrap("could not commit remote settlement")
	}

	// Rule C (§8/§13): a settlement outcome proving the cache wrong — a signed refresh_proxy rejection
	// or a quarantined receipt — deactivates the proxy so the next call re-resolves. Supervision-side,
	// outside the monetary write set (a failure here never rolls back settlement), and hash-conditional
	// so a stale dispatch settling after a re-resolve spares the refreshed row. A funding (402) rejection
	// carries no refresh_proxy, so it never reaches here.
	if r.RefreshProxy || quarantined {
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

	logger.Info("remote.settled", "action", action.Name, "status", ktx.Status, "charge", charge, "premium", premium, "import_fee", importFee)

	if ktx.Status == TxSuccess {
		return &CallReply{Result: fr.Result, TxID: ktx.ID, TraceID: trace.ID, ReceiptID: localReceipt.ID}, nil
	}
	// Return the committed local receipt alongside the error so an inbound caller can settle
	// the real charge (a re-proxied remote subcall may have settled with charge > 0).
	return &CallReply{TxID: ktx.ID, TraceID: trace.ID, ReceiptID: localReceipt.ID}, failErr
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
	var dispatch dispatchPayload
	if err := json.Unmarshal([]byte(*trace.DispatchJSON), &dispatch); err != nil {
		return ErrInternal.Wrapf("parse dispatch_json: %v", err)
	}
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
	if mp == 0 {
		// Fallback for a pre-041 dispatch that recorded none.
		mp = actionBasePrice(action)
	}
	// The locked gross is the proxy's full two-step local price (§13) — what BeginRun/BeginStepCall
	// funded — not the bare serving markup; settlement pays charge+premium to the peer and the import
	// fee to origin sys out of it, refunding the remainder. Take it from the dispatch record, which
	// froze it at the funding boundary: the action's price now floats with local policy, so re-reading
	// the column here would refund a call at a rate it was never locked at. Pre-041 dispatches
	// recorded no gross; those fall back to the column, which for them is still the funded value.
	q := dispatch.Gross
	if q == 0 {
		q = action.Price
	}

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
	fr, _ := fe.ExecuteFederation(ctx, target.KernelPublicKey, action.RemoteActionID, action.ArtifactHash, *trace.IdempotencyKey, dispatch.Args)
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
	maxAge := k.cfg.RemotePendingMaxAge
	if maxAge == 0 {
		maxAge = 24 * time.Hour
	}
	if now.Sub(trace.CreatedAt) > maxAge {
		ktx.Status = TxFailure
		logger.Warn("remote.retry.expired", "trace_id", trace.ID, "age_seconds", now.Sub(trace.CreatedAt).Seconds())
		_, sErr := k.settleFailedCall(ctx, logger, ktx, trace, callerWalletID, callerWalletKind, req, action, 0, ErrTimeout.Wrap("remote call unsettled past max pending age"))
		return sErr
	}
	return nil
}

// SignFederation signs a federation payload with the platform key and returns
// (signature, timestamp). Returns an error if the signing key is not configured.
func (k *Kernel) SignFederation(action, counterparty, recipient, expectedContractHash, idempotencyKey, argsHash string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignFederationPayload(k.cfg.SigningKey, action, counterparty, recipient, expectedContractHash, idempotencyKey, ts, argsHash)
	return
}

// SignStep signs a step-completion payload with the platform key and returns
// (signature, timestamp). recipient is the peer being addressed. Returns an error if the signing
// key is not configured.
func (k *Kernel) SignStep(stepID, counterparty, recipient, idempotencyKey, inputHash string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignStepPayload(k.cfg.SigningKey, stepID, counterparty, recipient, idempotencyKey, ts, inputHash)
	return
}

// SignStepList signs a step-list request with the platform key and returns
// (signature, timestamp). recipient is the peer being addressed.
func (k *Kernel) SignStepList(counterparty, recipient string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignStepListPayload(k.cfg.SigningKey, counterparty, recipient, ts)
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
			return nil, ErrPeerUnreachable.Wrapf("cannot reach %s (offline?)", peerKey).WithMeta("peer", peerKey)
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
func (k *Kernel) PeerStepsAwaitingUs(ctx context.Context, peerKey string) ([]PeerStepView, error) {
	if k.fedClient == nil {
		return nil, ErrInvalidState.Wrap("federation transport not running")
	}
	self := k.selfKey(ctx)
	sig, ts, err := k.SignStepList(self, peerKey)
	if err != nil {
		return nil, err
	}
	status, body, notDispatched, err := k.fedClient.ListPeerSteps(ctx, peerKey, ts, sig)
	reply, err := stepReply(status, body, notDispatched, err, peerKey)
	if err != nil {
		return nil, err
	}
	raw, ok := reply["steps"]
	if !ok {
		return nil, nil
	}
	b, _ := json.Marshal(raw)
	// Values, not pointers: the list is peer-controlled, and a reply of {"steps":[null]} would
	// otherwise decode to a nil element that every reader must remember to guard. Decoding into
	// values makes the malformed entry a zero one, which matches no step id and carries no payment.
	var views []PeerStepView
	if json.Unmarshal(b, &views) != nil {
		return nil, ErrExecutionFailed.Wrap("malformed peer step list")
	}
	return views, nil
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
	if forUserID != "" {
		if attestation, attestTS, err = k.SignStepAuth(self, peerKey, forUserID, stepID); err != nil {
			return nil, err
		}
	}
	status, body, notDispatched, err := k.fedClient.CompletePeerStep(ctx, peerKey, ts, sig, stepID,
		idempotencyKey, input, forUserID, attestation, attestTS)
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
func (k *Kernel) ResolveRequiredCaller(ctx context.Context, ref string) (callerID, remoteID string, err error) {
	ref = strings.TrimSpace(ref)
	owner, kernelAlias, hasKernel := strings.Cut(ref, "@")
	if !hasKernel {
		u, uerr := k.ResolveUser(ctx, ref)
		if uerr != nil || !u.IsLive() {
			// A tombstone resolves but can never complete: the step would park its price forever.
			return "", "", ErrNotFound.Wrapf("required caller %q not found", ref)
		}
		return u.ID, "", nil
	}
	// A sigil-prefixed "@bob" cuts to an empty owner; reject it rather than treat it as a
	// kernel-qualified ref with no owner (handles are bare, §14).
	if owner == "" || kernelAlias == "" {
		return "", "", ErrInvalidInput.Wrapf("required caller %q must be owner@kernel", ref)
	}
	peerKey, mount, kerr := k.ResolveKernelKey(ctx, kernelAlias)
	if kerr != nil {
		return "", "", kerr
	}
	resolver := k.fedClient
	if resolver == nil {
		return "", "", ErrNotFound.Wrap("remote resolution unavailable")
	}
	remoteUserID, _, rerr := resolver.ResolveRemoteUser(ctx, peerKey, owner)
	if rerr != nil {
		return "", "", rerr
	}
	// First meaningful use (§13): a verified remote-user resolve is our own outbound act, so a
	// petname is bound here too — on the petname being unbound, not on the account being absent
	// (see lazyResolveRemote). Best-effort — a missing name never blocks the step.
	if _, berr := k.BindPetname(ctx, peerKey, "", false); berr != nil {
		k.log.With(ctx).Warn("kernel.petname.bind_failed", "public_key", peerKey, "error", berr.Error())
	}
	if mount == nil {
		if mount, err = k.EnsureKernelAccount(ctx, peerKey); err != nil {
			return "", "", err
		}
	}
	return mount.ID, remoteUserID, nil
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
	if _, err := decodeRemotePublicKey(publicKey); err != nil {
		return err
	}
	return k.store.UpsertKernel(ctx, publicKey, nickname, about, time.Now().UTC())
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
	if err := k.store.UpsertKernel(ctx, publicKey, "", "", time.Now().UTC()); err != nil {
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
	if err := k.store.UpsertKernel(ctx, publicKey, "", "", time.Now().UTC()); err != nil {
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
	// Index subject-kernel's own execution receipts by ReceiptHash so a rating from another issuer can
	// be confirmed trade-backed: it must reference one of these and be named as its counterparty.
	type execFact struct{ counterparty string }
	execByHash := map[string]execFact{}
	for _, e := range rows {
		if e.IssuerPublicKey == subjectKernelPublicKey {
			execByHash[e.ReceiptHash] = execFact{counterparty: e.CounterpartyKernelPublicKey}
		}
	}
	agg := map[string]*SubjectEvidenceRow{}
	key := func(issuer, action string) string { return issuer + "\x1f" + action }
	get := func(issuer, action string) *SubjectEvidenceRow {
		kk := key(issuer, action)
		if agg[kk] == nil {
			agg[kk] = &SubjectEvidenceRow{IssuerPublicKey: issuer, SubjectActionID: action}
		}
		return agg[kk]
	}
	for _, e := range rows {
		row := get(e.IssuerPublicKey, e.SubjectActionID)
		// Interaction metrics come from each issuer's OWN evidence about the subject: the subject's
		// self-reported executions when issuer == subject (the execution summary), and each other
		// issuer's directly-observed calls otherwise (its counterparty-experience row). The two views
		// read this field but are never summed together (§13 two views), so no call is double-counted.
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
			// Corroboration (§13): a counterparty-experience interaction (issuer != subject) is
			// trade-backed only when the subject's OWN execution evidence names this issuer as
			// counterparty and the receipt hashes join — the same two-kernel link a rating needs. A
			// self-issued claim without that link is retained but shown unverified, never taken on faith.
			if e.IssuerPublicKey != subjectKernelPublicKey && e.RemoteReceiptHash != "" {
				if ef, ok := execByHash[e.RemoteReceiptHash]; ok && ef.counterparty == e.IssuerPublicKey {
					row.CorroboratedUses++
				}
			}
		}
		// Rating metrics: any issuer, but only counted when trade-backed and non-equivocated.
		if e.RatingJSON != "" && !e.Equivocated {
			var rt RatingEvidence
			if json.Unmarshal([]byte(e.RatingJSON), &rt) == nil {
				linked := false
				if e.RemoteReceiptHash != "" {
					if ef, ok := execByHash[e.RemoteReceiptHash]; ok && ef.counterparty == e.IssuerPublicKey {
						linked = true
					}
				}
				if linked {
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

// GetGossip returns this kernel's v0.13 gossip payload (§13): first-party identity, users (sys +
// owners of active public actions), the kernel's own signed action manifests, and one page of
// evidence bundles ordered by effective time after cursor. When requesterKey names a known,
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
	var users []GossipUser
	ownerSeen := map[string]bool{}
	owners := map[string]*Account{} // one read per owner, shared by the manifest and the user summary
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
		if !ownerSeen[ow.ID] {
			ownerSeen[ow.ID] = true
			if ow.KernelPublicKey == "" {
				users = append(users, GossipUser{UserID: ow.ID, Handle: ow.Handle, Description: ow.Description})
			}
		}
	}
	// Always advertise @sys so a discovering kernel can index the operator identity.
	if sys != nil && !ownerSeen[sys.ID] {
		users = append(users, GossipUser{UserID: sys.ID, Handle: sys.Handle, Description: sys.Description})
	}

	bundles, nextCursor, err := k.gossipEvidencePage(ctx, ourKey, cursor)
	if err != nil {
		return nil, err
	}

	resp := &GossipResponse{
		PublicKey:       ourKey,
		Handle:          handle,
		About:           about,
		Users:           users,
		ActionManifests: manifests,
		Evidence:        bundles,
		NextCursor:      nextCursor,
	}
	// Report the requester's credit here only if it is a known, non-suspended peer (§13 peer sync).
	// Gossip carries no membership: which kernels exist is routing discovery's job (§13 Transport),
	// so serving a pull learns nothing about the requester and provisions no account.
	if requesterKey != "" {
		if u, _ := k.store.ReadAccountByKernelKey(ctx, requesterKey); u != nil && u.SuspendedAt == nil {
			bal := u.Available
			resp.CounterpartyBalance = &bal
		}
	}
	return resp, nil
}

// buildEvidenceReceipt builds the wire-only signed projection of one of this kernel's receipts (§13).
// For an own action the subject is (ourKey, receipt.action_id); for a proxy call the store supplied
// the peer subject in row.Subject*. RemoteReceiptHash is receiptHash of the STORED remote receipt
// (canonical JSON — the one definition shared with the serving kernel's ReceiptHash), never the raw
// tx.RemoteReceiptHash. Signed under the evidence_receipt domain.
func (k *Kernel) buildEvidenceReceipt(ourKey string, row *GossipReceiptRow) (*EvidenceReceipt, error) {
	rh, err := receiptHash(row.Receipt)
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
	sig, err := signJCS(k.cfg.SigningKey, sigDomainEvidenceReceipt, cp)
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
func gossipRowIsExecuted(row *GossipReceiptRow) bool {
	if row.RemoteReceiptJSON == "" {
		return true // leg (a): own execution
	}
	var rr struct {
		TxID   string   `json:"tx_id"`
		Status TxStatus `json:"status"`
	}
	_ = json.Unmarshal([]byte(row.RemoteReceiptJSON), &rr)
	// Signed rejection: the serving kernel sets a rejection receipt's tx_id to the caller's
	// idempotency_key, distinguishing it from a genuine execution at any price (0 included).
	if row.IdempotencyKey != "" && rr.TxID == row.IdempotencyKey {
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
	sig, err := signJCS(k.cfg.SigningKey, sigDomainRating, re)
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

// DiscoveryDocsForKernel returns the locally-cached "action" discovery docs for one source kernel
// (§13), for the offline-inspect fallback. Regenerable — empty until the next gossip pull.
func (k *Kernel) DiscoveryDocsForKernel(ctx context.Context, publicKey string) ([]*DiscoveryDoc, error) {
	all, err := k.store.ListDiscoveryDocs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*DiscoveryDoc, 0)
	for _, d := range all {
		if d.KernelPublicKey == publicKey && d.Kind == "action" {
			out = append(out, d)
		}
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

// RecordPeerSync persists a successful peer gossip pull (§13 peer sync) keyed by public key:
// last_seen=now and, when the peer reported one, our cached credit on it. A no-op for unknown or
// suspended keys. Display-only cache; never a money path.
func (k *Kernel) RecordPeerSync(ctx context.Context, publicKey string, credit *int64) error {
	if u, err := k.store.ReadAccountByKernelKey(ctx, publicKey); err == nil && u != nil && u.SuspendedAt != nil {
		return nil
	}
	return k.store.UpdatePeerSync(ctx, publicKey, time.Now().UTC(), credit)
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
	sig, err := signReceipt(k.cfg.SigningKey, r)
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
	now := time.Now().UTC()
	if err := k.ObserveKernel(ctx, gossip.PublicKey, gossip.Handle, gossip.About); err != nil {
		return "", err
	}

	// Rebuild discovery docs from the verified catalog snapshot (replace-all per source kernel).
	const maxGossipElements = 10000
	docs := make([]*DiscoveryDoc, 0, len(gossip.ActionManifests)+len(gossip.Users))
	for i, m := range gossip.ActionManifests {
		if i >= maxGossipElements {
			break
		}
		if m == nil || VerifyManifestSignature(gossip.PublicKey, m) != nil {
			continue // only verified first-party manifests are indexed
		}
		// The catalog price a browser sees without resolving: the peer's signed serving markup now,
		// the origin's import fee at read time (§13). An unrepresentable one skips the manifest.
		sp, perr := markedUpPrice(m.Price, m.RemoteBPS)
		if perr != nil {
			continue
		}
		d := &DiscoveryDoc{
			KernelPublicKey: gossip.PublicKey,
			Kind:            "action",
			UserID:          m.OwnerID,
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
	for i, u := range gossip.Users {
		if i >= maxGossipElements {
			break
		}
		if u.UserID == "" {
			continue
		}
		d := &DiscoveryDoc{
			KernelPublicKey: gossip.PublicKey,
			Kind:            "user",
			UserID:          u.UserID,
			Handle:          u.Handle,
			Description:     u.Description,
			ObservedAt:      now,
		}
		if k.llm != nil {
			if vec, eerr := k.llm.Embed(ctx, u.Handle+" "+u.Description); eerr == nil {
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
	if err := verifyJCS(pub, sigDomainEvidenceReceipt, cp, er.Signature); err != nil {
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
		if err := verifyJCS(pub, sigDomainRating, rc, b.Rating.Signature); err != nil {
			return ErrUnauthorized.Wrap("rating signature invalid")
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
	if err := VerifyManifestSignature(remoteUser.KernelPublicKey, &m); err != nil {
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
	sr, err := markedUpPrice(m.Price, rbps)
	if err != nil {
		return nil, err
	}
	proxyPrice, err := markedUpPrice(sr, k.cfg.ImportBPS)
	if err != nil {
		return nil, err
	}
	incoming := []incomingOp{{
		key:  m.ActionID,
		hash: contentHash,
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
		// Identity and lifecycle only; reconcileImport calls apply for the contract fields.
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

	result, err := k.reconcileImport(ctx, existingByKey, func(a *Action) string { return a.ArtifactHash }, incoming, false)
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
		RemoteBPS:    k.cfg.RemoteBPS,
		Description:  a.Description,
		InputSchema:  a.InputSchema,
		OutputSchema: a.OutputSchema,
		Price:        a.Price,
		Kind:         a.Kind,
		ArtifactHash: a.ArtifactHash,
		UpdatedAt:    a.UpdatedAt,
		Stats:        stats,
	}
	sig, err := SignManifest(k.cfg.SigningKey, m)
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
func SignManifest(key ed25519.PrivateKey, m *ActionManifest) (string, error) {
	cp := *m
	cp.Signature = ""
	return signJCS(key, sigDomainManifest, cp)
}

// VerifyManifestSignature checks that m.Signature was produced by the private key
// corresponding to pubKeyB64 (base64url Ed25519 public key), and that the monetary fields the
// origin prices from are in range. A signature proves authorship, not sanity: a signed negative
// price or out-of-range markup would otherwise flow into the proxy price. This is the single
// funnel for all three trust boundaries — gossip ingest, authoritative import, and resolve.
func VerifyManifestSignature(pubKeyB64 string, m *ActionManifest) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return err
	}
	if m.Price < 0 || m.RemoteBPS < 0 || m.RemoteBPS > 10000 {
		return ErrInvalidInput.Wrapf("manifest price %d / remote_bps %d out of range", m.Price, m.RemoteBPS)
	}
	cp := *m
	cp.Signature = ""
	if err := verifyJCS(pub, sigDomainManifest, cp, m.Signature); err != nil {
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
	Counterparty         string `json:"counterparty"`
	ExpectedContractHash string `json:"expected_contract_hash"`
	IdempotencyKey       string `json:"idempotency_key"`
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
}

type stepListPayload struct {
	Counterparty string `json:"counterparty"`
	Recipient    string `json:"recipient"`
	Timestamp    string `json:"timestamp"`
}

// verifyPeerSignature verifies a peer's base64url Ed25519 signature over a payload in its own
// domain. It owns the cryptographic mechanics only; each exported verifier below translates a
// failure into its own typed, protocol-specific error.
func verifyPeerSignature(pubKeyB64, domain string, payload any, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	return verifyJCS(pub, domain, payload, sigB64)
}

// VerifyFederationSignature verifies an Ed25519 signature over the canonical federation call payload.
func VerifyFederationSignature(pubKeyB64, action, counterparty, recipient, expectedContractHash, idempotencyKey, timestamp, argsHash, sigB64 string) error {
	p := fedCallPayload{Action: action, ArgsHash: argsHash, Counterparty: counterparty,
		ExpectedContractHash: expectedContractHash, IdempotencyKey: idempotencyKey,
		Recipient: recipient, Timestamp: timestamp}
	if err := verifyPeerSignature(pubKeyB64, sigDomainFedCall, p, sigB64); err != nil {
		return ErrUnauthenticated.Wrap("federation signature is invalid")
	}
	return nil
}

// SignFederationPayload creates a base64url Ed25519 signature over the canonical federation payload.
func SignFederationPayload(key ed25519.PrivateKey, action, counterparty, recipient, expectedContractHash, idempotencyKey, timestamp, argsHash string) (string, error) {
	return signJCS(key, sigDomainFedCall, fedCallPayload{Action: action, ArgsHash: argsHash,
		Counterparty: counterparty, ExpectedContractHash: expectedContractHash,
		IdempotencyKey: idempotencyKey, Recipient: recipient, Timestamp: timestamp})
}

// SignStepPayload creates a base64url Ed25519 signature over the canonical step-completion payload
// — a key-set disjoint from every other signed Juice payload (§12, §13).
func SignStepPayload(key ed25519.PrivateKey, stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash string) (string, error) {
	return signJCS(key, sigDomainStepComplete, stepCompletePayload{Counterparty: counterparty,
		IdempotencyKey: idempotencyKey, InputHash: inputHash, Recipient: recipient,
		StepID: stepID, Timestamp: timestamp})
}

// VerifyStepSignature verifies an Ed25519 signature over the canonical step-completion payload.
// recipient must be the verifying kernel's own public key.
func VerifyStepSignature(pubKeyB64, stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash, sigB64 string) error {
	p := stepCompletePayload{Counterparty: counterparty, IdempotencyKey: idempotencyKey,
		InputHash: inputHash, Recipient: recipient, StepID: stepID, Timestamp: timestamp}
	if err := verifyPeerSignature(pubKeyB64, sigDomainStepComplete, p, sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step signature is invalid")
	}
	return nil
}

// SignStepAuthPayload signs the home-kernel attestation that its authenticated local user (userID)
// authorized completing step stepID (§13). recipient binds it to the serving kernel, closing
// cross-kernel replay.
func SignStepAuthPayload(key ed25519.PrivateKey, counterparty, recipient, userID, stepID, timestamp string) (string, error) {
	return signJCS(key, sigDomainStepAuth, stepAuthPayload{Counterparty: counterparty,
		Recipient: recipient, StepID: stepID, Timestamp: timestamp, UserID: userID})
}

// VerifyStepAuthSignature verifies the attestation. recipient must be the verifying kernel's own key.
func VerifyStepAuthSignature(pubKeyB64, counterparty, recipient, userID, stepID, timestamp, sigB64 string) error {
	p := stepAuthPayload{Counterparty: counterparty, Recipient: recipient, StepID: stepID,
		Timestamp: timestamp, UserID: userID}
	if err := verifyPeerSignature(pubKeyB64, sigDomainStepAuth, p, sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step attestation is invalid")
	}
	return nil
}

// SignStepAuth stamps the current time and signs the step_auth attestation with this kernel's
// platform key. counterparty is this kernel's own key; recipient is the serving peer's key.
func (k *Kernel) SignStepAuth(counterparty, recipient, userID, stepID string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignStepAuthPayload(k.cfg.SigningKey, counterparty, recipient, userID, stepID, ts)
	return
}

// SignStepListPayload creates a base64url Ed25519 signature over the canonical step-list payload.
// The sigDomainStepList prefix keeps this key-set disjoint from every other signed payload (§12).
func SignStepListPayload(key ed25519.PrivateKey, counterparty, recipient, timestamp string) (string, error) {
	return signJCS(key, sigDomainStepList, stepListPayload{Counterparty: counterparty,
		Recipient: recipient, Timestamp: timestamp})
}

// VerifyStepListSignature verifies an Ed25519 signature over the canonical step-list payload.
// recipient must be the verifying kernel's own public key.
func VerifyStepListSignature(pubKeyB64, counterparty, recipient, timestamp, sigB64 string) error {
	p := stepListPayload{Counterparty: counterparty, Recipient: recipient, Timestamp: timestamp}
	if err := verifyPeerSignature(pubKeyB64, sigDomainStepList, p, sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step signature is invalid")
	}
	return nil
}

// ---- Residual settlement (§13) ----

const (
	settleSecretTag = "juice-settle-v1"
	settleExpiry    = 24 * time.Hour
)

// settleSecret derives the creditor's per-settlement secret deterministically from the platform
// signing seed, so the creditor holds no state between the open and finish rounds (§13 stateless
// creditor): s = SHA-256(signing_seed ‖ "juice-settle-v1" ‖ settlement_id).
func (k *Kernel) settleSecret(settlementID string) string {
	seed := k.cfg.SigningKey.Seed()
	buf := append(append(append([]byte{}, seed...), settleSecretTag...), settlementID...)
	h := sha256.Sum256(buf)
	return hex.EncodeToString(h[:])
}

// settleOutcome is the fair probabilistic result: pay iff (first 8 bytes of
// SHA-256(settlement_id‖s‖n) mod Q) < d. Modulo bias is negligible for Q ≪ 2^64 (§13).
func settleOutcome(settlementID, secret, nonce string, quantum, amount int64) bool {
	h := sha256.Sum256([]byte(settlementID + secret + nonce))
	return int64(binary.BigEndian.Uint64(h[:8])%uint64(quantum)) < amount
}

// ourKeyB64 is this kernel's own platform public key, base64url — the federation identity used as
// creditor/debtor and recipient in settlement payloads.
func (k *Kernel) ourKeyB64() string {
	return base64.RawURLEncoding.EncodeToString(k.cfg.SigningKey.Public().(ed25519.PublicKey))
}

// signSettlementRecord fixes the creditor signature over JCS(record with Signature="").
func (k *Kernel) signSettlementRecord(r *SettlementRecord) error {
	r.Signature = ""
	sig, err := signJCS(k.cfg.SigningKey, sigDomainSettlementRec, r)
	if err != nil {
		return err
	}
	r.Signature = sig
	return nil
}

// verifySettlementRecord checks the creditor's signature on a record.
func verifySettlementRecord(r *SettlementRecord, creditorB64 string) error {
	pub, err := decodeRemotePublicKey(creditorB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid creditor public key")
	}
	cp := *r
	cp.Signature = ""
	if err := verifyJCS(pub, sigDomainSettlementRec, cp, r.Signature); err != nil {
		return ErrUnauthenticated.Wrap("settlement record signature is invalid")
	}
	return nil
}

// The three settle request payloads are disjoint from each other and from every other signed payload
// (§12): each is signed under its own domain (sigDomainSettleOpen/Finish/Reconcile). recipient binds a
// request to the intended creditor, closing cross-kernel replay.
// The three settle rounds (§13). Amount is a decimal string, as the signed key-set has always had
// it — a typed struct fixes the shape without moving a byte (sigfixture_test.go).
type settleOpenReq struct {
	Amount       string `json:"amount"`
	Counterparty string `json:"counterparty"`
	Recipient    string `json:"recipient"`
	SettlementID string `json:"settlement_id"`
	Timestamp    string `json:"timestamp"`
}

type settleFinishReq struct {
	Counterparty string `json:"counterparty"`
	Nonce        string `json:"nonce"`
	Recipient    string `json:"recipient"`
	SettlementID string `json:"settlement_id"`
	Timestamp    string `json:"timestamp"`
}

type settleReconcileReq struct {
	Counterparty string `json:"counterparty"`
	Recipient    string `json:"recipient"`
	SettlementID string `json:"settlement_id"`
	Timestamp    string `json:"timestamp"`
}

func settleOpenPayload(counterparty, recipient, settlementID string, amount int64, timestamp string) settleOpenReq {
	return settleOpenReq{Amount: strconv.FormatInt(amount, 10), Counterparty: counterparty,
		Recipient: recipient, SettlementID: settlementID, Timestamp: timestamp}
}

func settleFinishPayload(counterparty, recipient, settlementID, nonce, timestamp string) settleFinishReq {
	return settleFinishReq{Counterparty: counterparty, Nonce: nonce, Recipient: recipient,
		SettlementID: settlementID, Timestamp: timestamp}
}

func settleReconcilePayload(counterparty, recipient, settlementID, timestamp string) settleReconcileReq {
	return settleReconcileReq{Counterparty: counterparty, Recipient: recipient,
		SettlementID: settlementID, Timestamp: timestamp}
}

// GrossReceivables returns this kernel's total unsecured receivables across all peers (§13).
func (k *Kernel) GrossReceivables(ctx context.Context) (int64, error) {
	return k.store.GrossReceivables(ctx)
}

// HandleSettle is the creditor side of the /juice/fed/settle/1 exchange (§13). debtorKey is the
// authenticated counterparty; the caller (cmd/juice) has already matched the connection key and
// checked freshness. It verifies the debtor's scoped signature and serves one round: "open" commits
// a fresh secret and returns a signed open record; "finish" reveals the secret, computes the outcome,
// and applies the creditor three-way legs (idempotent by settlement_id — anti-grinding); "reconcile"
// binds a clear-for-zero when this creditor failed to reveal before expiry (FIX 2). Returns
// (status, body, err): a business rejection uses a non-200 status with a body; a protocol error uses err.
func (k *Kernel) HandleSettle(ctx context.Context, debtorKey, kind, timestamp, signature, settlementID string, amount int64, nonce string, recordJSON []byte) (int, []byte, error) {
	peer, err := k.store.ReadAccountByKernelKey(ctx, debtorKey)
	if err != nil || peer == nil || !peer.IsPeer() {
		return 0, nil, ErrNotFound.Wrap("unknown settlement counterparty")
	}
	if peer.SuspendedAt != nil {
		return 0, nil, ErrUnauthenticated.Wrap("peer suspended")
	}
	our := k.ourKeyB64()
	debtorPub, err := decodeRemotePublicKey(debtorKey)
	if err != nil {
		return 0, nil, ErrUnauthenticated.Wrap("invalid counterparty key")
	}
	sysID := k.cfg.FeeRecipientID

	switch kind {
	case "open":
		if err := verifyJCS(debtorPub, sigDomainSettleOpen, settleOpenPayload(debtorKey, our, settlementID, amount, timestamp), signature); err != nil {
			return 0, nil, ErrUnauthenticated.Wrap("settle_open signature invalid")
		}
		// Our books must agree that this peer owes us exactly the claimed debt d (= −available).
		if -peer.Available != amount {
			b, _ := json.Marshal(map[string]any{"error": "receivable mismatch", "receivable": -peer.Available})
			return 409, b, nil
		}
		Q := k.cfg.SettlementQuantum
		if amount <= 0 || Q <= 0 || amount >= Q {
			return 0, nil, ErrInvalidState.Wrap("debt is not within the probabilistic band 0 < d < Q")
		}
		s := k.settleSecret(settlementID)
		now := time.Now().UTC()
		rec := &SettlementRecord{
			SettlementID: settlementID, Creditor: our, Debtor: debtorKey,
			Amount: amount, Quantum: Q, Mode: "probabilistic",
			Commitment: sha256Hex(s), ExpiresAt: now.Add(settleExpiry), CreatedAt: now,
		}
		if err := k.signSettlementRecord(rec); err != nil {
			return 0, nil, err
		}
		b, _ := json.Marshal(rec)
		return 200, b, nil

	case "finish":
		if err := verifyJCS(debtorPub, sigDomainSettleFinish, settleFinishPayload(debtorKey, our, settlementID, nonce, timestamp), signature); err != nil {
			return 0, nil, ErrUnauthenticated.Wrap("settle_finish signature invalid")
		}
		if stored, err := k.store.ReadSettlementRecord(ctx, settlementID); err != nil {
			return 0, nil, err
		} else if stored != "" {
			return 200, []byte(stored), nil // idempotent replay — anti-grinding
		}
		open, oerr := k.verifyOwnOpenRecord(recordJSON, our, debtorKey, settlementID)
		if oerr != nil {
			return 0, nil, oerr
		}
		s := k.settleSecret(settlementID)
		if sha256Hex(s) != open.Commitment {
			return 0, nil, ErrInvalidState.Wrap("commitment mismatch")
		}
		pay := settleOutcome(settlementID, s, nonce, open.Quantum, open.Amount)
		final := *open
		final.Nonce, final.Secret = nonce, s
		final.Outcome = "clear"
		if pay {
			final.Outcome = "pay"
		}
		if err := k.signSettlementRecord(&final); err != nil {
			return 0, nil, err
		}
		fb, _ := json.Marshal(&final)
		// A pay outcome changes NO balance (§13): the record is stored for anti-grinding and the debt
		// stays d, pending the rail record. A clear outcome extinguishes the debt now (creditor: clear
		// +d on the peer row, sys absorbs −d).
		dClear, variance := int64(0), int64(0)
		if !pay {
			dClear, variance = open.Amount, -open.Amount
		}
		if _, err := k.store.CommitSettlement(ctx, settlementID, peer.ID, sysID, dClear, variance, open.Amount, string(fb)); err != nil {
			return 0, nil, err
		}
		return 200, fb, nil

	case "reconcile":
		if err := verifyJCS(debtorPub, sigDomainSettleReconcile, settleReconcilePayload(debtorKey, our, settlementID, timestamp), signature); err != nil {
			return 0, nil, ErrUnauthenticated.Wrap("settle_reconcile signature invalid")
		}
		if stored, err := k.store.ReadSettlementRecord(ctx, settlementID); err != nil {
			return 0, nil, err
		} else if stored != "" {
			return 200, []byte(stored), nil
		}
		open, oerr := k.verifyOwnOpenRecord(recordJSON, our, debtorKey, settlementID)
		if oerr != nil {
			return 0, nil, oerr
		}
		if time.Now().UTC().Before(open.ExpiresAt) {
			return 0, nil, ErrInvalidState.Wrap("open record has not expired; finish instead")
		}
		// Binding clear-for-zero (FIX 2): we failed to reveal in time, so the debt clears for zero. We
		// MUST apply the same outcome the debtor already applied locally, or be in provable default.
		final := *open
		final.Outcome = "clear"
		if err := k.signSettlementRecord(&final); err != nil {
			return 0, nil, err
		}
		fb, _ := json.Marshal(&final)
		if _, err := k.store.CommitSettlement(ctx, settlementID, peer.ID, sysID, open.Amount, -open.Amount, open.Amount, string(fb)); err != nil {
			return 0, nil, err
		}
		return 200, fb, nil
	}
	return 0, nil, ErrInvalidInput.Wrap("unknown settle kind")
}

// verifyOwnOpenRecord parses and validates that recordJSON is an open record this kernel signed for
// this debtor and settlement (the trustless state carriage that lets the creditor stay stateless).
func (k *Kernel) verifyOwnOpenRecord(recordJSON []byte, our, debtorKey, settlementID string) (*SettlementRecord, error) {
	var open SettlementRecord
	if err := json.Unmarshal(recordJSON, &open); err != nil {
		return nil, ErrInvalidInput.Wrap("missing or invalid open record")
	}
	if err := verifySettlementRecord(&open, our); err != nil {
		return nil, ErrUnauthenticated.Wrap("open record is not ours")
	}
	if open.SettlementID != settlementID || open.Debtor != debtorKey || open.Amount <= 0 || open.Quantum <= 0 {
		return nil, ErrInvalidState.Wrap("open record does not match request")
	}
	return &open, nil
}

// SettlePeer is the debtor side of residual settlement (§13), invoked by the operator via
// `admin settle <peer>`. It settles the position this kernel owes the peer: exact when |d| ≥ Q (the
// operator records the rail payment with deposit/withdraw --external-key), otherwise the two-party
// probabilistic commit/reveal. When the peer owes this kernel instead, settlement is theirs to
// initiate (Y only signals). Superuser only.
func (k *Kernel) SettlePeer(ctx context.Context, operatorID, peerRef string) (map[string]any, error) {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return nil, err
	}
	peer, err := k.ResolveUser(ctx, peerRef)
	if err != nil || peer == nil {
		return nil, ErrNotFound.Wrapf("peer %q not found", peerRef)
	}
	if !peer.IsPeer() {
		return nil, ErrInvalidInput.Wrap("settle applies only to peer accounts")
	}
	// Every name in a settlement result is one an operator retypes into the next command, so it
	// must resolve: the kernel's petname, else its key (§13). A kernel account has no handle.
	name := k.KernelName(ctx, peer.KernelPublicKey)
	// A prior paid outcome still awaiting its rail record must be finalized (admin settle --cash)
	// before opening a new lottery on the same position.
	if pending, err := k.store.HasPendingSettlement(ctx, peer.ID); err != nil {
		return nil, err
	} else if pending {
		return map[string]any{"status": "pending_cash", "handle": name,
			"message": "a paid probabilistic outcome is awaiting its rail record; pay the quantum, then run `admin settle <peer> --cash <settlement_id>`"}, nil
	}
	d := peer.Available // > 0 ⇒ this kernel owes the peer (we are the debtor)
	switch {
	case d == 0:
		return map[string]any{"status": "settled", "amount": int64(0), "handle": name}, nil
	case d < 0:
		return map[string]any{"status": "creditor", "amount": -d, "handle": name,
			"message": "the peer owes you; its operator initiates settlement"}, nil
	}
	Q := k.cfg.SettlementQuantum
	settlementID := uuid.NewString()
	if Q <= 0 || d >= Q {
		return map[string]any{"status": "exact", "mode": "exact", "amount": d, "settlement_id": settlementID, "handle": name,
			"message": fmt.Sprintf("pay %d on the rail, then run `admin withdraw %s %d --external-key %s`", d, name, d, settlementID)}, nil
	}

	settler := k.fedClient
	if settler == nil {
		return nil, ErrInvalidState.Wrap("federation transport does not support settlement")
	}
	our := k.ourKeyB64()

	// Round 1 — open: the creditor commits H(s).
	ts := time.Now().UTC().Format(time.RFC3339)
	openSig, err := signJCS(k.cfg.SigningKey, sigDomainSettleOpen, settleOpenPayload(our, peer.KernelPublicKey, settlementID, d, ts))
	if err != nil {
		return nil, err
	}
	status, body, err := settler.Settle(ctx, peer.KernelPublicKey, "open", ts, openSig, settlementID, d, "", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, ErrInvalidState.Wrapf("peer refused settlement open (status %d): %s", status, string(body))
	}
	open := SettlementRecord{}
	if err := json.Unmarshal(body, &open); err != nil {
		return nil, ErrInvalidState.Wrap("invalid open record")
	}
	if err := verifySettlementRecord(&open, peer.KernelPublicKey); err != nil {
		return nil, err
	}
	if open.SettlementID != settlementID || open.Amount != d || open.Creditor != peer.KernelPublicKey || open.Quantum <= 0 {
		return nil, ErrInvalidState.Wrap("open record does not match request")
	}
	openJSON := body

	// Round 2 — finish: reveal the nonce; the creditor reveals s and applies its legs.
	nonce := uuid.NewString()
	ts2 := time.Now().UTC().Format(time.RFC3339)
	finishSig, err := signJCS(k.cfg.SigningKey, sigDomainSettleFinish, settleFinishPayload(our, peer.KernelPublicKey, settlementID, nonce, ts2))
	if err != nil {
		return nil, err
	}
	status, body, err = settler.Settle(ctx, peer.KernelPublicKey, "finish", ts2, finishSig, settlementID, 0, nonce, openJSON)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, ErrInvalidState.Wrapf("peer refused settlement finish (status %d): %s", status, string(body))
	}
	final := SettlementRecord{}
	if err := json.Unmarshal(body, &final); err != nil {
		return nil, ErrInvalidState.Wrap("invalid final record")
	}
	if err := verifySettlementRecord(&final, peer.KernelPublicKey); err != nil {
		return nil, err
	}
	if final.SettlementID != settlementID || sha256Hex(final.Secret) != open.Commitment {
		return nil, ErrInvalidState.Wrap("final record fails the commitment check")
	}
	// Recompute the flip ourselves — the creditor cannot misreport it once s is revealed.
	pay := settleOutcome(settlementID, final.Secret, nonce, open.Quantum, d)
	if (pay && final.Outcome != "pay") || (!pay && final.Outcome != "clear") {
		return nil, ErrInvalidState.Wrap("final outcome contradicts the revealed secret")
	}
	// A pay outcome changes NO balance: the debt stays d until the operator records the rail payment
	// (§13). A clear outcome extinguishes it now (debtor: clear −d, sys gains +d).
	dClear, variance := int64(0), int64(0)
	if !pay {
		dClear, variance = -d, d
	}
	if _, err := k.store.CommitSettlement(ctx, settlementID, peer.ID, k.cfg.FeeRecipientID, dClear, variance, d, string(body)); err != nil {
		return nil, err
	}
	res := map[string]any{"mode": "probabilistic", "outcome": final.Outcome, "settlement_id": settlementID, "handle": name}
	if pay {
		res["status"] = "pending_cash"
		res["amount"] = open.Quantum
		res["message"] = fmt.Sprintf("you owe %d; pay it on the rail, then run `admin settle %s --cash %s`", open.Quantum, name, settlementID)
	} else {
		res["status"] = "settled"
		res["amount"] = int64(0)
	}
	return res, nil
}

// SettleCash finalizes a paid probabilistic outcome on this kernel after the operator moved the
// quantum on the rail (§13): it clears the debt d and books the variance ±(Q−d), deriving this
// kernel's side (creditor or debtor) from the stored signed record. Run on BOTH kernels — the debtor
// after paying, the creditor after receiving. Superuser only; idempotent by settlement_id.
func (k *Kernel) SettleCash(ctx context.Context, operatorID, peerRef, settlementID string) (map[string]any, error) {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return nil, err
	}
	peer, err := k.ResolveUser(ctx, peerRef)
	if err != nil || peer == nil || !peer.IsPeer() {
		return nil, ErrNotFound.Wrapf("peer %q not found", peerRef)
	}
	name := k.KernelName(ctx, peer.KernelPublicKey)
	recJSON, err := k.store.ReadSettlementRecord(ctx, settlementID)
	if err != nil {
		return nil, err
	}
	if recJSON == "" {
		return nil, ErrNotFound.Wrapf("no settlement %q", settlementID)
	}
	var rec SettlementRecord
	if err := json.Unmarshal([]byte(recJSON), &rec); err != nil {
		return nil, ErrInternal.Wrap("stored settlement record is invalid")
	}
	if rec.Outcome != "pay" {
		return nil, ErrInvalidState.Wrap("settlement is not an unpaid probabilistic outcome")
	}
	d, q := rec.Amount, rec.Quantum
	// Creditor: clear +d on the peer row, sys += (Q−d). Debtor: clear −d, sys −= (Q−d) (guarded by the
	// users CHECK — insufficient reserve rolls back and the settlement stays pending).
	var dClear, variance int64
	switch k.ourKeyB64() {
	case rec.Creditor:
		dClear, variance = d, q-d
	case rec.Debtor:
		dClear, variance = -d, -(q - d)
	default:
		return nil, ErrInvalidState.Wrap("this kernel is not a party to the settlement")
	}
	cashRec := fmt.Sprintf(`{"settlement_id":%q,"cash":%d,"outcome":"cash"}`, settlementID, q)
	if _, err := k.store.CommitSettlementCash(ctx, settlementID, peer.ID, k.cfg.FeeRecipientID, dClear, variance, q, cashRec); err != nil {
		return nil, err
	}
	return map[string]any{"status": "settled", "settlement_id": settlementID, "cash": q, "handle": name}, nil
}
