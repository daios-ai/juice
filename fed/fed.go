// Package fed is the federation transport: the sole carrier for cross-kernel calls,
// friend handshakes, manifest serving, gossip, and inspection (§13). It is a replaceable
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
)

// Protocol IDs are versioned libp2p streams. The version suffix lets the protocol evolve
// without silent incompatibility — HTTP+JSON was implicitly versionless; libp2p makes it explicit.
const (
	ProtocolCall     = "/juice/fed/call/1"
	ProtocolFriend   = "/juice/fed/friend/1"
	ProtocolManifest = "/juice/fed/manifest/1"
	ProtocolGossip   = "/juice/fed/gossip/1"
	ProtocolInspect  = "/juice/fed/inspect/1"
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

// FriendRequest is the wire form of the friend handshake (§13). No URL — v0.5 forbids URLs
// in the federation protocol; the peer is identified and reachable by PublicKey alone.
type FriendRequest struct {
	Handle    string `json:"handle"`
	PublicKey string `json:"public_key"` // requester's base64url Ed25519 public key
	Timestamp string `json:"timestamp"`  // RFC3339
	Signature string `json:"signature"`  // Ed25519 over JCS({handle,public_key,timestamp})
}

// FriendResponse reports acceptance ("accepted"/"pending") or a rejection reason.
type FriendResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Handlers is implemented by cmd/juice to answer inbound protocol streams. Each method
// receives the peer's verified public key (from the authenticated libp2p connection) plus
// the request, and returns opaque JSON. The transport applies no Juice semantics itself.
type Handlers interface {
	// OnCall handles an inbound /juice/fed/call/1 request. peerKey is the connection's
	// authenticated public key; the handler still verifies req.Signature per §13.
	OnCall(ctx context.Context, peerKey string, req CallRequest) CallResponse
	// OnFriend handles an inbound /juice/fed/friend/1 request.
	OnFriend(ctx context.Context, peerKey string, req FriendRequest) FriendResponse
	// OnManifest returns one JSON frame per action manifest to serve (chunked, relay-safe).
	OnManifest(ctx context.Context, peerKey string) ([]json.RawMessage, error)
	// OnGossip returns the gossip document as JSON.
	OnGossip(ctx context.Context, peerKey string) (json.RawMessage, error)
	// OnInspect returns the inspect document (identity + public actions + transacted friends) as JSON.
	OnInspect(ctx context.Context, peerKey string) (json.RawMessage, error)
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
