package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// VerifyRemoteReceipt verifies the stored remote receipt for a remote-proxy transaction.
// Available to any party satisfying CanReadTransaction. Returns ErrInvalidState for
// non-remote-proxy transactions (no remote receipt stored).
func (k *Kernel) VerifyRemoteReceipt(ctx context.Context, subjectID, txID string) (*ReceiptVerification, error) {
	tv, err := k.ReadTransaction(ctx, subjectID, txID)
	if err != nil {
		return nil, err
	}
	tx := tv.Transaction
	if tx.RemoteReceiptJSON == "" {
		return nil, ErrInvalidState.Wrap("transaction has no remote receipt")
	}

	// Decode the stored receipt.
	var r Receipt
	if err := json.Unmarshal([]byte(tx.RemoteReceiptJSON), &r); err != nil {
		return nil, ErrInternal.Wrapf("decode remote receipt: %v", err)
	}

	// Resolve the remote kernel's public key from the transaction's target user (the remote peer).
	// Use tx.TargetUserID and tx.RemoteActionID directly so verification works even after the
	// local proxy action is soft-deleted.
	owner, err := k.store.ReadUser(ctx, tx.TargetUserID)
	if err != nil {
		return nil, err
	}

	var checks ReceiptChecks

	// 1. Hash: SHA-256(receipt JSON) must equal stored hash.
	checks.ReceiptHash = sha256Hex(tx.RemoteReceiptJSON) == tx.RemoteReceiptHash

	// 2. Signature: verify Ed25519 over CanonicalJSON of receipt with Signature cleared.
	if owner.PublicKey != "" {
		pub, pubErr := decodeRemotePublicKey(owner.PublicKey)
		if pubErr == nil {
			cp := r
			cp.Signature = ""
			if payload, jcsErr := CanonicalJSON(cp); jcsErr == nil {
				if sig, decErr := base64.RawURLEncoding.DecodeString(r.Signature); decErr == nil {
					checks.Signature = ed25519.Verify(pub, payload, sig)
				}
			}
		}
	}

	// 3–9. Field equality checks.
	// The receipt carries the remote action's ID, not the local proxy ID.
	// Use the remote_action_id captured in the transaction at commit time.
	if tx.RemoteActionID != "" {
		checks.ActionID = r.ActionID == tx.RemoteActionID
	} else {
		checks.ActionID = r.ActionID == tx.ActionID
	}
	checks.Status = r.Status == tx.Status
	checks.Gross = r.Gross == tx.Gross
	checks.Net = r.Net == tx.Net
	checks.Fee = r.Fee == tx.Fee

	if h, hashErr := jcsHashStr(string(tx.ArgsJSON)); hashErr == nil {
		checks.ArgsHash = r.ArgsHash == h
	}
	if h, hashErr := jcsHashStr(string(tx.ReplyJSON)); hashErr == nil {
		checks.ReplyHash = r.ReplyHash == h
	}

	valid := checks.ReceiptHash && checks.Signature && checks.ActionID &&
		checks.Status && checks.Gross && checks.Net && checks.Fee &&
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

// SignFederation signs a federation payload with the platform key and returns
// (signature, timestamp). Returns an error if the signing key is not configured.
func (k *Kernel) SignFederation(action, counterparty, idempotencyKey, argsHash string) (sig, ts string, err error) {
	ts = time.Now().UTC().Format(time.RFC3339)
	sig, err = SignFederationPayload(k.cfg.SigningKey, action, counterparty, idempotencyKey, ts, argsHash)
	return
}

// ---- Federation operations ----

// RegisterRemoteKernel creates or updates a local user record representing a remote kernel peer.
// Only the superuser may register remote peers.
func (k *Kernel) RegisterRemoteKernel(ctx context.Context, subjectID, handle, publicKey, baseURL string) (*User, error) {
	if err := k.requireSuperuser(ctx, subjectID); err != nil {
		return nil, err
	}
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
	// This invariant holds for both new registrations and base-URL updates.
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
		Email:         handle + "@remote",
		PasswordHash:  "remote",
		PublicKey:     publicKey,
		RemoteBaseURL: baseURL,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := k.store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("remote_kernel.registered", "handle", handle, "base_url", baseURL)
	return u, nil
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

// ListRemoteKernels returns all local user records that represent remote kernel peers.
func (k *Kernel) ListRemoteKernels(ctx context.Context) ([]*User, error) {
	all, err := k.store.ListUsers(ctx, 1000, 0)
	if err != nil {
		return nil, err
	}
	var remote []*User
	for _, u := range all {
		if u.RemoteBaseURL != "" {
			remote = append(remote, u)
		}
	}
	return remote, nil
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
	incoming := []incomingOp{{
		key:  m.ActionID,
		hash: contentHash,
		apply: func(a *Action) {
			a.Name         = name
			a.Source       = source
			a.Price        = m.Price
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
				Price:          m.Price,
				Description:    m.Description,
				InputSchema:    m.InputSchema,
				OutputSchema:   m.OutputSchema,
				Source:         source,
				ArtifactHash:   contentHash, // store content hash so reconcileImport can compare on re-import
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

// manifestCanonicalPayload returns the canonical JCS bytes of m with Signature cleared.
func manifestCanonicalPayload(m *ActionManifest) ([]byte, error) {
	cp := *m
	cp.Signature = ""
	return CanonicalJSON(cp)
}

// SignManifest creates a base64url Ed25519 signature over the canonical ActionManifest.
func SignManifest(key ed25519.PrivateKey, m *ActionManifest) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	payload, err := manifestCanonicalPayload(m)
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize manifest: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}

// VerifyManifestSignature checks that m.Signature was produced by the private key
// corresponding to pubKeyB64 (base64url Ed25519 public key).
func VerifyManifestSignature(pubKeyB64 string, m *ActionManifest) error {
	pub, err := decodeRemotePublicKey(pubKeyB64)
	if err != nil {
		return err
	}
	payload, err := manifestCanonicalPayload(m)
	if err != nil {
		return ErrInternal.Wrapf("canonicalize manifest: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(m.Signature)
	if err != nil || !ed25519.Verify(pub, payload, sig) {
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
	payload, err := CanonicalJSON(map[string]string{
		"action":          action,
		"args_hash":       argsHash,
		"counterparty":    counterparty,
		"idempotency_key": idempotencyKey,
		"timestamp":       timestamp,
	})
	if err != nil {
		return ErrInternal.Wrapf("canonicalize federation payload: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !ed25519.Verify(pub, payload, sig) {
		return ErrUnauthenticated.Wrap("federation signature is invalid")
	}
	return nil
}

// SignFederationPayload creates a base64url Ed25519 signature over the canonical federation payload.
func SignFederationPayload(key Ed25519PrivateKey, action, counterparty, idempotencyKey, timestamp, argsHash string) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidState.Wrap("signing key is not configured")
	}
	payload, err := CanonicalJSON(map[string]string{
		"action":          action,
		"args_hash":       argsHash,
		"counterparty":    counterparty,
		"idempotency_key": idempotencyKey,
		"timestamp":       timestamp,
	})
	if err != nil {
		return "", ErrInternal.Wrapf("canonicalize federation payload: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}
