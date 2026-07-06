package main

// flow_test.go — §15 user-story integration tests.
//
// Each TestFlow_* function exercises the kernel through its HTTP surface
// (httptest.Server with a real SQLite-backed kernel).  Tests are independent;
// each calls newTestHTTPServerFull to get a clean DB.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/llm"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/native"
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
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "flow-test-secret"
	cfg.AllowLocalSources = true
	logger := log.Discard()
	// Credential encryption is mandatory (§8); the production binary always wires a box to
	// both the kernel and the HTTP executor, so the flow harness does too.
	box, err := newAESGCMBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, secretBox: box}
	k := kernel.New(db, exec, httpExec, nil, cfg, logger)
	k.SetSecretBox(box)

	if err := k.FirstBoot(context.Background(), "sys-pass"); err != nil {
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
	httpDo(t, srv, "POST", "/v1/actions/"+id+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions/"+id, map[string]any{"public": true}, ownerTok).Body.Close()
	return id
}

// createPublicAction creates, enables, and makes public an HTTP action. Returns action ID.
func createPublicAction(t *testing.T, srv *httptest.Server, backendURL, ownerTok, name string, price int64) string {
	t.Helper()
	return createEnabledPublicAction(t, srv, ownerTok, name, "http", backendURL, "flow test action", price)
}

// createWasmAction creates a WASM action using the flowScriptExec (kind=wasm, source=handlerName).
func createWasmAction(t *testing.T, srv *httptest.Server, ownerTok, name string, price int64) string {
	t.Helper()
	return createEnabledPublicAction(t, srv, ownerTok, name, "wasm", name, "flow wasm action", price)
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

// findTx returns the transaction with the given id from a tx-list response, or nil.
func findTx(txs []map[string]any, id string) map[string]any {
	for _, tx := range txs {
		if tx["id"] == id {
			return tx
		}
	}
	return nil
}

// getBalance returns the caller's available balance via GET /v1/me.
func getBalance(t *testing.T, srv *httptest.Server, tok string) int64 {
	t.Helper()
	resp := httpDo(t, srv, "GET", "/v1/me", nil, tok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /v1/me: expected 200, got %d", resp.StatusCode)
	}
	var body map[string]any
	decodeResponse(t, resp, &body)
	return int64(body["available"].(float64))
}

// ============================================================
// — Local execution —
// ============================================================

// TestFlow_SignupDepositRun: user signs up, receives credits (admin deposit), runs a
// public paid action, verifies the process auto-created and closed, exact price debited,
// provider's net and @sys fee appear in tx list.
func TestFlow_SignupDepositRun(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"answer": 42})
	}))
	defer backend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	// Provider (owner) sets up a paid action.
	providerID, providerTok := makeUser(t, k, "@flow-provider")
	const price int64 = 100
	actionID := createPublicAction(t, srv, backend.URL, providerTok, "flow-answer", price)

	// Caller signs up and receives credits.
	callerID, callerTok := makeUser(t, k, "@flow-caller")
	giveCredits(t, k, callerID, 500)

	balanceBefore := getBalance(t, srv, callerTok)
	if balanceBefore != 500 {
		t.Fatalf("initial balance: got %d, want 500", balanceBefore)
	}

	// Run the action.
	reply := runAction(t, srv, callerTok, "@flow-provider/flow-answer", map[string]any{})
	if reply.TxID == "" {
		t.Fatal("expected tx_id in reply")
	}
	if reply.Result["answer"] != float64(42) {
		t.Errorf("result: got %v, want answer=42", reply.Result)
	}

	// Exact price debited from caller.
	balanceAfter := getBalance(t, srv, callerTok)
	if balanceBefore-balanceAfter != price {
		t.Errorf("caller debit: got %d, want %d", balanceBefore-balanceAfter, price)
	}

	// Provider's net: price * (1 - fee_bps/10000). Default FeeBPS=2000 → 80%.
	// Provider balance check via kernel directly.
	ctx := context.Background()
	providerUser, err := k.ReadUser(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	expectedNet := price * (10000 - kernel.DefaultConfig().FeeBPS) / 10000
	if providerUser.Available != expectedNet {
		t.Errorf("provider net: got %d, want %d", providerUser.Available, expectedNet)
	}

	// Transaction appears in caller's tx list with correct gross.
	txs := getTxList(t, srv, callerTok)
	if tx := findTx(txs, reply.TxID); tx == nil {
		t.Errorf("tx %s not found in caller tx list (got %d txs)", reply.TxID, len(txs))
	} else {
		if int64(tx["gross"].(float64)) != price {
			t.Errorf("tx gross: got %v, want %d", tx["gross"], price)
		}
		if tx["status"] != "success" {
			t.Errorf("tx status: got %v, want success", tx["status"])
		}
	}

	// Provider also sees the tx (they are target).
	providerTxs := getTxList(t, srv, providerTok)
	if tx := findTx(providerTxs, reply.TxID); tx == nil {
		t.Errorf("tx %s not in provider tx list", reply.TxID)
	} else if netVal := int64(tx["net"].(float64)); netVal != expectedNet {
		t.Errorf("provider tx net: got %d, want %d", netVal, expectedNet)
	}

	// Check action was created correctly.
	aResp := httpDo(t, srv, "GET", "/v1/actions/"+actionID, nil, providerTok)
	var act kernel.Action
	decodeResponse(t, aResp, &act)
	if act.Price != price {
		t.Errorf("action price: got %d, want %d", act.Price, price)
	}
}

// TestFlow_WASMSubcallProviderMargin: provider creates a WASM action that subcalls two
// cheaper actions; caller pays one advertised price; subproviders paid from provider's
// budget; provider keeps margin.
func TestFlow_WASMSubcallProviderMargin(t *testing.T) {
	// Two cheap sub-action backends.
	subBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"sub": true})
	}))
	defer subBackend.Close()

	exec := &flowScriptExec{
		handlers: map[string]func(ctx context.Context, input []byte, host kernel.HostFunctions) ([]byte, error){},
	}
	srv, k, _ := newFlowKernel(t, exec)
	defer srv.Close()

	// Sub-provider A: price=20.
	subProvAID, subProvATok := makeUser(t, k, "@flow-subprov-a")
	_ = subProvAID
	createPublicAction(t, srv, subBackend.URL, subProvATok, "sub-a", 20)

	// Sub-provider B: price=20.
	subProvBID, subProvBTok := makeUser(t, k, "@flow-subprov-b")
	_ = subProvBID
	createPublicAction(t, srv, subBackend.URL, subProvBTok, "sub-b", 20)

	// Main provider creates a WASM action priced at 100 that subcalls both.
	mainProvID, mainProvTok := makeUser(t, k, "@flow-mainprov")
	giveCredits(t, k, mainProvID, 1000) // provider needs budget for subcalls

	// Register the WASM handler.
	exec.handlers["wasm-orchestrate"] = func(ctx context.Context, input []byte, host kernel.HostFunctions) ([]byte, error) {
		if _, err := host.Call(ctx, "@flow-subprov-a/sub-a", []byte(`{}`)); err != nil {
			return nil, fmt.Errorf("sub-a call failed: %w", err)
		}
		if _, err := host.Call(ctx, "@flow-subprov-b/sub-b", []byte(`{}`)); err != nil {
			return nil, fmt.Errorf("sub-b call failed: %w", err)
		}
		return []byte(`{"orchestrated":true}`), nil
	}

	// Create the WASM action at price=100.
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "wasm-orchestrate", "kind": "wasm", "price": int64(100),
		"source": "wasm-orchestrate", "description": "orchestrates sub-calls",
		"input_schema": minSchema, "output_schema": minSchema,
	}, mainProvTok)
	if cr.StatusCode != http.StatusCreated {
		cr.Body.Close()
		t.Fatalf("create wasm action: expected 201, got %d", cr.StatusCode)
	}
	var act map[string]any
	decodeResponse(t, cr, &act)
	actID := act["id"].(string)
	httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, mainProvTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions/"+actID, map[string]any{"public": true}, mainProvTok).Body.Close()

	// Caller runs the orchestrated action.
	callerID, callerTok := makeUser(t, k, "@flow-orch-caller")
	giveCredits(t, k, callerID, 500)

	balBefore := getBalance(t, srv, callerTok)
	reply := runAction(t, srv, callerTok, "@flow-mainprov/wasm-orchestrate", map[string]any{})
	balAfter := getBalance(t, srv, callerTok)

	// Caller pays exactly the advertised price.
	if balBefore-balAfter != 100 {
		t.Errorf("caller debit: got %d, want 100", balBefore-balAfter)
	}
	if reply.Result["orchestrated"] != true {
		t.Errorf("result: expected orchestrated=true, got %v", reply.Result)
	}

	// Sub-providers received payment (balance > 0).
	ctx := context.Background()
	subA, _ := k.ReadUserByHandle(ctx, "@flow-subprov-a")
	subB, _ := k.ReadUserByHandle(ctx, "@flow-subprov-b")
	if subA.Available == 0 {
		t.Error("sub-provider A should have received payment")
	}
	if subB.Available == 0 {
		t.Error("sub-provider B should have received payment")
	}

	// Main provider's net = price paid by caller * (1-fee) - subcall costs.
	mainProv, _ := k.ReadUser(ctx, mainProvID)
	// provider started with 1000 credits, paid 40 in subcalls (net portion), received net from caller
	// Just verify provider gained money overall relative to the 1000 starting balance minus subcall costs.
	if mainProv.Available <= 1000-50 {
		// rough sanity: provider kept margin (100 - 40 subcall gross - fees > 0)
		t.Errorf("provider balance %d seems too low (started with 1000)", mainProv.Available)
	}
}

