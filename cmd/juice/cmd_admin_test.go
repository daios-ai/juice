package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
)

func newAdminTestKernel(t *testing.T) *kernel.Kernel {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "admin_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "admin-test-secret"
	return kernel.New(db, nil, nil, nil, nil, cfg, log.Discard())
}

func TestAdminListUsers(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	for i := 0; i < 3; i++ {
		_, err := k.CreateUser(ctx, kernel.CreateUserRequest{
			Handle:   "@user" + string(rune('a'+i)),
			Email:    "user" + string(rune('a'+i)) + "@example.com",
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

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@target", Email: "target@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Suspend.
	if err := k.SuspendUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	// Login should fail.
	if _, err := k.Login(ctx, "@target", "pass"); err == nil {
		t.Error("expected login to fail for suspended user")
	}

	// Unsuspend.
	if err := k.UnsuspendUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	// Login should succeed.
	if _, err := k.Login(ctx, "@target", "pass"); err != nil {
		t.Errorf("expected login to succeed after unsuspend, got: %v", err)
	}
}

func TestAdminDeposit(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	admin, err := k.BootstrapSuperuser(ctx, kernel.CreateUserRequest{
		Handle: "@admin", Email: "admin@example.com", Password: "pass",
	}, "superuser_handle")
	if err != nil {
		t.Fatal(err)
	}
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@recipient", Email: "r@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Deposit succeeds and balance increases.
	d, err := k.Deposit(ctx, admin.ID, u.ID, 500, "initial grant")
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
	if _, err := k.Deposit(ctx, admin.ID, u.ID, 200, "top-up"); err != nil {
		t.Fatal(err)
	}
	u3, _ := k.ReadUser(ctx, u.ID)
	if u3.Available != 700 {
		t.Errorf("available after second deposit: got %d, want 700", u3.Available)
	}

	// Zero amount rejected.
	if _, err := k.Deposit(ctx, admin.ID, u.ID, 0, ""); err == nil {
		t.Error("expected error for zero amount")
	}

	// Negative amount rejected.
	if _, err := k.Deposit(ctx, admin.ID, u.ID, -1, ""); err == nil {
		t.Error("expected error for negative amount")
	}

	// Unknown user rejected.
	if _, err := k.Deposit(ctx, admin.ID, "nonexistent", 100, ""); err == nil {
		t.Error("expected error for unknown target user")
	}
}

func TestRequireSuperuser(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)

	admin, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	regular, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@regular", Email: "regular@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	adminToken, err := env.k.Login(ctx, "@sys", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(adminToken); err != nil {
		t.Fatal(err)
	}
	got, err := requireSuperuser(env.k)
	if err != nil {
		t.Fatalf("admin should pass superuser check: %v", err)
	}
	if got != admin.ID {
		t.Fatalf("subject id: got %q, want %q", got, admin.ID)
	}

	regularToken, err := env.k.Login(ctx, "@regular", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(regularToken); err != nil {
		t.Fatal(err)
	}
	_, err = requireSuperuser(env.k)
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("regular user should be unauthorized, got %v", err)
	}
	if regular.ID == "" {
		t.Fatal("regular user setup failed")
	}
}

// TestAdminDepositEnforcesSuperuser exercises the exact code path that
// adminUserDepositCmd uses: requireSuperuser guard followed by k.Deposit.
func TestAdminDepositEnforcesSuperuser(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)

	su, err := env.k.BootstrapSuperuser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "pass",
	}, "superuser_handle")
	if err != nil {
		t.Fatal(err)
	}
	regular, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@regular", Email: "regular@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@recipient", Email: "rec@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Non-superuser token: requireSuperuser must reject before Deposit is reached.
	regularToken, err := env.k.Login(ctx, "@regular", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(regularToken); err != nil {
		t.Fatal(err)
	}
	_, err = requireSuperuser(env.k)
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("non-superuser should be rejected by requireSuperuser, got %v", err)
	}
	_ = regular.ID // referenced above

	// Superuser token: requireSuperuser succeeds, Deposit goes through.
	suToken, err := env.k.Login(ctx, "@sys", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(suToken); err != nil {
		t.Fatal(err)
	}
	subjectID, err := requireSuperuser(env.k)
	if err != nil {
		t.Fatalf("superuser should pass requireSuperuser: %v", err)
	}
	if subjectID != su.ID {
		t.Fatalf("subjectID: got %q, want %q", subjectID, su.ID)
	}
	d, err := env.k.Deposit(ctx, subjectID, recipient.ID, 500, "test grant")
	if err != nil {
		t.Fatalf("deposit by superuser: %v", err)
	}
	if d.Amount != 500 {
		t.Errorf("deposit amount: got %d, want 500", d.Amount)
	}
}

func TestAdminListAllActions(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	u, _ := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@owner", Email: "owner@example.com", Password: "pass",
	})

	for i := 0; i < 3; i++ {
		_, err := k.CreateAction(ctx, kernel.CreateActionRequest{
			OwnerUserID:  u.ID,
			Name:         "/action" + string(rune('a'+i)),
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
