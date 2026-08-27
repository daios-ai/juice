package main

// Inbound federation (§13): everything a peer's signed request touches on this kernel — the
// transport handlers that answer each protocol stream, and the service functions that verify,
// admit, and settle the request. One file, so a federation change lands here rather than fanning
// out across the server and the service layer.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// fedHandlers answers inbound federation protocol streams.
type fedHandlers struct {
	kernel      *kernel.Kernel
	log         *log.Logger
	callLimiter *keyLimiter // per-peer inbound call rate limit (§13; known-peer-bounded)
}

// keyLimiter is a per-key token-bucket rate limiter. Keyed by peer public key on the federation
// call path — inbound calls are permissionless, so any signed key can call (§13); the distinct-key
// set is therefore bounded by the transport's Sybil caps (per-source/per-peer/global, §13), not by
// peering. Complements those transport frame/deadline caps and the economic (prepaid-balance)
// backstop with a per-key call-rate ceiling.
type keyLimiter struct {
	mu      sync.Mutex
	entries map[string]*rate.Limiter
	rate    rate.Limit
	burst   int
}

func newKeyLimiter(ratePerSec float64, burst int) *keyLimiter {
	return &keyLimiter{entries: map[string]*rate.Limiter{}, rate: rate.Limit(ratePerSec), burst: burst}
}

func (kl *keyLimiter) allow(key string) bool {
	kl.mu.Lock()
	defer kl.mu.Unlock()
	l, ok := kl.entries[key]
	if !ok {
		l = rate.NewLimiter(kl.rate, kl.burst)
		kl.entries[key] = l
	}
	return l.Allow()
}

// fedError renders a typed error as the {error, code} envelope every protocol reply uses (§14), so
// an offline-verifiable rejection always carries a code and its derived status.
func fedError(err error) fed.Response {
	code := kernel.KernelErrorCode(err)
	b, _ := json.Marshal(map[string]string{"error": err.Error(), "code": code})
	return fed.Response{Status: kernel.HTTPStatusFromCode(code), Body: b}
}

// fedOK marshals a handler body into a reply at the given status.
func fedOK(status int, body any) fed.Response {
	b, _ := json.Marshal(body)
	return fed.Response{Status: status, Body: b}
}

// admit is the guard every signed inbound stream shares: the payload signature already
// authenticates the counterparty, but the Noise-authenticated connection key must also match, so a
// validly-signed request cannot be relayed or replayed over a connection authenticated as a
// different peer (§13). It fails open only when the transport supplied no key — the signature
// remains the authority, and the step/call payloads bind their `recipient`, so a captured request
// still cannot be replayed across kernels. rateLimited requests additionally consume a per-key
// token. A nil return means the request may proceed.
func (h *fedHandlers) admit(counterparty, peerKey string, rateLimited bool) *fed.Response {
	if peerKey != "" && peerKey != counterparty {
		r := fedError(kernel.ErrUnauthenticated.Wrap("counterparty does not match the authenticated connection"))
		return &r
	}
	if rateLimited && h.callLimiter != nil {
		limitKey := peerKey
		if limitKey == "" {
			limitKey = counterparty
		}
		if !h.callLimiter.allow(limitKey) {
			b, _ := json.Marshal(map[string]string{"error": "rate limit exceeded"})
			return &fed.Response{Status: http.StatusTooManyRequests, Body: b}
		}
	}
	return nil
}

// OnCall verifies and executes an inbound federation call, returning the settlement envelope.
func (h *fedHandlers) OnCall(ctx context.Context, peerKey string, req fed.CallRequest) fed.CallResponse {
	if rej := h.admit(req.Counterparty, peerKey, true); rej != nil {
		return *rej
	}
	status, body, err := handleFederationCall(h.kernel, ctx, req.Counterparty, req.ExpectedContractHash,
		req.Timestamp, req.IdempotencyKey, req.Action, req.Signature, []byte(req.Args))
	if err != nil {
		// No receipt to settle on → the caller treats this as pending (retry).
		return fedError(err)
	}
	return fedOK(status, body)
}