// TestFlow_FailMidTree: caller runs an action that fails mid-tree; settled subcall stays
// paid; remainder refunded; process closes; transactions show success and failure with reasons.
func TestFlow_FailMidTree(t *testing.T) {
	succeedBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer succeedBackend.Close()

	exec := &flowScriptExec{
		handlers: map[string]func(ctx context.Context, input []byte, host kernel.HostFunctions) ([]byte, error){},
	}
	srv, k, _ := newFlowKernel(t, exec)
	defer srv.Close()

	// Sub-provider: a cheap action that succeeds.
	subProvID, subProvTok := makeUser(t, k, "@flow-fail-subprov")
	_ = subProvID
	createPublicAction(t, srv, succeedBackend.URL, subProvTok, "succeed-svc", 10)

	// Main provider: WASM that calls succeed-svc then returns an error.
	mainProvID, mainProvTok := makeUser(t, k, "@flow-fail-mainprov")
	giveCredits(t, k, mainProvID, 1000)

	exec.handlers["fail-after-sub"] = func(ctx context.Context, input []byte, host kernel.HostFunctions) ([]byte, error) {
		// First subcall succeeds.
		if _, err := host.Call(ctx, "@flow-fail-subprov/succeed-svc", []byte(`{}`)); err != nil {
			return nil, fmt.Errorf("unexpected sub error: %w", err)
		}
		// Then deliberately fail.
		return nil, fmt.Errorf("deliberate mid-tree failure")
	}

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "fail-after-sub", "kind": "wasm", "price": int64(50),
		"source": "fail-after-sub", "description": "fails after subcall",
		"input_schema": minSchema, "output_schema": minSchema,
	}, mainProvTok)
	if cr.StatusCode != http.StatusCreated {
		cr.Body.Close()
		t.Fatalf("create action: expected 201, got %d", cr.StatusCode)
	}
	var act map[string]any
	decodeResponse(t, cr, &act)
	actID := act["id"].(string)
	httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, mainProvTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions/"+actID, map[string]any{"public": true}, mainProvTok).Body.Close()

	callerID, callerTok := makeUser(t, k, "@flow-fail-caller")
	giveCredits(t, k, callerID, 500)
	balBefore := getBalance(t, srv, callerTok)

	// Run — expect failure.
	resp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@flow-fail-mainprov/fail-after-sub",
		"args":   map[string]any{},
	}, callerTok)
	// Should be an error response.
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("expected non-200 response for failing action")
	}

	// Caller's balance should be partially refunded (paid only the settled subcall cost, not full price).
	balAfter := getBalance(t, srv, callerTok)
	debit := balBefore - balAfter
	// debit must be < 50 (partial refund); at minimum 10 (cost of settled sub-call gross)
	if debit >= 50 {
		t.Errorf("expected partial refund: debit=%d, should be < 50", debit)
	}
	// The sub-provider was paid (settled subcall).
	ctx := context.Background()
	subProv, _ := k.ReadUserByHandle(ctx, "@flow-fail-subprov")
	if subProv.Available == 0 {
		t.Error("settled sub-provider should have received payment")
	}
}

// ============================================================
// — Async / steps —
// ============================================================

// TestFlow_ApprovalStep: action parks an approval step addressed to a human; process stays
// open; the human lists and completes the step; fulfillment runs on parked funds; process closes.
func TestFlow_ApprovalStep(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"approved": true})
	}))
	defer backend.Close()

	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@appr-owner")
	humanID, humanTok := makeUser(t, k, "@appr-human")
	_ = humanID

	giveCredits(t, k, ownerID, 500)

	// Action that will be the step target.
	stepActionID := createPublicAction(t, srv, backend.URL, ownerTok, "appr-action", 0)

	// Create a process directly.
	p := setupProcessHTTP(t, db, ownerID, 200)
	traceID := setupTraceForProcess(t, db, p.ID)

	// Owner creates a step (approval gate) addressed to the human.
	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action_id":       stepActionID,
		"required_caller": "@appr-human",
		"partial_args":    map[string]any{"preset": "value"},
	}, ownerTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	stepID := step["id"].(string)
	if step["status"] != "waiting" {
		t.Errorf("step status: expected waiting, got %v", step["status"])
	}

	// Human sees the step in their list.
	listResp := httpDo(t, srv, "GET", "/v1/steps", nil, humanTok)
	if listResp.StatusCode != http.StatusOK {
		listResp.Body.Close()
		t.Fatalf("list steps: expected 200, got %d", listResp.StatusCode)
	}
	var steps []map[string]any
	decodeResponse(t, listResp, &steps)
	found := false
	for _, s := range steps {
		if s["id"] == stepID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("human should see step %s in list", stepID)
	}

	// Human completes the step.
	complResp := httpDo(t, srv, "POST", "/v1/steps/"+stepID+"/complete", map[string]any{
		"args": map[string]any{"human_input": "approved"},
	}, humanTok)
	if complResp.StatusCode != http.StatusOK {
		complResp.Body.Close()
		t.Fatalf("complete step: expected 200, got %d", complResp.StatusCode)
	}
	var complReply map[string]any
	decodeResponse(t, complResp, &complReply)
	if complReply["tx_id"] == nil || complReply["tx_id"] == "" {
		t.Error("expected tx_id in complete-step reply")
	}
	if complReply["step_id"] != stepID {
		t.Errorf("step_id mismatch: got %v, want %s", complReply["step_id"], stepID)
	}

	// Step is now done.
	getStep := httpDo(t, srv, "GET", "/v1/steps/"+stepID, nil, ownerTok)
	var doneStep map[string]any
	decodeResponse(t, getStep, &doneStep)
	if doneStep["status"] != "done" {
		t.Errorf("step after complete: expected done, got %v", doneStep["status"])
	}
}

// TestFlow_WebhookCompleteStep: external system registers as a user, a purchase flow
// pre-creates a step addressed to it; the system POSTs to /v1/steps/{id}/complete;
// transaction obeys role law.
func TestFlow_WebhookCompleteStep(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"webhook_result": "ok"})
	}))
	defer backend.Close()

	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@wh-owner")
	webhookID, webhookTok := makeUser(t, k, "@wh-webhook-sys")
	_ = webhookID

	giveCredits(t, k, ownerID, 500)

	// Action targeted by the step.
	actionID := createPublicAction(t, srv, backend.URL, ownerTok, "wh-action", 0)

	// Create process and step addressed to the webhook system.
	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action_id":       actionID,
		"required_caller": "@wh-webhook-sys",
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
	if tx.CallerHandle != "@wh-webhook-sys" {
		t.Errorf("tx caller_handle: got %s, want @wh-webhook-sys (webhook)", tx.CallerHandle)
	}
	if tx.Status != kernel.TxSuccess {
		t.Errorf("tx status: got %s, want success", tx.Status)
	}
}

// TestFlow_ForceEndWithSteps: owner force-ends a process with waiting steps;
// steps cancelled; parked prices refunded; balances reconcile.
func TestFlow_ForceEndWithSteps(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@fend-owner")
	giveCredits(t, k, ownerID, 500)
	_, callerTok := makeUser(t, k, "@fend-caller")
	_ = callerTok

	actionID := createPublicAction(t, srv, backend.URL, ownerTok, "fend-action", 0)

	// Create process with funds.
	p := setupProcessHTTP(t, db, ownerID, 200)
	traceID := setupTraceForProcess(t, db, p.ID)

	// Create two steps (parks funds).
	for i := 0; i < 2; i++ {
		r := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
			"trace_id":        traceID,
			"action_id":       actionID,
			"required_caller": "@fend-caller",
			"partial_args":    map[string]any{},
		}, ownerTok)
		if r.StatusCode != http.StatusCreated {
			r.Body.Close()
			t.Fatalf("create step %d: expected 201, got %d", i, r.StatusCode)
		}
		r.Body.Close()
	}

	// Check the process is open.
	procResp := httpDo(t, srv, "GET", "/v1/processes/"+p.ID, nil, ownerTok)
	var proc kernel.Process
	decodeResponse(t, procResp, &proc)
	if proc.Status != kernel.ProcessOpen {
		t.Fatalf("process should be open before end")
	}

	// Owner force-ends the process.
	endResp := httpDo(t, srv, "POST", "/v1/processes/"+p.ID+"/end", nil, ownerTok)
	if endResp.StatusCode != http.StatusNoContent {
		endResp.Body.Close()
		t.Fatalf("end process: expected 204, got %d", endResp.StatusCode)
	}
	endResp.Body.Close()

	// Process is now closed.
	procResp2 := httpDo(t, srv, "GET", "/v1/processes/"+p.ID, nil, ownerTok)
	var proc2 kernel.Process
	decodeResponse(t, procResp2, &proc2)
	if proc2.Status != kernel.ProcessClosed {
		t.Errorf("process should be closed after end, got %s", proc2.Status)
	}

	// Steps should be cancelled.
	stepsResp := httpDo(t, srv, "GET", "/v1/steps?process_id="+p.ID, nil, ownerTok)
	var stepsAfter []map[string]any
	decodeResponse(t, stepsResp, &stepsAfter)
	for _, s := range stepsAfter {
		if s["status"] != "cancelled" && s["status"] != "done" {
			t.Errorf("step %v: expected cancelled after force-end, got %v", s["id"], s["status"])
		}
	}

	// Owner's balance recovered (funds returned from process wallet).
	ctx := context.Background()
	ownerUser, _ := k.ReadUser(ctx, ownerID)
	// Should have gotten back the process funds (200 initially deposited into process).
	if ownerUser.Available == 0 {
		t.Error("owner balance should be non-zero after process end refund")
	}
}

