// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
)

// newCapabilityKernel builds a flow-style HTTP server and returns the executor so the test can
// point its callback URL at the server — exactly what runServer does from the listen address.
func newCapabilityKernel(t *testing.T) (*httptest.Server, *kernel.Kernel, *store.DB) {
	t.Helper()
	db := newTestStore(t)
	cfg := testConfig("cap-test-secret")
	logger := log.Discard()
	box, err := newAESGCMBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, allowLocal: true, auth: newAuthenticator(box, db, true, cfg.ScriptTimeout)}
	k := newKernel(cfg, kernel.Dependencies{Store: db, HTTP: httpExec})
	k.SetSecretBox(box)
	if err := k.FirstBoot(context.Background(), "sys-pass", ""); err != nil {
		t.Fatal(err)
	}
	bootstrapSigning(t, k)

	srv := httptest.NewServer(mountFullRouter(&server{kernel: k, log: logger}))
	t.Cleanup(srv.Close)
	httpExec.callbackURL = srv.URL // production derives this from the bound listen address
	return srv, k, db
}

// capCallback POSTs to the kernel with the capability header (no JWT), returning the status.
func capCallback(cb, capTok, path string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", cb+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(capabilityHeader, capTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	out := make([]byte, 4096)
	n, _ := resp.Body.Read(out)
	return resp.StatusCode, out[:n]
}

