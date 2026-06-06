package main

import (
	"context"
	"os"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestAuthLoginLogout(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "@clitest",
		Email:    "clitest@example.com",
		Password: "clipass",
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, _, err := env.k.LoginWithRefresh(ctx, "@clitest", "clipass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadToken()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != tok {
		t.Error("loaded token does not match saved token")
	}

	if err := removeToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(); err == nil {
		t.Error("expected error after logout")
	}
}

func TestAuthWrongPassword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "@wrongpass",
		Email:    "wp@example.com",
		Password: "correct",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.k.Login(ctx, "@wrongpass", "wrong"); err == nil {
		t.Error("expected error for wrong password")
	}
}

func TestRevokeRefreshToken(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@revoke-user", Email: "rv@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, rt, err := env.k.LoginWithRefresh(ctx, "@revoke-user", "pass")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.k.RevokeRefreshToken(ctx, rt); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}

	// Rotating a revoked token must fail.
	if _, _, err := env.k.RefreshAccessToken(ctx, rt); err == nil {
		t.Error("expected error refreshing with revoked token")
	}

	// Revoking again must fail.
	if err := env.k.RevokeRefreshToken(ctx, rt); err == nil {
		t.Error("expected error revoking already-revoked token")
	}
}

func TestRequireCallerIDExpired(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	env := newTestEnv(t)
	// No token saved — requireCallerID must fail.
	_, err := requireCallerID(env.k)
	if err == nil {
		t.Error("expected error when no token saved")
	}
}
