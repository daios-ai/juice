package main

// Outbound federation (§13). One adapter owns everything this kernel needs to reach a peer —
// transport dispatch, request signing, settlement rounds, and resolve — so a federation change
// never touches the HTTP action executor. It implements kernel.FederationClient and holds neither
// the kernel nor its private key: signing is a callback into the kernel, which owns the key (§12).

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
)

// fedAdapter is the kernel.FederationClient implementation. transport is nil until serve time (a CLI
// process federates nothing), and localPubKey/signFederation are supplied at construction from the
// kernel, which loads the signing key at bootstrap.
type fedAdapter struct {
	transport      federationTransport // libp2p federation carrier; nil off the serving path
	localPubKey    string              // this kernel's base64url Ed25519 public key
	signFederation signerFunc          // signs as this kernel; the private key never leaves the kernel
}

// newFedAdapter builds the adapter around the kernel's own signer. Only the signers it uses are
// injected: outbound calls need the fed_call signature, while step and settle signing stay on the
// paths that own them (the control path and the kernel respectively).
func newFedAdapter(localPubKey string, sign signerFunc) *fedAdapter {
	return &fedAdapter{localPubKey: localPubKey, signFederation: sign}
}

// SetTransport installs the libp2p carrier once serve has started it, completing construction.
func (c *fedAdapter) SetTransport(tr federationTransport) { c.transport = tr }

// SetLocalPubKey records this kernel's own key, known after bootstrap loads the signing key.
func (c *fedAdapter) SetLocalPubKey(key string) { c.localPubKey = key }

// signerFunc signs a federation request with the platform key, returning (signature, timestamp).
type signerFunc = func(action, counterparty, recipient, expectedContractHash, idempotencyKey, argsHash string) (sig, ts string, err error)

// ExecuteFederation sends a cross-kernel call over the libp2p federation transport (§13),
// addressing the peer by its Ed25519 public key. The routing (peer key, action id) that used
// to live in a URL is now explicit arguments. A missing transport, or a transport error,
// returns a zero FederationResult so the kernel keeps the call pending for retry.
func (c *fedAdapter) ExecuteFederation(ctx context.Context, peerPublicKey, actionID, expectedContractHash, idempotencyKey string, args map[string]any) (kernel.FederationResult, error) {
	if c.transport == nil {
		// No transport at all: the request provably cannot have been sent (§13 never-dispatched).
		return kernel.FederationResult{NotDispatched: true}, nil
	}
	return executeFederationOverTransport(ctx, c.transport, c.signFederation, c.localPubKey,
		peerPublicKey, actionID, expectedContractHash, idempotencyKey, args)
}

// federationTransport is the outbound half of the libp2p transport this executor needs; *fed.Transport
// satisfies it. Keeping it an interface lets the fake in tests stand in without a real network.
type federationTransport interface {
	Call(ctx context.Context, peerKey string, req fed.CallRequest) (fed.CallResponse, error)
	Resolve(ctx context.Context, peerKey string, req fed.ResolveRequest) (fed.ResolveResponse, error)
	Settle(ctx context.Context, peerKey string, req fed.SettleRequest) (fed.SettleResponse, error)
	Step(ctx context.Context, peerKey string, req fed.StepRequest) (fed.StepResponse, error)
}

// Transport deadlines live here, with the carrier: the kernel owns step protocol semantics but has
// no business naming a wall-clock bound per operation. A list only measures reachability, while a
// completion waits on the peer running the resumed call synchronously — hence the wider bound. Both
// derive from the caller's context, so cancellation upstream still cuts them short.
const (
	fedStepListTimeout     = 8 * time.Second
	fedStepCompleteTimeout = 60 * time.Second
)

// CompletePeerStep and ListPeerSteps implement kernel.StepCaller over /juice/fed/step/1 (§13). The
// kernel hands over signed scalars; this builds the wire request, dispatches it, and reports the
// raw status/body plus the never-dispatched proof — no Juice semantics are applied here.
func (c *fedAdapter) CompletePeerStep(ctx context.Context, peerKey, timestamp, signature, stepID, idempotencyKey string,
	input []byte, forUserID, userAttestation, userTimestamp string) (int, []byte, bool, error) {
	return c.step(ctx, peerKey, fedStepCompleteTimeout, fed.StepRequest{
		Kind: "complete", Counterparty: c.localPubKey, Timestamp: timestamp, Signature: signature,
		StepID: stepID, IdempotencyKey: idempotencyKey, Input: json.RawMessage(input),
		ForUserID: forUserID, UserAttestation: userAttestation, UserTimestamp: userTimestamp,
	})
}

func (c *fedAdapter) ListPeerSteps(ctx context.Context, peerKey, timestamp, signature string) (int, []byte, bool, error) {
	return c.step(ctx, peerKey, fedStepListTimeout, fed.StepRequest{
		Kind: "list", Counterparty: c.localPubKey, Timestamp: timestamp, Signature: signature,
	})
}

