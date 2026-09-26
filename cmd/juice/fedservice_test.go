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

// Tests for /juice/fed/task/1 (§13): the wire verb that makes a task addressed to a peer
// completable. Before it existed such a task was a permanent funds trap — a key account holds no
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

// parkTaskForPeer parks a task whose required caller is the given peer — the exact shape
// @sys/message produces when addressed across a kernel boundary, and the trap this protocol
// closes. Visibility binds against the creator (@sys) at creation, not the peer (§4 binding rule);
// TestFedTask_PeerCompletesLocalAction covers the case the old rule forbade — a non-public target.
func parkTaskForPeer(t *testing.T, k *kernel.Kernel, db *store.DB, peerKey string) string {
	t.Helper()
	return parkTaskForPeerUser(t, k, db, peerKey, "")
}

// parkTaskForPeerUser addresses the task to one principal on that peer rather than to the kernel
// itself, which is what a remote handle resolves to (§13).
func parkTaskForPeerUser(t *testing.T, k *kernel.Kernel, db *store.DB, peerKey, remoteUserID string) string {
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
	task, err := k.CreateTask(ctx, setupTraceForProcess(t, db, p.ID), parkTaskAction(t, k), json.RawMessage(`{}`), kernel.Principal{AccountID: peer.ID, RemoteID: remoteUserID})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task.ID
}

// selfKey is the kernel's own public key — the `recipient` every task payload binds to (§13).
func selfKey(t *testing.T, k *kernel.Kernel) string {
	t.Helper()
	pub, err := k.GetConfig(context.Background(), configKeySigningPublic)
	if err != nil || pub == "" {
		t.Fatalf("signing public key: %v", err)
	}
	return pub
}

