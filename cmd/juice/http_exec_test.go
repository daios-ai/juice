package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
)

// erroringBox is a SecretBox whose Open always fails, simulating an unreadable auth_json
// (wrong key, key rotation, corrupted ciphertext).
type erroringBox struct{}

func (erroringBox) Seal(aad, plaintext string) (string, error) { return plaintext, nil }
func (erroringBox) Open(aad, ciphertext string) (string, error) {
	return "", fmt.Errorf("decrypt failed")
}

// httpSrc builds canonical HTTPSource JSON for a full URL + method (path split
// from the URL), mirroring what the kernel stores for a manual kind=http action.
func httpSrc(rawURL, method string, params ...kernel.HTTPParam) string {
	base, path := rawURL, ""
	if i := strings.Index(rawURL, "://"); i >= 0 {
		if j := strings.IndexByte(rawURL[i+3:], '/'); j >= 0 {
			base, path = rawURL[:i+3+j], rawURL[i+3+j:]
		}
	}
	b, _ := json.Marshal(kernel.HTTPSource{Type: "http", BaseURL: base, Path: path, Method: method, Params: params})
	return string(b)
}

func TestValidateResolvedIPBlocked(t *testing.T) {
	blocked := []string{"192.168.1.1", "10.0.0.1", "172.16.0.1", "169.254.1.1"}
	for _, ip := range blocked {
		err := validateResolvedIP(ip)
		if err == nil {
			t.Errorf("validateResolvedIP(%q): expected error, got nil", ip)
			continue
		}
		// Every SSRF rejection names the escape hatch uniformly (item 3).
		if !strings.Contains(err.Error(), "allow_local_sources") {
			t.Errorf("validateResolvedIP(%q): message %q should name allow_local_sources", ip, err.Error())
		}
	}
}

func TestValidateResolvedIPAllowed(t *testing.T) {
	// Loopback is permitted by default (127.0.0.1 / ::1); public IPs always are.
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "127.0.0.1", "::1"}
	for _, ip := range allowed {
		if err := validateResolvedIP(ip); err != nil {
			t.Errorf("validateResolvedIP(%q): unexpected error: %v", ip, err)
		}
	}
}

func TestValidateRedirectHostBlocked(t *testing.T) {
	cases := []string{"192.168.1.1", "10.0.0.1", "169.254.1.1"}
	for _, h := range cases {
		if err := validateRedirectHost(h, false); err == nil {
			t.Errorf("validateRedirectHost(%q, false): expected error, got nil", h)
		}
	}
}

func TestValidateRedirectHostAllowed(t *testing.T) {
	// Loopback redirect targets are permitted by default (no flag); the LAN needs allowLocal.
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		if err := validateRedirectHost(h, false); err != nil {
			t.Errorf("validateRedirectHost(%q, false): unexpected error: %v", h, err)
		}
	}
	for _, h := range []string{"192.168.1.1", "10.0.0.1"} {
		if err := validateRedirectHost(h, true); err != nil {
			t.Errorf("validateRedirectHost(%q, true): unexpected error: %v", h, err)
		}
	}
}

// fakeFedCaller stands in for the libp2p transport: it returns a canned CallResponse so the
// envelope-parsing + result/receipt extraction in executeFederationOverTransport is testable
// without a network.
type fakeFedCaller struct {
	resolveResp fed.ResolveResponse
	settleResp  fed.SettleResponse
	resp    fed.CallResponse
	err     error
	lastReq fed.CallRequest
}

func (f *fakeFedCaller) Call(_ context.Context, _ string, req fed.CallRequest) (fed.CallResponse, error) {
	f.lastReq = req
	return f.resp, f.err
}

func (f *fakeFedCaller) Resolve(_ context.Context, _ string, _ fed.ResolveRequest) (fed.ResolveResponse, error) {
	return f.resolveResp, f.err
}

func (f *fakeFedCaller) Settle(_ context.Context, _ string, _ fed.SettleRequest) (fed.SettleResponse, error) {
	return f.settleResp, f.err
}

