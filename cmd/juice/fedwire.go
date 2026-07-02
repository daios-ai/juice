package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
)

// fedwire connects the kernel to the libp2p federation transport (§13). Inbound protocol
// streams are answered by fedHandlers (which reuses the same verification and settlement logic
// the HTTP endpoints used); outbound remote-proxy calls go through the transport by peer key.

// startFedTransport builds and starts the federation transport for a serving kernel. It reads
// the platform signing key from config (the same key bootstrap loaded) so the libp2p identity
// is the kernel's Ed25519 identity (§12). AllowPrivateAddrs mirrors allow_local_sources so the
// flow harness can run a whole network on loopback.
func startFedTransport(ctx context.Context, k *kernel.Kernel, logger *log.Logger) (*fed.Transport, error) {
	privB64, _ := k.GetConfig(ctx, configKeySigningPrivate)
	privBytes, err := base64.RawURLEncoding.DecodeString(privB64)
	if err != nil || len(privBytes) != ed25519.PrivateKeySize {
		return nil, kernel.ErrInvalidState.Wrap("signing key unavailable for federation transport")
	}
	handlers := &fedHandlers{kernel: k, log: logger}
	tr, err := fed.New(ctx, fed.Config{
		SigningKey:        ed25519.PrivateKey(privBytes),
		BootstrapPeers:    globalCfg.BootstrapPeers,
		Handlers:          handlers,
		AllowPrivateAddrs: globalCfg.AllowLocalSources,
	})
	if err != nil {
		return nil, err
	}
	// Back-reference so inbound auto-accept can reciprocate over the same transport.
	handlers.transport = tr
	return tr, nil
}

// fedHandlers answers inbound federation protocol streams.
type fedHandlers struct {
	kernel    *kernel.Kernel
	log       *log.Logger
	transport *fed.Transport // set after New so reciprocal friend requests can go out
}

// OnCall verifies and executes an inbound federation call, returning the settlement envelope.
func (h *fedHandlers) OnCall(ctx context.Context, _ string, req fed.CallRequest) fed.CallResponse {
	status, body, err := handleFederationCall(h.kernel, ctx, req.Counterparty, req.Timestamp,
		req.IdempotencyKey, req.Action, req.Signature, []byte(req.Args))
	if err != nil {
		// No receipt to settle on → the caller treats this as pending (retry), exactly as the
		// HTTP path did when it returned an error status with no receipt body.
		b, _ := json.Marshal(map[string]string{"error": err.Error(), "code": kernel.KernelErrorCode(err)})
		return fed.CallResponse{Status: kernel.HTTPStatusFromCode(kernel.KernelErrorCode(err)), Body: b}
	}
	b, _ := json.Marshal(body)
	return fed.CallResponse{Status: status, Body: b}
}

// OnFriend verifies and accepts (or holds pending) an inbound friend request.
func (h *fedHandlers) OnFriend(ctx context.Context, _ string, req fed.FriendRequest) fed.FriendResponse {
	ts, err := time.Parse(time.RFC3339, req.Timestamp)
	if err != nil {
		return fed.FriendResponse{Status: "rejected", Error: "timestamp must be RFC3339"}
	}
	if diff := time.Since(ts); diff < -5*time.Minute || diff > 5*time.Minute {
		return fed.FriendResponse{Status: "rejected", Error: "timestamp out of range"}
	}
	if err := kernel.VerifyPeerRequestSignature(req.PublicKey, req.Handle, req.PublicKey, req.Timestamp, req.Signature); err != nil {
		return fed.FriendResponse{Status: "rejected", Error: "invalid signature"}
	}
	existing, _ := h.kernel.ReadUserByPublicKey(ctx, req.PublicKey)
	if existing != nil && existing.DeniedAt != nil {
		return fed.FriendResponse{Status: "rejected", Error: "peer is denied"}
	}
	if !globalCfg.PeerAutoAccept {
		_ = h.kernel.AccumulateGossip(ctx, &kernel.GossipResponse{PublicKey: req.PublicKey, Handle: req.Handle}, "friend-request")
		return fed.FriendResponse{Status: "pending"}
	}
	if _, err := h.kernel.CreateOrUpdateProxyPeer(ctx, req.Handle, req.PublicKey); err != nil {
		return fed.FriendResponse{Status: "rejected", Error: err.Error()}
	}
	// Reciprocate only for a previously-unknown peer, so two auto-accepting kernels don't
	// ping-pong friend requests forever.
	if existing == nil && h.transport != nil {
		go h.sendReciprocal(req.PublicKey)
	}
	return fed.FriendResponse{Status: "accepted"}
}

