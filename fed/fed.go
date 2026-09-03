// Package fed is the federation transport: the sole carrier for cross-kernel calls,
// single-action resolution, gossip, and steps (§13). It is a replaceable
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

// Protocol IDs are versioned libp2p streams. A signature/payload change keeps its stream id and
// surfaces as a per-payload signature failure: the network upgrades in lockstep, so adding
// CallRequest's recipient/expected_contract_hash fields (§13) warrants no id bump. The inspect
// protocol is served by gossip (§13); there is no bulk-manifest protocol — a call resolves one
// action on demand over ProtocolResolve (§8).
const (
	ProtocolCall    = "/juice/fed/call/1"
	ProtocolResolve = "/juice/fed/resolve/1"
	ProtocolGossip  = "/juice/fed/gossip/1"
	ProtocolStep    = "/juice/fed/step/1"
	ProtocolSettle  = "/juice/fed/settle/1"
)

// GossipRequest is the wire form of a /juice/fed/gossip/1 request (§13): the evidence cursor to
// resume from. Empty starts at the oldest retained evidence. The catalog snapshot rides every reply.
type GossipRequest struct {
	Cursor string `json:"cursor,omitempty"`
}

// ResolveRequest is the wire form of a /juice/fed/resolve/1 request (§13): the open, read-only
// single-action / single-principal resolution that makes calling need no prior subscription.
// Kind "action" resolves one action (Owner handle + Name) to its signed manifest; kind "user"
// resolves a user reference to its stable id and handle. The request carries no signature —
// it is public directory information; the returned manifest is itself signed.
type ResolveRequest struct {
	Kind  string `json:"kind"`            // "action" | "user"
	Owner string `json:"owner,omitempty"` // action owner handle (kind=action)
	Name  string `json:"name,omitempty"`  // action name (kind=action)
	User  string `json:"user,omitempty"`  // user reference: handle or id (kind=user)
}

// Response is the shape every protocol reply takes: a status plus an opaque JSON body. Status
// mirrors the HTTP status codes the pre-migration federation used (200 success, 402/403/409/422
// failures), so settlement branching above the seam is unchanged; the transport applies no Juice
// semantics to either field. The per-protocol aliases below name what each body carries.
type Response struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// ResolveResponse carries a signed ActionManifest (kind "action") or {"user_id","handle"}
// (kind "user"); on miss, an error.
type ResolveResponse = Response

// CallRequest is the wire form of an inbound federation call (§13). Args carries the exact
// bytes the caller hashed and signed, so the receiver's args_hash matches byte-for-byte.
type CallRequest struct {
	Action               string `json:"action"`                 // the action's stable id on the serving kernel
	Counterparty         string `json:"counterparty"`           // caller's base64url Ed25519 public key
	ExpectedContractHash string `json:"expected_contract_hash"` // contract hash the caller cached (§8 If-Match)
	IdempotencyKey       string `json:"idempotency_key"`        //
	Timestamp            string `json:"timestamp"`              // RFC3339
	// Signature is Ed25519 over JCS({action,args_hash,counterparty,expected_contract_hash,idempotency_key,recipient,timestamp}).
	// recipient (the serving kernel's key) is bound into the signature but not carried on the wire: the signer
	// signs the key it dialed, the receiver verifies with its own key, so a captured request cannot be replayed
	// to a third kernel (§13, matching the step protocol).
	Signature string          `json:"signature"`
	Args      json.RawMessage `json:"args"` // exact request bytes
}

// CallResponse carries the settlement envelope back to the caller: {result,receipt} or
// {error,receipt}.
type CallResponse = Response

// StepRequest is the wire form of a /juice/fed/step/1 request (§13). Kind selects the operation:
// "list" enumerates the waiting steps this peer is the required caller of, "complete" resumes one.
// Input carries the exact bytes the caller hashed and signed, so input_hash matches byte-for-byte.
type StepRequest struct {
	Kind            string          `json:"kind"`                       // "list" | "complete"
	Counterparty    string          `json:"counterparty"`               // caller's base64url Ed25519 public key
	Timestamp       string          `json:"timestamp"`                  // RFC3339
	Signature       string          `json:"signature"`                  // Ed25519 over the kind's canonical payload
	StepID          string          `json:"step_id,omitempty"`          // complete only
	IdempotencyKey  string          `json:"idempotency_key,omitempty"`  // complete only
	Input           json.RawMessage `json:"input,omitempty"`            // complete only; exact request bytes
	ForUserID       string          `json:"for_user_id,omitempty"`      // complete: the completing user's stable id on the requesting kernel (§13)
	UserAttestation string          `json:"user_attestation,omitempty"` // complete: home-kernel step_auth signature over that id
	UserTimestamp   string          `json:"user_timestamp,omitempty"`   // complete: attestation timestamp (own freshness window)
}

