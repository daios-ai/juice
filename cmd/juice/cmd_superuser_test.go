// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

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