// OnStep lists or completes the waiting steps this peer is the required caller of (§10, §13).
// The connection-key check and rate limit mirror OnCall: a step completion runs a funded call here.
func (h *fedHandlers) OnStep(ctx context.Context, peerKey string, req fed.StepRequest) fed.StepResponse {
	if rej := h.admit(req.Counterparty, peerKey, true); rej != nil {
		return *rej
	}

	var status int
	var body map[string]any
	var err error
	switch req.Kind {
	case "list":
		status, body, err = handleFederationStepList(h.kernel, ctx, req.Counterparty, req.Timestamp, req.Signature)
	case "complete":
		input := []byte(req.Input)
		if len(input) == 0 {
			input = []byte("{}")
		}
		status, body, err = handleFederationStepComplete(h.kernel, ctx, req.Counterparty, req.Timestamp,
			req.IdempotencyKey, req.StepID, req.Signature, input, req.ForUserID, req.UserAttestation, req.UserTimestamp)
	default:
		return fedError(kernel.ErrInvalidInput.Wrap("unknown step request kind"))
	}
	if err != nil {
		return fedError(err)
	}
	return fedOK(status, body)
}

// OnSettle answers the /juice/fed/settle/1 residual-settlement exchange (§13): the creditor side of
// the two-party commit/reveal. The connection-key check and freshness window mirror OnCall/OnStep;
// the kernel verifies the debtor's scoped signature and applies the three-way legs idempotently.
func (h *fedHandlers) OnSettle(ctx context.Context, peerKey string, req fed.SettleRequest) fed.SettleResponse {
	if rej := h.admit(req.Counterparty, peerKey, false); rej != nil {
		return *rej
	}
	if err := checkFederationTimestamp(req.Timestamp); err != nil {
		return fedError(err)
	}
	status, body, err := h.kernel.HandleSettle(ctx, req.Counterparty, req.Kind, req.Timestamp, req.Signature,
		req.SettlementID, req.Amount, req.Nonce, []byte(req.Record))
	if err != nil {
		return fedError(err)
	}
	return fed.Response{Status: status, Body: body}
}

// OnResolve answers the open /juice/fed/resolve/1 protocol (§13): resolve one action to its signed
// manifest, or one user reference to its stable id+handle — the primitive that lets a caller reach a
// remote action without prior subscription.
func (h *fedHandlers) OnResolve(ctx context.Context, _ string, req fed.ResolveRequest) fed.ResolveResponse {
	switch req.Kind {
	case "action":
		// The owner must be one of ours: confirming the handle locally is what stops a supplied
		// "owner@third-kernel" from making us resolve on a third party's behalf (§13).
		owner, oerr := h.kernel.ReadUserByHandle(ctx, req.Owner)
		if oerr != nil || owner == nil {
			return fedError(kernel.ErrNotFound.Wrap("action not found"))
		}
		// An empty name is a request for the owner's root, which the resolver answers with that
		// owner's index action (§13); a named request resolves exactly as a local reference does.
		ref := req.Owner
		if req.Name != "" {
			ref += "/" + req.Name
		}
		a, err := h.kernel.ResolveAction(ctx, ref)
		if err != nil || a == nil {
			return fedError(kernel.ErrNotFound.Wrap("action not found"))
		}
		m, err := h.kernel.GetActionManifest(ctx, a.ID)
		if err != nil {
			return fedError(err)
		}
		return fedOK(http.StatusOK, m)
	case "user":
		id, handle, err := h.kernel.ResolvePrincipal(ctx, req.User)
		if err != nil {
			return fedError(kernel.ErrNotFound.Wrap("user not found"))
		}
		return fedOK(http.StatusOK, map[string]string{"user_id": id, "handle": handle})
	default:
		return fedError(kernel.ErrInvalidInput.Wrap("unknown resolve kind"))
	}
}

// OnGossip returns one page of the gossip document (§13). peerKey is the connection's authenticated
// public key; GetGossip uses it to report the requesting peer its credit here (counterparty_balance,
// §13 peer sync). req.Cursor resumes the evidence stream.
func (h *fedHandlers) OnGossip(ctx context.Context, peerKey string, req fed.GossipRequest) (json.RawMessage, error) {
	g, err := h.kernel.GetGossip(ctx, peerKey, req.Cursor)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(g)
	if err != nil {
		return nil, err
	}
	// Served-response telemetry (§13 diagnostics): size + payload counts, to correlate a
	// puller's read-side failure against a heavy gossip frame near the relayed allowance.
	h.log.With(ctx).Debug("gossip.served",
		"requester", peerKey, "bytes", len(b),
		"manifests", len(g.ActionManifests), "evidence", len(g.Evidence))
	return b, nil
}

