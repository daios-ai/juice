package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func newTestHTTPServer(t *testing.T) (*httptest.Server, *kernel.Kernel) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "serve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "serve-test-secret"
	cfg.AllowLocalSources = true
	logger := log.Discard()
	k := kernel.New(db, nil, &httpActionExecutor{timeout: cfg.ScriptTimeout}, nil, nil, cfg, logger)

	ctx := context.Background()
	if _, err := k.BootstrapSuperuser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "sys-pass",
	}, "superuser_handle"); err != nil {
		t.Fatal(err)
	}

	// Set up a signing key so buildReceipt works in all call tests.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	k.SetSigningKey(priv, sys.ID, "@sys")
	if err := k.SetConfig(ctx, configKeySuperuser, "@sys"); err != nil {
		t.Fatal(err)
	}

	srv := &server{kernel: k, log: logger}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestIDMiddleware)

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Post("/v1/auth/token", srv.postTokenMulti)
	r.Post("/v1/auth/authorize", srv.postAuthorize)
	r.Post("/v1/auth/refresh", srv.postRefresh)
	r.Post("/v1/auth/logout", srv.postLogout)
	r.Post("/v1/users", srv.postUser)
	r.Post("/v1/federation/call", srv.postFederationCall)
	r.Get("/v1/actions", srv.getActions)
	r.Group(func(r chi.Router) {
		r.Use(srv.authMiddleware)
		r.Post("/v1/actions", srv.postAction)
		r.Get("/v1/actions/{id}", srv.getAction)
		r.Put("/v1/actions/{id}", srv.updateAction)
		r.Post("/v1/actions/{id}/enable", srv.enableAction)
		r.Post("/v1/actions/{id}/disable", srv.disableAction)
		r.Delete("/v1/actions/{id}", srv.deleteAction)
		r.Post("/v1/actions/{id}/acl", srv.grantACL)
		r.Delete("/v1/actions/{id}/acl", srv.revokeACL)
		r.Post("/v1/actions/{id}/grant-all", srv.grantAll)
		r.Post("/v1/actions/{id}/revoke-all", srv.revokeAll)
		r.Get("/v1/processes", srv.listProcesses)
		r.Post("/v1/processes", srv.postProcess)
		r.Get("/v1/processes/{id}", srv.getProcess)
		r.Post("/v1/processes/{id}/fund", srv.fundProcess)
		r.Post("/v1/processes/{id}/end", srv.endProcess)
		r.Post("/v1/call", srv.postCall)
		r.Get("/v1/transactions", srv.listTransactions)
		r.Get("/v1/transactions/{id}", srv.getTransaction)
		r.Post("/v1/transactions/{id}/rate", srv.rateTransaction)
		r.Get("/v1/stats/{action_id}", srv.getStats)
		r.Get("/v1/listeners", srv.listListeners)
		r.Post("/v1/listeners", srv.postListener)
		r.Get("/v1/listeners/{id}", srv.getListenerMeta)
		r.Get("/v1/listeners/{id}/events", srv.pollListenerEvents)
		r.Delete("/v1/listeners/{id}", srv.deleteListener)
		r.Post("/v1/events/emit", srv.postEmit)
		r.Post("/v1/events/{id}/consume", srv.postConsumeEvent)
		r.Get("/v1/me", srv.getMe)
	})

	return httptest.NewServer(r), k
}

// makeUser creates a user and returns (userID, accessToken).
func makeUser(t *testing.T, k *kernel.Kernel, handle string) (string, string) {
	t.Helper()
	u, err := k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: handle, Email: handle + "@test.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := k.Login(context.Background(), handle, "pass")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID, tok
}

// giveCredits deposits funds into a user's account via the kernel directly.
// Uses the @sys superuser as the operator (required by the kernel-level deposit enforcement).
func giveCredits(t *testing.T, k *kernel.Kernel, userID string, amount int64) {
	t.Helper()
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatalf("giveCredits: @sys not found: %v", err)
	}
	if _, err := k.Deposit(ctx, sys.ID, userID, amount, "test"); err != nil {
		t.Fatal(err)
	}
}

