package fed

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// fakeHandlers records the last inbound request and returns canned responses.
type fakeHandlers struct {
	lastCallPeer string
	lastCall     CallRequest
	callBody     json.RawMessage
	gossip       json.RawMessage
	manifests    []json.RawMessage
	friendStatus string
}

func (f *fakeHandlers) OnCall(_ context.Context, peerKey string, req CallRequest) CallResponse {
	f.lastCallPeer = peerKey
	f.lastCall = req
	return CallResponse{Status: 200, Body: f.callBody}
}
func (f *fakeHandlers) OnFriend(_ context.Context, _ string, _ FriendRequest) FriendResponse {
	return FriendResponse{Status: f.friendStatus}
}
func (f *fakeHandlers) OnManifest(_ context.Context, _ string) ([]json.RawMessage, error) {
	return f.manifests, nil
}
func (f *fakeHandlers) OnGossip(_ context.Context, _ string) (json.RawMessage, error) {
	return f.gossip, nil
}
func (f *fakeHandlers) OnInspect(_ context.Context, _ string) (json.RawMessage, error) {
	return f.gossip, nil
}

func newTestTransport(t *testing.T, h Handlers, bootstrap []string) *Transport {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	tr, err := New(context.Background(), Config{
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

// A full round-trip over real libp2p streams on loopback: B resolves A by key (via the
// bootstrap connection) and every protocol returns the server's canned payload.
func TestTransportRoundTrip(t *testing.T) {
	srv := &fakeHandlers{
		callBody:     json.RawMessage(`{"result":{"ok":true},"receipt":null}`),
		gossip:       json.RawMessage(`{"public_key":"srv","handle":"@srv"}`),
		manifests:    []json.RawMessage{json.RawMessage(`{"name":"a"}`), json.RawMessage(`{"name":"b"}`)},
		friendStatus: "accepted",
	}
	a := newTestTransport(t, srv, nil)

	// B bootstraps to A, so New connects B→A and resolve(A) succeeds without the DHT.
	b := newTestTransport(t, &fakeHandlers{}, a.ListenAddrs())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Call
	resp, err := b.Call(ctx, a.PublicKey(), CallRequest{
		Action: "@srv/act", Counterparty: b.PublicKey(), IdempotencyKey: "idem-1",
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

	// Friend
	fr, err := b.Friend(ctx, a.PublicKey(), FriendRequest{Handle: "@b", PublicKey: b.PublicKey(), Timestamp: "t", Signature: "s"})
	if err != nil || fr.Status != "accepted" {
		t.Fatalf("Friend: %v status=%q", err, fr.Status)
	}

	// Gossip
	g, err := b.Gossip(ctx, a.PublicKey())
	if err != nil || string(g) != `{"public_key":"srv","handle":"@srv"}` {
		t.Fatalf("Gossip: %v body=%s", err, g)
	}

	// Manifests (chunked, one frame per action)
	ms, err := b.Manifests(ctx, a.PublicKey())
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}
	if len(ms) != 2 || string(ms[0]) != `{"name":"a"}` || string(ms[1]) != `{"name":"b"}` {
		t.Fatalf("Manifests: got %v", ms)
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
	if _, err := a.Call(ctx, unknown, CallRequest{Action: "@x/y"}); err == nil {
		t.Fatal("expected error calling an unresolvable key")
	}
}
