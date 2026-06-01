package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	result, receiptJSON, err := exec.ExecuteFederation(context.Background(), srv.URL, "key-123", map[string]any{})
	if err != nil {
		t.Fatalf("ExecuteFederation: %v", err)
	}
	if result["ok"] != true {
		t.Errorf("result: got %v, want ok:true", result)
	}
	if receiptJSON == "" {
		t.Error("expected non-empty receiptJSON")
	}
}

func TestExecuteFederationNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	exec := &httpActionExecutor{}
	_, _, err := exec.ExecuteFederation(context.Background(), srv.URL, "key-x", map[string]any{})
	if err == nil {
		t.Fatal("expected error for non-200 response")
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
	result, err := exec.Execute(context.Background(), srv.URL, map[string]any{"msg": "hello"})
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
	_, err := exec.Execute(context.Background(), srv.URL, map[string]any{})
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
	_, err := exec.Execute(context.Background(), srv.URL, map[string]any{})
	if err == nil {
		t.Fatal("expected error for non-JSON response")
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
		"type":    "openapi",
		"base_url": srv.URL,
		"method":  "GET",
		"path":    "/greet",
	}
	srcJSON, _ := json.Marshal(src)

	exec := &httpActionExecutor{}
	result, err := exec.Execute(context.Background(), string(srcJSON), map[string]any{"name": "world"})
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
		"type":    "openapi",
		"base_url": srv.URL,
		"method":  "POST",
		"path":    "/send",
	}
	srcJSON, _ := json.Marshal(src)

	exec := &httpActionExecutor{}
	result, err := exec.Execute(context.Background(), string(srcJSON), map[string]any{"msg": "hello"})
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
	result, err := exec.Execute(context.Background(), string(srcJSON), map[string]any{"id": "42", "filter": "active"})
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
	result, err := exec.Execute(context.Background(), string(srcJSON), map[string]any{
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
