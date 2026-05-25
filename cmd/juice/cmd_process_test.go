package main

import (
	"context"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

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

	p, root, err := env.k.StartProcess(ctx, owner.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available != 500 {
		t.Errorf("process.available: got %d, want 500", p.Available)
	}
	if root.ParentTraceID != root.ID {
		t.Error("root trace ParentTraceID should equal ID")
	}

	if err := env.k.FundProcess(ctx, owner.ID, p.ID, 200); err != nil {
		t.Fatal(err)
	}
	p2, _ := env.k.ReadProcess(ctx, p.ID)
	if p2.Available != 700 {
		t.Errorf("process.available after fund: got %d, want 700", p2.Available)
	}

	if err := env.k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	p3, _ := env.k.ReadProcess(ctx, p.ID)
	if p3.Status != kernel.ProcessClosed {
		t.Error("process should be closed after end")
	}
}

func TestProcessFeedback(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@fbowner", Email: "fb@example.com", Password: "p",
	})
	p, root, err := env.k.StartProcess(ctx, owner.ID, 0)
	if err != nil {
		t.Fatal(err)
	}

	fb, err := env.k.RecursiveFeedback(ctx, p.ID, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fb.TraceID != root.ID {
		t.Errorf("feedback trace_id: got %q, want %q", fb.TraceID, root.ID)
	}
	if fb.RecursiveCost != 0 {
		t.Errorf("expected 0 recursive cost for empty process, got %d", fb.RecursiveCost)
	}
}

func TestProcessNegativeFundsFails(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@negfund", Email: "nf@example.com", Password: "p",
	})
	_, _, err := env.k.StartProcess(ctx, owner.ID, -1)
	if err == nil {
		t.Error("expected error starting process with negative funds")
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

	p, _, err := env.k.StartProcess(ctx, owner.ID, 400)
	if err != nil {
		t.Fatal(err)
	}

	u, _ := env.db.ReadUser(ctx, owner.ID)
	if u.Available != 600 {
		t.Errorf("owner balance after start: got %d, want 600", u.Available)
	}

	if err := env.k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	u2, _ := env.db.ReadUser(ctx, owner.ID)
	if u2.Available != 1000 {
		t.Errorf("owner balance after end: got %d, want 1000", u2.Available)
	}
}
