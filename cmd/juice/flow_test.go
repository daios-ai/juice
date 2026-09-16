// SPDX-License-Identifier: AGPL-3.0-only

package main

// flow_test.go — §15 user-story integration tests.
//
// Each TestFlow_* function exercises the kernel through its HTTP surface
// (httptest.Server with a real SQLite-backed kernel).  Tests are independent;
// each calls newTestHTTPServerFull to get a clean DB.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
)

// ---- flowScriptExec — in-process script executor for WASM-free flow tests ----

// flowScriptExec dispatches by artifact name to registered handler functions.
// Compile stores the source as the artifact (identity); Execute calls the handler.
type flowScriptExec struct {
	handlers map[string]func(ctx context.Context, input []byte, host kernel.HostFunctions) ([]byte, error)
}

func (e *flowScriptExec) Compile(_ context.Context, src []byte) ([]byte, string, error) {
	key := string(src)
	n := len(key)
	if n > 8 {
		n = 8
	}
	return src, "fakehash-" + key[:n], nil
}

func (e *flowScriptExec) Execute(ctx context.Context, artifact []byte, input []byte, host kernel.HostFunctions) ([]byte, error) {
	name := string(artifact)
	if h, ok := e.handlers[name]; ok {
		return h(ctx, input, host)
	}
	// Default: echo the input back.
	return input, nil
}

// newFlowKernel builds a fully bootstrapped kernel with a custom ScriptExecutor.
// It mirrors newTestHTTPServerFull but lets the caller inject a ScriptExecutor.
func newFlowKernel(t *testing.T, exec kernel.ScriptExecutor) (*httptest.Server, *kernel.Kernel, *store.DB) {
	t.Helper()
	db := newTestStore(t)

	cfg := testConfig("flow-test-secret")
	cfg.AllowLocalSources = true
	logger := log.Discard()
	// Credential encryption is mandatory (§8); the production binary always wires a box to
	// both the kernel and the HTTP executor, so the flow harness does too.
	box, err := newAESGCMBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, auth: newAuthenticator(box, db, true, cfg.ScriptTimeout)}
	k := newKernel(cfg, kernel.Dependencies{Store: db, Scripts: exec, HTTP: httpExec})
	k.SetSecretBox(box)

	if err := k.FirstBoot(context.Background(), "sys-pass", ""); err != nil {
		t.Fatal(err)
	}
	bootstrapSigning(t, k)

	srv := &server{kernel: k, log: logger}
	return httptest.NewServer(mountFullRouter(srv)), k, db
}

// ---- helpers used across flow tests ----

// runAction calls POST /v1/run and decodes the reply. Fatals on non-200.
func runAction(t *testing.T, srv *httptest.Server, tok, actionRef string, args map[string]any) kernel.CallReply {
	t.Helper()
	resp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": actionRef,
		"args":   args,
	}, tok)
	if resp.StatusCode != http.StatusOK {
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		t.Fatalf("POST /v1/run %s: expected 200, got %d — %v", actionRef, resp.StatusCode, body)
	}
	var reply kernel.CallReply
	decodeResponse(t, resp, &reply)
	return reply
}

// createEnabledPublicAction creates, enables, and makes public an action of the given kind.
// Returns action ID.
func createEnabledPublicAction(t *testing.T, srv *httptest.Server, ownerTok, name, kind, source, description string, price int64) string {
	t.Helper()
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": name, "kind": kind, "price": price, "source": source,
		"description":   description,
		"input_schema":  minSchema,
		"output_schema": minSchema,
	}, ownerTok)
	if cr.StatusCode != http.StatusCreated {
		cr.Body.Close()
		t.Fatalf("create %s action %s: expected 201, got %d", kind, name, cr.StatusCode)
	}
	var act map[string]any
	decodeResponse(t, cr, &act)
	id := act["id"].(string)
	httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": id}, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": id, "visibility": "public"}, ownerTok).Body.Close()
	return id
}

