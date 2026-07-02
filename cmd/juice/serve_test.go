package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
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

func newTestHTTPServerFull(t *testing.T) (*httptest.Server, *kernel.Kernel, *store.DB) {
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
	// Credential encryption is mandatory (§8); production wires a box to both the kernel and
	// the HTTP executor, so the test server does too.
	box, err := newAESGCMBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	k := kernel.New(db, nil, &httpActionExecutor{timeout: cfg.ScriptTimeout, secretBox: box}, nil, cfg, logger)
	k.SetSecretBox(box)

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
	k.SetSigningKey(priv, sys.ID)
	if err := k.SetConfig(ctx, configKeySuperuser, "@sys"); err != nil {
		t.Fatal(err)
	}

	srv := &server{kernel: k, log: logger}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestIDMiddleware)

	// Auth and user-creation routes (no rate limiting in tests).
	r.Post("/v1/auth/token", srv.postTokenMulti)
	r.Post("/v1/auth/authorize", srv.postAuthorize)
	r.Post("/v1/auth/refresh", srv.postRefresh)
	r.Post("/v1/auth/logout", srv.postLogout)
	r.Post("/v1/users", srv.postUser)

	registerRoutes(r, srv)

	return httptest.NewServer(r), k, db
}

func newTestHTTPServer(t *testing.T) (*httptest.Server, *kernel.Kernel) {
	t.Helper()
	srv, k, _ := newTestHTTPServerFull(t)
	return srv, k
}

// setupProcessHTTP creates a process+root trace directly via the store for HTTP integration tests.
// Used by tests that need a process_id before making HTTP calls.
func setupProcessHTTP(t *testing.T, db *store.DB, ownerID string, funds int64) *kernel.Process {
	t.Helper()
	ctx := context.Background()
	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	tr := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: ownerID,
		CallerUserID:  ownerID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := db.BeginRun(ctx, p, tr, ownerID, funds); err != nil {
		t.Fatalf("setupProcessHTTP: %v", err)
	}
	return p
}

// setupTraceForProcess returns the root trace ID for a process created by setupProcessHTTP.
func setupTraceForProcess(t *testing.T, db *store.DB, processID string) string {
	t.Helper()
	tr, err := db.ReadRootTrace(context.Background(), processID)
	if err != nil {
		t.Fatalf("setupTraceForProcess: %v", err)
	}
	return tr.ID
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
	if _, err := k.Deposit(ctx, sys.ID, userID, amount, "test", ""); err != nil {
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
// All test federation calls use an empty JSON body (json.Encoder output of {}).
func fedHeaders(t *testing.T, priv ed25519.PrivateKey, action, idempKey string) map[string]string {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339)
	counterparty := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	// json.NewEncoder appends a newline; match what httpDoWithHeaders sends.
	var bodyBuf bytes.Buffer
	json.NewEncoder(&bodyBuf).Encode(map[string]any{})
	argsHash := sha256HexBytes(bodyBuf.Bytes())
	sig, err := kernel.SignFederationPayload(priv, action, counterparty, idempKey, ts, argsHash)
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

	// Authenticated owner sees their own active private action.
	resp := httpDo(t, srv, "GET", "/v1/actions", nil, tok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list actions: expected 200, got %d", resp.StatusCode)
	}
	var actions []kernel.Action
	decodeResponse(t, resp, &actions)
	if len(actions) != 1 {
		t.Fatalf("owner should see own active private action, got %d", len(actions))
	}

	// Unauthenticated caller does not see the private action.
	resp2 := httpDo(t, srv, "GET", "/v1/actions", nil, "")
	if resp2.StatusCode != http.StatusOK {
		resp2.Body.Close()
		t.Fatalf("unauthenticated list actions: expected 200, got %d", resp2.StatusCode)
	}
	var actions2 []kernel.Action
	decodeResponse(t, resp2, &actions2)
	if len(actions2) != 0 {
		t.Fatal("private action should not appear in unauthenticated list")
	}

	httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{"public": true}, tok).Body.Close()
	resp3 := httpDo(t, srv, "GET", "/v1/actions", nil, tok)
	if resp3.StatusCode != http.StatusOK {
		resp3.Body.Close()
		t.Fatalf("list actions after grant-all: expected 200, got %d", resp3.StatusCode)
	}
	var actions3 []kernel.Action
	decodeResponse(t, resp3, &actions3)
	if len(actions3) == 0 {
		t.Error("expected public action in list after grant-all")
	}
}