// checkFederationTimestamp enforces the §13 ±5 minute freshness window on a signed request.
func checkFederationTimestamp(tsStr string) error {
	ts, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return kernel.ErrUnauthenticated.Wrap("timestamp must be RFC3339")
	}
	if diff := time.Since(ts); diff < -5*time.Minute || diff > 5*time.Minute {
		return kernel.ErrUnauthenticated.Wrap("timestamp out of range")
	}
	return nil
}

// maxPeerStepPage bounds one step-list reply. Reaching it sets `truncated` rather than silently
// dropping the tail: an operator must never read a capped page as "nothing is parked for you".
const maxPeerStepPage = 200

// beginIdempotency inserts the pending cross-kernel record for an inbound request, or reports the
// prior attempt (§13). It returns the new record, or (nil, existing) when this key was already seen —
// the two callers build their own replies from `existing`, because a completed step replay and a
// completed call replay carry different shapes. Only a uniqueness collision counts as "seen": any
// other store failure is a fault, and answering it as a replay would silently mis-serve the peer.
func beginIdempotency(k *kernel.Kernel, ctx context.Context, key, counterpartyID string) (rec, existing *kernel.IdempotencyRecord, err error) {
	now := time.Now().UTC()
	rec = &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     key,
		CounterpartyUserID: counterpartyID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	insertErr := k.InsertPendingIdempotencyRecord(ctx, rec)
	if insertErr == nil {
		return rec, nil, nil
	}
	if !errors.Is(insertErr, kernel.ErrInvalidInput) {
		return nil, nil, insertErr
	}
	prior, readErr := k.GetIdempotencyRecord(ctx, key, counterpartyID)
	if readErr != nil {
		return nil, nil, kernel.ErrInvalidState.Wrap("idempotency check failed")
	}
	return nil, prior, nil
}

// handleFederationStepList returns the waiting steps whose required caller is the requesting peer
// (§10, §13). Read-only: an unknown key gets an empty list rather than a lazily provisioned account
// — provisioning is reserved for a call, which is what actually creates a billing relationship.
func handleFederationStepList(k *kernel.Kernel, ctx context.Context, cpPubKey, tsStr, sigStr string) (int, map[string]any, error) {
	if err := checkFederationTimestamp(tsStr); err != nil {
		return 0, nil, err
	}
	self, err := k.GetConfig(ctx, configKeySigningPublic)
	if err != nil || self == "" {
		return 0, nil, kernel.ErrInvalidState.Wrap("signing key not configured")
	}
	if err := kernel.VerifyStepListSignature(cpPubKey, cpPubKey, self, tsStr, sigStr); err != nil {
		return 0, nil, err
	}
	// A store failure must not read as "nothing is parked for you" — that is precisely the
	// conclusion which leaves funds stranded. Only a genuinely absent key gets the empty list.
	peer, err := k.ReadAccountByKernelKey(ctx, cpPubKey)
	if err != nil && !errors.Is(err, kernel.ErrNotFound) {
		return 0, nil, err
	}
	if peer == nil || peer.KernelPublicKey == "" {
		return http.StatusOK, map[string]any{"steps": []*stepWithAction{}}, nil
	}
	// Scoped in SQL, oldest first: ListSteps' predicate also matches every step inside a process
	// this peer owns (its own inbound calls), which would crowd the completable ones out of the
	// page. A suspended peer is refused by requireActiveUser inside the kernel call.
	steps, err := k.ListStepsAwaitingCaller(ctx, peer.ID, maxPeerStepPage)
	if err != nil {
		return 0, nil, err
	}
	views := make([]*kernel.PeerStepView, len(steps))
	for i, s := range steps {
		action, _ := k.ReadAction(ctx, s.ActionID)
		views[i] = k.NewPeerStepView(s, action)
	}
	body := map[string]any{"steps": views}
	// A full page means more may be waiting. One honest flag, no continuation: this queue holds
	// pending cross-kernel approvals, not a corpus.
	if len(views) == maxPeerStepPage {
		body["truncated"] = true
	}
	return http.StatusOK, body, nil
}

