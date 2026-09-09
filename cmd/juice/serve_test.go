package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/fed"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

func newTestHTTPServerFull(t *testing.T) (*httptest.Server, *kernel.Kernel, *store.DB) {
	t.Helper()
	return newFlowKernel(t, nil)
}

// bootstrapSigning installs the signing key and superuser config on an already-FirstBooted
// kernel, returning the platform Ed25519 private key. Shared by the test server builders.
func bootstrapSigning(t *testing.T, k *kernel.Kernel) ed25519.PrivateKey {
	t.Helper()
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "sys")
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
	if err := k.SetConfig(ctx, configKeySuperuser, "sys"); err != nil {
		t.Fatal(err)
	}
	return priv
}

// mountFullRouter builds the chi router used by the full test servers: the unauthenticated
// auth/user-creation routes (no rate limiting in tests) plus all registered routes.
func mountFullRouter(srv *server) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestIDMiddleware)
	r.Post("/v1/auth/token", srv.postToken)
	r.Post("/v1/auth/authorize", srv.postAuthorize)
	r.Post("/v1/auth/refresh", srv.postRefresh)
	r.Post("/v1/auth/logout", srv.postLogout)
	r.Post("/v1/users", srv.postUser)
	registerRoutes(r, srv)
	return r
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
	if err := db.BeginRun(ctx, p, tr, ownerID, funds, 0, 0); err != nil {
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
		Handle: handle, Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := loginTokenFor(k, context.Background(), handle, "pass")
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
	sys, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatalf("giveCredits: @sys not found: %v", err)
	}
	if _, err := k.Deposit(ctx, sys.ID, userID, amount, "test", newRef()); err != nil {
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
	resp, err := srv.Client().Do(req)
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
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// fedHeaders builds signed auth headers for a federation call.
// All test federation calls use an empty JSON body (json.Encoder output of {}).
// fedCall exercises the inbound federation path directly — the same handleFederationCall the
// /juice/fed/call/1 transport handler invokes — and returns a synthetic *http.Response so the
// existing status/body assertions carry over. The signature covers the exact args bytes, so the
// receiver's args_hash matches (the same contract the transport preserves). `action` is the
// serving kernel's stable action id, exactly as the wire carries it (§13).
func fedCall(t *testing.T, k *kernel.Kernel, priv ed25519.PrivateKey, action, idempKey string, args map[string]any) *http.Response {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	body, _ := json.Marshal(args)
	ts := time.Now().UTC().Format(time.RFC3339)
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	argsHash := sha256HexBytes(body)
	// recipient is the serving kernel's own key; empty contract hash skips the §8 If-Match check.
	ownKey, _ := k.GetConfig(context.Background(), configKeySigningPublic)
	sig, err := testNet.SignFederationPayload(priv, action, cp, ownKey, "", idempKey, ts, argsHash, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return fedCallRaw(t, k, cp, ts, idempKey, action, sig, body)
}

// fedCallRaw calls handleFederationCall with fully explicit inputs (for edge cases like a
// missing counterparty or a tampered body) and wraps the result as an *http.Response.
func fedCallRaw(t *testing.T, k *kernel.Kernel, cp, ts, idempKey, action, sig string, body []byte) *http.Response {
	t.Helper()
	status, respBody, callErr := handleFederationCall(k, context.Background(), cp, "", ts, idempKey, action, sig, kernel.BuyerTerms{}, body)
	if callErr != nil {
		status = kernel.HTTPStatusFromCode(kernel.KernelErrorCode(callErr))
		respBody = map[string]any{"error": callErr.Error()}
	}
	b, _ := json.Marshal(respBody)
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(b))}
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

	saved := globalCfg.KernelHandle
	t.Cleanup(func() { globalCfg.KernelHandle = saved })
	globalCfg.KernelHandle = "kernel-test"

	resp := httpDo(t, srv, "GET", "/health", nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	// Unauthenticated health doubles as an identity banner: status plus the kernel's advertised
	// federation identity (handle + public key), so a client can see which kernel it's on.
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
	if body["handle"] != "kernel-test" {
		t.Errorf("handle = %q, want @kernel-test", body["handle"])
	}
	if body["public_key"] == "" {
		t.Error("public_key should be present in the health banner")
	}
}

func TestServeCreateUser(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	resp := httpDo(t, srv, "POST", "/v1/users", map[string]any{
		"handle": "http-alice", "email": "alice@example.com", "password": "testpass",
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
	if body["handle"] != "http-alice" {
		t.Errorf("response handle: got %v", body["handle"])
	}
}

// httpLogin drives the canonical authorize→exchange flow over HTTP (§12, §14) and returns the
// status a caller should assert plus the access token. Credential rejection surfaces at the
// authorize leg, so a bad password yields that leg's status and an empty token.
func httpLogin(t *testing.T, srv *httptest.Server, handle, password string) (int, string) {
	t.Helper()
	verifier := strings.Repeat("v", 43)
	h := sha256.Sum256([]byte(verifier))
	authResp := httpDo(t, srv, "POST", "/v1/auth/authorize", map[string]any{
		"handle": handle, "password": password,
		"code_challenge": base64.RawURLEncoding.EncodeToString(h[:]),
	}, "")
	if authResp.StatusCode != http.StatusOK {
		defer authResp.Body.Close()
		return authResp.StatusCode, ""
	}
	var auth map[string]string
	decodeResponse(t, authResp, &auth)
	tokResp := httpDo(t, srv, "POST", "/v1/auth/token", map[string]any{
		"code": strings.TrimPrefix(auth["redirect"], "?code="), "code_verifier": verifier,
	}, "")
	if tokResp.StatusCode != http.StatusOK {
		defer tokResp.Body.Close()
		return tokResp.StatusCode, ""
	}
	var tokens map[string]string
	decodeResponse(t, tokResp, &tokens)
	return http.StatusOK, tokens["access_token"]
}

func TestServeAuthToken(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, err := k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "http-bob", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	status, tok := httpLogin(t, srv, "http-bob", "pass")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if tok == "" {
		t.Error("expected non-empty access token")
	}
}

func TestServePKCEFlow(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, _ = makeUser(t, k, "pkce-user")

	verifier := strings.Repeat("x", 43)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	// Step 1: authorize.
	resp := httpDo(t, srv, "POST", "/v1/auth/authorize", map[string]any{
		"handle":         "pkce-user",
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

	_, tok := makeUser(t, k, "srv-actowner")

	resp := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "http-action", "kind": "http",
		"price": 0, "source": "http://example.com",
	}, tok)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create action: expected 201, got %d", resp.StatusCode)
	}
	var action struct {
		kernel.Action
		ActionRef string `json:"action"`
	}
	decodeResponse(t, resp, &action)
	if action.ID == "" {
		t.Error("expected action with ID")
	}
	// The owner is identified by @handle (owner_handle / action=@owner/name), never the raw UUID.
	if action.OwnerHandle != "srv-actowner" || action.ActionRef != "srv-actowner/"+action.Name {
		t.Errorf("action owner: got handle=%q ref=%q, want @srv-actowner", action.OwnerHandle, action.ActionRef)
	}
	if action.OwnerUserID != "" {
		t.Error("action response should not expose owner_user_id")
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

	_, tok := makeUser(t, k, "list-owner")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "list-me", "kind": "http", "price": 0, "source": "http://x.example",
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, tok).Body.Close()

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

	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID, "visibility": "public"}, tok).Body.Close()
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

// TestServeListPagination proves the limit/offset query params are honored across the
// list surface (previously getActions/listSteps discarded them).
func TestServeListPagination(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "page-owner")

	for i := 0; i < 3; i++ {
		cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
			"name": fmt.Sprintf("page-%d", i), "kind": "http", "price": 0,
			"source": "http://x.example", "description": "test action",
			"input_schema": minSchema, "output_schema": minSchema, "visibility": "public",
		}, tok)
		var a kernel.Action
		decodeResponse(t, cr, &a)
		httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": a.ID}, tok).Body.Close()
	}

	listLen := func(query string) int {
		resp := httpDo(t, srv, "GET", "/v1/actions"+query, nil, tok)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("GET /v1/actions%s: expected 200, got %d", query, resp.StatusCode)
		}
		var out []kernel.Action
		decodeResponse(t, resp, &out)
		return len(out)
	}

	if n := listLen("?limit=2"); n != 2 {
		t.Fatalf("limit=2: want 2 actions, got %d", n)
	}
	if n := listLen("?limit=2&offset=2"); n != 1 {
		t.Fatalf("limit=2&offset=2: want 1 action, got %d", n)
	}
	// A ceiling-exceeding limit is clamped, not rejected; all three still return.
	if n := listLen("?limit=100000"); n != 3 {
		t.Fatalf("limit clamp: want 3 actions, got %d", n)
	}
}