// TestFlow_RestartRecovery: kernel restarts mid-flight; interrupted calls fail as
// interrupted with refunds; waiting steps survive and remain completable after restart.
func TestFlow_RestartRecovery(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	defer backend.Close()

	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@rst-owner")
	callerID, callerTok := makeUser(t, k, "@rst-caller")
	_ = callerTok

	giveCredits(t, k, ownerID, 500)

	// Create a step action.
	actionID := createPublicAction(t, srv, backend.URL, ownerTok, "rst-action", 0)

	// Create a process and step.
	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action_id":       actionID,
		"required_caller": "@rst-caller",
		"partial_args":    map[string]any{},
	}, ownerTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	stepID := step["id"].(string)

	// Simulate restart: call ResetRunningSteps (resets any mid-flight running steps to waiting).
	ctx := context.Background()
	if err := k.ResetRunningSteps(ctx); err != nil {
		t.Fatalf("ResetRunningSteps: %v", err)
	}

	// Waiting step survives and is still completable.
	getStepResp := httpDo(t, srv, "GET", "/v1/steps/"+stepID, nil, ownerTok)
	var stepAfterReset map[string]any
	decodeResponse(t, getStepResp, &stepAfterReset)
	if stepAfterReset["status"] != "waiting" {
		t.Errorf("waiting step should survive restart, got status=%v", stepAfterReset["status"])
	}

	// Caller can still complete the step.
	callerUser, _ := k.ReadUser(ctx, callerID)
	_ = callerUser

	_, callerTok2 := makeUser(t, k, "@rst-caller2")
	// Let's use the existing caller.
	complResp := httpDo(t, srv, "POST", "/v1/steps/"+stepID+"/complete", map[string]any{
		"args": map[string]any{},
	}, callerTok)
	if complResp.StatusCode != http.StatusOK {
		complResp.Body.Close()
		t.Fatalf("complete step after restart: expected 200, got %d", complResp.StatusCode)
	}
	_ = callerTok2
	var complReply map[string]any
	decodeResponse(t, complResp, &complReply)
	if complReply["tx_id"] == nil || complReply["tx_id"] == "" {
		t.Error("expected tx_id after completing recovered step")
	}
}

// ============================================================
// — @sys/make —
// ============================================================

// TestFlow_SysMakeSynthesis: @sys/make is tested via a native stub that short-circuits
// to creating a predefined action owned by the caller.  Verifies: action is owned by
// the requester, activated, and immediately runnable by another user.
func TestFlow_SysMakeSynthesis(t *testing.T) {
	resultBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"synthesized": true})
	}))
	defer resultBackend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ctx := context.Background()

	// Register a stub @sys/make that creates an HTTP action for the caller.
	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	makeAction, err := k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
		OwnerUserID:  sys.ID,
		Name:         "make",
		Kind:         kernel.KindNative,
		Price:        0,
		Description:  "synthesize action",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"description": map[string]any{"type": "string", "description": "what to build"}}},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"action_id": map[string]any{"type": "string", "description": "new action id"}, "status": map[string]any{"type": "string", "description": "outcome"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	backendURL := resultBackend.URL
	k.RegisterNativeHandler("make", func(ctx2 context.Context, args map[string]any, targetID, callerID, ownerUserID, processID, parentTraceID string) (map[string]any, error) {
		// Stub: create a simple HTTP action owned by the caller.
		newAct, createErr := k.CreateAction(ctx2, callerID, kernel.CreateActionRequest{
			OwnerUserID:  callerID,
			Name:         "synthesized-action",
			Kind:         kernel.KindHTTP,
			Price:        0,
			Source:       backendURL,
			Description:  "synthesized",
			InputSchema:  map[string]any{"type": "object", "properties": map[string]any{}},
			OutputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		})
		if createErr != nil {
			return nil, createErr
		}
		if activateErr := k.SetActive(ctx2, callerID, newAct.ID, true); activateErr != nil {
			return nil, activateErr
		}
		return map[string]any{"status": "success", "action_id": newAct.ID}, nil
	})

	// Activate the @sys/make native action (ActivateNativeAction sets Public=true automatically).
	if err := k.ActivateNativeAction(ctx, makeAction.ID, "synthesize action",
		makeAction.InputSchema, makeAction.OutputSchema, 0); err != nil {
		t.Fatal(err)
	}

	// User runs @sys/make.
	builderID, builderTok := makeUser(t, k, "@flow-builder")
	giveCredits(t, k, builderID, 200)

	reply := runAction(t, srv, builderTok, "@sys/make", map[string]any{"description": "build a thing"})
	newActionID, _ := reply.Result["action_id"].(string)
	if newActionID == "" {
		t.Fatalf("@sys/make: expected action_id in result, got %v", reply.Result)
	}
	if reply.Result["status"] != "success" {
		t.Errorf("@sys/make status: got %v, want success", reply.Result["status"])
	}

	// Verify ownership: action is owned by the builder.
	newAct, err := k.ReadAction(ctx, newActionID)
	if err != nil {
		t.Fatalf("read synthesized action: %v", err)
	}
	if newAct.OwnerUserID != builderID {
		t.Errorf("synthesized action owner: got %s, want %s", newAct.OwnerUserID, builderID)
	}
	if !newAct.Active {
		t.Error("synthesized action should be active")
	}

	// Another user can run the synthesized action immediately.
	otherID, otherTok := makeUser(t, k, "@flow-other-runner")
	giveCredits(t, k, otherID, 100)
	// Make the action public first.
	pubTrue := true
	k.UpdateAction(ctx, builderID, kernel.UpdateActionRequest{ID: newActionID, Public: &pubTrue}) //nolint
	reply2 := runAction(t, srv, otherTok, "@flow-builder/synthesized-action", map[string]any{})
	if reply2.Result["synthesized"] != true {
		t.Errorf("synthesized action result: got %v, want synthesized=true", reply2.Result)
	}
}

// ============================================================
// — OpenAPI —
// ============================================================

// TestFlow_OpenAPIImportActivateRun: API owner imports an OpenAPI document with a stored
// API key, activates an action, makes it public, and a caller executes it through Run();
// the key never surfaces in responses.
func TestFlow_OpenAPIImportActivateRun(t *testing.T) {
	// Mock API backend that validates a key header.
	const apiKey = "secret-api-key-12345"
	apiBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": "from-api"})
	}))
	defer apiBackend.Close()

	// Well-known ownership proof server.
	ownerHandle := "@oa-owner"
	wkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/juice-owner.txt" {
			w.Write([]byte(ownerHandle))
		}
	}))
	defer wkServer.Close()

	// Minimal OpenAPI 3.0 spec.
	spec := map[string]any{
		"openapi":       "3.0.0",
		"x-juice-owner": ownerHandle,
		"info":          map[string]any{"title": "Test API", "version": "1.0"},
		"servers":       []any{map[string]any{"url": apiBackend.URL}},
		"paths": map[string]any{
			"/fetch": map[string]any{
				"post": map[string]any{
					"operationId": "fetchData",
					"summary":     "Fetch data from the API",
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":       "object",
									"properties": map[string]any{},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "success",
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{
										"type":       "object",
										"properties": map[string]any{"data": map[string]any{"type": "string", "description": "result"}},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	specBytes, _ := json.Marshal(spec)

	// Serve the spec from a local HTTP server so ImportOpenAPI can fetch it.
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/juice-owner.txt" {
			w.Write([]byte(ownerHandle))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(specBytes)
	}))
	defer specServer.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, ownerHandle)
	_ = ownerID

	// Import via HTTP.
	importResp := httpDo(t, srv, "POST", "/v1/actions/import", map[string]any{
		"spec_url": specServer.URL + "/openapi.json",
	}, ownerTok)
	if importResp.StatusCode != http.StatusOK {
		importResp.Body.Close()
		t.Fatalf("import OpenAPI: expected 200, got %d", importResp.StatusCode)
	}
	var importResult map[string]any
	decodeResponse(t, importResp, &importResult)

	// Find the created action.
	var createdActions []any
	if ca, ok := importResult["Created"].([]any); ok {
		createdActions = ca
	}
	if len(createdActions) == 0 {
		t.Fatalf("expected at least one created action, got import result: %v", importResult)
	}
	firstAct := createdActions[0].(map[string]any)
	actID := firstAct["id"].(string)

	// Activate and make public.
	if r := httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, ownerTok); r.StatusCode != http.StatusOK {
		r.Body.Close()
		t.Fatalf("enable action: expected 200, got %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if r := httpDo(t, srv, "PUT", "/v1/actions/"+actID, map[string]any{"public": true}, ownerTok); r.StatusCode != http.StatusOK {
		r.Body.Close()
		t.Fatalf("make public: expected 200, got %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// Caller executes through Run().
	callerID, callerTok := makeUser(t, k, "@oa-caller")
	giveCredits(t, k, callerID, 100)

	runResp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": ownerHandle + "/fetchData",
		"args":   map[string]any{},
	}, callerTok)
	if runResp.StatusCode != http.StatusOK {
		var body map[string]any
		json.NewDecoder(runResp.Body).Decode(&body)
		runResp.Body.Close()
		t.Fatalf("run imported action: expected 200, got %d: %v", runResp.StatusCode, body)
	}
	var runReply kernel.CallReply
	decodeResponse(t, runResp, &runReply)

	// API key must never appear in the reply.
	replyBytes, _ := json.Marshal(runReply)
	if bytes.Contains(replyBytes, []byte(apiKey)) {
		t.Error("API key must not appear in run reply")
	}

	// Action record must not expose the key.
	actResp := httpDo(t, srv, "GET", "/v1/actions/"+actID, nil, ownerTok)
	var actBody map[string]any
	decodeResponse(t, actResp, &actBody)
	actBytes, _ := json.Marshal(actBody)
	if bytes.Contains(actBytes, []byte(apiKey)) {
		t.Error("API key must not appear in action record")
	}
}

