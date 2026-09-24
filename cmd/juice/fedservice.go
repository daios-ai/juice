// SPDX-License-Identifier: AGPL-3.0-only

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

// keyLimiter is a per-key token-bucket rate limiter, used by every surface that admits work from
// a key it did not choose: inbound federation streams, keyed by peer public key, and the client
// API, keyed by client address. Keys are free to mint, so the map is swept: an entry unused for
// its idle window is dropped, and the set of live entries is bounded by traffic rather than by
// everyone who has ever connected (D12).
type keyLimiter struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
	rate    rate.Limit
	burst   int
}

type limiterEntry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

const limiterIdleWindow = 5 * time.Minute

// newKeyLimiter starts a limiter and its sweeper, which runs until ctx ends.
func newKeyLimiter(ctx context.Context, ratePerSec float64, burst int) *keyLimiter {
	kl := &keyLimiter{entries: map[string]*limiterEntry{}, rate: rate.Limit(ratePerSec), burst: burst}
	go kl.sweep(ctx)
	return kl
}

func (kl *keyLimiter) sweep(ctx context.Context) {
	ticker := time.NewTicker(limiterIdleWindow)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			kl.evictIdle(now.Add(-limiterIdleWindow))
		}
	}
}

// evictIdle drops the buckets of keys unheard from since cutoff. Without it the map is a slow leak
// a stranger can drive: one entry per key that ever dialed, kept for the life of the process.
func (kl *keyLimiter) evictIdle(cutoff time.Time) {
	kl.mu.Lock()
	defer kl.mu.Unlock()
	for key, e := range kl.entries {
		if e.lastSeen.Before(cutoff) {
			delete(kl.entries, key)
		}
	}
}

func (kl *keyLimiter) allow(key string) bool {
	kl.mu.Lock()
	defer kl.mu.Unlock()
	e, ok := kl.entries[key]
	if !ok {
		e = &limiterEntry{lim: rate.NewLimiter(kl.rate, kl.burst)}
		kl.entries[key] = e
	}
	e.lastSeen = time.Now()
	return e.lim.Allow()
}

// wireError is the one boundary shaping an error for a peer (§14): the code and its concise
// message cross; an internal error crosses as its class alone, so SQL and store text never
// leave the kernel.
func wireError(err error) (code, msg string) {
	code = kernel.KernelErrorCode(err)
	if code == kernel.KernelErrorCode(kernel.ErrInternal) {
		return code, "internal error"
	}
	return code, err.Error()
}

// fedError renders a typed error as the {error, code} envelope every protocol reply uses (§14), so
// an offline-verifiable rejection always carries a code and its derived status.
func fedError(err error) fed.Response {
	code, msg := wireError(err)
	b, _ := json.Marshal(map[string]string{"error": msg, "code": code})
	return fed.Response{Status: kernel.HTTPStatusFromCode(code), Body: b}
}

// fedOK marshals a handler body into a reply at the given status.
func fedOK(status int, body any) fed.Response {
	b, _ := json.Marshal(body)
	return fed.Response{Status: status, Body: b}
}

// admit is the gate every inbound stream passes: a request that claims a counterparty must be the
// connection that key authenticated, and every request is held to the per-key rate. A request that
// claims nobody — a read anyone may make — is still rated, by the key its connection proved.
func (h *fedHandlers) admit(counterparty, peerKey string, rateLimited bool) *fed.Response {
	if counterparty != "" && peerKey != "" && peerKey != counterparty {
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
		req.Timestamp, req.IdempotencyKey, req.Action, req.Signature, kernel.BuyerTerms{
			Commitment: req.Commitment, Lottery: req.Lottery, BlockchainAddress: req.BlockchainAddress, BlockchainProof: req.BlockchainProof,
		}, []byte(req.Args))
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
		status, body, err = handleFederationStepList(h.kernel, ctx, req.Counterparty, req.Timestamp, req.Signature, req.ForUserID)
	case "complete":
		input := []byte(req.Input)
		if len(input) == 0 {
			input = []byte("{}")
		}
		status, body, err = handleFederationStepComplete(h.kernel, ctx, req.Counterparty, req.Timestamp,
			req.IdempotencyKey, req.StepID, req.Signature, input, req.ForUserID, req.UserAttestation, req.UserTimestamp, req.UserSuperuser)
	default:
		return fedError(kernel.ErrInvalidInput.Wrap("unknown step request kind"))
	}
	if err != nil {
		return fedError(err)
	}
	return fedOK(status, body)
}