// createPublicAction creates, enables, and makes public an HTTP action. Returns action ID.
func createPublicAction(t *testing.T, srv *httptest.Server, backendURL, ownerTok, name string, price int64) string {
	t.Helper()
	return createEnabledPublicAction(t, srv, ownerTok, name, "http", backendURL, "flow test action", price)
}

// getTxList returns transaction list for the caller.
func getTxList(t *testing.T, srv *httptest.Server, tok string) []map[string]any {
	t.Helper()
	resp := httpDo(t, srv, "GET", "/v1/transactions", nil, tok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /v1/transactions: expected 200, got %d", resp.StatusCode)
	}
	var txs []map[string]any
	decodeResponse(t, resp, &txs)
	return txs
}

func TestFlow_WebhookCompleteStep(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"webhook_result": "ok"})
	}))
	defer backend.Close()

	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "wh-owner")
	webhookID, webhookTok := makeUser(t, k, "wh-webhook-sys")
	_ = webhookID

	giveCredits(t, k, ownerID, 500)

	// Action targeted by the step.
	actionID := createPublicAction(t, srv, backend.URL, ownerTok, "wh-action", 0)

	// Create process and step addressed to the webhook system.
	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action":          actionID,
		"required_caller": "wh-webhook-sys",
		"partial_args":    map[string]any{"purchase_id": "abc123"},
	}, ownerTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	stepID := step["id"].(string)

	// Webhook system completes the step.
	complResp := httpDo(t, srv, "POST", "/v1/steps/"+stepID+"/complete", map[string]any{
		"args": map[string]any{"payment_confirmed": true},
	}, webhookTok)
	if complResp.StatusCode != http.StatusOK {
		complResp.Body.Close()
		t.Fatalf("webhook complete step: expected 200, got %d", complResp.StatusCode)
	}
	var reply map[string]any
	decodeResponse(t, complResp, &reply)
	txID, _ := reply["tx_id"].(string)
	if txID == "" {
		t.Fatal("expected tx_id in webhook complete reply")
	}

	// Transaction must reflect webhook as caller (role law).
	txResp := httpDo(t, srv, "GET", "/v1/transactions/"+txID, nil, webhookTok)
	if txResp.StatusCode != http.StatusOK {
		txResp.Body.Close()
		t.Fatalf("get tx: expected 200, got %d", txResp.StatusCode)
	}
	var tx struct {
		kernel.Transaction
		CallerHandle string `json:"caller_handle"`
	}
	decodeResponse(t, txResp, &tx)
	if tx.CallerHandle != "wh-webhook-sys" {
		t.Errorf("tx caller_handle: got %s, want @wh-webhook-sys (webhook)", tx.CallerHandle)
	}
	if tx.Status != kernel.TxSuccess {
		t.Errorf("tx status: got %s, want success", tx.Status)
	}
}

func TestFlow_ReconcileNetAmounts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	defer backend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "rec-owner")
	callerID, callerTok := makeUser(t, k, "rec-caller")
	giveCredits(t, k, callerID, 1000)

	const price int64 = 50
	createPublicAction(t, srv, backend.URL, ownerTok, "rec-action", price)

	// Run 3 times.
	const runs = 3
	for i := 0; i < runs; i++ {
		r := runAction(t, srv, callerTok, "rec-owner/rec-action", map[string]any{})
		if r.TxID == "" {
			t.Fatalf("run %d: expected tx_id", i)
		}
	}

	// Sum net amounts from owner's tx list.
	ownerTxs := getTxList(t, srv, ownerTok)
	var sumNet int64
	for _, tx := range ownerTxs {
		if tx["action_name"] == "rec-action" && tx["status"] == "success" {
			sumNet += int64(tx["net"].(float64))
		}
	}

	// Owner's actual balance.
	ctx := context.Background()
	ownerUser, _ := k.ReadUser(ctx, ownerID)
	if ownerUser.Available != sumNet {
		t.Errorf("owner balance %d does not equal sum of tx net amounts %d", ownerUser.Available, sumNet)
	}
	if sumNet == 0 {
		t.Error("expected non-zero net amounts")
	}
}

