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

// remoteManifestPrice derives the base remote manifest price (mp) from a two-step proxy price
// (q = sr + ceil(sr·import_bps), sr = mp + ceil(mp·remote_bps)) by inverting both layers in order:
// sr = floor(q·10000/(10000+import_bps)), then mp = floor(sr·10000/(10000+remote_bps)) (§13).
func (k *Kernel) remoteManifestPrice(proxyPrice int64) int64 {
	sr := proxyPrice * 10000 / (10000 + k.cfg.ImportBPS)
	return sr * 10000 / (10000 + k.cfg.RemoteBPS)
}

// RemoteManifestPrice is the exported form of remoteManifestPrice, used by the service layer to
// compare a peer's cached credit against a proxy action's underlying manifest price (§13 peer_state).
func (k *Kernel) RemoteManifestPrice(proxyPrice int64) int64 {
	return k.remoteManifestPrice(proxyPrice)
}

// TransferEffect is the staged value channel of an effect-bearing action (§13): the delivered Amount,
// the resolved local-beneficiary Dest (empty for an outbound/remote destination), and the total Reserve
// to lock from the immediate caller C's own balance at admission. It is produced only for an action
// whose signed contract declares an effect, and settled deferredly by the kernel at commit; the two
// money channels (execution price, funded by the trace, and this value, funded by C) never mix.
type TransferEffect struct {
	Amount  int64
	Dest    string
	Reserve int64
}

// prepareTransferEffect stages the value channel for a call at funding time (§13), or returns nil when
// the action declares no transfer effect. Effect identity comes from the action's signed contract
// (actionValue keys on a.Effect), never its name. The reserve, locked from the immediate caller C's own
// balance, covers the delivered value plus the value-channel fees owed at settlement, by destination:
//   - local action, local target (local caller):  reserve = value                         (Dest = id)
//   - local action, local target (peer caller):   reserve = value + value_premium         (Dest = id)
//   - remote_proxy action (outbound, local caller): reserve = value + value_premium + value_import (Dest = "")
// It rejects, before any funds move, a non-positive amount, a peer caller relaying a cross-kernel
// transfer (non-transitive), an @-qualified target on a local action, and an unresolvable / peer /
// suspended local beneficiary.
func (k *Kernel) prepareTransferEffect(ctx context.Context, callerIsPeer bool, a *Action, args map[string]any) (*TransferEffect, error) {
	fn := k.actionValue(a)
	if fn == nil {
		return nil, nil
	}
	amount, ref, err := fn(args)
	if err != nil {
		return nil, err
	}
	if amount < 1 {
		return nil, ErrInvalidInput.Wrap("transfer amount must be a positive integer")
	}
	if a.Kind == KindRemoteProxy {
		if callerIsPeer {
			return nil, ErrInvalidInput.Wrap("a peer caller cannot relay a cross-kernel transfer (non-transitive)")
		}
		vp := ceilDiv(amount*k.cfg.RemoteBPS, 10000)
		vi := ceilDiv((amount+vp)*k.cfg.ImportBPS, 10000)
		return &TransferEffect{Amount: amount, Dest: "", Reserve: amount + vp + vi}, nil
	}
	if strings.Contains(ref, "@") {
		return nil, ErrInvalidInput.Wrap("address a cross-kernel transfer as sys@<kernel>/transfer with a bare target")
	}
	benef, err := k.ResolveUser(ctx, ref)
	if err != nil || benef == nil {
		return nil, ErrNotFound.Wrapf("transfer beneficiary %q not found", ref)
	}
	if benef.IsPeer() {
		return nil, ErrInvalidInput.Wrap("transfer beneficiary is a peer account")
	}
	if benef.SuspendedAt != nil {
		return nil, ErrInvalidInput.Wrap("transfer beneficiary is suspended")
	}
	reserve := amount
	if callerIsPeer {
		reserve += ceilDiv(amount*k.cfg.RemoteBPS, 10000) // inbound serving markup owed to sys at settlement
	}
	return &TransferEffect{Amount: amount, Dest: benef.ID, Reserve: reserve}, nil
}

// dispatchPayload is the persisted remote-proxy dispatch record, stored on Trace.DispatchJSON
// so a pending remote call can be replayed verbatim by RetryPendingRemoteDispatches after restart.
type dispatchPayload struct {
	Args        map[string]any `json:"args"`
	StepID      string         `json:"step_id"`
	RemotePrice int64          `json:"remote_price"`
	// Value is the delivered amount for a value transfer (§13); Gross is the funded local price (the
	// two-step markup on mp+value), used to reconstruct the locked amount on retry after restart —
	// action.Price alone is 0 for a transfer. Both 0 for a plain remote call.
	Value int64 `json:"value,omitempty"`
	Gross int64 `json:"gross,omitempty"`
	// ContractHash is the expected_contract_hash this dispatch bound (§8 If-Match). Read back at
	// settlement to key the hash-conditional proxy deactivation, so a stale dispatch settling after a
	// re-resolve never deactivates the refreshed row (§13).
	ContractHash string `json:"contract_hash,omitempty"`
}

// marshalDispatch serializes a dispatchPayload and returns a pointer suitable for Trace.DispatchJSON.
func marshalDispatch(args map[string]any, stepID string, mp, value, gross int64, contractHash string) *string {
	b, _ := json.Marshal(dispatchPayload{Args: args, StepID: stepID, RemotePrice: mp, Value: value, Gross: gross, ContractHash: contractHash})
	s := string(b)
	return &s
}

// PaymentDescriptor is what a serving kernel A advertises about a payment Step to the buyer B (§13): the
// bilateral obligation A can compute, and nothing more. `remote_max = amount + value_premium` is what B
// owes A; B computes its own `value_import`/`max_total` locally (its import policy is not A's business).
// Hash binds the descriptor into the completion idempotency key so the step cannot be completed for a
// different payment.
type PaymentDescriptor struct {
	Beneficiary string `json:"beneficiary"` // resolved local beneficiary user_id on the serving kernel
	Amount      int64  `json:"amount"`
	RemoteBPS   int64  `json:"remote_bps"`
	RemoteMax   int64  `json:"remote_max"`
	Hash        string `json:"hash,omitempty"`
}