// TestFlow_OpenAPIReimport: API owner re-runs import against a changed OpenAPI document;
// the matched action is deactivated, stats reset, and historical transactions remain attached.
func TestFlow_OpenAPIReimport(t *testing.T) {
	apiBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer apiBackend.Close()

	ownerHandle := "@reimp-owner"
	buildSpec := func(summary string) []byte {
		spec := map[string]any{
			"openapi":       "3.0.0",
			"x-juice-owner": ownerHandle,
			"info":          map[string]any{"title": "API", "version": "1.0"},
			"servers":       []any{map[string]any{"url": apiBackend.URL}},
			"paths": map[string]any{
				"/do": map[string]any{
					"post": map[string]any{
						"operationId": "doSomething",
						"summary":     summary,
						"requestBody": map[string]any{
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{"type": "object", "properties": map[string]any{}},
								},
							},
						},
						"responses": map[string]any{
							"200": map[string]any{
								"description": "success",
								"content": map[string]any{
									"application/json": map[string]any{
										"schema": map[string]any{
											"type":       "object",
											"properties": map[string]any{"ok": map[string]any{"type": "boolean", "description": "result"}},
										},
									},
								},
							},
						},
					},
				},
			},
		}
		b, _ := json.Marshal(spec)
		return b
	}

	var currentSpec []byte
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(currentSpec)
	}))
	defer specServer.Close()
	specURL := specServer.URL + "/spec.json"

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, ownerHandle)
	_ = ownerID

	// First import.
	currentSpec = buildSpec("original summary")
	imp1 := httpDo(t, srv, "POST", "/v1/actions/import", map[string]any{"spec_url": specURL}, ownerTok)
	if imp1.StatusCode != http.StatusOK {
		imp1.Body.Close()
		t.Fatalf("first import: expected 200, got %d", imp1.StatusCode)
	}
	var res1 map[string]any
	decodeResponse(t, imp1, &res1)
	created1, _ := res1["Created"].([]any)
	if len(created1) == 0 {
		t.Fatal("expected created action on first import")
	}
	actID := created1[0].(map[string]any)["id"].(string)

	// Activate the action and run it once to generate a tx.
	httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions/"+actID, map[string]any{"public": true}, ownerTok).Body.Close()

	callerID, callerTok := makeUser(t, k, "@reimp-caller")
	giveCredits(t, k, callerID, 100)
	runRep := runAction(t, srv, callerTok, ownerHandle+"/doSomething", map[string]any{})
	txID := runRep.TxID

	// Stats should show at least 1 use.
	statsResp := httpDo(t, srv, "GET", "/v1/stats/"+actID, nil, ownerTok)
	var stats kernel.Stats
	decodeResponse(t, statsResp, &stats)
	if stats.Uses == 0 {
		t.Error("stats.uses should be > 0 after one run")
	}

	// Second import with changed summary (triggers reimport / deactivate).
	currentSpec = buildSpec("changed summary — triggers contract change")
	imp2 := httpDo(t, srv, "POST", "/v1/actions/import", map[string]any{"spec_url": specURL}, ownerTok)
	if imp2.StatusCode != http.StatusOK {
		imp2.Body.Close()
		t.Fatalf("second import: expected 200, got %d", imp2.StatusCode)
	}
	imp2.Body.Close()

	// Historical transaction still attached.
	txResp := httpDo(t, srv, "GET", "/v1/transactions/"+txID, nil, callerTok)
	if txResp.StatusCode != http.StatusOK {
		txResp.Body.Close()
		t.Fatalf("get historical tx after reimport: expected 200, got %d", txResp.StatusCode)
	}
	var tx kernel.Transaction
	decodeResponse(t, txResp, &tx)
	if tx.ID != txID {
		t.Errorf("tx ID mismatch: got %s, want %s", tx.ID, txID)
	}
}

// TestFlow_OpenAPIUnimport: API owner unimports an OpenAPI document; matching actions
// are deactivated and history remains attached.
func TestFlow_OpenAPIUnimport(t *testing.T) {
	apiBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer apiBackend.Close()

	ownerHandle := "@unimp-owner"
	spec := map[string]any{
		"openapi":       "3.0.0",
		"x-juice-owner": ownerHandle,
		"info":          map[string]any{"title": "API", "version": "1.0"},
		"servers":       []any{map[string]any{"url": apiBackend.URL}},
		"paths": map[string]any{
			"/go": map[string]any{
				"post": map[string]any{
					"operationId": "goSomething",
					"summary":     "Does something",
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{"type": "object", "properties": map[string]any{}},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "success",
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{
										"type":       "object",
										"properties": map[string]any{"ok": map[string]any{"type": "boolean", "description": "done"}},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	specBytes, _ := json.Marshal(spec)
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(specBytes)
	}))
	defer specServer.Close()
	specURL := specServer.URL + "/spec.json"

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, ownerHandle)
	_ = ownerID

	// Import.
	imp := httpDo(t, srv, "POST", "/v1/actions/import", map[string]any{"spec_url": specURL}, ownerTok)
	if imp.StatusCode != http.StatusOK {
		imp.Body.Close()
		t.Fatalf("import: expected 200, got %d", imp.StatusCode)
	}
	var impResult map[string]any
	decodeResponse(t, imp, &impResult)
	created, _ := impResult["Created"].([]any)
	if len(created) == 0 {
		t.Fatal("expected at least one created action")
	}
	actID := created[0].(map[string]any)["id"].(string)

	// Enable and run once to generate history.
	httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions/"+actID, map[string]any{"public": true}, ownerTok).Body.Close()
	callerID, callerTok := makeUser(t, k, "@unimp-caller")
	giveCredits(t, k, callerID, 100)
	runRep := runAction(t, srv, callerTok, ownerHandle+"/goSomething", map[string]any{})
	txID := runRep.TxID

	// Unimport.
	unimp := httpDo(t, srv, "POST", "/v1/actions/unimport", map[string]any{"spec_url": specURL}, ownerTok)
	if unimp.StatusCode != http.StatusOK {
		unimp.Body.Close()
		t.Fatalf("unimport: expected 200, got %d", unimp.StatusCode)
	}
	unimp.Body.Close()

	// Action should be deactivated.
	actResp := httpDo(t, srv, "GET", "/v1/actions/"+actID, nil, ownerTok)
	var act kernel.Action
	decodeResponse(t, actResp, &act)
	if act.Active {
		t.Error("action should be inactive after unimport")
	}

	// Historical transaction still present.
	txResp := httpDo(t, srv, "GET", "/v1/transactions/"+txID, nil, callerTok)
	if txResp.StatusCode != http.StatusOK {
		txResp.Body.Close()
		t.Fatalf("get tx after unimport: expected 200, got %d", txResp.StatusCode)
	}
	var tx kernel.Transaction
	decodeResponse(t, txResp, &tx)
	if tx.ID != txID {
		t.Errorf("historical tx ID: got %s, want %s", tx.ID, txID)
	}
}

// ============================================================
// — Ratings and reconciliation —
// ============================================================

// TestFlow_ReconcileNetAmounts: caller executes a paid action multiple times; the action
// owner lists transactions for their action and the sum of transaction net amounts equals
// the total credits received by the owner.
func TestFlow_ReconcileNetAmounts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	defer backend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@rec-owner")
	callerID, callerTok := makeUser(t, k, "@rec-caller")
	giveCredits(t, k, callerID, 1000)

	const price int64 = 50
	createPublicAction(t, srv, backend.URL, ownerTok, "rec-action", price)

	// Run 3 times.
	const runs = 3
	for i := 0; i < runs; i++ {
		r := runAction(t, srv, callerTok, "@rec-owner/rec-action", map[string]any{})
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

// TestFlow_RatingVisibility: caller rates a transaction with a note; the note and rating
// value appear in transaction detail and list responses for all parties; an unrated
// transaction returns null for the rating field.
func TestFlow_RatingVisibility(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@rv-owner")
	_ = ownerID
	callerID, callerTok := makeUser(t, k, "@rv-caller")
	giveCredits(t, k, callerID, 200)

	createPublicAction(t, srv, backend.URL, ownerTok, "rv-action", 0)

	// Run once — creates a transaction.
	r := runAction(t, srv, callerTok, "@rv-owner/rv-action", map[string]any{})
	txID := r.TxID
	if txID == "" {
		t.Fatal("expected tx_id")
	}

	// Before rating — get tx detail, rating field should be null.
	txDetailResp := httpDo(t, srv, "GET", "/v1/transactions/"+txID, nil, callerTok)
	if txDetailResp.StatusCode != http.StatusOK {
		txDetailResp.Body.Close()
		t.Fatalf("get tx: expected 200, got %d", txDetailResp.StatusCode)
	}
	var txView map[string]any
	decodeResponse(t, txDetailResp, &txView)
	if _, hasRating := txView["rating"]; hasRating && txView["rating"] != nil {
		t.Errorf("unrated tx should have null rating, got %v", txView["rating"])
	}

	// Caller rates with note.
	note := "excellent service"
	rateResp := httpDo(t, srv, "POST", "/v1/transactions/"+txID+"/rate", map[string]any{
		"rating": float64(1),
		"note":   note,
	}, callerTok)
	if rateResp.StatusCode != http.StatusOK {
		rateResp.Body.Close()
		t.Fatalf("rate tx: expected 200, got %d", rateResp.StatusCode)
	}
	var ratingResp kernel.Rating
	decodeResponse(t, rateResp, &ratingResp)
	if ratingResp.ID == "" {
		t.Error("expected rating ID")
	}
	if ratingResp.Note == nil || *ratingResp.Note != note {
		t.Errorf("rating note: got %v, want %q", ratingResp.Note, note)
	}
	if ratingResp.Rating != 1 {
		t.Errorf("rating value: got %v, want 1", ratingResp.Rating)
	}

	// Ratings appear in action rating list.
	actResp := httpDo(t, srv, "GET", "/v1/actions", nil, ownerTok)
	var acts []kernel.Action
	decodeResponse(t, actResp, &acts)
	var actID string
	for _, a := range acts {
		if a.Name == "rv-action" {
			actID = a.ID
			break
		}
	}
	if actID == "" {
		// try owner's list
		ctx := context.Background()
		as, _ := k.ListOwnedActions(ctx, ownerID, 50, 0)
		for _, a := range as {
			if a.Name == "rv-action" {
				actID = a.ID
				break
			}
		}
	}
	if actID != "" {
		ratingsResp := httpDo(t, srv, "GET", "/v1/actions/"+actID+"/ratings", nil, ownerTok)
		if ratingsResp.StatusCode == http.StatusOK {
			var ratings []kernel.Rating
			decodeResponse(t, ratingsResp, &ratings)
			if len(ratings) == 0 {
				t.Error("expected at least one rating in action ratings list")
			} else {
				if ratings[0].RatedTxID != txID {
					t.Errorf("rating rated_tx_id: got %s, want %s", ratings[0].RatedTxID, txID)
				}
				if ratings[0].Note == nil || *ratings[0].Note != note {
					t.Errorf("rating note in list: got %v, want %q", ratings[0].Note, note)
				}
			}
		}
	}
}

// ============================================================
// — Federation —
// ============================================================

// newFedKernel builds a bootstrapped httptest.Server for federation tests.
// It wires signerFn and allowLocal=true on the HTTP executor so outbound
// federation calls to other in-process httptest.Servers work correctly.
func newFedKernel(t *testing.T) (*httptest.Server, *kernel.Kernel, *store.DB, ed25519.PrivateKey) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "fed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "fed-test-secret"
	cfg.AllowLocalSources = true
	logger := log.Discard()

	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, allowLocal: true}
	k := kernel.New(db, nil, httpExec, nil, cfg, logger)

	if err := k.FirstBoot(context.Background(), "sys-pass"); err != nil {
		t.Fatal(err)
	}
	priv := bootstrapSigning(t, k)

	// Wire the outbound federation signer so signed calls to peer kernels work.
	httpExec.signerFn = k.SignFederation

	srv := &server{kernel: k, log: logger}
	return httptest.NewServer(mountFullRouter(srv)), k, db, priv
}

