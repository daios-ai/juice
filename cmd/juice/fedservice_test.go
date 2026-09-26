// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/store"
	"github.com/google/uuid"
)

// Tests for /juice/fed/step/1 (§13): the wire verb that makes a step addressed to a peer
// completable. Before it existed such a step was a permanent funds trap — a key account holds no
// session token, and the federation protocols carried `run` but not `complete`.

// fedPeer registers a peer with a fresh keypair and returns its key and private key.
func fedPeer(t *testing.T, k *kernel.Kernel, handle string) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.BindPetname(context.Background(), pubB64, handle, false); err != nil {
		t.Fatal(err)
	}
	if _, err := k.EnsureKernelAccount(context.Background(), pubB64); err != nil {
		t.Fatal(err)
	}
	return pubB64, priv
}

// parkStepForPeer parks a step whose required caller is the given peer — the exact shape
// @sys/message produces when addressed across a kernel boundary, and the trap this protocol
// closes. Visibility binds against the creator (@sys) at creation, not the peer (§4 binding rule);
// TestFedStep_PeerCompletesLocalAction covers the case the old rule forbade — a non-public target.
func parkStepForPeer(t *testing.T, k *kernel.Kernel, db *store.DB, peerKey string) string {
	t.Helper()
	return parkStepForPeerUser(t, k, db, peerKey, "")
}

