// SPDX-License-Identifier: AGPL-3.0-only

package kernel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// loginToken returns just the access token from the canonical PKCE flow.
func loginToken(k *kernel.Kernel, ctx context.Context, handle, password string) (string, error) {
	access, _, err := loginTokens(k, ctx, handle, password)
	return access, err
}

// loginTokens is the canonical password→tokens path (§12): authorize with PKCE, then exchange the
// code — the only token-issuing flow §14 defines. Credential errors surface from StartAuthCode.
func loginTokens(k *kernel.Kernel, ctx context.Context, handle, password string) (access, refresh string, err error) {
	if !strings.Contains(handle, "@") {
		handle += "@" + kernel.TestOwnName // a login is an address (D15); a bare test handle is one on this kernel
	}
	verifier, err := kernel.GenerateCodeVerifier()
	if err != nil {
		return "", "", err
	}
	redirect, err := k.StartAuthCode(ctx, handle, password, kernel.CodeChallenge(verifier), "")
	if err != nil {
		return "", "", err
	}
	code := strings.TrimPrefix(redirect, "?code=")
	return k.ExchangeAuthCode(ctx, code, verifier, "")
}

func TestHashPassword(t *testing.T) {
	hash, err := kernel.HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" || hash == "secret" {
		t.Fatal("expected non-trivial hash")
	}
	if !kernel.CheckPassword("secret", hash) {
		t.Error("CheckPassword should return true for correct password")
	}
	if kernel.CheckPassword("wrong", hash) {
		t.Error("CheckPassword should return false for wrong password")
	}
}

// TestPasswordMinLength verifies the production 8-character floor. The suite runs with the
// floor relaxed to 1 (TestMain), so this test restores it to 8 and asserts every password-setting
// path rejects a short password and accepts an 8-character one.
func TestPasswordMinLength(t *testing.T) {
	kernel.SetMinPasswordLenForTesting(8)
	defer kernel.SetMinPasswordLenForTesting(1)

	ctx := context.Background()

	// Direct helper via a public path: CreateUser rejects short, accepts >= 8.
	st := newTestStore(t)
	k := newTestKernel(st)

	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "shorty@k", Password: "short12", // 7 chars
	}); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("CreateUser short password: got %v, want ErrInvalidInput", err)
	}
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "longy@k", Password: "pass1234", // 8 chars
	}); err != nil {
		t.Errorf("CreateUser 8-char password: unexpected error %v", err)
	}

	// UpdateUser rejects a short new password and accepts an 8-char one.
	if _, err := k.UpdateUser(ctx, userID(t, st, "longy"), kernel.UpdateUserRequest{
		CurrentPassword: "pass1234", NewPassword: "short12", // 7 chars
	}); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("UpdateUser short new password: got %v, want ErrInvalidInput", err)
	}
	if _, err := k.UpdateUser(ctx, userID(t, st, "longy"), kernel.UpdateUserRequest{
		CurrentPassword: "pass1234", NewPassword: "newpass8", // 8 chars
	}); err != nil {
		t.Errorf("UpdateUser 8-char new password: unexpected error %v", err)
	}

	// FirstBoot rejects a short superuser password.
	if err := newTestKernel(newTestStore(t)).FirstBoot(ctx, "short12", ""); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("FirstBoot short password: got %v, want ErrInvalidInput", err)
	}
}

// userID resolves a handle to its user ID via the store.
func userID(t *testing.T, st kernel.Store, handle string) string {
	t.Helper()
	u, err := st.ReadUserByHandle(context.Background(), handle)
	if err != nil {
		t.Fatalf("userID %s: %v", handle, err)
	}
	return u.ID
}

