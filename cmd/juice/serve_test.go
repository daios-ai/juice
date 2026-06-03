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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
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
	if err := k.FirstBoot(ctx, "sys-pass"); err != nil {
		t.Fatal(err)
	}

	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	privB64, err := k.GetConfig(ctx, configKeySigningPrivate)
	if err != nil {
		t.Fatal(err)
	}
	privBytes, err := base64.RawURLEncoding.DecodeString(privB64)
	if err != nil {
		t.Fatal(err)
	}
	priv := ed25519.PrivateKey(privBytes)
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
		r.Post("/v1/actions/import", srv.importOpenAPI)
		r.Post("/v1/actions/unimport", srv.unimportOpenAPI)
		r.Post("/v1/actions", srv.postAction)
		r.Get("/v1/actions/{id}", srv.getAction)
		r.Get("/v1/actions/{id}/ratings", srv.listActionRatings)
		r.Put("/v1/actions/{id}", srv.updateAction)
		r.Post("/v1/actions/{id}/enable", srv.enableAction)
		r.Post("/v1/actions/{id}/disable", srv.disableAction)
		r.Delete("/v1/actions/{id}", srv.deleteAction)
		r.Post("/v1/actions/{id}/acl", srv.grantACL)
		r.Delete("/v1/actions/{id}/acl/{subject_id}/{permission}", srv.revokeACL)
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

// httpDoWithHeaders sends an HTTP request with additional headers.
func httpDoWithHeaders(t *testing.T, srv *httptest.Server, method, path string, body any, tok string, extra map[string]string) *http.Response {
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
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// fedHeaders builds signed auth headers for a federation call.
func fedHeaders(t *testing.T, priv ed25519.PrivateKey, action, idempKey string) map[string]string {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339)
	sig, err := kernel.SignFederationPayload(priv, action, idempKey, ts)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"X-Timestamp":       ts,
		"X-Idempotency-Key": idempKey,
		"X-Signature":       sig,
	}
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

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := body["password_hash"]; ok {
		t.Error("response must not contain password_hash")
	}
	if body["handle"] != "@http-alice" {
		t.Errorf("response handle: got %v", body["handle"])
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
	resp := httpDo(t, srv, "POST", "/v1/auth/authorize", map[string]any{
		"handle":         "@pkce-user",
		"password":       "pass",
		"code_challenge": challenge,
	}, "")
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
	resp2 := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"code_verifier": verifier,
	}, "")
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
	resp := httpDo(t, srv, "POST", "/v1/actions", map[string]any{"name": "x"}, "")
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
		"name": "http-action", "kind": "http",
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
		"name": "list-me", "kind": "http", "price": 0, "source": "http://x.example",
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
	if len(actions) != 0 {
		t.Fatal("private action should not appear in public list")
	}
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/grant-all", nil, tok).Body.Close()
	resp = httpDo(t, srv, "GET", "/v1/actions", nil, tok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list actions after grant-all: expected 200, got %d", resp.StatusCode)
	}
	decodeResponse(t, resp, &actions)
	if len(actions) == 0 {
		t.Error("expected public action in list")
	}
}

func TestServeEnableDisableAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@toggle-owner")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "toggle-me", "kind": "http", "price": 0, "source": "http://x.example",
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
		"name": "delete-me", "kind": "http", "price": 0, "source": "http://x.example",
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
		"name": "acl-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
		"process_id": pid, "action": "@acl-owner/acl-action",
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
		"process_id": pid, "action": "@acl-owner/acl-action",
	}, user2Tok)
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("after grant: expected 200, got %d", r2.StatusCode)
	}

	// Owner revokes permission.
	rv := httpDo(t, srv, "DELETE", "/v1/actions/"+action.ID+"/acl/"+user2.ID+"/call", nil, ownerTok)
	rv.Body.Close()
	if rv.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke acl: expected 204, got %d", rv.StatusCode)
	}

	// user2 calls again — should be denied.
	r3 := httpDo(t, srv, "POST", "/v1/call", map[string]any{
		"process_id": pid, "action": "@acl-owner/acl-action",
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
		"name": "public-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
		"process_id": pid, "action": "@ga-owner/public-action",
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
		"process_id": pid, "action": "@ga-owner/public-action",
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
		"process_id": pid, "action": "@ga-owner/public-action",
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

func TestServeGetProcessUnauthorized(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@proc-owner")
	_, otherTok := makeUser(t, k, "@proc-other")

	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, ownerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	// Non-owner must get 403.
	get := httpDo(t, srv, "GET", "/v1/processes/"+pid, nil, otherTok)
	defer get.Body.Close()
	if get.StatusCode != http.StatusForbidden {
		t.Errorf("non-owner get process: expected 403, got %d", get.StatusCode)
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

	// Fund the process via HTTP — returns 200 with updated process body.
	fr := httpDo(t, srv, "POST", "/v1/processes/"+pid+"/fund", map[string]any{"funds": 100}, tok)
	defer fr.Body.Close()
	if fr.StatusCode != http.StatusOK {
		t.Fatalf("fund process: expected 200, got %d", fr.StatusCode)
	}
	var p kernel.Process
	decodeResponse(t, fr, &p)
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
		"name": "answer", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
		"process_id": pid,
		"action":     "@call-owner/answer",
		"args":       map[string]any{},
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
		"name": "tx-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
		"process_id": pid, "action": "@tx-owner/tx-action",
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
		"name": "rate-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
		"process_id": pid, "action": "@rate-owner/rate-action",
	}, callerTok)
	var callReply kernel.CallReply
	decodeResponse(t, call, &callReply)
	txID := callReply.TxID
	if txID == "" {
		t.Fatal("expected tx_id from call")
	}

	rate := httpDo(t, srv, "POST", "/v1/transactions/"+txID+"/rate", map[string]any{
		"rating": 1,
	}, callerTok)
	if rate.StatusCode != http.StatusOK {
		rate.Body.Close()
		t.Fatalf("rate transaction: expected 200, got %d", rate.StatusCode)
	}
	var ratingResp kernel.Rating
	decodeResponse(t, rate, &ratingResp)
	if ratingResp.ID == "" {
		t.Error("expected rating ID in response")
	}
	if ratingResp.Signature == "" {
		t.Error("expected signature in rating response")
	}
	if ratingResp.RatedTxID != txID {
		t.Errorf("rated_tx_id: got %q, want %q", ratingResp.RatedTxID, txID)
	}

	// Rating is stored in the ratings table (not on the transaction row).
}

func TestServeListActionRatings(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@list-ratings-owner")
	_, callerTok := makeUser(t, k, "@list-ratings-caller")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "list-ratings-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
		"process_id": pid, "action": "@list-ratings-owner/list-ratings-action",
	}, callerTok)
	var callReply kernel.CallReply
	decodeResponse(t, call, &callReply)
	txID := callReply.TxID
	if txID == "" {
		t.Fatal("expected tx_id from call")
	}

	rate := httpDo(t, srv, "POST", "/v1/transactions/"+txID+"/rate", map[string]any{"rating": 1}, callerTok)
	if rate.StatusCode != http.StatusOK {
		rate.Body.Close()
		t.Fatalf("rate transaction: expected 200, got %d", rate.StatusCode)
	}
	rate.Body.Close()

	resp := httpDo(t, srv, "GET", "/v1/actions/"+action.ID+"/ratings", nil, ownerTok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list ratings: expected 200, got %d", resp.StatusCode)
	}
	var ratings []kernel.Rating
	decodeResponse(t, resp, &ratings)
	if len(ratings) == 0 {
		t.Fatal("expected at least one rating in response")
	}
	if ratings[0].RatedTxID != txID {
		t.Errorf("rated_tx_id: got %q, want %q", ratings[0].RatedTxID, txID)
	}
}