// parkStepForPeerUser addresses the step to one principal on that peer rather than to the kernel
// itself, which is what a remote handle resolves to (§13).
func parkStepForPeerUser(t *testing.T, k *kernel.Kernel, db *store.DB, peerKey, remoteUserID string) string {
	t.Helper()
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := k.ReadAccountByKernelKey(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}
	p := setupProcessHTTP(t, db, sys.ID, 0)
	step, err := k.CreateStep(ctx, setupTraceForProcess(t, db, p.ID), parkStepAction(t, k), json.RawMessage(`{}`), kernel.Principal{AccountID: peer.ID, RemoteID: remoteUserID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	return step.ID
}

// selfKey is the kernel's own public key — the `recipient` every step payload binds to (§13).
func selfKey(t *testing.T, k *kernel.Kernel) string {
	t.Helper()
	pub, err := k.GetConfig(context.Background(), configKeySigningPublic)
	if err != nil || pub == "" {
		t.Fatalf("signing public key: %v", err)
	}
	return pub
}

// parkStepAction returns the id of an active public @sys action a step can be parked against.
// Public so CanCall(peer, action) holds at creation (§4).
func parkStepAction(t *testing.T, k *kernel.Kernel) string {
	t.Helper()
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(backend.Close)

	a, err := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID: sys.ID, Name: "approve-" + uuid.New().String()[:8], Kind: kernel.KindHTTP,
		Source: backend.URL, Price: 0, Description: "approval sink",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	pub := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pub}); err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func fedStepList(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey) (int, map[string]any, error) {
	t.Helper()
	return fedStepListFor(t, k, priv, "")
}

// fedStepListFor asks the same question on one principal's behalf, as a peer does when a user
// rather than its operator wants to see the work parked for them (§13).
func fedStepListFor(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey, forUserID string) (int, map[string]any, error) {
	t.Helper()
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, err := testNet.SignStepListPayload(priv, cp, selfKey(t, k), ts, forUserID)
	if err != nil {
		t.Fatal(err)
	}
	return handleFederationStepList(k, context.Background(), cp, ts, sig, forUserID)
}

// derivedStepKey is the key a completion must carry, taken from the kernel's own derivation rather
// than a copy of it: a test that restates the rule cannot catch the rule changing.
func derivedStepKey(t *testing.T, k *kernel.Kernel, stepID string, input []byte) string {
	t.Helper()
	return kernel.StepIdempotencyKey(selfKey(t, k), stepID, sha256HexBytes(input))
}

func fedStepComplete(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey, stepID, idempKey string, input []byte) (int, map[string]any, error) {
	t.Helper()
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, err := testNet.SignStepPayload(priv, stepID, cp, selfKey(t, k), idempKey, ts, sha256HexBytes(input), "", false)
	if err != nil {
		t.Fatal(err)
	}
	return handleFederationStepComplete(k, context.Background(), cp, ts, idempKey, stepID, sig, input, "", false)
}

// fedStepCompleteAs completes as one principal of the peer: the step payload signed by the peer
// kernel with userID inside it — and, when superuser, its word that userID is its operator — as
// the wire carries them (P8).
func fedStepCompleteAs(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey, stepID, idempKey string, input []byte, userID string, superuser bool) (int, map[string]any, error) {
	t.Helper()
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	self := selfKey(t, k)
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, err := testNet.SignStepPayload(priv, stepID, cp, self, idempKey, ts, sha256HexBytes(input), userID, superuser)
	if err != nil {
		t.Fatal(err)
	}
	return handleFederationStepComplete(k, context.Background(), cp, ts, idempKey, stepID, sig, input, userID, superuser)
}

// readStepFails is a store whose FIRST step read fails — the one the scope check makes — and
// counts every read after it: a check that skipped on the failure reads the step again to act.
type readStepFails struct {
	kernel.Store
	reads int
}

func (r *readStepFails) ReadStep(ctx context.Context, id string) (*kernel.Step, error) {
	r.reads++
	if r.reads == 1 {
		return nil, errors.New("disk gone")
	}
	return r.Store.ReadStep(ctx, id)
}

// TestFedStep_CompletionScopeMustMatch: a step addressed to one principal of the peer is completed
// by that principal alone; a step addressed to the peer kernel by that kernel — its own bare
// signature, or a user its home kernel attests is the operator — and by no ordinary user, whatever
// they know. A failed read of the step's addressing refuses rather than falling open.
func TestFedStep_CompletionScopeMustMatch(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	keyA, privA := fedPeer(t, k, "peer-scope")
	const alice, opsy = "alice-remote-id", "sys-remote-id"
	key := func(stepID string, input []byte) string {
		return kernel.StepIdempotencyKey(selfKey(t, k), stepID, sha256HexBytes(input))
	}
	in := []byte(`{}`)

	// Kernel-addressed: an attested ordinary user is refused; an attested operator, and the bare
	// kernel, complete.
	forKernel := parkStepForPeer(t, k, db, keyA)
	if _, _, err := fedStepCompleteAs(t, k, privA, forKernel, key(forKernel, in), in, alice, false); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("ordinary user completing a kernel-addressed step: want ErrUnauthorized, got %v", err)
	}
	if s, _ := db.ReadStep(context.Background(), forKernel); s.Status != kernel.StepWaiting {
		t.Fatalf("the refused completion moved the step to %q", s.Status)
	}
	if _, _, err := fedStepCompleteAs(t, k, privA, forKernel, key(forKernel, in), in, opsy, true); err != nil {
		t.Errorf("attested operator completing a kernel-addressed step: %v", err)
	}
	bare := parkStepForPeer(t, k, db, keyA)
	if _, _, err := fedStepComplete(t, k, privA, bare, key(bare, in), in); err != nil {
		t.Errorf("the kernel itself completing a kernel-addressed step: %v", err)
	}

	// User-addressed: the bare kernel and another user are refused; that user completes, and so
	// does the operator when the step is addressed to the operator's own id.
	forAlice := parkStepForPeerUser(t, k, db, keyA, alice)
	if _, _, err := fedStepComplete(t, k, privA, forAlice, key(forAlice, in), in); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("the bare kernel completing a user-addressed step: want ErrUnauthorized, got %v", err)
	}
	if _, _, err := fedStepCompleteAs(t, k, privA, forAlice, key(forAlice, in), in, opsy, true); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("the operator completing another user's step: want ErrUnauthorized, got %v", err)
	}
	if _, _, err := fedStepCompleteAs(t, k, privA, forAlice, key(forAlice, in), in, alice, false); err != nil {
		t.Errorf("the addressed user completing their step: %v", err)
	}
	forOps := parkStepForPeerUser(t, k, db, keyA, opsy)
	if _, _, err := fedStepCompleteAs(t, k, privA, forOps, key(forOps, in), in, opsy, true); err != nil {
		t.Errorf("the operator completing a step addressed to their own id: %v", err)
	}

	// A request that claims the operator scope the home kernel did not sign: the signature covers
	// the scope, so the claim fails verification.
	forKernel2 := parkStepForPeer(t, k, db, keyA)
	cp := keyA
	self := selfKey(t, k)
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, _ := testNet.SignStepPayload(privA, forKernel2, cp, self, key(forKernel2, in), ts, sha256HexBytes(in), alice, false) // signed as NOT operator
	if _, _, err := handleFederationStepComplete(k, context.Background(), cp, ts, key(forKernel2, in), forKernel2, sig, in, alice, true); err == nil {
		t.Error("a forged operator scope was accepted")
	}

	// The read of the step's addressing fails: refuse, never skip.
	blindStore := &readStepFails{Store: db}
	blind := newKernel(testConfig("scope-test-secret"), kernel.Dependencies{Store: blindStore})
	if _, _, err := fedStepCompleteAs(t, blind, privA, forKernel2, key(forKernel2, in), in, opsy, true); err == nil {
		t.Error("a failed read of the step's addressing must be an error")
	}
	if blindStore.reads != 1 {
		t.Errorf("the failed read was skipped over: the handler read the step %d times, want to stop at the first", blindStore.reads)
	}
}