// TestServeListActionsExcludesSuspendedOwner proves GET /v1/actions hides a suspended
// owner's active public action (§12 hide+disable).
func TestServeListActionsExcludesSuspendedOwner(t *testing.T) {
	srv, k, st := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, tok := makeUser(t, k, "susp-owner")
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "svc", "kind": "http", "price": 0, "source": "http://x.example",
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, tok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID, "visibility": "public"}, tok).Body.Close()

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

	ownerID, tok := makeUser(t, k, "artifact-owner")

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
	en := httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, tok)
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
		{"action update accepts large body", http.MethodPut, "/v1/actions", http.StatusOK},
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

	_, tok := makeUser(t, k, "toggle-owner")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "toggle-me", "kind": "http", "price": 0, "source": "http://x.example",
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)

	// Disable.
	r1 := httpDo(t, srv, "POST", "/v1/actions/disable", map[string]any{"target": action.ID}, tok)
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
	r2 := httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, tok)
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

	_, tok := makeUser(t, k, "del-owner")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "delete-me", "kind": "http", "price": 0, "source": "http://x.example",
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)

	del := httpDo(t, srv, "DELETE", "/v1/actions?target="+action.ID, nil, tok)
	defer del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", del.StatusCode)
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

	userID, tok := makeUser(t, k, "srv-proc")
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

	ownerID, ownerTok := makeUser(t, k, "proc-owner")
	_, otherTok := makeUser(t, k, "proc-other")
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

	userID, tok := makeUser(t, k, "fund-user")
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

	_, ownerTok := makeUser(t, k, "call-owner")
	_, callerTok := makeUser(t, k, "call-caller")

	// Create and activate a free public HTTP action.
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "answer", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID, "visibility": "public"}, ownerTok).Body.Close()

	// Make the call via /v1/run (new API — price=0, caller needs no credits).
	callResp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "call-owner/answer",
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

	_, tok := makeUser(t, k, "run-args-user")

	// Absent args field must be rejected (ErrInvalidInput = 422), not silently treated as {}.
	resp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "run-args-user/nonexistent",
	}, tok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for absent args, got %d", resp.StatusCode)
	}
}

func TestServeRunRejectsEmptyAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "run-action-user")

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

	_, ownerTok := makeUser(t, k, "tx-owner")
	_, callerTok := makeUser(t, k, "tx-caller")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "tx-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID, "visibility": "public"}, ownerTok).Body.Close()

	call := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "tx-owner/tx-action", "args": map[string]any{},
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

	_, ownerTok := makeUser(t, k, "rate-owner")
	_, callerTok := makeUser(t, k, "rate-caller")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "rate-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID, "visibility": "public"}, ownerTok).Body.Close()

	call := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "rate-owner/rate-action", "args": map[string]any{},
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

	_, ownerTok := makeUser(t, k, "list-ratings-owner")
	_, callerTok := makeUser(t, k, "list-ratings-caller")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "list-ratings-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID, "visibility": "public"}, ownerTok).Body.Close()

	call := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "list-ratings-owner/list-ratings-action", "args": map[string]any{},
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

	// The projection carries only the market signal — value, note, created_at — never the rater
	// id, the transaction/receipt it links, or a signature (§13, §16).
	readRatings := func(tok string) (*http.Response, []map[string]any) {
		resp := httpDo(t, srv, "GET", "/v1/actions/"+action.ID+"/ratings", nil, tok)
		var out []map[string]any
		if resp.StatusCode == http.StatusOK {
			decodeResponse(t, resp, &out)
		} else {
			resp.Body.Close()
		}
		return resp, out
	}

	resp, ratings := readRatings(ownerTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list ratings: expected 200, got %d", resp.StatusCode)
	}
	if len(ratings) != 1 {
		t.Fatalf("expected one rating, got %d", len(ratings))
	}
	if v, _ := ratings[0]["value"].(float64); v != 1 {
		t.Errorf("value: got %v, want 1", ratings[0]["value"])
	}
	if _, hasNote := ratings[0]["note"]; !hasNote {
		t.Error("projection must include a note field (null here)")
	}
	for _, leaked := range []string{"rater_user_id", "rated_tx_id", "rated_receipt_id", "signature", "id"} {
		if _, ok := ratings[0][leaked]; ok {
			t.Errorf("projection leaks %q", leaked)
		}
	}

	// Public action → readable anonymously (no token).
	if resp, _ := readRatings(""); resp.StatusCode != http.StatusOK {
		t.Errorf("anonymous read of a public action's ratings: got %d, want 200", resp.StatusCode)
	}

	// Reputation survives deactivation — the read is independent of active state (§8).
	httpDo(t, srv, "POST", "/v1/actions/disable", map[string]any{"target": action.ID}, ownerTok).Body.Close()
	if resp, r := readRatings(""); resp.StatusCode != http.StatusOK || len(r) != 1 {
		t.Errorf("deactivated public action ratings: got %d / %d rows, want 200 / 1", resp.StatusCode, len(r))
	}

	// A private action's ratings are owner-only: a non-owner (here anonymous) is refused.
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID, "visibility": "private"}, ownerTok).Body.Close()
	if resp, _ := readRatings(""); resp.StatusCode == http.StatusOK {
		t.Errorf("private action ratings must not be anonymously readable, got 200")
	}
	if resp, _ := readRatings(ownerTok); resp.StatusCode != http.StatusOK {
		t.Errorf("private action ratings must be readable by the owner, got %d", resp.StatusCode)
	}
}

func TestServeRateTransactionNotFound(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	tok, err := loginTokenFor(k, context.Background(), "sys", "sys-pass")
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

	_, ownerTok := makeUser(t, k, "stats-owner")
	_, callerTok := makeUser(t, k, "stats-caller")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "stats-action", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID, "visibility": "public"}, ownerTok).Body.Close()

	// Make one call to generate stats.
	httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "stats-owner/stats-action", "args": map[string]any{},
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

	_, tok := makeUser(t, k, "lookup-user")

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

	_, tok := makeUser(t, k, "upd-owner")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "upd-action", "kind": "http", "price": 0, "source": "http://x.example",
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, tok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": action.ID}, tok).Body.Close()

	newDesc := "updated description"
	resp := httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID,
		"description": newDesc,
	}, tok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update action: expected 200, got %d", resp.StatusCode)
	}
	var updated []kernel.Action
	decodeResponse(t, resp, &updated)
	if len(updated) != 1 {
		t.Fatalf("update must report the rows it wrote, got %d", len(updated))
	}
	if updated[0].Description != newDesc {
		t.Errorf("description: got %q, want %q", updated[0].Description, newDesc)
	}
	if updated[0].ID != action.ID {
		t.Errorf("ID mismatch after update")
	}
}

func TestServeListProcesses(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	userID, tok := makeUser(t, k, "lp-user")

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

	_, _ = makeUser(t, k, "logout-user")

	// Obtain a refresh token via PKCE.
	verifier := strings.Repeat("y", 43)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	authResp := httpDo(t, srv, "POST", "/v1/auth/authorize", map[string]any{
		"handle": "logout-user", "password": "pass", "code_challenge": challenge,
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
	db := newTestStore(t)

	cfg := testConfig("rl-test-secret")
	logger := log.Discard()
	k := newKernel(cfg, kernel.Dependencies{Store: db})
	if _, err := k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "rlu", Password: "pass",
	}); err != nil {
		t.Fatal(err)
	}

	srv := &server{kernel: k, log: logger}
	r := chi.NewRouter()
	r.Use(requestIDMiddleware)
	// Burst of 3 with zero refill rate so tokens don't recover during the test.
	r.With(ipRateLimiter(0, 3)).Post("/v1/auth/token", srv.postToken)
	ts := httptest.NewServer(r)
	defer ts.Close()

	body := map[string]any{"handle": "rlu", "password": "pass"}

	// httptest requests originate from loopback. A genuine local client (no X-Forwarded-For) is
	// exempt from rate limiting, so a burst well past the limit never 429s.
	for i := 0; i < 5; i++ {
		resp := httpDo(t, ts, "POST", "/v1/auth/token", body, "")
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Errorf("loopback request %d: unexpected 429 (genuine local client must be exempt)", i+1)
		}
	}

	// Behind a same-host proxy (loopback RemoteAddr + X-Forwarded-For), the real client is limited:
	// the resolved client is the last forwarded hop, so it is keyed and throttled per-client.
	xff := map[string]string{"X-Forwarded-For": "203.0.113.7"}
	for i := 0; i < 5; i++ {
		resp := httpDoWithHeaders(t, ts, "POST", "/v1/auth/token", body, "", xff)
		resp.Body.Close()
		if i < 3 {
			if resp.StatusCode == http.StatusTooManyRequests {
				t.Errorf("proxied request %d: unexpected 429 within burst", i+1)
			}
		} else {
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Errorf("proxied request %d: expected 429 after burst, got %d", i+1, resp.StatusCode)
			}
		}
	}
}