func TestServeRateTransactionNotFound(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	tok, err := k.Login(context.Background(), "@sys", "sys-pass")
	if err != nil {
		t.Fatal(err)
	}

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
		"name": "stats-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
		"process_id": proc["process_id"], "action": "@stats-owner/stats-action",
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
		"name": "lst-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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

	// Poll listener events — should have one pending event (returns array directly).
	poll := httpDo(t, srv, "GET", "/v1/listeners/"+lid+"/events", nil, ownerTok)
	if poll.StatusCode != http.StatusOK {
		poll.Body.Close()
		t.Fatalf("poll listener events: expected 200, got %d", poll.StatusCode)
	}
	var events []any
	decodeResponse(t, poll, &events)
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
		"name": "upd-action", "kind": "http", "price": 0, "source": "http://x.example",
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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
		"name": "ll-action", "kind": "http", "price": 0, "source": "http://ll.example",
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
		"name": "poll-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
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

	// Poll via the new /events sub-path — returns array directly.
	resp := httpDo(t, srv, "GET", "/v1/listeners/"+listener.ID+"/events", nil, ownerTok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("poll events: expected 200, got %d", resp.StatusCode)
	}
	var events []any
	decodeResponse(t, resp, &events)
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

	authResp := httpDo(t, srv, "POST", "/v1/auth/authorize", map[string]any{
		"handle": "@logout-user", "password": "pass", "code_challenge": challenge,
	}, "")
	var authResult map[string]string
	decodeResponse(t, authResp, &authResult)
	code := strings.TrimPrefix(authResult["redirect"], "?code=")

	tokenResp := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"grant_type": "authorization_code", "code": code, "code_verifier": verifier,
	}, "")
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
		"name": "ra-action", "kind": "http", "price": 0, "source": "http://example.com",
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

	// Generate a real keypair for the calling remote kernel.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	// Register the remote peer with its real public key.
	_, err = k.RegisterRemoteKernel(ctx, "@remote.example.com", pubB64, "http://remote.example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Register a public ping action on @sys pointing to the backend.
	a, err := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID:  sys.ID,
		Name:         "ping",
		Kind:         kernel.KindHTTP,
		Source:       backend.URL,
		Price:        0,
		Description:  "ping",
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

	// Signed federation call succeeds; counterparty is identified by public key.
	resp := httpDoWithHeaders(t, srv, "POST",
		"/v1/federation/call?action=@sys/ping&counterparty="+pubB64,
		map[string]any{}, "",
		fedHeaders(t, priv, "@sys/ping", "idem-key-1"))
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

	// Unknown action returns not found (auth passes, action check fails).
	resp2 := httpDoWithHeaders(t, srv, "POST",
		"/v1/federation/call?action=@sys/nope&counterparty="+pubB64,
		map[string]any{}, "",
		fedHeaders(t, priv, "@sys/nope", "idem-key-2"))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown action: expected 404, got %d", resp2.StatusCode)
	}

	// Missing counterparty returns 401.
	resp3 := httpDo(t, srv, "POST", "/v1/federation/call?action=@sys/ping", map[string]any{}, "")
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing counterparty: expected 401, got %d", resp3.StatusCode)
	}

	// Valid counterparty but missing action param returns 422 (action check is after auth).
	resp4 := httpDoWithHeaders(t, srv, "POST",
		"/v1/federation/call?counterparty="+pubB64,
		map[string]any{}, "",
		fedHeaders(t, priv, "", "idem-key-3"))
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("missing action param: expected 422, got %d", resp4.StatusCode)
	}
}

func TestFederationCallRejectsNonPublicAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}

	// Generate a keypair and register a remote peer.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.RegisterRemoteKernel(ctx, "@remote-caller", pubB64, "http://remote-caller.example.com"); err != nil {
		t.Fatal(err)
	}

	// Create a private inactive action owned by @sys.
	a, err := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID:  sys.ID,
		Name:         "secret",
		Kind:         kernel.KindHTTP,
		Source:       "http://127.0.0.1:19871",
		Price:        0,
		Description:  "private",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Private inactive action is rejected with 403 (action check after auth).
	resp := httpDoWithHeaders(t, srv, "POST",
		"/v1/federation/call?action=@sys/secret&counterparty="+pubB64,
		map[string]any{}, "",
		fedHeaders(t, priv, "@sys/secret", "idem-s-1"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("private action: expected 403, got %d", resp.StatusCode)
	}

	// Activate but keep private — still rejected.
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	resp2 := httpDoWithHeaders(t, srv, "POST",
		"/v1/federation/call?action=@sys/secret&counterparty="+pubB64,
		map[string]any{}, "",
		fedHeaders(t, priv, "@sys/secret", "idem-s-2"))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("active but private action: expected 403, got %d", resp2.StatusCode)
	}
}