// TestFedStep_ExactlyFullPageIsNotTruncated: the flag says more is waiting only when more is.
func TestFedStep_ExactlyFullPageIsNotTruncated(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	keyA, privA := fedPeer(t, k, "peer-page")
	for i := 0; i < maxPeerStepPage; i++ {
		parkStepForPeer(t, k, db, keyA)
	}
	_, body, err := fedStepList(t, k, privA)
	if err != nil {
		t.Fatal(err)
	}
	if steps, _ := body["steps"].([]*kernel.PeerStepView); len(steps) != maxPeerStepPage || body["truncated"] != nil {
		t.Fatalf("exactly one page: want %d steps and no truncation, got %d and %v", maxPeerStepPage, len(steps), body["truncated"])
	}
	parkStepForPeer(t, k, db, keyA)
	_, body, _ = fedStepList(t, k, privA)
	if steps, _ := body["steps"].([]*kernel.PeerStepView); len(steps) != maxPeerStepPage || body["truncated"] != true {
		t.Fatalf("one past the page: want %d steps and truncated, got %d and %v", maxPeerStepPage, len(steps), body["truncated"])
	}
}

// TestWireErrorShieldsInternal: the one wire boundary sends code + concise message; an internal
// error crosses as its class alone, so store/SQL text never reaches a peer (§14 error hygiene).
func TestWireErrorShieldsInternal(t *testing.T) {
	code, msg := wireError(kernel.ErrInternal.Wrapf("commit settlement: sys variance: %s", "constraint failed: CHECK constraint failed: accounts"))
	if code != "internal" || msg != "internal error" {
		t.Errorf("internal error crossed with detail: code=%q msg=%q", code, msg)
	}
	code, msg = wireError(kernel.ErrInsufficientFunds.Wrap("operator reserve below settlement variance"))
	if code != "insufficient_funds" || msg != "operator reserve below settlement variance" {
		t.Errorf("typed error lost code or message: code=%q msg=%q", code, msg)
	}
	resp := fedError(kernel.ErrInternal.Wrap("constraint failed: CHECK constraint failed: accounts"))
	if resp.Status != 500 || strings.Contains(string(resp.Body), "CHECK") {
		t.Errorf("fedError leaked internal detail: status=%d body=%s", resp.Status, resp.Body)
	}
}

func TestFedStep_ListShowsOnlyOwnWaitingSteps(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "peer-a")
	_, privB := fedPeer(t, k, "peer-b")
	stepID := parkStepForPeer(t, k, db, keyA)

	status, body, err := fedStepList(t, k, privA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	steps, _ := body["steps"].([]*kernel.PeerStepView)
	if len(steps) != 1 || steps[0].ID != stepID {
		t.Fatalf("expected exactly the parked step, got %+v", steps)
	}
	if len(steps[0].AllowedInput) == 0 {
		t.Errorf("expected allowed_input so the peer can complete without reading the action")
	}

	// A different peer sees nothing: the step is not addressed to it.
	_, bodyB, err := fedStepList(t, k, privB)
	if err != nil {
		t.Fatalf("list for peer B: %v", err)
	}
	if stepsB, _ := bodyB["steps"].([]*kernel.PeerStepView); len(stepsB) != 0 {
		t.Errorf("peer B must not see peer A's step, got %+v", stepsB)
	}
}

