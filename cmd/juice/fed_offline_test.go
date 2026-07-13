package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	inspectDoc json.RawMessage // non-nil → Inspect/Gossip succeed with this; nil → they fail (offline)
	reachPath  string          // "direct" | "relayed" | "unreachable" (default unreachable)
}

func (f *fakeFed) Inspect(context.Context, string) (json.RawMessage, error) {
	if f.inspectDoc == nil {
		return nil, errors.New("fed: cannot resolve peer (offline)")
	}
	return f.inspectDoc, nil
}
func (f *fakeFed) Gossip(ctx context.Context, s string) (json.RawMessage, error) {
	return f.Inspect(ctx, s)
}
func (f *fakeFed) Manifests(context.Context, string) ([]json.RawMessage, error) { return nil, nil }
func (f *fakeFed) Friend(context.Context, string, fed.FriendRequest) (fed.FriendResponse, error) {
	return fed.FriendResponse{}, nil
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

// seedFriendedPeer creates a proxy peer with one active+public imported proxy action, returning its
// @handle and base64url key — the local state that offline inspect should surface.
func seedFriendedPeer(t *testing.T, k *kernel.Kernel, handle string) (string, string) {
	t.Helper()
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)
	sys, err := k.ReadUserByHandle(ctx, "@sys")
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
	mBytes, _ := json.Marshal(m)
	if imp, _ := bulkImportPeerActionsFed(ctx, &fakeManifestFetcher{frames: []json.RawMessage{mBytes}}, k, sys.ID, key, peer); imp != 1 {
		t.Fatalf("seed import: got %d, want 1", imp)
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

// TestListPeersHidesDenied: admin peers lists friended peers by default and hides denied
// (unfriended) ones, like action list hides inactive; --all (?all=1) shows them.
func TestListPeersHidesDenied(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	seedFriendedPeer(t, k, "@peer-live")
	denied, _ := seedFriendedPeer(t, k, "@peer-gone")
	if err := k.DenyPeer(ctx, sys.ID, denied); err != nil {
		t.Fatal(err)
	}
	srv := &server{kernel: k, log: log.Discard()}

	def := listPeersResp(t, srv, false)
	if len(def) != 1 || def[0]["handle"] != "@peer-live" {
		t.Fatalf("default peers should list only the friended peer, got %v", def)
	}
	if all := listPeersResp(t, srv, true); len(all) != 2 {
		t.Fatalf("--all should list both peers, got %d", len(all))
	}
}

// TestInspectOfflineFriendedShowsLocalData: a friended peer that is offline still inspects — local
// last-known actions plus reachability=unreachable, not an opaque failure (§13).
func TestInspectOfflineFriendedShowsLocalData(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedFriendedPeer(t, k, "@peer-off")
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{reachPath: "unreachable"}}

	out := inspectResp(t, srv, handle)
	if out["source"] != "local" {
		t.Errorf("source = %v, want local", out["source"])
	}
	if out["online"] != false {
		t.Errorf("online = %v, want false", out["online"])
	}
	if acts, _ := out["actions"].([]any); len(acts) != 1 {
		t.Errorf("actions = %v, want 1 (last-imported)", out["actions"])
	}
}

// TestInspectLocalUserRejected: a handle that names a LOCAL account (no public key) is not a
// federation peer — inspect must reject it, not probe the handle as if it were a key. Regression:
// `admin inspect @chat` used to report a local user as an offline unknown peer.
func TestInspectLocalUserRejected(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "@chat", Email: "c@x.local", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	// fed is present so the guard, not a missing transport, is what rejects.
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{reachPath: "direct"}}

	req := httptest.NewRequest("GET", "/control/peers/inspect?key="+url.QueryEscape("@chat"), nil)
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
	req = httptest.NewRequest("GET", "/control/peers/inspect?key="+url.QueryEscape("@nope"), nil)
	rec = httptest.NewRecorder()
	srv.ctlInspectPeer(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inspect @nope: status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUnfriendLocalUserRejected: unfriend must never deny-list a local account (which would block
// it). A handle with no public key is rejected and its denied_at stays nil.
func TestUnfriendLocalUserRejected(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "@chat", Email: "c@x.local", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	srv := &server{kernel: k, log: log.Discard()}

	body, _ := json.Marshal(map[string]string{"handle": "@chat"})
	req := httptest.NewRequest("POST", "/control/peers/unfriend", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxCallerID, sys.ID))
	rec := httptest.NewRecorder()
	srv.ctlUnfriendPeer(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unfriend @chat: status %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := k.ReadUser(ctx, u.ID); got.DeniedAt != nil {
		t.Error("a local user must not be deny-listed by unfriend")
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
	doc, _ := json.Marshal(kernel.GossipResponse{
		Handle: "@live-peer", PublicKey: "livekey",
		Actions: []kernel.GossipAction{{ActionID: "a", Name: "@live-peer/x", Price: 3}},
	})
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{inspectDoc: doc, reachPath: "direct"}}

	out := inspectResp(t, srv, "livekey")
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
	handle, key := seedFriendedPeer(t, k, "@peer-sync")

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

// TestFriendOfflineClearError: friending an unreachable peer fails clearly (not a hang), mentioning
// unreachable.
func TestFriendOfflineClearError(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{reachPath: "unreachable"}}

	body, _ := json.Marshal(map[string]string{"key": "some-offline-key"})
	req := httptest.NewRequest("POST", "/control/peers/friend", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ctlFriendPeer(rec, req)
	if rec.Code < 400 {
		t.Fatalf("friend offline: status %d, want an error", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unreachable") {
		t.Errorf("friend offline error should mention unreachable: %s", rec.Body.String())
	}
}

// TestUnfriendWorksWithNoTransport: unfriend is purely local — it works even with the transport
// absent (offline), proving it never depends on reaching the peer.
func TestUnfriendWorksWithNoTransport(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedFriendedPeer(t, k, "@peer-unf")
	sys, _ := k.ReadUserByHandle(context.Background(), "@sys")
	srv := &server{kernel: k, log: log.Discard(), fed: nil} // transport down

	body, _ := json.Marshal(map[string]string{"handle": handle})
	req := httptest.NewRequest("POST", "/control/peers/unfriend", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxCallerID, sys.ID))
	rec := httptest.NewRecorder()
	srv.ctlUnfriendPeer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unfriend with no transport: status %d: %s", rec.Code, rec.Body.String())
	}
}

// TestFriendClearsDenial: an explicit admin friend of a previously-unfriended (denied) peer must
// clear denied_at, or the peer stays [denied] — hidden restore, inbound calls still rejected.
func TestFriendClearsDenial(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}

	// Seed a peer, then deny it exactly as unfriend does.
	handle, key := seedFriendedPeer(t, k, "@peer-den")
	if err := k.DenyPeer(ctx, sys.ID, handle); err != nil {
		t.Fatal(err)
	}
	if before, _ := k.ReadUserByPublicKey(ctx, key); before == nil || before.DeniedAt == nil {
		t.Fatal("peer should be denied after DenyPeer")
	}

	// Drive the outbound friend handler with a gossip doc matching the peer key.
	gdoc, _ := json.Marshal(kernel.GossipResponse{PublicKey: key, Handle: handle})
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{inspectDoc: gdoc, reachPath: "direct"}}
	body, _ := json.Marshal(map[string]string{"key": key})
	req := httptest.NewRequest("POST", "/control/peers/friend", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxCallerID, sys.ID))
	rec := httptest.NewRecorder()
	srv.ctlFriendPeer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("friend: status %d: %s", rec.Code, rec.Body.String())
	}

	after, _ := k.ReadUserByPublicKey(ctx, key)
	if after == nil {
		t.Fatal("peer vanished after friend")
	}
	if after.DeniedAt != nil {
		t.Fatalf("friend must clear denial; DeniedAt=%v", *after.DeniedAt)
	}
}

