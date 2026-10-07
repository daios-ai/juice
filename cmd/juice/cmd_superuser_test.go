// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// `peer inspect` shows the version the peer said it runs, and says unknown when it said none:
// a peer that is offline, or built before kernels sent one.
func TestPeerInspectShowsTheVersionThePeerSaid(t *testing.T) {
	for _, c := range []struct{ reply, want string }{
		{`{"path":"direct","rtt_millis":3,"version":"juice-kernel/v1.2.3"}`, "Version:      juice-kernel/v1.2.3\n"},
		{`{"path":"unreachable","rtt_millis":3}`, "Version:      unknown\n"},
	} {
		stubKernel(t, 6, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"public_key":"K","reachability":` + c.reply + `,"online":true,"source":"live"}`))
		})
		out := captureStdout(t, func() error {
			_, err := execTestCmd(t, peerInspectCmd(), "K")
			return err
		})
		if !strings.Contains(out, c.want) {
			t.Errorf("inspect lacks %q:\n%s", c.want, out)
		}
	}
}

// `admin kernel config KEY VALUE` is one merge patch: KEY as the file spells it, nested as the
// file nests it, VALUE as JSON where it reads as one and as text otherwise; a secret, a map's
// member, a group or a misspelling is refused here, before anything is sent.
func TestOnePatch(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"native.time.price", "7", `{"native":{"time":{"price":7}}}`},
		{"listen_addr", ":1", `{"listen_addr":":1"}`},
		{"log_level", "debug", `{"log_level":"debug"}`},
		{"lottery", "null", `{"lottery":null}`},
		{"fed_listen_addrs", `["/ip4/0.0.0.0/tcp/1"]`, `{"fed_listen_addrs":["/ip4/0.0.0.0/tcp/1"]}`},
	} {
		p, err := onePatch(tc.key, tc.value)
		b, _ := json.Marshal(p)
		if err != nil || string(b) != tc.want {
			t.Errorf("%s %s = %s, %v; want %s", tc.key, tc.value, b, err, tc.want)
		}
	}
	for _, key := range []string{"credentials_key", "native.llm.endpoints.openai.key", "native.llm.endpoints", "native", "bogus", ""} {
		if _, err := onePatch(key, "x"); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("%q was accepted: %v", key, err)
		}
	}
}

// The verb reads and writes through the API as any client does; its human view is the file one
// line per setting in key order, a secret as (set), a change announced; --patch @- carries what no
// command line may; and what cannot be sent is refused before anything is.
func TestAdminKernelConfigCommand(t *testing.T) {
	path := configFixture(t) // first: the client's records live under the same JUICE_HOME
	env := newTestEnv(t)
	tok := bootSuperuser(t, env)
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var err error
		out := captureStdout(t, func() error {
			_, err = execTestCmd(t, configCmd(), args...)
			return nil
		})
		return out, err
	}
	out, err := run()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"credentials_key = (set)\n", "fee_bps = 2000\n", "native.llm.chat = \"ollama/gemma\"\n",
		"native.llm.endpoints.ollama.key = (set)\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "sealed") || strings.Contains(out, "restart") ||
		strings.Index(out, "fee_bps") > strings.Index(out, "native.llm.chat") {
		t.Errorf("listing: %s", out)
	}
	if out, err = run("native.time.price", "7"); err != nil || !strings.Contains(out, "native.time.price = 7\n") || !strings.Contains(out, "restart it") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if cfg, err := LoadConfig(path); err != nil || cfg.Native.Time.Price != 7 || cfg.CredentialsKey != "sealed" {
		t.Fatalf("file after set: %+v, %v", cfg.Native.Time, err)
	}
	r, w, _ := os.Pipe()
	_, _ = w.WriteString(`{"native":{"llm":{"endpoints":{"ollama":{"key":"new"}}}}}`)
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	if _, err := run("--patch", "@-"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := LoadConfig(path); cfg.Native.LLM.Endpoints["ollama"].Key != "new" || cfg.Native.Time.Price != 7 {
		t.Fatalf("file after --patch: %+v", cfg.Native)
	}
	// An inline patch carries what no command line may only when it names no secret.
	if _, err := run("--patch", `{"native":{"llm":{"endpoints":{"ollama":{"prices":{}}}}}}`); err != nil {
		t.Fatalf("an inline patch naming no secret was refused: %v", err)
	}
	for _, args := range [][]string{{"fee_bps"}, {"--patch", "{}", "fee_bps", "1"}, {"--patch", "nope"}, {"bogus", "1"},
		{"--patch", `{"credentials_key":"x"}`}, {"--patch", `{"native":{"llm":{"endpoints":{"ollama":{"key":"typed"}}}}}`}} {
		if _, err := run(args...); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("%v: %v; want a refusal", args, err)
		}
	}
}

func newAdminTestKernel(t *testing.T) *kernel.Kernel {
	t.Helper()
	db := newTestStore(t)
	cfg := testConfig("admin-test-secret")
	k := newKernel(cfg, kernel.Dependencies{Store: db})
	return k
}

func TestAdminListUsers(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	for i := 0; i < 3; i++ {
		_, err := k.CreateUser(ctx, kernel.CreateUserRequest{
			Handle:   "user" + string(rune('a'+i)) + "@k",
			Password: "pass",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	users, err := k.ListUsers(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Errorf("expected 3 users, got %d", len(users))
	}
}

func TestAdminSuspendUnsuspend(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	if err := k.FirstBoot(ctx, "pass", ""); err != nil {
		t.Fatal(err)
	}
	admin, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "target@k", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Suspend.
	if err := k.SuspendUser(ctx, admin.ID, u.ID); err != nil {
		t.Fatal(err)
	}

	// Login should fail.
	if _, err := loginTokenFor(k, ctx, "target", "pass"); err == nil {
		t.Error("expected login to fail for suspended user")
	}

	// Unsuspend.
	if err := k.UnsuspendUser(ctx, admin.ID, u.ID); err != nil {
		t.Fatal(err)
	}

	// Login should succeed.
	if _, err := loginTokenFor(k, ctx, "target", "pass"); err != nil {
		t.Errorf("expected login to succeed after unsuspend, got: %v", err)
	}
}

func TestAdminDeposit(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	if err := k.FirstBoot(ctx, "pass", ""); err != nil {
		t.Fatal(err)
	}
	admin, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "recipient@k", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Deposit succeeds and balance increases.
	d, err := k.Deposit(ctx, admin.ID, u.ID, 500, "initial grant", newRef())
	if err != nil {
		t.Fatal(err)
	}
	if d.Amount != 500 {
		t.Errorf("deposit amount: got %d, want 500", d.Amount)
	}
	if d.OperatorUserID != admin.ID {
		t.Errorf("operator: got %q, want %q", d.OperatorUserID, admin.ID)
	}

	u2, err := k.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u2.Available != 500 {
		t.Errorf("available after deposit: got %d, want 500", u2.Available)
	}

	// Second deposit accumulates.
	if _, err := k.Deposit(ctx, admin.ID, u.ID, 200, "top-up", newRef()); err != nil {
		t.Fatal(err)
	}
	u3, _ := k.ReadUser(ctx, u.ID)
	if u3.Available != 700 {
		t.Errorf("available after second deposit: got %d, want 700", u3.Available)
	}

	// Zero amount rejected.
	if _, err := k.Deposit(ctx, admin.ID, u.ID, 0, "", newRef()); err == nil {
		t.Error("expected error for zero amount")
	}

	// Negative amount rejected.
	if _, err := k.Deposit(ctx, admin.ID, u.ID, -1, "", newRef()); err == nil {
		t.Error("expected error for negative amount")
	}

	// Unknown user rejected.
	if _, err := k.Deposit(ctx, admin.ID, "nonexistent", 100, "", newRef()); err == nil {
		t.Error("expected error for unknown target user")
	}

	// A peer is refused here, at the money boundary itself. The route above resolves users alone, so
	// nothing reaches this with a peer today — which is exactly why it is checked here: a peer holds
	// no money on any path, and what it owes closes when it pays (D14, P10).
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	peer, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Deposit(ctx, admin.ID, peer.ID, 100, "", newRef()); err == nil {
		t.Error("a peer account was credited")
	}
	if p, _ := k.ReadUser(ctx, peer.ID); p.Available != 0 || p.Locked != 0 {
		t.Errorf("peer row after the refusal: %d/%d, want 0/0", p.Available, p.Locked)
	}
}

// Superuser enforcement lives in requireSuperuserMW on the TCP admin routes; it is covered
// end-to-end in control_test.go (TestAdminSuperuserGate).

func TestAdminListAllActions(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	u, _ := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "owner@k", Password: "pass",
	})

	for i := 0; i < 3; i++ {
		_, err := k.CreateAction(ctx, u.ID, kernel.CreateActionRequest{
			OwnerUserID:  u.ID,
			Title:        "Test action",
			Name:         "action" + string(rune('a'+i)),
			Kind:         kernel.KindHTTP,
			Price:        0,
			InputSchema:  map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
			Source:       "http://example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	actions, err := k.ListAllActions(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 3 {
		t.Errorf("expected 3 actions, got %d", len(actions))
	}
}

// The @sys system-wide tx view is now the standard `tx list` (ListTransactions already drops
// the party filter for superusers); superuser scope on the read endpoints is covered in
// control/serve tests. The bespoke enriched admin-txs view was removed with adminListTxRows.
