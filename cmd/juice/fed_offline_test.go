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
