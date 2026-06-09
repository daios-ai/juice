package main

import (
	"crypto/ed25519"
	"crypto/rand"
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
)

func remoteTestPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(pub)
}

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
	k := kernel.New(db, nil, nil, nil, cfg, log.Discard())

	if err := k.FirstBoot(t.Context(), "sys-pass"); err != nil {
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
			"public_key": remoteTestPublicKey(t),
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

	err = runRemoteAdd(remote.URL)
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
	if _, err := base64.RawURLEncoding.DecodeString(u.PublicKey); err != nil {
		t.Errorf("PublicKey should be base64url: %v", err)
	}
}

func TestRemoteList(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	sys, err := k.ReadUserByHandle(t.Context(), "@sys")
	if err != nil {
		t.Fatal(err)
	}

	// Register a remote kernel directly via kernel API.
	if _, err := k.RegisterRemoteKernel(t.Context(), sys.ID, "@list-remote", remoteTestPublicKey(t), "https://list.example.com"); err != nil {
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

	// Generate a real signing keypair for the "remote" kernel.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	const actionID = "action-remote-id"
	m := kernel.ActionManifest{
		ActionID:     actionID,
		OwnerHandle:  "@import-remote",
		Name:         "greet",
		Description:  "says hello",
		Kind:         kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	// Stand up a fake remote kernel that serves action list and signed manifest.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			json.NewEncoder(w).Encode([]map[string]string{{"ID": actionID, "Name": "greet"}})
		}
	}))
	defer remote.Close()

	sys, err := k.ReadUserByHandle(t.Context(), "@sys")
	if err != nil {
		t.Fatal(err)
	}

	// Register the remote kernel with the real public key.
	if _, err := k.RegisterRemoteKernel(t.Context(), sys.ID, "@import-remote", pubB64, remote.URL); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	tok, _ := k.Login(t.Context(), "@sys", "sys-pass")
	_ = saveToken(tok)

	if err := runRemoteImport("@import-remote", "greet"); err != nil {
		t.Fatalf("runRemoteImport: %v", err)
	}

	// Imported action should be findable in the full action list.
	actions, err := k.ListAllActions(t.Context(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range actions {
		if a.Name == "greet" {
			found = true
		}
	}
	if !found {
		t.Error("expected imported action /greet to appear in @import-remote's actions")
	}
}

func TestRemoteImportDisappearedDeactivatesProxy(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	const actionID = "disappear-action-id"
	m := kernel.ActionManifest{
		ActionID:     actionID,
		OwnerHandle:  "@disappear-remote",
		Name:         "bye",
		Description:  "going away",
		Kind:         kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	// First: remote server returns the action.
	serveAction := true
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/manifest") {
			json.NewEncoder(w).Encode(m)
			return
		}
		if serveAction {
			json.NewEncoder(w).Encode([]map[string]string{{"ID": actionID, "Name": "bye"}})
		} else {
			json.NewEncoder(w).Encode([]map[string]string{})
		}
	}))
	defer remote.Close()

	sys, err := k.ReadUserByHandle(t.Context(), "@sys")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.RegisterRemoteKernel(t.Context(), sys.ID, "@disappear-remote", pubB64, remote.URL); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	tok, _ := k.Login(t.Context(), "@sys", "sys-pass")
	_ = saveToken(tok)

	// Import the action.
	if err := runRemoteImport("@disappear-remote", "bye"); err != nil {
		t.Fatalf("initial import: %v", err)
	}

	// Reimport with remote no longer listing the action.
	serveAction = false
	if err := runRemoteImport("@disappear-remote", "bye"); err != nil {
		t.Fatalf("reimport after disappearance: %v", err)
	}

	// Local proxy must be deactivated.
	actions, err := k.ListAllActions(t.Context(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.Name == "bye" && a.Active {
			t.Error("expected local proxy to be deactivated after remote action disappeared")
		}
	}
}

func TestRemoteUnimport(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	sys, err := k.ReadUserByHandle(t.Context(), "@sys")
	if err != nil {
		t.Fatal(err)
	}
	remoteUser, err := k.RegisterRemoteKernel(t.Context(), sys.ID, "@unimport-peer", pubB64, "https://unimport.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "unimport-action-id",
		OwnerHandle:  "@unimport-peer",
		Name:         "greet",
		Kind:         kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	if _, err := k.ImportRemoteAction(t.Context(), sys.ID, remoteUser.ID, m); err != nil {
		t.Fatalf("ImportRemoteAction: %v", err)
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	tok, _ := k.Login(t.Context(), "@sys", "sys-pass")
	_ = saveToken(tok)

	if err := runRemoteUnimport("@unimport-peer", "greet"); err != nil {
		t.Fatalf("runRemoteUnimport: %v", err)
	}
}