func TestServeRequestIDHeader(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	resp := httpDo(t, srv, "POST", "/v1/users", map[string]any{
		"handle": "ridtest", "email": "rid@example.com", "password": "p",
	}, "")
	defer resp.Body.Close()
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID header in response")
	}
}

func TestServeGetMe(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	uid, tok := makeUser(t, k, "metest")
	giveCredits(t, k, uid, 500)

	resp := httpDo(t, srv, "GET", "/v1/me", nil, tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me: expected 200, got %d", resp.StatusCode)
	}
	var got map[string]any
	decodeResponse(t, resp, &got)

	if got["handle"] != "metest" {
		t.Errorf("handle: got %v, want @metest", got["handle"])
	}
	if _, ok := got["description"]; !ok {
		t.Errorf("description key missing from /v1/me")
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

	_, tok := makeUser(t, k, "putmetest")

	// Update description only.
	resp := httpDo(t, srv, "PUT", "/v1/me", map[string]any{"description": "hi there"}, tok)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("update description: want 200, got %d: %s", resp.StatusCode, body)
	}
	var got map[string]any
	decodeResponse(t, resp, &got)
	if got["description"] != "hi there" {
		t.Errorf("description in response: got %v, want 'hi there'", got["description"])
	}

	// Confirm via GET /v1/me.
	resp2 := httpDo(t, srv, "GET", "/v1/me", nil, tok)
	var me map[string]any
	decodeResponse(t, resp2, &me)
	if me["description"] != "hi there" {
		t.Errorf("GET /v1/me description: got %v, want 'hi there'", me["description"])
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
	if status, _ := httpLogin(t, srv, "putmetest", "pass"); status != http.StatusUnauthorized {
		t.Errorf("old password: want 401, got %d", status)
	}
	if status, _ := httpLogin(t, srv, "putmetest", "newpass"); status != http.StatusOK {
		t.Errorf("new password: want 200, got %d", status)
	}

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
	resp6 := httpDo(t, srv, "PUT", "/v1/me", map[string]any{"description": "x"}, "")
	resp6.Body.Close()
	if resp6.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated: want 401, got %d", resp6.StatusCode)
	}
}

func TestGetActionReadPermission(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "ra-owner")
	_, strangerTok := makeUser(t, k, "ra-stranger")
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
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": action.ID, "visibility": "public"}, ownerTok).Body.Close()
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
	sys, err := k.ReadUserByHandle(ctx, "sys")
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
	_, err = k.EnsureKernelAccount(ctx, pubB64)
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
	pubAll := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll}); err != nil {
		t.Fatal(err)
	}

	// Signed federation call succeeds; counterparty is identified by public key.
	resp := fedCall(t, k, priv, a.ID, "idem-key-1", map[string]any{})
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
	resp2 := fedCall(t, k, priv, uuid.New().String(), "idem-key-2", map[string]any{})
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown action: expected 404, got %d", resp2.StatusCode)
	}

	// Missing counterparty (empty key) is unauthenticated.
	resp3 := fedCallRaw(t, k, "", time.Now().UTC().Format(time.RFC3339), "idem-key-3b", a.ID, "", []byte("{}"))
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing counterparty: expected 401, got %d", resp3.StatusCode)
	}

	// An empty action id names no action, exactly like an unknown one.
	resp4 := fedCall(t, k, priv, "", "idem-key-3", map[string]any{})
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Errorf("empty action: expected 404, got %d", resp4.StatusCode)
	}
}

// TestFederationCallResolvesByStableID: the inbound wire reference is this kernel's stable action
// id (§13), so an owner rename — display metadata, excluded from the contract hash — never strands
// a caller's cached proxy. Resolving by id also makes a proxy row nameable inbound, which the old
// handle lookup could not do, so it must be refused: federation is non-transitive (§8).
func TestFederationCallResolvesByStableID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	ctx := context.Background()
	sys, _ := k.ReadUserByHandle(ctx, "sys")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := k.EnsureKernelAccount(ctx, pubB64)
	if err != nil {
		t.Fatal(err)
	}

	ownerID, _ := makeUser(t, k, "provider")
	a, err := k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: "greet", Kind: kernel.KindHTTP,
		Source: backend.URL, Price: 0, Description: "greet",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetActive(ctx, ownerID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	pubAll := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, ownerID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll}); err != nil {
		t.Fatal(err)
	}

	// The owner renames: a handle-addressed dispatch would now resolve to nothing and park forever.
	if _, err := k.RenameUser(ctx, sys.ID, ownerID, "provider2"); err != nil {
		t.Fatal(err)
	}
	resp := fedCall(t, k, priv, a.ID, "idem-stable-1", map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("call after owner rename: expected 200, got %d", resp.StatusCode)
	}

	// A cached proxy row is never re-served, even when named by its id.
	m := kernel.ActionManifest{
		ActionID: "remote-act", OwnerHandle: "far", Name: "far-act", Kind: kernel.KindHTTP,
		Price: 0, RemoteBPS: kernel.DefaultEconomy().RemoteBPS, Description: "far",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-far", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	m.Signature, _ = testNet.SignManifest(priv, &m)
	proxy, err := k.ImportPeerAction(ctx, peer.ID, m)
	if err != nil {
		t.Fatal(err)
	}
	resp2 := fedCall(t, k, priv, proxy.ID, "idem-stable-2", map[string]any{})
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("inbound call naming a proxy id: expected 404, got %d", resp2.StatusCode)
	}
}

