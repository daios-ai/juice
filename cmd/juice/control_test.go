package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// bootSuperuser first-boots @sys on env.k, marks it the configured superuser, and returns a
// @sys bearer token. The superuser verbs are ordinary TCP routes now (gated by
// requireSuperuserMW), so tests drive them against flagServer like any other endpoint.
func bootSuperuser(t *testing.T, env *testEnv) string {
	t.Helper()
	ctx := context.Background()
	if err := env.k.FirstBoot(ctx, "sys-pass"); err != nil {
		t.Fatal(err)
	}
	if err := env.k.SetConfig(ctx, configKeySuperuser, "@sys"); err != nil {
		t.Fatal(err)
	}
	tok, err := env.k.Login(ctx, "@sys", "sys-pass")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// tcpDo sends a request to the test TCP server (flagServer), attaching the bearer token when set.
func tcpDo(t *testing.T, token, method, path string, body any) ([]byte, int) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, flagServer+path, rdr)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode
}

// TestAdminDepositOverTCP: a superuser deposit over the public TCP API mutates a real balance.
func TestAdminDepositOverTCP(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	suTok := bootSuperuser(t, env)

	recipient, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@rcpt", Email: "r@example.com", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, status := tcpDo(t, suTok, "POST", "/control/deposit",
		map[string]any{"handle": "@rcpt", "amount": 500})
	if status != http.StatusOK {
		t.Fatalf("deposit status %d: %s", status, body)
	}
	u, err := env.k.ReadUser(ctx, recipient.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Available != 500 {
		t.Errorf("available after deposit: got %d, want 500", u.Available)
	}
}

// TestAdminDepositByKey: a peer is funded by its base64url public key (the global name it was
// friended with), not just its local @handle — the out-of-band settlement path.
func TestAdminDepositByKey(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	suTok := bootSuperuser(t, env)

	sys, err := env.k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := env.k.AddPeer(ctx, sys.ID, "@peerx", keyB64)
	if err != nil {
		t.Fatal(err)
	}

	// Address the peer by key, not by @handle.
	body, status := tcpDo(t, suTok, "POST", "/control/deposit",
		map[string]any{"handle": keyB64, "amount": 300})
	if status != http.StatusOK {
		t.Fatalf("deposit-by-key status %d: %s", status, body)
	}
	u, err := env.k.ReadUser(ctx, peer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Available != 300 {
		t.Errorf("peer balance after deposit-by-key: got %d, want 300", u.Available)
	}
}

// TestAdminSuperuserGate proves the two-factor gate on the TCP routes: no bearer token is
// rejected (authMiddleware), and a valid but non-superuser token is rejected (requireSuperuserMW).
func TestAdminSuperuserGate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	_ = bootSuperuser(t, env)

	if _, status := tcpDo(t, "", "GET", "/control/users", nil); status != http.StatusUnauthorized {
		t.Errorf("no token: got status %d, want 401", status)
	}

	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@regular", Email: "reg@example.com", Password: "pw",
	}); err != nil {
		t.Fatal(err)
	}
	regTok, err := env.k.Login(ctx, "@regular", "pw")
	if err != nil {
		t.Fatal(err)
	}
	body, status := tcpDo(t, regTok, "POST", "/control/deposit",
		map[string]any{"handle": "@regular", "amount": 1})
	if status == http.StatusOK {
		t.Fatalf("non-superuser deposit should be rejected, got 200")
	}
	if !strings.Contains(string(body), "superuser") {
		t.Errorf("expected superuser-required error, got: %s", body)
	}
}