func httpDo(t *testing.T, srv *httptest.Server, method, path string, body any, tok string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func httpDoForm(t *testing.T, srv *httptest.Server, path string, values url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", srv.URL+path, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeResponse(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// ---- tests ----

func TestServeHealth(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	resp := httpDo(t, srv, "GET", "/health", nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestServeCreateUser(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	resp := httpDo(t, srv, "POST", "/v1/users", map[string]any{
		"handle": "@http-alice", "email": "alice@example.com", "password": "testpass",
	}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
}

func TestServeAuthToken(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, err := k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "@http-bob", Email: "bob@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"handle": "@http-bob", "password": "pass",
	}, "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]string
	decodeResponse(t, resp, &result)
	if result["token"] == "" {
		t.Error("expected non-empty token")
	}
}

func TestServePKCEFlow(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, _ = makeUser(t, k, "@pkce-user")

	verifier := strings.Repeat("x", 43)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	// Step 1: authorize.
	resp := httpDoForm(t, srv, "/v1/auth/authorize", url.Values{
		"handle":         {"@pkce-user"},
		"password":       {"pass"},
		"code_challenge": {challenge},
	})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("authorize: expected 200, got %d", resp.StatusCode)
	}
	var authResp map[string]string
	decodeResponse(t, resp, &authResp)
	code := strings.TrimPrefix(authResp["redirect"], "?code=")
	if code == "" {
		t.Fatal("expected code in redirect")
	}

	// Step 2: exchange code for tokens.
	resp2 := httpDoForm(t, srv, "/v1/auth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
	})
	if resp2.StatusCode != http.StatusOK {
		resp2.Body.Close()
		t.Fatalf("token exchange: expected 200, got %d", resp2.StatusCode)
	}
	var tokenResp map[string]string
	decodeResponse(t, resp2, &tokenResp)
	if tokenResp["access_token"] == "" {
		t.Error("expected access_token")
	}
	if tokenResp["refresh_token"] == "" {
		t.Error("expected refresh_token")
	}

	// Step 3: refresh.
	resp3 := httpDo(t, srv, "POST", "/v1/auth/refresh", map[string]any{
		"refresh_token": tokenResp["refresh_token"],
	}, "")
	if resp3.StatusCode != http.StatusOK {
		resp3.Body.Close()
		t.Fatalf("refresh: expected 200, got %d", resp3.StatusCode)
	}
	var refreshResp map[string]string
	decodeResponse(t, resp3, &refreshResp)
	if refreshResp["access_token"] == "" {
		t.Error("refresh: expected new access_token")
	}
}

func TestServeAuthRequired(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	// POST /v1/actions requires authentication; GET is public (returns active public actions).
	resp := httpDo(t, srv, "POST", "/v1/actions", map[string]any{"name": "/x"}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", resp.StatusCode)
	}
}

func TestServeCreateAndGetAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	userID, tok := makeUser(t, k, "@srv-actowner")

	resp := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/http-action", "kind": "http",
		"price": 0, "source": "http://example.com",
	}, tok)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create action: expected 201, got %d", resp.StatusCode)
	}
	var action kernel.Action
	decodeResponse(t, resp, &action)
	if action.ID == "" {
		t.Error("expected action with ID")
	}
	if action.OwnerUserID != userID {
		t.Errorf("action owner: got %q, want %q", action.OwnerUserID, userID)
	}

	resp2 := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, tok)
	if resp2.StatusCode != http.StatusOK {
		resp2.Body.Close()
		t.Fatalf("get action: expected 200, got %d", resp2.StatusCode)
	}
	var fetched kernel.Action
	decodeResponse(t, resp2, &fetched)
	if fetched.ID != action.ID {
		t.Errorf("fetched action ID: got %q, want %q", fetched.ID, action.ID)
	}
}

func TestServeListActions(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@list-owner")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/list-me", "kind": "http", "price": 0, "source": "http://x.example",
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, tok).Body.Close()

	resp := httpDo(t, srv, "GET", "/v1/actions", nil, tok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list actions: expected 200, got %d", resp.StatusCode)
	}
	var actions []kernel.Action
	decodeResponse(t, resp, &actions)
	if len(actions) == 0 {
		t.Error("expected at least one action in list")
	}
}

func TestServeEnableDisableAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@toggle-owner")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/toggle-me", "kind": "http", "price": 0, "source": "http://x.example",
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)

	// Disable.
	r1 := httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/disable", nil, tok)
	defer r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("disable: expected 200, got %d", r1.StatusCode)
	}

	fetched := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, tok)
	var a1 kernel.Action
	decodeResponse(t, fetched, &a1)
	if a1.Active {
		t.Error("action should be inactive after disable")
	}

	// Enable.
	r2 := httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, tok)
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("enable: expected 200, got %d", r2.StatusCode)
	}

	fetched2 := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, tok)
	var a2 kernel.Action
	decodeResponse(t, fetched2, &a2)
	if !a2.Active {
		t.Error("action should be active after enable")
	}
}

func TestServeDeleteAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@del-owner")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/delete-me", "kind": "http", "price": 0, "source": "http://x.example",
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)

	del := httpDo(t, srv, "DELETE", "/v1/actions/"+action.ID, nil, tok)
	defer del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", del.StatusCode)
	}

	get := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, tok)
	defer get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Errorf("after delete: expected 404, got %d", get.StatusCode)
	}
}

func TestServeACL(t *testing.T) {
	// Set up a backend so the HTTP action can actually execute.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@acl-owner")
	_, user2Tok := makeUser(t, k, "@acl-user2")

	// Create a private (non-public) action.
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/acl-action", "kind": "http", "price": 0, "source": backend.URL,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	// Activate.
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, ownerTok).Body.Close()

	// user2 opens a process.
	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, user2Tok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	// user2 tries to call — should be denied (403).
	r1 := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": pid, "target": "@acl-owner", "action_name": "/acl-action",
	}, user2Tok)
	r1.Body.Close()
	if r1.StatusCode != http.StatusForbidden {
		t.Fatalf("before grant: expected 403, got %d", r1.StatusCode)
	}

	// Owner grants call permission to user2.
	user2, _ := k.ReadUserByHandle(context.Background(), "@acl-user2")
	gr := httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/acl", map[string]any{
		"subject_user_id": user2.ID, "permission": "call",
	}, ownerTok)
	gr.Body.Close()
	if gr.StatusCode != http.StatusNoContent {
		t.Fatalf("grant acl: expected 204, got %d", gr.StatusCode)
	}

	// user2 calls — should succeed.
	r2 := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": pid, "target": "@acl-owner", "action_name": "/acl-action",
	}, user2Tok)
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("after grant: expected 200, got %d", r2.StatusCode)
	}

	// Owner revokes permission.
	rv := httpDo(t, srv, "DELETE", "/v1/actions/"+action.ID+"/acl", map[string]any{
		"subject_user_id": user2.ID, "permission": "call",
	}, ownerTok)
	rv.Body.Close()
	if rv.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke acl: expected 204, got %d", rv.StatusCode)
	}

	// user2 calls again — should be denied.
	r3 := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": pid, "target": "@acl-owner", "action_name": "/acl-action",
	}, user2Tok)
	r3.Body.Close()
	if r3.StatusCode != http.StatusForbidden {
		t.Fatalf("after revoke: expected 403, got %d", r3.StatusCode)
	}
}

