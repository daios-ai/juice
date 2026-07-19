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

// remoteManifestPrice derives the original remote manifest price (mp) from a proxy price
// (q = mp + import duty): mp = floor(q * 10000 / (10000 + ImportBPS)).
func (k *Kernel) remoteManifestPrice(proxyPrice int64) int64 {
	return proxyPrice * 10000 / (10000 + k.cfg.ImportBPS)
}

// RemoteManifestPrice is the exported form of remoteManifestPrice, used by the service layer to
// compare a peer's cached credit against a proxy action's underlying manifest price (§13 peer_state).
func (k *Kernel) RemoteManifestPrice(proxyPrice int64) int64 {
	return k.remoteManifestPrice(proxyPrice)
}

// dispatchPayload is the persisted remote-proxy dispatch record, stored on Trace.DispatchJSON
// so a pending remote call can be replayed verbatim by RetryPendingRemoteDispatches after restart.
type dispatchPayload struct {
	Args        map[string]any `json:"args"`
	StepID      string         `json:"step_id"`
	RemotePrice int64          `json:"remote_price"`
}

// marshalDispatch serializes a dispatchPayload and returns a pointer suitable for Trace.DispatchJSON.
func marshalDispatch(args map[string]any, stepID string, mp int64) *string {
	b, _ := json.Marshal(dispatchPayload{Args: args, StepID: stepID, RemotePrice: mp})
	s := string(b)
	return &s
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

// signJCS signs the JCS-canonical form of v with key.
func signJCS(key ed25519.PrivateKey, v any) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	payload, err := CanonicalJSON(v)
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}