// ownKey is this kernel's own public key, the recipient every inbound signature is bound to.
func (h *fedHandlers) ownKey(ctx context.Context) string {
	k, _ := h.kernel.GetConfig(ctx, configKeySigningPublic)
	return k
}

// OnReveal answers the /juice/fed/settle/1 protocol (P10): the seller side of one obligation's
// draw. The connection-key check and freshness window mirror OnCall/OnStep; the kernel verifies the
// buyer's signature, recomputes the outcome from the revealed secret, and applies it idempotently.
func (h *fedHandlers) OnReveal(ctx context.Context, peerKey string, req fed.RevealRequest) fed.RevealResponse {
	if rej := h.admit(req.Counterparty, peerKey, true); rej != nil {
		return *rej
	}
	if err := checkFederationTimestamp(req.Timestamp); err != nil {
		return fedError(err)
	}
	t, err := h.kernel.HandleReveal(ctx, req.Counterparty, kernel.RevealPayload{
		Counterparty: req.Counterparty, Recipient: h.ownKey(ctx), Secret: req.Secret,
		TicketID: req.TicketID, Timestamp: req.Timestamp, TxHash: req.TxHash,
	}, req.Signature)
	if err != nil {
		return fedError(err)
	}
	body, _ := json.Marshal(t)
	return fed.Response{Status: http.StatusOK, Body: body}
}