// PaymentDescriptorHash is the SHA-256 over the JCS-canonical descriptor (excluding hash), shared by the
// serving kernel (which advertises it) and the buyer (which binds it into the idempotency key). Both
// sides compute it identically, so a mismatch means a tampered payment.
func PaymentDescriptorHash(d PaymentDescriptor) string {
	d.Hash = ""
	payload, _ := CanonicalJSON(d)
	return sha256Hex(string(payload))
}

// BuildPaymentDescriptor computes the payment descriptor for an effect-bearing step from its partial
// args (§13), resolving the local beneficiary and pricing the serving markup. Returns nil for a
// non-transfer action or when the step's args do not name a resolvable local beneficiary.
func (k *Kernel) BuildPaymentDescriptor(ctx context.Context, action *Action, partialArgs []byte) (*PaymentDescriptor, error) {
	fn := k.actionValue(action)
	if fn == nil {
		return nil, nil // not a payment step
	}
	var args map[string]any
	if err := json.Unmarshal(partialArgs, &args); err != nil {
		return nil, nil
	}
	amount, ref, err := fn(args)
	if err != nil || amount < 1 || strings.Contains(ref, "@") {
		return nil, nil
	}
	benef, err := k.ResolveUser(ctx, ref)
	if err != nil || benef == nil || benef.IsPeer() || benef.SuspendedAt != nil {
		return nil, nil
	}
	valuePremium := ceilDiv(amount*k.cfg.RemoteBPS, 10000)
	d := PaymentDescriptor{
		Beneficiary: benef.ID,
		Amount:      amount,
		RemoteBPS:   k.cfg.RemoteBPS,
		RemoteMax:   amount + valuePremium,
	}
	d.Hash = PaymentDescriptorHash(d)
	return &d, nil
}

// StepPaymentHash returns the payment-descriptor hash for a step, or "" when it is not a payment step
// (§13). The serving side folds it into the expected completion idempotency key to bind the payment.
func (k *Kernel) StepPaymentHash(ctx context.Context, stepID string) string {
	step, err := k.store.ReadStep(ctx, stepID)
	if err != nil {
		return ""
	}
	action, err := k.store.ReadAction(ctx, step.ActionID)
	if err != nil {
		return ""
	}
	d, err := k.BuildPaymentDescriptor(ctx, action, step.PartialArgs)
	if err != nil || d == nil {
		return ""
	}
	return d.Hash
}

// ReadPendingTransferByKey returns the buyer-side pending payment record for an idempotency key (§13),
// or ErrNotFound. Used by the completion orchestration to make funding idempotent across retries.
func (k *Kernel) ReadPendingTransferByKey(ctx context.Context, idempotencyKey string) (*PendingTransfer, error) {
	return k.store.ReadPendingTransferByKey(ctx, idempotencyKey)
}

// ReadPendingTransfer returns a pending payment record by id (§13 operator surface), or ErrNotFound.
func (k *Kernel) ReadPendingTransfer(ctx context.Context, id string) (*PendingTransfer, error) {
	return k.store.ReadPendingTransfer(ctx, id)
}

// ListPendingTransfers backs the admin transfers resource (§13): an empty status returns only the
// unresolved records (pending + quarantined); an explicit status filters to exactly that one.
func (k *Kernel) ListPendingTransfers(ctx context.Context, status string, limit, offset int) ([]*PendingTransfer, error) {
	return k.store.ListPendingTransfers(ctx, status, limit, offset)
}

// AdmitRemotePaidStep locks the buyer's reserve (max_total = remote_max + its own import fee) from its
// own balance and records a pending_transfers row, reserve-first and atomic (§13). It stores the RAW
// completion input (not just its hash) so a later retry rebuilds the same signed request, and the
// descriptor's remote_max so settlement re-validates without the live descriptor. The idempotency key is
// payment-bound (control layer). Returns the pending record; a duplicate key (retry) returns ErrConflict.
func (k *Kernel) AdmitRemotePaidStep(ctx context.Context, buyerID, peerKey, stepID string, input []byte, idempotencyKey string, d PaymentDescriptor) (*PendingTransfer, error) {
	valueImport := ceilDiv(d.RemoteMax*k.cfg.ImportBPS, 10000)
	pt := &PendingTransfer{
		ID:             uuid.New().String(),
		BuyerID:        buyerID,
		PeerKey:        peerKey,
		StepID:         stepID,
		InputHash:      sha256Hex(string(input)),
		Input:          json.RawMessage(input),
		IdempotencyKey: idempotencyKey,
		Beneficiary:    d.Beneficiary,
		Amount:         d.Amount,
		RemoteMax:      d.RemoteMax,
		Reserve:        d.RemoteMax + valueImport, // max_total: value + value_premium + value_import
		Status:         "pending",
		CreatedAt:      time.Now().UTC(),
	}
	if err := k.store.InsertPendingTransfer(ctx, pt); err != nil {
		return nil, err
	}
	return pt, nil
}

