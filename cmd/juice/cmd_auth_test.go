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

func TestRequireSubjectIDExpired(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	env := newTestEnv(t)
	// No token saved — requireSubjectID must fail.
	_, err := requireSubjectID(env.k)
	if err == nil {
		t.Error("expected error when no token saved")
	}
}
