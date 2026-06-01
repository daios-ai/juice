package main

import (
	"context"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

func TestCallClosedProcess(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.User{
		ID:        uuid.New().String(),
		Handle:    "@call-owner",
		Email:     "co@example.com",
		Available: 1000,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	hash, _ := kernel.HashPassword("pass")
	owner.PasswordHash = hash
	_ = env.db.CreateUser(ctx, owner)

	p, root, _ := env.k.StartProcess(ctx, owner.ID, owner.ID, 100)
	_ = env.k.EndProcess(ctx, owner.ID, p.ID)

	// Create an HTTP action — Call will fail before exec (closed process).
	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "/echo",
		Kind: kernel.KindHTTP, Source: "http://example.com/echo",
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	_, err := env.k.Call(ctx, kernel.CallRequest{
		SubjectID:     owner.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  owner.ID,
		ActionName:    "/echo",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected error calling on closed process")
	}
}

func TestCallInsufficientFunds(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@poorowner", Email: "poor@example.com", Password: "p",
	})
	p, root, _ := env.k.StartProcess(ctx, owner.ID, owner.ID, 0)

	_, err := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "/expensive",
		Kind: kernel.KindHTTP, Source: "http://x.com", Price: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Activate it.
	a, _ := env.k.ReadActionByOwnerName(ctx, owner.ID, "/expensive")
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	_, err = env.k.Call(ctx, kernel.CallRequest{
		SubjectID:     owner.ID,
		ProcessID:     p.ID,
		ParentTraceID: root.ID,
		TargetUserID:  owner.ID,
		ActionName:    "/expensive",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected insufficient funds error")
	}
}
