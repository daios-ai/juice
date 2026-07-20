// Package fed is the federation transport: the sole carrier for cross-kernel calls,
// manifest serving, gossip, and inspection (§13). It is a replaceable
// module behind an interface, exactly like store and llm; the kernel never imports it.
//
// Peers are addressed only by Ed25519 public key. The transport resolves a key to a live
// libp2p connection — direct when the peer is publicly reachable, hole-punched through NAT
// when possible, relayed as a last resort — and carries opaque JSON payloads that the
// caller (cmd/juice) signs and verifies with the §13 rules. The transport knows nothing
// about receipts, settlement, or money; it moves bytes between kernels by key.
package fed

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
)

// ErrNotDispatched marks a transport failure where the request provably never left this host:
// resolving/connecting to the peer failed before any request byte was written (§13 never-dispatched).
// The caller (cmd/juice) translates it into FederationResult.NotDispatched so the kernel — which
// never imports fed — can fail-fast a first dispatch. Post-connection failures (stream negotiation,
// write, read) are NOT wrapped: bytes may have reached the peer, so the call stays pending for retry.
var ErrNotDispatched = errors.New("fed: request not dispatched")

// Protocol IDs are versioned libp2p streams. The version suffix lets the protocol evolve
// without silent incompatibility — HTTP+JSON was implicitly versionless; libp2p makes it explicit.
const (
	ProtocolCall     = "/juice/fed/call/1"
	ProtocolManifest = "/juice/fed/manifest/1"
	ProtocolGossip   = "/juice/fed/gossip/1"
	ProtocolInspect  = "/juice/fed/inspect/1"
	ProtocolStep     = "/juice/fed/step/1"
)

// CallRequest is the wire form of an inbound federation call (§13). Args carries the exact
// bytes the caller hashed and signed, so the receiver's args_hash matches byte-for-byte.
type CallRequest struct {
	Action         string          `json:"action"`          // remote action ref, @owner/name
	Counterparty   string          `json:"counterparty"`    // caller's base64url Ed25519 public key
	IdempotencyKey string          `json:"idempotency_key"` //
	Timestamp      string          `json:"timestamp"`       // RFC3339
	Signature      string          `json:"signature"`       // Ed25519 over JCS({action,args_hash,counterparty,idempotency_key,timestamp})
	Args           json.RawMessage `json:"args"`            // exact request bytes
}

// CallResponse carries the settlement envelope back to the caller. Status mirrors the HTTP
// status codes the pre-migration federation used (200 success, 402/403/409/422 failures),
// so settlement branching above the seam is unchanged. Body is {result,receipt} or {error,receipt}.
type CallResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// StepRequest is the wire form of a /juice/fed/step/1 request (§13). Kind selects the operation:
// "list" enumerates the waiting steps this peer is the required caller of, "complete" resumes one.
// Input carries the exact bytes the caller hashed and signed, so input_hash matches byte-for-byte.
type StepRequest struct {
	Kind           string          `json:"kind"`                      // "list" | "complete"
	Counterparty   string          `json:"counterparty"`              // caller's base64url Ed25519 public key
	Timestamp      string          `json:"timestamp"`                 // RFC3339
	Signature      string          `json:"signature"`                 // Ed25519 over the kind's canonical payload
	StepID         string          `json:"step_id,omitempty"`         // complete only
	IdempotencyKey string          `json:"idempotency_key,omitempty"` // complete only
	Input          json.RawMessage `json:"input,omitempty"`           // complete only; exact request bytes
}

// StepResponse mirrors CallResponse: a status plus an opaque JSON body. Unlike a call, a step
// completion parks nothing on the requester, so failures are plain typed errors — there is no
// local trace awaiting a signed rejection receipt (§13).
type StepResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// Handlers is implemented by cmd/juice to answer inbound protocol streams. Each method
// receives the peer's verified public key (from the authenticated libp2p connection) plus
// the request, and returns opaque JSON. The transport applies no Juice semantics itself.
type Handlers interface {
	// OnCall handles an inbound /juice/fed/call/1 request. peerKey is the connection's
	// authenticated public key; the handler still verifies req.Signature per §13.
	OnCall(ctx context.Context, peerKey string, req CallRequest) CallResponse
	// OnManifest returns one JSON frame per action manifest to serve (chunked, relay-safe).
	OnManifest(ctx context.Context, peerKey string) ([]json.RawMessage, error)
	// OnGossip returns the gossip document as JSON.
	OnGossip(ctx context.Context, peerKey string) (json.RawMessage, error)
	// OnInspect returns the inspect document (identity + public actions + transacted peers) as JSON.
	OnInspect(ctx context.Context, peerKey string) (json.RawMessage, error)
	// OnStep handles an inbound /juice/fed/step/1 request: listing or completing the waiting
	// steps this peer is the required caller of (§10, §13).
	OnStep(ctx context.Context, peerKey string, req StepRequest) StepResponse
}

// Config configures a transport host.
type Config struct {
	SigningKey     ed25519.PrivateKey // platform key; also the libp2p identity (§12)
	ListenAddrs    []string           // multiaddrs to listen on; empty = sensible defaults
	BootstrapPeers []string           // seed multiaddrs (incl. /p2p/<id>); sole discovery source
	Handlers       Handlers           // inbound protocol handlers (from cmd/juice)
	// AllowPrivateAddrs keeps loopback/private multiaddrs usable so the flow harness can run a
	// full network on 127.0.0.1. Production leaves this false (public reachability only).
	AllowPrivateAddrs bool
}