// TestFedStep_ListIsUserScoped: listing and completing must have the same granularity. A step
// addressed to one principal on the peer is listed to that principal, and never to another; the
// operator's kernel-level ask still sees every step it may complete, user-addressed ones included,
// which is the only place a peer-held step is visible to them.
func TestFedStep_ListIsUserScoped(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "peer-scoped")
	const alice, bob = "alice-remote-id", "bob-remote-id"
	forAlice := parkStepForPeerUser(t, k, db, keyA, alice)
	forKernel := parkStepForPeer(t, k, db, keyA)

	ids := func(body map[string]any) []string {
		steps, _ := body["steps"].([]*kernel.PeerStepView)
		out := make([]string, len(steps))
		for i, s := range steps {
			out[i] = s.ID
		}
		return out
	}

	_, aliceBody, err := fedStepListFor(t, k, privA, alice)
	if err != nil {
		t.Fatalf("list for alice: %v", err)
	}
	if got := ids(aliceBody); len(got) != 1 || got[0] != forAlice {
		t.Fatalf("alice must see exactly the step addressed to her, got %v", got)
	}

	_, bobBody, err := fedStepListFor(t, k, privA, bob)
	if err != nil {
		t.Fatalf("list for bob: %v", err)
	}
	if got := ids(bobBody); len(got) != 0 {
		t.Errorf("bob must see none of alice's steps, got %v", got)
	}

	// The operator asks as the whole kernel and sees both — omitting the user must never filter.
	_, allBody, err := fedStepList(t, k, privA)
	if err != nil {
		t.Fatalf("kernel-level list: %v", err)
	}
	got := ids(allBody)
	if len(got) != 2 {
		t.Fatalf("the operator must see every step this kernel may complete, got %v", got)
	}
	steps, _ := allBody["steps"].([]*kernel.PeerStepView)
	for _, s := range steps {
		if s.ID == forAlice && s.RequiredCaller != alice {
			t.Errorf("a user-addressed step must name its principal, got %q", s.RequiredCaller)
		}
		if s.ID == forKernel && s.RequiredCaller != "" {
			t.Errorf("a kernel-addressed step names no principal, got %q", s.RequiredCaller)
		}
	}
}

// A signature-valid stranger is not provisioned an account by a read; it simply has no steps.
func TestFedStep_ListUnknownKeyIsEmptyAndProvisionsNothing(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	status, body, err := fedStepList(t, k, priv)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if steps, _ := body["steps"].([]*kernel.PeerStepView); len(steps) != 0 {
		t.Errorf("expected no steps for a stranger, got %+v", steps)
	}
	if u, _ := k.ReadAccountByKernelKey(context.Background(), cp); u != nil {
		t.Errorf("a read must not provision an account, but %s now exists", u.Handle)
	}
}

