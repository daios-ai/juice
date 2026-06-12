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
	"net/url"
	"strings"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
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
//   - Charge: tx.net == receipt.gross (local charge paid to proxy == what remote reported)
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

	// 7. Args hash.
	if h, hashErr := jcsHashStr(string(tx.ArgsJSON)); hashErr == nil {
		checks.ArgsHash = r.ArgsHash == h
	}

	// 8. Reply hash (only meaningful on success).
	if tx.Status == TxSuccess {
		if h, hashErr := jcsHashStr(string(tx.ReplyJSON)); hashErr == nil {
			checks.ReplyHash = r.ReplyHash == h
		}
	} else {
		checks.ReplyHash = true // not applicable on failure
	}

	valid := checks.ReceiptHash && checks.Signature && checks.ActionID &&
		checks.Status && checks.Charge && checks.SettlementArith &&
		checks.ArgsHash && checks.ReplyHash

	return &ReceiptVerification{
		TransactionID:         txID,
		Valid:                 valid,
		RemoteKernelHandle:    owner.Handle,
		RemoteKernelPublicKey: owner.PublicKey,
		Checks:                checks,
		Receipt:               &r,
	}, nil
}

// verifyRemoteReceiptSignature returns nil if the receipt's Ed25519 signature is valid
// against pubKeyB64. When pubKeyB64 is empty the check is skipped (no key configured).
func verifyRemoteReceiptSignature(r *Receipt, pubKeyB64 string) error {
	if pubKeyB64 == "" {
		return nil
	}
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return err
	}
	cp := *r
	cp.Signature = ""
	return verifyJCS(pub, cp, r.Signature)
}

// parseAndVerifyRemoteReceipt parses receiptJSON and verifies the Ed25519 signature.
// Returns ErrTimeout (keep-trace-open) on absent, unparseable, or invalidly signed receipts.
// Settlement must only proceed when this function returns without error.
func parseAndVerifyRemoteReceipt(receiptJSON, pubKeyB64 string) (*Receipt, error) {
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
	return &r, nil
}

// settleRemoteCall settles a remote-proxy call after ExecuteFederation returns.
// If the receipt is absent or has an invalid signature, the trace stays open for retry (ErrTimeout).
// Otherwise it commits CommitRemoteSettlement with the correct charge/duty/refund split.
func (k *Kernel) settleRemoteCall(ctx context.Context, logger *log.Logger, action *Action, ktx *Transaction, trace *Trace, callerWalletID, callerWalletKind string, req CallRequest, target *User, mp int64, fr FederationResult, latency float64) (*CallReply, error) {
	// A missing, unparseable, or unsigned receipt keeps the trace open for retry.
	rp, err := parseAndVerifyRemoteReceipt(fr.ReceiptJSON, target.PublicKey)
	if err != nil {
		return nil, err
	}
	r := *rp

	// Clamp remote charge to mp (protection against overcharging).
	charge := r.Charge
	if charge < 0 {
		charge = 0
	}
	if charge > mp {
		charge = mp
	}
	var duty int64
	if r.Status == TxSuccess {
		duty = ceilDiv(charge*k.cfg.ImportBPS, 10000)
	}

	if r.Status == TxSuccess && fr.Result != nil {
		replyJSON, _ := json.Marshal(fr.Result)
		ktx.ReplyJSON = json.RawMessage(replyJSON)
	}
	ktx.Status = r.Status
	ktx.Net = charge
	ktx.Fee = duty
	ktx.Reason = r.Reason
	ktx.RemoteReceiptHash = sha256Hex(fr.ReceiptJSON)
	ktx.RemoteReceiptJSON = fr.ReceiptJSON

	stats := k.computeStats(ctx, action.ID, ktx, latency)
	localReceipt, receiptErr := k.buildReceipt(ktx, charge)
	if receiptErr != nil {
		return nil, ErrInternal.Wrap("could not build receipt")
	}
	if err := k.store.CommitRemoteSettlement(ctx, ktx, localReceipt, trace.ID, callerWalletID, callerWalletKind, target.ID, k.cfg.FeeRecipientID, charge, duty, stats, req.IdempotencyRecordID, req.StepID); err != nil {
		return nil, ErrInternal.Wrap("could not commit remote settlement")
	}

	logger.Info("remote.settled", "action", action.Name, "status", r.Status, "charge", charge, "duty", duty)

	if r.Status == TxSuccess {
		return &CallReply{Result: fr.Result, TxID: ktx.ID, TraceID: trace.ID, ReceiptID: localReceipt.ID}, nil
	}
	reason := ktx.Reason
	if reason == "" {
		reason = "remote call failed"
	}
	return nil, ErrExecutionFailed.Wrap(reason)
}