func TestServeGrantRevokeAll(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@ga-owner")
	_, user3Tok := makeUser(t, k, "@ga-user3")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/public-action", "kind": "http", "price": 0, "source": backend.URL,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, ownerTok).Body.Close()

	// user3 opens a process.
	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, user3Tok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	// Before grant-all: user3 cannot call.
	r1 := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": pid, "target": "@ga-owner", "action_name": "/public-action",
	}, user3Tok)
	r1.Body.Close()
	if r1.StatusCode != http.StatusForbidden {
		t.Fatalf("before grant-all: expected 403, got %d", r1.StatusCode)
	}

	// Grant-all.
	ga := httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/grant-all", nil, ownerTok)
	ga.Body.Close()
	if ga.StatusCode != http.StatusNoContent {
		t.Fatalf("grant-all: expected 204, got %d", ga.StatusCode)
	}

	// user3 can now call.
	r2 := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": pid, "target": "@ga-owner", "action_name": "/public-action",
	}, user3Tok)
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("after grant-all: expected 200, got %d", r2.StatusCode)
	}

	// Revoke-all.
	ra := httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/revoke-all", nil, ownerTok)
	ra.Body.Close()
	if ra.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke-all: expected 204, got %d", ra.StatusCode)
	}

	// user3 can no longer call.
	r3 := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": pid, "target": "@ga-owner", "action_name": "/public-action",
	}, user3Tok)
	r3.Body.Close()
	if r3.StatusCode != http.StatusForbidden {
		t.Fatalf("after revoke-all: expected 403, got %d", r3.StatusCode)
	}
}

func TestServeProcessLifecycle(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@srv-proc")

	resp := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, tok)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create process: expected 201, got %d", resp.StatusCode)
	}
	var proc map[string]any
	decodeResponse(t, resp, &proc)
	pid := proc["process_id"].(string)
	if pid == "" {
		t.Fatal("expected process_id")
	}

	// Get process.
	get := httpDo(t, srv, "GET", "/v1/processes/"+pid, nil, tok)
	if get.StatusCode != http.StatusOK {
		get.Body.Close()
		t.Fatalf("get process: expected 200, got %d", get.StatusCode)
	}
	var p kernel.Process
	decodeResponse(t, get, &p)
	if p.ID != pid {
		t.Errorf("get process: ID mismatch")
	}

	// End process.
	end := httpDo(t, srv, "POST", "/v1/processes/"+pid+"/end", nil, tok)
	defer end.Body.Close()
	if end.StatusCode != http.StatusNoContent {
		t.Errorf("end process: expected 204, got %d", end.StatusCode)
	}
}

func TestServeFundProcess(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	userID, tok := makeUser(t, k, "@fund-user")
	giveCredits(t, k, userID, 200)

	// Start process with 0 initial funds.
	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, tok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	// Fund the process via HTTP.
	fr := httpDo(t, srv, "POST", "/v1/processes/"+pid+"/fund", map[string]any{"funds": 100}, tok)
	defer fr.Body.Close()
	if fr.StatusCode != http.StatusNoContent {
		t.Fatalf("fund process: expected 204, got %d", fr.StatusCode)
	}

	// Verify available funds increased.
	get := httpDo(t, srv, "GET", "/v1/processes/"+pid, nil, tok)
	var p kernel.Process
	decodeResponse(t, get, &p)
	if p.Available != 100 {
		t.Errorf("funded process: expected 100 available, got %d", p.Available)
	}
}

func TestServeCall(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"answer": 42})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@call-owner")
	_, callerTok := makeUser(t, k, "@call-caller")

	// Create and activate a free public HTTP action.
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/answer", "kind": "http", "price": 0, "source": backend.URL,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/grant-all", nil, ownerTok).Body.Close()

	// Caller opens a process.
	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, callerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	// Make the call.
	callResp := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id":  pid,
		"target":      "@call-owner",
		"action_name": "/answer",
		"args":        map[string]any{},
	}, callerTok)
	if callResp.StatusCode != http.StatusOK {
		callResp.Body.Close()
		t.Fatalf("call: expected 200, got %d", callResp.StatusCode)
	}
	var reply kernel.CallReply
	decodeResponse(t, callResp, &reply)
	if reply.TxID == "" {
		t.Error("expected tx_id in call reply")
	}
	if reply.Result["answer"] != float64(42) {
		t.Errorf("call result: expected answer=42, got %v", reply.Result)
	}
}