// Every way a step request is refused, in one table. Each case starts from the same fixture — a
// registered peer with one step parked for it — and mutates exactly one thing, so what is under
// test is the mutation and not the setup. These were seven near-identical tests.
func TestFedStep_RequestsAreRejected(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// run performs the rejected request. keyA/privA are the peer the step is parked for.
		run func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, stepID string) error
	}{
		{"bad list signature", func(t *testing.T, k *kernel.Kernel, keyA string, _ ed25519.PrivateKey, _ string) error {
			ts := time.Now().UTC().Format(time.RFC3339)
			_, _, err := handleFederationStepList(k, ctx, keyA, ts, "bogus", "")
			return err
		}},
		{"bad complete signature", func(t *testing.T, k *kernel.Kernel, keyA string, _ ed25519.PrivateKey, stepID string) error {
			ts := time.Now().UTC().Format(time.RFC3339)
			_, _, err := handleFederationStepComplete(k, ctx, keyA, ts, "idem-1", stepID, "bogus", []byte("{}"), "", false)
			return err
		}},
		{"stale timestamp", func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, _ string) error {
			stale := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
			sig, _ := testNet.SignStepListPayload(privA, keyA, selfKey(t, k), stale, "")
			_, _, err := handleFederationStepList(k, ctx, keyA, stale, sig, "")
			return err
		}},
		{"input does not match input_hash", func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, stepID string) error {
			ts := time.Now().UTC().Format(time.RFC3339)
			sig, _ := testNet.SignStepPayload(privA, stepID, keyA, selfKey(t, k), "idem-t", ts, sha256HexBytes([]byte(`{"ok":true}`)), "", false)
			_, _, err := handleFederationStepComplete(k, ctx, keyA, ts, "idem-t", stepID, sig, []byte(`{"ok":false}`), "", false)
			return err
		}},
		{"signed for another kernel", func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, stepID string) error {
			ts := time.Now().UTC().Format(time.RFC3339)
			sig, _ := testNet.SignStepListPayload(privA, keyA, "some-other-kernels-key", ts, "")
			_, _, err := handleFederationStepList(k, ctx, keyA, ts, sig, "")
			return err
		}},
		{"known peer that is not the required caller", func(t *testing.T, k *kernel.Kernel, _ string, _ ed25519.PrivateKey, stepID string) error {
			_, privB := fedPeer(t, k, "peer-b")
			_, _, err := fedStepComplete(t, k, privB, stepID, "idem-b", []byte("{}"))
			return err
		}},
		{"unknown key", func(t *testing.T, k *kernel.Kernel, _ string, _ ed25519.PrivateKey, stepID string) error {
			_, privX, _ := ed25519.GenerateKey(rand.Reader)
			_, _, err := fedStepComplete(t, k, privX, stepID, "idem-x", []byte("{}"))
			return err
		}},
		{"suspended peer", func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, stepID string) error {
			sys, _ := k.ReadUserByHandle(ctx, "sys")
			peer, _ := k.ReadAccountByKernelKey(ctx, keyA)
			if err := k.SuspendUser(ctx, sys.ID, peer.ID); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			if _, _, err := fedStepList(t, k, privA); err == nil {
				t.Error("a suspended peer's list must also be refused")
			}
			_, _, err := fedStepComplete(t, k, privA, stepID, "idem-s", []byte("{}"))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, k, db := newTestHTTPServerFull(t)
			defer srv.Close()
			keyA, privA := fedPeer(t, k, "peer-a")
			stepID := parkStepForPeer(t, k, db, keyA)
			if err := tc.run(t, k, keyA, privA, stepID); err == nil {
				t.Error("expected the request to be rejected")
			}
		})
	}
}

// Rejections handled by OnStep before any handler runs.
func TestFedStep_OnStepRejects(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	h := &fedHandlers{kernel: k}

	if got := h.OnStep(context.Background(), "", fed.StepRequest{Kind: "nonsense"}).Status; got != kernel.ErrInvalidInput.HTTP {
		t.Errorf("unknown kind: got %d, want %d", got, kernel.ErrInvalidInput.HTTP)
	}
	// Defense in depth: a validly-signed request may not be replayed over a connection
	// authenticated as a different peer (mirrors OnCall).
	resp := h.OnStep(context.Background(), "different-connection-key",
		fed.StepRequest{Kind: "list", Counterparty: "claimed-key"})
	if resp.Status != kernel.ErrUnauthenticated.HTTP {
		t.Errorf("mismatched connection key: got %d, want %d", resp.Status, kernel.ErrUnauthenticated.HTTP)
	}
}