// TestFederationCallSignsRejectionForNonExecutableAction: an inbound call to a known-but-non-
// executable action (inactive, or active-but-private) returns a SIGNED zero-charge rejection
// receipt carrying the action's UUID — so the caller settles immediately instead of pinning
// funds until the 24h pending bound (§13). Previously this returned a bare error with no receipt.
func TestFederationCallSignsRejectionForNonExecutableAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}

	// Generate a keypair and register a remote peer.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.EnsureKernelAccount(ctx, pubB64); err != nil {
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

	// assertSignedRejection checks the response carries a zero-charge failure receipt whose
	// action_id is the real UUID (so the caller's VerifyRemoteReceipt action_id match passes).
	assertSignedRejection := func(label string, resp *http.Response) {
		t.Helper()
		defer resp.Body.Close()
		var env struct {
			Receipt *kernel.Receipt `json:"receipt"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
		if env.Receipt == nil {
			t.Fatalf("%s: expected a signed rejection receipt, got none (status %d)", label, resp.StatusCode)
		}
		if env.Receipt.Status != kernel.TxFailure {
			t.Errorf("%s: receipt status = %q, want failure", label, env.Receipt.Status)
		}
		if env.Receipt.Charge != 0 || env.Receipt.Gross != 0 || env.Receipt.Net != 0 || env.Receipt.Fee != 0 {
			t.Errorf("%s: expected zero-charge receipt, got charge=%d gross=%d net=%d fee=%d",
				label, env.Receipt.Charge, env.Receipt.Gross, env.Receipt.Net, env.Receipt.Fee)
		}
		if env.Receipt.ActionID != a.ID {
			t.Errorf("%s: receipt action_id = %q, want %q (caller verification would fail otherwise)",
				label, env.Receipt.ActionID, a.ID)
		}
		if env.Receipt.Signature == "" {
			t.Errorf("%s: rejection receipt is unsigned", label)
		}
		// The reason must reflect why the action wouldn't run, not a misleading generic "denied".
		if env.Receipt.Reason == "" || env.Receipt.Reason == "denied" || env.Receipt.Reason == "counterparty denied" {
			t.Errorf("%s: reason = %q, want a specific non-executable reason", label, env.Receipt.Reason)
		}
	}

	// Inactive action → signed rejection.
	assertSignedRejection("inactive", fedCall(t, k, priv, a.ID, "idem-s-1", map[string]any{}))

	// Activate but keep private → still non-executable for a non-owner → signed rejection.
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	assertSignedRejection("active-private", fedCall(t, k, priv, a.ID, "idem-s-2", map[string]any{}))
}

// TestWaitingOnPeer: a waiting step whose required caller is a peer (proxy) user is flagged
// waiting_on_peer; a local-user caller or a non-waiting step is not (§13 — advisory, never a gate).
func TestWaitingOnPeer(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	ctx := context.Background()

	localID, _ := makeUser(t, k, "local-caller")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	peerKey := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := k.EnsureKernelAccount(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.BindPetname(ctx, peerKey, "peer-caller", false); err != nil {
		t.Fatal(err)
	}

	uc := newAccountCache(k, ctx)
	peerStep := &kernel.Step{Status: kernel.StepWaiting, RequiredCallerUserID: peer.ID}
	pv := enrichStep(k, ctx, peerStep, nil, uc)
	if !pv.WaitingOnPeer {
		t.Error("step addressed to a peer should be waiting_on_peer")
	}
	if pv.RequiredCallerHandle != "peer-caller" {
		t.Errorf("required_caller_handle: got %q, want peer-caller", pv.RequiredCallerHandle)
	}
	localStep := &kernel.Step{Status: kernel.StepWaiting, RequiredCallerUserID: localID}
	if enrichStep(k, ctx, localStep, nil, uc).WaitingOnPeer {
		t.Error("step addressed to a local user should not be waiting_on_peer")
	}
	doneStep := &kernel.Step{Status: kernel.StepDone, RequiredCallerUserID: peer.ID}
	if enrichStep(k, ctx, doneStep, nil, uc).WaitingOnPeer {
		t.Error("a non-waiting step should never be waiting_on_peer")
	}
}

// TestStartRemoteRetryLoop: the serve retry worker lists pending traces, retries the due ones, and
// stops promptly when its context is cancelled (§13 — this is what settles parked remote calls
// without a restart). The retry's own settlement behavior is covered in kernel/federation_test.go.
func TestStartRemoteRetryLoop(t *testing.T) {
	// One trace created well in the past, so it is due on the first tick (nextAt = created + base).
	tr := &kernel.Trace{ID: "t1", ProcessID: "p1", CreatedAt: time.Now().Add(-time.Hour)}
	list := func(context.Context) ([]*kernel.Trace, error) { return []*kernel.Trace{tr}, nil }
	calls := make(chan string, 100)
	retry := func(_ context.Context, tr *kernel.Trace) error {
		select {
		case calls <- tr.ID:
		default:
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		startRemoteRetryLoop(ctx, nil, list, retry, func(context.Context) {}, time.Millisecond)
		close(done)
	}()

	select {
	case id := <-calls:
		if id != "t1" {
			t.Fatalf("retried wrong trace: %q", id)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("due trace was never retried")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop on ctx cancel")
	}
}

// TestStartRemoteRetryLoopDrainsFirst: work that was already parked when the transport came up is
// retried immediately, not one interval later (§13). The interval here is far longer than the test
// would ever wait, so only the startup drain can produce the call — a restart mid-call resumes as
// soon as there is a carrier. A trace too young for the backoff schedule is still drained: the
// snapshot is the pre-existing work, not what the scheduler considers due.
func TestStartRemoteRetryLoopDrainsFirst(t *testing.T) {
	parked := &kernel.Trace{ID: "parked", ProcessID: "p1", CreatedAt: time.Now()}
	list := func(context.Context) ([]*kernel.Trace, error) { return nil, nil }
	calls := make(chan string, 4)
	retry := func(_ context.Context, tr *kernel.Trace) error {
		select {
		case calls <- tr.ID:
		default:
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go startRemoteRetryLoop(ctx, []*kernel.Trace{parked}, list, retry, func(context.Context) {}, time.Hour)

	select {
	case id := <-calls:
		if id != "parked" {
			t.Fatalf("drained wrong trace: %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pre-existing parked work was not drained at startup")
	}
}

// TestStartPeerRetentionSweep checks the §13 retention sweep runs once at startup, keeps ticking,
// and stops on ctx cancel. The purge func is injected (no DB needed), like the retry-loop test.
func TestStartPeerRetentionSweep(t *testing.T) {
	calls := make(chan struct{}, 8)
	purge := func(context.Context) (int, error) {
		select {
		case calls <- struct{}{}:
		default:
		}
		return 0, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		startPeerRetentionSweep(ctx, purge, time.Millisecond)
		close(done)
	}()

	// Startup pass runs immediately, then the ticker drives at least one more.
	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatalf("purge pass %d did not run", i)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweep did not stop on ctx cancel")
	}
}

// TestBackoffScheduler pins the retry schedule: first attempt one base after creation, exponential
// spacing (base, 2×, 4×, …) capped, and pruning of traces that have resolved (dropped from the list).
func TestBackoffScheduler(t *testing.T) {
	base := time.Minute
	s := newBackoffScheduler(base)
	t0 := time.Now()
	tr := &kernel.Trace{ID: "a", ProcessID: "p", CreatedAt: t0}
	in := []*kernel.Trace{tr}

	// Before created+base: not due (skips the inline round-trip window).
	if got := s.due(in, t0.Add(30*time.Second)); len(got) != 0 {
		t.Fatalf("expected not due before base, got %d", len(got))
	}
	// At created+base: first retry.
	if got := s.due(in, t0.Add(base)); len(got) != 1 {
		t.Fatalf("expected 1 due at base, got %d", len(got))
	}
	// Immediately after: not due — next attempt is base later.
	if got := s.due(in, t0.Add(base+time.Second)); len(got) != 0 {
		t.Fatalf("expected not due right after first retry, got %d", len(got))
	}
	// Second retry one base after the first; then spacing must double to 2×base.
	if got := s.due(in, t0.Add(2*base)); len(got) != 1 {
		t.Fatalf("expected 2nd retry at 2×base, got %d", len(got))
	}
	if got := s.due(in, t0.Add(3*base)); len(got) != 0 {
		t.Fatalf("expected still backed off at 3×base (needs 2×base gap), got %d", len(got))
	}
	if got := s.due(in, t0.Add(4*base)); len(got) != 1 {
		t.Fatalf("expected 3rd retry at 4×base, got %d", len(got))
	}

	// Resolved: the trace drops out of the list → its schedule state is pruned.
	if got := s.due(nil, t0.Add(5*base)); len(got) != 0 {
		t.Fatalf("expected nothing due for empty list, got %d", len(got))
	}
	if len(s.entries) != 0 {
		t.Fatalf("expected pruned scheduler state, got %d entries", len(s.entries))
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
	sys, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.EnsureKernelAccount(ctx, pubB64); err != nil {
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
	pubAll := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll}); err != nil {
		t.Fatal(err)
	}

	action := a.ID
	cpKey := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	now := func() string { return time.Now().UTC().Format(time.RFC3339) }

	// No counterparty (empty key) → 401.
	r1 := fedCallRaw(t, k, "", now(), "idem-auth-1", action, "sig", []byte("{}"))
	r1.Body.Close()
	if r1.StatusCode != http.StatusUnauthorized {
		t.Errorf("no counterparty: want 401, got %d", r1.StatusCode)
	}

	// Unknown but signature-valid counterparty → handshake-free subscription (§13): the caller's
	// zero-balance billing account is lazily provisioned and the price-0 call succeeds.
	_, unknownPriv, _ := ed25519.GenerateKey(rand.Reader)
	unknownKey := base64.RawURLEncoding.EncodeToString(unknownPriv.Public().(ed25519.PublicKey))
	r2 := fedCall(t, k, unknownPriv, action, "idem-auth-2", map[string]any{})
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("unknown counterparty: want 200 (lazily provisioned), got %d", r2.StatusCode)
	}
	if u, _ := k.ReadAccountByKernelKey(ctx, unknownKey); u == nil || u.KernelPublicKey != unknownKey {
		t.Error("unknown caller should have been provisioned a proxy account")
	}

	// Missing timestamp → 401.
	r3 := fedCallRaw(t, k, cpKey, "", "idem-auth-3", action, "invalidsig", []byte("{}"))
	r3.Body.Close()
	if r3.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing timestamp: want 401, got %d", r3.StatusCode)
	}

	// Expired timestamp → 401 (age check precedes the signature check).
	oldTS := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	r4 := fedCallRaw(t, k, cpKey, oldTS, "idem-auth-4", action, "sig", []byte("{}"))
	r4.Body.Close()
	if r4.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired timestamp: want 401, got %d", r4.StatusCode)
	}

	// Invalid signature → 401.
	r6 := fedCallRaw(t, k, cpKey, now(), "idem-auth-6", action, "badsignature", []byte("{}"))
	r6.Body.Close()
	if r6.StatusCode != http.StatusUnauthorized {
		t.Errorf("invalid signature: want 401, got %d", r6.StatusCode)
	}

	// Valid auth + idempotency replay: second call with same key returns the cached result.
	r7 := fedCall(t, k, priv, action, "idem-replay-1", map[string]any{})
	defer r7.Body.Close()
	if r7.StatusCode != http.StatusOK {
		t.Fatalf("first call: want 200, got %d", r7.StatusCode)
	}
	r8 := fedCall(t, k, priv, action, "idem-replay-1", map[string]any{})
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
	sys, _ := k.ReadUserByHandle(ctx, "sys")

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, _ = k.EnsureKernelAccount(ctx, pubB64)

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
	pubAll := kernel.VisibilityPublic
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll})

	// First call: must return a non-nil receipt.
	r1 := fedCall(t, k, priv, a.ID, "replay-idem-1", map[string]any{})
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
	r2 := fedCall(t, k, priv, a.ID, "replay-idem-1", map[string]any{})
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

	// health must resolve the target through serverBaseURL like every other command, so --server
	// (here via flagServer, set by stubServer) is honored and it never falls back to another kernel.
	old := flagServer
	flagServer = srv.URL
	t.Cleanup(func() { flagServer = old })

	if _, err := execTestCmd(t, healthCmd()); err != nil {
		t.Fatalf("health: unexpected error: %v", err)
	}
}

// TestHealthCmdHonorsServer proves health hits the --server target, not the localhost:4040 default:
// it points flagServer at a live stub and a bogus default, and the live stub must receive /health.
func TestHealthCmdHonorsServer(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "handle": "k", "public_key": "pk"})
	}))
	defer srv.Close()

	old := flagServer
	flagServer = srv.URL
	t.Cleanup(func() { flagServer = old })

	if _, err := execTestCmd(t, healthCmd()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if hit != "/health" {
		t.Fatalf("health did not hit the --server target (got path %q)", hit)
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

	_, tok := makeUser(t, k, "import-srv-owner")

	resp := httpDo(t, srv, "POST", "/v1/actions/import",
		map[string]any{"name": "mail", "spec_url": specSrv.URL + "/spec.json"}, tok)
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
	if result.Created[0].Name != "mail/sayHello" {
		t.Errorf("name: got %q, want %q", result.Created[0].Name, "mail/sayHello")
	}

	// A re-import carries the name alone: the endpoint supplies the document URL it recorded.
	again := httpDo(t, srv, "POST", "/v1/actions/import", map[string]any{"name": "mail"}, tok)
	defer again.Body.Close()
	if again.StatusCode != http.StatusOK {
		t.Fatalf("re-import by name: want 200, got %d", again.StatusCode)
	}
	var second kernel.ImportResult
	if err := json.NewDecoder(again.Body).Decode(&second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(second.Unchanged) != 1 {
		t.Errorf("re-import by name: unchanged=%d, want 1", len(second.Unchanged))
	}

	// A name that holds no installation has no URL to re-read.
	miss := httpDo(t, srv, "POST", "/v1/actions/import", map[string]any{"name": "nothing"}, tok)
	defer miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Errorf("re-import of an unknown name: got %d, want 404", miss.StatusCode)
	}
}

// TestServeActionTargets: one mutation endpoint per verb, addressed by target — an id names one
// row, an owner/path names the whole subtree, and a row-specific field needs a single row.
func TestServeActionTargets(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	ownerID, tok := makeUser(t, k, "targetowner")
	ctx := context.Background()
	mk := func(name string) *kernel.Action {
		a, err := k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
			OwnerUserID: ownerID, Name: name, Kind: kernel.KindHTTP,
			Source: "http://api.example.com", Description: "an action",
			InputSchema:  map[string]any{"type": "object", "description": "in"},
			OutputSchema: map[string]any{"type": "object", "description": "out"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	root, member, sibling := mk("mail"), mk("mail/send"), mk("mailer")

	en := httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": "targetowner/mail"}, tok)
	defer en.Body.Close()
	if en.StatusCode != http.StatusOK {
		t.Fatalf("enable subtree: got %d", en.StatusCode)
	}
	var enabled []actionResp
	if err := json.NewDecoder(en.Body).Decode(&enabled); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(enabled) != 2 {
		t.Fatalf("enable must report both rows, got %d", len(enabled))
	}
	if got, _ := k.ReadAction(ctx, sibling.ID); got.Active {
		t.Error("mailer lies outside the mail subtree")
	}

	// A uniform field applies to the whole subtree.
	up := httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": "targetowner/mail", "visibility": "local"}, tok)
	defer up.Body.Close()
	if up.StatusCode != http.StatusOK {
		t.Fatalf("subtree visibility: got %d", up.StatusCode)
	}
	for _, a := range []*kernel.Action{root, member} {
		got, _ := k.ReadAction(ctx, a.ID)
		if got.Visibility != kernel.VisibilityLocal {
			t.Errorf("%s visibility = %q", got.Name, got.Visibility)
		}
	}

	// A row-specific field needs a target that names one row.
	bad := httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": "targetowner/mail", "description": "one only"}, tok)
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("description over a subtree: got %d, want 422", bad.StatusCode)
	}
	ok := httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": member.ID, "description": "one only"}, tok)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Errorf("description on one row: got %d", ok.StatusCode)
	}

	// A target naming nothing is a miss, not an empty success.
	none := httpDo(t, srv, "POST", "/v1/actions/disable", map[string]any{"target": "targetowner/nothing"}, tok)
	defer none.Body.Close()
	if none.StatusCode != http.StatusNotFound {
		t.Errorf("unmatched target: got %d, want 404", none.StatusCode)
	}

	del := httpDo(t, srv, "DELETE", "/v1/actions?target=targetowner/mail", nil, tok)
	defer del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete subtree: got %d", del.StatusCode)
	}
	if got, _ := k.ReadAction(ctx, member.ID); got != nil {
		t.Error("subtree member survived deletion")
	}
	if got, _ := k.ReadAction(ctx, sibling.ID); got == nil {
		t.Error("mailer must survive deletion of the mail subtree")
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
	sys, _ := k.ReadUserByHandle(ctx, "sys")

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
	pubAll := kernel.VisibilityPublic
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll})

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, _ = k.EnsureKernelAccount(ctx, pubB64)

	ikey1 := uuid.New().String()
	r1 := fedCall(t, k, priv, a.ID, ikey1, map[string]any{})
	defer r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first call: want 200, got %d", r1.StatusCode)
	}

	// Replay same key → 200.
	r2 := fedCall(t, k, priv, a.ID, ikey1, map[string]any{})
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("replay: want 200, got %d", r2.StatusCode)
	}

	// Pending in-flight key → 409. The inbound caller is a kernel account, addressed by its key:
	// it holds no handle (§13).
	caller, err := k.ReadAccountByKernelKey(ctx, base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	ikey2 := uuid.New().String()
	now := time.Now().UTC()
	_ = k.InsertPendingIdempotencyRecord(ctx, &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     ikey2,
		CounterpartyUserID: caller.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(time.Hour),
	})
	r3 := fedCall(t, k, priv, a.ID, ikey2, map[string]any{})
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusConflict {
		t.Errorf("pending: want 409, got %d", r3.StatusCode)
	}
}

// TestFederationIdempotencyPreconditionFailure verifies that a schema-validation failure
// (precondition 8) completes the idempotency record so replays return the error, not 409.
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

	sys, _ := k.ReadUserByHandle(ctx, "sys")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, _ = k.EnsureKernelAccount(ctx, pubB64)

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
	pubAll := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll}); err != nil {
		t.Fatal(err)
	}

	ikey := uuid.New().String()
	// fedHeaders signs an empty body {}; strict schema requires "name" → schema error.
	r1 := fedCall(t, k, priv, a.ID, ikey, map[string]any{})
	defer r1.Body.Close()
	if r1.StatusCode == http.StatusConflict {
		t.Fatal("first call returned 409: idempotency record was not created")
	}
	// Must be a client error (schema violation → 422).
	if r1.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("schema failure: want 422, got %d", r1.StatusCode)
	}

	// Replay same key → must return the same error, not 409.
	r2 := fedCall(t, k, priv, a.ID, ikey, map[string]any{})
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

	sys, _ := k.ReadUserByHandle(ctx, "sys")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, _ = k.EnsureKernelAccount(ctx, pubB64)

	a, _ := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID: sys.ID, Name: "fail-exec", Kind: kernel.KindHTTP,
		Source: backend.URL, Description: "always-failing action",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	k.SetActive(ctx, sys.ID, a.ID, true)
	pubAll := kernel.VisibilityPublic
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll})

	ikey := uuid.New().String()
	r1 := fedCall(t, k, priv, a.ID, ikey, map[string]any{})
	defer r1.Body.Close()
	if r1.StatusCode == http.StatusConflict {
		t.Fatal("first call returned 409")
	}

	// Replay same key: must return error (not 409) and a non-nil receipt.
	r2 := fedCall(t, k, priv, a.ID, ikey, map[string]any{})
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
// Rule B (§8 If-Match): a call whose expected_contract_hash no longer matches the action's current
// contract is refused before execution with a signed refresh_proxy rejection, so the caller re-resolves.
func TestFederationCallContractHashMismatch(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	ctx := context.Background()

	sys, _ := k.ReadUserByHandle(ctx, "sys")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	a, _ := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID: sys.ID, Name: "chash-act", Kind: kernel.KindHTTP,
		Source: backend.URL, Description: "chash",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	})
	k.SetActive(ctx, sys.ID, a.ID, true)
	pubAll := kernel.VisibilityPublic
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll})

	cp := base64.RawURLEncoding.EncodeToString(pub)
	ts := time.Now().UTC().Format(time.RFC3339)
	body := []byte("{}")
	argsHash := sha256HexBytes(body)
	ownKey, _ := k.GetConfig(ctx, configKeySigningPublic)
	// Sign a stale contract hash: it verifies (it is in the signed payload) but does not match current.
	sig, _ := testNet.SignFederationPayload(priv, a.ID, cp, ownKey, "stale-hash", "idem-chash-1", ts, argsHash, "", 0)
	status, respBody, err := handleFederationCall(k, ctx, cp, "stale-hash", ts, "idem-chash-1", a.ID, sig, kernel.BuyerTerms{}, body)
	if err != nil {
		t.Fatalf("handleFederationCall: %v", err)
	}
	if status != http.StatusConflict {
		t.Errorf("status: got %d, want 409 (invalid_state, aligned with the receipt's code)", status)
	}
	rc, _ := respBody["receipt"].(*kernel.Receipt)
	if rc == nil || !rc.RefreshProxy || rc.Status != kernel.TxFailure || rc.Charge != 0 {
		t.Errorf("want a signed zero-charge refresh_proxy rejection, got %+v", rc)
	}
}

func TestFederationCallRejectsArgsHashMismatch(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	ctx := context.Background()

	sys, _ := k.ReadUserByHandle(ctx, "sys")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	_, err := k.EnsureKernelAccount(ctx, pubB64)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID: sys.ID, Name: "hash-check", Kind: kernel.KindHTTP,
		Source: backend.URL, Description: "hash-check",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	})
	k.SetActive(ctx, sys.ID, a.ID, true)
	pubAll := kernel.VisibilityPublic
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll})

	// Sign over the empty body {} but deliver a different body — the receiver hashes the bytes
	// it actually got, so the signature no longer matches and the call is rejected (401).
	cp := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	ts := time.Now().UTC().Format(time.RFC3339)
	signedHash := sha256HexBytes([]byte("{}"))
	ownKey, _ := k.GetConfig(ctx, configKeySigningPublic)
	sig, _ := testNet.SignFederationPayload(priv, a.ID, cp, ownKey, "", "idem-hash-1", ts, signedHash, "", 0)
	resp := fedCallRaw(t, k, cp, ts, "idem-hash-1", a.ID, sig, []byte(`{"injected":true}`))
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
	sys, _ := k.ReadUserByHandle(ctx, "sys")

	userID, tok := makeUser(t, k, "vrr-http-user")
	giveCredits(t, k, userID, 100)

	// Create a local HTTP action and make a call via /v1/run to get a transaction.
	a, _ := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID: sys.ID, Name: "vrr-http", Kind: kernel.KindHTTP,
		Source: backend.URL, Description: "vrr-http",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	})
	k.SetActive(ctx, sys.ID, a.ID, true)
	pubAll := kernel.VisibilityPublic
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubAll})

	callResp := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "sys/vrr-http", "args": map[string]any{},
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

	// A local call is verifiable too: one surface, audited against this kernel's own key (§11).
	resp := httpDo(t, srv, "GET", "/v1/transactions/"+txID+"/receipt-verification", nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local receipt verification: want 200, got %d", resp.StatusCode)
	}
	var v kernel.ReceiptVerification
	decodeResponse(t, resp, &v)
	if !v.Valid {
		t.Errorf("local receipt reported invalid over HTTP, checks=%v", v.Checks)
	}

	// Unknown transaction → 404.
	resp2 := httpDo(t, srv, "GET", "/v1/transactions/"+uuid.New().String()+"/receipt-verification", nil, tok)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown tx: want 404, got %d", resp2.StatusCode)
	}
}

// A peer serves one page of what it holds, under its own order: a filter or an offset has nothing
// to act on, so combining one with ?peer= is refused rather than silently ignored.
func TestServeListStepsPeerRefusesFilters(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	_, tok := makeUser(t, k, "peer-list-flags")
	for _, q := range []string{"limit=5", "offset=1", "status=waiting", "process_id=x"} {
		resp := httpDo(t, srv, "GET", "/v1/steps?peer=cGVlcg&"+q, nil, tok)
		resp.Body.Close()
		if resp.StatusCode != kernel.ErrInvalidInput.HTTP {
			t.Errorf("?peer with %s: want %d, got %d", q, kernel.ErrInvalidInput.HTTP, resp.StatusCode)
		}
	}
}

func TestServeListActionsOwnerAuth(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "la-auth-owner")

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
	resp := httpDo(t, srv, "GET", "/v1/actions?owner=la-auth-owner", nil, "")
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
	resp2 := httpDo(t, srv, "GET", "/v1/actions?owner=la-auth-owner", nil, ownerTok)
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

// TestSuperuserScopeOverTCP proves supervision is scope on the normal TCP endpoints: over the
// public API a @sys token sees another user's private action and process and may disable any
// action, while a normal caller stays own-scoped. This is what replaced admin actions/
// processes/disable (no separate admin surface).
func TestSuperuserScopeOverTCP(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()
	ctx := context.Background()

	sysTok, err := loginTokenFor(k, ctx, "sys", "sys-pass")
	if err != nil {
		t.Fatal(err)
	}
	aliceID, aliceTok := makeUser(t, k, "alice")

	// @alice creates a private, inactive action.
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "secret", "kind": "http", "price": 0, "source": "http://127.0.0.1:1/x",
		"description": "private", "input_schema": minSchema, "output_schema": minSchema,
	}, aliceTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)

	// Anonymous listing of @alice's actions excludes the private one; @sys sees it.
	anon := decodeActions(t, httpDo(t, srv, "GET", "/v1/actions?owner=alice", nil, ""))
	if len(anon) != 0 {
		t.Errorf("anonymous should see 0 of @alice's actions, got %d", len(anon))
	}
	asSys := decodeActions(t, httpDo(t, srv, "GET", "/v1/actions?owner=alice", nil, sysTok))
	if len(asSys) != 1 {
		t.Errorf("sys should see @alice's private action, got %d", len(asSys))
	}

	// @alice owns a process; @sys sees it in the system-wide process list, a stranger doesn't.
	giveCredits(t, k, aliceID, 100)
	proc := setupProcessHTTP(t, db, aliceID, 100)
	if !containsProcess(t, httpDo(t, srv, "GET", "/v1/processes", nil, sysTok), proc.ID) {
		t.Error("sys process list should include @alice's process")
	}
	_, bobTok := makeUser(t, k, "bob")
	if containsProcess(t, httpDo(t, srv, "GET", "/v1/processes", nil, bobTok), proc.ID) {
		t.Error("bob must not see @alice's process")
	}

	// @sys may disable @alice's action over TCP (owner-or-superuser); @bob may not.
	if resp := httpDo(t, srv, "POST", "/v1/actions/disable", map[string]any{"target": action.ID}, bobTok); resp.StatusCode < 400 {
		t.Errorf("bob disabling @alice's action should fail, got %d", resp.StatusCode)
	}
	if resp := httpDo(t, srv, "POST", "/v1/actions/disable", map[string]any{"target": action.ID}, sysTok); resp.StatusCode >= 400 {
		t.Errorf("sys disabling @alice's action should succeed, got %d", resp.StatusCode)
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

// ---- discovery ----

// fakeDiscoverer stands in for *fed.Transport so the discovery pass is exercised without libp2p.
type fakeDiscoverer struct {
	bootstrap []string
	providers []string // routing-discovery providers enumerated this pass
	gossip    map[string]json.RawMessage
	// broken names peers whose pull fails AFTER dispatch (a dropped stream), the outcome the real
	// transport leaves untagged because it cannot prove whether the request arrived.
	broken     map[string]bool
	gossiped   []string
	advertised int
}

func (f *fakeDiscoverer) Advertise(context.Context) (time.Duration, error) {
	f.advertised++
	return time.Minute, nil
}
func (f *fakeDiscoverer) DiscoverProviders(context.Context) ([]string, error) {
	return f.providers, nil
}
func (f *fakeDiscoverer) BootstrapKeys() []string { return f.bootstrap }
func (f *fakeDiscoverer) Gossip(_ context.Context, key string, _ string) (json.RawMessage, error) {
	f.gossiped = append(f.gossiped, key)
	if f.broken[key] {
		return nil, fmt.Errorf("fed: read gossip: stream reset")
	}
	if raw, ok := f.gossip[key]; ok {
		return raw, nil
	}
	// An unreachable peer fails at resolve/connect, which the transport marks as provably never
	// dispatched (§13) — the distinction the contact cache turns on, so the fake must carry it.
	return nil, fmt.Errorf("%w: cannot resolve peer", fed.ErrNotDispatched)
}

// noCursor / discardCursor are the getCursor / setCursor stubs for discoverOnce tests.
func noCursor(context.Context, string) string             { return "" }
func discardCursor(context.Context, string, string) error { return nil }

// discoverOnce (§13): advertise this kernel, then dedup peers ∪ bootstrap ∪ routing-discovery
// providers, pull each, and accumulate only a VERIFIED reply (introducer = the authenticated key we
// dialed, == g.PublicKey). A non-verified outcome — offline, or a responder claiming a different
// identity than the dialed key — is never accumulated (and is retried a later pass).
func TestDiscoverOnce(t *testing.T) {
	mkGossip := func(pk string) json.RawMessage {
		b, _ := json.Marshal(kernel.GossipResponse{PublicKey: pk, Handle: pk})
		return b
	}
	poison, _ := json.Marshal(kernel.GossipResponse{PublicKey: "X", Handle: "X"}) // D claims X ≠ D
	f := &fakeDiscoverer{
		bootstrap: []string{"A"},
		providers: []string{"A", "B", "C", "D"}, // A duplicates bootstrap; C offline; D mismatched
		gossip:    map[string]json.RawMessage{"A": mkGossip("A"), "B": mkGossip("B"), "D": poison},
	}
	var got []string
	acc := func(_ context.Context, g *kernel.GossipResponse, introducer string) (string, error) {
		if introducer != g.PublicKey {
			t.Errorf("introducer %q must be the authenticated key (== g.PublicKey %q)", introducer, g.PublicKey)
		}
		got = append(got, g.PublicKey)
		return "", nil
	}
	noFriends := func(context.Context) []string { return nil }
	discoverOnce(context.Background(), f, noFriends, acc, noContact, noCursor, discardCursor, log.Discard())

	sort.Strings(got)
	if strings.Join(got, ",") != "A,B" {
		t.Errorf("accumulated %v, want [A B] (dup deduped, offline C and mismatched D excluded)", got)
	}
	if f.advertised == 0 {
		t.Error("discoverOnce must advertise this kernel to the routing-discovery namespace")
	}
}

// discoverOnce runs peer sync with no seeds at all (empty bootstrap_peers and no known kernels, §13):
// it still pulls gossip from each known peer and records that it was reached. A kernel that answered
// is a contact whether or not it has ever traded here.
func TestDiscoverOncePeerSyncNoSeeds(t *testing.T) {
	g, _ := json.Marshal(kernel.GossipResponse{PublicKey: "F", Handle: "F"})
	f := &fakeDiscoverer{gossip: map[string]json.RawMessage{"F": g}}
	peers := func(context.Context) []string { return []string{"F"} }
	var contacts []contactCall
	acc := func(context.Context, *kernel.GossipResponse, string) (string, error) { return "", nil }
	discoverOnce(context.Background(), f, peers, acc, recordInto(&contacts), noCursor, discardCursor, log.Discard())

	if len(contacts) != 1 {
		t.Fatalf("recorded %d contacts, want 1", len(contacts))
	}
	if c := contacts[0]; c.key != "F" || c.outcome != contactReached {
		t.Errorf("contact = (%q, outcome=%v), want (F, reached)", c.key, c.outcome)
	}
}

// contactCall records one contactRecorder invocation for assertions.
type contactCall struct {
	key     string
	outcome contactOutcome
}

func recordInto(out *[]contactCall) contactRecorder {
	return func(_ context.Context, key string, outcome contactOutcome) {
		*out = append(*out, contactCall{key, outcome})
	}
}

func noContact(context.Context, string, contactOutcome) {}

// A dial that never got an answer is the observation that proves a peer unreachable, so the pass
// records it; every later stage means the peer DID answer and leaves reachability alone (§13).
func TestDiscoverOnceRecordsContactOutcomes(t *testing.T) {
	good, _ := json.Marshal(kernel.GossipResponse{PublicKey: "UP", Handle: "UP"})
	f := &fakeDiscoverer{
		providers: []string{"UP", "DOWN", "LIAR", "FLAKY"},
		bootstrap: []string{"UP"},
		broken:    map[string]bool{"FLAKY": true},
		gossip: map[string]json.RawMessage{
			"UP": good,
			// LIAR answers, but as somebody else: a verification failure, not a reachability one.
			"LIAR": func() json.RawMessage {
				b, _ := json.Marshal(kernel.GossipResponse{PublicKey: "OTHER", Handle: "OTHER"})
				return b
			}(),
		},
	}
	acc := func(context.Context, *kernel.GossipResponse, string) (string, error) { return "", nil }
	var contacts []contactCall
	discoverOnce(context.Background(), f, func(context.Context) []string { return nil },
		acc, recordInto(&contacts), noCursor, discardCursor, log.Discard())

	byKey := map[string]contactCall{}
	for _, c := range contacts {
		byKey[c.key] = c
	}
	if c, seen := byKey["UP"]; !seen || c.outcome != contactReached {
		t.Errorf("a verified pull must record a successful contact, got %+v (seen=%v)", c, seen)
	}
	if c, seen := byKey["DOWN"]; !seen || c.outcome != contactUndispatched {
		t.Errorf("an undialable peer must record a failed contact, got %+v (seen=%v)", c, seen)
	}
	if _, seen := byKey["LIAR"]; seen {
		t.Error("a peer that answered (even wrongly) must not be recorded as unreachable")
	}
	// The pull broke after dispatch, so nobody knows whether it arrived: the pass reports that
	// honestly and newContactRecorder is what declines to write it (asserted below).
	if c, seen := byKey["FLAKY"]; !seen || c.outcome != contactUnknown {
		t.Errorf("a pull that broke after dispatch = %+v (seen=%v), want contactUnknown", c, seen)
	}
}

// Only proof reaches the database. The recorder is the single place that decides it, so every
// observer can report what it saw without knowing which outcomes are worth persisting.
func TestContactRecorderPersistsOnlyProof(t *testing.T) {
	type write struct {
		key string
		ok  bool
	}
	var writes []write
	rec := newContactRecorder(func(_ context.Context, key string, ok bool) error {
		writes = append(writes, write{key, ok})
		return nil
	})
	ctx := context.Background()
	rec(ctx, "A", contactReached)
	rec(ctx, "B", contactUndispatched)
	rec(ctx, "C", contactUnknown) // proves nothing
	rec(ctx, "", contactReached)  // no peer to date

	if len(writes) != 2 {
		t.Fatalf("wrote %v, want only the two proven outcomes", writes)
	}
	if writes[0] != (write{"A", true}) || writes[1] != (write{"B", false}) {
		t.Errorf("wrote %v, want A=success and B=failure", writes)
	}
}

// A cancelled context must not cancel the write recording it: the timeout that proves a peer
// unreachable arrives with its context already dead.
func TestContactRecorderSurvivesCancelledContext(t *testing.T) {
	var got bool
	rec := newContactRecorder(func(ctx context.Context, _ string, _ bool) error {
		got = ctx.Err() == nil
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec(ctx, "A", contactUndispatched)
	if !got {
		t.Error("the contact write inherited the cancellation that produced it")
	}
}

// readJSONLogEvents parses a JSON-format log file and returns the records whose message equals
// event. Used to assert diagnostic log lines without a capturable logger (log.New writes only to
// stderr + an optional file path — its handler is unexported, so the file is the seam).
func readJSONLogEvents(t *testing.T, path, event string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line not JSON: %v", err)
		}
		if m["msg"] == event {
			out = append(out, m)
		}
	}
	return out
}

// discoverOnce tags every failed pull with the stage it failed at plus its elapsed time, so an
// operator can tell a timeout (elapsed≈cap) from a fast hard-fail and see which stage broke (§13
// diag). Each non-verified outcome maps to a distinct stage: transport / decode / mismatch / accumulate.
func TestDiscoverPullFailureLog(t *testing.T) {
	mkGossip := func(pk string) json.RawMessage {
		b, _ := json.Marshal(kernel.GossipResponse{PublicKey: pk, Handle: pk})
		return b
	}
	f := &fakeDiscoverer{
		bootstrap: []string{"OFF", "DEC", "MIS", "ACC"}, // configured seeds, all pulled this pass
		gossip: map[string]json.RawMessage{
			"DEC": json.RawMessage("{not json"), // decode: malformed reply
			"MIS": mkGossip("OTHER"),            // mismatch: claims OTHER ≠ MIS
			"ACC": mkGossip("ACC"),              // verifies, but accumulate rejects
			// "OFF" absent from the map → fakeDiscoverer returns an error → transport stage
		},
	}
	acc := func(_ context.Context, g *kernel.GossipResponse, _ string) (string, error) {
		if g.PublicKey == "ACC" {
			return "", fmt.Errorf("rejected")
		}
		return "", nil
	}
	noFriends := func(context.Context) []string { return nil }

	logPath := filepath.Join(t.TempDir(), "disc.log")
	logger, err := log.New(log.Config{Level: "debug", FilePath: logPath, Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	discoverOnce(context.Background(), f, noFriends, acc, noContact, noCursor, discardCursor, logger)

	stageByKey := map[string]string{}
	for _, e := range readJSONLogEvents(t, logPath, "discovery.pull.failed") {
		key, _ := e["key"].(string)
		stage, _ := e["stage"].(string)
		stageByKey[key] = stage
		if _, ok := e["elapsed_ms"]; !ok {
			t.Errorf("%s: discovery.pull.failed missing elapsed_ms", key)
		}
	}
	want := map[string]string{"OFF": "transport", "DEC": "decode", "MIS": "mismatch", "ACC": "accumulate"}
	for k, w := range want {
		if stageByKey[k] != w {
			t.Errorf("key %s: stage %q, want %q", k, stageByKey[k], w)
		}
	}
}

// OnGossip emits gossip.served with the serialized response size and payload counts (reusing the
// marshal it already performs), so a puller's read-side failure can be correlated against a heavy
// gossip frame near the relayed allowance (§13 diag).
func TestOnGossipServedTelemetry(t *testing.T) {
	_, k, _ := newFlowKernel(t, nil)
	logPath := filepath.Join(t.TempDir(), "gossip.log")
	logger, err := log.New(log.Config{Level: "debug", FilePath: logPath, Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	h := &fedHandlers{kernel: k, log: logger}
	if _, err := h.OnGossip(context.Background(), "peerX", fed.GossipRequest{}); err != nil {
		t.Fatal(err)
	}
	events := readJSONLogEvents(t, logPath, "gossip.served")
	if len(events) != 1 {
		t.Fatalf("gossip.served emitted %d times, want 1", len(events))
	}
	e := events[0]
	if e["requester"] != "peerX" {
		t.Errorf("requester %v, want peerX", e["requester"])
	}
	if b, _ := e["bytes"].(float64); b <= 0 {
		t.Errorf("bytes %v, want > 0", e["bytes"])
	}
	if _, ok := e["manifests"]; !ok {
		t.Error("gossip.served missing manifests count")
	}
}

// startDiscoveryLoop runs one pass immediately, then ticks, and stops when ctx is cancelled.
func TestStartDiscoveryLoop(t *testing.T) {
	var mu sync.Mutex
	passes := 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		startDiscoveryLoop(ctx, 5*time.Millisecond, func(context.Context) {
			mu.Lock()
			passes++
			mu.Unlock()
		})
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop on ctx cancel")
	}
	mu.Lock()
	p := passes
	mu.Unlock()
	if p < 2 {
		t.Errorf("expected >= 2 passes (startup + ticks), got %d", p)
	}
}

// TestGrantRoutesRequireAuth: the delegated-OAuth grant routes are behind authentication (§8/§14).
func TestGrantRoutesRequireAuth(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	cases := []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/grants/plan?selector=@x", nil},
		{"POST", "/v1/grants/start", map[string]any{"selector": "x/y"}},
		{"POST", "/v1/grants/complete", map[string]any{"state": "s"}},
		{"POST", "/v1/grants", map[string]any{"selector": "x/y", "token": "t"}},
		{"DELETE", "/v1/grants?selector=@x/y", nil},
		{"DELETE", "/v1/grants?account=bearer:x", nil},
	}
	for _, c := range cases {
		resp := httpDo(t, srv, c.method, c.path, c.body, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without token: got %d, want 401", c.method, c.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestKeyLimiter: the per-peer federation limiter allows a burst then throttles, per key (F4).
func TestKeyLimiter(t *testing.T) {
	kl := newKeyLimiter(1, 2) // 1/s, burst 2
	if !kl.allow("a") || !kl.allow("a") {
		t.Fatal("a burst of 2 should pass")
	}
	if kl.allow("a") {
		t.Error("the third immediate call for the same key should be throttled")
	}
	if !kl.allow("b") {
		t.Error("a different key must have its own bucket")
	}
}

// TestOnCallRejectsPeerKeyMismatch: an inbound call whose signed Counterparty does not match the
// Noise-authenticated connection key is rejected before execution (F4 defense-in-depth). The
// mismatch check precedes any kernel work, so a nil-kernel handler is sufficient.
func TestOnCallRejectsPeerKeyMismatch(t *testing.T) {
	h := &fedHandlers{callLimiter: newKeyLimiter(50, 100)}
	resp := h.OnCall(context.Background(), "peerA", fed.CallRequest{
		Counterparty: "peerB", Timestamp: "t", IdempotencyKey: "k", Action: "x/y", Signature: "s", Args: json.RawMessage("{}"),
	})
	if resp.Status != http.StatusUnauthorized {
		t.Errorf("mismatched peer key: status %d, want 401", resp.Status)
	}
}

// TestServeActionsRefMode: the listing endpoint's reference mode resolves one reference through the
// kernel's resolver — so an application root and a raw id behave here exactly as they do when
// called — requires authentication because resolving can dial a peer, and never mixes with the
// flat filters.
func TestServeActionsRefMode(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, tok := makeUser(t, k, "app-owner")
	ownerHandle := "app-owner"
	mk := func(name string) string {
		cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
			"name": name, "kind": "http", "price": 0, "source": "http://x.example",
			"description": "an action", "input_schema": minSchema, "output_schema": minSchema,
			"visibility": "public",
		}, tok)
		var a kernel.Action
		decodeResponse(t, cr, &a)
		httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": a.ID, "visibility": "public"}, tok).Body.Close()
		httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": a.ID}, tok).Body.Close()
		return a.ID
	}
	idxID := mk("mail/index")
	rootID := mk("index")

	get := func(query, token string) (int, []actionResp) {
		resp := httpDo(t, srv, "GET", "/v1/actions?"+query, nil, token)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, nil
		}
		var out []actionResp
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.StatusCode, out
	}

	// A group and an owner root both resolve to the index answering there.
	for query, want := range map[string]string{
		"ref=" + url.QueryEscape(ownerHandle+"/mail"): idxID,
		"ref=" + url.QueryEscape(ownerHandle):         rootID,
		"ref=" + idxID:                                idxID,
	} {
		code, out := get(query, tok)
		if code != http.StatusOK || len(out) != 1 || out[0].ID != want {
			t.Errorf("%s: got code=%d %+v, want the single action %s", query, code, out, want)
		}
	}

	// Authentication is required by the dial, not by resolution: a kernel-qualified reference is
	// refused anonymously, while a local one stays open — that is what keeps a public action's
	// ratings anonymously readable through the same client-side reference resolution (§11).
	if code, _ := get("ref="+url.QueryEscape("someone@"+strings.Repeat("A", 43)+"/mail"), ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous kernel-qualified ref: got %d, want 401", code)
	}
	if code, out := get("ref="+url.QueryEscape(ownerHandle+"/mail"), ""); code != http.StatusOK || len(out) != 1 || out[0].ID != idxID {
		t.Errorf("anonymous local ref on a public action: got code=%d %+v", code, out)
	}
	// Action names carry no character restriction, so an @ inside a NAME is still a local
	// reference: only the head before the first / qualifies a kernel.
	atID := mk("mail@home")
	if code, out := get("ref="+url.QueryEscape(ownerHandle+"/mail@home"), ""); code != http.StatusOK || len(out) != 1 || out[0].ID != atID {
		t.Errorf("anonymous local ref whose name contains @: got code=%d %+v", code, out)
	}
	// Reference mode and the flat filters are different questions and never combine.
	if code, _ := get("ref="+url.QueryEscape(ownerHandle+"/mail")+"&name=mail/index", tok); code != http.StatusUnprocessableEntity {
		t.Errorf("ref+name: got %d, want 422", code)
	}
	// A miss is an empty list, like every other filter on this endpoint.
	if code, out := get("ref="+url.QueryEscape(ownerHandle+"/absent"), tok); code != http.StatusOK || len(out) != 0 {
		t.Errorf("miss: got code=%d %+v, want an empty list", code, out)
	}
	// The flat filters stay exact: ?name= names a row, and never resolves a group.
	if code, out := get("name=mail&owner="+ownerHandle, tok); code != http.StatusOK || len(out) != 0 {
		t.Errorf("name filter must stay exact: got code=%d %+v", code, out)
	}
}

// TestServeImportOpenAPIName: the import endpoint takes the application's name, one document per
// name, and the operation keyed index becomes its root.
func TestServeImportOpenAPIName(t *testing.T) {
	const spec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/":{"get":{"operationId":"index","description":"the application","parameters":[{"name":"q","in":"query","description":"query","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}},"/hello":{"get":{"operationId":"greet","description":"says hello","parameters":[{"name":"name","in":"query","description":"who","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	specSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(spec))
	}))
	defer specSrv.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()
	_, tok := makeUser(t, k, "app-import-owner")

	resp := httpDo(t, srv, "POST", "/v1/actions/import",
		map[string]any{"name": "mail", "spec_url": specSrv.URL + "/spec.json"}, tok)
	defer resp.Body.Close()
	var result kernel.ImportResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := map[string]bool{}
	for _, a := range result.Created {
		names[a.Name] = true
	}
	if !names["mail/index"] || !names["mail/greet"] {
		t.Fatalf("imported under the application name: got %v", names)
	}

	// The same document under a second name is an independent application.
	resp2 := httpDo(t, srv, "POST", "/v1/actions/import",
		map[string]any{"name": "inbox", "spec_url": specSrv.URL + "/spec.json"}, tok)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("second installation: got %d, want 200", resp2.StatusCode)
	}

	// A second document under an occupied name is refused.
	resp3 := httpDo(t, srv, "POST", "/v1/actions/import",
		map[string]any{"name": "mail", "spec_url": specSrv.URL + "/other.json"}, tok)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("re-binding a name: got %d, want 422", resp3.StatusCode)
	}
}
