package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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
	sys, err := k.ReadUserByHandle(context.Background(), "@sys")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.AddPeer(context.Background(), sys.ID, handle, pubB64); err != nil {
		t.Fatal(err)
	}
	return pubB64, priv
}

// parkStepForPeer parks a step whose required caller is the given peer — the exact shape
// @sys/message produces when addressed across a kernel boundary, and the trap this protocol
// closes. The target action must be public for CanCall(peer, action) to hold at creation (§4).
func parkStepForPeer(t *testing.T, k *kernel.Kernel, db *store.DB, peerKey string) string {
	t.Helper()
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := k.ReadUserByPublicKey(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}
	p := setupProcessHTTP(t, db, sys.ID, 0)
	step, err := k.CreateStep(ctx, setupTraceForProcess(t, db, p.ID), parkStepAction(t, k), json.RawMessage(`{}`), peer.ID)
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
	sys, err := k.ReadUserByHandle(ctx, "@sys")
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
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, err := kernel.SignStepListPayload(priv, cp, selfKey(t, k), ts)
	if err != nil {
		t.Fatal(err)
	}
	return handleFederationStepList(k, context.Background(), cp, ts, sig, "")
}

func fedStepComplete(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey, stepID, idempKey string, input []byte) (int, map[string]any, error) {
	t.Helper()
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, err := kernel.SignStepPayload(priv, stepID, cp, selfKey(t, k), idempKey, ts, sha256HexBytes(input))
	if err != nil {
		t.Fatal(err)
	}
	return handleFederationStepComplete(k, context.Background(), cp, ts, idempKey, stepID, sig, input)
}

func TestFedStep_ListShowsOnlyOwnWaitingSteps(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "@peer-a")
	_, privB := fedPeer(t, k, "@peer-b")
	stepID := parkStepForPeer(t, k, db, keyA)

	status, body, err := fedStepList(t, k, privA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	steps, _ := body["steps"].([]*peerStepView)
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
	if stepsB, _ := bodyB["steps"].([]*peerStepView); len(stepsB) != 0 {
		t.Errorf("peer B must not see peer A's step, got %+v", stepsB)
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
	if steps, _ := body["steps"].([]*peerStepView); len(steps) != 0 {
		t.Errorf("expected no steps for a stranger, got %+v", steps)
	}
	if u, _ := k.ReadUserByPublicKey(context.Background(), cp); u != nil {
		t.Errorf("a read must not provision an account, but %s now exists", u.Handle)
	}
}

func TestFedStep_BadSignatureAndStaleTimestampRejected(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "@peer-a")
	stepID := parkStepForPeer(t, k, db, keyA)
	ts := time.Now().UTC().Format(time.RFC3339)

	if _, _, err := handleFederationStepList(k, context.Background(), keyA, ts, "bogus", ""); err == nil {
		t.Error("expected a bad list signature to be rejected")
	}
	if _, _, err := handleFederationStepComplete(k, context.Background(), keyA, ts, "idem-1", stepID, "bogus", []byte("{}")); err == nil {
		t.Error("expected a bad complete signature to be rejected")
	}

	stale := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	sig, _ := kernel.SignStepListPayload(privA, keyA, selfKey(t, k), stale)
	if _, _, err := handleFederationStepList(k, context.Background(), keyA, stale, sig, ""); err == nil {
		t.Error("expected a stale timestamp to be rejected")
	}
}

func TestFedStep_CompleteSettlesAndIsIdempotent(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "@peer-a")
	stepID := parkStepForPeer(t, k, db, keyA)

	status, body, err := fedStepComplete(t, k, privA, stepID, "idem-1", []byte("{}"))
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
	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	step, err := k.ReadStep(ctx, sys.ID, stepID)
	if err != nil {
		t.Fatalf("ReadStep: %v", err)
	}
	if step.Status != kernel.StepDone {
		t.Errorf("expected step done, got %s", step.Status)
	}
	peer, _ := k.ReadUserByPublicKey(ctx, keyA)
	tx, err := k.ReadTransaction(ctx, sys.ID, txID)
	if err != nil {
		t.Fatalf("ReadTransaction: %v", err)
	}
	if tx.CallerUserID != peer.ID {
		t.Errorf("expected caller_user_id=%s (the peer), got %s", peer.ID, tx.CallerUserID)
	}

	// A replay with the same idempotency key returns the stored result, re-executing nothing.
	statusR, bodyR, err := fedStepComplete(t, k, privA, stepID, "idem-1", []byte("{}"))
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

func TestFedStep_CompleteRejectsWrongPeerAndStranger(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, _ := fedPeer(t, k, "@peer-a")
	_, privB := fedPeer(t, k, "@peer-b")
	stepID := parkStepForPeer(t, k, db, keyA)

	// Peer B is a known peer but not this step's required caller.
	if _, _, err := fedStepComplete(t, k, privB, stepID, "idem-b", []byte("{}")); err == nil {
		t.Error("expected a non-required-caller peer to be refused")
	}

	// A stranger holds no steps at all.
	_, privX, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := fedStepComplete(t, k, privX, stepID, "idem-x", []byte("{}")); err == nil {
		t.Error("expected an unknown key to be refused")
	}
}