// RetryPendingRemoteDispatches retries all in-flight remote proxy traces that have an
// idempotency key but no settled transaction. Called once at startup (after Recover) and
// periodically by the server ticker. Errors for individual traces are logged and skipped.
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

// retryRemoteTrace re-issues one pending remote dispatch and settles it if a receipt arrives.
func (k *Kernel) retryRemoteTrace(ctx context.Context, logger *log.Logger, trace *Trace) error {
	if trace.IdempotencyKey == nil || trace.DispatchJSON == nil {
		return nil
	}
	var dispatch struct {
		Args        map[string]any `json:"args"`
		StepID      string         `json:"step_id"`
		RemotePrice int64          `json:"remote_price"`
	}
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
	fr, _ := fe.ExecuteFederation(ctx, action.Source, *trace.IdempotencyKey, dispatch.Args)
	if fr.ReceiptJSON == "" {
		// Still pending.
		return nil
	}
	mp := dispatch.RemotePrice
	if mp == 0 {
		// Fallback: derive from action.Price and ImportBPS.
		mp = action.Price * 10000 / (10000 + k.cfg.ImportBPS)
	}
	maxDuty := ceilDiv(mp*k.cfg.ImportBPS, 10000)
	q := mp + maxDuty

	callerWalletKind := CallerProcess
	callerWalletID := process.ID
	if dispatch.StepID != "" {
		callerWalletKind = CallerStep
		callerWalletID = ""
	} else if trace.ParentTraceID != nil {
		callerWalletKind = CallerTrace
		callerWalletID = *trace.ParentTraceID
	}

	now := time.Now().UTC()
	ktx := &Transaction{
		ID:             uuid.New().String(),
		ProcessID:      trace.ProcessID,
		TraceID:        trace.ID,
		ParentTraceID:  func() string {
			if trace.ParentTraceID != nil {
				return *trace.ParentTraceID
			}
			return ""
		}(),
		OwnerUserID:    process.OwnerUserID,
		CallerUserID:   trace.CallerUserID,
		TargetUserID:   trace.ActionOwnerID,
		ActionID:       trace.ActionID,
		ActionName:     action.Name,
		RemoteActionID: action.RemoteActionID,
		Status:         TxFailure,
		Gross:          q,
		StartedAt:      trace.CreatedAt,
		EndedAt:        now,
	}
	argsJSON, _ := json.Marshal(dispatch.Args)
	ktx.ArgsJSON = json.RawMessage(argsJSON)

	req := CallRequest{ProcessID: trace.ProcessID, StepID: dispatch.StepID}
	_, err = k.settleRemoteCall(ctx, logger, action, ktx, trace, callerWalletID, callerWalletKind, req, target, mp, fr, 0)
	if errors.Is(err, ErrTimeout) {
		return nil // still pending (no receipt or invalid signature); retrier will try again
	}
	return err
}

// SignFederation signs a federation payload with the platform key and returns
// (signature, timestamp). Returns an error if the signing key is not configured.
func (k *Kernel) SignFederation(action, counterparty, idempotencyKey, argsHash string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignFederationPayload(k.cfg.SigningKey, action, counterparty, idempotencyKey, ts, argsHash)
	return
}

// SignPeerRequestNow signs a peer friend request with the platform key and returns
// (signature, timestamp). Returns an error if the signing key is not configured.
func (k *Kernel) SignPeerRequestNow(handle, publicKey, baseURL string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignPeerRequest(k.cfg.SigningKey, handle, publicKey, baseURL, ts)
	return
}

// ---- Peer / friendship operations ----

