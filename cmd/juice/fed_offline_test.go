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

// TestListPeersHidesSuspended: admin peers lists active peers by default and hides suspended ones,
// like action list hides inactive; --all (?all=1) shows them.
func TestListPeersHidesSuspended(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	seedPeer(t, k, "@peer-live")
	_, goneKey := seedPeer(t, k, "@peer-gone")
	gone, _ := k.ReadUserByPublicKey(ctx, goneKey)
	if err := k.SuspendUser(ctx, sys.ID, gone.ID); err != nil {
		t.Fatal(err)
	}
	srv := &server{kernel: k, log: log.Discard()}

	def := listPeersResp(t, srv, false)
	if len(def) != 1 || def[0]["handle"] != "@peer-live" {
		t.Fatalf("default peers should list only the active peer, got %v", def)
	}
	if all := listPeersResp(t, srv, true); len(all) != 2 {
		t.Fatalf("--all should list both peers, got %d", len(all))
	}
}

// TestInspectOfflineFriendedShowsLocalData: a friended peer that is offline still inspects — local
// last-known actions plus reachability=unreachable, not an opaque failure (§13).
func TestInspectOfflineFriendedShowsLocalData(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "@peer-off")
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
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "@chat", Password: "pw"}); err != nil {
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

// TestUnsubscribeLocalUserRejected: unsubscribe must never touch a local account (it has no
// imported catalog). A handle with no public key is rejected as not-a-peer.
func TestUnsubscribeLocalUserRejected(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "@chat", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	srv := &server{kernel: k, log: log.Discard()}

	body, _ := json.Marshal(map[string]string{"handle": "@chat"})
	req := httptest.NewRequest("POST", "/control/peers/unsubscribe", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxCallerID, sys.ID))
	rec := httptest.NewRecorder()
	srv.ctlUnsubscribePeer(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unsubscribe @chat: status %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "local user") {
		t.Errorf("expected a 'local user, not a peer' message, got: %s", rec.Body.String())
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
	handle, key := seedPeer(t, k, "@peer-sync")

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

// TestSubscribeOfflineClearError: subscribing to an unreachable peer fails clearly (not a hang),
// mentioning unreachable.
func TestSubscribeOfflineClearError(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{reachPath: "unreachable"}}

	body, _ := json.Marshal(map[string]string{"key": "some-offline-key"})
	req := httptest.NewRequest("POST", "/control/peers/subscribe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ctlSubscribePeer(rec, req)
	if rec.Code < 400 {
		t.Fatalf("subscribe offline: status %d, want an error", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unreachable") {
		t.Errorf("subscribe offline error should mention unreachable: %s", rec.Body.String())
	}
}

// TestUnsubscribeWorksWithNoTransport: unsubscribe is purely local — it works even with the
// transport absent (offline), proving it never depends on reaching the peer.
func TestUnsubscribeWorksWithNoTransport(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "@peer-unf")
	sys, _ := k.ReadUserByHandle(context.Background(), "@sys")
	srv := &server{kernel: k, log: log.Discard(), fed: nil} // transport down

	body, _ := json.Marshal(map[string]string{"handle": handle})
	req := httptest.NewRequest("POST", "/control/peers/unsubscribe", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxCallerID, sys.ID))
	rec := httptest.NewRecorder()
	srv.ctlUnsubscribePeer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unsubscribe with no transport: status %d: %s", rec.Code, rec.Body.String())
	}
}

// TestResubscribeReactivatesProxy: after unsubscribe deactivates a proxy, subscribing again must
// reactivate it. The re-subscribe sees a byte-identical manifest, so reconcileImport files it
// under Unchanged — which the import activation loop must still enable, or the proxy stays dead
// and uncallable (the bug this guards).
func TestResubscribeReactivatesProxy(t *testing.T) {
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

	// First subscribe: import and activate.
	if imp, _ := bulkImportPeerActionsFed(ctx, fetch, k, sys.ID, key, peer); imp != 1 {
		t.Fatalf("first import: got %d, want 1", imp)
	}
	owned, err := k.ListOwnedActions(ctx, peer.ID, 100, 0)
	if err != nil || len(owned) != 1 {
		t.Fatalf("owned after subscribe: %v (err %v)", owned, err)
	}
	proxy := owned[0]
	if !proxy.Active {
		t.Fatal("proxy should be active after first subscribe")
	}

	// Unsubscribe deactivates the proxy.
	if err := k.Unsubscribe(ctx, sys.ID, key); err != nil {
		t.Fatal(err)
	}

	// Re-subscribe with the identical manifest → Unchanged → must be reactivated.
	bulkImportPeerActionsFed(ctx, fetch, k, sys.ID, key, peer)
	owned, err = k.ListOwnedActions(ctx, peer.ID, 100, 0)
	if err != nil || len(owned) != 1 {
		t.Fatalf("owned after re-subscribe: %v (err %v)", owned, err)
	}
	if !owned[0].Active {
		t.Fatal("proxy should be reactivated after re-subscribe")
	}
}

