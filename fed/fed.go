// SPDX-License-Identifier: AGPL-3.0-only

// Package fed is the federation transport: the sole carrier for cross-kernel calls,
// single-action resolution, gossip, and tasks (§13). It is a replaceable
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
	ProtocolCall     = "/juice/fed/call/1"
	ProtocolResolve  = "/juice/fed/resolve/1"
	ProtocolGossip   = "/juice/fed/gossip/1"
	ProtocolTask     = "/juice/fed/task/1"
	ProtocolReveal   = "/juice/fed/settle/1"
	ProtocolTransfer = "/juice/fed/transfer/1"
)

// GossipRequest is the wire form of a /juice/fed/gossip/1 request (§13): the evidence cursor to
// resume from. Empty starts at the oldest retained evidence. The catalog snapshot rides every reply.
type GossipRequest struct {
	Cursor string `json:"cursor,omitempty"`
	// CatalogCursor resumes a catalogue scan, empty to open a new one (§13).
	CatalogCursor string `json:"catalog_cursor,omitempty"`
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
	// BlockchainAddress and BlockchainProof are the asker's proven vault on a user resolve (P11): a
	// kernel about to pay one of the answerer's users says first where the money will come from.
	BlockchainAddress string `json:"blockchain_address,omitempty"`
	BlockchainProof   string `json:"blockchain_proof,omitempty"`
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
	Action               string `json:"action"`                       // the action's stable id on the serving kernel
	Counterparty         string `json:"counterparty"`                 // caller's base64url Ed25519 public key
	ExpectedContractHash string `json:"expected_contract_hash"`       // contract hash the caller cached (§8 If-Match)
	IdempotencyKey       string `json:"idempotency_key"`              //
	Timestamp            string `json:"timestamp"`                    // RFC3339
	Commitment           string `json:"commitment,omitempty"`         // hash of the caller's half of the settlement draw (P10)
	Lottery              int64  `json:"lottery,omitempty"`            // the ticket face value this call is dispatched under (P10)
	BlockchainAddress    string `json:"blockchain_address,omitempty"` // where a winning ticket will be paid from, proven by the rail key
	BlockchainProof      string `json:"blockchain_proof,omitempty"`   // that address's own signature over the caller's key (D23)
	CallerUserID         string `json:"caller_user_id,omitempty"`     // the buyer's own user the call is made for: their stable id there (P4)
	CallerHandle         string `json:"caller_handle,omitempty"`      // and the handle they go by, so the seller records who called
	// Signature is Ed25519 over JCS({action,args_hash,caller_handle,caller_user_id,commitment,counterparty,expected_contract_hash,idempotency_key,lottery,recipient,timestamp}).
	// recipient (the serving kernel's key) is bound into the signature but not carried on the wire: the signer
	// signs the key it dialed, the receiver verifies with its own key, so a captured request cannot be replayed
	// to a third kernel (§13, matching the task protocol).
	Signature string          `json:"signature"`
	Args      json.RawMessage `json:"args"` // exact request bytes
}

// CallResponse carries the settlement envelope back to the caller: {result,receipt} or
// {error,receipt}.
type CallResponse = Response

// TaskRequest is the wire form of a /juice/fed/task/1 request (P8). Kind selects the operation:
// "notice" tells the addressee's kernel a task's state, "complete" resumes a task and "cancel"
// declines it at its holder. Input carries the exact bytes the caller hashed and signed, so
// input_hash matches byte-for-byte.
type TaskRequest struct {
	Kind           string          `json:"kind"`                      // "notice" | "complete" | "cancel"
	Counterparty   string          `json:"counterparty"`              // caller's base64url Ed25519 public key
	Timestamp      string          `json:"timestamp"`                 // RFC3339
	Signature      string          `json:"signature"`                 // Ed25519 over the kind's canonical payload
	TaskID         string          `json:"task_id,omitempty"`         // complete, cancel
	IdempotencyKey string          `json:"idempotency_key,omitempty"` // complete only
	Input          json.RawMessage `json:"input,omitempty"`           // complete only; exact request bytes
	ForUserID      string          `json:"for_user_id,omitempty"`     // complete, cancel: the acting user's stable id on the requesting kernel, signed into the payload (P8)
	UserSuperuser  bool            `json:"user_superuser,omitempty"`  // complete, cancel: the home kernel's word that this user is its operator, the scope a kernel-addressed task demands
	Notice         json.RawMessage `json:"notice,omitempty"`          // notice only: the task's state, signed with the envelope
}