// verifyJCS checks that sigB64 is a valid Ed25519 signature over the JCS-canonical form of v.
func verifyJCS(pub ed25519.PublicKey, v any, sigB64 string) error {
	payload, err := CanonicalJSON(v)
	if err != nil {
		return ErrInternal.Wrapf("canonicalize: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !ed25519.Verify(pub, payload, sig) {
		return ErrUnauthorized.Wrap("signature is invalid")
	}
	return nil
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
//   - Charge: tx.net == receipt.charge (local charge paid to proxy == what remote reported)
//   - SettlementArith: duty rule — tx.fee == ceil(tx.net*import_bps/10000) for success, 0 for failure
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

	// 2. Signature.
	checks.Signature = verifyRemoteReceiptSignature(&r, owner.PublicKey) == nil

	// 3. ActionID: receipt carries the remote action's ID.
	if tx.RemoteActionID != "" {
		checks.ActionID = r.ActionID == tx.RemoteActionID
	} else {
		checks.ActionID = r.ActionID == tx.ActionID
	}

	// 4. Status consistency.
	checks.Status = r.Status == tx.Status

	// 5. Charge: local tx.net (what we paid the proxy) must equal remote receipt.charge.
	checks.Charge = r.Charge == tx.Net

	// 6. Settlement arithmetic: duty rule.
	if tx.Status == TxSuccess {
		expectedDuty := ceilDiv(tx.Net*k.cfg.ImportBPS, 10000)
		checks.SettlementArith = tx.Fee == expectedDuty
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
		checks.Status && checks.Charge && checks.SettlementArith &&
		checks.RefundConservation && checks.ArgsHash && checks.ReplyHash

	return &ReceiptVerification{
		TransactionID:         txID,
		Valid:                 valid,
		RemoteKernelHandle:    owner.Handle,
		RemoteKernelPublicKey: owner.PublicKey,
		Checks:                checks,
		Receipt:               &r,
	}, nil
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
	return verifyJCS(pub, cp, r.Signature)
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
func remoteReceiptInvalid(r Receipt, mp int64, replyJSON []byte) string {
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
	return ""
}

// settleRemoteCall settles a remote-proxy call after ExecuteFederation returns.
// If the receipt is absent or has an invalid signature, the trace stays open for retry (ErrTimeout).
// Otherwise it commits CommitRemoteSettlement with the correct charge/duty/refund split.
func (k *Kernel) settleRemoteCall(ctx context.Context, logger *log.Logger, action *Action, ktx *Transaction, trace *Trace, callerWalletID, callerWalletKind string, req CallRequest, target *User, mp int64, fr FederationResult, latency float64) (*CallReply, error) {
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
	var duty int64
	if invalid := remoteReceiptInvalid(r, mp, replyJSON); invalid != "" {
		logger.Warn("remote.receipt_invalid", "action", action.Name, "reason", invalid)
		charge = 0
		ktx.Status = TxFailure
		ktx.Reason = "remote receipt invalid: " + invalid
	} else {
		if r.Status == TxSuccess {
			duty = ceilDiv(charge*k.cfg.ImportBPS, 10000)
			ktx.ReplyJSON = json.RawMessage(replyJSON)
		}
		ktx.Status = r.Status
		ktx.Reason = r.Reason
	}
	ktx.Net = charge
	ktx.Fee = duty
	ktx.RemoteReceiptHash = sha256Hex(fr.ReceiptJSON)
	ktx.RemoteReceiptJSON = fr.ReceiptJSON

	stats := k.computeStats(ctx, action.ID, ktx, latency)
	localReceipt, receiptErr := k.buildReceipt(ktx, charge+duty)
	if receiptErr != nil {
		return nil, ErrInternal.Wrap("could not build receipt")
	}
	// Detach settlement from execution-scoped cancellation so the remote settlement
	// (charge/duty/refund + audit record) always commits once the signed receipt is in.
	sctx, cancel := settlementContext(ctx)
	defer cancel()
	if err := k.store.CommitRemoteSettlement(sctx, ktx, localReceipt, trace.ID, callerWalletID, callerWalletKind, target.ID, k.cfg.FeeRecipientID, charge, duty, stats, req.IdempotencyRecordID, req.StepID); err != nil {
		return nil, ErrInternal.Wrap("could not commit remote settlement")
	}

	logger.Info("remote.settled", "action", action.Name, "status", ktx.Status, "charge", charge, "duty", duty)

	if ktx.Status == TxSuccess {
		return &CallReply{Result: fr.Result, TxID: ktx.ID, TraceID: trace.ID, ReceiptID: localReceipt.ID}, nil
	}
	reason := ktx.Reason
	if reason == "" {
		reason = "remote call failed"
	}
	failErr := error(ErrExecutionFailed.Wrap(reason))
	// A signed zero-charge rejection carried on transport status 402 is the remote's structured
	// ErrInsufficientFunds (the inbound handler's 402 mapping): OUR prepaid credit there is
	// exhausted, not the caller's balance. Surface it as the operator-actionable ErrPeerUnfunded
	// so a client never renders it as the caller's own insufficient_funds. Gated on the receipt
	// being settleable (charge == 0 with a preserved remote reason), so a quarantined invalid
	// receipt — which also forces charge 0 — never takes this branch.
	if fr.HTTPStatus == 402 && charge == 0 && ktx.Reason == r.Reason {
		failErr = PeerUnfundedError(target.Handle)
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
		// Fallback: derive from action.Price and ImportBPS.
		mp = k.remoteManifestPrice(action.Price)
	}
	maxDuty := ceilDiv(mp*k.cfg.ImportBPS, 10000)
	q := mp + maxDuty

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

	// fr.NotDispatched is deliberately ignored on the retry path: a parked trace's request may
	// already have executed remotely, so §13 forbids fail-fast here — only a signed receipt or the
	// max-pending-age bound below settles it. Never-dispatched fail-fast lives solely in Call (§6).
	fr, _ := fe.ExecuteFederation(ctx, target.PublicKey, action.Source, *trace.IdempotencyKey, dispatch.Args)
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
		_, sErr := k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, 0, ErrTimeout.Wrap("remote call unsettled past max pending age"))
		return sErr
	}
	return nil
}

// SignFederation signs a federation payload with the platform key and returns
// (signature, timestamp). Returns an error if the signing key is not configured.
func (k *Kernel) SignFederation(action, counterparty, idempotencyKey, argsHash string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignFederationPayload(k.cfg.SigningKey, action, counterparty, idempotencyKey, ts, argsHash)
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

// ListPeers returns all remote kernel peers (proxy users, identified by a set public_key).
func (k *Kernel) ListPeers(ctx context.Context) ([]*User, error) {
	all, err := k.store.ListUsers(ctx, 1000, 0)
	if err != nil {
		return nil, err
	}
	var peers []*User
	for _, u := range all {
		if u.PublicKey != "" {
			peers = append(peers, u)
		}
	}
	return peers, nil
}

// PeerKeys returns the public keys of all known, non-suspended peers — the pull set for the
// discovery loop's peer sync (§13 peer sync).
func (k *Kernel) PeerKeys(ctx context.Context) []string {
	peers, err := k.ListPeers(ctx)
	if err != nil {
		return nil
	}
	var keys []string
	for _, p := range peers {
		if p.SuspendedAt == nil && p.PublicKey != "" {
			keys = append(keys, p.PublicKey)
		}
	}
	return keys
}

// Unsubscribe drops a peer's imported catalog here by deactivating all proxy actions it owns
// (§13). The peer's billing account, balance, and history are untouched; a later subscribe
// re-imports. Superuser-only.
func (k *Kernel) Unsubscribe(ctx context.Context, subjectID, handle string) error {
	if err := k.requireSuperuser(ctx, subjectID); err != nil {
		return err
	}
	u, err := k.ResolveUser(ctx, handle)
	if err != nil {
		return ErrNotFound.Wrapf("peer %q not found", handle)
	}
	return k.store.DeactivateActionsOwnedBy(ctx, u.ID)
}

// ListDiscoveredKernels returns all kernels learned via gossip accumulation.
func (k *Kernel) ListDiscoveredKernels(ctx context.Context) ([]*DiscoveredKernel, error) {
	return k.store.ListDiscoveredKernels(ctx)
}

// PeerLocalView returns the locally-held view of a friended peer — its handle, public key, and the
// active proxy actions imported from it — for the offline-inspect fallback (§13). The identifier is
// an `@handle` or a base64url key. ErrNotFound when no local proxy user matches.
func (k *Kernel) PeerLocalView(ctx context.Context, ident string) (handle, publicKey string, actions []GossipAction, err error) {
	u, _ := k.ResolveUser(ctx, ident)
	if u == nil || u.PublicKey == "" {
		return "", "", nil, ErrNotFound.Wrapf("no friended peer %q", ident)
	}
	acts, _ := k.store.ListActionsByOwner(ctx, u.ID, 500, 0)
	for _, a := range acts {
		if !a.Active || a.Kind != KindRemoteProxy {
			continue
		}
		ga := GossipAction{ActionID: a.ID, Name: qualifiedActionName(a), Description: a.Description, Price: a.Price}
		if s, _ := k.store.ReadStats(ctx, a.ID); s != nil {
			ga.Uses = s.Uses
			ga.Rating = s.RatingEstimate
		}
		actions = append(actions, ga)
	}
	return u.Handle, u.PublicKey, actions, nil
}

// DiscoveryRoster groups the raw discovered_kernels rows into the known-network directory view
// (§13): one entry per kernel, each carrying every introducer's gossiped action stats (self-report
// when the introducer is the kernel itself, hearsay otherwise) and, for kernels we have friended
// and transacted with, our own earned stats as ground truth. Enrichment lives here (not the CLI)
// so both the CLI and any HTTP client get the same shape. Display only — never callability.
func (k *Kernel) DiscoveryRoster(ctx context.Context) ([]*KernelRoster, error) {
	rows, err := k.store.ListDiscoveredKernels(ctx)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*KernelRoster{}
	var order []string
	for _, d := range rows {
		r := byKey[d.PublicKey]
		if r == nil {
			r = &KernelRoster{PublicKey: d.PublicKey, Handle: d.Handle}
			byKey[d.PublicKey] = r
			order = append(order, d.PublicKey)
		}
		if r.Handle == "" {
			r.Handle = d.Handle
		}
		var actions []GossipAction
		_ = json.Unmarshal(d.StatsJSON, &actions)
		r.Sources = append(r.Sources, RosterSource{
			IntroducedBy: d.IntroducedBy,
			SelfReported: d.IntroducedBy == d.PublicKey,
			Actions:      actions,
		})
	}
	// Attach our own earned stats for any discovered kernel we have a local proxy user for.
	for _, key := range order {
		proxy, err := k.store.ReadUserByPublicKey(ctx, key)
		if err != nil || proxy == nil {
			continue
		}
		stats, err := k.store.ListStatsByOwner(ctx, proxy.ID)
		if err != nil {
			continue
		}
		for _, s := range stats {
			act, err := k.store.ReadAction(ctx, s.ActionID)
			if err != nil || act == nil {
				continue
			}
			byKey[key].Own = append(byKey[key].Own, GossipAction{
				ActionID: s.ActionID, Name: qualifiedActionName(act), Price: act.Price,
				Uses: s.Uses, Rating: s.RatingEstimate,
			})
		}
	}
	out := make([]*KernelRoster, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out, nil
}

// PurgeIdlePeers reaps peers idle past PeerRetention at zero balance (§13 Retention): it deletes
// each such peer's proxy actions, stats, stat_tags, and discovered_kernels rows and forgets the
// peer identity, keeping the transaction ledger intact. Internal maintenance (like
// RetryPendingRemoteDispatches, no superuser gate) — driven by the serve sweep and once at startup.
// PeerRetention <= 0 disables it. Returns the number of peers purged.
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
	return purged, nil
}

// qualifiedActionName renders an action as @owner/name — the address form used everywhere else —
// so gossip and roster views name actions consistently (@sys/message, @alice/greet) rather than as
// bare names. For a remote proxy the local Name already encodes the remote owner (owner/name), and
// the OwnerHandle is only our private mount alias for the peer; naming it in the peer's own
// namespace (a leading @) keeps the name portable — the same action reads identically whether the
// peer self-reports it or we vouch for it. Falls back to the bare name if OwnerHandle wasn't loaded.
func qualifiedActionName(a *Action) string {
	if a.Kind == KindRemoteProxy {
		return "@" + a.Name
	}
	if a.OwnerHandle != "" {
		return a.OwnerHandle + "/" + a.Name
	}
	return a.Name
}

// GetGossip returns this kernel's gossip payload: identity, public active actions, and peer list.
// When requesterKey names a known, non-suspended peer, the response also carries that peer's credit
// on this kernel (CounterpartyBalance, §13 peer sync); it is nil for strangers, suspended keys, and
// anonymous pulls (requesterKey == "").
func (k *Kernel) GetGossip(ctx context.Context, requesterKey string) (*GossipResponse, error) {
	var pubKeyB64 string
	if len(k.cfg.SigningKey) == ed25519.PrivateKeySize {
		pub := k.cfg.SigningKey.Public().(ed25519.PublicKey)
		pubKeyB64 = base64.RawURLEncoding.EncodeToString(pub)
	}
	handle, _ := k.store.GetConfig(ctx, "kernel_handle")
	// The kernel's self-description is @sys's user description (§13): one primitive, not a config key.
	var about string
	if sys, err := k.store.ReadUserByHandle(ctx, "@sys"); err == nil && sys != nil {
		about = sys.Description
	}

	actions, err := k.store.ListVisibleActions(ctx, false, 100, 0)
	if err != nil {
		return nil, err
	}
	var gossipActions []GossipAction
	for _, a := range actions {
		if !a.Active {
			continue
		}
		if k.isDelegatedAuth(a) {
			continue // never advertised to peers (§8/§13)
		}
		if a.Kind == KindRemoteProxy {
			continue // imports are not our own actions; peers reach them only by friending the owner directly (§13)
		}
		stats, _ := k.store.ReadStats(ctx, a.ID)
		ga := GossipAction{
			ActionID:    a.ID,
			Name:        qualifiedActionName(a),
			Description: a.Description,
			Price:       a.Price,
		}
		if stats != nil {
			ga.Uses = stats.Uses
			ga.Rating = stats.RatingEstimate
		}
		gossipActions = append(gossipActions, ga)
	}

	peers, _ := k.ListPeers(ctx)
	var friendViews []GossipFriendView
	for _, p := range peers {
		if p.SuspendedAt != nil {
			continue
		}
		peerStats, _ := k.store.ListStatsByOwner(ctx, p.ID)
		if len(peerStats) == 0 {
			continue // not transacted; endorsement is earned by trade, not by subscribing
		}
		var fActions []GossipAction
		for _, s := range peerStats {
			act, err := k.store.ReadAction(ctx, s.ActionID)
			if err != nil || act == nil {
				continue
			}
			fActions = append(fActions, GossipAction{
				ActionID:    s.ActionID,
				Name:        qualifiedActionName(act),
				Description: act.Description,
				Price:       act.Price,
				Uses:        s.Uses,
				Rating:      s.RatingEstimate,
			})
		}
		friendViews = append(friendViews, GossipFriendView{
			Handle:    p.Handle,
			PublicKey: p.PublicKey,
			Actions:   fActions,
		})
	}

	resp := &GossipResponse{
		PublicKey: pubKeyB64,
		Handle:    handle,
		About:     about,
		Actions:   gossipActions,
		Friends:   friendViews,
	}
	// Report the requester's credit here only if it is a known, non-suspended peer (§13 peer sync).
	if requesterKey != "" {
		if u, _ := k.store.ReadUserByPublicKey(ctx, requesterKey); u != nil && u.SuspendedAt == nil {
			bal := u.Available
			resp.CounterpartyBalance = &bal
		}
	}
	return resp, nil
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
// settled failure is legible rather than always reading "denied".
func (k *Kernel) CreateSignedRejectionReceipt(counterpartyID, actionParam, argsHash, idempotencyKey, reason string) (*Receipt, error) {
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

// AccumulateGossip stores a gossip response in the discovered_kernels table and creates
// StatTag rows (key="gossip_uses"/"gossip_rating") for any gossip actions we have locally
// imported as proxy actions from that kernel. The source field is the introducer's public key.
func (k *Kernel) AccumulateGossip(ctx context.Context, gossip *GossipResponse, introducerPublicKey string) error {
	if gossip.PublicKey == "" {
		return ErrInvalidInput.Wrap("gossip missing public_key")
	}
	// gossip.Actions/Friends are peer-controlled and each drives a DB write; cap the fan-out so one
	// gossip pull cannot force an unbounded write amplification (§13 information, not authority).
	const maxGossipElements = 10000
	if len(gossip.Actions) > maxGossipElements {
		gossip.Actions = gossip.Actions[:maxGossipElements]
	}
	if len(gossip.Friends) > maxGossipElements {
		gossip.Friends = gossip.Friends[:maxGossipElements]
	}
	statsJSON, _ := json.Marshal(gossip.Actions)
	now := time.Now().UTC()
	if err := k.store.CreateOrUpdateDiscoveredKernel(ctx, &DiscoveredKernel{
		PublicKey:    gossip.PublicKey,
		IntroducedBy: introducerPublicKey,
		Handle:       gossip.Handle,
		StatsJSON:    json.RawMessage(statsJSON),
		FirstSeen:    now,
		UpdatedAt:    now,
	}); err != nil {
		return err
	}
	// Store each transacted peer as a discovered kernel, introduced by the gossip source.
	for _, f := range gossip.Friends {
		if f.PublicKey == "" {
			continue
		}
		friendStatsJSON, _ := json.Marshal(f.Actions)
		_ = k.store.CreateOrUpdateDiscoveredKernel(ctx, &DiscoveredKernel{
			PublicKey:    f.PublicKey,
			IntroducedBy: gossip.PublicKey,
			Handle:       f.Handle,
			StatsJSON:    json.RawMessage(friendStatsJSON),
			FirstSeen:    now,
			UpdatedAt:    now,
		})
	}

	// For each gossip action, find the matching local proxy (by remote_action_id) and
	// write gossip_uses / gossip_rating stat tags so @sys/lookup can use them as a prior.
	remoteUser, err := k.store.ReadUserByPublicKey(ctx, gossip.PublicKey)
	if err != nil || remoteUser == nil {
		return nil // peer not yet registered locally; skip stat tags
	}
	for _, ga := range gossip.Actions {
		proxy, err := k.store.ReadActionByOwnerRemoteID(ctx, remoteUser.ID, ga.ActionID)
		if err != nil || proxy == nil {
			continue // not imported locally
		}
		source := introducerPublicKey
		if source == "" {
			source = gossip.PublicKey
		}
		_ = k.store.UpsertStatTag(ctx, &StatTag{ActionID: proxy.ID, Key: "gossip_uses", Value: fmt.Sprintf("%d", ga.Uses), Source: source, UpdatedAt: now})
		_ = k.store.UpsertStatTag(ctx, &StatTag{ActionID: proxy.ID, Key: "gossip_rating", Value: fmt.Sprintf("%g", ga.Rating), Source: source, UpdatedAt: now})
	}
	return nil
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
	payload, _ := CanonicalJSON(map[string]any{
		"action_id":     m.ActionID,
		"artifact_hash": m.ArtifactHash,
		"description":   m.Description,
		"input_schema":  string(inputJSON),
		"kind":          string(m.Kind),
		"name":          m.Name,
		"output_schema": string(outputJSON),
		"owner_handle":  m.OwnerHandle,
		"price":         m.Price,
	})
	h := sha256.Sum256(payload)
	return hex.EncodeToString(h[:])
}

// ImportRemoteAction creates or updates a local remote_proxy action from a remote kernel's manifest.
// The action is owned by the remote kernel user identified by remoteUserID.
// It is idempotent: re-running with the same manifest preserves the action's active state.
// Only the superuser may import remote actions.
func (k *Kernel) ImportRemoteAction(ctx context.Context, subjectID, remoteUserID string, m ActionManifest) (*ImportResult, error) {
	if err := k.requireSuperuser(ctx, subjectID); err != nil {
		return nil, err
	}
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
	// Owner-qualified (addressed @peer/owner/name) so same-named actions from different owners on the peer don't collide.
	name := strings.TrimPrefix(m.OwnerHandle, "@") + "/" + m.Name
	// Proxy price = mp + ceil(mp * import_bps / 10000): the caller pays the remote price
	// plus the local import duty, all locked atomically at dispatch time.
	proxyPrice := m.Price + ceilDiv(m.Price*k.cfg.ImportBPS, 10000)
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
		},
		new: func() *Action {
			now := time.Now().UTC()
			return &Action{
				ID:             uuid.New().String(),
				OwnerUserID:    remoteUserID,
				Name:           name,
				Kind:           KindRemoteProxy,
				Active:         false,
				Visibility:     VisibilityPrivate, // promoted to local when the peer is friended (§13)
				Price:          proxyPrice,
				Description:    m.Description,
				InputSchema:    m.InputSchema,
				OutputSchema:   m.OutputSchema,
				Source:         source,
				ArtifactHash:   contentHash,
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

// UnimportRemoteAction deactivates the local proxy action for the given remote handle and action name.
func (k *Kernel) UnimportRemoteAction(ctx context.Context, subjectID, remoteHandle, actionName string) (*Action, error) {
	remoteUser, err := k.store.ReadUserByHandle(ctx, remoteHandle)
	if err != nil {
		return nil, ErrNotFound.Wrapf("remote kernel %q not found", remoteHandle)
	}
	if remoteUser.PublicKey == "" {
		return nil, ErrInvalidInput.Wrapf("%q is not a remote kernel", remoteHandle)
	}
	a, err := k.store.ReadActionByOwnerName(ctx, remoteUser.ID, actionName)
	if err != nil {
		return nil, err
	}
	if a.Kind != KindRemoteProxy {
		return nil, ErrInvalidInput.Wrap("action is not a remote proxy")
	}
	if subjectID != a.OwnerUserID {
		if err := k.requireSuperuser(ctx, subjectID); err != nil {
			return nil, ErrUnauthorized.Wrap("owner or superuser required to unimport remote action")
		}
	}
	if err := k.deactivateImported(ctx, []*Action{a}, false); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("action.unimported_remote", "action_id", a.ID, "name", a.Name)
	return a, nil
}

// ReconcileRemoteAction applies the remote-import policy for a single action.
// When manifest is nil (manifest endpoint non-200 or action gone), the local proxy is
// deactivated and stats are reset. When manifest is non-nil, ImportRemoteAction runs.
func (k *Kernel) ReconcileRemoteAction(ctx context.Context, subjectID, remoteHandle, actionName string, manifest *ActionManifest) (*ImportResult, error) {
	if manifest == nil {
		a, err := k.UnimportRemoteAction(ctx, subjectID, remoteHandle, actionName)
		if err != nil {
			return nil, err
		}
		_ = k.ResetActionStats(ctx, a.ID)
		return &ImportResult{}, nil
	}
	remoteUser, err := k.store.ReadUserByHandle(ctx, remoteHandle)
	if err != nil {
		return nil, ErrNotFound.Wrapf("remote kernel %q not found", remoteHandle)
	}
	return k.ImportRemoteAction(ctx, subjectID, remoteUser.ID, *manifest)
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
		OwnerHandle:  owner.Handle,
		Name:         a.Name,
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

// SignManifest creates a base64url Ed25519 signature over the canonical ActionManifest.
func SignManifest(key ed25519.PrivateKey, m *ActionManifest) (string, error) {
	cp := *m
	cp.Signature = ""
	return signJCS(key, cp)
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
	if err := verifyJCS(pub, cp, m.Signature); err != nil {
		return ErrUnauthorized.Wrap("manifest signature is invalid")
	}
	return nil
}

// VerifyFederationSignature verifies an Ed25519 signature over the canonical federation payload
// JCS({action, args_hash, counterparty, idempotency_key, timestamp}).
func VerifyFederationSignature(pubKeyB64, action, counterparty, idempotencyKey, timestamp, argsHash, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	if err := verifyJCS(pub, map[string]string{
		"action":          action,
		"args_hash":       argsHash,
		"counterparty":    counterparty,
		"idempotency_key": idempotencyKey,
		"timestamp":       timestamp,
	}, sigB64); err != nil {
		return ErrUnauthenticated.Wrap("federation signature is invalid")
	}
	return nil
}

// SignFederationPayload creates a base64url Ed25519 signature over the canonical federation payload.
func SignFederationPayload(key ed25519.PrivateKey, action, counterparty, idempotencyKey, timestamp, argsHash string) (string, error) {
	return signJCS(key, map[string]string{
		"action":          action,
		"args_hash":       argsHash,
		"counterparty":    counterparty,
		"idempotency_key": idempotencyKey,
		"timestamp":       timestamp,
	})
}

// Step payloads name their `recipient` — the serving kernel's public key. Every other signed
// payload in the system identifies only its sender, so a captured request is replayable to any
// kernel that would accept it; for a step list that would let one kernel enumerate another's
// parked steps on a third kernel. Binding the recipient closes that here. The call payload has
// the same weakness, but adding a field there breaks the wire for every existing peer, which §12
// assigns to a federation-protocol version bump.

// SignStepPayload creates a base64url Ed25519 signature over the canonical step-completion payload
// JCS({counterparty, idempotency_key, input_hash, recipient, step_id, timestamp}) — a key-set
// disjoint from every other signed Juice payload (§12, §13).
func SignStepPayload(key ed25519.PrivateKey, stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash string) (string, error) {
	return signJCS(key, stepPayload(stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash))
}

// VerifyStepSignature verifies an Ed25519 signature over the canonical step-completion payload.
// recipient must be the verifying kernel's own public key.
func VerifyStepSignature(pubKeyB64, stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	if err := verifyJCS(pub, stepPayload(stepID, counterparty, recipient, idempotencyKey, timestamp, inputHash), sigB64); err != nil {
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

// SignStepListPayload creates a base64url Ed25519 signature over
// JCS({counterparty, recipient, scope, timestamp}). The fixed scope value keeps this key-set
// disjoint from every other signed payload (§12, §13).
func SignStepListPayload(key ed25519.PrivateKey, counterparty, recipient, timestamp string) (string, error) {
	return signJCS(key, stepListPayload(counterparty, recipient, timestamp))
}

// VerifyStepListSignature verifies an Ed25519 signature over the canonical step-list payload.
// recipient must be the verifying kernel's own public key.
func VerifyStepListSignature(pubKeyB64, counterparty, recipient, timestamp, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthenticated.Wrap("invalid counterparty public key")
	}
	if err := verifyJCS(pub, stepListPayload(counterparty, recipient, timestamp), sigB64); err != nil {
		return ErrUnauthenticated.Wrap("step signature is invalid")
	}
	return nil
}

func stepListPayload(counterparty, recipient, timestamp string) map[string]string {
	return map[string]string{
		"counterparty": counterparty,
		"recipient":    recipient,
		"scope":        "step_list",
		"timestamp":    timestamp,
	}
}