// TestServeListActionsExcludesSuspendedOwner proves GET /v1/actions hides a suspended
// owner's active public action (§12 hide+disable).
func TestServeListActionsExcludesSuspendedOwner(t *testing.T) {
	srv, k, st := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, tok := makeUser(t, k, "@susp-owner")
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "svc", "kind": "http", "price": 0, "source": "http://x.example",
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, tok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{"public": true}, tok).Body.Close()

	var before []kernel.Action
	decodeResponse(t, httpDo(t, srv, "GET", "/v1/actions", nil, ""), &before)
	if len(before) != 1 {
		t.Fatalf("want 1 public action before suspension, got %d", len(before))
	}

	if err := st.SuspendUser(context.Background(), ownerID); err != nil {
		t.Fatal(err)
	}
	var after []kernel.Action
	decodeResponse(t, httpDo(t, srv, "GET", "/v1/actions", nil, ""), &after)
	if len(after) != 0 {
		t.Fatalf("suspended owner's action should be hidden, got %d", len(after))
	}
}

// TestServeCreateWasmActionFromArtifact verifies POST /v1/actions accepts a pre-compiled
// base64 WASM artifact via "wasm_artifact" — CLI/HTTP parity with
// `action create --kind wasm --artifact`. It stores the artifact, computes the hash, and
// the artifact-only action (empty source) activates over HTTP. Uses newFlowKernel so the
// kernel has a ScriptExecutor that hashes the artifact (newTestHTTPServer wires none).
func TestServeCreateWasmActionFromArtifact(t *testing.T) {
	srv, k, _ := newFlowKernel(t, &flowScriptExec{})
	defer srv.Close()

	ownerID, tok := makeUser(t, k, "@artifact-owner")

	// A base64 artifact with no source — mirrors @sys/tinygo/compile output.
	b64 := base64.StdEncoding.EncodeToString([]byte("fake-wasm-artifact-bytes"))
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "from-artifact", "kind": "wasm", "price": 0,
		"wasm_artifact": b64,
		"description":   "registered from a precompiled artifact",
		"input_schema":  minSchema, "output_schema": minSchema,
	}, tok)
	if cr.StatusCode != http.StatusCreated {
		cr.Body.Close()
		t.Fatalf("create wasm action from artifact: expected 201, got %d", cr.StatusCode)
	}
	var action kernel.Action
	decodeResponse(t, cr, &action)
	if action.Kind != kernel.KindWasm {
		t.Errorf("kind = %q, want wasm", action.Kind)
	}
	if action.WasmArtifact != b64 {
		t.Errorf("wasm_artifact not stored: got %q, want the posted base64", action.WasmArtifact)
	}
	if action.ArtifactHash == "" {
		t.Error("ArtifactHash should be computed from the wasm_artifact bytes")
	}

	// The artifact-only action (empty source) must activate over HTTP.
	en := httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, tok)
	if en.StatusCode != http.StatusOK {
		en.Body.Close()
		t.Fatalf("enable artifact-only wasm action: expected 200, got %d", en.StatusCode)
	}
	en.Body.Close()

	got, err := k.ReadActionByOwnerName(context.Background(), ownerID, "from-artifact")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Active {
		t.Error("artifact-only wasm action should be active after enable")
	}
}

// TestMaxBytesMiddlewareActionRoute verifies the path-aware body limit: a body over the
// 1 MiB default is accepted on the action create route (it may carry a WASM artifact) but
// rejected on an ordinary route.
func TestMaxBytesMiddlewareActionRoute(t *testing.T) {
	h := maxBytesMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	big := bytes.Repeat([]byte("a"), 4<<20) // 4 MiB — over the 1 MiB default, under 16 MiB

	cases := []struct {
		name, method, path string
		want               int
	}{
		{"action create accepts large body", http.MethodPost, "/v1/actions", http.StatusOK},
		{"action update accepts large body", http.MethodPut, "/v1/actions/abc-123", http.StatusOK},
		{"run rejects large body", http.MethodPost, "/v1/run", http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, bytes.NewReader(big))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("%s %s with 4 MiB body: got %d, want %d", c.method, c.path, rec.Code, c.want)
			}
		})
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