func TestServeListAndGetTransaction(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@tx-owner")
	_, callerTok := makeUser(t, k, "@tx-caller")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/tx-action", "kind": "http", "price": 0, "source": backend.URL,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/grant-all", nil, ownerTok).Body.Close()

	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, callerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	call := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": pid, "target": "@tx-owner", "action_name": "/tx-action",
	}, callerTok)
	var callReply kernel.CallReply
	decodeResponse(t, call, &callReply)
	txID := callReply.TxID
	if txID == "" {
		t.Fatal("expected tx_id from call")
	}

	// List transactions.
	list := httpDo(t, srv, "GET", "/v1/transactions", nil, callerTok)
	if list.StatusCode != http.StatusOK {
		list.Body.Close()
		t.Fatalf("list transactions: expected 200, got %d", list.StatusCode)
	}
	var txs []kernel.Transaction
	decodeResponse(t, list, &txs)
	if len(txs) == 0 {
		t.Error("expected at least one transaction")
	}

	// Get transaction by ID.
	get := httpDo(t, srv, "GET", "/v1/transactions/"+txID, nil, callerTok)
	if get.StatusCode != http.StatusOK {
		get.Body.Close()
		t.Fatalf("get transaction: expected 200, got %d", get.StatusCode)
	}
	var tx kernel.Transaction
	decodeResponse(t, get, &tx)
	if tx.ID != txID {
		t.Errorf("get transaction: ID mismatch, got %v", tx.ID)
	}
}

func TestServeRateTransaction(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@rate-owner")
	_, callerTok := makeUser(t, k, "@rate-caller")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/rate-action", "kind": "http", "price": 0, "source": backend.URL,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/grant-all", nil, ownerTok).Body.Close()

	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, callerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	call := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": pid, "target": "@rate-owner", "action_name": "/rate-action",
	}, callerTok)
	var callReply kernel.CallReply
	decodeResponse(t, call, &callReply)
	txID := callReply.TxID
	if txID == "" {
		t.Fatal("expected tx_id from call")
	}

	// Rate it (caller is the process owner); rating must be 0 or 1.
	rate := httpDo(t, srv, "POST", "/v1/transactions/"+txID+"/rate", map[string]any{
		"rating": 1,
	}, callerTok)
	defer rate.Body.Close()
	if rate.StatusCode != http.StatusNoContent {
		t.Fatalf("rate transaction: expected 204, got %d", rate.StatusCode)
	}

	// Rating is stored in the ratings table (not on the transaction row).
}

func TestServeRateTransactionNotFound(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@rater")

	resp := httpDo(t, srv, "POST", "/v1/transactions/no-such-id/rate",
		map[string]any{"rating": 1.0}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown tx, got %d", resp.StatusCode)
	}
}

func TestServeGetStats(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@stats-owner")
	_, callerTok := makeUser(t, k, "@stats-caller")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/stats-action", "kind": "http", "price": 0, "source": backend.URL,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/grant-all", nil, ownerTok).Body.Close()

	// Make one call to generate stats.
	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, callerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": proc["process_id"], "target": "@stats-owner", "action_name": "/stats-action",
	}, callerTok).Body.Close()

	get := httpDo(t, srv, "GET", "/v1/stats/"+action.ID, nil, ownerTok)
	if get.StatusCode != http.StatusOK {
		get.Body.Close()
		t.Fatalf("get stats: expected 200, got %d", get.StatusCode)
	}
	var stats kernel.Stats
	decodeResponse(t, get, &stats)
	if stats.Uses == 0 {
		t.Error("expected uses > 0 in stats after a call")
	}
}

func TestServeLookupEndpointRemoved(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@lookup-user")

	// POST /v1/lookup was removed; the route no longer exists.
	resp := httpDo(t, srv, "POST", "/v1/lookup", map[string]any{"query": "something"}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for removed /v1/lookup, got %d", resp.StatusCode)
	}
}