func TestIssueAndVerifyToken(t *testing.T) {
	secret := "test-secret"
	id, err := kernel.IssueToken("user-1", secret, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected non-empty token")
	}
	got, err := kernel.VerifyToken(id, secret, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "user-1" {
		t.Errorf("got subject %q, want %q", got, "user-1")
	}
}

func TestVerifyTokenExpired(t *testing.T) {
	secret := "test-secret"
	tok, err := kernel.IssueToken("user-1", secret, "", "", -time.Second) // already expired
	if err != nil {
		t.Fatal(err)
	}
	_, err = kernel.VerifyToken(tok, secret, "", "")
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestVerifyTokenWrongSecret(t *testing.T) {
	tok, err := kernel.IssueToken("user-1", "secret-a", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = kernel.VerifyToken(tok, "secret-b", "", "")
	if err == nil {
		t.Error("expected error for wrong secret")
	}
}

// ---- PKCE / auth code flow ----

func TestGenerateCodeVerifier(t *testing.T) {
	v, err := kernel.GenerateCodeVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) < 40 {
		t.Errorf("code verifier too short: %q", v)
	}
	v2, _ := kernel.GenerateCodeVerifier()
	if v == v2 {
		t.Error("code verifiers should be unique")
	}
}

func TestCodeChallenge(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge := kernel.CodeChallenge(verifier)
	if challenge == "" {
		t.Error("expected non-empty challenge")
	}
	if !kernel.VerifyCodeChallenge(verifier, challenge) {
		t.Error("VerifyCodeChallenge should return true for matching pair")
	}
	if kernel.VerifyCodeChallenge("wrong-verifier", challenge) {
		t.Error("VerifyCodeChallenge should return false for wrong verifier")
	}
}

func TestStartAndExchangeAuthCode(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "charlie@k",
		Password: "pw123",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = u

	verifier, _ := kernel.GenerateCodeVerifier()
	challenge := kernel.CodeChallenge(verifier)

	redirect, err := k.StartAuthCode(ctx, "charlie@k", "pw123", challenge, "http://localhost:9999/cb")
	if err != nil {
		t.Fatal(err)
	}
	if redirect == "" {
		t.Fatal("expected redirect URI")
	}

	var code string
	for i := 0; i < len(redirect); i++ {
		if redirect[i:i+5] == "code=" {
			code = redirect[i+5:]
			break
		}
	}
	if code == "" {
		t.Fatalf("could not extract code from redirect: %s", redirect)
	}

	access, refresh, err := k.ExchangeAuthCode(ctx, code, verifier, "http://localhost:9999/cb")
	if err != nil {
		t.Fatal(err)
	}
	if access == "" || refresh == "" {
		t.Error("expected both access and refresh tokens")
	}

	subjectID, err := k.VerifyToken(access)
	if err != nil {
		t.Fatal(err)
	}
	u2, _ := k.ReadUserByHandle(ctx, "charlie")
	if subjectID != u2.ID {
		t.Errorf("token subject: got %q, want %q", subjectID, u2.ID)
	}

	_, _, err = k.ExchangeAuthCode(ctx, code, verifier, "http://localhost:9999/cb")
	if err == nil {
		t.Error("expected error reusing auth code")
	}
}

func TestExchangeAuthCodeWrongVerifier(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	_, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "dave@k",
		Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	verifier, _ := kernel.GenerateCodeVerifier()
	challenge := kernel.CodeChallenge(verifier)

	redirect, _ := k.StartAuthCode(ctx, "dave@k", "pass", challenge, "")
	var code string
	for i := 0; i < len(redirect); i++ {
		if i+5 <= len(redirect) && redirect[i:i+5] == "code=" {
			code = redirect[i+5:]
			break
		}
	}

	_, _, err = k.ExchangeAuthCode(ctx, code, "wrong-verifier", "")
	if err == nil {
		t.Error("expected error with wrong code_verifier")
	}
}

func TestRefreshAccessToken(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	_, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "eve@k",
		Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	access1, refresh1, err := loginTokens(k, ctx, "eve", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if access1 == "" || refresh1 == "" {
		t.Fatal("expected access and refresh tokens from the PKCE exchange")
	}

	access2, refresh2, err := k.RefreshAccessToken(ctx, refresh1)
	if err != nil {
		t.Fatal(err)
	}
	if access2 == "" || refresh2 == "" {
		t.Error("expected new tokens after refresh")
	}
	if refresh2 == refresh1 {
		t.Error("rotated refresh token should be different from old one")
	}

	_, _, err = k.RefreshAccessToken(ctx, refresh1)
	if err == nil {
		t.Error("old refresh token should be revoked after rotation")
	}
}

func TestSuspendedUserCannotUseAuthFlows(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	admin, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "sys@k", Password: "su-pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "suspended-auth@k", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier, _ := kernel.GenerateCodeVerifier()
	challenge := kernel.CodeChallenge(verifier)
	redirect, err := k.StartAuthCode(ctx, u.Handle+"@k", "pass", challenge, "")
	if err != nil {
		t.Fatal(err)
	}
	_, refresh, err := loginTokens(k, ctx, u.Handle, "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SuspendUser(ctx, admin.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := k.StartAuthCode(ctx, u.Handle+"@k", "pass", challenge, ""); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("StartAuthCode: got %v, want ErrUnauthenticated", err)
	}
	code := redirect[len("?code="):]
	if _, _, err := k.ExchangeAuthCode(ctx, code, verifier, ""); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("ExchangeAuthCode: got %v, want ErrUnauthenticated", err)
	}
	if _, _, err := k.RefreshAccessToken(ctx, refresh); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("RefreshAccessToken: got %v, want ErrUnauthenticated", err)
	}
	if _, _, err := loginTokens(k, ctx, u.Handle, "pass"); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("login: got %v, want ErrUnauthenticated", err)
	}
}

