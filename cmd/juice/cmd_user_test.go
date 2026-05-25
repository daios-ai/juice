package main

import (
	"context"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestUserCreate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "@testuser",
		Email:    "test@example.com",
		Password: "testpass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle != "@testuser" {
		t.Errorf("handle: got %q, want @testuser", u.Handle)
	}
	if u.PasswordHash == "testpass" {
		t.Error("password must be hashed")
	}
}

func TestUserReadByHandle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "@readtest",
		Email:    "read@example.com",
		Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	u, err := env.k.ReadUserByHandle(ctx, "@readtest")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "read@example.com" {
		t.Errorf("email: got %q, want read@example.com", u.Email)
	}
}

func TestUserDuplicateHandleFails(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	req := kernel.CreateUserRequest{Handle: "@dup", Email: "a@b.com", Password: "p"}
	if _, err := env.k.CreateUser(ctx, req); err != nil {
		t.Fatal(err)
	}
	req.Email = "c@d.com"
	if _, err := env.k.CreateUser(ctx, req); err == nil {
		t.Error("expected error for duplicate handle")
	}
}