// OnManifest returns one signed manifest per active public action (chunked, relay-safe).
func (h *fedHandlers) OnManifest(ctx context.Context, _ string) ([]json.RawMessage, error) {
	actions, err := h.kernel.ListPublicActions(ctx, 200, 0)
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, a := range actions {
		if !a.Active {
			continue
		}
		m, err := h.kernel.GetActionManifest(ctx, a.ID)
		if err != nil {
			continue
		}
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// OnGossip returns the gossip document (§13).
func (h *fedHandlers) OnGossip(ctx context.Context, _ string) (json.RawMessage, error) {
	g, err := h.kernel.GetGossip(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(g)
}

// OnInspect returns identity + public actions + transacted friends. Gossip already carries all
// three, so the inspect document is the gossip document viewed by a prospective friend.
func (h *fedHandlers) OnInspect(ctx context.Context, peerKey string) (json.RawMessage, error) {
	return h.OnGossip(ctx, peerKey)
}

// sendReciprocal sends a signed friend request back to a peer by key over the transport.
func (h *fedHandlers) sendReciprocal(peerKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pubKeyB64, _ := h.kernel.GetConfig(ctx, configKeySigningPublic)
	localHandle := globalCfg.KernelHandle
	if localHandle == "" {
		localHandle, _ = h.kernel.GetConfig(ctx, configKeySuperuser)
	}
	if pubKeyB64 == "" {
		return
	}
	sig, ts, err := h.kernel.SignPeerRequestNow(localHandle, pubKeyB64)
	if err != nil {
		return
	}
	_, _ = h.transport.Friend(ctx, peerKey, fed.FriendRequest{
		Handle: localHandle, PublicKey: pubKeyB64, Timestamp: ts, Signature: sig,
	})
}

// federationTransport is the outbound half of the transport the executor needs. *fed.Transport
// satisfies it; keeping it an interface lets http_exec.go stay free of the fed import.
type federationTransport interface {
	Call(ctx context.Context, peerKey string, req fed.CallRequest) (fed.CallResponse, error)
}

// executeFederationOverTransport is the transport-backed kernel.FederationExecutor. It signs the
// request as this kernel and sends the exact args bytes so the receiver's args_hash matches.
func executeFederationOverTransport(ctx context.Context, tr federationTransport, signerFn signerFunc,
	localPubKey, peerPublicKey, actionRef, idempotencyKey string, args map[string]any) (kernel.FederationResult, error) {

	body, err := json.Marshal(args)
	if err != nil {
		return kernel.FederationResult{}, kernel.ErrInvalidInput.Wrap("could not serialize args")
	}
	argsHash := sha256HexBytes(body)
	req := fed.CallRequest{
		Action:         actionRef,
		Counterparty:   localPubKey,
		IdempotencyKey: idempotencyKey,
		Args:           json.RawMessage(body),
	}
	if signerFn != nil {
		if sig, ts, serr := signerFn(actionRef, localPubKey, idempotencyKey, argsHash); serr == nil {
			req.Signature = sig
			req.Timestamp = ts
		}
	}
	resp, err := tr.Call(ctx, peerPublicKey, req)
	if err != nil {
		// Transport error: no receipt → the kernel keeps the call pending for retry (§13).
		return kernel.FederationResult{HTTPStatus: 0}, nil
	}
	var envelope struct {
		Result  map[string]any  `json:"result"`
		Receipt json.RawMessage `json:"receipt"`
	}
	var receiptJSON string
	if json.Unmarshal(resp.Body, &envelope) == nil && len(envelope.Receipt) > 0 && string(envelope.Receipt) != "null" {
		receiptJSON = string(envelope.Receipt)
	}
	return kernel.FederationResult{Result: envelope.Result, ReceiptJSON: receiptJSON, HTTPStatus: resp.Status}, nil
}

var _ = http.StatusOK