// ---- Peer step commands (§13) ----

// The driving side signs with this kernel's platform key and forwards the peer's reply verbatim.
func TestPeerStepsListForwardsPeerReply(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, key := seedPeer(t, k, "@peer-steps")
	f := &fakeFed{stepBody: json.RawMessage(`{"steps":[{"id":"s1","action":"@sys/sink"}]}`), stepStatus: 200}
	srv := &server{kernel: k, log: log.Discard(), fed: f}

	req := httptest.NewRequest("GET", "/control/peers/steps?key="+url.QueryEscape(handle), nil)
	rec := httptest.NewRecorder()
	srv.ctlPeerSteps(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"s1"`) {
		t.Errorf("expected the peer's step list to be forwarded, got %s", rec.Body.String())
	}
	// The @handle was resolved to the peer's key, and the request is a signed list.
	if f.lastStep.Kind != "list" || f.lastStep.Signature == "" || f.lastStep.Counterparty == "" {
		t.Errorf("expected a signed list request, got %+v", f.lastStep)
	}
	_ = key
}

func TestPeerStepCompleteSignsExactInput(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "@peer-steps")
	f := &fakeFed{stepBody: json.RawMessage(`{"tx_id":"tx-9"}`), stepStatus: 200}
	srv := &server{kernel: k, log: log.Discard(), fed: f}

	body := `{"key":"` + handle + `","step_id":"s1","input":{"approve":true}}`
	req := httptest.NewRequest("POST", "/control/peers/steps/complete", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ctlCompletePeerStep(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if f.lastStep.Kind != "complete" || f.lastStep.StepID != "s1" {
		t.Fatalf("unexpected request %+v", f.lastStep)
	}
	if f.lastStep.IdempotencyKey == "" {
		t.Error("expected an idempotency key so a retry is safe")
	}
	// The signature must cover the exact bytes sent, so the peer's input_hash check matches.
	peerKey, _ := srv.resolvePeerKey(context.Background(), handle)
	if err := kernel.VerifyStepSignature(f.lastStep.Counterparty, f.lastStep.StepID, f.lastStep.Counterparty,
		peerKey, f.lastStep.IdempotencyKey, f.lastStep.Timestamp, sha256HexBytes(f.lastStep.Input), f.lastStep.Signature); err != nil {
		t.Errorf("signature must verify over the exact input bytes: %v", err)
	}
}

// An offline peer fails promptly with the typed peer error, not an opaque 500 (§13).
func TestPeerStepsOfflineIsPeerUnreachable(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "@peer-off")
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{}}

	req := httptest.NewRequest("GET", "/control/peers/steps?key="+url.QueryEscape(handle), nil)
	rec := httptest.NewRecorder()
	srv.ctlPeerSteps(rec, req)
	if rec.Code != kernel.ErrPeerUnreachable.HTTP {
		t.Errorf("status %d, want %d; body=%s", rec.Code, kernel.ErrPeerUnreachable.HTTP, rec.Body.String())
	}
}

// A peer's typed rejection survives the hop: an already-claimed step reads as invalid_state here,
// not as a generic failure.
func TestPeerStepCompletePropagatesPeerErrorCode(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "@peer-steps")
	f := &fakeFed{stepBody: json.RawMessage(`{"error":"step is not waiting","code":"invalid_state"}`), stepStatus: 409}
	srv := &server{kernel: k, log: log.Discard(), fed: f}

	body := `{"key":"` + handle + `","step_id":"s1"}`
	req := httptest.NewRequest("POST", "/control/peers/steps/complete", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ctlCompletePeerStep(rec, req)
	if rec.Code != kernel.ErrInvalidState.HTTP {
		t.Errorf("status %d, want %d; body=%s", rec.Code, kernel.ErrInvalidState.HTTP, rec.Body.String())
	}
}

// A local (non-peer) account is not a federation counterparty.
func TestPeerStepsRejectsLocalUser(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	if _, err := k.CreateUser(context.Background(), kernel.CreateUserRequest{Handle: "@localu", Password: "pw12345678"}); err != nil {
		t.Fatal(err)
	}
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{stepBody: json.RawMessage(`{}`), stepStatus: 200}}

	req := httptest.NewRequest("GET", "/control/peers/steps?key="+url.QueryEscape("@localu"), nil)
	rec := httptest.NewRecorder()
	srv.ctlPeerSteps(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// Regression: input_hash must cover the bytes the transport actually sends. Marshaling the outer
// StepRequest compacts and HTML-escapes an embedded RawMessage, so hashing a caller's raw body
// would sign bytes the peer never sees — and every non-CLI client would be permanently unable to
// complete a step. Drives the handler with a pretty-printed body, the case the CLI never produces.
func TestPeerStepCompleteNormalizesInputBeforeHashing(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "@peer-steps")
	f := &fakeFed{stepBody: json.RawMessage(`{"tx_id":"tx-9"}`), stepStatus: 200}
	srv := &server{kernel: k, log: log.Discard(), fed: f}

	pretty := "{\n  \"city\": \"Rio\",\n  \"note\": \"a<b&c\"\n}"
	body := `{"key":"` + handle + `","step_id":"s1","input":` + pretty + `}`
	req := httptest.NewRequest("POST", "/control/peers/steps/complete", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ctlCompletePeerStep(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// What the peer receives after the transport re-encodes the request must hash to what was
	// signed — i.e. the sent Input must already be a marshal fixed point.
	wire, err := json.Marshal(f.lastStep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded fed.StepRequest
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Input, f.lastStep.Input) {
		t.Fatalf("input is not a marshal fixed point:\n sent:     %s\n received: %s", f.lastStep.Input, decoded.Input)
	}
	peerKey, _ := srv.resolvePeerKey(context.Background(), handle)
	if err := kernel.VerifyStepSignature(decoded.Counterparty, decoded.StepID, decoded.Counterparty,
		peerKey, decoded.IdempotencyKey, decoded.Timestamp, sha256HexBytes(decoded.Input), decoded.Signature); err != nil {
		t.Errorf("signature must verify over the bytes the peer receives: %v", err)
	}
}

// The idempotency key must be derived, not minted per attempt: a retry after a timeout has to
// present the SAME key or the peer cannot recognize it as a duplicate, and an already-executed
// completion's tx_id and receipt are lost.
func TestPeerStepCompleteIdempotencyKeyIsDerived(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "@peer-steps")
	f := &fakeFed{stepBody: json.RawMessage(`{"tx_id":"tx-9"}`), stepStatus: 200}
	srv := &server{kernel: k, log: log.Discard(), fed: f}

	post := func(bodyJSON string) string {
		t.Helper()
		req := httptest.NewRequest("POST", "/control/peers/steps/complete", strings.NewReader(bodyJSON))
		rec := httptest.NewRecorder()
		srv.ctlCompletePeerStep(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		return f.lastStep.IdempotencyKey
	}

	same := `{"key":"` + handle + `","step_id":"s1","input":{"ok":true}}`
	first, second := post(same), post(same)
	if first != second {
		t.Errorf("a retry must reuse the key: %s vs %s", first, second)
	}
	// Different input is a different request and must not collide with the stored result.
	if other := post(`{"key":"` + handle + `","step_id":"s1","input":{"ok":false}}`); other == first {
		t.Error("different input must derive a different key")
	}
	if other := post(`{"key":"` + handle + `","step_id":"s2","input":{"ok":true}}`); other == first {
		t.Error("a different step must derive a different key")
	}
}

// A failure that may have executed remotely must not be reported as "peer offline" — only a
// provably-never-dispatched request is unreachable (§13).
func TestPeerStepCompleteMidStreamFailureIsNotUnreachable(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	handle, _ := seedPeer(t, k, "@peer-steps")
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{stepMidStream: true}}

	body := `{"key":"` + handle + `","step_id":"s1"}`
	req := httptest.NewRequest("POST", "/control/peers/steps/complete", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ctlCompletePeerStep(rec, req)
	if rec.Code != kernel.ErrTimeout.HTTP {
		t.Errorf("status %d, want %d (timeout, may have executed); body=%s",
			rec.Code, kernel.ErrTimeout.HTTP, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "offline") {
		t.Errorf("a mid-stream failure must not claim the peer is offline: %s", rec.Body.String())
	}
}