// CreateOrUpdateProxyPeer creates or updates a local user record representing a remote kernel peer.
// Used both by the friendship acceptance path and by federation admins.
func (k *Kernel) CreateOrUpdateProxyPeer(ctx context.Context, handle, publicKey, baseURL string) (*User, error) {
	if publicKey == "" || baseURL == "" {
		return nil, ErrInvalidInput.Wrap("handle, public_key, and base_url are required")
	}
	if err := validateHandle(handle); err != nil {
		return nil, err
	}
	if _, err := decodeRemotePublicKey(publicKey); err != nil {
		return nil, err
	}
	if err := validateRemoteBaseURL(baseURL); err != nil {
		return nil, err
	}
	// Reject if the handle or base URL is already claimed by a different public key.
	if byHandle, err := k.store.ReadUserByHandle(ctx, handle); err == nil && byHandle != nil && byHandle.PublicKey != publicKey {
		return nil, ErrInvalidInput.Wrap("handle already registered with a different public key")
	}
	if byURL, err := k.store.ReadRemoteKernelByBaseURL(ctx, baseURL); err == nil && byURL != nil && byURL.PublicKey != publicKey {
		return nil, ErrInvalidInput.Wrap("base URL already registered with a different public key")
	}
	// Same identity — update base URL and propagate to owned proxy actions if it changed.
	existing, err := k.store.ReadUserByPublicKey(ctx, publicKey)
	if err == nil && existing != nil {
		oldBase := existing.RemoteBaseURL
		if err := k.store.UpdateRemoteBaseURL(ctx, existing.ID, baseURL); err != nil {
			return nil, err
		}
		existing.RemoteBaseURL = baseURL
		if oldBase != baseURL {
			if err := k.store.UpdateRemoteProxySourceURLs(ctx, existing.ID, oldBase, baseURL); err != nil {
				return nil, err
			}
		}
		return existing, nil
	}
	now := time.Now().UTC()
	u := &User{
		ID:            uuid.New().String(),
		Handle:        handle,
		PublicKey:     publicKey,
		RemoteBaseURL: baseURL,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := k.store.CreateProxyUser(ctx, u); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("peer.created", "handle", handle, "base_url", baseURL)
	return u, nil
}

// AddPeer requires superuser and creates/updates a proxy peer record.
func (k *Kernel) AddPeer(ctx context.Context, subjectID, handle, publicKey, baseURL string) (*User, error) {
	if err := k.requireSuperuser(ctx, subjectID); err != nil {
		return nil, err
	}
	return k.CreateOrUpdateProxyPeer(ctx, handle, publicKey, baseURL)
}

// ListPeers returns all remote kernel peers (proxy users with a RemoteBaseURL).
func (k *Kernel) ListPeers(ctx context.Context) ([]*User, error) {
	all, err := k.store.ListUsers(ctx, 1000, 0)
	if err != nil {
		return nil, err
	}
	var peers []*User
	for _, u := range all {
		if u.RemoteBaseURL != "" {
			peers = append(peers, u)
		}
	}
	return peers, nil
}

// DenyPeer atomically denies a peer: sets denied_at, deactivates all their proxy actions,
// and cancels+refunds any waiting steps addressed to them as caller.
func (k *Kernel) DenyPeer(ctx context.Context, subjectID, handle string) error {
	if err := k.requireSuperuser(ctx, subjectID); err != nil {
		return err
	}
	u, err := k.store.ReadUserByHandle(ctx, handle)
	if err != nil {
		return ErrNotFound.Wrapf("peer %q not found", handle)
	}
	return k.store.DenyPeerCascade(ctx, u.ID)
}

// UndenyPeer clears denied_at on the proxy user for the given handle.
// Proxy actions are NOT automatically reactivated — use remote import to re-enable them.
func (k *Kernel) UndenyPeer(ctx context.Context, subjectID, handle string) error {
	if err := k.requireSuperuser(ctx, subjectID); err != nil {
		return err
	}
	u, err := k.store.ReadUserByHandle(ctx, handle)
	if err != nil {
		return ErrNotFound.Wrapf("peer %q not found", handle)
	}
	return k.store.UndenyUser(ctx, u.ID)
}

// ListDiscoveredKernels returns all kernels learned via gossip accumulation.
func (k *Kernel) ListDiscoveredKernels(ctx context.Context) ([]*DiscoveredKernel, error) {
	return k.store.ListDiscoveredKernels(ctx)
}

// GetGossip returns this kernel's gossip payload: identity, public active actions, and peer list.
func (k *Kernel) GetGossip(ctx context.Context) (*GossipResponse, error) {
	var pubKeyB64 string
	if len(k.cfg.SigningKey) == ed25519.PrivateKeySize {
		pub := k.cfg.SigningKey.Public().(ed25519.PublicKey)
		pubKeyB64 = base64.RawURLEncoding.EncodeToString(pub)
	}
	handle, _ := k.store.GetConfig(ctx, "kernel_handle")
	baseURL, _ := k.store.GetConfig(ctx, "kernel_base_url")

	actions, err := k.store.ListPublicActions(ctx, 100, 0)
	if err != nil {
		return nil, err
	}
	var gossipActions []GossipAction
	for _, a := range actions {
		if !a.Active {
			continue
		}
		stats, _ := k.store.ReadStats(ctx, a.ID)
		ga := GossipAction{
			ActionID:    a.ID,
			Name:        a.Name,
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
		if p.DeniedAt != nil {
			continue
		}
		peerStats, _ := k.store.ListStatsByOwner(ctx, p.ID)
		if len(peerStats) == 0 {
			continue // not transacted; endorsement is earned by trade, not by friending
		}
		var fActions []GossipAction
		for _, s := range peerStats {
			act, err := k.store.ReadAction(ctx, s.ActionID)
			if err != nil || act == nil {
				continue
			}
			fActions = append(fActions, GossipAction{
				ActionID:    s.ActionID,
				Name:        act.Name,
				Description: act.Description,
				Price:       act.Price,
				Uses:        s.Uses,
				Rating:      s.RatingEstimate,
			})
		}
		friendViews = append(friendViews, GossipFriendView{
			Handle:    p.Handle,
			BaseURL:   p.RemoteBaseURL,
			PublicKey: p.PublicKey,
			Actions:   fActions,
		})
	}

	return &GossipResponse{
		PublicKey: pubKeyB64,
		Handle:    handle,
		BaseURL:   baseURL,
		Actions:   gossipActions,
		Friends:   friendViews,
	}, nil
}

// CreateSignedRejectionReceipt produces a signed Receipt (status=failure, gross=0) for a
// denied inbound federation call. No transaction is created; the receipt is signed with
// the kernel's Ed25519 key so the caller can verify the rejection was authentic.
func (k *Kernel) CreateSignedRejectionReceipt(counterpartyID, actionParam, argsHash, idempotencyKey string) (*Receipt, error) {
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
		Reason:       "denied",
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
	statsJSON, _ := json.Marshal(gossip.Actions)
	now := time.Now().UTC()
	if err := k.store.CreateOrUpdateDiscoveredKernel(ctx, &DiscoveredKernel{
		PublicKey:    gossip.PublicKey,
		IntroducedBy: introducerPublicKey,
		Handle:       gossip.Handle,
		BaseURL:      gossip.BaseURL,
		StatsJSON:    json.RawMessage(statsJSON),
		FirstSeen:    now,
		UpdatedAt:    now,
	}); err != nil {
		return err
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

func validateRemoteBaseURL(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return ErrInvalidInput.Wrap("remote_base_url must be an absolute URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidInput.Wrap("remote_base_url must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ErrInvalidInput.Wrap("remote_base_url must not include userinfo, query, or fragment")
	}
	return nil
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
	if remoteUser.RemoteBaseURL == "" {
		return nil, ErrInvalidInput.Wrap("user is not a remote kernel")
	}
	if m.ActionID == "" {
		return nil, ErrInvalidInput.Wrap("manifest missing action_id")
	}
	if remoteUser.PublicKey == "" {
		return nil, ErrInvalidInput.Wrap("remote kernel has no public key")
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
	// counterparty is this kernel's base64url Ed25519 public key so the remote can
	// look it up by key (handle-based lookup would require knowing what handle the
	// remote assigned to us, which we don't have without a round-trip).
	localCounterparty := ""
	if len(k.cfg.SigningKey) == ed25519.PrivateKeySize {
		pub := k.cfg.SigningKey.Public().(ed25519.PublicKey)
		localCounterparty = base64.RawURLEncoding.EncodeToString(pub)
	}
	source := strings.TrimRight(remoteUser.RemoteBaseURL, "/") +
		"/v1/federation/call?action=" + url.QueryEscape(m.OwnerHandle+"/"+m.Name) +
		"&counterparty=" + url.QueryEscape(localCounterparty)

	existingByKey := map[string]*Action{}
	if existing, err := k.store.ReadActionByOwnerRemoteID(ctx, remoteUserID, m.ActionID); err == nil {
		existingByKey[m.ActionID] = existing
	}

	contentHash := remoteManifestHash(m)
	name := m.Name
	// Proxy price = mp + ceil(mp * import_bps / 10000): the caller pays the remote price
	// plus the local import duty, all locked atomically at dispatch time.
	proxyPrice := m.Price + ceilDiv(m.Price*k.cfg.ImportBPS, 10000)
	incoming := []incomingOp{{
		key:  m.ActionID,
		hash: contentHash,
		apply: func(a *Action) {
			a.Name         = name
			a.Source       = source
			a.Price        = proxyPrice
			a.Description  = m.Description
			a.InputSchema  = m.InputSchema
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
	if remoteUser.RemoteBaseURL == "" {
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
	if !a.Active || !a.Public {
		return nil, ErrUnauthorized.Wrap("manifest only available for public active actions")
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

// SignPeerRequest creates a base64url Ed25519 signature over JCS({handle, public_key, base_url, timestamp}).
// Used when sending a friend request to POST /v1/peers on a remote kernel.
func SignPeerRequest(key ed25519.PrivateKey, handle, publicKey, baseURL, timestamp string) (string, error) {
	return signJCS(key, map[string]string{
		"base_url":   baseURL,
		"handle":     handle,
		"public_key": publicKey,
		"timestamp":  timestamp,
	})
}

// VerifyPeerRequestSignature verifies a friend-request signature: Ed25519 over
// JCS({handle, public_key, base_url, timestamp}) by the key embedded in the request.
func VerifyPeerRequestSignature(pubKeyB64, handle, publicKey, baseURL, timestamp, sigB64 string) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return ErrUnauthorized.Wrap("invalid public key in peer request")
	}
	return verifyJCS(pub, map[string]string{
		"base_url":   baseURL,
		"handle":     handle,
		"public_key": publicKey,
		"timestamp":  timestamp,
	}, sigB64)
}