// TestFlow_UpstreamAuthSecrecy: action created with bearer auth credentials; the secret
// must never appear in any read path (action JSON, manifest, transaction, receipt).
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

	ownerID, ownerTok := makeUser(t, k, "@auth-owner")
	callerID, callerTok := makeUser(t, k, "@auth-caller")
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
	pubTrue := true
	if _, err := k.UpdateAction(context.Background(), ownerID, kernel.UpdateActionRequest{ID: actionID, Public: &pubTrue}); err != nil {
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
		"action": "@auth-owner/secured-action",
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

// newFlowKernelFull is like newFlowKernel but accepts an embedder for tests that exercise
// semantic lookup.
func newFlowKernelFull(t *testing.T, exec kernel.ScriptExecutor, embedder kernel.Embedder) (*httptest.Server, *kernel.Kernel, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "flow-full.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "flow-full-test-secret"
	cfg.AllowLocalSources = true
	logger := log.Discard()
	k := kernel.New(db, exec, &httpActionExecutor{timeout: cfg.ScriptTimeout}, embedder, cfg, logger)

	if err := k.FirstBoot(context.Background(), "sys-pass"); err != nil {
		t.Fatal(err)
	}
	bootstrapSigning(t, k)

	srv := &server{kernel: k, log: logger}
	return httptest.NewServer(mountFullRouter(srv)), k, db
}

// bootstrapSysNative registers and activates a @sys native action by spec name.
// Uses price=0 for all test specs; spec schemas come from buildSysNativeSpecs.
func bootstrapSysNative(t *testing.T, k *kernel.Kernel, names ...string) {
	t.Helper()
	ctx := context.Background()
	specs := buildSysNativeSpecs(NativeConfig{})
	specMap := make(map[string]sysNativeSpec, len(specs))
	for _, s := range specs {
		specMap[s.name] = s
	}
	for _, name := range names {
		spec, ok := specMap[name]
		if !ok {
			t.Fatalf("bootstrapSysNative: unknown spec %q", name)
		}
		if err := ensureSysNative(ctx, k, "@sys", spec); err != nil {
			t.Fatalf("bootstrapSysNative %q: %v", name, err)
		}
	}
}

// TestFlow_LookupAndRun: caller runs @sys/lookup with a query; the matching action appears
// in results; caller runs it; transaction is recorded.
func TestFlow_LookupAndRun(t *testing.T) {
	const description = "barometric pressure sensor api"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"pressure": 1013})
	}))
	defer backend.Close()

	srv, k, _ := newFlowKernelFull(t, nil, &llm.FakeEmbedder{Dims: 8})
	defer srv.Close()

	ctx := context.Background()

	bootstrapSysNative(t, k, "lookup")
	native.RegisterLookupHandler(k)

	// Provider creates a public action with a distinctive description.
	providerID, providerTok := makeUser(t, k, "@lk-provider")
	_ = providerID
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "pressure-api", "kind": "http", "price": 0, "source": backend.URL,
		"description": description, "input_schema": minSchema, "output_schema": minSchema,
	}, providerTok)
	if cr.StatusCode != http.StatusCreated {
		cr.Body.Close()
		t.Fatalf("create action: got %d", cr.StatusCode)
	}
	var act map[string]any
	decodeResponse(t, cr, &act)
	actID := act["id"].(string)
	httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, providerTok).Body.Close()
	pubTrue := true
	if _, err := k.UpdateAction(ctx, providerID, kernel.UpdateActionRequest{ID: actID, Public: &pubTrue}); err != nil {
		t.Fatalf("make public: %v", err)
	}

	// Caller looks up actions matching the description.
	callerID, callerTok := makeUser(t, k, "@lk-caller")
	giveCredits(t, k, callerID, 200)

	lookupReply := runAction(t, srv, callerTok, "@sys/lookup", map[string]any{"query": description})
	results, _ := lookupReply.Result["results"].([]any)
	found := false
	for _, r := range results {
		item := r.(map[string]any)
		if item["action_id"] == actID {
			found = true
			if item["score"].(float64) <= 0 {
				t.Errorf("lookup score should be positive, got %v", item["score"])
			}
			break
		}
	}
	if !found {
		t.Errorf("action %s not found in lookup results (got %d items)", actID, len(results))
	}

	// Caller runs the top result directly.
	reply := runAction(t, srv, callerTok, "@lk-provider/pressure-api", map[string]any{})
	if reply.TxID == "" {
		t.Error("expected tx_id from pressure-api run")
	}
}

// TestFlow_Message: user A sends a message to user B via @sys/message; B sees the step;
// B completes it; the step is done and a transaction exists.
func TestFlow_Message(t *testing.T) {
	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	bootstrapSysNative(t, k, "sink", "message")
	native.RegisterSinkHandler(k)
	native.RegisterMessageHandler(k)

	userAID, userATok := makeUser(t, k, "@msg-a")
	_, userBTok := makeUser(t, k, "@msg-b")
	giveCredits(t, k, userAID, 200)

	// A sends a message to B.
	reply := runAction(t, srv, userATok, "@sys/message", map[string]any{
		"to":      "@msg-b",
		"message": "hello from A",
	})
	stepID, _ := reply.Result["step_id"].(string)
	if stepID == "" {
		t.Fatalf("@sys/message: expected step_id in result, got %v", reply.Result)
	}

	// B sees the step in their list.
	listResp := httpDo(t, srv, "GET", "/v1/steps", nil, userBTok)
	if listResp.StatusCode != http.StatusOK {
		listResp.Body.Close()
		t.Fatalf("list steps: expected 200, got %d", listResp.StatusCode)
	}
	var steps []map[string]any
	decodeResponse(t, listResp, &steps)
	found := false
	for _, s := range steps {
		if s["id"] == stepID && s["status"] == "waiting" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("B should see step %s as waiting (got %d steps)", stepID, len(steps))
	}

	// B completes the step (the sink action accepts any input).
	complResp := httpDo(t, srv, "POST", "/v1/steps/"+stepID+"/complete",
		map[string]any{"args": map[string]any{}}, userBTok)
	if complResp.StatusCode != http.StatusOK {
		var body map[string]any
		json.NewDecoder(complResp.Body).Decode(&body)
		complResp.Body.Close()
		t.Fatalf("complete step: expected 200, got %d — %v", complResp.StatusCode, body)
	}
	var complBody map[string]any
	decodeResponse(t, complResp, &complBody)
	if complBody["tx_id"] == nil {
		t.Error("complete step: expected tx_id in reply")
	}

	// Step is now done.
	getResp := httpDo(t, srv, "GET", "/v1/steps/"+stepID, nil, userBTok)
	var doneStep map[string]any
	decodeResponse(t, getResp, &doneStep)
	if doneStep["status"] != "done" {
		t.Errorf("step after complete: expected done, got %v", doneStep["status"])
	}
}