func TestServeProcessLifecycle(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	userID, tok := makeUser(t, k, "@srv-proc")
	p := setupProcessHTTP(t, db, userID, 0)
	pid := p.ID

	// Get process.
	get := httpDo(t, srv, "GET", "/v1/processes/"+pid, nil, tok)
	if get.StatusCode != http.StatusOK {
		get.Body.Close()
		t.Fatalf("get process: expected 200, got %d", get.StatusCode)
	}
	var proc kernel.Process
	decodeResponse(t, get, &proc)
	if proc.ID != pid {
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
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@proc-owner")
	_, otherTok := makeUser(t, k, "@proc-other")
	_ = ownerTok

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID

	// Non-owner must get 403.
	get := httpDo(t, srv, "GET", "/v1/processes/"+pid, nil, otherTok)
	defer get.Body.Close()
	if get.StatusCode != http.StatusForbidden {
		t.Errorf("non-owner get process: expected 403, got %d", get.StatusCode)
	}
}

func TestServeFundProcess(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	userID, tok := makeUser(t, k, "@fund-user")
	giveCredits(t, k, userID, 200)

	// Create process with initial funds via store (process creation is internal in new model).
	p := setupProcessHTTP(t, db, userID, 100)

	// Verify via GET /v1/processes/{id} that the process has funds.
	get := httpDo(t, srv, "GET", "/v1/processes/"+p.ID, nil, tok)
	if get.StatusCode != http.StatusOK {
		get.Body.Close()
		t.Fatalf("get process: expected 200, got %d", get.StatusCode)
	}
	var proc kernel.Process
	decodeResponse(t, get, &proc)
	// With BeginRun, process.available is always 0 — funds are held in the root trace.
	if proc.Available != 0 {
		t.Errorf("funded process: expected 0 available (funds in root trace), got %d", proc.Available)
	}
	if proc.Status != kernel.ProcessOpen {
		t.Errorf("funded process: expected status=open, got %s", proc.Status)
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
	httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{"public": true}, ownerTok).Body.Close()

	// Make the call via /v1/run (new API — price=0, caller needs no credits).
	callResp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@call-owner/answer",
		"args":   map[string]any{},
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

func TestServeRunRejectsAbsentArgs(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@run-args-user")

	// Absent args field must be rejected (ErrInvalidInput = 422), not silently treated as {}.
	resp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@run-args-user/nonexistent",
	}, tok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for absent args, got %d", resp.StatusCode)
	}
}

func TestServeRunRejectsEmptyAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@run-action-user")

	// Present args={} with absent action must be rejected (ErrInvalidInput = 422).
	resp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"args": map[string]any{},
	}, tok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for missing action, got %d", resp.StatusCode)
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
	httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{"public": true}, ownerTok).Body.Close()

	call := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@tx-owner/tx-action", "args": map[string]any{},
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
	httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{"public": true}, ownerTok).Body.Close()

	call := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@rate-owner/rate-action", "args": map[string]any{},
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
	httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{"public": true}, ownerTok).Body.Close()

	call := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@list-ratings-owner/list-ratings-action", "args": map[string]any{},
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
	httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{"public": true}, ownerTok).Body.Close()

	// Make one call to generate stats.
	httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@stats-owner/stats-action", "args": map[string]any{},
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
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	userID, tok := makeUser(t, k, "@lp-user")

	// Create two processes directly via the store (POST /v1/processes no longer exists).
	setupProcessHTTP(t, db, userID, 0)
	setupProcessHTTP(t, db, userID, 0)

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
	k := kernel.New(db, nil, nil, nil, cfg, logger)
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

