package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
)

// fakeFed is a fedClient whose reachability and live-fetch outcome are controlled per test, so the
// admin handlers' online/offline paths are exercisable without a real network.
type fakeFed struct {
	inspectDoc    json.RawMessage // non-nil → Inspect/Gossip succeed with this; nil → they fail (offline)
	reachPath     string          // "direct" | "relayed" | "unreachable" (default unreachable)
	stepBody      json.RawMessage // non-nil → Step succeeds with this; nil → offline
	stepStatus    int             // status Step returns alongside stepBody
	stepMidStream bool            // Step fails after dispatch (may have executed remotely)
	lastStep      fed.StepRequest // the last outbound step request, for assertions
}

func (f *fakeFed) Gossip(_ context.Context, _ string, _ string) (json.RawMessage, error) {
	if f.inspectDoc == nil {
		return nil, errors.New("fed: cannot resolve peer (offline)")
	}
	return f.inspectDoc, nil
}
func (f *fakeFed) Step(_ context.Context, _ string, req fed.StepRequest) (fed.StepResponse, error) {
	f.lastStep = req
	// stepMidStream models a failure AFTER bytes may have reached the peer (a stream error, or a
	// timeout while it runs the resumed call) — distinct from an unresolvable peer, which provably
	// never sent anything. Only the latter is ErrNotDispatched (§13).
	if f.stepMidStream {
		return fed.StepResponse{}, errors.New("fed: stream closed mid-request")
	}
	if f.stepBody == nil {
		return fed.StepResponse{}, fmt.Errorf("%w: cannot resolve peer (offline)", fed.ErrNotDispatched)
	}
	return fed.StepResponse{Status: f.stepStatus, Body: f.stepBody}, nil
}
func (f *fakeFed) Probe(context.Context, string) fed.Reachability {
	p := f.reachPath
	if p == "" {
		p = "unreachable"
	}
	return fed.Reachability{Path: p}
}
func (f *fakeFed) ListenAddrs() []string { return nil }
func (f *fakeFed) Close() error          { return nil }

// seedPeer creates a proxy peer with one active+public imported proxy action, returning its
// @handle and base64url key — the local state that offline inspect should surface.
func seedPeer(t *testing.T, k *kernel.Kernel, handle string) (string, string) {
	t.Helper()
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)
	sys, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := k.AddPeer(ctx, sys.ID, handle, key)
	if err != nil {
		t.Fatal(err)
	}
	m := kernel.ActionManifest{
		ActionID: "act-1", OwnerHandle: handle, Name: "greet", Description: "greet",
		Kind: kernel.KindHTTP, Price: 5, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "sha256-x", Stats: &kernel.Stats{},
		UpdatedAt: time.Now(),
	}
	sig, _ := kernel.SignManifest(priv, &m)
	m.Signature = sig
	// Cold resolve caches and activates the proxy (§8): the sole import path.
	if _, err := k.ImportPeerAction(ctx, peer.ID, m); err != nil {
		t.Fatalf("seed import: %v", err)
	}
	return handle, key
}

func inspectResp(t *testing.T, srv *server, ident string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/control/peers/inspect?key="+url.QueryEscape(ident), nil)
	rec := httptest.NewRecorder()
	srv.ctlInspectPeer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func listPeersResp(t *testing.T, srv *server, all bool) []map[string]any {
	t.Helper()
	path := "/control/peers"
	if all {
		path += "?all=1"
	}
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	srv.ctlListPeers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("peers status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Peers []map[string]any `json:"peers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Peers
}

// TestListPeersHidesSuspended: admin peers lists active peers by default and hides suspended ones,
// like action list hides inactive; --all (?all=1) shows them.
func TestListPeersHidesSuspended(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	seedPeer(t, k, "peer-live")
	_, goneKey := seedPeer(t, k, "peer-gone")
	gone, _ := k.ReadUserByPublicKey(ctx, goneKey)
	if err := k.SuspendUser(ctx, sys.ID, gone.ID); err != nil {
		t.Fatal(err)
	}
	srv := &server{kernel: k, log: log.Discard()}

	def := listPeersResp(t, srv, false)
	if len(def) != 1 || def[0]["handle"] != "peer-live" {
		t.Fatalf("default peers should list only the active peer, got %v", def)
	}
	if all := listPeersResp(t, srv, true); len(all) != 2 {
		t.Fatalf("--all should list both peers, got %d", len(all))
	}
}

// TestInspectOfflineFriendedShowsLocalData: a peer we know locally that is offline still inspects —
// source=local + reachability=unreachable, not an opaque failure (§13). Under v0.13 the locally-held
// action view comes from the regenerable discovery cache (empty until a gossip pull), not the
// imported proxy rows; the essential guarantee is a graceful local degrade, not the action list.
func TestInspectOfflineFriendedShowsLocalData(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "peer-off")
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{reachPath: "unreachable"}}

	out := inspectResp(t, srv, handle)
	if out["source"] != "local" {
		t.Errorf("source = %v, want local", out["source"])
	}
	if out["online"] != false {
		t.Errorf("online = %v, want false", out["online"])
	}
}