func TestFederationCallAuth(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.RegisterRemoteKernel(ctx, "@auth-test-remote", pubB64, "http://auth-remote.example.com"); err != nil {
		t.Fatal(err)
	}

	a, err := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID:  sys.ID,
		Name:         "authtest",
		Kind:         kernel.KindHTTP,
		Source:       backend.URL,
		Price:        0,
		Description:  "auth test",
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

	action := "@sys/authtest"
	path := "/v1/federation/call?action=" + action + "&counterparty=" + pubB64

	// No counterparty → 401.
	r1 := httpDo(t, srv, "POST", "/v1/federation/call?action="+action, map[string]any{}, "")
	r1.Body.Close()
	if r1.StatusCode != http.StatusUnauthorized {
		t.Errorf("no counterparty: want 401, got %d", r1.StatusCode)
	}

	// Unknown public key (unregistered counterparty) → 401.
	_, unknownPriv, _ := ed25519.GenerateKey(rand.Reader)
	unknownPub := base64.RawURLEncoding.EncodeToString(unknownPriv.Public().(ed25519.PublicKey))
	r2 := httpDoWithHeaders(t, srv, "POST",
		"/v1/federation/call?action="+action+"&counterparty="+unknownPub,
		map[string]any{}, "",
		fedHeaders(t, unknownPriv, action, "idem-auth-2"))
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown counterparty: want 401, got %d", r2.StatusCode)
	}

	// Missing X-Timestamp → 401.
	r3 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", map[string]string{
		"X-Idempotency-Key": "idem-auth-3",
		"X-Signature":       "invalidsig",
	})
	r3.Body.Close()
	if r3.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing timestamp: want 401, got %d", r3.StatusCode)
	}

	// Expired timestamp → 401.
	oldTS := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	sig, _ := kernel.SignFederationPayload(priv, action, "idem-auth-4", oldTS)
	r4 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", map[string]string{
		"X-Timestamp":       oldTS,
		"X-Idempotency-Key": "idem-auth-4",
		"X-Signature":       sig,
	})
	r4.Body.Close()
	if r4.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired timestamp: want 401, got %d", r4.StatusCode)
	}

	// Missing X-Idempotency-Key → 422 (ErrInvalidInput).
	ts5 := time.Now().UTC().Format(time.RFC3339)
	sig5, _ := kernel.SignFederationPayload(priv, action, "", ts5)
	r5 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", map[string]string{
		"X-Timestamp": ts5,
		"X-Signature": sig5,
	})
	r5.Body.Close()
	if r5.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("missing idempotency key: want 422, got %d", r5.StatusCode)
	}

	// Invalid signature → 401.
	r6 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", map[string]string{
		"X-Timestamp":       time.Now().UTC().Format(time.RFC3339),
		"X-Idempotency-Key": "idem-auth-6",
		"X-Signature":       "badsignature",
	})
	r6.Body.Close()
	if r6.StatusCode != http.StatusUnauthorized {
		t.Errorf("invalid signature: want 401, got %d", r6.StatusCode)
	}

	// Valid auth + idempotency replay: second call with same key returns cached result.
	h1 := fedHeaders(t, priv, action, "idem-replay-1")
	r7 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", h1)
	defer r7.Body.Close()
	if r7.StatusCode != http.StatusOK {
		t.Fatalf("first call: want 200, got %d", r7.StatusCode)
	}
	// Replay with same idempotency key: second call must return the cached result.
	h2 := fedHeaders(t, priv, action, "idem-replay-1")
	r8 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", h2)
	defer r8.Body.Close()
	if r8.StatusCode != http.StatusOK {
		t.Errorf("idempotency replay: want 200, got %d", r8.StatusCode)
	}
}