func TestPutMe(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "@putmetest")

	// Update email only.
	resp := httpDo(t, srv, "PUT", "/v1/me", map[string]any{"email": "new@example.com"}, tok)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("update email: want 200, got %d: %s", resp.StatusCode, body)
	}
	var got map[string]any
	decodeResponse(t, resp, &got)
	if got["email"] != "new@example.com" {
		t.Errorf("email in response: got %v, want new@example.com", got["email"])
	}

	// Confirm via GET /v1/me.
	resp2 := httpDo(t, srv, "GET", "/v1/me", nil, tok)
	var me map[string]any
	decodeResponse(t, resp2, &me)
	if me["email"] != "new@example.com" {
		t.Errorf("GET /v1/me email: got %v, want new@example.com", me["email"])
	}

	// Change password with correct current password.
	resp3 := httpDo(t, srv, "PUT", "/v1/me", map[string]any{
		"current_password": "pass",
		"password":         "newpass",
	}, tok)
	if resp3.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp3.Body)
		resp3.Body.Close()
		t.Fatalf("change password: want 200, got %d: %s", resp3.StatusCode, body)
	}
	resp3.Body.Close()

	// Old password login must fail; new password must succeed.
	oldLogin := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"grant_type": "password", "handle": "@putmetest", "password": "pass",
	}, "")
	if oldLogin.StatusCode != http.StatusUnauthorized {
		t.Errorf("old password: want 401, got %d", oldLogin.StatusCode)
	}
	oldLogin.Body.Close()

	newLogin := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"grant_type": "password", "handle": "@putmetest", "password": "newpass",
	}, "")
	if newLogin.StatusCode != http.StatusOK {
		t.Errorf("new password: want 200, got %d", newLogin.StatusCode)
	}
	newLogin.Body.Close()

	// Wrong current password returns 401.
	resp4 := httpDo(t, srv, "PUT", "/v1/me", map[string]any{
		"current_password": "wrong",
		"password":         "other",
	}, tok)
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong current password: want 401, got %d", resp4.StatusCode)
	}

	// No fields returns 422.
	resp5 := httpDo(t, srv, "PUT", "/v1/me", map[string]any{}, tok)
	resp5.Body.Close()
	if resp5.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no fields: want 422, got %d", resp5.StatusCode)
	}

	// Unauthenticated returns 401.
	resp6 := httpDo(t, srv, "PUT", "/v1/me", map[string]any{"email": "x@x.com"}, "")
	resp6.Body.Close()
	if resp6.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated: want 401, got %d", resp6.StatusCode)
	}
}

func TestGetActionReadPermission(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@ra-owner")
	_, strangerTok := makeUser(t, k, "@ra-stranger")
	_ = k

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "ra-action", "kind": "http", "price": 0, "source": "http://example.com",
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)

	// Owner can read their own private action.
	r1 := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, ownerTok)
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Errorf("owner: expected 200, got %d", r1.StatusCode)
	}

	// Stranger cannot read a private action.
	r2 := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, strangerTok)
	r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden {
		t.Errorf("stranger: expected 403, got %d", r2.StatusCode)
	}

	// Making the action public allows anyone to read it.
	httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{"public": true}, ownerTok).Body.Close()
	r3 := httpDo(t, srv, "GET", "/v1/actions/"+action.ID, nil, strangerTok)
	r3.Body.Close()
	if r3.StatusCode != http.StatusOK {
		t.Errorf("stranger on public action: expected 200, got %d", r3.StatusCode)
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
	_, err = k.AddPeer(ctx, sys.ID, "@remote.example.com", pubB64, "http://remote.example.com")
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
	pubAll := true
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubAll}); err != nil {
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
	if _, err := k.AddPeer(ctx, sys.ID, "@remote-caller", pubB64, "http://remote-caller.example.com"); err != nil {
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
	if _, err := k.AddPeer(ctx, sys.ID, "@auth-test-remote", pubB64, "http://auth-remote.example.com"); err != nil {
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
	pubAll := true
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubAll}); err != nil {
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
	cpKey := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	var authBuf bytes.Buffer
	json.NewEncoder(&authBuf).Encode(map[string]any{})
	authArgsHash := sha256HexBytes(authBuf.Bytes())
	sig, _ := kernel.SignFederationPayload(priv, action, cpKey, "idem-auth-4", oldTS, authArgsHash)
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
	sig5, _ := kernel.SignFederationPayload(priv, action, cpKey, "", ts5, authArgsHash)
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
	_, _ = k.AddPeer(ctx, sys.ID, "@replay.example.com", pubB64, "http://replay.example.com")

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
	pubAll := true
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubAll})

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
	_, err := execTestCmd(t, healthCmd(), "--url", srv.URL)
	if err != nil {
		t.Fatalf("health: unexpected error: %v", err)
	}
}

