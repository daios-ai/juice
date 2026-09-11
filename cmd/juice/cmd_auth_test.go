package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// loginTokenFor returns just the access token from the canonical PKCE flow.
func loginTokenFor(k *kernel.Kernel, ctx context.Context, handle, password string) (string, error) {
	access, _, err := loginTokensFor(k, ctx, handle, password)
	return access, err
}

// loginTokensFor drives the canonical PKCE flow (§12): authorize, then exchange the code.
func loginTokensFor(k *kernel.Kernel, ctx context.Context, handle, password string) (access, refresh string, err error) {
	verifier, err := kernel.GenerateCodeVerifier()
	if err != nil {
		return "", "", err
	}
	redirect, err := k.StartAuthCode(ctx, handle, password, kernel.CodeChallenge(verifier), "")
	if err != nil {
		return "", "", err
	}
	return k.ExchangeAuthCode(ctx, strings.TrimPrefix(redirect, "?code="), verifier, "")
}

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
	sigB64, err := signRecoveryChallenge(testNet, priv, nonce)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(sigB64)
	payload, _ := testNet.RecoveryChallengeSigningBytes(nonce)
	pub, _ := base64.RawURLEncoding.DecodeString(pubB64)
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		t.Error("recovery challenge signature did not verify against the enrolled key")
	}

	if _, err := deriveRecoveryKey("not a valid recovery phrase"); err == nil {
		t.Error("an invalid mnemonic should be rejected")
	}
}

// TestRecoveryCeremony pins the enrollment sequence (§12): the phrase is displayed before the
// commit callback runs, acknowledgment gates the commit when a reader is supplied, headless
// (nil ack) never pauses or prompts, and a failed commit announces that the phrase is dead.
func TestRecoveryCeremony(t *testing.T) {
	t.Run("headless commits without pausing", func(t *testing.T) {
		var out strings.Builder
		var committed string
		if err := runRecoveryCeremony(&out, nil, "Recovery phrase", func(pub string) error {
			committed = pub
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(committed)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			t.Errorf("commit did not receive a valid recovery public key: %q", committed)
		}
		if !strings.Contains(out.String(), "shown only once") {
			t.Error("notice missing from output")
		}
		if strings.Contains(out.String(), "Press Enter") {
			t.Error("headless ceremony must not prompt for acknowledgment")
		}
	})

	t.Run("acknowledgment gates the commit", func(t *testing.T) {
		r, w := io.Pipe()
		var out safeBuilder
		committed := make(chan string, 1)
		done := make(chan error, 1)
		go func() {
			done <- runRecoveryCeremony(&out, r, "sys recovery phrase", func(pub string) error {
				committed <- pub
				return nil
			})
		}()
		select {
		case <-committed:
			t.Fatal("commit ran before acknowledgment")
		case <-time.After(50 * time.Millisecond):
		}
		if !strings.Contains(out.String(), "sys recovery phrase") {
			t.Error("phrase notice not shown while awaiting acknowledgment")
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		<-committed
	})

	t.Run("failed commit announces the phrase is dead", func(t *testing.T) {
		var out strings.Builder
		wantErr := kernel.ErrInvalidState.Wrap("boom")
		err := runRecoveryCeremony(&out, nil, "Recovery phrase", func(string) error { return wantErr })
		if err != wantErr {
			t.Fatalf("commit error not returned: %v", err)
		}
		if !strings.Contains(out.String(), "NOT enrolled") {
			t.Error("discard warning missing after failed commit")
		}
	})
}

// safeBuilder is a strings.Builder safe for the ceremony goroutine and the asserting test to share.
type safeBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
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

	tok, _, err := loginTokensFor(env.k, ctx, "clitest", "clipass")
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
	if _, err := loginTokenFor(env.k, ctx, "wrongpass", "wrong"); err == nil {
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

	_, rt, err := loginTokensFor(env.k, ctx, "revoke-user", "pass")
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

// TestLoginSelectsAndLogoutUnselects drives the commands an operator actually types: logging in
// names the account and its kernel in one word, stores the session under that name, and acts as it
// from then on; logging out ends it and leaves nothing selected, so the next command says so rather
// than acting as whoever else is logged in.
func TestLoginSelectsAndLogoutUnselects(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "alice", Password: "alicepass"}); err != nil {
		t.Fatal(err)
	}
	// newTestEnv selects tester@test; the kernel record is what `kernel add` would have written.
	if _, err := execTestCmd(t, loginCmd(), "alice@test", "--password", "alicepass"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := loadClientConfig().Current; got != "alice@test" {
		t.Fatalf("login did not select what it authenticated: %q", got)
	}
	if tok, err := loadToken(); err != nil || tok == "" {
		t.Fatalf("no session stored: %q %v", tok, err)
	}
	if c := readCredentials(login{Handle: "alice", Kernel: "test"}); c.PrincipalID == "" {
		t.Error("the login did not record which account it holds")
	}

	// A login this client does not hold cannot be switched to, and a password is not asked for.
	if _, err := execTestCmd(t, authUseCmd(), "bob@test"); err == nil {
		t.Error("switching to a login not held was accepted")
	}

	if _, err := execTestCmd(t, logoutCmd()); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if got := loadClientConfig().Current; got != "" {
		t.Errorf("logout left %q selected", got)
	}
	if held := logins(); len(held) != 0 {
		t.Errorf("logout kept credentials: %v", held)
	}
	if _, _, err := selected(); err == nil {
		t.Error("a command after logout must say there is no login")
	}
}

// TestLoginNeedsItsKernelNamed: a login is an account at a kernel, so the kernel is named every
// time — there is no bare form that would depend on whatever was selected before, and no kernel
// this client has not registered.
func TestLoginNeedsItsKernelNamed(t *testing.T) {
	newTestEnv(t)
	if _, err := execTestCmd(t, loginCmd(), "alice", "--password", "x"); err == nil {
		t.Error("a bare handle was accepted")
	}
	if _, err := execTestCmd(t, loginCmd(), "alice@nosuch", "--password", "x"); err == nil {
		t.Error("an unregistered kernel was accepted")
	}
}