// SettleRemotePaidStep disposes of a buyer-side payment reserve on the serving kernel's completion
// receipt (§13), the same rule as the outbound-call value channel:
//   - valid SUCCESS binding the payment (signature over A's key; value == amount; value_to ==
//     beneficiary; value_premium == remote_max − amount) ⇒ settle: value+value_premium to A's proxy row
//     (buyer owes A), value_import to buyer sys, remainder refunded;
//   - valid FAILURE/rejection ⇒ refund the whole reserve;
//   - an inconsistent/mis-bound receipt after a possibly-executed completion ⇒ QUARANTINE (reserve stays
//     locked; A may have paid the beneficiary).
// receiptJSON is nil/empty when no receipt arrived (uncertain): the record is left pending for retry.
func (k *Kernel) SettleRemotePaidStep(ctx context.Context, pending *PendingTransfer, d PaymentDescriptor, receiptJSON []byte) error {
	if len(receiptJSON) == 0 {
		return nil // uncertain: leave pending, retry with the same key
	}
	var r Receipt
	if err := json.Unmarshal(receiptJSON, &r); err != nil {
		return k.store.SetPendingTransferStatus(ctx, pending.ID, "quarantined", "receipt unparseable")
	}
	if verifyRemoteReceiptSignature(&r, pending.PeerKey) != nil {
		return k.store.SetPendingTransferStatus(ctx, pending.ID, "quarantined", "invalid receipt signature")
	}
	if r.Status != TxSuccess {
		return k.store.RefundPendingTransfer(ctx, pending.ID) // valid signed failure
	}
	valuePremium := d.RemoteMax - d.Amount
	switch {
	case r.Value != d.Amount:
		return k.store.SetPendingTransferStatus(ctx, pending.ID, "quarantined", "receipt value != amount")
	case r.ValueTo != d.Beneficiary:
		return k.store.SetPendingTransferStatus(ctx, pending.ID, "quarantined", "receipt beneficiary mismatch")
	case r.ValuePremium != valuePremium:
		return k.store.SetPendingTransferStatus(ctx, pending.ID, "quarantined", "receipt value_premium mismatch")
	}
	proxy, err := k.store.ReadUserByPublicKey(ctx, pending.PeerKey)
	if err != nil || proxy == nil {
		return ErrNotFound.Wrap("serving-kernel proxy row not found for settlement")
	}
	credit := r.Value + r.ValuePremium // buyer owes A value + serving markup
	valueImport := pending.Reserve - credit
	return k.store.CommitPendingTransfer(ctx, pending.ID, proxy.ID, credit, k.cfg.FeeRecipientID, valueImport)
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
	if owner.PublicKey != "" {
		if pub, derr := decodeRemotePublicKey(owner.PublicKey); derr == nil {
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
	rbps := int64(0)
	if act, aerr := k.store.ReadAction(ctx, tx.ActionID); aerr == nil && act.RemoteBPS != nil {
		rbps = *act.RemoteBPS
	}
	checks.Premium = r.Premium == ceilDiv(r.Charge*rbps, 10000)
	checks.ValuePremium = r.ValuePremium == ceilDiv(r.Value*rbps, 10000)

	// 7. Settlement arithmetic: the origin import fee on the actual obligation (tx.net = paid).
	if tx.Status == TxSuccess {
		checks.SettlementArith = tx.Fee == ceilDiv(tx.Net*k.cfg.ImportBPS, 10000)
	} else {
		checks.SettlementArith = tx.Fee == 0
	}

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
		checks.Status && checks.Charge && checks.Premium && checks.ValuePremium && checks.SettlementArith &&
		checks.RefundConservation && checks.ArgsHash && checks.ReplyHash

	return &ReceiptVerification{
		TransactionID:         txID,
		Valid:                 valid,
		RemoteKernelHandle:    owner.Handle,
		RemoteKernelPublicKey: owner.PublicKey,
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
func remoteReceiptInvalid(r Receipt, mp, rbps, sentValue int64, replyJSON []byte) string {
	// refresh_proxy is only ever a valid zero-charge pre-execution rejection (§13 rule C). A receipt
	// setting it on a success or any charged/value-bearing failure is malformed and quarantines,
	// so a hostile peer cannot pair a paid receipt with a cache-invalidation signal.
	if r.RefreshProxy && !(r.Status == TxFailure && r.Charge == 0 && r.Premium == 0 && r.Value == 0 && r.ValuePremium == 0) {
		return "refresh_proxy on a non-rejection receipt"
	}
	switch r.Status {
	case TxSuccess:
		if r.Charge != mp {
			return "success charge != mp"
		}
		if r.Value != sentValue {
			return "success value != sent"
		}
		if h, err := jcsHashStr(string(replyJSON)); err != nil || r.ReplyHash != h {
			return "reply_hash mismatch"
		}
	case TxFailure:
		if r.Charge < 0 || r.Charge > mp {
			return "failure charge out of range"
		}
		// Value delivery is all-or-nothing (§13): a failed transfer delivers nothing.
		if r.Value != 0 {
			return "failure delivered value"
		}
	default:
		return "unknown status"
	}
	// The two channels are audited separately (§13, the un-folded model): the execution premium must be
	// the manifest-snapshot rate on the charge, and the value premium the same rate on the delivered
	// value — never ceil((charge+value)·rbps), whose rounding merge is the bug. Either mismatch quarantines.
	if r.Premium != ceilDiv(r.Charge*rbps, 10000) {
		return "premium != ceil(charge*rbps)"
	}
	if r.ValuePremium != ceilDiv(r.Value*rbps, 10000) {
		return "value_premium != ceil(value*rbps)"
	}
	return ""
}

// settleRemoteCall settles a remote-proxy call after ExecuteFederation returns.
// If the receipt is absent or has an invalid signature, the trace stays open for retry (ErrTimeout).
// Otherwise it commits CommitRemoteSettlement with the correct charge/duty/refund split.
func (k *Kernel) settleRemoteCall(ctx context.Context, logger *log.Logger, action *Action, ktx *Transaction, trace *Trace, callerWalletID, callerWalletKind string, req CallRequest, target *User, mp int64, fr FederationResult, latency float64) (*CallReply, error) {
	// The value we dispatched (for a value transfer) and the contract hash we dispatched with ride on
	// the trace, so both the direct and the retry settle paths read them from one source (§13).
	var sentValue int64
	var dispatchedHash string
	if trace.DispatchJSON != nil {
		var d dispatchPayload
		if json.Unmarshal([]byte(*trace.DispatchJSON), &d) == nil {
			sentValue = d.Value
			dispatchedHash = d.ContractHash
		}
	}
	// A missing, unparseable, unsigned, or mismatched receipt keeps the trace open for retry.
	// action_id and args_hash are enforced here so settlement is valid by construction.
	expectedArgsHash, _ := jcsHashStr(string(ktx.ArgsJSON))
	rp, err := parseAndVerifyRemoteReceipt(fr.ReceiptJSON, target.PublicKey, action.RemoteActionID, expectedArgsHash)
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
	rbps := int64(0)
	if action.RemoteBPS != nil {
		rbps = *action.RemoteBPS
	}
	// The two channels settle independently (§13): the EXECUTION channel (charge/premium/import) is the
	// normal proxy settlement on the trace-funded gross q; the VALUE channel is the TransferEffect reserve
	// locked on the caller C, disposed of via `vs` — settled to the peer proxy row on success, refunded on
	// a valid failure, or kept LOCKED (quarantined) on an invalid receipt after a possibly-executed dispatch.
	var premium, importFee int64
	var valueForReceipt, valuePremForReceipt int64
	var valueToForReceipt string
	vs := ValueSettlement{Reserve: trace.ValueReserve}
	if invalid := remoteReceiptInvalid(r, mp, rbps, sentValue, replyJSON); invalid != "" {
		logger.Warn("remote.receipt_invalid", "action", action.Name, "reason", invalid)
		charge = 0
		ktx.Status = TxFailure
		ktx.Reason = "remote receipt invalid: " + invalid
		vs.Quarantine = true
	} else {
		premium = r.Premium // execution serving markup
		ktx.Status = r.Status
		ktx.Reason = r.Reason
		if r.Status == TxSuccess {
			importFee = ceilDiv((charge+premium)*k.cfg.ImportBPS, 10000) // execution import on the execution obligation
			ktx.ReplyJSON = json.RawMessage(replyJSON)
			// Value channel: C owes the peer value+value_premium (credited to the proxy row), and origin
			// sys retains value_import; the remainder of the reserve refunds to C inside the commit.
			vs.Credit = r.Value + r.ValuePremium
			vs.SysCredit = ceilDiv((r.Value+r.ValuePremium)*k.cfg.ImportBPS, 10000)
			valueForReceipt = r.Value
			valuePremForReceipt = trace.ValueReserve - r.Value // caller's value overhead: value_premium + value_import
			valueToForReceipt = r.ValueTo
		} else {
			vs.Refund = true // valid failure/rejection: return the whole value reserve to C
		}
	}
	paid := charge + premium // execution bilateral payable to the peer
	ktx.Net = paid
	ktx.Fee = importFee
	ktx.RemoteReceiptHash = sha256Hex(fr.ReceiptJSON)
	ktx.RemoteReceiptJSON = fr.ReceiptJSON

	stats := k.computeStats(ctx, action.ID, ktx, latency)
	localReceipt, receiptErr := k.buildReceipt(ktx, paid+importFee, 0, valueForReceipt, valuePremForReceipt, valueToForReceipt)
	if receiptErr != nil {
		return nil, ErrInternal.Wrap("could not build receipt")
	}
	// Classify a failure BEFORE committing: the commit stores the error body a replaying peer will
	// be served, so it needs this call's code. Computing it here also keeps one definition of the
	// 402/unfunded predicate, reused for the returned error below.
	var failErr error
	if ktx.Status != TxSuccess {
		reason := ktx.Reason
		if reason == "" {
			reason = "remote call failed"
		}
		failErr = ErrExecutionFailed.Wrap(reason)
		// A signed zero-charge rejection carried on transport status 402 is the remote's structured
		// ErrInsufficientFunds (the inbound handler's 402 mapping): OUR prepaid credit there is
		// exhausted, not the caller's balance. Surface it as the operator-actionable ErrPeerUnfunded
		// so a client never renders it as the caller's own insufficient_funds. Gated on the receipt
		// being settleable (charge == 0 with a preserved remote reason), so a quarantined invalid
		// receipt — which also forces charge 0 — never takes this branch.
		if fr.HTTPStatus == 402 && charge == 0 && ktx.Reason == r.Reason {
			failErr = PeerUnfundedError(target.Handle)
		}
	}

	// Detach settlement from execution-scoped cancellation so the remote settlement
	// (charge/duty/refund + audit record) always commits once the signed receipt is in.
	sctx, cancel := settlementContext(ctx)
	defer cancel()
	if err := k.store.CommitRemoteSettlement(sctx, ktx, localReceipt, trace.ID, callerWalletID, callerWalletKind, target.ID, k.cfg.FeeRecipientID, paid, importFee, vs, stats, req.IdempotencyRecordID, req.StepID, KernelErrorCode(failErr)); err != nil {
		return nil, ErrInternal.Wrap("could not commit remote settlement")
	}

	// Rule C (§8/§13): a settlement outcome proving the cache wrong — a signed refresh_proxy rejection
	// or a quarantined receipt — deactivates the proxy so the next call re-resolves. Supervision-side,
	// outside the monetary write set (a failure here never rolls back settlement), and hash-conditional
	// so a stale dispatch settling after a re-resolve spares the refreshed row. A funding (402) rejection
	// carries no refresh_proxy, so it never reaches here.
	if r.RefreshProxy || vs.Quarantine {
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
	fe, ok := k.http.(FederationExecutor)
	if !ok {
		return ErrInvalidState.Wrap("federation executor not configured")
	}
	mp := dispatch.RemotePrice
	if mp == 0 {
		// Fallback: derive from action.Price and RemoteBPS.
		mp = k.remoteManifestPrice(action.Price)
	}
	// The locked gross is the proxy's full two-step local price (§13) — what BeginRun/BeginStepCall
	// funded — not the bare serving markup; settlement pays charge+premium to the peer and the import
	// fee to origin sys out of it, refunding the remainder.
	q := action.Price

	callerWalletID, callerWalletKind := callerWalletFor(dispatch.StepID, process.ID, trace.ParentTraceID)

	now := time.Now().UTC()
	ktx := newTraceFailureTx(trace, process, action, q, now)
	if trace.ParentTraceID != nil {
		ktx.ParentTraceID = *trace.ParentTraceID
	}
	ktx.RemoteActionID = action.RemoteActionID
	argsJSON, _ := json.Marshal(dispatch.Args)
	ktx.ArgsJSON = json.RawMessage(argsJSON)

	req := CallRequest{StepID: dispatch.StepID}
	// The inbound record rides on the trace, so it survives for every action kind and is found
	// here whether this retry settles on a receipt or hits the max-age bound below.
	if trace.IdempotencyRecordID != nil {
		req.IdempotencyRecordID = *trace.IdempotencyRecordID
	}

	// fr.NotDispatched is deliberately ignored on the retry path: a parked trace's request may
	// already have executed remotely, so §13 forbids fail-fast here — only a signed receipt or the
	// max-pending-age bound below settles it. Never-dispatched fail-fast lives solely in Call (§6).
	fr, _ := fe.ExecuteFederation(ctx, target.PublicKey, action.Source, action.ArtifactHash, *trace.IdempotencyKey, dispatch.Args)
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
		ktx.Reason = "remote call unsettled past max pending age"
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

// ---- Peer operations ----

// ResolveKernelKey maps a kernel qualifier (from an owner@kernel/name reference) to the peer's
// public key and, when one exists locally, its mount (the peer proxy user). Resolution order is
// strictly: a bound local alias (a peer user's handle) → a raw base64url key → not found. A
// discovered-kernel label NEVER resolves a reference (§13), so no kernel can capture a name by
// gossiping a label first. mount may be nil for a raw key not yet mounted.
func (k *Kernel) ResolveKernelKey(ctx context.Context, ident string) (peerKey string, mount *User, err error) {
	ident = strings.TrimSpace(ident)
	if u, err := k.store.ReadUserByHandle(ctx, ident); err == nil && u != nil && u.PublicKey != "" {
		return u.PublicKey, u, nil
	}
	if looksLikeKey(ident) {
		u, _ := k.store.ReadUserByPublicKey(ctx, ident) // may be nil: known key, not yet mounted
		return ident, u, nil
	}
	return "", nil, ErrNotFound.Wrapf("kernel %q is not a known peer alias or key", ident)
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
		if uerr != nil || u == nil {
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
	resolver, ok := k.http.(RemoteResolver)
	if !ok {
		return "", "", ErrNotFound.Wrap("remote resolution unavailable")
	}
	remoteUserID, _, rerr := resolver.ResolveRemoteUser(ctx, peerKey, owner)
	if rerr != nil {
		return "", "", rerr
	}
	if mount == nil {
		handle := kernelAlias
		if IsPublicKey(kernelAlias) && len(peerKey) >= 8 {
			handle = "k-" + peerKey[:8]
		}
		if mount, err = k.CreateOrUpdateProxyPeer(ctx, handle, peerKey); err != nil {
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
	if err != nil || u == nil || u.SuspendedAt != nil {
		return "", "", ErrNotFound.Wrapf("user %s not found", ref)
	}
	return u.ID, u.Handle, nil
}

// ResolveKernelMount is ResolveKernelKey requiring an existing local mount (peer account).
func (k *Kernel) ResolveKernelMount(ctx context.Context, ident string) (*User, error) {
	_, mount, err := k.ResolveKernelKey(ctx, ident)
	if err != nil {
		return nil, err
	}
	if mount == nil {
		return nil, ErrNotFound.Wrapf("kernel %q has no local account yet", ident)
	}
	return mount, nil
}

// CreateOrUpdateProxyPeer creates or updates a local user record representing a remote kernel peer,
// addressed by Ed25519 public key. Location is not stored — the federation transport resolves the
// key to a live path (§13) — so re-friending a known key is idempotent with nothing to update.
// Used both by the friendship acceptance path and by federation admins.
func (k *Kernel) CreateOrUpdateProxyPeer(ctx context.Context, handle, publicKey string) (*User, error) {
	if publicKey == "" {
		return nil, ErrInvalidInput.Wrap("handle and public_key are required")
	}
	// Canonicalize the local proxy alias only; the manifest owner_handle and signature
	// inputs are never rewritten (they must match what the remote kernel signed).
	handle = NormalizeHandle(handle)
	if err := validateHandle(handle); err != nil {
		return nil, err
	}
	if _, err := decodeRemotePublicKey(publicKey); err != nil {
		return nil, err
	}
	// Same identity → idempotent re-registration; nothing to update (no location is stored).
	existing, err := k.store.ReadUserByPublicKey(ctx, publicKey)
	if err == nil && existing != nil {
		return existing, nil
	}
	// Find a free handle: try handle, handle-2, ..., handle-99.
	resolvedHandle := ""
	for i := 0; i < 99; i++ {
		candidate := handle
		if i > 0 {
			candidate = handle + "-" + strconv.Itoa(i+1)
		}
		byHandle, _ := k.store.ReadUserByHandle(ctx, candidate)
		if byHandle == nil {
			resolvedHandle = candidate
			break
		}
	}
	if resolvedHandle == "" {
		return nil, ErrInvalidInput.Wrapf("no free handle for %s (tried 99 variants)", handle)
	}
	now := time.Now().UTC()
	// A key-only account: no password, authenticates by federation signature. Same insert.
	u := &User{
		ID:        uuid.New().String(),
		Handle:    resolvedHandle,
		PublicKey: publicKey,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := k.store.CreateUser(ctx, u); err != nil {
		// A friend and its reciprocal can both pass the existence check above and race to
		// create the same proxy user (CLI + server both writing this DB); the loser hits a
		// unique-key conflict. Treat that as idempotent success: re-read by public key and
		// return the row the winner created, rather than surfacing the conflict.
		if winner, rerr := k.store.ReadUserByPublicKey(ctx, publicKey); rerr == nil && winner != nil {
			return winner, nil
		}
		return nil, err
	}
	k.log.With(ctx).Info("peer.created", "handle", resolvedHandle, "public_key", publicKey)
	return u, nil
}

// AddPeer requires superuser and creates/updates a proxy peer record.
func (k *Kernel) AddPeer(ctx context.Context, subjectID, handle, publicKey string) (*User, error) {
	if err := k.requireSuperuser(ctx, subjectID); err != nil {
		return nil, err
	}
	return k.CreateOrUpdateProxyPeer(ctx, handle, publicKey)
}

// ListPeers returns remote kernel peers (proxy users, identified by a set public_key), newest
// first. includeSuspended and limit/offset are pushed to the store (limit<=0 = all).
func (k *Kernel) ListPeers(ctx context.Context, includeSuspended bool, limit, offset int) ([]*User, error) {
	return k.store.ListPeers(ctx, includeSuspended, limit, offset)
}

// PeerKeys returns the public keys of all known, non-suspended peers — the pull set for the
// discovery loop's peer sync (§13 peer sync). Enumerates unbounded (limit 0).
func (k *Kernel) PeerKeys(ctx context.Context) []string {
	peers, err := k.ListPeers(ctx, false, 0, 0)
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(peers))
	for _, p := range peers {
		keys = append(keys, p.PublicKey)
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
		// Execution metrics come only from the subject's own first-party rows.
		if e.IssuerPublicKey == subjectKernelPublicKey {
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
// PEX peer-exchange bounds (§13). One sample size caps both directions; a stub is evicted after a
// few non-verified pulls; the unverified cache is bounded so hints can't grow it without bound.
const (
	maxGossipKernelHints = 25
	maxStubAttempts      = 3
	maxUnverifiedKernels = 200
)

// gossipHintHorizon is how recently this kernel must have verified another for it to be relayed as a
// PEX hint (§13). Freshness-gated relay is the standard alternative to death certificates: a kernel
// dead longer than the horizon is hinted by no one and converges out. Derived from the discovery
// interval (max with 1h so a short test interval still leaves a workable window), never a config key.
func (k *Kernel) gossipHintHorizon() time.Duration {
	iv := k.cfg.DiscoveryInterval
	if iv <= 0 {
		iv = 300 * time.Second
	}
	if h := 12 * iv; h > time.Hour {
		return h
	}
	return time.Hour
}

func (k *Kernel) GetGossip(ctx context.Context, requesterKey, cursor string) (*GossipResponse, error) {
	ourKey := k.ourKeyB64()
	handle, _ := k.store.GetConfig(ctx, "kernel_handle")
	// The kernel's self-description is @sys's user description (§13): one primitive, not a config key.
	var about string
	if sys, err := k.store.ReadUserByHandle(ctx, "sys"); err == nil && sys != nil {
		about = sys.Description
	}

	actions, err := k.store.ListVisibleActions(ctx, false, 100, 0)
	if err != nil {
		return nil, err
	}
	var manifests []*ActionManifest
	ownerSeen := map[string]bool{}
	var users []GossipUser
	for _, a := range actions {
		if !a.Active || a.Visibility != VisibilityPublic {
			continue
		}
		if k.isDelegatedAuth(a) {
			continue // delegated-auth actions are never advertised to peers (§8/§13)
		}
		if a.Kind == KindRemoteProxy {
			continue // imports are not our own actions; peers reach them by resolving the owner directly (§13)
		}
		m, merr := k.GetActionManifest(ctx, a.ID)
		if merr != nil || m == nil {
			continue
		}
		manifests = append(manifests, m)
		if !ownerSeen[a.OwnerUserID] {
			ownerSeen[a.OwnerUserID] = true
			if ow, oerr := k.store.ReadUser(ctx, a.OwnerUserID); oerr == nil && ow != nil && ow.PublicKey == "" {
				users = append(users, GossipUser{UserID: ow.ID, Handle: ow.Handle, Description: ow.Description})
			}
		}
	}
	// Always advertise @sys so a discovering kernel can index the operator identity.
	if sys, err := k.store.ReadUserByHandle(ctx, "sys"); err == nil && sys != nil && !ownerSeen[sys.ID] {
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
	if requesterKey != "" {
		if u, _ := k.store.ReadUserByPublicKey(ctx, requesterKey); u != nil && u.SuspendedAt == nil {
			bal := u.Available
			resp.CounterpartyBalance = &bal
		}
	}

	// PEX (§13): learn the authenticated requester as an unverified stub (the "advertise" half — a
	// puller thereby becomes discoverable), and relay a bounded random sample of kernels we have
	// ourselves verified within the freshness horizon. Keys only; best-effort; a stub row is never
	// a user account, so gossip opens no billing relationship.
	if requesterKey != "" && requesterKey != ourKey {
		_ = k.store.InsertDiscoveredKernelStub(ctx, requesterKey, time.Now().UTC(), maxUnverifiedKernels)
	}
	if sample, err := k.store.SampleVerifiedKernels(ctx, time.Now().UTC().Add(-k.gossipHintHorizon()), maxGossipKernelHints); err == nil {
		for _, hk := range sample {
			if hk != ourKey && hk != requesterKey {
				resp.KnownKernels = append(resp.KnownKernels, hk)
			}
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

// gossipEvidencePage builds one ordered evidence page after cursor, plus the next cursor (§13).
func (k *Kernel) gossipEvidencePage(ctx context.Context, ourKey, cursor string) ([]EvidenceBundle, string, error) {
	rows, err := k.store.ListReceiptsForGossip(ctx, cursor, gossipEvidencePageSize)
	if err != nil {
		return nil, "", err
	}
	bundles := make([]EvidenceBundle, 0, len(rows))
	next := cursor
	for _, row := range rows {
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
		next = row.Cursor
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

// ReadDiscoveredKernel returns the slim discovered-kernel row for a public key, or nil if unknown.
func (k *Kernel) ReadDiscoveredKernel(ctx context.Context, publicKey string) (*DiscoveredKernel, error) {
	return k.store.ReadDiscoveredKernel(ctx, publicKey)
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
	if dk, err := k.store.ReadDiscoveredKernel(ctx, publicKey); err == nil && dk != nil {
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
	u, err := k.store.ReadUserByPublicKey(ctx, publicKey)
	if err != nil || u == nil || u.SuspendedAt != nil {
		return nil
	}
	return k.store.UpdatePeerSync(ctx, u.ID, time.Now().UTC(), credit)
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
		ActionID:     actionParam,    // action ref string (no UUID; no call was executed)
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
	if err := k.store.CreateOrUpdateDiscoveredKernel(ctx, &DiscoveredKernel{
		PublicKey: gossip.PublicKey,
		Handle:    gossip.Handle,
		About:     gossip.About,
		FirstSeen: now,
		UpdatedAt: now,
	}); err != nil {
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

	// PEX (§13): ingest the relayed known-kernel sample as unverified stubs — information, never
	// authority. Keys only, bounded, own/sender/syntactically-invalid skipped; a stub is believed
	// only after this kernel's own direct verified pull.
	for i, hk := range gossip.KnownKernels {
		if i >= maxGossipKernelHints {
			break
		}
		if hk == "" || hk == gossip.PublicKey || hk == k.ourKeyB64() {
			continue
		}
		if _, err := decodeRemotePublicKey(hk); err != nil {
			continue
		}
		_ = k.store.InsertDiscoveredKernelStub(ctx, hk, now, maxUnverifiedKernels)
	}
	return gossip.NextCursor, nil
}

// KernelsForPull returns up to limit discovered-kernel keys in PEX pull order (§13), the candidate
// set the discovery loop unions with peers and bootstrap seeds.
func (k *Kernel) KernelsForPull(ctx context.Context, limit int) []string {
	keys, _ := k.store.ListKernelsForPull(ctx, limit)
	return keys
}

// RecordKernelPullFailure counts one non-verified pull against a candidate and evicts a spent stub
// (§13), so a dead or poisoned hint rotates to the back and is dropped rather than pinning the set.
func (k *Kernel) RecordKernelPullFailure(ctx context.Context, publicKey string) error {
	return k.store.RecordKernelPullFailure(ctx, publicKey, time.Now().UTC(), maxStubAttempts)
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
		"effect":        m.Effect,
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
	return a, nil
}

func (k *Kernel) importRemoteActionCore(ctx context.Context, remoteUserID string, m ActionManifest) (*ImportResult, error) {
	remoteUser, err := k.store.ReadUser(ctx, remoteUserID)
	if err != nil {
		return nil, err
	}
	if remoteUser.PublicKey == "" {
		return nil, ErrInvalidInput.Wrap("user is not a remote kernel")
	}
	if m.ActionID == "" {
		return nil, ErrInvalidInput.Wrap("manifest missing action_id")
	}
	if err := VerifyManifestSignature(remoteUser.PublicKey, &m); err != nil {
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
	// The peer is identified by remoteUser.PublicKey; the federation transport resolves that
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
	sr := m.Price + ceilDiv(m.Price*rbps, 10000)
	proxyPrice := sr + ceilDiv(sr*k.cfg.ImportBPS, 10000)
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
			a.Effect = m.Effect // signed effect contract: the proxy is value-bearing iff the peer signed it
		},
		new: func() *Action {
			now := time.Now().UTC()
			return &Action{
				ID:             uuid.New().String(),
				OwnerUserID:    remoteUserID,
				Name:           name,
				Kind:           KindRemoteProxy,
				Active:         false,
				Visibility:     VisibilityPrivate, // promoted to local on successful resolve (§8, below)
				Price:          proxyPrice,
				Description:    m.Description,
				InputSchema:    m.InputSchema,
				OutputSchema:   m.OutputSchema,
				Source:         source,
				ArtifactHash:   contentHash,
				RemoteActionID: m.ActionID,
				RemoteOwnerID:  m.OwnerID,
				RemoteBPS:      &rbps,
				Effect:         m.Effect,
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
func (k *Kernel) GetActionManifest(ctx context.Context, actionID string) (*ActionManifest, error) {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if !a.Active || a.Visibility != VisibilityPublic {
		return nil, ErrUnauthorized.Wrap("manifest only available for public active actions")
	}
	// Delegated-OAuth actions are never advertised: a remote peer's proxy user cannot complete a
	// browser consent, so importing one could only ever produce grant-required failures (§8/§13).
	if k.isDelegatedAuth(a) {
		return nil, ErrUnauthorized.Wrap("delegated-OAuth actions are not served as manifests")
	}
	// Imported (remote_proxy) actions are never re-exported: a kernel serves manifests only for its
	// own actions, so friendship stays non-transitive — reaching a peer's imported action requires
	// friending its true owner directly (§13).
	if a.Kind == KindRemoteProxy {
		return nil, ErrUnauthorized.Wrap("imported (remote-proxy) actions are not re-exported to peers")
	}
	owner, err := k.store.ReadUser(ctx, a.OwnerUserID)
	if err != nil {
		return nil, err
	}
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
		Effect:       a.Effect, // signed so a peer decides value-bearing from the contract, not the name (§13)
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
// corresponding to pubKeyB64 (base64url Ed25519 public key).
func VerifyManifestSignature(pubKeyB64 string, m *ActionManifest) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return err
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
func fedCallPayload(action, counterparty, recipient, expectedContractHash, idempotencyKey, timestamp, argsHash string) map[string]string {
	return map[string]string{
		"action":                 action,
		"args_hash":              argsHash,
		"counterparty":           counterparty,
		"expected_contract_hash": expectedContractHash,
		"idempotency_key":        idempotencyKey,
		"recipient":              recipient,
		"timestamp":              timestamp,
	}
}

// VerifyFederationSignature verifies an Ed25519 signature over the canonical federation call payload.
func VerifyFederationSignature(pubKeyB64, action, counterparty, recipient, expectedContractHash, idempotencyKey, timestamp, argsHash, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	if err := verifyJCS(pub, sigDomainFedCall, fedCallPayload(action, counterparty, recipient, expectedContractHash, idempotencyKey, timestamp, argsHash), sigB64); err != nil {
		return ErrUnauthenticated.Wrap("federation signature is invalid")
	}
	return nil
}

// SignFederationPayload creates a base64url Ed25519 signature over the canonical federation payload.
func SignFederationPayload(key ed25519.PrivateKey, action, counterparty, recipient, expectedContractHash, idempotencyKey, timestamp, argsHash string) (string, error) {
	return signJCS(key, sigDomainFedCall, fedCallPayload(action, counterparty, recipient, expectedContractHash, idempotencyKey, timestamp, argsHash))
}

// SignStepPayload creates a base64url Ed25519 signature over the canonical step-completion payload
// JCS({counterparty, idempotency_key, input_hash, recipient, step_id, timestamp}) — a key-set
// disjoint from every other signed Juice payload (§12, §13).
func SignStepPayload(key ed25519.PrivateKey, stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash string) (string, error) {
	return signJCS(key, sigDomainStepComplete, stepPayload(stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash))
}

// VerifyStepSignature verifies an Ed25519 signature over the canonical step-completion payload.
// recipient must be the verifying kernel's own public key.
func VerifyStepSignature(pubKeyB64, stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	if err := verifyJCS(pub, sigDomainStepComplete, stepPayload(stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash), sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step signature is invalid")
	}
	return nil
}

func stepPayload(stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash string) map[string]string {
	return map[string]string{
		"counterparty":    counterparty,
		"idempotency_key": idempotencyKey,
		"input_hash":      inputHash,
		"recipient":       recipient,
		"step_id":         stepID,
		"timestamp":       timestamp,
	}
}

// SignStepAuthPayload signs the home-kernel attestation that its authenticated local user (userID)
// authorized completing step stepID (§13). The fixed scope "step_auth" plus the user_id key keep
// this key-set disjoint from the step-complete and step-list payloads; recipient binds it to the
// serving kernel, closing cross-kernel replay.
func SignStepAuthPayload(key ed25519.PrivateKey, counterparty, recipient, userID, stepID, timestamp string) (string, error) {
	return signJCS(key, sigDomainStepAuth, stepAuthPayload(counterparty, recipient, userID, stepID, timestamp))
}

// VerifyStepAuthSignature verifies the attestation. recipient must be the verifying kernel's own key.
func VerifyStepAuthSignature(pubKeyB64, counterparty, recipient, userID, stepID, timestamp, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	if err := verifyJCS(pub, sigDomainStepAuth, stepAuthPayload(counterparty, recipient, userID, stepID, timestamp), sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step attestation is invalid")
	}
	return nil
}

func stepAuthPayload(counterparty, recipient, userID, stepID, timestamp string) map[string]string {
	// No "scope" key: the sigDomainStepAuth prefix now provides domain separation (§12).
	return map[string]string{
		"counterparty": counterparty,
		"recipient":    recipient,
		"step_id":      stepID,
		"timestamp":    timestamp,
		"user_id":      userID,
	}
}

// SignStepAuth stamps the current time and signs the step_auth attestation with this kernel's
// platform key. counterparty is this kernel's own key; recipient is the serving peer's key.
func (k *Kernel) SignStepAuth(counterparty, recipient, userID, stepID string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignStepAuthPayload(k.cfg.SigningKey, counterparty, recipient, userID, stepID, ts)
	return
}

// SignStepListPayload creates a base64url Ed25519 signature over
// JCS({counterparty, recipient, scope, timestamp}). The fixed scope value keeps this key-set
// disjoint from every other signed payload (§12, §13).
func SignStepListPayload(key ed25519.PrivateKey, counterparty, recipient, timestamp string) (string, error) {
	return signJCS(key, sigDomainStepList, stepListPayload(counterparty, recipient, timestamp))
}

// VerifyStepListSignature verifies an Ed25519 signature over the canonical step-list payload.
// recipient must be the verifying kernel's own public key.
func VerifyStepListSignature(pubKeyB64, counterparty, recipient, timestamp, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	if err := verifyJCS(pub, sigDomainStepList, stepListPayload(counterparty, recipient, timestamp), sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step signature is invalid")
	}
	return nil
}

func stepListPayload(counterparty, recipient, timestamp string) map[string]string {
	// No "scope" key: the sigDomainStepList prefix now provides domain separation (§12).
	return map[string]string{
		"counterparty": counterparty,
		"recipient":    recipient,
		"timestamp":    timestamp,
	}
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
func settleOpenPayload(counterparty, recipient, settlementID string, amount int64, timestamp string) map[string]string {
	return map[string]string{"amount": strconv.FormatInt(amount, 10), "counterparty": counterparty, "recipient": recipient, "settlement_id": settlementID, "timestamp": timestamp}
}

func settleFinishPayload(counterparty, recipient, settlementID, nonce, timestamp string) map[string]string {
	return map[string]string{"counterparty": counterparty, "nonce": nonce, "recipient": recipient, "settlement_id": settlementID, "timestamp": timestamp}
}

func settleReconcilePayload(counterparty, recipient, settlementID, timestamp string) map[string]string {
	return map[string]string{"counterparty": counterparty, "recipient": recipient, "settlement_id": settlementID, "timestamp": timestamp}
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
	peer, err := k.store.ReadUserByPublicKey(ctx, debtorKey)
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
	// A prior paid outcome still awaiting its rail record must be finalized (admin settle --cash)
	// before opening a new lottery on the same position.
	if pending, err := k.store.HasPendingSettlement(ctx, peer.ID); err != nil {
		return nil, err
	} else if pending {
		return map[string]any{"status": "pending_cash", "handle": peer.Handle,
			"message": "a paid probabilistic outcome is awaiting its rail record; pay the quantum, then run `admin settle <peer> --cash <settlement_id>`"}, nil
	}
	d := peer.Available // > 0 ⇒ this kernel owes the peer (we are the debtor)
	switch {
	case d == 0:
		return map[string]any{"status": "settled", "amount": int64(0), "handle": peer.Handle}, nil
	case d < 0:
		return map[string]any{"status": "creditor", "amount": -d, "handle": peer.Handle,
			"message": "the peer owes you; its operator initiates settlement"}, nil
	}
	Q := k.cfg.SettlementQuantum
	settlementID := uuid.NewString()
	if Q <= 0 || d >= Q {
		return map[string]any{"status": "exact", "mode": "exact", "amount": d, "settlement_id": settlementID, "handle": peer.Handle,
			"message": fmt.Sprintf("pay %d on the rail, then run `admin withdraw %s %d --external-key %s`", d, peer.Handle, d, settlementID)}, nil
	}

	settler, ok := k.http.(FederationSettler)
	if !ok {
		return nil, ErrInvalidState.Wrap("federation transport does not support settlement")
	}
	our := k.ourKeyB64()

	// Round 1 — open: the creditor commits H(s).
	ts := time.Now().UTC().Format(time.RFC3339)
	openSig, err := signJCS(k.cfg.SigningKey, sigDomainSettleOpen, settleOpenPayload(our, peer.PublicKey, settlementID, d, ts))
	if err != nil {
		return nil, err
	}
	status, body, err := settler.Settle(ctx, peer.PublicKey, "open", ts, openSig, settlementID, d, "", nil)
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
	if err := verifySettlementRecord(&open, peer.PublicKey); err != nil {
		return nil, err
	}
	if open.SettlementID != settlementID || open.Amount != d || open.Creditor != peer.PublicKey || open.Quantum <= 0 {
		return nil, ErrInvalidState.Wrap("open record does not match request")
	}
	openJSON := body

	// Round 2 — finish: reveal the nonce; the creditor reveals s and applies its legs.
	nonce := uuid.NewString()
	ts2 := time.Now().UTC().Format(time.RFC3339)
	finishSig, err := signJCS(k.cfg.SigningKey, sigDomainSettleFinish, settleFinishPayload(our, peer.PublicKey, settlementID, nonce, ts2))
	if err != nil {
		return nil, err
	}
	status, body, err = settler.Settle(ctx, peer.PublicKey, "finish", ts2, finishSig, settlementID, 0, nonce, openJSON)
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
	if err := verifySettlementRecord(&final, peer.PublicKey); err != nil {
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
	res := map[string]any{"mode": "probabilistic", "outcome": final.Outcome, "settlement_id": settlementID, "handle": peer.Handle}
	if pay {
		res["status"] = "pending_cash"
		res["amount"] = open.Quantum
		res["message"] = fmt.Sprintf("you owe %d; pay it on the rail, then run `admin settle %s --cash %s`", open.Quantum, peer.Handle, settlementID)
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
	return map[string]any{"status": "settled", "settlement_id": settlementID, "cash": q, "handle": peer.Handle}, nil
}
