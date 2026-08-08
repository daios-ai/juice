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
	tok, err := env.k.Login(ctx, "sys", "sys-pass")
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
		map[string]any{"handle": "rcpt", "amount": 500})
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

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := env.k.EnsureKernelAccount(ctx, keyB64)
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
	regTok, err := env.k.Login(ctx, "regular", "pw")
	if err != nil {
		t.Fatal(err)
	}
	body, status := tcpDo(t, regTok, "POST", "/control/deposit",
		map[string]any{"handle": "regular", "amount": 1})
	if status == http.StatusOK {
		t.Fatalf("non-superuser deposit should be rejected, got 200")
	}
	if !strings.Contains(string(body), "superuser") {
		t.Errorf("expected superuser-required error, got: %s", body)
	}
}

// TestAdminTransfersOverTCP exercises the admin transfers resource end-to-end over the gated TCP API
// (§13): list returns operational views (no internal idempotency key / buyer id / input), show reads
// one, and retry refuses a non-pending record (quarantine is not an operator-chosen outcome).
func TestAdminTransfersOverTCP(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	suTok := bootSuperuser(t, env)

	buyer, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "buyer", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.k.Deposit(ctx, mustSysID(t, env.k), buyer.ID, 500, "seed", "seed-1"); err != nil {
		t.Fatal(err)
	}
	desc := kernel.PaymentDescriptor{Beneficiary: "benef-on-peer", Amount: 100, RemoteBPS: 500, RemoteMax: 105}
	// Two records: one left pending, one quarantined (a retry must refuse the quarantined one).
	pending, err := env.k.AdmitRemotePaidStep(ctx, buyer.ID, "peerkeyA", "step-1", []byte(`{}`), "idem-pending", desc)
	if err != nil {
		t.Fatal(err)
	}
	quarantined, err := env.k.AdmitRemotePaidStep(ctx, buyer.ID, "peerkeyA", "step-2", []byte(`{}`), "idem-quar", desc)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.db.SetPendingTransferStatus(ctx, quarantined.ID, "quarantined", "receipt value != amount"); err != nil {
		t.Fatal(err)
	}

	// list (default = unresolved) returns BOTH, as operational views — no internal fields leak.
	body, status := tcpDo(t, suTok, "GET", "/control/transfers", nil)
	if status != http.StatusOK {
		t.Fatalf("list: status %d body %s", status, body)
	}
	for _, leak := range []string{"idempotency_key", "buyer_id", "input_hash", "\"input\""} {
		if strings.Contains(string(body), leak) {
			t.Errorf("list view leaks internal field %s: %s", leak, body)
		}
	}
	if !strings.Contains(string(body), "buyer_handle") || !strings.Contains(string(body), pending.ID) || !strings.Contains(string(body), quarantined.ID) {
		t.Errorf("list must carry both records with a buyer_handle: %s", body)
	}

	// status filter selects one.
	qbody, _ := tcpDo(t, suTok, "GET", "/control/transfers?status=quarantined", nil)
	if strings.Contains(string(qbody), pending.ID) || !strings.Contains(string(qbody), quarantined.ID) {
		t.Errorf("status=quarantined must return only the quarantined record: %s", qbody)
	}

	// show one by id.
	sbody, sstatus := tcpDo(t, suTok, "GET", "/control/transfers/"+pending.ID, nil)
	if sstatus != http.StatusOK || !strings.Contains(string(sbody), pending.ID) {
		t.Errorf("show: status %d body %s", sstatus, sbody)
	}

	// retry a quarantined (non-pending) record is refused — the ONLY mutation, and it never forces.
	rbody, rstatus := tcpDo(t, suTok, "POST", "/control/transfers/"+quarantined.ID+"/retry", nil)
	if rstatus == http.StatusOK {
		t.Errorf("retry of a quarantined record must be refused, got 200: %s", rbody)
	}
	if !strings.Contains(string(rbody), "not pending") {
		t.Errorf("retry refusal must explain the record is not pending: %s", rbody)
	}
}

// mustSysID returns the @sys user id (the fee recipient in tests).
func mustSysID(t *testing.T, k *kernel.Kernel) string {
	t.Helper()
	sys, err := k.ReadUserByHandle(context.Background(), "sys")
	if err != nil {
		t.Fatal(err)
	}
	return sys.ID
}