func TestServeListenerFlow(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"consumed": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@lst-owner")
	sourceID, sourceTok := makeUser(t, k, "@lst-source")

	// Owner creates a free public action.
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/lst-action", "kind": "http", "price": 0, "source": backend.URL,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/grant-all", nil, ownerTok).Body.Close()

	// Owner creates a listener watching the source user.
	lr := httpDo(t, srv, "POST", "/v1/listeners", map[string]any{
		"source_user_id":   sourceID,
		"event_name":       "test.ping",
		"target_action_id": action.ID,
	}, ownerTok)
	if lr.StatusCode != http.StatusCreated {
		lr.Body.Close()
		t.Fatalf("create listener: expected 201, got %d", lr.StatusCode)
	}
	var listener kernel.Listener
	decodeResponse(t, lr, &listener)
	lid := listener.ID
	if lid == "" {
		t.Fatal("expected listener ID")
	}

	// Source user emits event.
	emit := httpDo(t, srv, "POST", "/v1/events/emit", map[string]any{
		"event_name": "test.ping", "args": map[string]any{},
	}, sourceTok)
	if emit.StatusCode != http.StatusOK {
		emit.Body.Close()
		t.Fatalf("emit: expected 200, got %d", emit.StatusCode)
	}
	var emitResp map[string]any
	decodeResponse(t, emit, &emitResp)
	eventIDs, _ := emitResp["event_ids"].([]any)
	if len(eventIDs) == 0 {
		t.Fatal("expected at least one event_id from emit")
	}
	eventID := eventIDs[0].(string)

	// Poll listener events — should have one pending event.
	poll := httpDo(t, srv, "GET", "/v1/listeners/"+lid+"/events", nil, ownerTok)
	if poll.StatusCode != http.StatusOK {
		poll.Body.Close()
		t.Fatalf("poll listener events: expected 200, got %d", poll.StatusCode)
	}
	var pollResp map[string]any
	decodeResponse(t, poll, &pollResp)
	events, _ := pollResp["events"].([]any)
	if len(events) == 0 {
		t.Error("expected pending event in listener poll")
	}

	// Owner opens a process to consume the event.
	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, ownerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	// Consume the event.
	consume := httpDo(t, srv, "POST", "/v1/events/"+eventID+"/consume", map[string]any{
		"process_id": pid,
	}, ownerTok)
	if consume.StatusCode != http.StatusOK {
		consume.Body.Close()
		t.Fatalf("consume event: expected 200, got %d", consume.StatusCode)
	}
	consume.Body.Close()

	// Delete listener.
	del := httpDo(t, srv, "DELETE", "/v1/listeners/"+lid, nil, ownerTok)
	defer del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete listener: expected 204, got %d", del.StatusCode)
	}
}

func TestServeUpdateAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@upd-owner")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/upd-action", "kind": "http", "price": 0, "source": "http://x.example",
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, tok).Body.Close()

	newDesc := "updated description"
	resp := httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{
		"description": newDesc,
	}, tok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update action: expected 200, got %d", resp.StatusCode)
	}
	var updated kernel.Action
	decodeResponse(t, resp, &updated)
	if updated.Description != newDesc {
		t.Errorf("description: got %q, want %q", updated.Description, newDesc)
	}
	// Update must deactivate the action (source/schema change wasn't made here but description is safe;
	// price/source changes deactivate — just verify the response has the action).
	if updated.ID != action.ID {
		t.Errorf("ID mismatch after update")
	}
}

func TestServeListProcesses(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@lp-user")

	// Create two processes.
	httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, tok).Body.Close()
	httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, tok).Body.Close()

	resp := httpDo(t, srv, "GET", "/v1/processes", nil, tok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list processes: expected 200, got %d", resp.StatusCode)
	}
	var processes []kernel.Process
	decodeResponse(t, resp, &processes)
	if len(processes) != 2 {
		t.Errorf("list processes: got %d, want 2", len(processes))
	}
}