func TestFedStep_CompleteSettlesAndIsIdempotent(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "peer-a")
	stepID := parkStepForPeer(t, k, db, keyA)

	status, body, err := fedStepComplete(t, k, privA, stepID, derivedStepKey(t, k, stepID, []byte("{}")), []byte("{}"))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	txID, _ := body["tx_id"].(string)
	if txID == "" {
		t.Fatalf("expected a tx_id, got %v", body)
	}

	// The step is done, and the completion transaction obeys the role law: the peer is the caller.
	ctx := context.Background()
	sys, _ := k.ReadUserByHandle(ctx, "sys")
	step, err := k.ReadStep(ctx, sys.ID, stepID)
	if err != nil {
		t.Fatalf("ReadStep: %v", err)
	}
	if step.Status != kernel.StepDone {
		t.Errorf("expected step done, got %s", step.Status)
	}
	peer, _ := k.ReadAccountByKernelKey(ctx, keyA)
	tx, err := k.ReadTransaction(ctx, sys.ID, txID)
	if err != nil {
		t.Fatalf("ReadTransaction: %v", err)
	}
	if tx.CallerUserID != peer.ID {
		t.Errorf("expected caller_user_id=%s (the peer), got %s", peer.ID, tx.CallerUserID)
	}

	// A replay with the same idempotency key returns the stored result, re-executing nothing.
	statusR, bodyR, err := fedStepComplete(t, k, privA, stepID, derivedStepKey(t, k, stepID, []byte("{}")), []byte("{}"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if statusR != http.StatusOK || bodyR["tx_id"] != txID {
		t.Errorf("replay must return the stored result, got status=%d body=%v", statusR, bodyR)
	}

	// A fresh key against the now-done step fails: it is no longer waiting.
	if _, _, err := fedStepComplete(t, k, privA, stepID, "idem-2", []byte("{}")); err == nil {
		t.Error("expected completing a done step to fail")
	}
}

// TestFedStep_PeerCompletesLocalAction: a peer completes a step whose target is a LOCAL action it
// could never call directly (§4). Visibility bound against the creator (@sys) at creation (§10),
// and completion re-checks only liveness — so the old rule's "a peer may be parked only for a
// public action" restriction is gone, without a peer ever gaining reach to the local action.
func TestFedStep_PeerCompletesLocalAction(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	ctx := context.Background()

	keyA, privA := fedPeer(t, k, "peer-a")
	peer, _ := k.ReadAccountByKernelKey(ctx, keyA)
	sys, _ := k.ReadUserByHandle(ctx, "sys")

	// A local action owned by @sys — the creator — and a step parked for the peer against it.
	actionID := parkStepAction(t, k)
	local := kernel.VisibilityLocal
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: actionID, Visibility: &local}); err != nil {
		t.Fatal(err)
	}
	p := setupProcessHTTP(t, db, sys.ID, 0)
	step, err := k.CreateStep(ctx, setupTraceForProcess(t, db, p.ID), actionID, json.RawMessage(`{}`), kernel.Principal{AccountID: peer.ID})
	if err != nil {
		t.Fatalf("CreateStep parking a local action for a peer: %v", err)
	}

	status, body, err := fedStepComplete(t, k, privA, step.ID, derivedStepKey(t, k, step.ID, []byte("{}")), []byte("{}"))
	if err != nil || status != http.StatusOK {
		t.Fatalf("peer complete of a local-target step: status=%d err=%v", status, err)
	}
	if body["tx_id"] == "" || body["tx_id"] == nil {
		t.Fatalf("expected a settled completion, got %v", body)
	}
}

func TestFedStep_ListNotCrowdedOutByOwnProcesses(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	ctx := context.Background()

	keyA, privA := fedPeer(t, k, "peer-a")
	peer, _ := k.ReadAccountByKernelKey(ctx, keyA)
	sys, _ := k.ReadUserByHandle(ctx, "sys")

	// The step actually addressed to the peer, created FIRST so a newest-first cap would drop it.
	stepID := parkStepForPeer(t, k, db, keyA)

	// 60 steps inside processes the peer owns, awaiting a local user — visible to it via
	// CanListStep, but not completable by it.
	local, _ := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "local@k", Password: "pw12345678"})
	action := parkStepAction(t, k)
	for i := 0; i < 60; i++ {
		p := setupProcessHTTP(t, db, peer.ID, 0)
		if _, err := k.CreateStep(ctx, setupTraceForProcess(t, db, p.ID), action, json.RawMessage(`{}`), kernel.Principal{AccountID: local.ID}); err != nil {
			t.Fatalf("seed step %d: %v", i, err)
		}
	}
	_ = sys

	_, body, err := fedStepList(t, k, privA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	steps, _ := body["steps"].([]*kernel.PeerStepView)
	if len(steps) != 1 || steps[0].ID != stepID {
		ids := make([]string, len(steps))
		for i, s := range steps {
			ids[i] = s.ID
		}
		t.Fatalf("expected exactly the peer-addressed step %s, got %v", stepID, ids)
	}
}

// The peer-facing view must carry the request and nothing about the requester. This asserts on the
// serialized JSON rather than the struct, because the defect it guards against was reusing a type
// whose EMBEDDED fields leaked — a field-by-field check on the wrong type would have passed.
// owner_handle is the sharp one: a local user identity crossing a kernel boundary is what §5's
// encapsulation exists to prevent.
func TestFedStep_ListDisclosesRequestNotRequester(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "peer-a")
	stepID := parkStepForPeer(t, k, db, keyA)

	_, body, err := fedStepList(t, k, privA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(wire)

	// Present: what the completer needs.
	for _, want := range []string{stepID, "allowed_input", "price", "created_at"} {
		if !strings.Contains(got, want) {
			t.Errorf("peer view must carry %q; got %s", want, got)
		}
	}
	// Absent: local composition and identity.
	for _, leak := range []string{"owner_handle", "created_by", "required_caller", "parent_trace_id",
		"action_id", "completion_trace_id", "waiting_on_peer"} {
		if strings.Contains(got, leak) {
			t.Errorf("peer view leaks %q: %s", leak, got)
		}
	}
	// The process owner's handle must not appear under any key at all.
	if strings.Contains(got, "sys") {
		t.Errorf("peer view leaks a local handle: %s", got)
	}
}