// parkTaskAction returns the id of an active public @sys action a task can be parked against.
// Public so CanCall(peer, action) holds at creation (§4).
func parkTaskAction(t *testing.T, k *kernel.Kernel) string {
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

func fedTaskList(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey) (int, map[string]any, error) {
	t.Helper()
	return fedTaskListFor(t, k, priv, "")
}

// fedTaskListFor asks the same question on one principal's behalf, as a peer does when a user
// rather than its operator wants to see the work parked for them (§13).
func fedTaskListFor(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey, forUserID string) (int, map[string]any, error) {
	t.Helper()
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, err := testNet.SignTaskListPayload(priv, cp, selfKey(t, k), ts, forUserID)
	if err != nil {
		t.Fatal(err)
	}
	return handleFederationTaskList(k, context.Background(), cp, ts, sig, forUserID)
}

// derivedTaskKey is the key a completion must carry, taken from the kernel's own derivation rather
// than a copy of it: a test that restates the rule cannot catch the rule changing.
func derivedTaskKey(t *testing.T, k *kernel.Kernel, taskID string, input []byte) string {
	t.Helper()
	return kernel.TaskIdempotencyKey(selfKey(t, k), taskID, sha256HexBytes(input))
}

func fedTaskComplete(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey, taskID, idempKey string, input []byte) (int, map[string]any, error) {
	t.Helper()
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, err := testNet.SignTaskPayload(priv, taskID, cp, selfKey(t, k), idempKey, ts, sha256HexBytes(input), "", false)
	if err != nil {
		t.Fatal(err)
	}
	return handleFederationTaskComplete(k, context.Background(), cp, ts, idempKey, taskID, sig, input, "", false)
}

// fedTaskCompleteAs completes as one principal of the peer: the task payload signed by the peer
// kernel with userID inside it — and, when superuser, its word that userID is its operator — as
// the wire carries them (P8).
func fedTaskCompleteAs(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey, taskID, idempKey string, input []byte, userID string, superuser bool) (int, map[string]any, error) {
	t.Helper()
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	self := selfKey(t, k)
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, err := testNet.SignTaskPayload(priv, taskID, cp, self, idempKey, ts, sha256HexBytes(input), userID, superuser)
	if err != nil {
		t.Fatal(err)
	}
	return handleFederationTaskComplete(k, context.Background(), cp, ts, idempKey, taskID, sig, input, userID, superuser)
}

// readTaskFails is a store whose FIRST task read fails — the one the scope check makes — and
// counts every read after it: a check that skipped on the failure reads the task again to act.
type readTaskFails struct {
	kernel.Store
	reads int
}

func (r *readTaskFails) ReadTask(ctx context.Context, id string) (*kernel.Task, error) {
	r.reads++
	if r.reads == 1 {
		return nil, errors.New("disk gone")
	}
	return r.Store.ReadTask(ctx, id)
}

// TestFedTask_CompletionScopeMustMatch: a task addressed to one principal of the peer is completed
// by that principal alone; a task addressed to the peer kernel by that kernel — its own bare
// signature, or a user its home kernel attests is the operator — and by no ordinary user, whatever
// they know. A failed read of the task's addressing refuses rather than falling open.
func TestFedTask_CompletionScopeMustMatch(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	keyA, privA := fedPeer(t, k, "peer-scope")
	const alice, opsy = "alice-remote-id", "sys-remote-id"
	key := func(taskID string, input []byte) string {
		return kernel.TaskIdempotencyKey(selfKey(t, k), taskID, sha256HexBytes(input))
	}
	in := []byte(`{}`)

	// Kernel-addressed: an attested ordinary user is refused; an attested operator, and the bare
	// kernel, complete.
	forKernel := parkTaskForPeer(t, k, db, keyA)
	if _, _, err := fedTaskCompleteAs(t, k, privA, forKernel, key(forKernel, in), in, alice, false); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("ordinary user completing a kernel-addressed task: want ErrUnauthorized, got %v", err)
	}
	if s, _ := db.ReadTask(context.Background(), forKernel); s.Status != kernel.TaskWaiting {
		t.Fatalf("the refused completion moved the task to %q", s.Status)
	}
	if _, _, err := fedTaskCompleteAs(t, k, privA, forKernel, key(forKernel, in), in, opsy, true); err != nil {
		t.Errorf("attested operator completing a kernel-addressed task: %v", err)
	}
	bare := parkTaskForPeer(t, k, db, keyA)
	if _, _, err := fedTaskComplete(t, k, privA, bare, key(bare, in), in); err != nil {
		t.Errorf("the kernel itself completing a kernel-addressed task: %v", err)
	}

	// User-addressed: the bare kernel and another user are refused; that user completes, and so
	// does the operator when the task is addressed to the operator's own id.
	forAlice := parkTaskForPeerUser(t, k, db, keyA, alice)
	if _, _, err := fedTaskComplete(t, k, privA, forAlice, key(forAlice, in), in); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("the bare kernel completing a user-addressed task: want ErrUnauthorized, got %v", err)
	}
	if _, _, err := fedTaskCompleteAs(t, k, privA, forAlice, key(forAlice, in), in, opsy, true); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("the operator completing another user's task: want ErrUnauthorized, got %v", err)
	}
	if _, _, err := fedTaskCompleteAs(t, k, privA, forAlice, key(forAlice, in), in, alice, false); err != nil {
		t.Errorf("the addressed user completing their task: %v", err)
	}
	forOps := parkTaskForPeerUser(t, k, db, keyA, opsy)
	if _, _, err := fedTaskCompleteAs(t, k, privA, forOps, key(forOps, in), in, opsy, true); err != nil {
		t.Errorf("the operator completing a task addressed to their own id: %v", err)
	}

	// A request that claims the operator scope the home kernel did not sign: the signature covers
	// the scope, so the claim fails verification.
	forKernel2 := parkTaskForPeer(t, k, db, keyA)
	cp := keyA
	self := selfKey(t, k)
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, _ := testNet.SignTaskPayload(privA, forKernel2, cp, self, key(forKernel2, in), ts, sha256HexBytes(in), alice, false) // signed as NOT operator
	if _, _, err := handleFederationTaskComplete(k, context.Background(), cp, ts, key(forKernel2, in), forKernel2, sig, in, alice, true); err == nil {
		t.Error("a forged operator scope was accepted")
	}

	// The read of the task's addressing fails: refuse, never skip.
	blindStore := &readTaskFails{Store: db}
	blind := newKernel(testConfig("scope-test-secret"), kernel.Dependencies{Store: blindStore})
	if _, _, err := fedTaskCompleteAs(t, blind, privA, forKernel2, key(forKernel2, in), in, opsy, true); err == nil {
		t.Error("a failed read of the task's addressing must be an error")
	}
	if blindStore.reads != 1 {
		t.Errorf("the failed read was skipped over: the handler read the task %d times, want to stop at the first", blindStore.reads)
	}
}