func TestServeListListeners(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@ll-owner")
	sourceID, _ := makeUser(t, k, "@ll-source")

	act := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/ll-action", "kind": "http", "price": 0, "source": "http://ll.example",
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, act, &action)
	_ = ownerID
	_ = sourceID

	// Create two listeners.
	httpDo(t, srv, "POST", "/v1/listeners", map[string]any{
		"source_user_id": sourceID, "event_name": "a", "target_action_id": action.ID,
	}, ownerTok).Body.Close()
	httpDo(t, srv, "POST", "/v1/listeners", map[string]any{
		"source_user_id": sourceID, "event_name": "b", "target_action_id": action.ID,
	}, ownerTok).Body.Close()

	resp := httpDo(t, srv, "GET", "/v1/listeners", nil, ownerTok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list listeners: expected 200, got %d", resp.StatusCode)
	}
	var listeners []kernel.Listener
	decodeResponse(t, resp, &listeners)
	if len(listeners) != 2 {
		t.Errorf("list listeners: got %d, want 2", len(listeners))
	}
}

func TestServePollListenerEvents(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@poll-owner")
	sourceID, sourceTok := makeUser(t, k, "@poll-source")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/poll-action", "kind": "http", "price": 0, "source": backend.URL,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/grant-all", nil, ownerTok).Body.Close()
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, ownerTok).Body.Close()

	lr := httpDo(t, srv, "POST", "/v1/listeners", map[string]any{
		"source_user_id": sourceID, "event_name": "ping", "target_action_id": action.ID,
	}, ownerTok)
	var listener kernel.Listener
	decodeResponse(t, lr, &listener)

	httpDo(t, srv, "POST", "/v1/events/emit", map[string]any{
		"event_name": "ping", "args": map[string]any{},
	}, sourceTok).Body.Close()

	// Poll via the new /events sub-path.
	resp := httpDo(t, srv, "GET", "/v1/listeners/"+listener.ID+"/events", nil, ownerTok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("poll events: expected 200, got %d", resp.StatusCode)
	}
	var result map[string]any
	decodeResponse(t, resp, &result)
	events, _ := result["events"].([]any)
	if len(events) == 0 {
		t.Error("expected at least one pending event")
	}
}

func TestServeLogout(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, _ = makeUser(t, k, "@logout-user")

	// Obtain a refresh token via PKCE.
	verifier := strings.Repeat("y", 43)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	authResp := httpDoForm(t, srv, "/v1/auth/authorize", url.Values{
		"handle": {"@logout-user"}, "password": {"pass"}, "code_challenge": {challenge},
	})
	var authResult map[string]string
	decodeResponse(t, authResp, &authResult)
	code := strings.TrimPrefix(authResult["redirect"], "?code=")

	tokenResp := httpDoForm(t, srv, "/v1/auth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
	})
	var tokenResult map[string]string
	decodeResponse(t, tokenResp, &tokenResult)
	refreshToken := tokenResult["refresh_token"]
	if refreshToken == "" {
		t.Fatal("expected refresh token from PKCE flow")
	}

	// Logout — revoke the refresh token.
	out := httpDo(t, srv, "POST", "/v1/auth/logout", map[string]any{
		"refresh_token": refreshToken,
	}, "")
	out.Body.Close()
	if out.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: expected 204, got %d", out.StatusCode)
	}

	// Refresh with the revoked token must fail.
	ref := httpDo(t, srv, "POST", "/v1/auth/refresh", map[string]any{
		"refresh_token": refreshToken,
	}, "")
	ref.Body.Close()
	if ref.StatusCode != http.StatusUnauthorized {
		t.Errorf("refresh after logout: expected 401, got %d", ref.StatusCode)
	}
}

func TestRateLimitLogin(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "rl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "rl-test-secret"
	logger := log.Discard()
	k := kernel.New(db, nil, nil, nil, nil, cfg, logger)
	if _, err := k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "@rlu", Email: "rlu@example.com", Password: "pass",
	}); err != nil {
		t.Fatal(err)
	}

	srv := &server{kernel: k, log: logger}
	r := chi.NewRouter()
	r.Use(requestIDMiddleware)
	// Burst of 3 with zero refill rate so tokens don't recover during the test.
	r.With(ipRateLimiter(0, 3)).Post("/v1/auth/token", srv.postTokenMulti)
	ts := httptest.NewServer(r)
	defer ts.Close()

	body := map[string]any{"handle": "@rlu", "password": "pass"}
	for i := 0; i < 5; i++ {
		resp := httpDo(t, ts, "POST", "/v1/auth/token", body, "")
		resp.Body.Close()
		if i < 3 {
			if resp.StatusCode == http.StatusTooManyRequests {
				t.Errorf("request %d: unexpected 429 within burst", i+1)
			}
		} else {
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Errorf("request %d: expected 429 after burst, got %d", i+1, resp.StatusCode)
			}
		}
	}
}

