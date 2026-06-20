package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// erroringBox is a SecretBox whose Open always fails, simulating an unreadable auth_json
// (wrong key, key rotation, corrupted ciphertext).
type erroringBox struct{}

func (erroringBox) Seal(aad, plaintext string) (string, error)  { return plaintext, nil }
func (erroringBox) Open(aad, ciphertext string) (string, error) { return "", fmt.Errorf("decrypt failed") }

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
	blocked := []string{"127.0.0.1", "::1", "192.168.1.1", "10.0.0.1", "172.16.0.1", "169.254.1.1"}
	for _, ip := range blocked {
		if err := validateResolvedIP(ip); err == nil {
			t.Errorf("validateResolvedIP(%q): expected error, got nil", ip)
		}
	}
}

func TestValidateResolvedIPAllowed(t *testing.T) {
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34"}
	for _, ip := range allowed {
		if err := validateResolvedIP(ip); err != nil {
			t.Errorf("validateResolvedIP(%q): unexpected error: %v", ip, err)
		}
	}
}

func TestValidateRedirectHostBlocked(t *testing.T) {
	cases := []string{"localhost", "127.0.0.1", "::1", "192.168.1.1", "10.0.0.1", "169.254.1.1"}
	for _, h := range cases {
		if err := validateRedirectHost(h, false); err == nil {
			t.Errorf("validateRedirectHost(%q, false): expected error, got nil", h)
		}
	}
}

func TestValidateRedirectHostAllowed(t *testing.T) {
	cases := []string{"localhost", "127.0.0.1", "10.0.0.1"}
	for _, h := range cases {
		if err := validateRedirectHost(h, true); err != nil {
			t.Errorf("validateRedirectHost(%q, true): unexpected error: %v", h, err)
		}
	}
}

func TestExecuteFederationSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Idempotency-Key") == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result":  map[string]any{"ok": true},
			"receipt": map[string]any{"id": "r1", "status": "success"},
		})
	}))
	defer srv.Close()

	exec := &httpActionExecutor{}
	fr, err := exec.ExecuteFederation(context.Background(), srv.URL, "key-123", map[string]any{})
	if err != nil {
		t.Fatalf("ExecuteFederation: %v", err)
	}
	if fr.Result["ok"] != true {
		t.Errorf("result: got %v, want ok:true", fr.Result)
	}
	if fr.ReceiptJSON == "" {
		t.Error("expected non-empty receiptJSON")
	}
}

func TestExecuteFederationNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	exec := &httpActionExecutor{}
	fr, err := exec.ExecuteFederation(context.Background(), srv.URL, "key-x", map[string]any{})
	if err != nil {
		t.Fatalf("ExecuteFederation: unexpected error: %v", err)
	}
	if fr.ReceiptJSON != "" {
		t.Error("expected empty receiptJSON for non-JSON non-200 response")
	}
	if fr.HTTPStatus != 500 {
		t.Errorf("expected HTTPStatus=500, got %d", fr.HTTPStatus)
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
	result, err := exec.Execute(context.Background(), &kernel.Action{Source: httpSrc(srv.URL, "POST")}, map[string]any{"msg": "hello"})
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
	_, err := exec.Execute(context.Background(), &kernel.Action{Source: httpSrc(srv.URL, "POST")}, map[string]any{})
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
	_, err := exec.Execute(context.Background(), &kernel.Action{Source: httpSrc(srv.URL, "POST")}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for non-JSON response")
	}
}

// TestHTTPActionAuthDecryptFailsClosed: undecryptable auth_json must abort with
// ErrInvalidState and make no upstream request (fail closed, not unauthenticated).
func TestHTTPActionAuthDecryptFailsClosed(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	exec := &httpActionExecutor{secretBox: erroringBox{}}
	_, err := exec.Execute(context.Background(),
		&kernel.Action{Source: httpSrc(srv.URL, "POST"), AuthJSON: "unreadable-ciphertext"}, map[string]any{})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("got %v, want ErrInvalidState", err)
	}
	if hit {
		t.Error("upstream request was made despite unusable credentials (must fail closed)")
	}
}

// TestHTTPActionAuthParseFailsClosed: malformed plaintext auth_json (no SecretBox) must
// abort with ErrInvalidState before any upstream request.
func TestHTTPActionAuthParseFailsClosed(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	exec := &httpActionExecutor{} // box nil → auth_json treated as plaintext
	_, err := exec.Execute(context.Background(),
		&kernel.Action{Source: httpSrc(srv.URL, "POST"), AuthJSON: "{not valid json"}, map[string]any{})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("got %v, want ErrInvalidState", err)
	}
	if hit {
		t.Error("upstream request was made despite unparseable credentials (must fail closed)")
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
	exec := &httpActionExecutor{} // box nil → plaintext auth_json
	_, err := exec.Execute(context.Background(),
		&kernel.Action{Source: httpSrc(srv.URL, "POST"), AuthJSON: string(authJSON)}, map[string]any{})
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
	result, err := exec.Execute(context.Background(), &kernel.Action{Source: string(srcJSON)}, map[string]any{"name": "world"})
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
	result, err := exec.Execute(context.Background(), &kernel.Action{Source: string(srcJSON)}, map[string]any{"msg": "hello"})
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
	result, err := exec.Execute(context.Background(), &kernel.Action{Source: string(srcJSON)}, map[string]any{"id": "42", "filter": "active"})
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
	})
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
		&kernel.Action{Source: httpSrc(srv.URL, "GET")}, map[string]any{"q": "hi"})
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
		&kernel.Action{Source: httpSrc(srv.URL+"/items/{id}", "GET")}, map[string]any{"id": "42"})
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
		&kernel.Action{Source: "https://not-json.example.com"}, map[string]any{})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("got %v, want ErrInvalidState", err)
	}
}