// TestFedTask_ExactlyFullPageIsNotTruncated: the flag says more is waiting only when more is.
func TestFedTask_ExactlyFullPageIsNotTruncated(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	keyA, privA := fedPeer(t, k, "peer-page")
	for i := 0; i < maxPeerTaskPage; i++ {
		parkTaskForPeer(t, k, db, keyA)
	}
	_, body, err := fedTaskList(t, k, privA)
	if err != nil {
		t.Fatal(err)
	}
	if tasks, _ := body["tasks"].([]*kernel.PeerTaskView); len(tasks) != maxPeerTaskPage || body["truncated"] != nil {
		t.Fatalf("exactly one page: want %d tasks and no truncation, got %d and %v", maxPeerTaskPage, len(tasks), body["truncated"])
	}
	parkTaskForPeer(t, k, db, keyA)
	_, body, _ = fedTaskList(t, k, privA)
	if tasks, _ := body["tasks"].([]*kernel.PeerTaskView); len(tasks) != maxPeerTaskPage || body["truncated"] != true {
		t.Fatalf("one past the page: want %d tasks and truncated, got %d and %v", maxPeerTaskPage, len(tasks), body["truncated"])
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

func TestFedTask_ListShowsOnlyOwnWaitingTasks(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "peer-a")
	_, privB := fedPeer(t, k, "peer-b")
	taskID := parkTaskForPeer(t, k, db, keyA)

	status, body, err := fedTaskList(t, k, privA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	tasks, _ := body["tasks"].([]*kernel.PeerTaskView)
	if len(tasks) != 1 || tasks[0].ID != taskID {
		t.Fatalf("expected exactly the parked task, got %+v", tasks)
	}
	if len(tasks[0].AllowedInput) == 0 {
		t.Errorf("expected allowed_input so the peer can complete without reading the action")
	}

	// A different peer sees nothing: the task is not addressed to it.
	_, bodyB, err := fedTaskList(t, k, privB)
	if err != nil {
		t.Fatalf("list for peer B: %v", err)
	}
	if tasksB, _ := bodyB["tasks"].([]*kernel.PeerTaskView); len(tasksB) != 0 {
		t.Errorf("peer B must not see peer A's task, got %+v", tasksB)
	}
}

// TestFedTask_ListIsUserScoped: listing and completing must have the same granularity. A task
// addressed to one principal on the peer is listed to that principal, and never to another; the
// operator's kernel-level ask still sees every task it may complete, user-addressed ones included,
// which is the only place a peer-held task is visible to them.
func TestFedTask_ListIsUserScoped(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "peer-scoped")
	const alice, bob = "alice-remote-id", "bob-remote-id"
	forAlice := parkTaskForPeerUser(t, k, db, keyA, alice)
	forKernel := parkTaskForPeer(t, k, db, keyA)

	ids := func(body map[string]any) []string {
		tasks, _ := body["tasks"].([]*kernel.PeerTaskView)
		out := make([]string, len(tasks))
		for i, s := range tasks {
			out[i] = s.ID
		}
		return out
	}

	_, aliceBody, err := fedTaskListFor(t, k, privA, alice)
	if err != nil {
		t.Fatalf("list for alice: %v", err)
	}
	if got := ids(aliceBody); len(got) != 1 || got[0] != forAlice {
		t.Fatalf("alice must see exactly the task addressed to her, got %v", got)
	}

	_, bobBody, err := fedTaskListFor(t, k, privA, bob)
	if err != nil {
		t.Fatalf("list for bob: %v", err)
	}
	if got := ids(bobBody); len(got) != 0 {
		t.Errorf("bob must see none of alice's tasks, got %v", got)
	}

	// The operator asks as the whole kernel and sees both — omitting the user must never filter.
	_, allBody, err := fedTaskList(t, k, privA)
	if err != nil {
		t.Fatalf("kernel-level list: %v", err)
	}
	got := ids(allBody)
	if len(got) != 2 {
		t.Fatalf("the operator must see every task this kernel may complete, got %v", got)
	}
	tasks, _ := allBody["tasks"].([]*kernel.PeerTaskView)
	for _, s := range tasks {
		if s.ID == forAlice && s.RequiredCaller != alice {
			t.Errorf("a user-addressed task must name its principal, got %q", s.RequiredCaller)
		}
		if s.ID == forKernel && s.RequiredCaller != "" {
			t.Errorf("a kernel-addressed task names no principal, got %q", s.RequiredCaller)
		}
	}
}

// A signature-valid stranger is not provisioned an account by a read; it simply has no tasks.
func TestFedTask_ListUnknownKeyIsEmptyAndProvisionsNothing(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	status, body, err := fedTaskList(t, k, priv)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if tasks, _ := body["tasks"].([]*kernel.PeerTaskView); len(tasks) != 0 {
		t.Errorf("expected no tasks for a stranger, got %+v", tasks)
	}
	if u, _ := k.ReadAccountByKernelKey(context.Background(), cp); u != nil {
		t.Errorf("a read must not provision an account, but %s now exists", u.Handle)
	}
}

// Every way a task request is refused, in one table. Each case starts from the same fixture — a
// registered peer with one task parked for it — and mutates exactly one thing, so what is under
// test is the mutation and not the setup. These were seven near-identical tests.
func TestFedTask_RequestsAreRejected(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// run performs the rejected request. keyA/privA are the peer the task is parked for.
		run func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, taskID string) error
	}{
		{"bad list signature", func(t *testing.T, k *kernel.Kernel, keyA string, _ ed25519.PrivateKey, _ string) error {
			ts := time.Now().UTC().Format(time.RFC3339)
			_, _, err := handleFederationTaskList(k, ctx, keyA, ts, "bogus", "")
			return err
		}},
		{"bad complete signature", func(t *testing.T, k *kernel.Kernel, keyA string, _ ed25519.PrivateKey, taskID string) error {
			ts := time.Now().UTC().Format(time.RFC3339)
			_, _, err := handleFederationTaskComplete(k, ctx, keyA, ts, "idem-1", taskID, "bogus", []byte("{}"), "", false)
			return err
		}},
		{"stale timestamp", func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, _ string) error {
			stale := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
			sig, _ := testNet.SignTaskListPayload(privA, keyA, selfKey(t, k), stale, "")
			_, _, err := handleFederationTaskList(k, ctx, keyA, stale, sig, "")
			return err
		}},
		{"input does not match input_hash", func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, taskID string) error {
			ts := time.Now().UTC().Format(time.RFC3339)
			sig, _ := testNet.SignTaskPayload(privA, taskID, keyA, selfKey(t, k), "idem-t", ts, sha256HexBytes([]byte(`{"ok":true}`)), "", false)
			_, _, err := handleFederationTaskComplete(k, ctx, keyA, ts, "idem-t", taskID, sig, []byte(`{"ok":false}`), "", false)
			return err
		}},
		{"signed for another kernel", func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, taskID string) error {
			ts := time.Now().UTC().Format(time.RFC3339)
			sig, _ := testNet.SignTaskListPayload(privA, keyA, "some-other-kernels-key", ts, "")
			_, _, err := handleFederationTaskList(k, ctx, keyA, ts, sig, "")
			return err
		}},
		{"known peer that is not the required caller", func(t *testing.T, k *kernel.Kernel, _ string, _ ed25519.PrivateKey, taskID string) error {
			_, privB := fedPeer(t, k, "peer-b")
			_, _, err := fedTaskComplete(t, k, privB, taskID, "idem-b", []byte("{}"))
			return err
		}},
		{"unknown key", func(t *testing.T, k *kernel.Kernel, _ string, _ ed25519.PrivateKey, taskID string) error {
			_, privX, _ := ed25519.GenerateKey(rand.Reader)
			_, _, err := fedTaskComplete(t, k, privX, taskID, "idem-x", []byte("{}"))
			return err
		}},
		{"suspended peer", func(t *testing.T, k *kernel.Kernel, keyA string, privA ed25519.PrivateKey, taskID string) error {
			sys, _ := k.ReadUserByHandle(ctx, "sys")
			peer, _ := k.ReadAccountByKernelKey(ctx, keyA)
			if err := k.SuspendUser(ctx, sys.ID, peer.ID); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			if _, _, err := fedTaskList(t, k, privA); err == nil {
				t.Error("a suspended peer's list must also be refused")
			}
			_, _, err := fedTaskComplete(t, k, privA, taskID, "idem-s", []byte("{}"))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, k, db := newTestHTTPServerFull(t)
			defer srv.Close()
			keyA, privA := fedPeer(t, k, "peer-a")
			taskID := parkTaskForPeer(t, k, db, keyA)
			if err := tc.run(t, k, keyA, privA, taskID); err == nil {
				t.Error("expected the request to be rejected")
			}
		})
	}
}