// handleFederationStepComplete resumes a waiting step on behalf of the requesting peer (§10, §13).
// Unlike a call, the requester parks nothing locally — the step's price was parked here at creation
// — so failures are plain typed errors: there is no remote trace awaiting a signed rejection.
func handleFederationStepComplete(k *kernel.Kernel, ctx context.Context, cpPubKey, tsStr, idempotencyKey, stepID, sigStr string, rawInput []byte, forUserID, userAttestation, userTimestamp string) (int, map[string]any, error) {
	if err := checkFederationTimestamp(tsStr); err != nil {
		return 0, nil, err
	}
	self, err := k.GetConfig(ctx, configKeySigningPublic)
	if err != nil || self == "" {
		return 0, nil, kernel.ErrInvalidState.Wrap("signing key not configured")
	}
	if err := kernel.VerifyStepSignature(cpPubKey, stepID, cpPubKey, self, idempotencyKey, tsStr, sha256HexBytes(rawInput), sigStr); err != nil {
		return 0, nil, err
	}
	// A stranger can hold no step here: CreateStep resolves required_caller to an existing user,
	// so an unknown key is necessarily not the required caller of anything.
	peer, err := k.ReadAccountByKernelKey(ctx, cpPubKey)
	if err != nil && !errors.Is(err, kernel.ErrNotFound) {
		return 0, nil, err
	}
	if peer == nil || peer.KernelPublicKey == "" {
		return 0, nil, kernel.ErrUnauthorized.Wrap("unknown peer")
	}

	// If the step is addressed to a specific remote user (§13), require and verify a home-kernel
	// step_auth attestation naming that user. A missing attestation is a pre-upgrade home kernel,
	// reported as a typed unauthorized so the requester can surface "upgrade required" rather than a
	// generic failure. A kernel-level step (no remote id) keeps today's wire/behavior unchanged.
	if remoteID, rerr := k.StepRemoteRequiredCaller(ctx, stepID); rerr == nil && remoteID != nil {
		if forUserID == "" || userAttestation == "" || userTimestamp == "" {
			return 0, nil, kernel.ErrUnauthorized.Wrap("this step requires a home-kernel user attestation (upgrade required)")
		}
		if err := checkFederationTimestamp(userTimestamp); err != nil {
			return 0, nil, err
		}
		if err := kernel.VerifyStepAuthSignature(cpPubKey, cpPubKey, self, forUserID, stepID, userTimestamp, userAttestation); err != nil {
			return 0, nil, err
		}
		if forUserID != *remoteID {
			return 0, nil, kernel.ErrUnauthorized.Wrap("attested user is not the step's required caller")
		}
	}

	// The key is derived, not chosen (§13): recompute what this completion must present and reject a
	// mismatch. The key is already inside the signed step payload, so binding it here binds the whole
	// completion to this step and these exact input bytes.
	expectedKey := kernel.StepIdempotencyKey(self, stepID, sha256HexBytes(rawInput))
	if idempotencyKey != expectedKey {
		return 0, nil, kernel.ErrUnauthorized.Wrap("idempotency key does not match the step and input")
	}

	rec, existing, err := beginIdempotency(k, ctx, idempotencyKey, peer.ID)
	if err != nil {
		return 0, nil, err
	}
	if existing != nil {
		if existing.Status != "complete" {
			return duplicateInFlight()
		}
		return replayStepRecord(existing, stepID)
	}

	// Federated: the record id rides into the kernel, so whichever commit finally settles this
	// completion — here, or later via the remote-dispatch retry loop, the max-age bound, or a
	// forced closure — completes the record atomically with the transaction (§5, §13). The service
	// layer therefore disposes of the record only in the cases where NO commit will ever happen.
	reply, err := k.CompleteStepFederated(ctx, peer.ID, stepID, rawInput, rec.ID)
	if err != nil {
		// Disposition follows CompleteStep's outcome contract (§10) — never a re-read of the step's
		// status, which cannot distinguish these three cases:
		switch {
		case errors.Is(err, kernel.ErrTimeout):
			// Claimed and dispatched to a peer; the receipt may still arrive and commit, and that
			// commit now completes the record. Leave it PENDING so a replay honestly reports a
			// duplicate in flight rather than claiming an outcome that has not happened yet.
		case reply != nil:
			// A transaction committed and then failed; the commit already completed the record.
		default:
			// Nothing settled and the step is waiting again: no commit will ever complete this
			// record, so drop it — otherwise a corrected retry is locked out by a key that
			// produced no result.
			_ = k.DeleteIdempotencyRecord(ctx, rec.ID)
		}
		return 0, nil, err
	}
	body := map[string]any{"result": reply.Result, "tx_id": reply.TxID, "trace_id": reply.TraceID, "step_id": stepID}
	if reply.ReceiptID != "" {
		receipt, _ := k.GetReceiptByID(ctx, reply.ReceiptID)
		body["receipt"] = receipt
	}
	return http.StatusOK, body, nil
}

