package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	logger := log.Discard()
	k := kernel.New(db, nil, nil, cfg, logger)

	srv := &server{kernel: k, log: logger}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestIDMiddleware)

	r.Post("/v1/auth/token", srv.postTokenMulti)
	r.Post("/v1/users", srv.postUser)
	r.Group(func(r chi.Router) {
		r.Use(srv.authMiddleware)
		r.Get("/v1/actions", srv.getActions)
		r.Post("/v1/actions", srv.postAction)
		r.Get("/v1/actions/{id}", srv.getAction)
		r.Post("/v1/actions/{id}/enable", srv.enableAction)
		r.Post("/v1/actions/{id}/disable", srv.disableAction)
		r.Delete("/v1/actions/{id}", srv.deleteAction)
		r.Post("/v1/processes", srv.postProcess)
		r.Get("/v1/processes/{id}", srv.getProcess)
		r.Post("/v1/processes/{id}/fund", srv.fundProcess)
		r.Post("/v1/processes/{id}/end", srv.endProcess)
		r.Post("/v1/call", srv.postCall)
		r.Get("/v1/transactions", srv.listTransactions)
		r.Get("/v1/transactions/{id}", srv.getTransaction)
		r.Get("/v1/stats/{action_id}", srv.getStats)
		r.Post("/v1/listeners", srv.postListener)
		r.Delete("/v1/listeners/{id}", srv.deleteListener)
		r.Post("/v1/events/emit", srv.postEmit)
		r.Get("/v1/processes/{id}/feedback/{trace_id}", srv.getProcessFeedback)
	})

	return httptest.NewServer(r), k
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

func decodeResponse(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
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

func TestServeAuthRequired(t *testing.T) {
	srv, _ := newTestHTTPServer(t)
	defer srv.Close()

	resp := httpDo(t, srv, "GET", "/v1/actions", nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", resp.StatusCode)
	}
}

func TestServeCreateAndGetAction(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	user, _ := k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "@srv-actowner", Email: "sa@example.com", Password: "pass",
	})
	tok, _ := k.Login(context.Background(), "@srv-actowner", "pass")

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
	if action.OwnerUserID != user.ID {
		t.Errorf("action owner: got %q, want %q", action.OwnerUserID, user.ID)
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

func TestServeProcessLifecycle(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "@srv-proc", Email: "sp@example.com", Password: "pass",
	})
	tok, _ := k.Login(context.Background(), "@srv-proc", "pass")

	resp := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, tok)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create process: expected 201, got %d", resp.StatusCode)
	}
	var proc map[string]any
	decodeResponse(t, resp, &proc)

	pid, ok := proc["process_id"].(string)
	if !ok || pid == "" {
		t.Fatal("expected process_id in response")
	}

	resp2 := httpDo(t, srv, "POST", "/v1/processes/"+pid+"/end", nil, tok)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Errorf("end process: expected 204, got %d", resp2.StatusCode)
	}
}

func TestServeRateTransactionNotFound(t *testing.T) {
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "@rater", Email: "rater@example.com", Password: "pass",
	})
	tok, _ := k.Login(context.Background(), "@rater", "pass")

	resp := httpDo(t, srv, "POST", "/v1/transactions/no-such-id/rate",
		map[string]any{"rating": 1.0}, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown tx, got %d", resp.StatusCode)
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