func TestFlow_UpstreamAuthSecrecy(t *testing.T) {
	const secretToken = "super-secret-bearer-token-xyz"

	// Backend that echoes the Authorization header for verification.
	var receivedAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "auth-owner")
	callerID, callerTok := makeUser(t, k, "auth-caller")
	giveCredits(t, k, callerID, 100)

	// Create action with bearer auth credentials via HTTP API.
	createResp := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name":          "secured-action",
		"kind":          "http",
		"price":         0,
		"description":   "action with upstream auth",
		"input_schema":  map[string]any{"type": "object"},
		"output_schema": map[string]any{"type": "object"},
		"source":        backend.URL,
		"auth": map[string]any{
			"scheme":  "bearer",
			"secrets": map[string]any{"token": secretToken},
		},
	}, ownerTok)
	if createResp.StatusCode != http.StatusCreated {
		createResp.Body.Close()
		t.Fatalf("create action: expected 201, got %d", createResp.StatusCode)
	}
	var actionBody map[string]any
	decodeResponse(t, createResp, &actionBody)
	actionID := actionBody["id"].(string)

	// Activate the action and make it public.
	pubTrue := kernel.VisibilityPublic
	if _, err := k.UpdateAction(context.Background(), ownerID, kernel.UpdateActionRequest{ID: actionID, Visibility: &pubTrue}); err != nil {
		t.Fatalf("make public: %v", err)
	}
	if err := k.SetActive(context.Background(), ownerID, actionID, true); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// GET /v1/actions/{id} must NOT contain the secret.
	getResp := httpDo(t, srv, "GET", "/v1/actions/"+actionID, nil, ownerTok)
	if getResp.StatusCode != http.StatusOK {
		getResp.Body.Close()
		t.Fatalf("get action: expected 200, got %d", getResp.StatusCode)
	}
	var actionRaw json.RawMessage
	decodeResponse(t, getResp, &actionRaw)
	if strings.Contains(string(actionRaw), secretToken) {
		t.Error("R9 violation: secret token appears in GET /v1/actions response")
	}

	// The signed manifest (served to peers over the transport, §13) must NOT contain the secret.
	manifest, err := k.GetActionManifest(context.Background(), actionID)
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	manifestRaw, _ := json.Marshal(manifest)
	if strings.Contains(string(manifestRaw), secretToken) {
		t.Error("R9 violation: secret token appears in manifest")
	}

	// Run the action — backend should receive the Authorization header.
	runResult := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "auth-owner/secured-action",
		"args":   map[string]any{},
	}, callerTok)
	if runResult.StatusCode != http.StatusOK {
		var body map[string]any
		json.NewDecoder(runResult.Body).Decode(&body)
		runResult.Body.Close()
		t.Fatalf("run: expected 200, got %d — %v", runResult.StatusCode, body)
	}
	var runBody map[string]any
	decodeResponse(t, runResult, &runBody)
	txID := runBody["tx_id"].(string)

	// Backend must have received the bearer token.
	if receivedAuth != "Bearer "+secretToken {
		t.Errorf("backend did not receive correct auth header: got %q, want %q",
			receivedAuth, "Bearer "+secretToken)
	}

	// GET /v1/transactions/{id} must NOT contain the secret.
	txResp := httpDo(t, srv, "GET", "/v1/transactions/"+txID, nil, callerTok)
	if txResp.StatusCode != http.StatusOK {
		txResp.Body.Close()
		t.Fatalf("get tx: expected 200, got %d", txResp.StatusCode)
	}
	var txRaw json.RawMessage
	decodeResponse(t, txResp, &txRaw)
	if strings.Contains(string(txRaw), secretToken) {
		t.Error("R9 violation: secret token appears in transaction response")
	}
}

// ============================================================
// — Missing §15 user-story flows —
// ============================================================

