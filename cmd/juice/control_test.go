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
	if err := env.k.FirstBoot(ctx, "sys-pass", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.k.SetConfig(ctx, configKeySuperuser, "sys"); err != nil {
		t.Fatal(err)
	}
	tok, err := loginTokenFor(env.k, ctx, "sys", "sys-pass")
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
		Handle: "rcpt", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, status := tcpDo(t, suTok, "POST", "/control/deposit",
		map[string]any{"handle": "rcpt", "amount": 500, "ref": "test-payment"})
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
	// Recording the same payment again moves nothing and answers with the entry that recorded it —
	// a reply, not a crash: the handler renders whatever the kernel returns, so the kernel must
	// return something.
	body2, status2 := tcpDo(t, suTok, "POST", "/control/deposit",
		map[string]any{"handle": "rcpt", "amount": 500, "ref": "test-payment"})
	if status2 != http.StatusOK {
		t.Fatalf("replayed deposit: status %d: %s", status2, body2)
	}
	if strings.TrimSpace(string(body2)) == "" || string(body2) != string(body) {
		t.Errorf("replay must answer with the same entry:\n first  %s\n second %s", body, body2)
	}
	if u, _ := env.k.ReadUser(ctx, recipient.ID); u.Available != 500 {
		t.Errorf("replay moved money: %d", u.Available)
	}
}

// TestPeerRosterIsAPlainArrayWhenEmpty: a list with nothing in it is `[]`, never `null` (API.md
// R6). The roster was the one list that reached the wire as a nil slice.
func TestPeerRosterIsAPlainArrayWhenEmpty(t *testing.T) {
	env := newTestEnv(t)
	suTok := bootSuperuser(t, env)
	body, status := tcpDo(t, suTok, "GET", "/control/peers", nil)
	if status != http.StatusOK {
		t.Fatalf("peers: status %d: %s", status, body)
	}
	if got := strings.TrimSpace(string(body)); got != "[]" {
		t.Fatalf("empty roster: got %s, want []", got)
	}
}

// TestAdminDepositToAPeerIsRefused: a peer account is identity, never a wallet (P10, D14). The only
// money that may reach one is the payment closing an obligation it owes, and that money is credited
// to the seller. Naming the peer itself must be refused rather than preloading a balance no path
// would ever spend — the last way the retired peer-wallet economy could still be reached.
func TestAdminDepositToAPeerIsRefused(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	suTok := bootSuperuser(t, env)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := env.k.EnsureKernelAccount(ctx, keyB64)
	if err != nil {
		t.Fatal(err)
	}

	// Addressed by key or by petname, and with or without an amount, it is the same refusal.
	body, status := tcpDo(t, suTok, "POST", "/control/deposit",
		map[string]any{"handle": keyB64, "amount": 300, "ref": "test-payment"})
	if status == http.StatusOK {
		t.Fatalf("a bare deposit to a peer was accepted: %s", body)
	}
	u, err := env.k.ReadUser(ctx, peer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Available != 0 || u.Locked != 0 {
		t.Errorf("peer row after the refusal: %d/%d, want 0/0 — a peer account holds no money on any path",
			u.Available, u.Locked)
	}
}

// TestAdminRenameOverTCP: a superuser rename over the public TCP API vacates the old handle,
// which a fresh account then reuses.
func TestAdminRenameOverTCP(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	suTok := bootSuperuser(t, env)

	bob, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "bob", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, status := tcpDo(t, suTok, "POST", "/control/users/bob/rename",
		map[string]any{"new_name": "bob-retired"})
	if status != http.StatusOK {
		t.Fatalf("rename status %d: %s", status, body)
	}
	if got, err := env.k.ReadUserByHandle(ctx, "bob-retired"); err != nil || got.ID != bob.ID {
		t.Errorf("renamed handle does not resolve to bob: %v", err)
	}
	// The freed @bob is reusable by a distinct fresh account.
	fresh, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "bob", Password: "pw",
	})
	if err != nil {
		t.Fatalf("reuse freed handle: %v", err)
	}
	if fresh.ID == bob.ID {
		t.Errorf("reused handle must be a distinct account")
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
		Handle: "regular", Password: "pw",
	}); err != nil {
		t.Fatal(err)
	}
	regTok, err := loginTokenFor(env.k, ctx, "regular", "pw")
	if err != nil {
		t.Fatal(err)
	}
	body, status := tcpDo(t, regTok, "POST", "/control/deposit",
		map[string]any{"handle": "regular", "amount": 1, "ref": "test-payment"})
	if status == http.StatusOK {
		t.Fatalf("non-superuser deposit should be rejected, got 200")
	}
	if !strings.Contains(string(body), "superuser") {
		t.Errorf("expected superuser-required error, got: %s", body)
	}
}

// A user is addressed by handle, never by an id: only GET /v1/me answers with the caller's own
// (D20). The operator's own routes were writing the account row verbatim.
func TestOperatorRoutesWithholdAccountIDs(t *testing.T) {
	env := newTestEnv(t)
	suTok := bootSuperuser(t, env)
	u, err := env.k.CreateUser(context.Background(), kernel.CreateUserRequest{Handle: "shown", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/control/users", "/control/users/shown"} {
		body, status := tcpDo(t, suTok, "GET", path, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, status, body)
		}
		if strings.Contains(string(body), u.ID) {
			t.Errorf("%s carries the raw account id: %s", path, body)
		}
		if !strings.Contains(string(body), "shown") {
			t.Errorf("%s should still name the account: %s", path, body)
		}
	}
}

// A list with nothing in it is `[]`, never `null` (API.md R6). These two are built outside the
// store, so the store's guarantee does not reach them.
func TestListsBuiltOutsideTheStoreAreArrays(t *testing.T) {
	env := newTestEnv(t)
	suTok := bootSuperuser(t, env)
	body, status := tcpDo(t, suTok, "GET", "/control/identity", nil)
	if status != http.StatusOK {
		t.Fatalf("identity: status %d: %s", status, body)
	}
	if strings.Contains(string(body), `"addrs":null`) {
		t.Errorf("addrs answered null with no transport: %s", body)
	}
}