// TestInspectLocalUserRejected: a handle that names a LOCAL account (no public key) is not a
// federation peer — inspect must reject it, not probe the handle as if it were a key. Regression:
// `admin inspect @chat` used to report a local user as an offline unknown peer.
func TestInspectLocalUserRejected(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "chat", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	// fed is present so the guard, not a missing transport, is what rejects.
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{reachPath: "direct"}}

	req := httptest.NewRequest("GET", "/control/peers/inspect?key="+url.QueryEscape("chat"), nil)
	rec := httptest.NewRecorder()
	srv.ctlInspectPeer(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("inspect @chat: status %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "local user") {
		t.Errorf("expected a 'local user, not a peer' message, got: %s", rec.Body.String())
	}

	// An @handle that names nothing at all is "no peer" (404), not a key to probe: a stranger is
	// inspected by key, never by an unfriended handle.
	req = httptest.NewRequest("GET", "/control/peers/inspect?key="+url.QueryEscape("nope"), nil)
	rec = httptest.NewRecorder()
	srv.ctlInspectPeer(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inspect @nope: status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestInspectOfflineStranger: an unreachable peer we never friended → empty view, source=none,
// never a hang or opaque error.
func TestInspectOfflineStranger(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{reachPath: "unreachable"}}

	out := inspectResp(t, srv, base64.RawURLEncoding.EncodeToString(pub))
	if out["source"] != "none" || out["online"] != false {
		t.Errorf("stranger offline: source=%v online=%v, want none/false", out["source"], out["online"])
	}
}

// TestInspectOnlineLive: a reachable peer yields live identity/actions with source=live.
func TestInspectOnlineLive(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	livekey := base64.RawURLEncoding.EncodeToString(pub)
	doc, _ := json.Marshal(kernel.GossipResponse{
		Handle: "live-peer", PublicKey: livekey,
		ActionManifests: []*kernel.ActionManifest{{ActionID: "a", Name: "x", OwnerHandle: "live-peer", Price: 3}},
	})
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{inspectDoc: doc, reachPath: "direct"}}

	out := inspectResp(t, srv, livekey)
	if out["source"] != "live" || out["online"] != true {
		t.Errorf("live: source=%v online=%v, want live/true", out["source"], out["online"])
	}
	if acts, _ := out["actions"].([]any); len(acts) != 1 {
		t.Errorf("live actions = %v, want 1", out["actions"])
	}
}

// TestInspectPersistsPeerSync: a live inspect of a friended peer persists the freshness it just
// fetched — last_seen and our credit there — so peer_state refreshes on demand rather than only on
// the 5-minute discovery timer (§13 peer sync). The stranger-live case (no proxy row) is covered by
// TestInspectOnlineLive, whose inspectDoc key has no user: RecordPeerSync no-ops without erroring.
func TestInspectPersistsPeerSync(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	handle, key := seedPeer(t, k, "peer-sync")

	// A freshly seeded peer has no sync cache yet (its proxies read offline).
	before, _ := k.ReadUserByPublicKey(ctx, key)
	if before == nil || before.PeerLastSeen != nil {
		t.Fatalf("seeded peer should start with no last_seen, got %+v", before)
	}

	bal := int64(777)
	doc, _ := json.Marshal(kernel.GossipResponse{Handle: handle, PublicKey: key, CounterpartyBalance: &bal})
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{inspectDoc: doc, reachPath: "direct"}}

	if out := inspectResp(t, srv, key); out["source"] != "live" {
		t.Fatalf("source = %v, want live", out["source"])
	}

	after, _ := k.ReadUserByPublicKey(ctx, key)
	if after == nil || after.PeerLastSeen == nil {
		t.Fatal("inspect must persist last_seen for a friended peer")
	}
	if after.PeerCredit == nil || *after.PeerCredit != bal {
		t.Fatalf("inspect must persist peer_credit; got %v, want %d", after.PeerCredit, bal)
	}
}