// TestRefriendReactivatesProxy: after unfriend deactivates a proxy, friending again must
// reactivate it. The re-friend sees a byte-identical manifest, so reconcileImport files it
// under Unchanged — which the friend activation loop must still enable, or the proxy stays
// dead and uncallable (the bug this guards).
func TestRefriendReactivatesProxy(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := k.AddPeer(ctx, sys.ID, "@peer-rf", key)
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID: "act-1", OwnerHandle: "@peer-rf", Name: "greet", Description: "greet",
		Kind: kernel.KindHTTP, Price: 5, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "sha256-x", Stats: &kernel.Stats{},
		UpdatedAt: time.Now(),
	}
	sig, _ := kernel.SignManifest(priv, &m)
	m.Signature = sig
	mBytes, _ := json.Marshal(m)
	fetch := &fakeManifestFetcher{frames: []json.RawMessage{mBytes}}

	// First friend: import and activate.
	if imp, _ := bulkImportPeerActionsFed(ctx, fetch, k, sys.ID, key, peer); imp != 1 {
		t.Fatalf("first import: got %d, want 1", imp)
	}
	owned, err := k.ListOwnedActions(ctx, peer.ID, 100, 0)
	if err != nil || len(owned) != 1 {
		t.Fatalf("owned after friend: %v (err %v)", owned, err)
	}
	proxy := owned[0]
	if !proxy.Active {
		t.Fatal("proxy should be active after first friend")
	}

	// Unfriend cascade deactivates the proxy.
	if err := k.SetActive(ctx, sys.ID, proxy.ID, false); err != nil {
		t.Fatal(err)
	}

	// Re-friend with the identical manifest → Unchanged → must be reactivated.
	bulkImportPeerActionsFed(ctx, fetch, k, sys.ID, key, peer)
	owned, err = k.ListOwnedActions(ctx, peer.ID, 100, 0)
	if err != nil || len(owned) != 1 {
		t.Fatalf("owned after re-friend: %v (err %v)", owned, err)
	}
	if !owned[0].Active {
		t.Fatal("proxy should be reactivated after re-friend")
	}
}
