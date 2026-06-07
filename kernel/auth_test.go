package kernel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

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
		Handle:   "@charlie",
		Email:    "charlie@example.com",
		Password: "pw123",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = u

	verifier, _ := kernel.GenerateCodeVerifier()
	challenge := kernel.CodeChallenge(verifier)

	redirect, err := k.StartAuthCode(ctx, "@charlie", "pw123", challenge, "http://localhost:9999/cb")
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
	u2, _ := k.ReadUserByHandle(ctx, "@charlie")
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
		Handle:   "@dave",
		Email:    "dave@example.com",
		Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	verifier, _ := kernel.GenerateCodeVerifier()
	challenge := kernel.CodeChallenge(verifier)

	redirect, _ := k.StartAuthCode(ctx, "@dave", "pass", challenge, "")
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
		Handle:   "@eve",
		Email:    "eve@example.com",
		Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	access1, refresh1, err := k.LoginWithRefresh(ctx, "@eve", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if access1 == "" || refresh1 == "" {
		t.Fatal("expected access and refresh tokens from LoginWithRefresh")
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
		Handle: "@sys", Email: "sys@sys", Password: "su-pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@suspended-auth", Email: "suspended@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier, _ := kernel.GenerateCodeVerifier()
	challenge := kernel.CodeChallenge(verifier)
	redirect, err := k.StartAuthCode(ctx, u.Handle, "pass", challenge, "")
	if err != nil {
		t.Fatal(err)
	}
	_, refresh, err := k.LoginWithRefresh(ctx, u.Handle, "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SuspendUser(ctx, admin.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := k.StartAuthCode(ctx, u.Handle, "pass", challenge, ""); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("StartAuthCode: got %v, want ErrUnauthenticated", err)
	}
	code := redirect[len("?code="):]
	if _, _, err := k.ExchangeAuthCode(ctx, code, verifier, ""); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("ExchangeAuthCode: got %v, want ErrUnauthenticated", err)
	}
	if _, _, err := k.RefreshAccessToken(ctx, refresh); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("RefreshAccessToken: got %v, want ErrUnauthenticated", err)
	}
	if _, _, err := k.LoginWithRefresh(ctx, u.Handle, "pass"); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("LoginWithRefresh: got %v, want ErrUnauthenticated", err)
	}
}

func TestSuspendedSubjectRejectedBySupervisionOps(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	su := setupUser(t, st, "@sys", 0)

	u := setupUser(t, st, "@victim", 1000)
	now := time.Now().UTC()
	u.SuspendedAt = &now
	if err := st.SuspendUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	target := setupUser(t, st, "@target", 0)

	// CreateAction: requireSelf rejects suspended subject.
	_, err := k.CreateAction(ctx, u.ID, kernel.CreateActionRequest{
		OwnerUserID: u.ID, Name: "x", Kind: kernel.KindHTTP, Price: 0,
	})
	if !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Errorf("CreateAction: got %v, want ErrUnauthenticated", err)
	}

	// StartProcess: requireSelf rejects suspended subject.
	_, _, err = k.StartProcess(ctx, u.ID, u.ID, 0)
	if !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Errorf("StartProcess: got %v, want ErrUnauthenticated", err)
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
		Handle: "@redir-user", Email: "redir@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	verifier, _ := kernel.GenerateCodeVerifier()
	challenge := kernel.CodeChallenge(verifier)
	redirect, err := k.StartAuthCode(ctx, "@redir-user", "pass", challenge, "http://legit.example.com/cb")
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

// TestSuspendedSubjectRejectedByProcessAndListenerOps verifies that the nine
// supervision methods added in fix 3 enforce the kernel-level suspension check.
func TestSuspendedSubjectRejectedByProcessAndListenerOps(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	// Set up @sys so requireSuperuser-based methods work in this kernel.
	setupSys(t, k, st)

	// Create and immediately suspend the victim user.
	victim := setupUser(t, st, "@victim2", 1000)
	now := time.Now().UTC()
	victim.SuspendedAt = &now
	if err := st.SuspendUser(ctx, victim.ID); err != nil {
		t.Fatal(err)
	}

	// Create a live process owned by another user to test fund/end/authority ops.
	other := setupUser(t, st, "@other2", 500)
	p, _, err := k.StartProcess(ctx, other.ID, other.ID, 100)
	if err != nil {
		t.Fatal(err)
	}

	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, kernel.ErrUnauthenticated) {
			t.Errorf("%s: want ErrUnauthenticated for suspended subject, got %v", name, err)
		}
	}

	check("FundProcess", k.FundProcess(ctx, victim.ID, p.ID, 10))
	check("EndProcess", k.EndProcess(ctx, victim.ID, p.ID))

	// Create a listener owned by @other2 to test listener ops.
	target := setupAction(t, st, other.ID, "tgt", 0)
	target.Public = true
	_ = st.UpdateAction(ctx, target)
	l, err := k.CreateListener(ctx, other.ID, kernel.CreateListenerRequest{
		SourceUserID:   other.ID,
		EventName:      "evt",
		TargetActionID: target.ID,
	})
	if err != nil {
		t.Fatalf("CreateListener setup: %v", err)
	}

	check("CreateListener (suspended)", func() error {
		_, err := k.CreateListener(ctx, victim.ID, kernel.CreateListenerRequest{
			SourceUserID:   other.ID,
			EventName:      "evt",
			TargetActionID: target.ID,
		})
		return err
	}())
	check("PollListener", func() error { _, err := k.PollListener(ctx, victim.ID, l.ID); return err }())
	check("DeleteListener", k.DeleteListener(ctx, victim.ID, l.ID))
	check("GetListener", func() error { _, err := k.GetListener(ctx, victim.ID, l.ID); return err }())
}

// TestRegisterRemoteKernelRequiresSuperuser verifies that non-superusers cannot
// register remote peers at the kernel boundary.
func TestRegisterRemoteKernelRequiresSuperuser(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	setupSys(t, k, st)
	notSys := setupUser(t, st, "@not-sys", 0)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	_, err := k.RegisterRemoteKernel(ctx, notSys.ID, "@peer", pubB64, "https://peer.example.com")
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("non-superuser RegisterRemoteKernel: want ErrUnauthorized, got %v", err)
	}
}