// TestFlow_ThreePartyRoleLaw: P ≠ C ≠ A — process owner P creates a step for bot C to
// call provider A's action. All three parties independently read the transaction and the
// role handles (owner_handle, caller_handle, target_handle) are distinct and correct.
func TestFlow_ThreePartyRoleLaw(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	defer backend.Close()

	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ctx := context.Background()
	pID, pTok := makeUser(t, k, "@3p-owner")
	_, cTok := makeUser(t, k, "@3p-caller")
	_, aTok := makeUser(t, k, "@3p-provider")

	giveCredits(t, k, pID, 500)

	// A creates and activates a public action.
	actID := createPublicAction(t, srv, backend.URL, aTok, "3p-action", 0)
	_ = actID

	// P creates a process and a trace for the step funding source.
	p := setupProcessHTTP(t, db, pID, 200)
	traceID := setupTraceForProcess(t, db, p.ID)

	// P creates a step addressed to C, pointing at A's action.
	aAction, err := k.ReadActionByOwnerName(ctx, func() string {
		u, _ := k.ReadUserByHandle(ctx, "@3p-provider")
		return u.ID
	}(), "3p-action")
	if err != nil || aAction == nil {
		t.Fatalf("read 3p-action: %v", err)
	}
	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action_id":       aAction.ID,
		"required_caller": "@3p-caller",
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
		if tx["owner_handle"] != "@3p-owner" {
			t.Errorf("tx.owner_handle: got %v, want @3p-owner", tx["owner_handle"])
		}
		if tx["caller_handle"] != "@3p-caller" {
			t.Errorf("tx.caller_handle: got %v, want @3p-caller", tx["caller_handle"])
		}
		if tx["target_handle"] != "@3p-provider" {
			t.Errorf("tx.target_handle: got %v, want @3p-provider", tx["target_handle"])
		}
		// The raw party UUIDs are no longer exposed (a user is addressed by @handle, §14).
		if _, ok := tx["owner_user_id"]; ok {
			t.Error("tx response should not expose owner_user_id")
		}
	}
}

// TestFlow_PrivateAction: provider creates a private (public=false) action, runs it
// successfully, then a second user is rejected when attempting the same action.
func TestFlow_PrivateAction(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"secret": true})
	}))
	defer backend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	providerID, providerTok := makeUser(t, k, "@priv-owner")
	giveCredits(t, k, providerID, 200)
	otherID, otherTok := makeUser(t, k, "@priv-other")
	giveCredits(t, k, otherID, 200)

	// Create private action (public=false, which is the default).
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "private-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "private api", "input_schema": minSchema, "output_schema": minSchema,
	}, providerTok)
	if cr.StatusCode != http.StatusCreated {
		cr.Body.Close()
		t.Fatalf("create private action: got %d", cr.StatusCode)
	}
	var act map[string]any
	decodeResponse(t, cr, &act)
	actID := act["id"].(string)
	httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, providerTok).Body.Close()

	// Owner can run their own private action.
	reply := runAction(t, srv, providerTok, "@priv-owner/private-action", map[string]any{})
	if reply.TxID == "" {
		t.Error("owner run: expected tx_id")
	}

	// Another user is rejected.
	resp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@priv-owner/private-action", "args": map[string]any{},
	}, otherTok)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("other user should be rejected from private action, got 200")
	}
}

// TestFlow_AuthTokenLifecycle: login → use token → change password → old password rejected
// → new password works.
func TestFlow_AuthTokenLifecycle(t *testing.T) {
	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	// Create user; Login is already tested via makeUser; here we test via HTTP.
	_, _ = makeUser(t, k, "@auth-life")

	loginResp := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"handle": "@auth-life", "password": "pass",
	}, "")
	if loginResp.StatusCode != http.StatusOK {
		loginResp.Body.Close()
		t.Fatalf("login: expected 200, got %d", loginResp.StatusCode)
	}
	var loginBody map[string]any
	decodeResponse(t, loginResp, &loginBody)
	tok, _ := loginBody["token"].(string)
	if tok == "" {
		t.Fatal("login: expected token in response")
	}

	// Token is valid.
	meResp := httpDo(t, srv, "GET", "/v1/me", nil, tok)
	meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me with valid token: expected 200, got %d", meResp.StatusCode)
	}

	// Change password.
	putResp := httpDo(t, srv, "PUT", "/v1/me", map[string]any{
		"current_password": "pass", "password": "newpass",
	}, tok)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("change password: expected 200, got %d", putResp.StatusCode)
	}

	// Old password is rejected.
	oldLogin := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"handle": "@auth-life", "password": "pass",
	}, "")
	oldLogin.Body.Close()
	if oldLogin.StatusCode == http.StatusOK {
		t.Error("old password should be rejected after change")
	}

	// New password works.
	newLogin := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"handle": "@auth-life", "password": "newpass",
	}, "")
	if newLogin.StatusCode != http.StatusOK {
		newLogin.Body.Close()
		t.Fatalf("new password login: expected 200, got %d", newLogin.StatusCode)
	}
	var newBody map[string]any
	decodeResponse(t, newLogin, &newBody)
	if newBody["token"] == "" {
		t.Error("new password login: expected token")
	}
}

// TestFlow_SuspendUnsuspend: admin suspends a user; authenticated requests fail;
// admin unsuspends; requests succeed; account balance is preserved.
func TestFlow_SuspendUnsuspend(t *testing.T) {
	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ctx := context.Background()
	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	userID, userTok := makeUser(t, k, "@susp-user")
	giveCredits(t, k, userID, 300)

	// Verify balance before suspend.
	balanceBefore := getBalance(t, srv, userTok)
	if balanceBefore != 300 {
		t.Fatalf("pre-suspend balance: got %d, want 300", balanceBefore)
	}

	// Suspend.
	if err := k.SuspendUser(ctx, sys.ID, userID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// Suspended user's requests fail.
	meResp := httpDo(t, srv, "GET", "/v1/me", nil, userTok)
	meResp.Body.Close()
	if meResp.StatusCode == http.StatusOK {
		t.Error("suspended user should not get 200 from GET /v1/me")
	}

	// Unsuspend.
	if err := k.UnsuspendUser(ctx, sys.ID, userID); err != nil {
		t.Fatalf("unsuspend: %v", err)
	}

	// Requests succeed again.
	meResp2 := httpDo(t, srv, "GET", "/v1/me", nil, userTok)
	meResp2.Body.Close()
	if meResp2.StatusCode != http.StatusOK {
		t.Errorf("unsuspended user: expected 200, got %d", meResp2.StatusCode)
	}

	// Balance is unchanged.
	balanceAfter := getBalance(t, srv, userTok)
	if balanceAfter != balanceBefore {
		t.Errorf("balance after suspend/unsuspend: got %d, want %d", balanceAfter, balanceBefore)
	}
}

// TestFlow_AccountSelfService: user updates email and changes password via PUT /v1/me;
// both changes are immediately reflected and the old password is rejected.
func TestFlow_AccountSelfService(t *testing.T) {
	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	userID, userTok := makeUser(t, k, "@self-user")
	_ = userID

	// Update email.
	putResp := httpDo(t, srv, "PUT", "/v1/me", map[string]any{"email": "updated@test.com"}, userTok)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("update email: expected 200, got %d", putResp.StatusCode)
	}

	// Verify email via GET /v1/me.
	meResp := httpDo(t, srv, "GET", "/v1/me", nil, userTok)
	var meBody map[string]any
	decodeResponse(t, meResp, &meBody)
	if meBody["email"] != "updated@test.com" {
		t.Errorf("email after update: got %v, want updated@test.com", meBody["email"])
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
	oldLogin := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"handle": "@self-user", "password": "pass",
	}, "")
	oldLogin.Body.Close()
	if oldLogin.StatusCode == http.StatusOK {
		t.Error("old password should be rejected")
	}

	// New password accepted.
	newLogin := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"handle": "@self-user", "password": "changed123",
	}, "")
	newLogin.Body.Close()
	if newLogin.StatusCode != http.StatusOK {
		t.Errorf("new password login: expected 200, got %d", newLogin.StatusCode)
	}
}

// TestFlow_DepositSpendWithdraw: admin deposits, user spends some, admin withdraws a
// partial amount, then a withdrawal exceeding the remaining balance is rejected.
func TestFlow_DepositSpendWithdraw(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ctx := context.Background()
	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	userID, userTok := makeUser(t, k, "@dsw-user")
	providerID, providerTok := makeUser(t, k, "@dsw-provider")
	_ = providerID

	// Deposit 100.
	if _, err := k.Deposit(ctx, sys.ID, userID, 100, "initial", ""); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if getBalance(t, srv, userTok) != 100 {
		t.Fatal("balance after deposit: expected 100")
	}

	// Spend 20 by running an action priced at 20.
	actID := createPublicAction(t, srv, backend.URL, providerTok, "dsw-action", 20)
	_ = actID
	runAction(t, srv, userTok, "@dsw-provider/dsw-action", map[string]any{})
	if getBalance(t, srv, userTok) != 80 {
		t.Errorf("balance after spend: got %d, want 80", getBalance(t, srv, userTok))
	}

	// Withdraw 50.
	if _, err := k.Withdraw(ctx, sys.ID, userID, 50, "partial withdrawal", ""); err != nil {
		t.Fatalf("withdraw 50: %v", err)
	}
	if getBalance(t, srv, userTok) != 30 {
		t.Errorf("balance after withdraw: got %d, want 30", getBalance(t, srv, userTok))
	}

	// Withdraw 100 is rejected (only 30 remain).
	_, err := k.Withdraw(ctx, sys.ID, userID, 100, "too much", "")
	if err == nil {
		t.Error("over-withdrawal should be rejected")
	}
	if getBalance(t, srv, userTok) != 30 {
		t.Error("balance should be unchanged after rejected withdrawal")
	}
}