func TestServeImportOpenAPI(t *testing.T) {
	const spec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
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
	if result.Created[0].Name != "sayHello" {
		t.Errorf("name: got %q, want %q", result.Created[0].Name, "sayHello")
	}
}

func TestServeUnimportOpenAPI(t *testing.T) {
	const spec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
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
	pubAll := true
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubAll})

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, _ = k.AddPeer(ctx, sys.ID, "@replay-caller", pubB64, "http://localhost:0")

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

// TestFederationIdempotencyPreconditionFailure verifies that a schema-validation failure
// (precondition 7) completes the idempotency record so replays return the error, not 409.
func TestFederationIdempotencyPreconditionFailure(t *testing.T) {
	// Backend won't be reached (schema validation fails first), but needs to exist for activation.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	ctx := context.Background()

	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, _ = k.AddPeer(ctx, sys.ID, "@schema-fail-peer", pubB64, "http://schema-fail.example.com")

	// Action requires a "name" field; empty body {} will fail schema validation.
	a, err := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID: sys.ID, Name: "strict", Kind: kernel.KindHTTP,
		Source: backend.URL, Description: "strict schema action",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string", "description": "The name"}},
			"required":   []string{"name"},
		},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	pubAll := true
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubAll}); err != nil {
		t.Fatal(err)
	}

	path := "/v1/federation/call?action=@sys/strict&counterparty=" + pubB64
	ikey := uuid.New().String()
	// fedHeaders signs an empty body {}; strict schema requires "name" → schema error.
	r1 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", fedHeaders(t, priv, "@sys/strict", ikey))
	defer r1.Body.Close()
	if r1.StatusCode == http.StatusConflict {
		t.Fatal("first call returned 409: idempotency record was not created")
	}
	// Must be a client error (schema violation → 422).
	if r1.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("schema failure: want 422, got %d", r1.StatusCode)
	}

	// Replay same key → must return the same error, not 409.
	r2 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", fedHeaders(t, priv, "@sys/strict", ikey))
	defer r2.Body.Close()
	if r2.StatusCode == http.StatusConflict {
		t.Errorf("replay after precondition failure: got 409 (record still pending), want error response")
	}
	if r2.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("replay: want 422, got %d", r2.StatusCode)
	}
}

// TestFederationIdempotencyCommittedFailureHasReceipt verifies that replaying a key
// whose call was committed as a failure returns a non-nil receipt (Bug 1b fix).
func TestFederationIdempotencyCommittedFailureHasReceipt(t *testing.T) {
	// Backend always returns 500 → execution failure → CommitFailedCall.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	ctx := context.Background()

	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, _ = k.AddPeer(ctx, sys.ID, "@committed-fail-peer", pubB64, "http://committed-fail.example.com")

	a, _ := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID: sys.ID, Name: "fail-exec", Kind: kernel.KindHTTP,
		Source: backend.URL, Description: "always-failing action",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	k.SetActive(ctx, sys.ID, a.ID, true)
	pubAll := true
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubAll})

	path := "/v1/federation/call?action=@sys/fail-exec&counterparty=" + pubB64
	ikey := uuid.New().String()
	r1 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", fedHeaders(t, priv, "@sys/fail-exec", ikey))
	defer r1.Body.Close()
	if r1.StatusCode == http.StatusConflict {
		t.Fatal("first call returned 409")
	}

	// Replay same key: must return error (not 409) and a non-nil receipt.
	r2 := httpDoWithHeaders(t, srv, "POST", path, map[string]any{}, "", fedHeaders(t, priv, "@sys/fail-exec", ikey))
	if r2.StatusCode == http.StatusConflict {
		t.Errorf("committed failure replay: got 409, want error response")
	}
	var body struct {
		Receipt *kernel.Receipt `json:"receipt"`
	}
	decodeResponse(t, r2, &body)
	if body.Receipt == nil {
		t.Error("committed failure replay: receipt is nil, want non-nil")
	}
}