func TestServeRequestIDHeader(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	resp := httpDo(t, srv, "POST", "/v1/users", map[string]any{
		"handle": "@ridtest", "email": "rid@example.com", "password": "p",
	}, "")
	defer resp.Body.Close()
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID header in response")
	}
}

func TestServeGetMe(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	uid, tok := makeUser(t, k, "@metest")
	giveCredits(t, k, uid, 500)

	resp := httpDo(t, srv, "GET", "/v1/me", nil, tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me: expected 200, got %d", resp.StatusCode)
	}
	var got map[string]any
	decodeResponse(t, resp, &got)

	if got["handle"] != "@metest" {
		t.Errorf("handle: got %v, want @metest", got["handle"])
	}
	if got["email"] != "@metest@test.com" {
		t.Errorf("email: got %v, want @metest@test.com", got["email"])
	}
	if got["available"].(float64) != 500 {
		t.Errorf("available: got %v, want 500", got["available"])
	}

	// Unauthenticated request must be rejected.
	resp2 := httpDo(t, srv, "GET", "/v1/me", nil, "")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated: expected 401, got %d", resp2.StatusCode)
	}
}

func TestGetActionRequiresReadPermission(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@ra-owner")
	_, strangerTok := makeUser(t, k, "@ra-stranger")
	_, readerTok := makeUser(t, k, "@ra-reader")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "/ra-action", "kind": "http", "price": 0, "source": "http://example.com",
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)

	// Owner can read their own action.
	r1 := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, ownerTok)
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Errorf("owner: expected 200, got %d", r1.StatusCode)
	}

	// Stranger has no read permission — expect 403.
	r2 := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, strangerTok)
	r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden {
		t.Errorf("stranger: expected 403, got %d", r2.StatusCode)
	}

	// Grant read permission to reader via kernel.
	reader, _ := k.ReadUserByHandle(context.Background(), "@ra-reader")
	gr := httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/acl", map[string]any{
		"subject_user_id": reader.ID, "permission": "read",
	}, ownerTok)
	gr.Body.Close()
	if gr.StatusCode != http.StatusNoContent {
		t.Fatalf("grant read: expected 204, got %d", gr.StatusCode)
	}

	// Reader can now access the action.
	r3 := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, readerTok)
	r3.Body.Close()
	if r3.StatusCode != http.StatusOK {
		t.Errorf("reader with ACL: expected 200, got %d", r3.StatusCode)
	}
}

func TestFederationCall(t *testing.T) {
	// Stand up a backend that returns {"pong": true}.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"pong":true}`))
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}

	// Register a public /ping action on @sys pointing to the backend.
	a, err := k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: sys.ID,
		Name:        "/ping",
		Kind:        kernel.KindHTTP,
		Source:      backend.URL,
		Price:       0,
		Description: "ping",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := k.GrantAll(ctx, sys.ID, a.ID); err != nil {
		t.Fatal(err)
	}

	// Store the superuser handle in config (required by postFederationCall).
	if err := k.SetConfig(ctx, configKeySuperuser, "@sys"); err != nil {
		t.Fatal(err)
	}

	// POST to federation endpoint — no auth required.
	resp := httpDo(t, srv, "POST", "/v1/federation/call?action=/ping", map[string]any{}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var envelope map[string]any
	json.NewDecoder(resp.Body).Decode(&envelope)
	resultMap, _ := envelope["result"].(map[string]any)
	if resultMap["pong"] != true {
		t.Errorf("expected pong:true in result, got %v", envelope)
	}

	// Unknown action returns not found.
	resp2 := httpDo(t, srv, "POST", "/v1/federation/call?action=/nope", map[string]any{}, "")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown action: expected 404, got %d", resp2.StatusCode)
	}
}