// TestCapabilityComposition drives the whole feature end to end: a kind=http action, while its
// call is in flight, uses the trace-scoped capability to subcall another action (POST /v1/call)
// and to create a task (POST /v1/tasks). It asserts the role law, funding from the trace, and the
// task's parentage — the concrete C1–C8 acceptance.
func TestCapabilityComposition(t *testing.T) {
	srv, k, db := newCapabilityKernel(t)
	ctx := context.Background()

	provID, provTok := makeUser(t, k, "prov")
	subID, subTok := makeUser(t, k, "sub")
	callerID, callerTok := makeUser(t, k, "caller")
	giveCredits(t, k, callerID, 1000)

	// A leaf sub-action owned by @sub.
	leaf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(leaf.Close)
	createEnabledPublicAction(t, srv, subTok, "sub", "http", leaf.URL, "leaf sub-action", 10)

	// Capture what the composing endpoint's callbacks returned.
	var callStatus, taskStatus int
	var taskID string
	compose := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb := r.Header.Get(callbackHeader)
		capTok := r.Header.Get(capabilityHeader)
		// Subcall @sub/sub within our trace (juice.call ≡ /v1/call).
		callStatus, _ = capCallback(cb, capTok, "/v1/call", map[string]any{"action": "sub@k/sub", "args": map[string]any{}})
		// Create a task addressed to @caller (juice.task_create ≡ /v1/tasks, no trace_id).
		var sBody []byte
		taskStatus, sBody = capCallback(cb, capTok, "/v1/tasks", map[string]any{
			"action": "sub@k/sub", "required_caller": "caller@k", "partial_args": map[string]any{},
		})
		var sv map[string]any
		_ = json.Unmarshal(sBody, &sv)
		if id, ok := sv["id"].(string); ok {
			taskID = id
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	t.Cleanup(compose.Close)
	createEnabledPublicAction(t, srv, provTok, "compose", "http", compose.URL, "composing action", 100)

	reply := runAction(t, srv, callerTok, "prov@k/compose", map[string]any{})

	if callStatus != http.StatusOK {
		t.Fatalf("/v1/call callback status = %d, want 200", callStatus)
	}
	if taskStatus != http.StatusCreated {
		t.Fatalf("/v1/tasks callback status = %d, want 201", taskStatus)
	}

	// Role law of the subcall: caller = the composing action's owner (§6/§9), target = sub owner,
	// owner = the process owner (the run caller).
	txs, err := db.ListTransactions(ctx, kernel.TxFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var sub *kernel.Transaction
	for _, tx := range txs {
		if tx.ActionName == "sub" && tx.Status == kernel.TxSuccess {
			sub = tx
		}
	}
	if sub == nil {
		t.Fatal("no successful subcall transaction for @sub/sub")
	}
	if sub.CallerUserID != provID {
		t.Errorf("subcall caller = %s, want %s (compose owner)", sub.CallerUserID, provID)
	}
	if sub.TargetUserID != subID {
		t.Errorf("subcall target = %s, want %s (sub owner)", sub.TargetUserID, subID)
	}
	if sub.OwnerUserID != callerID {
		t.Errorf("subcall owner = %s, want %s (process owner)", sub.OwnerUserID, callerID)
	}

	// The task was created under the capability, parented to the composing action's trace and
	// parked from it (§9/§10).
	if taskID == "" {
		t.Fatal("no task id returned from /v1/tasks callback")
	}
	task, err := db.ReadTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ParentTraceID == nil || *task.ParentTraceID != reply.TraceID {
		t.Errorf("task parent_trace_id = %v, want %s (the composing action's trace)", task.ParentTraceID, reply.TraceID)
	}
	if task.RequiredCallerUserID != callerID {
		t.Errorf("task required_caller = %s, want %s", task.RequiredCallerUserID, callerID)
	}

	// Funding from the trace: the composing call's price (100) bounded the subcall (10) and the
	// task park (10); the remaining 80 is its taxable value added.
	var comp *kernel.Transaction
	for _, tx := range txs {
		if tx.ActionName == "compose" {
			comp = tx
		}
	}
	if comp == nil {
		t.Fatal("no compose transaction")
	}
	if comp.Net+comp.Fee != 80 {
		t.Errorf("compose taxable net+fee = %d, want 80 (100 − subcall 10 − task park 10)", comp.Net+comp.Fee)
	}
}

// TestCapabilityConcurrentCallbacksBounded proves the subtree bound holds under concurrent
// composition: a price-100 action fires three concurrent subcalls of price 40; the atomic,
// fenced fund-move (§9) lets at most two commit (80 ≤ 100) and rejects the third, and the caller
// is charged exactly the advertised price — no over-spend.
func TestCapabilityConcurrentCallbacksBounded(t *testing.T) {
	srv, k, db := newCapabilityKernel(t)

	_, provTok := makeUser(t, k, "prov")
	_, subTok := makeUser(t, k, "sub")
	callerID, callerTok := makeUser(t, k, "caller")
	giveCredits(t, k, callerID, 1000)

	leaf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(leaf.Close)
	createEnabledPublicAction(t, srv, subTok, "sub", "http", leaf.URL, "leaf", 40)

	var mu sync.Mutex
	statuses := map[int]int{}
	compose := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb, capTok := r.Header.Get(callbackHeader), r.Header.Get(capabilityHeader)
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				st, _ := capCallback(cb, capTok, "/v1/call", map[string]any{"action": "sub@k/sub", "args": map[string]any{}})
				mu.Lock()
				statuses[st]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		_ = json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	t.Cleanup(compose.Close)
	createEnabledPublicAction(t, srv, provTok, "compose", "http", compose.URL, "composer", 100)

	before := readAvailable(t, db, callerID)
	runAction(t, srv, callerTok, "prov@k/compose", map[string]any{})

	if statuses[http.StatusOK] != 2 {
		t.Errorf("successful subcalls = %d, want 2 (80 ≤ 100 < 120)", statuses[http.StatusOK])
	}
	// The caller is charged exactly the advertised price; the subtree never over-spent.
	after := readAvailable(t, db, callerID)
	if before-after != 100 {
		t.Errorf("caller charged %d, want exactly the advertised price 100", before-after)
	}
}

func readAvailable(t *testing.T, db *store.DB, userID string) int64 {
	t.Helper()
	u, err := db.ReadUser(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return u.Available
}

// TestCapabilityTaskCompleteIsTraceConfined proves the capability cannot complete a task outside
// its own trace (§9 "no other trace"). The endpoint runs in ITS OWN process while a task addressed
// to the same owner waits in a victim's process: without confinement, holding the capability would
// fire that task — spending funds the victim committed, at a time the attacker chooses. The task
// stays waiting and the victim's balance is untouched.
func TestCapabilityTaskCompleteIsTraceConfined(t *testing.T) {
	srv, k, db := newCapabilityKernel(t)
	ctx := context.Background()

	provID, provTok := makeUser(t, k, "prov")
	victimID, victimTok := makeUser(t, k, "victim")
	giveCredits(t, k, provID, 1000)
	giveCredits(t, k, victimID, 1000)

	// A leaf the parked task will target, owned by the victim so its price is the victim's to spend.
	leaf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(leaf.Close)
	createEnabledPublicAction(t, srv, victimTok, "leaf", "http", leaf.URL, "victim leaf", 10)

	// The victim's own composing action parks a task addressed to @prov, inside the VICTIM's process.
	var victimTaskID string
	victimCompose := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, body := capCallback(r.Header.Get(callbackHeader), r.Header.Get(capabilityHeader), "/v1/tasks",
			map[string]any{"action": "victim@k/leaf", "required_caller": "prov@k", "partial_args": map[string]any{}})
		var sv map[string]any
		_ = json.Unmarshal(body, &sv)
		victimTaskID, _ = sv["id"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"parked": true})
	}))
	t.Cleanup(victimCompose.Close)
	createEnabledPublicAction(t, srv, victimTok, "park", "http", victimCompose.URL, "victim parker", 100)
	runAction(t, srv, victimTok, "victim@k/park", map[string]any{})
	if victimTaskID == "" {
		t.Fatal("setup: the victim's process did not park a task")
	}

	// @prov's own action, running in @prov's process, tries to complete that task with its capability.
	var completeStatus int
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completeStatus, _ = capCallback(r.Header.Get(callbackHeader), r.Header.Get(capabilityHeader),
			"/v1/tasks/"+victimTaskID+"/complete", map[string]any{"args": map[string]any{}})
		_ = json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	t.Cleanup(attacker.Close)
	createEnabledPublicAction(t, srv, provTok, "hook", "http", attacker.URL, "attacker hook", 50)

	before := readAvailable(t, db, victimID)
	runAction(t, srv, provTok, "prov@k/hook", map[string]any{})

	if completeStatus != http.StatusForbidden {
		t.Errorf("cross-trace capability completion: status = %d, want 403 (unauthorized)", completeStatus)
	}
	task, err := db.ReadTask(ctx, victimTaskID)
	if err != nil {
		t.Fatalf("ReadTask: %v", err)
	}
	if task.Status != kernel.TaskWaiting {
		t.Errorf("the victim's task must stay waiting, got %s", task.Status)
	}
	if after := readAvailable(t, db, victimID); after != before {
		t.Errorf("the victim's balance moved: %d → %d", before, after)
	}
}