func TestExecuteFederationSuccess(t *testing.T) {
	fc := &fakeFedCaller{resp: fed.CallResponse{
		Status: 200,
		Body:   []byte(`{"result":{"ok":true},"receipt":{"id":"r1","status":"success"}}`),
	}}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := func(action, cp, recipient, chash, ikey, argsHash string) (string, string, error) {
		return "sig", "ts", nil
	}
	fr, err := executeFederationOverTransport(context.Background(), fc, signer,
		base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		"peerkey", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-123", map[string]any{})
	if err != nil {
		t.Fatalf("executeFederationOverTransport: %v", err)
	}
	if fr.Result["ok"] != true {
		t.Errorf("result: got %v, want ok:true", fr.Result)
	}
	if fr.ReceiptJSON == "" {
		t.Error("expected non-empty receiptJSON")
	}
	// The exact args bytes were signed and forwarded (the args_hash contract).
	if fc.lastReq.IdempotencyKey != "key-123" || string(fc.lastReq.Args) != "{}" {
		t.Errorf("request not forwarded verbatim: %+v", fc.lastReq)
	}
}

func TestExecuteFederationNon200(t *testing.T) {
	// A non-200 with no parseable receipt → no receipt, status propagated (caller stays pending).
	fc := &fakeFedCaller{resp: fed.CallResponse{Status: 500, Body: []byte(`error`)}}
	fr, err := executeFederationOverTransport(context.Background(), fc, nil, "local", "peer", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-x", map[string]any{})
	if err != nil {
		t.Fatalf("executeFederationOverTransport: unexpected error: %v", err)
	}
	if fr.ReceiptJSON != "" {
		t.Error("expected empty receiptJSON for non-receipt response")
	}
	if fr.HTTPStatus != 500 {
		t.Errorf("expected HTTPStatus=500, got %d", fr.HTTPStatus)
	}
}

// A transport error yields a zero result so the kernel keeps the call pending for retry (§13).
// A plain (post-connect) error is NOT NotDispatched: the request may have executed remotely.
func TestExecuteFederationTransportError(t *testing.T) {
	fc := &fakeFedCaller{err: fmt.Errorf("unreachable")}
	fr, err := executeFederationOverTransport(context.Background(), fc, nil, "local", "peer", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-y", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fr.HTTPStatus != 0 || fr.ReceiptJSON != "" || fr.NotDispatched {
		t.Errorf("plain transport error should be pending (not NotDispatched), got %+v", fr)
	}
}

// A never-dispatched transport error (resolve/connect failed) sets NotDispatched so a first
// dispatch can fail fast (§13). The nil-transport executor is the same provably-never-sent case.
func TestExecuteFederationNotDispatched(t *testing.T) {
	fc := &fakeFedCaller{err: fmt.Errorf("%w: cannot resolve", fed.ErrNotDispatched)}
	fr, err := executeFederationOverTransport(context.Background(), fc, nil, "local", "peer", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-z", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fr.NotDispatched {
		t.Errorf("ErrNotDispatched should set NotDispatched, got %+v", fr)
	}

	// nil transport → provably never sent.
	e := &httpActionExecutor{}
	fr2, err := e.ExecuteFederation(context.Background(), "peer", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-w", map[string]any{})
	if err != nil {
		t.Fatalf("nil-transport ExecuteFederation: %v", err)
	}
	if !fr2.NotDispatched {
		t.Errorf("nil transport should set NotDispatched, got %+v", fr2)
	}
}

func TestHTTPActionExecutorSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"echo": in["msg"]})
	}))
	defer srv.Close()

	exec := &httpActionExecutor{}
	result, err := exec.Execute(context.Background(), &kernel.Action{Source: httpSrc(srv.URL, "POST")}, map[string]any{"msg": "hello"}, "", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result["echo"] != "hello" {
		t.Errorf("echo: got %v, want %q", result["echo"], "hello")
	}
}

func TestHTTPActionExecutorNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	exec := &httpActionExecutor{}
	_, err := exec.Execute(context.Background(), &kernel.Action{Source: httpSrc(srv.URL, "POST")}, map[string]any{}, "", "")
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestHTTPActionExecutorInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	exec := &httpActionExecutor{}
	_, err := exec.Execute(context.Background(), &kernel.Action{Source: httpSrc(srv.URL, "POST")}, map[string]any{}, "", "")
	if err == nil {
		t.Fatal("expected error for non-JSON response")
	}
}

// malformedBox decrypts to invalid JSON, simulating credentials that decrypt but cannot be parsed.
type malformedBox struct{}

func (malformedBox) Seal(aad, plaintext string) (string, error)  { return plaintext, nil }
func (malformedBox) Open(aad, ciphertext string) (string, error) { return "{not valid json", nil }

// TestHTTPActionAuthFailsClosed: auth_json that cannot be authentically decrypted and parsed must
// abort with ErrInvalidState and make no upstream request — never treated as plaintext (§8).
func TestHTTPActionAuthFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		box  kernel.SecretBox
	}{
		{"no_box", nil},                  // credentials present, no encryption configured
		{"undecryptable", erroringBox{}}, // wrong key / corrupt ciphertext
		{"malformed", malformedBox{}},    // decrypts to invalid JSON
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
				json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}))
			defer srv.Close()

			exec := &httpActionExecutor{auth: newAuthenticator(tc.box, nil, false, 0)}
			_, err := exec.Execute(context.Background(),
				&kernel.Action{Source: httpSrc(srv.URL, "POST"), AuthJSON: "x"}, map[string]any{}, "", "")
			if !errors.Is(err, kernel.ErrInvalidState) {
				t.Fatalf("got %v, want ErrInvalidState", err)
			}
			if hit {
				t.Error("upstream request made despite unusable credentials (must fail closed)")
			}
		})
	}
}

// TestHTTPActionValidAuthApplied: a valid bearer auth_json is applied to the request.
func TestHTTPActionValidAuthApplied(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	auth := kernel.AuthInput{Scheme: "bearer", Secrets: map[string]any{"token": "s3cret"}}
	authJSON, _ := json.Marshal(auth)
	box, err := newAESGCMBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("newAESGCMBox: %v", err)
	}
	action := &kernel.Action{ID: "act-auth", Source: httpSrc(srv.URL, "POST")}
	action.AuthJSON, err = box.Seal(action.ID, string(authJSON))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	exec := &httpActionExecutor{auth: newAuthenticator(box, nil, false, 0)}
	_, err = exec.Execute(context.Background(), action, map[string]any{}, "", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer s3cret")
	}
}

func TestExecuteOpenAPIGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		got := r.URL.Query().Get("name")
		json.NewEncoder(w).Encode(map[string]any{"echo": got})
	}))
	defer srv.Close()

	src := map[string]any{
		"type":     "openapi",
		"base_url": srv.URL,
		"method":   "GET",
		"path":     "/greet",
	}
	srcJSON, _ := json.Marshal(src)

	exec := &httpActionExecutor{}
	result, err := exec.Execute(context.Background(), &kernel.Action{Source: string(srcJSON)}, map[string]any{"name": "world"}, "", "")
	if err != nil {
		t.Fatalf("Execute OpenAPI GET: %v", err)
	}
	if result["echo"] != "world" {
		t.Errorf("echo: got %v, want %q", result["echo"], "world")
	}
}

func TestExecuteOpenAPIPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"received": body["msg"]})
	}))
	defer srv.Close()

	src := map[string]any{
		"type":     "openapi",
		"base_url": srv.URL,
		"method":   "POST",
		"path":     "/send",
	}
	srcJSON, _ := json.Marshal(src)

	exec := &httpActionExecutor{}
	result, err := exec.Execute(context.Background(), &kernel.Action{Source: string(srcJSON)}, map[string]any{"msg": "hello"}, "", "")
	if err != nil {
		t.Fatalf("Execute OpenAPI POST: %v", err)
	}
	if result["received"] != "hello" {
		t.Errorf("received: got %v, want %q", result["received"], "hello")
	}
}

func TestExecuteOpenAPIPathParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path should be /items/42 with query ?filter=active.
		path := r.URL.Path
		filter := r.URL.Query().Get("filter")
		json.NewEncoder(w).Encode(map[string]any{"path": path, "filter": filter})
	}))
	defer srv.Close()

	src := map[string]any{
		"type":     "openapi",
		"base_url": srv.URL,
		"method":   "GET",
		"path":     "/items/{id}",
	}
	srcJSON, _ := json.Marshal(src)

	exec := &httpActionExecutor{}
	result, err := exec.Execute(context.Background(), &kernel.Action{Source: string(srcJSON)}, map[string]any{"id": "42", "filter": "active"}, "", "")
	if err != nil {
		t.Fatalf("Execute OpenAPI path param: %v", err)
	}
	if result["path"] != "/items/42" {
		t.Errorf("path: got %v, want /items/42", result["path"])
	}
	if result["filter"] != "active" {
		t.Errorf("filter: got %v, want active", result["filter"])
	}
}

// TestExecuteOpenAPIPostQueryParam verifies that a POST operation with a declared query
// parameter sends it in the URL query string, not the request body (issue 8 regression).
func TestExecuteOpenAPIPostQueryParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{
			"query_param": r.URL.Query().Get("format"),
			"body_param":  body["data"],
		})
	}))
	defer srv.Close()

	// Source with explicit Params: "format" is a query param, "data" is a body param.
	src := map[string]any{
		"type":     "openapi",
		"base_url": srv.URL,
		"method":   "POST",
		"path":     "/upload",
		"params": []map[string]any{
			{"name": "format", "in": "query"},
			{"name": "data", "in": "body"},
		},
	}
	srcJSON, _ := json.Marshal(src)

	exec := &httpActionExecutor{}
	result, err := exec.Execute(context.Background(), &kernel.Action{Source: string(srcJSON)}, map[string]any{
		"format": "json",
		"data":   "hello",
	}, "", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result["query_param"] != "json" {
		t.Errorf("query_param: got %v, want %q", result["query_param"], "json")
	}
	if result["body_param"] != "hello" {
		t.Errorf("body_param: got %v, want %q", result["body_param"], "hello")
	}
}

// TestExecuteHTTPManualGet: a manual kind=http action with method GET routes
// otherwise-unbound args to the query string (implicit routing).
func TestExecuteHTTPManualGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"q": r.URL.Query().Get("q")})
	}))
	defer srv.Close()

	exec := &httpActionExecutor{}
	result, err := exec.Execute(context.Background(),
		&kernel.Action{Source: httpSrc(srv.URL, "GET")}, map[string]any{"q": "hi"}, "", "")
	if err != nil {
		t.Fatalf("Execute manual GET: %v", err)
	}
	if result["q"] != "hi" {
		t.Errorf("q: got %v, want %q", result["q"], "hi")
	}
}

// TestExecuteHTTPManualPathTemplate: a manual action whose URL carries a {name}
// placeholder substitutes it from args via implicit routing.
func TestExecuteHTTPManualPathTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path})
	}))
	defer srv.Close()

	exec := &httpActionExecutor{}
	result, err := exec.Execute(context.Background(),
		&kernel.Action{Source: httpSrc(srv.URL+"/items/{id}", "GET")}, map[string]any{"id": "42"}, "", "")
	if err != nil {
		t.Fatalf("Execute manual path template: %v", err)
	}
	if result["path"] != "/items/42" {
		t.Errorf("path: got %v, want /items/42", result["path"])
	}
}

// TestExecuteHTTPInvalidSource: a non-HTTPSource source is rejected (no sniffing).
func TestExecuteHTTPInvalidSource(t *testing.T) {
	exec := &httpActionExecutor{}
	_, err := exec.Execute(context.Background(),
		&kernel.Action{Source: "https://not-json.example.com"}, map[string]any{}, "", "")
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("got %v, want ErrInvalidState", err)
	}
}

func TestFetchWebSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "test-agent/1.0" {
			t.Errorf("User-Agent = %q, want test-agent/1.0", got)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "<html>ok</html>")
	}))
	defer srv.Close()

	// allowLocal=true so the httptest loopback server is reachable.
	exec := &httpActionExecutor{allowLocal: true}
	status, body, ct, finalURL, err := exec.fetchWeb(context.Background(), srv.URL, "test-agent/1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if string(body) != "<html>ok</html>" {
		t.Errorf("body = %q", string(body))
	}
	if ct != "text/html; charset=utf-8" {
		t.Errorf("content_type = %q", ct)
	}
	if finalURL != srv.URL {
		t.Errorf("final_url = %q, want %q", finalURL, srv.URL)
	}
}

func TestFetchWebReturnsNon2xxStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "nope")
	}))
	defer srv.Close()

	exec := &httpActionExecutor{allowLocal: true}
	status, body, _, _, err := exec.fetchWeb(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("non-2xx must not error, got %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if string(body) != "nope" {
		t.Errorf("body = %q", string(body))
	}
}

func TestFetchWebRejectsPrivateWhenLocalDisallowed(t *testing.T) {
	// allowLocal defaults to false: a private/LAN address must be rejected at parse time.
	// (Loopback is permitted by default, so it is no longer rejected here.)
	exec := &httpActionExecutor{}
	_, _, _, _, err := exec.fetchWeb(context.Background(), "http://192.168.1.1/", "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
}

func TestFetchWebRejectsNonHTTPScheme(t *testing.T) {
	exec := &httpActionExecutor{allowLocal: true}
	_, _, _, _, err := exec.fetchWeb(context.Background(), "file:///etc/passwd", "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
}

func TestFetchWebSchemelessDefaultsToHTTPS(t *testing.T) {
	// A scheme-less URL is upgraded to https before the SSRF guard runs. Pointing it
	// at a private host with allowLocal=false proves the https:// prefix was applied:
	// the guard rejects the resolved private address rather than failing to parse.
	exec := &httpActionExecutor{}
	_, _, _, _, err := exec.fetchWeb(context.Background(), "192.168.1.1/path", "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput (guard on https-upgraded private host)", err)
	}
}

func TestDefaultScheme(t *testing.T) {
	cases := map[string]string{
		"example.com":          "https://example.com",
		"example.com/wiki/X":   "https://example.com/wiki/X",
		"en.wikipedia.org:443": "https://en.wikipedia.org:443",
		"//example.com":        "https://example.com",
		"http://example.com":   "http://example.com",  // explicit http respected, not upgraded
		"https://example.com":  "https://example.com", // unchanged
		"ftp://example.com":    "ftp://example.com",   // scheme present; rejected later by validatePublicURL
		"  example.com  ":      "https://example.com", // trimmed
		"":                     "",
	}
	for in, want := range cases {
		if got := defaultScheme(in); got != want {
			t.Errorf("defaultScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExecuteOAuthClientCredentials: an http action with the client-credentials scheme fetches a
// bearer from the token endpoint and applies it to the upstream request (§8).
func TestExecuteOAuthClientCredentials(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_secret") != "s3cret" {
			http.Error(w, "bad", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "cc-tok", "expires_in": 3600})
	}))
	defer provider.Close()

	var sawAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()

	box, _ := newAESGCMBox(make([]byte, 32))
	auth := kernel.AuthInput{
		Scheme:  kernel.AuthSchemeOAuthClientCreds,
		Config:  map[string]any{"token_url": provider.URL, "client_id": "c"},
		Secrets: map[string]any{"client_secret": "s3cret"},
	}
	authJSON, _ := json.Marshal(auth)
	action := &kernel.Action{ID: "act-cc", Source: httpSrc(upstream.URL, "POST")}
	action.AuthJSON, _ = box.Seal(action.ID, string(authJSON))

	exec := &httpActionExecutor{allowLocal: true, auth: newAuthenticator(box, newFakeGrantStore(), true, 0)}
	if _, err := exec.Execute(context.Background(), action, map[string]any{}, "", ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if sawAuth != "Bearer cc-tok" {
		t.Errorf("upstream Authorization = %q, want Bearer cc-tok", sawAuth)
	}
}

// TestExecuteOAuth401RefreshRetry: a stale cached token yields a 401; the executor forces one
// refresh and retries, and the second (fresh) token succeeds (§8).
func TestExecuteOAuth401RefreshRetry(t *testing.T) {
	var issued int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issued++
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("cc-%d", issued), "expires_in": 3600})
	}))
	defer provider.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the second-issued token is accepted; the first always 401s.
		if r.Header.Get("Authorization") == "Bearer cc-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()

	box, _ := newAESGCMBox(make([]byte, 32))
	auth := kernel.AuthInput{
		Scheme:  kernel.AuthSchemeOAuthClientCreds,
		Config:  map[string]any{"token_url": provider.URL, "client_id": "c"},
		Secrets: map[string]any{"client_secret": "s"},
	}
	authJSON, _ := json.Marshal(auth)
	action := &kernel.Action{ID: "act-401", Source: httpSrc(upstream.URL, "POST")}
	action.AuthJSON, _ = box.Seal(action.ID, string(authJSON))

	exec := &httpActionExecutor{allowLocal: true, auth: newAuthenticator(box, newFakeGrantStore(), true, 0)}
	if _, err := exec.Execute(context.Background(), action, map[string]any{}, "", ""); err != nil {
		t.Fatalf("Execute with 401 retry: %v", err)
	}
	if issued < 2 {
		t.Errorf("expected a token refresh on 401; provider issued %d tokens", issued)
	}
}

