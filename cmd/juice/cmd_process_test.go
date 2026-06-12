package main

import (
	"context"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

// setupProcessCmd creates a process directly via the store for cmd/juice tests.
func setupProcessCmd(t *testing.T, env *testEnv, ownerID string, funds int64) *kernel.Process {
	t.Helper()
	ctx := context.Background()
	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	if err := env.db.CreateProcess(ctx, p, ownerID, funds); err != nil {
		t.Fatalf("setupProcessCmd: %v", err)
	}
	return p
}

func TestProcessStartFundEnd(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.User{
		ID:        "user-proc-test",
		Handle:    "@proctest",
		Email:     "proc@example.com",
		Available: 2000,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	hash, _ := kernel.HashPassword("pass")
	owner.PasswordHash = hash
	_ = env.db.CreateUser(ctx, owner)

	// CreateProcess deducts from user.available → user.locked.
	p := setupProcessCmd(t, env, owner.ID, 500)
	proc, err := env.db.ReadProcess(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proc.Available != 500 {
		t.Errorf("process.available: got %d, want 500", proc.Available)
	}

	if err := env.k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	p3, _ := env.k.ReadProcess(ctx, owner.ID, p.ID)
	if p3.Status != kernel.ProcessClosed {
		t.Error("process should be closed after end")
	}
}

func TestProcessList(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@list-proc", Email: "lp@example.com", Password: "p",
	})
	other, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@list-proc-other", Email: "lpo@example.com", Password: "p",
	})

	setupProcessCmd(t, env, owner.ID, 0)
	setupProcessCmd(t, env, owner.ID, 0)
	setupProcessCmd(t, env, other.ID, 0)

	processes, err := env.k.ListProcesses(ctx, owner.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 2 {
		t.Errorf("ListProcesses: got %d, want 2", len(processes))
	}
	for _, p := range processes {
		if p.OwnerUserID != owner.ID {
			t.Errorf("unexpected owner %s", p.OwnerUserID)
		}
	}
}

func TestProcessNegativeFundsFails(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@negfund", Email: "nf@example.com", Password: "p",
	})
	// Try to create process with negative funds via the store — should fail.
	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: owner.ID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	err := env.db.CreateProcess(ctx, p, owner.ID, -1)
	if err == nil {
		t.Error("expected error creating process with negative funds")
	}
}

func TestProcessEndReturnsBalance(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.User{
		ID:        "balance-return-user",
		Handle:    "@baltest",
		Email:     "bal@example.com",
		Available: 1000,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	hash, _ := kernel.HashPassword("pass")
	owner.PasswordHash = hash
	_ = env.db.CreateUser(ctx, owner)

	p := setupProcessCmd(t, env, owner.ID, 400)

	u, _ := env.db.ReadUser(ctx, owner.ID)
	if u.Available != 600 {
		t.Errorf("owner balance after CreateProcess: got %d, want 600", u.Available)
	}

	if err := env.k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	u2, _ := env.db.ReadUser(ctx, owner.ID)
	if u2.Available != 1000 {
		t.Errorf("owner balance after end: got %d, want 1000", u2.Available)
	}
}