// TestCapabilityCannotDriveFederation: a capability is local to its trace and carries no supervision
// authority (§9), so it completes only a task its own trace parked — never one held on a peer, even
// one addressed to the action's owner, which a completion would sign and send as that owner. The
// assertion is that NOTHING is dispatched, not merely that the call errors.
func TestCapabilityCannotDriveFederation(t *testing.T) {
	srv, k, db := newCapabilityKernel(t)

	// A recording transport behind a real adapter: any federation dispatch shows up in lastTask.
	f := &fakeFed{taskBody: json.RawMessage(`{"tx_id":"tx-peer"}`), taskStatus: 200}
	self, _ := k.GetConfig(context.Background(), configKeySigningPublic)
	adapter := newFedAdapter(self, nil, nil)
	adapter.SetTransport(f)
	k.SetFederation(adapter)

	provID, provTok := makeUser(t, k, "prov")
	callerID, callerTok := makeUser(t, k, "caller")
	giveCredits(t, k, callerID, 1000)

	// A task a peer holds for the action's owner, delivered to the owner's mailbox.
	peerKey := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	heldID := uuid.New().String()
	if err := db.RecordTaskNotice(context.Background(), peerKey, provID, &kernel.TaskNotice{
		ID: heldID, Revision: 1, Status: kernel.TaskWaiting, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	var status int
	var body []byte
	compose := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body = capCallback(r.Header.Get(callbackHeader), r.Header.Get(capabilityHeader),
			"/v1/tasks/"+heldID+"/complete", map[string]any{"args": map[string]any{}})
		_ = json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	t.Cleanup(compose.Close)
	createEnabledPublicAction(t, srv, provTok, "hook", "http", compose.URL, "hook", 50)

	runAction(t, srv, callerTok, "prov@k/hook", map[string]any{})

	if status < 400 {
		t.Errorf("capability completing a peer-held task: status = %d, want a refusal; body=%s", status, body)
	}
	if f.lastTask.Kind != "" {
		t.Errorf("a capability must not reach federation at all; dispatched %+v", f.lastTask)
	}
}

// TestCapabilityRejectedOnRun proves /v1/run never accepts a capability: the route is JWT-only,
// so a capability-only request cannot reach the wallet/BeginRun path (C8).
func TestCapabilityRejectedOnRun(t *testing.T) {
	srv, _, _ := newCapabilityKernel(t)
	resp := httpDoWithHeaders(t, srv, "POST", "/v1/run",
		map[string]any{"action": "sys@k/whatever", "args": map[string]any{}}, "",
		map[string]string{capabilityHeader: "anything.sig"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1/run with a capability and no JWT: status = %d, want 401", resp.StatusCode)
	}
}