// TestExecuteInjectsCapabilityHeaders proves the capability + callback headers ride on the
// dispatch request when a callback URL is configured, and are absent otherwise (§9, C2).
func TestExecuteInjectsCapabilityHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	exec := &httpActionExecutor{allowLocal: true, callbackURL: "http://cb.example:9999"}
	if _, err := exec.Execute(context.Background(),
		&kernel.Action{Source: httpSrc(srv.URL, "POST")}, map[string]any{}, "", "captoken.sig"); err != nil {
		t.Fatal(err)
	}
	if got.Get(capabilityHeader) != "captoken.sig" {
		t.Errorf("capability header = %q, want captoken.sig", got.Get(capabilityHeader))
	}
	if got.Get(callbackHeader) != "http://cb.example:9999" {
		t.Errorf("callback header = %q", got.Get(callbackHeader))
	}

	// With no callback URL, a leaf endpoint receives neither header.
	exec2 := &httpActionExecutor{allowLocal: true}
	if _, err := exec2.Execute(context.Background(),
		&kernel.Action{Source: httpSrc(srv.URL, "POST")}, map[string]any{}, "", "captoken.sig"); err != nil {
		t.Fatal(err)
	}
	if got.Get(capabilityHeader) != "" || got.Get(callbackHeader) != "" {
		t.Errorf("headers sent with no callback URL: cap=%q cb=%q", got.Get(capabilityHeader), got.Get(callbackHeader))
	}
}

// TestCapabilityHeaderStrippedOnCrossHostRedirect proves the capability header does not follow a
// host-changing redirect (§9, C2) — Go strips Authorization automatically but not our headers.
func TestCapabilityHeaderStrippedOnCrossHostRedirect(t *testing.T) {
	var endHeaders http.Header
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/end" {
			endHeaders = r.Header.Clone()
			w.Write([]byte(`{}`))
			return
		}
		// Redirect to the same server via a different hostname (localhost) → a host change.
		http.Redirect(w, r, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)+"/end", http.StatusFound)
	}))
	defer srv.Close()

	exec := &httpActionExecutor{allowLocal: true, callbackURL: "http://cb:1"}
	if _, err := exec.Execute(context.Background(),
		&kernel.Action{Source: httpSrc(srv.URL+"/start", "GET")}, map[string]any{}, "", "captoken.sig"); err != nil {
		t.Fatal(err)
	}
	if endHeaders.Get(capabilityHeader) != "" {
		t.Errorf("capability leaked across host-changing redirect: %q", endHeaders.Get(capabilityHeader))
	}
}