// A completion that settles as a FAILURE must replay as that failure, not as 200. Otherwise a
// retry tells the operator the step succeeded when it did not.
func TestFedStep_ReplayOfSettledFailureKeepsErrorStatus(t *testing.T) {
	result := map[string]any{"error": "boom", "code": kernel.ErrExecutionFailed.Code}
	failed := &kernel.Receipt{Status: kernel.TxFailure}
	if got := replayStatus(result, failed); got != kernel.ErrExecutionFailed.HTTP {
		t.Errorf("settled failure replays as %d, want %d", got, kernel.ErrExecutionFailed.HTTP)
	}
	if got := replayStatus(map[string]any{"ok": true}, &kernel.Receipt{Status: kernel.TxSuccess}); got != http.StatusOK {
		t.Errorf("settled success replays as %d, want 200", got)
	}
	// The receipt decides, not the body: a successful action whose own output carries an "error"
	// field (a validator returning {"error": null}) must still replay as success.
	if got := replayStatus(map[string]any{"error": nil}, &kernel.Receipt{Status: kernel.TxSuccess}); got != http.StatusOK {
		t.Errorf("a success whose result contains an error field replays as %d, want 200", got)
	}
	// With no receipt (a pre-execution rejection, nothing committed) the body is all there is.
	if got := replayStatus(map[string]any{"error": "boom"}, nil); got == http.StatusOK {
		t.Error("a stored rejection must not replay as 200")
	}
	// The in-flight reply carries a code so the requester re-raises a typed error.
	status, body, _ := duplicateInFlight()
	if status != http.StatusConflict || body["code"] != kernel.ErrInvalidState.Code {
		t.Errorf("duplicate-in-flight = (%d, %v), want 409 with an invalid_state code", status, body)
	}
}

// The step payloads must not verify as any other signed Juice payload, and vice versa (§12).
func TestFedStep_SignatureDomainsAreDisjoint(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	const hash = "abc123"

	const rcpt = "recipient-kernel-key"
	stepSig, _ := testNet.SignStepPayload(priv, "step-1", cp, rcpt, "idem-1", ts, hash, "", false)
	listSig, _ := testNet.SignStepListPayload(priv, cp, rcpt, ts, "")
	callSig, _ := testNet.SignFederationPayload(priv, kernel.OutboundCall{ActionID: "act-id", ExpectedContractHash: "chash", IdempotencyKey: "idem-1", Commitment: "", Lottery: 0}, cp, rcpt, ts, hash)

	// A call signature must not pass as a step signature, nor either step kind as the other.
	if err := testNet.VerifyStepSignature(cp, "step-1", cp, rcpt, "idem-1", ts, hash, "", false, callSig); err == nil {
		t.Error("a federation call signature must not verify as a step completion")
	}
	if err := testNet.VerifyStepSignature(cp, "step-1", cp, rcpt, "idem-1", ts, hash, "", false, listSig); err == nil {
		t.Error("a step list signature must not verify as a step completion")
	}
	if err := testNet.VerifyStepListSignature(cp, cp, rcpt, ts, "", stepSig); err == nil {
		t.Error("a step completion signature must not verify as a step list")
	}
	if err := testNet.VerifyFederationSignature(cp, kernel.OutboundCall{ActionID: "act-id", ExpectedContractHash: "chash", IdempotencyKey: "idem-1", Commitment: "", Lottery: 0}, cp, rcpt, ts, hash, stepSig); err == nil {
		t.Error("a step signature must not verify as a federation call")
	}
	// Sanity: each verifies under its own domain.
	if err := testNet.VerifyStepSignature(cp, "step-1", cp, rcpt, "idem-1", ts, hash, "", false, stepSig); err != nil {
		t.Errorf("step signature should verify in its own domain: %v", err)
	}
	if err := testNet.VerifyStepListSignature(cp, cp, rcpt, ts, "", listSig); err != nil {
		t.Errorf("list signature should verify in its own domain: %v", err)
	}
}
