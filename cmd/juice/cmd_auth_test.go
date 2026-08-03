package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// TestRecoveryKeyDerivation pins that the CLI's seed-phrase derivation is deterministic and that a
// challenge it signs verifies against the enrolled public key under the kernel's exact payload —
// the CLI↔kernel signature-domain contract for §12 recovery.
func TestRecoveryKeyDerivation(t *testing.T) {
	mnemonic, pubB64, err := generateRecovery()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := deriveRecoveryKey(mnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)); got != pubB64 {
		t.Fatalf("derivation not deterministic: %s vs %s", got, pubB64)
	}

	const nonce = "test-nonce"
	sigB64, err := signRecoveryChallenge(priv, nonce)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(sigB64)
	payload, _ := kernel.RecoveryChallengeSigningBytes(nonce)
	pub, _ := base64.RawURLEncoding.DecodeString(pubB64)
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		t.Error("recovery challenge signature did not verify against the enrolled key")
	}

	if _, err := deriveRecoveryKey("not a valid recovery phrase"); err == nil {
		t.Error("an invalid mnemonic should be rejected")
	}
}

func TestAuthLoginLogout(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "clitest",
		Password: "clipass",
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, _, err := env.k.LoginWithRefresh(ctx, "clitest", "clipass")
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
		Handle:   "wrongpass",
		Password: "correct",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.k.Login(ctx, "wrongpass", "wrong"); err == nil {
		t.Error("expected error for wrong password")
	}
}

func TestRevokeRefreshToken(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "revoke-user", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, rt, err := env.k.LoginWithRefresh(ctx, "revoke-user", "pass")
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

// Token verification for authenticated commands is enforced server-side (authMiddleware)
// and covered in serve_test.go / control_test.go.