// TestFederationCallRejectsArgsHashMismatch verifies that a federation call
// whose body has been tampered with (args_hash no longer matches) is rejected.
func TestFederationCallRejectsArgsHashMismatch(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	ctx := context.Background()

	sys, _ := k.ReadUserByHandle(ctx, "@sys")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, err := k.AddPeer(ctx, sys.ID, "@args-hash-peer", pubB64, "http://argshash.example.com")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID: sys.ID, Name: "hash-check", Kind: kernel.KindHTTP,
		Source: backend.URL, Description: "hash-check",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	})
	k.SetActive(ctx, sys.ID, a.ID, true)
	pubAll := true
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubAll})

	path := "/v1/federation/call?action=@sys/hash-check&counterparty=" + pubB64
	// Sign with empty body {} but send a different body — args_hash mismatch.
	hdrs := fedHeaders(t, priv, "@sys/hash-check", "idem-hash-1")
	// Send a body different from what was signed.
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]any{"injected": true})
	req, _ := http.NewRequest("POST", srv.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("args_hash mismatch: want 401, got %d", resp.StatusCode)
	}
}

// TestReceiptVerificationEndpoint tests GET /v1/transactions/{id}/receipt-verification.
func TestReceiptVerificationEndpoint(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	ctx := context.Background()
	sys, _ := k.ReadUserByHandle(ctx, "@sys")

	userID, tok := makeUser(t, k, "@vrr-http-user")
	giveCredits(t, k, userID, 100)

	// Create a local HTTP action and make a call via /v1/run to get a transaction.
	a, _ := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID: sys.ID, Name: "vrr-http", Kind: kernel.KindHTTP,
		Source: backend.URL, Description: "vrr-http",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	})
	k.SetActive(ctx, sys.ID, a.ID, true)
	pubAll := true
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubAll})

	callResp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@sys/vrr-http", "args": map[string]any{},
	}, tok)
	if callResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(callResp.Body)
		callResp.Body.Close()
		t.Fatalf("call: want 200, got %d: %s", callResp.StatusCode, body)
	}
	var callResult kernel.CallReply
	decodeResponse(t, callResp, &callResult)
	txID := callResult.TxID
	if txID == "" {
		t.Fatal("call returned empty tx_id")
	}

	// Non-remote-proxy transaction → 409 ErrInvalidState.
	resp := httpDo(t, srv, "GET", "/v1/transactions/"+txID+"/receipt-verification", nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("non-remote-proxy: want 409, got %d", resp.StatusCode)
	}

	// Unknown transaction → 404.
	resp2 := httpDo(t, srv, "GET", "/v1/transactions/"+uuid.New().String()+"/receipt-verification", nil, tok)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown tx: want 404, got %d", resp2.StatusCode)
	}
}

func TestServeListActionsOwnerAuth(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@la-auth-owner")

	// Create a private (inactive, non-public) action.
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "la-auth-priv", "kind": "http", "price": 0, "source": "http://x.example",
		"description": "private test action", "input_schema": minSchema, "output_schema": minSchema,
	}, ownerTok)
	var created map[string]any
	decodeResponse(t, cr, &created)
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create action: expected 201, got %d", cr.StatusCode)
	}

	// Without token: owner's private action not visible.
	resp := httpDo(t, srv, "GET", "/v1/actions?owner=@la-auth-owner", nil, "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("unauthenticated list: expected 200, got %d", resp.StatusCode)
	}
	var noAuth []map[string]any
	decodeResponse(t, resp, &noAuth)
	if len(noAuth) != 0 {
		t.Errorf("unauthenticated: expected 0 results, got %d", len(noAuth))
	}

	// With owner token: private action is visible.
	resp2 := httpDo(t, srv, "GET", "/v1/actions?owner=@la-auth-owner", nil, ownerTok)
	if resp2.StatusCode != http.StatusOK {
		resp2.Body.Close()
		t.Fatalf("authenticated list: expected 200, got %d", resp2.StatusCode)
	}
	var withAuth []map[string]any
	decodeResponse(t, resp2, &withAuth)
	if len(withAuth) == 0 {
		t.Error("authenticated owner: expected private action to appear")
	}
}

func TestServeWriteErrHasCode(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	// GET a nonexistent action — should return 404 with both "error" and "code" fields.
	resp := httpDo(t, srv, "GET", "/v1/actions/"+uuid.New().String(), nil, "sys-token-placeholder")
	defer resp.Body.Close()
	// The token is invalid so we expect 401, but any error response has both fields.
	var body map[string]any
	decodeResponse(t, resp, &body)
	if _, ok := body["error"]; !ok {
		t.Error("error response missing 'error' field")
	}
	if _, ok := body["code"]; !ok {
		t.Error("error response missing 'code' field")
	}
}

