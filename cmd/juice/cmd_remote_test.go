package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
)

// newRemoteTestKernel opens a fresh DB, bootstraps @sys, and returns the kernel.
func newRemoteTestKernel(t *testing.T) (*kernel.Kernel, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "remote_test.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	t.Setenv("JUICE_SECRET_KEY", "remote-test-secret")

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "remote-test-secret"
	k := kernel.New(db, nil, nil, nil, nil, cfg, log.Discard())

	if _, err := k.BootstrapSuperuser(t.Context(), kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "sys-pass",
	}, "superuser_handle"); err != nil {
		t.Fatal(err)
	}

	// Point flagDB at this DB so openKernel() in CLI handlers finds it.
	origDB := flagDB
	flagDB = dbPath
	t.Cleanup(func() { flagDB = origDB })

	return k, db
}

func TestRemoteAdd(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	// Stand up a fake remote kernel that serves /.well-known/juice-kernel.json.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/juice-kernel.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"handle":     "@remote-node",
			"public_key": "pubkey-abc",
			"base_url":   "",
		})
	}))
	defer remote.Close()

	// Log in as @sys to save a token for requireSuperuser.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	tok, err := k.Login(t.Context(), "@sys", "sys-pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	err = runRemoteAdd(nil, []string{remote.URL})
	if err != nil {
		t.Fatalf("runRemoteAdd: %v", err)
	}

	// Handle is derived from the URL host, not the remote's self-reported handle.
	parsed, _ := url.Parse(remote.URL)
	expectedHandle := "@" + parsed.Host
	u, err := k.ReadUserByHandle(t.Context(), expectedHandle)
	if err != nil {
		t.Fatalf("ReadUserByHandle %s: %v", expectedHandle, err)
	}
	if u.RemoteBaseURL == "" {
		t.Error("expected RemoteBaseURL to be set")
	}
	if u.PublicKey != "pubkey-abc" {
		t.Errorf("PublicKey: got %q, want %q", u.PublicKey, "pubkey-abc")
	}
}

func TestRemoteList(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	// Register a remote kernel directly via kernel API.
	if _, err := k.RegisterRemoteKernel(t.Context(), "@list-remote", "pk-xyz", "https://list.example.com"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	tok, _ := k.Login(t.Context(), "@sys", "sys-pass")
	_ = saveToken(tok)

	if err := runRemoteList(nil, nil); err != nil {
		t.Fatalf("runRemoteList: %v", err)
	}
}

func TestRemoteImport(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	// Stand up a fake remote kernel that serves an action list.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]any{{
			"id":          "action-remote-id",
			"name":        "/greet",
			"description": "says hello",
			"price":       0,
			"kind":        "http",
			"input_schema":  map[string]any{"type": "object"},
			"output_schema": map[string]any{"type": "object"},
		}})
	}))
	defer remote.Close()

	// Register the remote kernel.
	if _, err := k.RegisterRemoteKernel(t.Context(), "@import-remote", "pk-import", remote.URL); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	tok, _ := k.Login(t.Context(), "@sys", "sys-pass")
	_ = saveToken(tok)

	if err := runRemoteImport(nil, []string{"@import-remote", "/greet"}); err != nil {
		t.Fatalf("runRemoteImport: %v", err)
	}

	// Imported action should be findable in the full action list.
	actions, err := k.ListActions(t.Context(), false, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range actions {
		if a.Name == "/greet" {
			found = true
		}
	}
	if !found {
		t.Error("expected imported action /greet to appear in @import-remote's actions")
	}
}