func TestSuspendedSubjectRejectedBySupervisionOps(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	su := setupUser(t, st, "sys", 0)

	u := setupUser(t, st, "victim", 1000)
	now := time.Now().UTC()
	u.SuspendedAt = &now
	if err := st.SuspendUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	target := setupUser(t, st, "target", 0)

	// CreateAction: requireSelf rejects suspended subject.
	_, err := k.CreateAction(ctx, u.ID, kernel.CreateActionRequest{
		OwnerUserID: u.ID, Name: "x", Kind: kernel.KindHTTP, Price: 0,
	})
	if !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Errorf("CreateAction: got %v, want ErrUnauthenticated", err)
	}

	// Run: requireActiveUser rejects suspended subject.
	_, err = k.Run(ctx, kernel.RunRequest{CallerID: u.ID, ActionRef: "any@k/nonexistent", Args: nil})
	if !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Errorf("Run: got %v, want ErrUnauthenticated", err)
	}

	// requireSuperuser rejects a suspended @sys.
	suNow := time.Now().UTC()
	su.SuspendedAt = &suNow
	if err := st.SuspendUser(ctx, su.ID); err != nil {
		t.Fatal(err)
	}
	if err := k.SuspendUser(ctx, su.ID, target.ID); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Errorf("SuspendUser via suspended sys: got %v, want ErrUnauthenticated", err)
	}
}

// ---- #15 issuer/audience and redirect_uri tests ----