func (c *fedAdapter) step(ctx context.Context, peerKey string, timeout time.Duration, req fed.StepRequest) (int, []byte, bool, error) {
	if c.transport == nil {
		return 0, nil, true, nil // no carrier: the request provably cannot have been sent (§13)
	}
	octx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := c.transport.Step(octx, peerKey, req)
	if err != nil {
		return 0, nil, errors.Is(err, fed.ErrNotDispatched), err
	}
	return resp.Status, resp.Body, false, nil
}

// Settle implements kernel.FederationSettler over /juice/fed/settle/1 (§13): the debtor forwards one
// signed round to the peer and returns its raw response body (a signed SettlementRecord) and status.
func (c *fedAdapter) Settle(ctx context.Context, peerPublicKey, kind, timestamp, signature, settlementID string, amount int64, nonce string, record []byte) (int, []byte, error) {
	if c.transport == nil {
		return 0, nil, kernel.ErrPeerUnreachable.Wrap("federation transport not running")
	}
	resp, err := c.transport.Settle(ctx, peerPublicKey, fed.SettleRequest{
		Kind: kind, Counterparty: c.localPubKey, Timestamp: timestamp, Signature: signature,
		SettlementID: settlementID, Amount: amount, Nonce: nonce, Record: record,
	})
	if err != nil {
		return 0, nil, kernel.ErrPeerUnreachable.Wrap("peer unreachable")
	}
	return resp.Status, resp.Body, nil
}

// ResolveRemoteAction / ResolveRemoteUser implement kernel.RemoteResolver over the transport's
// /juice/fed/resolve/1 protocol (§13 subscription-free calls): fetch one signed manifest, or map a
// user reference to its stable id+handle on the peer. A missing transport is ErrPeerUnreachable so
// the kernel never treats "no network" as "action absent".
func (c *fedAdapter) ResolveRemoteAction(ctx context.Context, peerPublicKey, owner, name string) (*kernel.ActionManifest, error) {
	if c.transport == nil {
		return nil, kernel.ErrPeerUnreachable.Wrap("federation transport not running")
	}
	resp, err := c.transport.Resolve(ctx, peerPublicKey, fed.ResolveRequest{Kind: "action", Owner: owner, Name: name})
	if err != nil {
		return nil, kernel.ErrPeerUnreachable.Wrap("peer unreachable")
	}
	if resp.Status != 200 {
		return nil, kernel.ErrNotFound.Wrap("remote action not found")
	}
	var m kernel.ActionManifest
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return nil, kernel.ErrInvalidInput.Wrap("invalid remote manifest")
	}
	return &m, nil
}

func (c *fedAdapter) ResolveRemoteUser(ctx context.Context, peerPublicKey, ref string) (string, string, error) {
	if c.transport == nil {
		return "", "", kernel.ErrPeerUnreachable.Wrap("federation transport not running")
	}
	resp, err := c.transport.Resolve(ctx, peerPublicKey, fed.ResolveRequest{Kind: "user", User: ref})
	if err != nil {
		return "", "", kernel.ErrPeerUnreachable.Wrap("peer unreachable")
	}
	if resp.Status != 200 {
		return "", "", kernel.ErrNotFound.Wrap("remote user not found")
	}
	var body struct {
		UserID string `json:"user_id"`
		Handle string `json:"handle"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return "", "", kernel.ErrInvalidInput.Wrap("invalid resolve response")
	}
	return body.UserID, body.Handle, nil
}

// executeFederationOverTransport is the transport-backed kernel.FederationExecutor. It signs the
// request as this kernel and sends the exact args bytes so the receiver's args_hash matches.
func executeFederationOverTransport(ctx context.Context, tr federationTransport, signerFn signerFunc,
	localPubKey, peerPublicKey, actionID, expectedContractHash, idempotencyKey string, args map[string]any) (kernel.FederationResult, error) {

	body, err := json.Marshal(args)
	if err != nil {
		return kernel.FederationResult{}, kernel.ErrInvalidInput.Wrap("could not serialize args")
	}
	argsHash := sha256HexBytes(body)
	req := fed.CallRequest{
		Action:               actionID,
		Counterparty:         localPubKey,
		ExpectedContractHash: expectedContractHash,
		IdempotencyKey:       idempotencyKey,
		Args:                 json.RawMessage(body),
	}
	if signerFn != nil {
		if sig, ts, serr := signerFn(actionID, localPubKey, peerPublicKey, expectedContractHash, idempotencyKey, argsHash); serr == nil {
			req.Signature = sig
			req.Timestamp = ts
		}
	}
	resp, err := tr.Call(ctx, peerPublicKey, req)
	if err != nil {
		// Provably-never-sent (resolve/connect failed) → NotDispatched, so a first dispatch may
		// fail fast (§13). Any other transport error stays pending: the request may have executed
		// remotely, so only a signed receipt (or the max-age bound) may settle it.
		return kernel.FederationResult{NotDispatched: errors.Is(err, fed.ErrNotDispatched)}, nil
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