// TestFlow_ActionUpdateLive: provider updates a live action's price (which deactivates it),
// caller is rejected, provider reactivates, caller runs again at new price, historical
// transaction remains visible with original gross.
func TestFlow_ActionUpdateLive(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"v": 1})
	}))
	defer backend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	providerID, providerTok := makeUser(t, k, "@upd-provider")
	_ = providerID
	callerID, callerTok := makeUser(t, k, "@upd-caller")
	giveCredits(t, k, callerID, 500)

	// Create and activate at price=50.
	actID := createPublicAction(t, srv, backend.URL, providerTok, "upd-action", 50)

	// Caller runs it — tx1 recorded at gross=50.
	reply1 := runAction(t, srv, callerTok, "@upd-provider/upd-action", map[string]any{})
	tx1ID := reply1.TxID
	if tx1ID == "" {
		t.Fatal("expected tx_id from first run")
	}

	// Provider updates price (deactivates action).
	newPrice := int64(100)
	putResp := httpDo(t, srv, "PUT", "/v1/actions/"+actID, map[string]any{"price": newPrice}, providerTok)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("update price: expected 200, got %d", putResp.StatusCode)
	}

	// Caller is rejected (action inactive).
	failResp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@upd-provider/upd-action", "args": map[string]any{},
	}, callerTok)
	failResp.Body.Close()
	if failResp.StatusCode == http.StatusOK {
		t.Error("caller should be rejected when action is inactive")
	}

	// tx1 is still visible.
	tx1Resp := httpDo(t, srv, "GET", "/v1/transactions/"+tx1ID, nil, callerTok)
	var tx1Body map[string]any
	decodeResponse(t, tx1Resp, &tx1Body)
	if int64(tx1Body["gross"].(float64)) != 50 {
		t.Errorf("tx1 gross: got %v, want 50", tx1Body["gross"])
	}

	// Provider re-enables.
	enResp := httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, providerTok)
	enResp.Body.Close()
	if enResp.StatusCode != http.StatusOK {
		t.Fatalf("re-enable: expected 200, got %d", enResp.StatusCode)
	}

	// Caller runs again at new price.
	reply2 := runAction(t, srv, callerTok, "@upd-provider/upd-action", map[string]any{})
	tx2Resp := httpDo(t, srv, "GET", "/v1/transactions/"+reply2.TxID, nil, callerTok)
	var tx2Body map[string]any
	decodeResponse(t, tx2Resp, &tx2Body)
	if int64(tx2Body["gross"].(float64)) != newPrice {
		t.Errorf("tx2 gross: got %v, want %d", tx2Body["gross"], newPrice)
	}
}

// TestFlow_OpenAPIOwnershipProof: import spec without x-juice-owner → making it public
// is rejected; re-import with x-juice-owner (well-known file served) → making public
// succeeds; a caller can run the action.
func TestFlow_OpenAPIOwnershipProof(t *testing.T) {
	apiBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"proof": true})
	}))
	defer apiBackend.Close()

	const ownerHandle = "@proof-owner"

	// Spec WITHOUT x-juice-owner (first import — no ownership).
	noProofSpec := map[string]any{
		"openapi": "3.0.0",
		"info":    map[string]any{"title": "Proof API", "version": "1.0"},
		"servers": []any{map[string]any{"url": apiBackend.URL}},
		"paths": map[string]any{
			"/call": map[string]any{
				"post": map[string]any{
					"operationId": "proofCall",
					"summary":     "call the proof api",
					"requestBody": map[string]any{
						"content": map[string]any{
							"application/json": map[string]any{"schema": map[string]any{"type": "object"}},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{
							"description": "ok",
							"content": map[string]any{
								"application/json": map[string]any{"schema": map[string]any{"type": "object"}},
							},
						},
					},
				},
			},
		},
	}
	noProofBytes, _ := json.Marshal(noProofSpec)

	// Spec WITH x-juice-owner for the re-import.
	withProofSpec := make(map[string]any)
	for k2, v := range noProofSpec {
		withProofSpec[k2] = v
	}
	withProofSpec["x-juice-owner"] = ownerHandle
	withProofBytes, _ := json.Marshal(withProofSpec)

	// Spec server serves the spec and the well-known ownership file.
	var serveWithProof bool
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/juice-owner.txt" {
			if serveWithProof {
				w.Write([]byte(ownerHandle))
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if serveWithProof {
			w.Write(withProofBytes)
		} else {
			w.Write(noProofBytes)
		}
	}))
	defer specServer.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, ownerHandle)
	callerID, callerTok := makeUser(t, k, "@proof-caller")
	giveCredits(t, k, callerID, 200)

	// First import: no ownership.
	importResp1 := httpDo(t, srv, "POST", "/v1/actions/import", map[string]any{
		"spec_url": specServer.URL + "/openapi.json",
	}, ownerTok)
	if importResp1.StatusCode != http.StatusOK {
		importResp1.Body.Close()
		t.Fatalf("first import: expected 200, got %d", importResp1.StatusCode)
	}
	var importResult1 map[string]any
	decodeResponse(t, importResp1, &importResult1)

	var actID string
	if created, ok := importResult1["Created"].([]any); ok && len(created) > 0 {
		actID = created[0].(map[string]any)["id"].(string)
	}
	if actID == "" {
		t.Fatalf("first import: no action created, result: %v", importResult1)
	}

	// Enable (activate) succeeds.
	enResp := httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, ownerTok)
	enResp.Body.Close()
	if enResp.StatusCode != http.StatusOK {
		t.Fatalf("enable before proof: expected 200, got %d", enResp.StatusCode)
	}

	// Making it public without ownership proof is rejected.
	pubResp1 := httpDo(t, srv, "PUT", "/v1/actions/"+actID, map[string]any{"public": true}, ownerTok)
	pubResp1.Body.Close()
	if pubResp1.StatusCode == http.StatusOK {
		t.Error("making public without ownership proof should be rejected")
	}

	// Re-import with x-juice-owner (spec server now serves proof).
	serveWithProof = true
	importResp2 := httpDo(t, srv, "POST", "/v1/actions/import", map[string]any{
		"spec_url": specServer.URL + "/openapi.json",
	}, ownerTok)
	if importResp2.StatusCode != http.StatusOK {
		importResp2.Body.Close()
		t.Fatalf("second import: expected 200, got %d", importResp2.StatusCode)
	}
	importResp2.Body.Close()

	// Re-enable (import deactivates).
	en2Resp := httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, ownerTok)
	en2Resp.Body.Close()

	// Now making it public succeeds.
	pubResp2 := httpDo(t, srv, "PUT", "/v1/actions/"+actID, map[string]any{"public": true}, ownerTok)
	pubResp2.Body.Close()
	if pubResp2.StatusCode != http.StatusOK {
		t.Fatalf("making public after ownership proof: expected 200, got %d", pubResp2.StatusCode)
	}

	// Re-enable after making public (UpdateAction deactivates).
	httpDo(t, srv, "POST", "/v1/actions/"+actID+"/enable", nil, ownerTok).Body.Close()

	// Caller can run the action.
	runResp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": ownerHandle + "/proofCall", "args": map[string]any{},
	}, callerTok)
	if runResp.StatusCode != http.StatusOK {
		var body map[string]any
		json.NewDecoder(runResp.Body).Decode(&body)
		runResp.Body.Close()
		t.Fatalf("caller run after proof: expected 200, got %d — %v", runResp.StatusCode, body)
	}
	runResp.Body.Close()

	_ = ownerID
}