// Rejections handled by OnTask before any handler runs.
func TestFedTask_OnTaskRejects(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	h := &fedHandlers{kernel: k}

	if got := h.OnTask(context.Background(), "", fed.TaskRequest{Kind: "nonsense"}).Status; got != kernel.ErrInvalidInput.HTTP {
		t.Errorf("unknown kind: got %d, want %d", got, kernel.ErrInvalidInput.HTTP)
	}
	// Defense in depth: a validly-signed request may not be replayed over a connection
	// authenticated as a different peer (mirrors OnCall).
	resp := h.OnTask(context.Background(), "different-connection-key",
		fed.TaskRequest{Kind: "list", Counterparty: "claimed-key"})
	if resp.Status != kernel.ErrUnauthenticated.HTTP {
		t.Errorf("mismatched connection key: got %d, want %d", resp.Status, kernel.ErrUnauthenticated.HTTP)
	}
}

func TestFedTask_CompleteSettlesAndIsIdempotent(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "peer-a")
	taskID := parkTaskForPeer(t, k, db, keyA)

	status, body, err := fedTaskComplete(t, k, privA, taskID, derivedTaskKey(t, k, taskID, []byte("{}")), []byte("{}"))
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

	// The task is done, and the completion transaction obeys the role law: the peer is the caller.
	ctx := context.Background()
	sys, _ := k.ReadUserByHandle(ctx, "sys")
	task, err := k.ReadTask(ctx, sys.ID, taskID)
	if err != nil {
		t.Fatalf("ReadTask: %v", err)
	}
	if task.Status != kernel.TaskDone {
		t.Errorf("expected task done, got %s", task.Status)
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
	statusR, bodyR, err := fedTaskComplete(t, k, privA, taskID, derivedTaskKey(t, k, taskID, []byte("{}")), []byte("{}"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if statusR != http.StatusOK || bodyR["tx_id"] != txID {
		t.Errorf("replay must return the stored result, got status=%d body=%v", statusR, bodyR)
	}

	// A fresh key against the now-done task fails: it is no longer waiting.
	if _, _, err := fedTaskComplete(t, k, privA, taskID, "idem-2", []byte("{}")); err == nil {
		t.Error("expected completing a done task to fail")
	}
}

// TestFedTask_PeerCompletesLocalAction: a peer completes a task whose target is a LOCAL action it
// could never call directly (§4). Visibility bound against the creator (@sys) at creation (§10),
// and completion re-checks only liveness — so the old rule's "a peer may be parked only for a
// public action" restriction is gone, without a peer ever gaining reach to the local action.
func TestFedTask_PeerCompletesLocalAction(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	ctx := context.Background()

	keyA, privA := fedPeer(t, k, "peer-a")
	peer, _ := k.ReadAccountByKernelKey(ctx, keyA)
	sys, _ := k.ReadUserByHandle(ctx, "sys")

	// A local action owned by @sys — the creator — and a task parked for the peer against it.
	actionID := parkTaskAction(t, k)
	local := kernel.VisibilityLocal
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: actionID, Visibility: &local}); err != nil {
		t.Fatal(err)
	}
	p := setupProcessHTTP(t, db, sys.ID, 0)
	task, err := k.CreateTask(ctx, setupTraceForProcess(t, db, p.ID), actionID, json.RawMessage(`{}`), kernel.Principal{AccountID: peer.ID})
	if err != nil {
		t.Fatalf("CreateTask parking a local action for a peer: %v", err)
	}

	status, body, err := fedTaskComplete(t, k, privA, task.ID, derivedTaskKey(t, k, task.ID, []byte("{}")), []byte("{}"))
	if err != nil || status != http.StatusOK {
		t.Fatalf("peer complete of a local-target task: status=%d err=%v", status, err)
	}
	if body["tx_id"] == "" || body["tx_id"] == nil {
		t.Fatalf("expected a settled completion, got %v", body)
	}
}