// ---- completePeerStep: the failure paths the flow cannot reach ----
//
// flow_fed_step_complete exercises this code when everything works. These three cover what happens
// when it does not, which on this path is what actually matters: whether a caller can retry
// safely, and whether it is told the truth about what the peer did with its money.

// peerStepServer builds a server with one seeded peer, returning its @handle.
func peerStepServer(t *testing.T, f *fakeFed) (*server, string) {
	t.Helper()
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "peer-steps")
	return &server{kernel: k, log: log.Discard(), fed: f}, handle
}

// A retry after a lost reply must present the SAME idempotency key, or the peer cannot recognise
// it as a duplicate: it re-executes, and the transaction and receipt of the first completion are
// unreachable. The key is derived from the request, so identical requests derive identical keys.
func TestCompletePeerStep_DerivesTheIdempotencyKey(t *testing.T) {
	f := &fakeFed{stepBody: json.RawMessage(`{"tx_id":"tx-9"}`), stepStatus: 200}
	srv, handle := peerStepServer(t, f)
	ctx := context.Background()

	key := func(stepID string, input string) string {
		t.Helper()
		if _, err := srv.completePeerStep(ctx, handle, stepID, json.RawMessage(input), "", ""); err != nil {
			t.Fatalf("completePeerStep: %v", err)
		}
		return f.lastStep.IdempotencyKey
	}

	first := key("s1", `{"ok":true}`)
	if first == "" {
		t.Fatal("no idempotency key was sent")
	}
	if retry := key("s1", `{"ok":true}`); retry != first {
		t.Errorf("a retry must reuse the key: %s vs %s", first, retry)
	}
	// Genuinely different requests must not collide with the stored result of the first.
	if other := key("s1", `{"ok":false}`); other == first {
		t.Error("different input must derive a different key")
	}
	if other := key("s2", `{"ok":true}`); other == first {
		t.Error("a different step must derive a different key")
	}
}

// The signature covers a hash of the input, so the bytes hashed must be the bytes the peer
// receives. Marshaling the outer StepRequest compacts and HTML-escapes an embedded RawMessage, so
// hashing a caller's raw body would sign bytes the peer never sees and every completion from a
// non-CLI client would fail verification. Driven with a pretty-printed body containing < and &.
func TestCompletePeerStep_SignsTheBytesItSends(t *testing.T) {
	f := &fakeFed{stepBody: json.RawMessage(`{"tx_id":"tx-9"}`), stepStatus: 200}
	srv, handle := peerStepServer(t, f)

	pretty := json.RawMessage("{\n  \"city\": \"Rio\",\n  \"note\": \"a<b&c\"\n}")
	if _, err := srv.completePeerStep(context.Background(), handle, "s1", pretty, "", ""); err != nil {
		t.Fatalf("completePeerStep: %v", err)
	}

	// Round-trip the request as the transport does, then verify against what came out the far side.
	wire, err := json.Marshal(f.lastStep)
	if err != nil {
		t.Fatal(err)
	}
	var received fed.StepRequest
	if err := json.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received.Input, f.lastStep.Input) {
		t.Fatalf("input is not a marshal fixed point:\n sent:     %s\n received: %s",
			f.lastStep.Input, received.Input)
	}
	peerKey, _ := srv.resolvePeerKey(context.Background(), handle)
	if err := kernel.VerifyStepSignature(received.Counterparty, received.StepID, received.Counterparty,
		peerKey, received.IdempotencyKey, received.Timestamp,
		sha256HexBytes(received.Input), received.Signature); err != nil {
		t.Errorf("signature must verify over the bytes the peer receives: %v", err)
	}
}

// Never-sent and may-have-run need opposite handling: the first is safe to retry, the second may
// already have committed a transaction on the peer. Reporting a may-have-run as "offline" tells an
// operator nothing happened while the caller has been charged.
func TestCompletePeerStep_DistinguishesNeverSentFromMayHaveRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fed       *fakeFed
		want      *kernel.KernelError
		forbidden string
	}{
		{"provably never sent", &fakeFed{}, kernel.ErrPeerUnreachable, ""},
		{"failed after dispatch", &fakeFed{stepMidStream: true}, kernel.ErrTimeout, "offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, handle := peerStepServer(t, tc.fed)
			_, err := srv.completePeerStep(context.Background(), handle, "s1", json.RawMessage(`{}`), "", "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.forbidden != "" && strings.Contains(err.Error(), tc.forbidden) {
				t.Errorf("a request that may have executed must not claim %q: %v", tc.forbidden, err)
			}
		})
	}
}