// replayStepRecord rebuilds a completion reply from a completed idempotency record. The kernel
// stores the two halves the same way for every commit path — result_json is the bare action result
// (or an {error,code} body), receipt_json the signed receipt — so success is discriminated on the
// RECEIPT's status rather than by probing the result for an "error" key, which a legitimate result
// carrying its own "error" field would trip.
func replayStepRecord(rec *kernel.IdempotencyRecord, stepID string) (int, map[string]any, error) {
	var result map[string]any
	_ = json.Unmarshal([]byte(rec.ResultJSON), &result)
	var receipt *kernel.Receipt
	if rec.ReceiptJSON != "" {
		_ = json.Unmarshal([]byte(rec.ReceiptJSON), &receipt)
	}
	if receipt == nil {
		// No transaction committed (a pre-execution rejection): there is nothing to point at.
		return replayStatus(result, nil), result, nil
	}
	body := map[string]any{
		"result": result, "tx_id": receipt.TxID, "trace_id": receipt.TraceID,
		"step_id": stepID, "receipt": receipt,
	}
	if receipt.Status != kernel.TxSuccess {
		// A settled FAILURE still charged the caller, so the replay carries the same ids the fresh
		// response did — otherwise a retry after a dropped connection loses the only pointer to the
		// transaction it paid for, which is the loss withSettlementMeta exists to prevent.
		body["error"], body["code"] = result["error"], result["code"]
		body["meta"] = map[string]string{
			"step_id": stepID, "tx_id": receipt.TxID,
			"trace_id": receipt.TraceID, "receipt_id": receipt.ID,
		}
	}
	return replayStatus(result, receipt), body, nil
}

// replayStatus is the HTTP status a replayed idempotency record must carry. It reads the signed
// RECEIPT's status, not the result body: a settled failure must never replay as success, and
// probing the result for an "error" key misclassifies a successful action whose own output happens
// to carry that field (e.g. a validator returning {"error": null}). The result body is consulted
// only for records with no receipt — the pre-execution rejections, which never committed.
func replayStatus(result map[string]any, receipt *kernel.Receipt) int {
	if receipt != nil {
		if receipt.Status == kernel.TxSuccess {
			return http.StatusOK
		}
		code, _ := result["code"].(string)
		return kernel.HTTPStatusFromCode(code)
	}
	if _, isErr := result["error"]; !isErr {
		return http.StatusOK
	}
	code, _ := result["code"].(string)
	return kernel.HTTPStatusFromCode(code)
}

// duplicateInFlight is the §13 reply for a replay that arrives while the first attempt is still
// running. It carries a code so the requesting kernel re-raises a typed error: without one,
// ErrorFromCode("") degrades it to execution_failed and an operator reads a transient duplicate
// as a hard failure and stops retrying.
func duplicateInFlight() (int, map[string]any, error) {
	return http.StatusConflict, map[string]any{
		"error": "duplicate in flight",
		"code":  kernel.ErrInvalidState.Code,
	}, nil
}

// settleIdempotencyWithReceipt marks a record complete, falling back to deleting it if that write
// fails. A record stuck pending answers every retry with 409 "duplicate in flight" forever, with
// the money already spent and the result unreachable; deleting it instead lets a retry through to
// an honest typed error. Neither is good, but only one is a dead end.
func settleIdempotencyWithReceipt(k *kernel.Kernel, ctx context.Context, recID, resultJSON, receiptJSON string) {
	if err := k.CompleteIdempotencyRecordIfPending(ctx, recID, resultJSON, receiptJSON); err != nil {
		_ = k.DeleteIdempotencyRecord(ctx, recID)
	}
}