func TestFedTask_ListNotCrowdedOutByOwnProcesses(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	ctx := context.Background()

	keyA, privA := fedPeer(t, k, "peer-a")
	peer, _ := k.ReadAccountByKernelKey(ctx, keyA)
	sys, _ := k.ReadUserByHandle(ctx, "sys")

	// The task actually addressed to the peer, created FIRST so a newest-first cap would drop it.
	taskID := parkTaskForPeer(t, k, db, keyA)

	// 60 tasks inside processes the peer owns, awaiting a local user — visible to it via
	// CanListTask, but not completable by it.
	local, _ := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "local@k", Password: "pw12345678"})
	action := parkTaskAction(t, k)
	for i := 0; i < 60; i++ {
		p := setupProcessHTTP(t, db, peer.ID, 0)
		if _, err := k.CreateTask(ctx, setupTraceForProcess(t, db, p.ID), action, json.RawMessage(`{}`), kernel.Principal{AccountID: local.ID}); err != nil {
			t.Fatalf("seed task %d: %v", i, err)
		}
	}
	_ = sys

	_, body, err := fedTaskList(t, k, privA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	tasks, _ := body["tasks"].([]*kernel.PeerTaskView)
	if len(tasks) != 1 || tasks[0].ID != taskID {
		ids := make([]string, len(tasks))
		for i, s := range tasks {
			ids[i] = s.ID
		}
		t.Fatalf("expected exactly the peer-addressed task %s, got %v", taskID, ids)
	}
}

