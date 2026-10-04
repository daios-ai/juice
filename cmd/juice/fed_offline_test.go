// SPDX-License-Identifier: AGPL-3.0-only

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
	"github.com/go-chi/chi/v5"
)

// fakeFed is a fedClient whose reachability and live-fetch outcome are controlled per test, so the
// admin handlers' online/offline paths are exercisable without a real network.
type fakeFed struct {
	inspectDoc    json.RawMessage // non-nil → Inspect/Gossip succeed with this; nil → they fail (offline)
	reachPath     string          // "direct" | "relayed" | "unreachable" (default unreachable)
	taskBody      json.RawMessage // non-nil → Task succeeds with this; nil → offline
	taskStatus    int             // status Task returns alongside taskBody
	taskMidStream bool            // Task fails after dispatch (may have executed remotely)
	lastTask      fed.TaskRequest // the last outbound task request, for assertions
	addrs         []string        // what ListenAddrs reports, for the handlers that publish them
}

func (f *fakeFed) Gossip(_ context.Context, _ string, _ fed.GossipRequest) (json.RawMessage, error) {
	if f.inspectDoc == nil {
		return nil, errors.New("fed: cannot resolve peer (offline)")
	}
	return f.inspectDoc, nil
}
func (f *fakeFed) Call(_ context.Context, _ string, _ fed.CallRequest) (fed.CallResponse, error) {
	return fed.CallResponse{}, errors.New("fed: call not used in these tests")
}
func (f *fakeFed) Resolve(_ context.Context, _ string, _ fed.ResolveRequest) (fed.ResolveResponse, error) {
	return fed.ResolveResponse{}, errors.New("fed: resolve not used in these tests")
}
func (f *fakeFed) Reveal(_ context.Context, _ string, _ fed.RevealRequest) (fed.RevealResponse, error) {
	return fed.RevealResponse{}, errors.New("fed: reveal not used in these tests")
}
func (f *fakeFed) Transfer(_ context.Context, _ string, _ fed.TransferRequest) (fed.TransferResponse, error) {
	return fed.TransferResponse{}, errors.New("fed: transfer not used in these tests")
}
func (f *fakeFed) Task(_ context.Context, _ string, req fed.TaskRequest) (fed.TaskResponse, error) {
	f.lastTask = req
	// taskMidStream models a failure AFTER bytes may have reached the peer (a stream error, or a
	// timeout while it runs the resumed call) — distinct from an unresolvable peer, which provably
	// never sent anything. Only the latter is ErrNotDispatched (§13).
	if f.taskMidStream {
		return fed.TaskResponse{}, errors.New("fed: stream closed mid-request")
	}
	if f.taskBody == nil {
		return fed.TaskResponse{}, fmt.Errorf("%w: cannot resolve peer (offline)", fed.ErrNotDispatched)
	}
	return fed.TaskResponse{Status: f.taskStatus, Body: f.taskBody}, nil
}
func (f *fakeFed) Probe(context.Context, string) fed.Reachability {
	p := f.reachPath
	if p == "" {
		p = "unreachable"
	}
	return fed.Reachability{Path: p}
}
func (f *fakeFed) ListenAddrs() []string { return f.addrs }
func (f *fakeFed) Close() error          { return nil }

// seedPeer creates a proxy peer with one active+public imported proxy action, returning its
// @handle and base64url key — the local state that offline inspect should surface.
func seedPeer(t *testing.T, k *kernel.Kernel, handle string) (string, string) {
	t.Helper()
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := k.EnsureKernelAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	// The outbound-use path also binds the local petname (§13), which is how the peer is
	// addressable by name in the commands below.
	if _, err := k.BindPetname(ctx, key, handle, false); err != nil {
		t.Fatal(err)
	}
	m := kernel.ActionManifest{
		ActionID: "act-1", OwnerHandle: handle, Title: "Test action", Name: "greet", Description: "greet",
		Kind: kernel.KindHTTP, Price: 5, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "sha256-x",
		UpdatedAt: time.Now(),
	}
	sig, _ := testNet.SignManifest(priv, &m)
	m.Signature = sig
	// Cold resolve caches and activates the proxy (§8): the sole import path.
	if _, err := k.ImportPeerAction(ctx, peer.ID, m); err != nil {
		t.Fatalf("seed import: %v", err)
	}
	return handle, key
}