func TestFedStep_SuspendedPeerRefused(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ctx := context.Background()
	keyA, privA := fedPeer(t, k, "@peer-a")
	stepID := parkStepForPeer(t, k, db, keyA)

	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	peer, _ := k.ReadUserByPublicKey(ctx, keyA)
	if err := k.SuspendUser(ctx, sys.ID, peer.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	if _, _, err := fedStepList(t, k, privA); err == nil {
		t.Error("expected a suspended peer's list to be refused")
	}
	if _, _, err := fedStepComplete(t, k, privA, stepID, "idem-s", []byte("{}")); err == nil {
		t.Error("expected a suspended peer's completion to be refused")
	}
}

// The step payloads must not verify as any other signed Juice payload, and vice versa (§12).
func TestFedStep_SignatureDomainsAreDisjoint(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	const hash = "abc123"

	const rcpt = "recipient-kernel-key"
	stepSig, _ := kernel.SignStepPayload(priv, "step-1", cp, rcpt, "idem-1", ts, hash)
	listSig, _ := kernel.SignStepListPayload(priv, cp, rcpt, ts)
	callSig, _ := kernel.SignFederationPayload(priv, "@o/a", cp, "idem-1", ts, hash)

	// A call signature must not pass as a step signature, nor either step kind as the other.
	if err := kernel.VerifyStepSignature(cp, "step-1", cp, rcpt, "idem-1", ts, hash, callSig); err == nil {
		t.Error("a federation call signature must not verify as a step completion")
	}
	if err := kernel.VerifyStepSignature(cp, "step-1", cp, rcpt, "idem-1", ts, hash, listSig); err == nil {
		t.Error("a step list signature must not verify as a step completion")
	}
	if err := kernel.VerifyStepListSignature(cp, cp, rcpt, ts, stepSig); err == nil {
		t.Error("a step completion signature must not verify as a step list")
	}
	if err := kernel.VerifyFederationSignature(cp, "@o/a", cp, "idem-1", ts, hash, stepSig); err == nil {
		t.Error("a step signature must not verify as a federation call")
	}
	// Sanity: each verifies under its own domain.
	if err := kernel.VerifyStepSignature(cp, "step-1", cp, rcpt, "idem-1", ts, hash, stepSig); err != nil {
		t.Errorf("step signature should verify in its own domain: %v", err)
	}
	if err := kernel.VerifyStepListSignature(cp, cp, rcpt, ts, listSig); err != nil {
		t.Errorf("list signature should verify in its own domain: %v", err)
	}
}

// A tampered input is caught by input_hash even though the signature covers only the hash.
func TestFedStep_TamperedInputRejected(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "@peer-a")
	stepID := parkStepForPeer(t, k, db, keyA)

	ts := time.Now().UTC().Format(time.RFC3339)
	sig, _ := kernel.SignStepPayload(privA, stepID, keyA, selfKey(t, k), "idem-t", ts, sha256HexBytes([]byte(`{"ok":true}`)))
	_, _, err := handleFederationStepComplete(k, context.Background(), keyA, ts, "idem-t", stepID, sig, []byte(`{"ok":false}`))
	if err == nil {
		t.Error("expected a body that does not match input_hash to be rejected")
	}
}

func TestFedStep_UnknownKindRejected(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	h := &fedHandlers{kernel: k}
	resp := h.OnStep(context.Background(), "", fed.StepRequest{Kind: "nonsense"})
	if resp.Status != kernel.ErrInvalidInput.HTTP {
		t.Errorf("expected %d for an unknown kind, got %d", kernel.ErrInvalidInput.HTTP, resp.Status)
	}
}

// Defense in depth: a validly-signed request may not be replayed over a connection authenticated
// as a different peer (mirrors OnCall).
func TestFedStep_ConnectionKeyMustMatchCounterparty(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	h := &fedHandlers{kernel: k}
	resp := h.OnStep(context.Background(), "different-connection-key",
		fed.StepRequest{Kind: "list", Counterparty: "claimed-key"})
	if resp.Status != kernel.ErrUnauthenticated.HTTP {
		t.Errorf("expected %d when the connection key differs, got %d", kernel.ErrUnauthenticated.HTTP, resp.Status)
	}
}