// TaskResponse carries an acknowledgement or a completion's result, with the task's current notice
// on a completion or decline, or an error.
type TaskResponse = Response

// RevealRequest is the wire form of a /juice/fed/settle/1 request (P10): the buyer tells the seller
// how one obligation's draw came out. The secret makes the outcome checkable against the commitment
// the seller already holds; a paying reveal also names the payment and proves the address it comes
// from, since the seller credits cash by its finalized sender.
type RevealRequest struct {
	Counterparty string `json:"counterparty"`      // buyer's base64url Ed25519 public key
	Timestamp    string `json:"timestamp"`         // RFC3339
	Signature    string `json:"signature"`         // Ed25519 over the scoped canonical payload
	TicketID     string `json:"ticket_id"`         // the call's idempotency key, naming the obligation
	Secret       string `json:"secret"`            // the buyer's committed randomness, revealed
	TxHash       string `json:"tx_hash,omitempty"` // paying only: the payment that settles it
}

// RevealResponse carries the ticket as the seller now holds it, or an error.
type RevealResponse = Response

// TransferRequest is the wire form of a /juice/fed/transfer/1 request (P11): the sender tells the
// kernel a payment went to whom it is for. Signature covers every field but itself and the proof,
// which is the rail key's own signature over the vault, as on a call.
type TransferRequest struct {
	Counterparty      string `json:"counterparty"`       // sender's base64url Ed25519 public key
	Timestamp         string `json:"timestamp"`          // RFC3339
	Signature         string `json:"signature"`          // Ed25519 over the scoped canonical payload
	ID                string `json:"id"`                 // the transaction that made the payment
	BeneficiaryID     string `json:"beneficiary_id"`     // the user it is for, by stable id on the receiver
	Amount            int64  `json:"amount"`             // what the payment carries
	TxHash            string `json:"tx_hash"`            // the payment on the rail
	BlockchainAddress string `json:"blockchain_address"` // the sender's vault the payment comes from
	BlockchainProof   string `json:"blockchain_proof"`   // the rail key's proof of that vault
}

// TransferResponse acknowledges the word stored, or carries an error.
type TransferResponse = Response

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
	// OnTask handles an inbound /juice/fed/task/1 request: a task notice, or completing or
	// declining a task addressed to this peer (P8).
	OnTask(ctx context.Context, peerKey string, req TaskRequest) TaskResponse
	// OnReveal handles an inbound /juice/fed/settle/1 request (P10): the seller side of one
	// obligation's draw. peerKey is the connection's authenticated key; the handler still verifies
	// req.Signature against req.Counterparty per §13.
	OnReveal(ctx context.Context, peerKey string, req RevealRequest) RevealResponse
	// OnTransfer handles an inbound /juice/fed/transfer/1 request (P11): whom a payment to this kernel
	// is for. The handler verifies req.Signature against req.Counterparty.
	OnTransfer(ctx context.Context, peerKey string, req TransferRequest) TransferResponse
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
	// network fingerprint, so kernels of different worlds never find each other (D23). Empty is a
	// configuration error, not a default: an unnamespaced kernel would meet every world at once.
	Namespace string
	// MaxInboundPeers is how many inbound connections this host accepts, RelaySlots how many
	// kernels behind NAT it relays for, each holding one of them. Zero takes the default (D12).
	MaxInboundPeers int
	RelaySlots      int
	// AgentVersion is what this kernel says it runs, sent in libp2p's identify exchange on every
	// connection and read back by Probe. Empty leaves the library's own string. It is a label:
	// nothing on the wire is decided by it, so a kernel that sends another one is not stranded.
	AgentVersion string
	// Observe is told how each federation request ended — the protocol it rode, "in" or "out", and
	// whether it completed — for the operator's metrics. Nil observes nothing. It is given no peer,
	// so nothing it records names one.
	Observe func(protocol, direction string, ok bool)
}

// AgentPrefix opens every agent version a Juice kernel sends. Probe keeps a peer's string only
// under it: a kernel built before versions were sent still identifies with the library's own
// string, and that would read as a version of Juice if the prefix were not required.
const AgentPrefix = "juice-kernel/"