// inspectReq builds the request the router would deliver: the target rides on the route, so a
// test calling the handler directly has to put it there too.
func inspectReq(ident string) *http.Request {
	req := httptest.NewRequest("GET", "/v1/admin/peers/"+url.PathEscape(ident)+"/inspect", nil)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("target", ident)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
}

func inspectResp(t *testing.T, srv *server, ident string) map[string]any {
	t.Helper()
	req := inspectReq(ident)
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
	path := "/v1/admin/peers"
	if all {
		path += "?all=1"
	}
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	srv.ctlListPeers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("peers status %d: %s", rec.Code, rec.Body.String())
	}
	var peers []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &peers); err != nil {
		t.Fatal(err)
	}
	return peers
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
	gone, _ := k.ReadAccountByKernelKey(ctx, goneKey)
	if err := k.SuspendUser(ctx, sys.ID, gone.ID); err != nil {
		t.Fatal(err)
	}
	srv := &server{kernel: k, log: log.Discard()}

	def := listPeersResp(t, srv, false)
	if len(def) != 1 || def[0]["petname"] != "peer-live" {
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

// TestInspectCatalogIsOneShapeAndPrice: inspect answers from a live gossip pull or, when the peer is
// down, from the discovery cache — and an operator must not have to know which. Both branches carry
// the same fields and the same price, the indicative local all-in (serving price + import fee) that
// sys/lookup shows. Two failures this pins: the cached ServingPrice is untagged, so serializing docs
// directly reported every action free; and the live branch's raw manifest price is the seller's
// base, which is a different number from the cached one under the same key.
func TestInspectCatalogIsOneShapeAndPrice(t *testing.T) {
	k, db := newRemoteTestKernel(t)
	ctx := context.Background()
	handle, key := seedPeer(t, k, "peer-priced")

	// mp=20 at the default 500 bps serving markup → serving 21; +500 bps import → 23 all-in.
	const mp, wantAllIn = int64(20), float64(23)
	rbps := kernel.DefaultEconomy().RemoteBPS
	m := kernel.ActionManifest{
		ActionID: "act-1", OwnerHandle: handle, Title: "Test action", Name: "greet", Description: "greet",
		Kind: kernel.KindHTTP, Price: mp, RemoteBPS: rbps,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	}
	doc, _ := json.Marshal(kernel.GossipResponse{
		Handle: handle, PublicKey: key, ActionManifests: []*kernel.ActionManifest{&m},
	})
	if err := db.ApplyCatalogPage(ctx, key, []*kernel.DiscoveryDoc{{
		KernelPublicKey: key, ActionID: "act-1", Name: "greet",
		Description: "greet", ServingPrice: 21, ObservedAt: time.Now().UTC(),
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	}}, "", 0); err != nil {
		t.Fatal(err)
	}

	read := func(t *testing.T, f *fakeFed, wantSource string) map[string]any {
		t.Helper()
		out := inspectResp(t, &server{kernel: k, log: log.Discard(), fed: f}, handle)
		if out["source"] != wantSource {
			t.Fatalf("source = %v, want %v", out["source"], wantSource)
		}
		acts, ok := out["actions"].([]any)
		if !ok || len(acts) != 1 {
			t.Fatalf("%s: actions = %v, want exactly one", wantSource, out["actions"])
		}
		a, _ := acts[0].(map[string]any)
		return a
	}

	live := read(t, &fakeFed{inspectDoc: doc, reachPath: "direct"}, "live")
	cached := read(t, &fakeFed{reachPath: "unreachable"}, "local")

	for _, c := range []struct {
		name string
		a    map[string]any
	}{{"live", live}, {"cached", cached}} {
		if c.a["price"] != wantAllIn {
			t.Errorf("%s price = %v, want the indicative all-in %v", c.name, c.a["price"], wantAllIn)
		}
		if c.a["indicative"] != true {
			t.Errorf("%s price must be marked indicative, got %v", c.name, c.a["indicative"])
		}
		if name, _ := c.a["name"].(string); c.a["action_id"] != "act-1" || !strings.HasSuffix(name, "@peer-priced/greet") {
			t.Errorf("%s projection lost its identity fields: %v", c.name, c.a)
		}
	}
	// One schema, not merely one price: the same key set from both branches.
	if len(live) != len(cached) {
		t.Errorf("live and cached projections differ in shape: %v vs %v", live, cached)
	}

	// A peer that answers and offers nothing is not a peer we could not reach. The gossip producer
	// leaves an empty manifest slice nil and the field is omitempty, so treating emptiness as "no
	// live data" would answer a retired catalog with whatever the cache still holds, labelled live.
	t.Run("live peer with no actions does not fall back to cache", func(t *testing.T) {
		empty, _ := json.Marshal(kernel.GossipResponse{Handle: handle, PublicKey: key})
		out := inspectResp(t, &server{kernel: k, log: log.Discard(),
			fed: &fakeFed{inspectDoc: empty, reachPath: "direct"}}, handle)
		if out["source"] != "live" {
			t.Fatalf("source = %v, want live", out["source"])
		}
		if acts, _ := out["actions"].([]any); len(acts) != 0 {
			t.Errorf("a live peer offering nothing must report no actions, got %v", acts)
		}
	})
}

// TestInspectLocalUserRejected: a handle that names a LOCAL account (no public key) is not a
// federation peer — inspect must reject it, not probe the handle as if it were a key. Regression:
// `admin inspect @chat` used to report a local user as an offline unknown peer.
func TestInspectLocalUserRejected(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "chat@k", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	// fed is present so the guard, not a missing transport, is what rejects.
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{reachPath: "direct"}}

	req := inspectReq("chat")
	rec := httptest.NewRecorder()
	srv.ctlInspectPeer(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inspect chat: status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no peer") {
		t.Errorf("expected a 'local user, not a peer' message, got: %s", rec.Body.String())
	}

	// An @handle that names nothing at all is "no peer" (404), not a key to probe: a stranger is
	// inspected by key, never by an unfriended handle.
	req = inspectReq("nope")
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

// TestInspectWritesNothing: inspect is a diagnostic read and writes nothing (§14), so even a live
// pull that observes the peer's freshness leaves the sync cache alone — the discovery loop owns it.
// Otherwise retention and display state would depend on being looked at, and a GET would carry
// durable side effects.
func TestInspectWritesNothing(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	handle, key := seedPeer(t, k, "peer-sync")

	// A freshly seeded peer has no sync cache yet (its proxies read offline).
	before, _ := k.ReadKernel(ctx, key)
	if before == nil || before.LastSeen != nil {
		t.Fatalf("seeded peer should start with no last_seen, got %+v", before)
	}

	doc, _ := json.Marshal(kernel.GossipResponse{Handle: handle, PublicKey: key})
	srv := &server{kernel: k, log: log.Discard(), fed: &fakeFed{inspectDoc: doc, reachPath: "direct"}}

	// The response itself is live: what must not happen is persistence of what it saw.
	if out := inspectResp(t, srv, key); out["source"] != "live" {
		t.Fatalf("source = %v, want live", out["source"])
	}

	after, _ := k.ReadKernel(ctx, key)
	if after == nil {
		t.Fatal("the peer row must survive an inspect")
	}
	if after.LastSeen != nil {
		t.Errorf("inspect must not persist last_seen, got %v", after.LastSeen)
	}
}

// ---- completing a task held by a peer: the failure paths the flow cannot reach ----
//
// flow_fed_task_complete exercises this code when everything works. These cover what happens when
// it does not, which on this path is what actually matters: whether a caller can retry safely, and
// whether it is told the truth about what the peer did with its money.

// peerTaskServer builds a server with one seeded peer and one local user, returning the peer's key
// and a function that delivers a task held by the peer to that user, as its notice would (P8).
func peerTaskServer(t *testing.T, f *fakeFed) (*server, string, func(taskID string) string) {
	t.Helper()
	k, db := newRemoteTestKernel(t)
	_, key := seedPeer(t, k, "peer-tasks")
	user, err := k.CreateUser(context.Background(), kernel.CreateUserRequest{Handle: "tasker@" + k.OwnName(context.Background()), Password: "password1"})
	if err != nil {
		t.Fatal(err)
	}
	// The outbound task protocol runs kernel-side over kernel.TaskCaller (P8), so the fake backs a
	// real fedAdapter: these tests exercise the whole path, not a stub of it.
	self, _ := k.GetConfig(context.Background(), configKeySigningPublic)
	adapter := newFedAdapter(self, nil, nil)
	adapter.SetTransport(f)
	k.SetFederation(adapter)
	deliver := func(taskID string) string {
		t.Helper()
		if err := db.RecordTaskNotice(context.Background(), key, user.ID, &kernel.TaskNotice{
			ID: taskID, Revision: 1, Status: kernel.TaskWaiting, UserID: user.ID, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		return user.ID
	}
	return &server{kernel: k, log: log.Discard(), fed: f}, key, deliver
}

// A retry after a lost reply must present the SAME idempotency key, or the peer cannot recognise
// it as a duplicate: it re-executes, and the transaction and receipt of the first completion are
// unreachable. The key is derived from the request, so identical requests derive identical keys.
func TestCompletePeerTask_DerivesTheIdempotencyKey(t *testing.T) {
	f := &fakeFed{taskBody: json.RawMessage(`{"tx_id":"tx-9"}`), taskStatus: 200}
	srv, _, deliver := peerTaskServer(t, f)
	ctx := context.Background()

	sentKey := func(taskID string, input string) string {
		t.Helper()
		_, _ = srv.kernel.CompleteTask(ctx, deliver(taskID), taskID, json.RawMessage(input)) // the fake's reply carries no notice
		return f.lastTask.IdempotencyKey
	}

	first := sentKey("s1", `{"ok":true}`)
	if first == "" {
		t.Fatal("no idempotency key was sent")
	}
	if retry := sentKey("s1", `{"ok":true}`); retry != first {
		t.Errorf("a retry must reuse the key: %s vs %s", first, retry)
	}
	// Genuinely different requests must not collide with the stored result of the first.
	if other := sentKey("s1", `{"ok":false}`); other == first {
		t.Error("different input must derive a different key")
	}
	if other := sentKey("s2", `{"ok":true}`); other == first {
		t.Error("a different task must derive a different key")
	}
}

// The signature covers a hash of the input, so the bytes hashed must be the bytes the peer
// receives. Marshaling the outer TaskRequest compacts and HTML-escapes an embedded RawMessage, so
// hashing a caller's raw body would sign bytes the peer never sees and every completion from a
// non-CLI client would fail verification. Driven with a pretty-printed body containing < and &.
func TestCompletePeerTask_SignsTheBytesItSends(t *testing.T) {
	f := &fakeFed{taskBody: json.RawMessage(`{"tx_id":"tx-9"}`), taskStatus: 200}
	srv, key, deliver := peerTaskServer(t, f)

	pretty := json.RawMessage("{\n  \"city\": \"Rio\",\n  \"note\": \"a<b&c\"\n}")
	_, _ = srv.kernel.CompleteTask(context.Background(), deliver("s1"), "s1", pretty) // the fake's reply carries no notice

	// Round-trip the request as the transport does, then verify against what came out the far side.
	wire, err := json.Marshal(f.lastTask)
	if err != nil {
		t.Fatal(err)
	}
	var received fed.TaskRequest
	if err := json.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received.Input, f.lastTask.Input) {
		t.Fatalf("input is not a marshal fixed point:\n sent:     %s\n received: %s",
			f.lastTask.Input, received.Input)
	}
	if err := testNet.VerifyTaskSignature("complete", received.TaskID, received.Counterparty,
		key, received.IdempotencyKey, received.Timestamp,
		sha256HexBytes(received.Input), received.ForUserID, received.UserSuperuser, received.Signature); err != nil {
		t.Errorf("signature must verify over the bytes the peer receives: %v", err)
	}
}

// A completion is reported as a success only once the entry here says so (P8): a reply without the
// task's notice is the holder's work unrecorded here, reported with its transaction; and a holder this
// kernel has suspended is sent nothing, since its reply could not be recorded after the work ran.
func TestCompletePeerTask_SucceedsOnlyWhenRecorded(t *testing.T) {
	f := &fakeFed{taskBody: json.RawMessage(`{"tx_id":"tx-9"}`), taskStatus: 200}
	srv, key, deliver := peerTaskServer(t, f)
	ctx := context.Background()
	user := deliver("s1")
	_, err := srv.kernel.CompleteTask(ctx, user, "s1", json.RawMessage(`{}`))
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || !errors.Is(err, kernel.ErrExecutionFailed) || ke.Meta["tx_id"] != "tx-9" {
		t.Fatalf("a reply without a notice: %v, want an execution failure naming tx-9", err)
	}
	sys, _ := srv.kernel.ReadUserByHandle(ctx, "sys")
	if err := srv.kernel.SuspendKernel(ctx, sys.ID, key); err != nil {
		t.Fatal(err)
	}
	f.lastTask = fed.TaskRequest{}
	if _, err := srv.kernel.CancelTask(ctx, user, "s1"); !errors.Is(err, kernel.ErrUnauthorized) || f.lastTask.Kind != "" {
		t.Errorf("declining at a suspended holder: %v, sent %q; want refused, nothing sent", err, f.lastTask.Kind)
	}
}

// Never-sent and may-have-run need opposite handling: the first is safe to retry, the second may
// already have committed a transaction on the peer. Reporting a may-have-run as "offline" tells an
// operator nothing happened while the caller has been charged.
func TestCompletePeerTask_DistinguishesNeverSentFromMayHaveRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fed       *fakeFed
		want      *kernel.KernelError
		forbidden string
	}{
		{"provably never sent", &fakeFed{}, kernel.ErrPeerUnreachable, ""},
		{"failed after dispatch", &fakeFed{taskMidStream: true}, kernel.ErrTimeout, "offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, key, deliver := peerTaskServer(t, tc.fed)
			_, err := srv.kernel.CompleteTask(context.Background(), deliver("s1"), "s1", json.RawMessage(`{}`))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.forbidden != "" && strings.Contains(err.Error(), tc.forbidden) {
				t.Errorf("a request that may have executed must not claim %q: %v", tc.forbidden, err)
			}
			// Either way the error names the peer, so a client never has to infer who failed from
			// what the user typed (§14).
			var ke *kernel.KernelError
			if !errors.As(err, &ke) || ke.Meta["peer"] != key {
				t.Errorf("Meta[peer] = %q, want the peer key %q", ke.Meta["peer"], key)
			}
		})
	}
}