func TestFlow_ThreePartyRoleLaw(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	defer backend.Close()

	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ctx := context.Background()
	pID, pTok := makeUser(t, k, "3p-owner")
	_, cTok := makeUser(t, k, "3p-caller")
	_, aTok := makeUser(t, k, "3p-provider")

	giveCredits(t, k, pID, 500)

	// A creates and activates a public action.
	actID := createPublicAction(t, srv, backend.URL, aTok, "3p-action", 0)
	_ = actID

	// P creates a process and a trace for the step funding source.
	p := setupProcessHTTP(t, db, pID, 200)
	traceID := setupTraceForProcess(t, db, p.ID)

	// P creates a step addressed to C, pointing at A's action.
	aAction, err := k.ReadActionByOwnerName(ctx, func() string {
		u, _ := k.ReadUserByHandle(ctx, "3p-provider")
		return u.ID
	}(), "3p-action")
	if err != nil || aAction == nil {
		t.Fatalf("read 3p-action: %v", err)
	}
	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action":          aAction.ID,
		"required_caller": "3p-caller",
		"partial_args":    map[string]any{},
	}, pTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	stepID := step["id"].(string)

	// C completes the step.
	complResp := httpDo(t, srv, "POST", "/v1/steps/"+stepID+"/complete",
		map[string]any{"args": map[string]any{}}, cTok)
	if complResp.StatusCode != http.StatusOK {
		var body map[string]any
		json.NewDecoder(complResp.Body).Decode(&body)
		complResp.Body.Close()
		t.Fatalf("complete step: expected 200, got %d — %v", complResp.StatusCode, body)
	}
	var complBody map[string]any
	decodeResponse(t, complResp, &complBody)
	txID, _ := complBody["tx_id"].(string)
	if txID == "" {
		t.Fatal("expected tx_id in complete-step reply")
	}

	// All three parties can read the transaction.
	for _, tok := range []string{pTok, cTok, aTok} {
		r := httpDo(t, srv, "GET", "/v1/transactions/"+txID, nil, tok)
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			t.Errorf("GET /v1/transactions/%s: expected 200 for one party, got %d", txID, r.StatusCode)
			continue
		}
		var tx map[string]any
		decodeResponse(t, r, &tx)
		if tx["owner_handle"] != "3p-owner" {
			t.Errorf("tx.owner_handle: got %v, want @3p-owner", tx["owner_handle"])
		}
		if tx["caller_handle"] != "3p-caller" {
			t.Errorf("tx.caller_handle: got %v, want @3p-caller", tx["caller_handle"])
		}
		if tx["target_handle"] != "3p-provider" {
			t.Errorf("tx.target_handle: got %v, want @3p-provider", tx["target_handle"])
		}
		// The raw party UUIDs are no longer exposed (a user is addressed by @handle, §14).
		if _, ok := tx["owner_user_id"]; ok {
			t.Error("tx response should not expose owner_user_id")
		}
	}
}

func TestFlow_AccountSelfService(t *testing.T) {
	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	userID, userTok := makeUser(t, k, "self-user")
	_ = userID

	// Update description.
	putResp := httpDo(t, srv, "PUT", "/v1/me", map[string]any{"description": "self-service user"}, userTok)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("update description: expected 200, got %d", putResp.StatusCode)
	}

	// Verify description via GET /v1/me.
	meResp := httpDo(t, srv, "GET", "/v1/me", nil, userTok)
	var meBody map[string]any
	decodeResponse(t, meResp, &meBody)
	if meBody["description"] != "self-service user" {
		t.Errorf("description after update: got %v, want 'self-service user'", meBody["description"])
	}

	// Change password.
	pwResp := httpDo(t, srv, "PUT", "/v1/me", map[string]any{
		"current_password": "pass", "password": "changed123",
	}, userTok)
	pwResp.Body.Close()
	if pwResp.StatusCode != http.StatusOK {
		t.Fatalf("change password: expected 200, got %d", pwResp.StatusCode)
	}

	// Old password rejected.
	if status, _ := httpLogin(t, srv, "self-user", "pass"); status == http.StatusOK {
		t.Error("old password should be rejected")
	}

	// New password accepted.
	if status, _ := httpLogin(t, srv, "self-user", "changed123"); status != http.StatusOK {
		t.Errorf("new password login: expected 200, got %d", status)
	}
}