// TestServeAdvertisesBoundAddr covers the --addr :0 path in runServer: the real port is
// known only after net.Listen binds, so runServer advertises it as kernel_base_url and
// getWellKnown reports it (federation under :0 depends on this — see serve.go runServer).
func TestServeAdvertisesBoundAddr(t *testing.T) {
	_, k := newTestHTTPServer(t)
	ctx := context.Background()

	// --addr 127.0.0.1:0 → an OS-assigned port, unknown until the bind succeeds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if p := ln.Addr().(*net.TCPAddr).Port; p == 0 {
		t.Fatalf("expected a real bound port, got :0")
	}
	want := "http://" + ln.Addr().String()
	if err := k.SetConfig(ctx, "kernel_base_url", want); err != nil {
		t.Fatal(err)
	}

	srv := &server{kernel: k, log: log.Discard()}
	rec := httptest.NewRecorder()
	srv.getWellKnown(rec, httptest.NewRequest("GET", "/.well-known/juice-kernel.json", nil))

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode well-known: %v", err)
	}
	if body["base_url"] != want {
		t.Errorf("base_url = %q, want %q", body["base_url"], want)
	}
}

// TestSuperuserScopeOverTCP proves supervision is scope on the normal TCP endpoints: over the
// public API a @sys token sees another user's private action and process and may disable any
// action, while a normal caller stays own-scoped. This is what replaced admin actions/
// processes/disable (no separate admin surface).
func TestSuperuserScopeOverTCP(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	ctx := context.Background()

	sysTok, err := k.Login(ctx, "@sys", "sys-pass")
	if err != nil {
		t.Fatal(err)
	}
	aliceID, aliceTok := makeUser(t, k, "@alice")

	// @alice creates a private, inactive action.
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "secret", "kind": "http", "price": 0, "source": "http://127.0.0.1:1/x",
		"description": "private", "input_schema": minSchema, "output_schema": minSchema,
	}, aliceTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)

	// Anonymous listing of @alice's actions excludes the private one; @sys sees it.
	anon := decodeActions(t, httpDo(t, srv, "GET", "/v1/actions?owner=@alice", nil, ""))
	if len(anon) != 0 {
		t.Errorf("anonymous should see 0 of @alice's actions, got %d", len(anon))
	}
	asSys := decodeActions(t, httpDo(t, srv, "GET", "/v1/actions?owner=@alice", nil, sysTok))
	if len(asSys) != 1 {
		t.Errorf("@sys should see @alice's private action, got %d", len(asSys))
	}

	// @alice owns a process; @sys sees it in the system-wide process list, a stranger doesn't.
	giveCredits(t, k, aliceID, 100)
	proc := setupProcessHTTP(t, db, aliceID, 100)
	if !containsProcess(t, httpDo(t, srv, "GET", "/v1/processes", nil, sysTok), proc.ID) {
		t.Error("@sys process list should include @alice's process")
	}
	_, bobTok := makeUser(t, k, "@bob")
	if containsProcess(t, httpDo(t, srv, "GET", "/v1/processes", nil, bobTok), proc.ID) {
		t.Error("@bob must not see @alice's process")
	}

	// @sys may disable @alice's action over TCP (owner-or-superuser); @bob may not.
	if resp := httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/disable", nil, bobTok); resp.StatusCode < 400 {
		t.Errorf("@bob disabling @alice's action should fail, got %d", resp.StatusCode)
	}
	if resp := httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/disable", nil, sysTok); resp.StatusCode >= 400 {
		t.Errorf("@sys disabling @alice's action should succeed, got %d", resp.StatusCode)
	}
}

func decodeActions(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode actions: %v", err)
	}
	return out
}

func containsProcess(t *testing.T, resp *http.Response, id string) bool {
	t.Helper()
	defer resp.Body.Close()
	var procs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&procs); err != nil {
		t.Fatalf("decode processes: %v", err)
	}
	for _, p := range procs {
		if p["id"] == id {
			return true
		}
	}
	return false
}