func TestIssueTokenWithIssuerAudience(t *testing.T) {
	secret := "test-secret"
	issuer := "https://auth.example.com"
	audience := "my-api"

	tok, err := kernel.IssueToken("user-1", secret, issuer, audience, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Correct issuer+audience should verify.
	got, err := kernel.VerifyToken(tok, secret, issuer, audience)
	if err != nil {
		t.Fatalf("VerifyToken with correct iss/aud: %v", err)
	}
	if got != "user-1" {
		t.Errorf("subject: got %q, want user-1", got)
	}

	// Wrong issuer must be rejected.
	if _, err := kernel.VerifyToken(tok, secret, "https://wrong.example.com", audience); err == nil {
		t.Error("expected error for wrong issuer")
	}

	// Wrong audience must be rejected.
	if _, err := kernel.VerifyToken(tok, secret, issuer, "wrong-api"); err == nil {
		t.Error("expected error for wrong audience")
	}
}

func TestExchangeAuthCodeRedirectURIMismatch(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	_, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "redir-user@k", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	verifier, _ := kernel.GenerateCodeVerifier()
	challenge := kernel.CodeChallenge(verifier)
	redirect, err := k.StartAuthCode(ctx, "redir-user@k", "pass", challenge, "http://legit.example.com/cb")
	if err != nil {
		t.Fatal(err)
	}
	var code string
	for i := 0; i+5 <= len(redirect); i++ {
		if redirect[i:i+5] == "code=" {
			code = redirect[i+5:]
			break
		}
	}
	if code == "" {
		t.Fatalf("could not extract code from redirect: %s", redirect)
	}

	_, _, err = k.ExchangeAuthCode(ctx, code, verifier, "http://attacker.example.com/cb")
	if err == nil {
		t.Error("expected error when redirect_uri does not match stored value")
	}
}

// TestSuspendedSubjectRejectedByProcessOps verifies that process operations
// enforce the kernel-level suspension check.
func TestSuspendedSubjectRejectedByProcessOps(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	// Set up @sys so requireSuperuser-based methods work in this kernel.
	setupSys(t, k, st)

	// Create and immediately suspend the victim user.
	victim := setupUser(t, st, "victim2", 1000)
	now := time.Now().UTC()
	victim.SuspendedAt = &now
	if err := st.SuspendUser(ctx, victim.ID); err != nil {
		t.Fatal(err)
	}

	// Create a live process owned by another user to test end/authority ops.
	other := setupUser(t, st, "other2", 500)
	p := setupProcess(t, st, other.ID, 100)

	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, kernel.ErrUnauthenticated) {
			t.Errorf("%s: want ErrUnauthenticated for suspended subject, got %v", name, err)
		}
	}

	check("EndProcess", k.EndProcess(ctx, victim.ID, p.ID))
}

// TestRegisterRemoteKernelRequiresSuperuser verifies that non-superusers cannot
// register remote peers at the kernel boundary.
func TestRegisterRemoteKernelRequiresSuperuser(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	setupSys(t, k, st)
	notSys := setupUser(t, st, "not-sys", 0)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	_, err := k.RenameKernel(ctx, notSys.ID, pubB64, "squatter")
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("non-superuser AddPeer: want ErrUnauthorized, got %v", err)
	}
}

// TestSeedPhraseRecovery covers the §12 recovery path: an enrolled recovery key signs a server
// nonce to reset a lost password; the nonce is single-use; a wrong key is rejected; and an account
// with no key enrolled cannot start recovery.
func TestSeedPhraseRecovery(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	k := newTestKernel(st)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recoveryPub := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "rec@k", Password: "origpass", RecoveryPublicKey: recoveryPub,
	}); err != nil {
		t.Fatal(err)
	}

	sign := func(key ed25519.PrivateKey, nonce string) string {
		payload, err := testNet.RecoveryChallengeSigningBytes(nonce)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
	}

	// Happy path: start, sign, complete -> password reset.
	nonce, err := k.StartRecovery(ctx, "rec@k")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.CompleteRecovery(ctx, "rec@k", nonce, sign(priv, nonce), "newpass1"); err != nil {
		t.Fatalf("CompleteRecovery: %v", err)
	}
	if _, _, err := loginTokens(k, ctx, "rec", "origpass"); err == nil {
		t.Error("old password should be rejected after recovery")
	}
	if _, _, err := loginTokens(k, ctx, "rec", "newpass1"); err != nil {
		t.Errorf("new password should work: %v", err)
	}

	// The nonce is single-use: a replay fails.
	if err := k.CompleteRecovery(ctx, "rec@k", nonce, sign(priv, nonce), "other123"); err == nil {
		t.Error("consumed nonce should not be reusable")
	}

	// A signature from the wrong key is rejected.
	nonce2, err := k.StartRecovery(ctx, "rec@k")
	if err != nil {
		t.Fatal(err)
	}
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	if err := k.CompleteRecovery(ctx, "rec@k", nonce2, sign(wrongPriv, nonce2), "hacked12"); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("wrong-key recovery: got %v, want ErrUnauthorized", err)
	}

	// An account with no recovery key enrolled cannot start recovery.
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "norec@k", Password: "password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.StartRecovery(ctx, "norec@k"); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("StartRecovery without a key: got %v, want ErrInvalidState", err)
	}
}