// handleFederationCall validates the inbound federation request (counterparty, timestamp,
// signature) and executes the call. Returns (httpStatus, responseBody, err).
func handleFederationCall(k *kernel.Kernel, ctx context.Context, cpPubKey, expectedContractHash, tsStr, idempotencyKey, actionParam, sigStr string, rawBody []byte) (int, map[string]any, error) {
	argsHash := sha256HexBytes(rawBody)

	if err := checkFederationTimestamp(tsStr); err != nil {
		return 0, nil, err
	}
	// recipient is this kernel's own key: verifying with it (not the wire value) rejects a request signed
	// for a different kernel, so a captured call cannot be replayed here (§13). cpPubKey is the
	// transport-authenticated caller key (OnCall proved connection key == counterparty).
	ownKey, _ := k.GetConfig(ctx, configKeySigningPublic)
	if err := kernel.VerifyFederationSignature(cpPubKey, actionParam, cpPubKey, ownKey, expectedContractHash, idempotencyKey, tsStr, argsHash, sigStr); err != nil {
		return 0, nil, err
	}
	// Resolve or lazily provision the caller's billing account (§13, handshake-free): a
	// signature-valid caller with no account here gets a zero-balance one, so a price-0 call
	// succeeds and a priced call hits the normal insufficient-funds rejection the provider clears
	// with a deposit. A suspended counterparty needs no gate here — RunFederated rejects it via
	// requireActiveUser and the pre-execution branch signs a zero-charge rejection receipt.
	// No petname is bound here: a stranger calling us is not our act of naming (§13 lifecycle), so
	// an inbound call can never seed a local name from a self-asserted nickname.
	counterparty, err := k.ReadAccountByKernelKey(ctx, cpPubKey)
	if err != nil || counterparty == nil {
		if counterparty, err = k.EnsureKernelAccount(ctx, cpPubKey); err != nil {
			return 0, nil, err
		}
	}

	// The inbound wire reference is this (the serving) kernel's stable action id (§13): a handle is
	// mutable display metadata, so naming execution by it parks a caller forever the moment it drifts.
	// An empty or unknown id is simply an unknown action.
	action, err := k.ReadAction(ctx, actionParam)
	if err != nil || action == nil {
		return 0, nil, kernel.ErrNotFound.Wrapf("action %s not found", actionParam)
	}
	// A proxy is never re-served: federation is non-transitive (§8), and resolving by id would
	// otherwise reach the cache row that the old handle lookup could not name.
	if action.Kind == kernel.KindRemoteProxy {
		return 0, nil, kernel.ErrNotFound.Wrapf("action %s not found", actionParam)
	}

	var args map[string]any
	if err := json.Unmarshal(rawBody, &args); err != nil {
		return 0, nil, kernel.ErrInvalidInput.Wrap("invalid JSON")
	}
	// A known-but-non-executable action (inactive, non-public, suspended owner) is NOT rejected
	// here: letting the call flow into RunFederated makes CanCall fail before any transaction, and
	// the pre-execution branch below signs a zero-charge rejection receipt carrying action.ID — so
	// the caller settles immediately instead of pinning funds until the 24h pending bound (§13).
	// Only a genuinely absent/unverifiable action stays a plain error (its ID can't match the
	// caller's stored RemoteActionID, so a receipt there would just re-pin the caller).

	rec, existing, err := beginIdempotency(k, ctx, idempotencyKey, counterparty.ID)
	if err != nil {
		return 0, nil, err
	}
	if existing != nil {
		if existing.Status != "complete" {
			return duplicateInFlight()
		}
		var result map[string]any
		_ = json.Unmarshal([]byte(existing.ResultJSON), &result)
		var receipt *kernel.Receipt
		if existing.ReceiptJSON != "" {
			_ = json.Unmarshal([]byte(existing.ReceiptJSON), &receipt)
		}
		return replayStatus(result, receipt), map[string]any{"result": result, "receipt": receipt}, nil
	}

	// If-Match precondition (§8): the caller pins the contract hash it cached. If the action is
	// servable and its current contract differs, refuse before execution with a signed refresh_proxy
	// rejection so the caller re-resolves. A non-servable action (inactive/non-public) yields an error
	// here and falls through to RunFederated, which signs a plain (non-refresh) rejection — re-resolving
	// a withdrawn action would not help. Placed after the idempotency insert so a replay is idempotent.
	if expectedContractHash != "" {
		if cur, herr := k.CurrentContractHash(ctx, action.ID); herr == nil && cur != expectedContractHash {
			code := kernel.KernelErrorCode(kernel.ErrInvalidState)
			errJSON, _ := json.Marshal(map[string]string{"error": "contract changed", "code": code})
			if receipt, signErr := k.CreateSignedRejectionReceipt(counterparty.ID, action.ID, argsHash, idempotencyKey, "contract changed", true); signErr == nil {
				receiptJSON, _ := json.Marshal(receipt)
				settleIdempotencyWithReceipt(k, ctx, rec.ID, string(errJSON), string(receiptJSON))
				return kernel.HTTPStatusFromCode(code), map[string]any{"error": "contract changed", "code": code, "receipt": receipt}, nil
			}
			_ = k.DeleteIdempotencyRecord(ctx, rec.ID)
			return 0, nil, kernel.ErrInvalidState.Wrap("contract changed")
		}
	}

	reply, callErr := k.RunFederated(ctx, counterparty.ID, action.OwnerUserID, action.Name, args, rec.ID)
	if callErr != nil {
		errJSON, _ := json.Marshal(map[string]string{
			"error": callErr.Error(),
			"code":  kernel.KernelErrorCode(callErr),
		})
		// A committed transaction (reply carries a receipt) means the call settled — possibly
		// with charge > 0 from settled descendants. Return THAT receipt so the caller settles
		// the real charge, preserving bilateral conservation rather than under-paying with 0.
		if reply != nil && reply.ReceiptID != "" {
			receipt, _ := k.GetReceiptByID(ctx, reply.ReceiptID)
			receiptJSON, _ := json.Marshal(receipt)
			settleIdempotencyWithReceipt(k, ctx, rec.ID, string(errJSON), string(receiptJSON))
			return kernel.HTTPStatus(callErr), map[string]any{"error": callErr.Error(), "code": kernel.KernelErrorCode(callErr), "receipt": receipt}, nil
		}
		// A parked remote dispatch has committed nothing yet and may still settle with a real
		// charge; its own settlement completes the record (§13). Signing a zero-charge rejection
		// here would answer the peer with an outcome that has not happened.
		if errors.Is(callErr, kernel.ErrTimeout) {
			return 0, nil, callErr
		}
		// Pre-execution rejection (no transaction committed, e.g. insufficient funds): sign a
		// zero-charge rejection receipt so the caller can settle locally without leaving the
		// trace pending. Status and code derive from the error — HTTPStatus maps both
		// ErrInsufficientFunds and ErrPeerUnfunded to 402, so its settleRemoteCall attributes them
		// to the operator (settle/deposit), never to the caller's own funds (§13).
		msg, code := callErr.Error(), kernel.KernelErrorCode(callErr)
		// A pre-execution rejection here (non-executable action, suspended caller, bad input, or funding)
		// is not a contract-hash fault — re-resolving would not change the outcome — so refresh_proxy is
		// false. Only the If-Match mismatch above sets it (§13).
		if receipt, signErr := k.CreateSignedRejectionReceipt(counterparty.ID, action.ID, argsHash, idempotencyKey, msg, false); signErr == nil {
			receiptJSON, _ := json.Marshal(receipt)
			settleIdempotencyWithReceipt(k, ctx, rec.ID, string(errJSON), string(receiptJSON))
			return kernel.HTTPStatus(callErr), map[string]any{"error": msg, "code": code, "receipt": receipt}, nil
		}
		_ = k.DeleteIdempotencyRecord(ctx, rec.ID)
		return 0, nil, callErr
	}

	var receipt *kernel.Receipt
	if reply.ReceiptID != "" {
		receipt, _ = k.GetReceiptByID(ctx, reply.ReceiptID)
	}
	return http.StatusOK, map[string]any{"result": reply.Result, "receipt": receipt}, nil
}