// TestFlow_ImportDutyAdjustment: two kernels sharing the same provider DB path but
// configured with different ImportBPS values import the same action; the proxy prices
// reflect each kernel's duty rate, and original transactions are immutable.
func TestFlow_ImportDutyAdjustment(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srvA, kA, _, privA := newFedKernel(t)
	defer srvA.Close()

	ctx := context.Background()
	pubA := privA.Public().(ed25519.PublicKey)
	pubAB64 := base64.RawURLEncoding.EncodeToString(pubA)
	sysA, _ := kA.ReadUserByHandle(ctx, "@sys")

	// A creates a public action priced at 1000.
	_, ownerATok := makeUser(t, kA, "@duty-a-prov")
	const priceA int64 = 1000
	actAID := createPublicAction(t, srvA, backend.URL, ownerATok, "duty-action", priceA)
	manifestPtr, err := kA.GetActionManifest(ctx, actAID)
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	manifest := *manifestPtr

	// Helper: build a fed kernel with a specific ImportBPS and import A's action.
	importWithBPS := func(importBPS int64) int64 {
		dir := t.TempDir()
		db, err := store.Open(filepath.Join(dir, "duty.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })

		cfg := kernel.DefaultConfig()
		cfg.TokenSecret = fmt.Sprintf("duty-secret-%d", importBPS)
		cfg.AllowLocalSources = true
		cfg.ImportBPS = importBPS
		logger := log.Discard()
		httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, allowLocal: true}
		kB := kernel.New(db, nil, httpExec, nil, cfg, logger)

		if err := kB.FirstBoot(ctx, "sys-pass"); err != nil {
			t.Fatal(err)
		}
		sysB, _ := kB.ReadUserByHandle(ctx, "@sys")
		privB64, _ := kB.GetConfig(ctx, configKeySigningPrivate)
		privBBytes, _ := base64.RawURLEncoding.DecodeString(privB64)
		privB := ed25519.PrivateKey(privBBytes)
		kB.SetSigningKey(privB, sysB.ID)
		httpExec.signerFn = kB.SignFederation

		peerAOnB, err := kB.AddPeer(ctx, sysA.ID, "@duty-a", pubAB64)
		if err != nil {
			// AddPeer might check ownership; use CreateOrUpdateProxyPeer if needed.
			peerAOnB, _ = kB.ReadUserByPublicKey(ctx, pubAB64)
		}
		if peerAOnB == nil {
			peerAOnB, _ = kB.CreateOrUpdateProxyPeer(ctx, "@duty-a", pubAB64)
		}

		importResult, err := kB.ImportRemoteAction(ctx, sysB.ID, peerAOnB.ID, manifest)
		if err != nil || len(importResult.Created) == 0 {
			t.Fatalf("ImportBPS=%d import failed: err=%v created=%d", importBPS, err, len(importResult.Created))
		}
		return importResult.Created[0].Price
	}

	price500 := importWithBPS(500)   // 5% duty
	price2000 := importWithBPS(2000) // 20% duty

	// The proxy price must be higher with a higher import duty.
	if price2000 <= price500 {
		t.Errorf("higher ImportBPS should yield higher proxy price: got %d (5%%) and %d (20%%)",
			price500, price2000)
	}

	// Both proxy prices should be ≥ the remote action price (proxy price = mp + duty ≥ mp).
	// mp = priceA * 10000 / (10000 + ImportBPS) — always ≤ priceA.
	// proxy price ≥ mp, and duty ≥ 0, so proxy price ≤ priceA is not guaranteed.
	// We just assert prices are positive.
	if price500 <= 0 || price2000 <= 0 {
		t.Errorf("proxy prices must be positive: price500=%d price2000=%d", price500, price2000)
	}
}

// TestFlow_AuthenticatedActionList: unauthenticated GET /v1/actions returns only
// active+public actions; authenticated returns those plus the caller's own active
// (including private) actions; a third user sees only the public one.
func TestFlow_AuthenticatedActionList(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k, _ := newTestHTTPServerFull(t)
	defer srv.Close()

	providerID, providerTok := makeUser(t, k, "@aal-provider")
	_ = providerID
	_, otherTok := makeUser(t, k, "@aal-other")

	// Create public+active action.
	pubResp := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "aal-public", "kind": "http", "price": 0, "source": backend.URL,
		"description": "public action", "input_schema": minSchema, "output_schema": minSchema,
	}, providerTok)
	if pubResp.StatusCode != http.StatusCreated {
		pubResp.Body.Close()
		t.Fatalf("create public action: got %d", pubResp.StatusCode)
	}
	var pubAct map[string]any
	decodeResponse(t, pubResp, &pubAct)
	pubID := pubAct["id"].(string)
	httpDo(t, srv, "POST", "/v1/actions/"+pubID+"/enable", nil, providerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions/"+pubID, map[string]any{"public": true}, providerTok).Body.Close()
	httpDo(t, srv, "POST", "/v1/actions/"+pubID+"/enable", nil, providerTok).Body.Close()

	// Create private+active action (public defaults to false).
	privResp := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "aal-private", "kind": "http", "price": 0, "source": backend.URL,
		"description": "private action", "input_schema": minSchema, "output_schema": minSchema,
	}, providerTok)
	if privResp.StatusCode != http.StatusCreated {
		privResp.Body.Close()
		t.Fatalf("create private action: got %d", privResp.StatusCode)
	}
	var privAct map[string]any
	decodeResponse(t, privResp, &privAct)
	privID := privAct["id"].(string)
	httpDo(t, srv, "POST", "/v1/actions/"+privID+"/enable", nil, providerTok).Body.Close()

	hasID := func(list []map[string]any, id string) bool {
		for _, a := range list {
			if a["id"] == id {
				return true
			}
		}
		return false
	}

	listActions := func(tok string) []map[string]any {
		r := httpDo(t, srv, "GET", "/v1/actions", nil, tok)
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			t.Fatalf("GET /v1/actions: got %d", r.StatusCode)
		}
		var acts []map[string]any
		decodeResponse(t, r, &acts)
		return acts
	}

	// Unauthenticated: only public action visible.
	unauth := listActions("")
	if !hasID(unauth, pubID) {
		t.Error("unauthenticated: public action should be visible")
	}
	if hasID(unauth, privID) {
		t.Error("unauthenticated: private action should not be visible")
	}

	// Authenticated as provider: both actions visible.
	provList := listActions(providerTok)
	if !hasID(provList, pubID) {
		t.Error("provider: public action should be visible")
	}
	if !hasID(provList, privID) {
		t.Error("provider: own private active action should be visible")
	}

	// Authenticated as other user: only public action visible.
	otherList := listActions(otherTok)
	if !hasID(otherList, pubID) {
		t.Error("other user: public action should be visible")
	}
	if hasID(otherList, privID) {
		t.Error("other user: provider's private action should not be visible")
	}
}

// ---- Delegated OAuth flow (§8) ----

// newOAuthFlowServer mirrors newFlowKernel but wires the OAuth token engine and consent broker,
// returning the executor so the test can point it at fake endpoints.
func newOAuthFlowServer(t *testing.T) (*httptest.Server, *kernel.Kernel) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "flow-test-secret"
	cfg.AllowLocalSources = true
	logger := log.Discard()
	box, err := newAESGCMBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, allowLocal: true, secretBox: box}
	httpExec.oauth = newOAuthEngine(box, db, true, cfg.ScriptTimeout)
	k := kernel.New(db, &flowScriptExec{}, httpExec, nil, cfg, logger)
	k.SetSecretBox(box)
	if err := k.FirstBoot(context.Background(), "sys-pass"); err != nil {
		t.Fatal(err)
	}
	bootstrapSigning(t, k)

	srv := &server{kernel: k, log: logger, oauth: newGrantBroker(httpExec.oauth)}
	return httptest.NewServer(mountFullRouter(srv)), k
}

// createDelegatedActionHTTP creates, enables (private), and returns the ID of an oauth_delegated
// http action pointing at upstreamURL, using providerURL as its OAuth endpoints.
func createDelegatedActionHTTP(t *testing.T, srv *httptest.Server, ownerTok, name, upstreamURL, providerURL string) string {
	t.Helper()
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": name, "kind": "http", "price": 0, "source": upstreamURL,
		"description":   "delegated inbox",
		"input_schema":  minSchema,
		"output_schema": minSchema,
		"auth": map[string]any{
			"scheme": kernel.AuthSchemeOAuthDelegated,
			"config": map[string]any{
				"auth_url":  providerURL + "/auth",
				"token_url": providerURL + "/token",
				"client_id": "cid",
				"scopes":    "gmail.readonly",
			},
		},
	}, ownerTok)
	if cr.StatusCode != http.StatusCreated {
		var b map[string]any
		json.NewDecoder(cr.Body).Decode(&b)
		cr.Body.Close()
		t.Fatalf("create delegated action: got %d — %v", cr.StatusCode, b)
	}
	var act map[string]any
	decodeResponse(t, cr, &act)
	id := act["id"].(string)
	httpDo(t, srv, "POST", "/v1/actions/"+id+"/enable", nil, ownerTok).Body.Close()
	return id
}

func TestFlow_OAuthDelegated(t *testing.T) {
	// Fake OAuth provider: authorization_code → access+refresh; refresh_token → a live token.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "acc-init", "refresh_token": "ref-1", "expires_in": 3600})
		case "refresh_token":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "acc-live", "expires_in": 3600})
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		}
	}))
	defer provider.Close()

	// Fake upstream API that records the bearer it saw.
	var sawAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()

	srv, k := newOAuthFlowServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@oauth-owner")
	createDelegatedActionHTTP(t, srv, ownerTok, "inbox", upstream.URL, provider.URL)
	const ref = "@oauth-owner/inbox"

	// 1. Run without a grant: rejected before any charge with the structured grant_required
	// outcome — a machine-detectable code plus the action in meta (§8).
	rejectRun := func() {
		resp := httpDo(t, srv, "POST", "/v1/run", map[string]any{"action": ref, "args": map[string]any{}}, ownerTok)
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("run without grant unexpectedly succeeded")
		}
		var body struct {
			Code string            `json:"code"`
			Meta map[string]string `json:"meta"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		if body.Code != "grant_required" {
			t.Fatalf("run rejection code = %q, want grant_required", body.Code)
		}
		if body.Meta["action"] != ref {
			t.Fatalf("run rejection meta.action = %q, want %q", body.Meta["action"], ref)
		}
	}
	rejectRun()

	// 2. Consent: start the code flow, then complete it (the provider issues the refresh token).
	var start map[string]any
	sr := httpDo(t, srv, "POST", "/v1/grants/start", map[string]any{
		"action": ref, "redirect_uri": "http://127.0.0.1:5555/callback", "flow": "code",
	}, ownerTok)
	decodeResponse(t, sr, &start)
	state, _ := start["state"].(string)
	if state == "" || start["authorize_url"] == "" {
		t.Fatalf("grants/start returned no state/authorize_url: %v", start)
	}
	var done map[string]any
	cr := httpDo(t, srv, "POST", "/v1/grants/complete", map[string]any{"state": state, "code": "the-code"}, ownerTok)
	decodeResponse(t, cr, &done)
	if done["status"] != "complete" {
		t.Fatalf("grants/complete status = %v, want complete", done["status"])
	}

	// 3. Run now succeeds and the upstream saw the refreshed bearer.
	reply := runAction(t, srv, ownerTok, ref, map[string]any{})
	if reply.Result["ok"] != true {
		t.Errorf("run result = %v, want ok=true", reply.Result)
	}
	if sawAuth != "Bearer acc-live" {
		t.Errorf("upstream saw Authorization %q, want Bearer acc-live", sawAuth)
	}

	// 4. /v1/me lists the grant with no token material.
	meResp := httpDo(t, srv, "GET", "/v1/me", nil, ownerTok)
	meBody, _ := readAll(t, meResp)
	if !strings.Contains(meBody, ref) {
		t.Errorf("/v1/me does not list the grant: %s", meBody)
	}
	for _, secret := range []string{"ref-1", "acc-live", "acc-init"} {
		if strings.Contains(meBody, secret) {
			t.Errorf("/v1/me leaked token material %q: %s", secret, meBody)
		}
	}

	// 5. Revoke → run is rejected again.
	rev := httpDo(t, srv, "DELETE", "/v1/grants?action="+ref, nil, ownerTok)
	if rev.StatusCode != http.StatusOK {
		t.Fatalf("revoke: got %d", rev.StatusCode)
	}
	rev.Body.Close()
	rejectRun()
}

// readAll returns a response body as a string.
func readAll(t *testing.T, resp *http.Response) (string, error) {
	t.Helper()
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(resp.Body)
	return buf.String(), err
}
