package main

import (
	"os"
	"testing"
)

func TestEnvOr(t *testing.T) {
	const key = "JUICE_TEST_VAR_ENVTEST"
	os.Unsetenv(key)
	if got := envOr(key, "default"); got != "default" {
		t.Errorf("envOr unset: got %q, want %q", got, "default")
	}
	os.Setenv(key, "custom")
	t.Cleanup(func() { os.Unsetenv(key) })
	if got := envOr(key, "default"); got != "custom" {
		t.Errorf("envOr set: got %q, want %q", got, "custom")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	if err := saveToken("tok123"); err != nil {
		t.Fatal(err)
	}
	got, err := loadToken()
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok123" {
		t.Errorf("loadToken: got %q, want %q", got, "tok123")
	}
	if err := removeToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(); err == nil {
		t.Error("expected error after removeToken")
	}
}

func TestRefreshTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	if err := saveRefreshToken("rt456"); err != nil {
		t.Fatal(err)
	}
	got, err := loadRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if got != "rt456" {
		t.Errorf("loadRefreshToken: got %q, want %q", got, "rt456")
	}
	if err := removeRefreshToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRefreshToken(); err == nil {
		t.Error("expected error after removeRefreshToken")
	}
}