// A peer this kernel cannot reach is named by every path that fails to reach it, not just the call
// path: a cold resolve is where a first call to an offline peer actually fails (§13), and naming it
// is the difference between "something is offline" and a peer the operator can act on.
func TestResolveNamesTheUnreachablePeer(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	self, _ := k.GetConfig(context.Background(), configKeySigningPublic)
	adapter := newFedAdapter(self, nil, nil)
	const key = "k-offline-peer"

	// No transport at all, and a transport that cannot reach the peer, are both "unreachable".
	if _, err := adapter.ResolveRemoteAction(context.Background(), key, "bob", "greet"); !named(err, key) {
		t.Errorf("resolve with no transport: %v, want ErrPeerUnreachable naming %s", err, key)
	}
	adapter.SetTransport(&fakeFed{})
	if _, err := adapter.ResolveRemoteAction(context.Background(), key, "bob", "greet"); !named(err, key) {
		t.Errorf("resolve of an offline peer: %v, want ErrPeerUnreachable naming %s", err, key)
	}
	if _, err := adapter.ResolveRemoteUser(context.Background(), key, "bob"); !named(err, key) {
		t.Errorf("user resolve of an offline peer: %v, want ErrPeerUnreachable naming %s", err, key)
	}
	if err := adapter.Reveal(context.Background(), key, kernel.RevealPayload{TicketID: "t1"}, "sig"); !named(err, key) {
		t.Errorf("reveal to an offline peer: %v, want ErrPeerUnreachable naming %s", err, key)
	}
}

// named reports whether err is ErrPeerUnreachable carrying peer in its structured meta.
func named(err error, peer string) bool {
	var ke *kernel.KernelError
	return errors.Is(err, kernel.ErrPeerUnreachable) && errors.As(err, &ke) && ke.Meta["peer"] == peer
}
