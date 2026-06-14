package main

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestUserMe(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@meuser", Email: "me@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := env.k.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Handle != "@meuser" {
		t.Errorf("handle: got %q, want @meuser", got.Handle)
	}
	if got.Email != "me@example.com" {
		t.Errorf("email: got %q, want me@example.com", got.Email)
	}
}

func TestUserUpdateEmail(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@updemail", Email: "old@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := env.k.UpdateUser(ctx, u.ID, kernel.UpdateUserRequest{Email: "new@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "new@example.com" {
		t.Errorf("email after update: got %q, want new@example.com", got.Email)
	}

	// Verify persisted.
	stored, err := env.k.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Email != "new@example.com" {
		t.Errorf("stored email: got %q, want new@example.com", stored.Email)
	}
}

func TestUserUpdatePassword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@updpass", Email: "updpass@example.com", Password: "oldpass",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := env.k.UpdateUser(ctx, u.ID, kernel.UpdateUserRequest{
		CurrentPassword: "oldpass",
		NewPassword:     "newpass",
	}); err != nil {
		t.Fatal(err)
	}

	// Old password must be rejected.
	if _, _, err := env.k.LoginWithRefresh(ctx, "@updpass", "oldpass"); err == nil {
		t.Error("old password should be rejected after change")
	}
	// New password must be accepted.
	if _, _, err := env.k.LoginWithRefresh(ctx, "@updpass", "newpass"); err != nil {
		t.Errorf("new password should work: %v", err)
	}
}

func TestUserUpdatePasswordWrongCurrent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@wrongpass", Email: "wrongpass@example.com", Password: "correct",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = env.k.UpdateUser(ctx, u.ID, kernel.UpdateUserRequest{
		CurrentPassword: "wrong",
		NewPassword:     "newpass",
	})
	if err == nil {
		t.Fatal("expected error for wrong current password")
	}
	if !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Errorf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestUserUpdateNoFields(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@nofields", Email: "nofields@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = env.k.UpdateUser(ctx, u.ID, kernel.UpdateUserRequest{})
	if err == nil {
		t.Fatal("expected error when no fields provided")
	}
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUserUpdateProxyUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	proxy := &kernel.User{
		ID:            "proxy-id-1",
		Handle:        "@remote-peer",
		PublicKey:     "dGVzdGtleQ==",
		RemoteBaseURL: "https://remote.example.com",
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := env.db.CreateProxyUser(ctx, proxy); err != nil {
		t.Fatal(err)
	}

	_, err := env.k.UpdateUser(ctx, proxy.ID, kernel.UpdateUserRequest{Email: "x@x.com"})
	if err == nil {
		t.Fatal("expected error for proxy user")
	}
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState, got %v", err)
	}
}
