// SPDX-License-Identifier: AGPL-3.0-only

package fed

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
)

// fakeHandlers records the last inbound request and returns canned responses.
type fakeHandlers struct {
	lastCallPeer     string
	lastCall         CallRequest
	callBody         json.RawMessage
	gossip           json.RawMessage
	gossipErr        error
	resolveBody      json.RawMessage
	lastTaskPeer     string
	lastTask         TaskRequest
	taskBody         json.RawMessage
	lastRevealPeer   string
	lastReveal       RevealRequest
	revealBody       json.RawMessage
	lastTransferPeer string
	lastTransfer     TransferRequest
}

func (f *fakeHandlers) OnCall(_ context.Context, peerKey string, req CallRequest) CallResponse {
	f.lastCallPeer = peerKey
	f.lastCall = req
	return CallResponse{Status: 200, Body: f.callBody}
}
func (f *fakeHandlers) OnResolve(_ context.Context, _ string, _ ResolveRequest) ResolveResponse {
	return ResolveResponse{Status: 200, Body: f.resolveBody}
}
func (f *fakeHandlers) OnGossip(_ context.Context, _ string, _ GossipRequest) (json.RawMessage, error) {
	return f.gossip, f.gossipErr
}
func (f *fakeHandlers) OnTask(_ context.Context, peerKey string, req TaskRequest) TaskResponse {
	f.lastTaskPeer = peerKey
	f.lastTask = req
	return TaskResponse{Status: 200, Body: f.taskBody}
}
func (f *fakeHandlers) OnReveal(_ context.Context, peerKey string, req RevealRequest) RevealResponse {
	f.lastRevealPeer = peerKey
	f.lastReveal = req
	return RevealResponse{Status: 200, Body: f.revealBody}
}

func (f *fakeHandlers) OnTransfer(_ context.Context, peerKey string, req TransferRequest) TransferResponse {
	f.lastTransferPeer = peerKey
	f.lastTransfer = req
	return TransferResponse{Status: 200, Body: json.RawMessage(`{}`)}
}

func newTestTransport(t *testing.T, h Handlers, bootstrap []string) *Transport {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	tr, err := New(context.Background(), Config{
		Namespace:         testNamespace,
		SigningKey:        priv,
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		BootstrapPeers:    bootstrap,
		Handlers:          h,
		AllowPrivateAddrs: true,
	})
	if err != nil {
		t.Fatalf("New transport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// newTestTransportMode is newTestTransport with an explicit DHT mode, so a test can build a real
// client/server topology on loopback. AllowPrivateAddrs otherwise forces every node to ModeServer —
// exactly the configuration that masked the client-mode discovery regression (§15).
func newTestTransportMode(t *testing.T, h Handlers, bootstrap []string, mode dht.ModeOpt) *Transport {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	tr, err := newTransport(context.Background(), Config{
		Namespace:         testNamespace,
		SigningKey:        priv,
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		BootstrapPeers:    bootstrap,
		Handlers:          h,
		AllowPrivateAddrs: true,
	}, withDHTMode(mode))
	if err != nil {
		t.Fatalf("New transport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// A full round-trip over real libp2p streams on loopback: B resolves A by key (via the
// bootstrap connection) and every protocol returns the server's canned payload.
// testNamespace stands for one network's rendezvous string: kernels of different worlds carry
// different ones and so never meet (D23).
const testNamespace = "juice/fed/discovery/1/test"

func TestTransportRoundTrip(t *testing.T) {
	srv := &fakeHandlers{
		callBody: json.RawMessage(`{"result":{"ok":true},"receipt":null}`),
		gossip:   json.RawMessage(`{"public_key":"srv","handle":"@srv"}`),
		taskBody: json.RawMessage(`{"result":{},"tx_id":"tx-1"}`),
	}
	a := newTestTransport(t, srv, nil)

	// B bootstraps to A, so New connects B→A and resolve(A) succeeds without the DHT.
	b := newTestTransport(t, &fakeHandlers{}, a.ListenAddrs())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Call
	resp, err := b.Call(ctx, a.PublicKey(), CallRequest{
		Action: "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", Counterparty: b.PublicKey(), IdempotencyKey: "idem-1",
		Timestamp: "2026-07-02T00:00:00Z", Signature: "sig", Args: json.RawMessage(`{"x":1}`),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != `{"result":{"ok":true},"receipt":null}` {
		t.Fatalf("Call response: status=%d body=%s", resp.Status, resp.Body)
	}
	// The server saw B's proven key and the exact args bytes (args_hash contract).
	if srv.lastCallPeer != b.PublicKey() {
		t.Errorf("server saw peer %q, want %q", srv.lastCallPeer, b.PublicKey())
	}
	if string(srv.lastCall.Args) != `{"x":1}` {
		t.Errorf("server saw args %s, want {\"x\":1}", srv.lastCall.Args)
	}

	// Gossip
	g, err := b.Gossip(ctx, a.PublicKey(), GossipRequest{})
	if err != nil || string(g) != `{"public_key":"srv","handle":"@srv"}` {
		t.Fatalf("Gossip: %v body=%s", err, g)
	}

	// Task (§13): the completion verb carries the exact input bytes, like Call's args.
	sResp, err := b.Task(ctx, a.PublicKey(), TaskRequest{
		Kind: "complete", Counterparty: b.PublicKey(), Timestamp: "2026-07-02T00:00:00Z",
		Signature: "sig", TaskID: "task-1", IdempotencyKey: "idem-2", Input: json.RawMessage(`{"approve":true}`),
	})
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if sResp.Status != 200 || string(sResp.Body) != `{"result":{},"tx_id":"tx-1"}` {
		t.Fatalf("Task response: status=%d body=%s", sResp.Status, sResp.Body)
	}
	if srv.lastTaskPeer != b.PublicKey() {
		t.Errorf("server saw task peer %q, want %q", srv.lastTaskPeer, b.PublicKey())
	}
	if string(srv.lastTask.Input) != `{"approve":true}` || srv.lastTask.TaskID != "task-1" {
		t.Errorf("server saw task %+v", srv.lastTask)
	}

	// Transfer (P11): whom a payment is for reaches the handler with the sender's proven key and
	// every field intact.
	tr := TransferRequest{Counterparty: b.PublicKey(), Timestamp: "2026-07-02T00:00:00Z", Signature: "sig",
		ID: "tx-9", BeneficiaryID: "u-1", Amount: 40, TxHash: "0xabc", BlockchainAddress: "0xvault", BlockchainProof: "proof"}
	if tResp, err := b.Transfer(ctx, a.PublicKey(), tr); err != nil || tResp.Status != 200 {
		t.Fatalf("Transfer: %v %+v", err, tResp)
	}
	if srv.lastTransferPeer != b.PublicKey() || srv.lastTransfer != tr {
		t.Errorf("server saw transfer %+v from %q", srv.lastTransfer, srv.lastTransferPeer)
	}

	// Reachability probe reports a live path.
	r := b.Probe(ctx, a.PublicKey())
	if r.Path != "direct" && r.Path != "relayed" {
		t.Errorf("Probe path: got %q, want direct or relayed", r.Path)
	}
}

// Resolving an unknown key with no route must fail rather than hang forever.
func TestTransportUnresolvableKey(t *testing.T) {
	a := newTestTransport(t, &fakeHandlers{}, nil)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	unknown := base64.RawURLEncoding.EncodeToString(pub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.Call(ctx, unknown, CallRequest{Action: "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31"}); err == nil {
		t.Fatal("expected error calling an unresolvable key")
	}
}