func TestFederationReplayReceiptNotNil(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"pong":true}`))
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	ctx := context.Background()
	sys, _ := k.ReadUserByHandle(ctx, "@sys")

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, _ = k.RegisterRemoteKernel(ctx, "@replay.example.com", pubB64, "http://replay.example.com")

	a, _ := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID:  sys.ID,
		Name:         "replay-ping",
		Kind:         kernel.KindHTTP,
		Source:       backend.URL,
		Price:        0,
		Description:  "replay ping",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	_ = k.SetActive(ctx, sys.ID, a.ID, true)
	_ = k.GrantAll(ctx, sys.ID, a.ID)

	path := "/v1/federation/call?action=@sys/replay-ping&counterparty=" + pubB64

	// First call: must return a non-nil receipt.
	r1 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", fedHeaders(t, priv, "@sys/replay-ping", "replay-idem-1"))
	defer r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first call: want 200, got %d", r1.StatusCode)
	}
	var env1 map[string]any
	json.NewDecoder(r1.Body).Decode(&env1)
	if env1["receipt"] == nil {
		t.Error("first call: receipt should be non-nil")
	}

	// Replay with same idempotency key: receipt must also be non-nil.
	r2 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", fedHeaders(t, priv, "@sys/replay-ping", "replay-idem-1"))
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("replay: want 200, got %d", r2.StatusCode)
	}
	var env2 map[string]any
	json.NewDecoder(r2.Body).Decode(&env2)
	if env2["receipt"] == nil {
		t.Error("idempotency replay: receipt should be non-nil (was not stored)")
	}
}

func TestHealthCmd(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	t.Setenv("JUICE_URL", srv.URL)
	_, err := runCmd(t, healthCmd(), "--url", srv.URL)
	if err != nil {
		t.Fatalf("health: unexpected error: %v", err)
	}
}

func TestServeImportOpenAPI(t *testing.T) {
	const spec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	specSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(spec))
	}))
	defer specSrv.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@import-srv-owner")

	resp := httpDo(t, srv, "POST", "/v1/actions/import",
		map[string]any{"spec_url": specSrv.URL + "/spec.json"}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import: want 200, got %d", resp.StatusCode)
	}

	var result kernel.ImportResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode import result: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(result.Created))
	}
	if result.Created[0].Name != "/sayHello" {
		t.Errorf("name: got %q, want %q", result.Created[0].Name, "/sayHello")
	}
}

func TestServeUnimportOpenAPI(t *testing.T) {
	const spec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	specSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(spec))
	}))
	defer specSrv.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@unimport-srv-owner")
	specURL := specSrv.URL + "/spec.json"

	// Import first.
	ir := httpDo(t, srv, "POST", "/v1/actions/import", map[string]any{"spec_url": specURL}, tok)
	ir.Body.Close()
	if ir.StatusCode != http.StatusOK {
		t.Fatalf("import: want 200, got %d", ir.StatusCode)
	}

	// Unimport all.
	ur := httpDo(t, srv, "POST", "/v1/actions/unimport", map[string]any{"spec_url": specURL}, tok)
	defer ur.Body.Close()
	if ur.StatusCode != http.StatusOK {
		t.Fatalf("unimport: want 200, got %d", ur.StatusCode)
	}
	var actions []kernel.Action
	if err := json.NewDecoder(ur.Body).Decode(&actions); err != nil {
		t.Fatalf("decode unimport result: %v", err)
	}
	if len(actions) != 1 {
		t.Errorf("expected 1 deactivated action, got %d", len(actions))
	}
}

// TestFederationReplay verifies inbound federation idempotency:
// first call → 200, replay of same key → 200, pending in-flight key → 409.
// Replaces the shell flow that required Python nacl.signing.
func TestFederationReplay(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"greeting": "hello"})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	ctx := context.Background()
	sys, _ := k.ReadUserByHandle(ctx, "@sys")

	a, err := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID:  sys.ID,
		Name:         "fed-greet",
		Kind:         kernel.KindHTTP,
		Price:        0,
		Description:  "greet endpoint",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Source:       backend.URL,
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	_ = k.SetActive(ctx, sys.ID, a.ID, true)
	_ = k.GrantAll(ctx, sys.ID, a.ID)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, _ = k.RegisterRemoteKernel(ctx, "@replay-caller", pubB64, "http://localhost:0")

	path := "/v1/federation/call?action=@sys/fed-greet&counterparty=" + pubB64

	ikey1 := uuid.New().String()
	r1 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", fedHeaders(t, priv, "@sys/fed-greet", ikey1))
	defer r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first call: want 200, got %d", r1.StatusCode)
	}

	// Replay same key → 200.
	r2 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", fedHeaders(t, priv, "@sys/fed-greet", ikey1))
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("replay: want 200, got %d", r2.StatusCode)
	}

	// Pending in-flight key → 409.
	caller, _ := k.ReadUserByHandle(ctx, "@replay-caller")
	ikey2 := uuid.New().String()
	now := time.Now().UTC()
	_ = k.InsertPendingIdempotencyRecord(ctx, &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     ikey2,
		CounterpartyUserID: caller.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(time.Hour),
	})
	r3 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", fedHeaders(t, priv, "@sys/fed-greet", ikey2))
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusConflict {
		t.Errorf("pending: want 409, got %d", r3.StatusCode)
	}
}