// OnResolve answers the open /juice/fed/resolve/1 protocol (§13): resolve one action to its signed
// manifest, or one user reference to its stable id+handle — the primitive that lets a caller reach a
// remote action without prior subscription.
func (h *fedHandlers) OnResolve(ctx context.Context, peerKey string, req fed.ResolveRequest) fed.ResolveResponse {
	// Unauthenticated in the sense that anyone may ask, but not unbounded: each answer is a store
	// read and a fresh signature, so the asker is held to the same rate as a caller (D12).
	if rej := h.admit("", peerKey, true); rej != nil {
		return *rej
	}
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
		// The reply carries where this kernel is paid, with its own proof. A buyer takes on an
		// obligation the moment it calls, so it must know how to pay before it does — and one cold
		// resolve is all a first call has (P10). It rides beside the manifest rather than inside it:
		// the contract is what the action is, not where its kernel banks.
		addr, proof := h.kernel.BlockchainIdentity(ctx)
		return fedOK(http.StatusOK, kernel.ResolvedAction{Manifest: m, BlockchainAddress: addr, BlockchainProof: proof})
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
// public key, which the limiter bounds; req resumes the evidence stream and the catalogue scan.
func (h *fedHandlers) OnGossip(ctx context.Context, peerKey string, req fed.GossipRequest) (json.RawMessage, error) {
	if refused := h.admit("", peerKey, true); refused != nil {
		return refused.Body, nil
	}
	g, err := h.kernel.GetGossip(ctx, kernel.GossipRequest{Cursor: req.Cursor, CatalogCursor: req.CatalogCursor})
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

// servedOutcome answers a request this kernel has already served. The answer is the receipt it
// signed for that request and the reply its transaction recorded — both permanent — so a retry
// after a lost reply returns what the first attempt produced, for as long as the receipt exists.
// Nothing expires it: the seller's memory of a call must outlast the buyer's patience, or a retry
// would execute the work a second time (P4, U35).
func servedOutcome(k *kernel.Kernel, ctx context.Context, counterparty, key string) (*kernel.Receipt, map[string]any, bool) {
	receipt, replyJSON, err := k.ReadFederatedOutcome(ctx, counterparty, key)
	if err != nil || receipt == nil {
		return nil, nil, false
	}
	var result map[string]any
	_ = json.Unmarshal(replyJSON, &result)
	return receipt, result, true
}

// takeExecutionLock claims the right to execute one request. The row exists only while execution
// may be running: the commit that writes the receipt deletes it, and every replay afterwards is
// answered by the receipt instead. Insert-or-report is one statement, so two arrivals of one
// request race in the store and exactly one proceeds.
func takeExecutionLock(k *kernel.Kernel, ctx context.Context, key, counterpartyID, argsJSON string) (rec *kernel.IdempotencyRecord, inFlight bool, err error) {
	rec = &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     key,
		CounterpartyUserID: counterpartyID,
		ArgsJSON:           argsJSON,
		CreatedAt:          time.Now().UTC(),
	}
	held, err := k.InsertPendingIdempotencyRecord(ctx, rec)
	if err != nil {
		return nil, false, err
	}
	if held != nil {
		return nil, true, nil
	}
	return rec, false, nil
}

// handleFederationStepList returns the waiting steps whose required caller is the requesting peer
// (§10, §13). Read-only: an unknown key gets an empty list rather than a lazily provisioned account
// — provisioning is reserved for a call, which is what actually creates a billing relationship.
func handleFederationStepList(k *kernel.Kernel, ctx context.Context, cpPubKey, tsStr, sigStr, forUserID string) (int, map[string]any, error) {
	if err := checkFederationTimestamp(tsStr); err != nil {
		return 0, nil, err
	}
	self, err := k.GetConfig(ctx, configKeySigningPublic)
	if err != nil || self == "" {
		return 0, nil, kernel.ErrInvalidState.Wrap("signing key not configured")
	}
	if err := k.Network().VerifyStepListSignature(cpPubKey, cpPubKey, self, tsStr, forUserID, sigStr); err != nil {
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
	// One past the page: a page exactly full reports more only when there is more.
	steps, err := k.ListStepsAwaitingCaller(ctx, peer.ID, forUserID, maxPeerStepPage+1)
	if err != nil {
		return 0, nil, err
	}
	truncated := len(steps) > maxPeerStepPage
	if truncated {
		steps = steps[:maxPeerStepPage]
	}
	views := make([]*kernel.PeerStepView, len(steps))
	for i, s := range steps {
		action, _ := k.ReadAction(ctx, s.ActionID)
		views[i] = k.NewPeerStepView(s, action)
	}
	body := map[string]any{"steps": views}
	// A full page means more may be waiting. One honest flag, no continuation: this queue holds
	// pending cross-kernel approvals, not a corpus.
	if truncated {
		body["truncated"] = true
	}
	return http.StatusOK, body, nil
}

// handleFederationStepComplete resumes a waiting step on behalf of the requesting peer (§10, §13).
// Unlike a call, the requester parks nothing locally — the step's price was parked here at creation
// — so failures are plain typed errors: there is no remote trace awaiting a signed rejection.
func handleFederationStepComplete(k *kernel.Kernel, ctx context.Context, cpPubKey, tsStr, idempotencyKey, stepID, sigStr string, rawInput []byte, forUserID, userAttestation, userTimestamp string, userSuperuser bool) (int, map[string]any, error) {
	if err := checkFederationTimestamp(tsStr); err != nil {
		return 0, nil, err
	}
	self, err := k.GetConfig(ctx, configKeySigningPublic)
	if err != nil || self == "" {
		return 0, nil, kernel.ErrInvalidState.Wrap("signing key not configured")
	}
	if err := k.Network().VerifyStepSignature(cpPubKey, stepID, cpPubKey, self, idempotencyKey, tsStr, sha256HexBytes(rawInput), sigStr); err != nil {
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
	// Scope must match (§13): a step addressed to one principal on the peer is completed by that
	// principal, attested by its home kernel; a step addressed to the peer kernel itself is
	// completed by that kernel — its own bare signature, or a user its home kernel attests is its
	// operator. Neither scope reaches the other, and a read that fails refuses rather than skips.
	remoteID, err := k.StepRemoteRequiredCaller(ctx, stepID)
	if err != nil {
		return 0, nil, err
	}
	if forUserID == "" {
		if remoteID != nil {
			return 0, nil, kernel.ErrUnauthorized.Wrap("this step is addressed to a user of your kernel; complete it as that user")
		}
	} else {
		if userAttestation == "" || userTimestamp == "" {
			return 0, nil, kernel.ErrUnauthorized.Wrap("a completion as a user requires a home-kernel attestation")
		}
		if err := checkFederationTimestamp(userTimestamp); err != nil {
			return 0, nil, err
		}
		if err := k.Network().VerifyStepAuthSignature(cpPubKey, cpPubKey, self, forUserID, stepID, userTimestamp, userSuperuser, userAttestation); err != nil {
			return 0, nil, err
		}
		switch {
		case remoteID != nil && forUserID != *remoteID:
			return 0, nil, kernel.ErrUnauthorized.Wrap("attested user is not the step's required caller")
		case remoteID == nil && !userSuperuser:
			return 0, nil, kernel.ErrUnauthorized.Wrap("this step is addressed to your kernel; only its operator completes it")
		}
	}

	// The key is derived, not chosen (§13): recompute what this completion must present and reject a
	// mismatch. The key is already inside the signed step payload, so binding it here binds the whole
	// completion to this step and these exact input bytes.
	expectedKey := kernel.StepIdempotencyKey(self, stepID, sha256HexBytes(rawInput))
	if idempotencyKey != expectedKey {
		return 0, nil, kernel.ErrUnauthorized.Wrap("idempotency key does not match the step and input")
	}

	if receipt, result, served := servedOutcome(k, ctx, cpPubKey, idempotencyKey); served {
		return replayStepOutcome(receipt, result, stepID)
	}
	rec, inFlight, err := takeExecutionLock(k, ctx, idempotencyKey, peer.ID, string(rawInput))
	if err != nil {
		return 0, nil, err
	}
	if inFlight {
		return duplicateInFlight()
	}

	// Federated: the lock id rides into the kernel, so whichever commit finally settles this
	// completion — here, or later via the remote-dispatch retry loop — releases the lock atomically
	// with the transaction and the receipt that replaces it (§5, §13). The service layer therefore
	// disposes of the lock only in the cases where NO commit will ever happen.
	reply, err := k.CompleteStepFederated(ctx, peer.ID, stepID, rawInput, rec.ID, idempotencyKey, cpPubKey)
	if err != nil {
		// Disposition follows CompleteStep's outcome contract (§10) — never a re-read of the step's
		// status, which cannot distinguish these three cases:
		switch {
		case errors.Is(err, kernel.ErrTimeout):
			// Claimed and dispatched to a peer; the receipt may still arrive and commit, and that
			// commit now completes the record. Leave it PENDING so a replay honestly reports a
			// duplicate in flight rather than claiming an outcome that has not happened yet.
		case reply != nil:
			// A transaction committed and then failed, or the outcome waits on a call beneath it
			// (D3): either way the commit completes the record.
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

// replayStepOutcome rebuilds a completion reply from the permanent pair: the signed receipt and
// the reply its transaction recorded. Success is read off the RECEIPT, never by probing the result
// for an "error" key, which a legitimate result carrying that field would trip.
func replayStepOutcome(receipt *kernel.Receipt, result map[string]any, stepID string) (int, map[string]any, error) {
	body := map[string]any{
		"result": result, "tx_id": receipt.TxID, "trace_id": receipt.TraceID,
		"step_id": stepID, "receipt": receipt,
	}
	if receipt.Status != kernel.TxSuccess {
		// A settled FAILURE still charged the caller, so the replay carries the same ids the fresh
		// response did — otherwise a retry after a dropped connection loses the only pointer to the
		// transaction it paid for, which is the loss this branch exists to prevent.
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

// signedRejection is the one way this kernel refuses an inbound call: a signed, zero-charge
// receipt naming the request it refuses, so the buyer settles at once instead of holding its money
// against an answer that will never come. It commits nothing and holds no lock — a refusal is
// deterministic, so a retry recomputes it — and every refusal reads the same to a stranger, which
// is what keeps a catalogue it cannot see from being mapped by asking (U48, P4).
func signedRejection(k *kernel.Kernel, ctx context.Context, cpPubKey, counterpartyID, actionID string, rawArgs []byte, idempotencyKey string, cause error, refreshProxy bool) (int, map[string]any, error) {
	code, msg := wireError(cause)
	receipt, err := k.CreateSignedRejectionReceipt(counterpartyID, cpPubKey, actionID, rawArgs, idempotencyKey, msg, refreshProxy)
	if err != nil {
		return 0, nil, cause
	}
	return kernel.HTTPStatus(cause), map[string]any{"error": msg, "code": code, "receipt": receipt}, nil
}

// handleFederationCall validates the inbound federation request (counterparty, timestamp,
// signature) and executes the call. Returns (httpStatus, responseBody, err).
func handleFederationCall(k *kernel.Kernel, ctx context.Context, cpPubKey, expectedContractHash, tsStr, idempotencyKey, actionParam, sigStr string, buyer kernel.BuyerTerms, rawBody []byte) (int, map[string]any, error) {
	// The request's own hash is over the bytes as sent (P4); a receipt's is over their canonical
	// form (P5). Two fields, two rules, and a receipt is never hashed from wire bytes — Go escapes
	// `<`, `>` and `&` on the way out and canonical JSON does not, so an argument containing one
	// would hash two ways and the buyer would reject its own answer.
	argsHash := sha256HexBytes(rawBody)

	if err := checkFederationTimestamp(tsStr); err != nil {
		return 0, nil, err
	}
	// recipient is this kernel's own key: verifying with it (not the wire value) rejects a request signed
	// for a different kernel, so a captured call cannot be replayed here (§13). cpPubKey is the
	// transport-authenticated caller key (OnCall proved connection key == counterparty).
	ownKey, _ := k.GetConfig(ctx, configKeySigningPublic)
	if err := k.Network().VerifyFederationSignature(cpPubKey, actionParam, cpPubKey, ownKey, expectedContractHash, idempotencyKey, tsStr, argsHash, buyer.Commitment, buyer.Lottery, sigStr); err != nil {
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

	buyer.IdempotencyKey = idempotencyKey

	// An answer already given is given again, before anything else is read: the receipt this
	// kernel signed for this request, with the reply it recorded. A retry after a lost reply must
	// never execute the work twice, and must be answered even when the action has since been
	// deleted — which is why the outcome is looked up before the action (P4).
	if receipt, result, served := servedOutcome(k, ctx, cpPubKey, idempotencyKey); served {
		return replayStatus(result, receipt), map[string]any{"result": result, "receipt": receipt}, nil
	}

	var args map[string]any
	if err := json.Unmarshal(rawBody, &args); err != nil {
		return 0, nil, kernel.ErrInvalidInput.Wrap("invalid JSON")
	}

	// The inbound wire reference is this (the serving) kernel's stable action id (§13): a handle is
	// mutable display metadata, so naming execution by it parks a caller forever the moment it drifts.
	// Anything this kernel does not serve abroad — absent, inactive, private, delegated-auth, or an
	// import, federation being non-transitive (§8) — is refused with a signed rejection like every
	// other pre-execution refusal, and with one reason for all of them: telling a stranger which of
	// those it was would describe a catalogue it cannot see (U48). A rejection commits nothing and
	// takes no lock, so a flood of them leaves nothing behind.
	action, err := k.ReadAction(ctx, actionParam)
	if err != nil || !k.ServedAbroad(action) {
		return signedRejection(k, ctx, cpPubKey, counterparty.ID, actionParam, rawBody, idempotencyKey,
			kernel.ErrNotFound.Wrap("action not found"), false)
	}

	// If-Match precondition (§8): the caller pins the contract hash it cached. A mismatch is
	// refused before execution with a signed refresh_proxy rejection so the caller re-resolves.
	if expectedContractHash != "" {
		if cur, herr := k.CurrentContractHash(ctx, action.ID); herr == nil && cur != expectedContractHash {
			return signedRejection(k, ctx, cpPubKey, counterparty.ID, action.ID, rawBody, idempotencyKey,
				kernel.ErrInvalidState.Wrap("contract changed"), true)
		}
	}

	// The lock is taken only now, when execution may actually begin, and released by the commit
	// that writes the receipt. Whichever commit finally settles — here, the retry loop, crash
	// recovery — carries the record id, so the release is atomic with the outcome (§5, §13).
	rec, inFlight, err := takeExecutionLock(k, ctx, idempotencyKey, counterparty.ID, string(rawBody))
	if err != nil {
		return 0, nil, err
	}
	if inFlight {
		return duplicateInFlight()
	}

	reply, callErr := k.RunFederated(ctx, counterparty.ID, action, args, rec.ID, buyer)
	if callErr != nil {
		wireCode, wireMsg := wireError(callErr)
		// A committed transaction (reply carries a receipt) means the call settled — possibly
		// with charge > 0 from settled descendants. Return THAT receipt so the caller settles
		// the real charge, so the two kernels agree on what was charged rather than under-paying with 0.
		if reply != nil && reply.ReceiptID != "" {
			receipt, _ := k.GetReceiptByID(ctx, reply.ReceiptID)
			return kernel.HTTPStatus(callErr), map[string]any{"error": wireMsg, "code": wireCode, "receipt": receipt}, nil
		}
		// A parked remote dispatch has committed nothing yet and may still settle with a real
		// charge; its own settlement writes the receipt and releases the lock (§13). Signing a
		// zero-charge rejection here would answer the peer with an outcome that has not happened.
		// The same holds for an outcome deferred behind a call still running beneath it (D3).
		if errors.Is(callErr, kernel.ErrTimeout) || reply.Deferred() {
			return 0, nil, callErr
		}
		// Pre-execution rejection (no transaction committed, e.g. insufficient funds): release the
		// lock and sign a zero-charge rejection so the caller settles at once. Not a contract-hash
		// fault — re-resolving would not change the outcome — so refresh_proxy is false.
		_ = k.DeleteIdempotencyRecord(ctx, rec.ID)
		return signedRejection(k, ctx, cpPubKey, counterparty.ID, action.ID, rawBody, idempotencyKey, callErr, false)
	}

	var receipt *kernel.Receipt
	if reply.ReceiptID != "" {
		receipt, _ = k.GetReceiptByID(ctx, reply.ReceiptID)
	}
	return http.StatusOK, map[string]any{"result": reply.Result, "receipt": receipt}, nil
}