// A payload signed for a different kernel must not verify here. This is what closes cross-kernel
// replay: without recipient binding, a request kernel B received from A could be replayed to C.
func TestFedStep_RejectsPayloadSignedForAnotherKernel(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "@peer-a")
	stepID := parkStepForPeer(t, k, db, keyA)
	ts := time.Now().UTC().Format(time.RFC3339)
	const otherKernel = "some-other-kernels-public-key"

	listSig, _ := kernel.SignStepListPayload(privA, keyA, otherKernel, ts)
	if _, _, err := handleFederationStepList(k, context.Background(), keyA, ts, listSig, ""); err == nil {
		t.Error("a list signed for another kernel must not verify here")
	}
	compSig, _ := kernel.SignStepPayload(privA, stepID, keyA, otherKernel, "idem-x", ts, sha256HexBytes([]byte("{}")))
	if _, _, err := handleFederationStepComplete(k, context.Background(), keyA, ts, "idem-x", stepID, compSig, []byte("{}")); err == nil {
		t.Error("a completion signed for another kernel must not verify here")
	}
}

// The list must be scoped in SQL, not filtered after a row cap: a peer's own inbound-call
// processes would otherwise crowd out the steps it can actually complete — the exact rows this
// protocol exists to surface. Regression for a 50-row cap applied before the filter.
func TestFedStep_ListNotCrowdedOutByOwnProcesses(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	ctx := context.Background()

	keyA, privA := fedPeer(t, k, "@peer-a")
	peer, _ := k.ReadUserByPublicKey(ctx, keyA)
	sys, _ := k.ReadUserByHandle(ctx, "@sys")

	// The step actually addressed to the peer, created FIRST so a newest-first cap would drop it.
	stepID := parkStepForPeer(t, k, db, keyA)

	// 60 steps inside processes the peer owns, awaiting a local user — visible to it via
	// CanListStep, but not completable by it.
	local, _ := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "@local", Password: "pw12345678"})
	action := parkStepAction(t, k)
	for i := 0; i < 60; i++ {
		p := setupProcessHTTP(t, db, peer.ID, 0)
		if _, err := k.CreateStep(ctx, setupTraceForProcess(t, db, p.ID), action, json.RawMessage(`{}`), local.ID); err != nil {
			t.Fatalf("seed step %d: %v", i, err)
		}
	}
	_ = sys

	_, body, err := fedStepList(t, k, privA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	steps, _ := body["steps"].([]*peerStepView)
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

	keyA, privA := fedPeer(t, k, "@peer-a")
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
	if strings.Contains(got, "@sys") {
		t.Errorf("peer view leaks a local handle: %s", got)
	}
}

// A completion that settles as a FAILURE must replay as that failure, not as 200. Otherwise a
// retry tells the operator the step succeeded when it did not.
func TestFedStep_ReplayOfSettledFailureKeepsErrorStatus(t *testing.T) {
	result := map[string]any{"error": "boom", "code": kernel.ErrExecutionFailed.Code}
	if got := storedIdempotentStatus(result); got != kernel.ErrExecutionFailed.HTTP {
		t.Errorf("stored error replays as %d, want %d", got, kernel.ErrExecutionFailed.HTTP)
	}
	if got := storedIdempotentStatus(map[string]any{"tx_id": "t1"}); got != http.StatusOK {
		t.Errorf("stored success replays as %d, want 200", got)
	}
	// An unknown/missing code degrades to 500, never to 200.
	if got := storedIdempotentStatus(map[string]any{"error": "boom"}); got == http.StatusOK {
		t.Error("a stored error with no code must not replay as 200")
	}
	// The in-flight reply carries a code so the requester re-raises a typed error.
	status, body, _ := duplicateInFlight()
	if status != http.StatusConflict || body["code"] != kernel.ErrInvalidState.Code {
		t.Errorf("duplicate-in-flight = (%d, %v), want 409 with an invalid_state code", status, body)
	}
}

// Scenario (review finding 8): a completion that fails AFTER committing has charged the caller,
// so the error must say where that transaction is. handle() writes only the error, and writeErr
// carries just {error, code, meta} — so the ids have to ride in Meta or they are lost.
func TestCompletion_ErrorCarriesTheSettledTransactionIDs(t *testing.T) {
	reply := &kernel.StepReply{
		CallReply: &kernel.CallReply{TxID: "tx-1", TraceID: "tr-1", ReceiptID: "rc-1"},
		StepID:    "st-1",
	}
	err := withSettlementMeta(kernel.ErrExecutionFailed.Wrap("upstream exploded"), reply)
	ke, ok := err.(*kernel.KernelError)
	if !ok {
		t.Fatalf("expected a KernelError, got %T", err)
	}
	for k, want := range map[string]string{"tx_id": "tx-1", "trace_id": "tr-1", "receipt_id": "rc-1", "step_id": "st-1"} {
		if ke.Meta[k] != want {
			t.Errorf("meta[%q] = %q, want %q", k, ke.Meta[k], want)
		}
	}
	if ke.Code != kernel.ErrExecutionFailed.Code {
		t.Errorf("code changed to %q; the error's own classification must survive", ke.Code)
	}
	// Nothing settled → nothing to point at, and the error must pass through untouched.
	plain := kernel.ErrInvalidState.Wrap("step is not waiting")
	if got := withSettlementMeta(plain, nil); got != error(plain) {
		t.Errorf("an error with no settled reply must be returned unchanged, got %v", got)
	}
}
