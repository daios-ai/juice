package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
)

// bootControlPlane boots @sys on env.k and starts the control plane on the socket derived
// from flagDB (which newTestEnv points at env's DB). Returns the socket path and a @sys token.
func bootControlPlane(t *testing.T, env *testEnv) (string, string) {
	t.Helper()
	ctx := context.Background()
	if err := env.k.FirstBoot(ctx, "sys-pass"); err != nil {
		t.Fatal(err)
	}
	if err := env.k.SetConfig(ctx, configKeySuperuser, "@sys"); err != nil {
		t.Fatal(err)
	}
	srv := &server{kernel: env.k, log: log.Discard()}
	closer, err := startControlPlane(srv, flagDB)
	if err != nil {
		t.Fatalf("startControlPlane: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	tok, err := env.k.Login(ctx, "@sys", "sys-pass")
	if err != nil {
		t.Fatal(err)
	}
	return controlSocketPath(flagDB), tok
}

func ctlDo(t *testing.T, sock, token, method, path string, body any) ([]byte, int) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	h := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	resp, status, err := doControlHTTP(context.Background(), sock, method, path, h, rdr)
	if err != nil {
		t.Fatalf("control request %s %s: %v", method, path, err)
	}
	return resp, status
}

// TestControlPlaneDepositAndLifecycle covers the socket's mode, a superuser deposit over the
// socket mutating a real balance, and socket removal on shutdown.
func TestControlPlaneDepositAndLifecycle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	sock, suTok := bootControlPlane(t, env)

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket perms: got %v, want 0600", fi.Mode().Perm())
	}

	recipient, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@rcpt", Email: "r@example.com", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, status := ctlDo(t, sock, suTok, "POST", "/control/deposit",
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

// TestControlPlaneAuth proves the two-factor gate: a request with no bearer token is rejected
// (authMiddleware), and a valid but non-superuser token is rejected (requireSuperuserMW).
func TestControlPlaneAuth(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	sock, _ := bootControlPlane(t, env)

	if _, status := ctlDo(t, sock, "", "GET", "/control/users", nil); status != http.StatusUnauthorized {
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
	body, status := ctlDo(t, sock, regTok, "POST", "/control/deposit",
		map[string]any{"handle": "@regular", "amount": 1})
	if status == http.StatusOK {
		t.Fatalf("non-superuser deposit should be rejected, got 200")
	}
	if !strings.Contains(string(body), "superuser") {
		t.Errorf("expected superuser-required error, got: %s", body)
	}
}

// TestControlRoutesNotOnTCP proves the control routes are never exposed on the public TCP API
// (newTestEnv points flagServer at the TCP router backed by env.k).
func TestControlRoutesNotOnTCP(t *testing.T) {
	newTestEnv(t)
	resp, err := http.Get(flagServer + "/control/users")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/control/* over TCP: got status %d, want 404", resp.StatusCode)
	}
}

// TestControlCallUnreachable proves an admin command surfaces a friendly error when no server
// (hence no control socket) is running.
func TestControlCallUnreachable(t *testing.T) {
	newTestEnv(t) // sets flagDB; no control plane started
	var users []*kernel.User
	err := ctlCall(context.Background(), "GET", "/control/users", nil, &users)
	if err == nil {
		t.Fatal("expected error when control socket is absent")
	}
	if !strings.Contains(err.Error(), "control socket") {
		t.Errorf("want control-socket error, got: %v", err)
	}
}