// StepResponse carries a step list or completion result. Unlike a call, a step completion parks
// nothing on the requester, so failures are plain typed errors — there is no local trace awaiting a
// signed rejection receipt (§13).
type StepResponse = Response

// SettleRequest is the wire form of a /juice/fed/settle/1 request (§13): the debtor-driven two-party
// commit/reveal that settles a sub-quantum residual debt probabilistically. Kind selects the round:
// "open" asks the creditor to commit (returns a signed open record with H(s)); "finish" hands the
// nonce back with the creditor's own open record so the creditor reveals s, computes the outcome, and
// applies the three-way settlement; "reconcile" re-presents an expired open record so the creditor
// applies the binding clear-for-zero (FIX 2). Signatures are over disjoint scoped payloads (§12).
type SettleRequest struct {
	Kind         string          `json:"kind"`              // "open" | "finish" | "reconcile"
	Counterparty string          `json:"counterparty"`      // debtor's base64url Ed25519 public key
	Timestamp    string          `json:"timestamp"`         // RFC3339
	Signature    string          `json:"signature"`         // Ed25519 over the kind's scoped canonical payload
	SettlementID string          `json:"settlement_id"`     // debtor-chosen unique id, binds the whole exchange
	Amount       int64           `json:"amount,omitempty"`  // open: the debt d the debtor owes (creditor checks == its receivable)
	Nonce        string          `json:"nonce,omitempty"`   // finish: the debtor's committed nonce
	TxHash       string          `json:"tx_hash,omitempty"` // announce: the payment that closes the debt
	Record       json.RawMessage `json:"record,omitempty"`  // finish/reconcile: the creditor-signed open record carried back
}

// SettleResponse carries a signed SettlementRecord (open → commitment; finish/reconcile → final
// record with outcome), or an error.
type SettleResponse = Response

// Handlers is implemented by cmd/juice to answer inbound protocol streams. Each method
// receives the peer's verified public key (from the authenticated libp2p connection) plus
// the request, and returns opaque JSON. The transport applies no Juice semantics itself.
type Handlers interface {
	// OnCall handles an inbound /juice/fed/call/1 request. peerKey is the connection's
	// authenticated public key; the handler still verifies req.Signature per §13.
	OnCall(ctx context.Context, peerKey string, req CallRequest) CallResponse
	// OnResolve answers a /juice/fed/resolve/1 request: one action's signed manifest or one
	// user's stable id+handle (§13). peerKey is informational; the reply is public directory data.
	OnResolve(ctx context.Context, peerKey string, req ResolveRequest) ResolveResponse
	// OnGossip returns one page of the gossip document (first-party catalog snapshot + one
	// evidence page after req.Cursor) as JSON (§13). Gossip carries no membership — discovery of
	// which kernels exist is routing discovery's job (Advertise/DiscoverProviders).
	OnGossip(ctx context.Context, peerKey string, req GossipRequest) (json.RawMessage, error)
	// OnStep handles an inbound /juice/fed/step/1 request: listing or completing the waiting
	// steps this peer is the required caller of (§10, §13).
	OnStep(ctx context.Context, peerKey string, req StepRequest) StepResponse
	// OnSettle handles an inbound /juice/fed/settle/1 request (§13): the creditor side of the
	// two-party commit/reveal residual settlement. peerKey is the connection's authenticated key;
	// the handler still verifies req.Signature against req.Counterparty per §13.
	OnSettle(ctx context.Context, peerKey string, req SettleRequest) SettleResponse
}

// Config configures a transport host.
type Config struct {
	SigningKey     ed25519.PrivateKey // platform key; also the libp2p identity (§12)
	ListenAddrs    []string           // multiaddrs to listen on; empty = sensible defaults
	BootstrapPeers []string           // seed multiaddrs (incl. /p2p/<id>); DHT bootstrap + discovery seed (§13)
	Handlers       Handlers           // inbound protocol handlers (from cmd/juice)
	// AllowPrivateAddrs keeps loopback/private multiaddrs usable so the flow harness can run a
	// full network on 127.0.0.1. Production leaves this false (public reachability only).
	AllowPrivateAddrs bool
	// Namespace is the rendezvous string this kernel advertises and enumerates. It carries the
	// network digest, so kernels of different worlds never find each other (D23). Empty is a
	// configuration error, not a default: an unnamespaced kernel would meet every world at once.
	Namespace string
}
