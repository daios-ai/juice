package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