// The peer-facing view must carry the request and nothing about the requester. This asserts on the
// serialized JSON rather than the struct, because the defect it guards against was reusing a type
// whose EMBEDDED fields leaked — a field-by-field check on the wrong type would have passed.
// owner_handle is the sharp one: a local user identity crossing a kernel boundary is what §5's
// encapsulation exists to prevent.
func TestFedTask_ListDisclosesRequestNotRequester(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	keyA, privA := fedPeer(t, k, "peer-a")
	taskID := parkTaskForPeer(t, k, db, keyA)

	_, body, err := fedTaskList(t, k, privA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(wire)

	// Present: what the completer needs.
	for _, want := range []string{taskID, "allowed_input", "price", "created_at"} {
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
// retry tells the operator the task succeeded when it did not.
func TestFedTask_ReplayOfSettledFailureKeepsErrorStatus(t *testing.T) {
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

// The task payloads must not verify as any other signed Juice payload, and vice versa (§12).
func TestFedTask_SignatureDomainsAreDisjoint(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	const hash = "abc123"

	const rcpt = "recipient-kernel-key"
	taskSig, _ := testNet.SignTaskPayload(priv, "task-1", cp, rcpt, "idem-1", ts, hash, "", false)
	listSig, _ := testNet.SignTaskListPayload(priv, cp, rcpt, ts, "")
	callSig, _ := testNet.SignFederationPayload(priv, kernel.OutboundCall{ActionID: "act-id", ExpectedContractHash: "chash", IdempotencyKey: "idem-1", Commitment: "", Lottery: 0}, cp, rcpt, ts, hash)

	// A call signature must not pass as a task signature, nor either task kind as the other.
	if err := testNet.VerifyTaskSignature("task-1", cp, rcpt, "idem-1", ts, hash, "", false, callSig); err == nil {
		t.Error("a federation call signature must not verify as a task completion")
	}
	if err := testNet.VerifyTaskSignature("task-1", cp, rcpt, "idem-1", ts, hash, "", false, listSig); err == nil {
		t.Error("a task list signature must not verify as a task completion")
	}
	if err := testNet.VerifyTaskListSignature(cp, rcpt, ts, "", taskSig); err == nil {
		t.Error("a task completion signature must not verify as a task list")
	}
	if err := testNet.VerifyFederationSignature(cp, kernel.OutboundCall{ActionID: "act-id", ExpectedContractHash: "chash", IdempotencyKey: "idem-1", Commitment: "", Lottery: 0}, cp, rcpt, ts, hash, taskSig); err == nil {
		t.Error("a task signature must not verify as a federation call")
	}
	// Sanity: each verifies under its own domain.
	if err := testNet.VerifyTaskSignature("task-1", cp, rcpt, "idem-1", ts, hash, "", false, taskSig); err != nil {
		t.Errorf("task signature should verify in its own domain: %v", err)
	}
	if err := testNet.VerifyTaskListSignature(cp, rcpt, ts, "", listSig); err != nil {
		t.Errorf("list signature should verify in its own domain: %v", err)
	}
}
