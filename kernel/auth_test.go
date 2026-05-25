package kernel

import (
	"context"
	"testing"
	"time"
)

func TestHashPassword(t *testing.T) {
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" || hash == "secret" {
		t.Fatal("expected non-trivial hash")
	}
	if !CheckPassword("secret", hash) {
		t.Error("CheckPassword should return true for correct password")
	}
	if CheckPassword("wrong", hash) {
		t.Error("CheckPassword should return false for wrong password")
	}
}

func TestIssueAndVerifyToken(t *testing.T) {
	secret := "test-secret"
	id, err := IssueToken("user-1", secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected non-empty token")
	}
	got, err := VerifyToken(id, secret)
	if err != nil {
		t.Fatal(err)
	}
	if got != "user-1" {
		t.Errorf("got subject %q, want %q", got, "user-1")
	}
}

func TestVerifyTokenExpired(t *testing.T) {
	secret := "test-secret"
	tok, err := IssueToken("user-1", secret, -time.Second) // already expired
	if err != nil {
		t.Fatal(err)
	}
	_, err = VerifyToken(tok, secret)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestVerifyTokenWrongSecret(t *testing.T) {
	tok, err := IssueToken("user-1", "secret-a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = VerifyToken(tok, "secret-b")
	if err == nil {
		t.Error("expected error for wrong secret")
	}
}

// ---- PKCE / auth code flow ----

func TestGenerateCodeVerifier(t *testing.T) {
	v, err := GenerateCodeVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) < 40 {
		t.Errorf("code verifier too short: %q", v)
	}
	v2, _ := GenerateCodeVerifier()
	if v == v2 {
		t.Error("code verifiers should be unique")
	}
}

func TestCodeChallenge(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge := CodeChallenge(verifier)
	if challenge == "" {
		t.Error("expected non-empty challenge")
	}
	if !VerifyCodeChallenge(verifier, challenge) {
		t.Error("VerifyCodeChallenge should return true for matching pair")
	}
	if VerifyCodeChallenge("wrong-verifier", challenge) {
		t.Error("VerifyCodeChallenge should return false for wrong verifier")
	}
}

func TestStartAndExchangeAuthCode(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	u, err := k.CreateUser(ctx, CreateUserRequest{
		Handle:   "@charlie",
		Email:    "charlie@example.com",
		Password: "pw123",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = u

	verifier, _ := GenerateCodeVerifier()
	challenge := CodeChallenge(verifier)

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

	access, refresh, err := k.ExchangeAuthCode(ctx, code, verifier)
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

	_, _, err = k.ExchangeAuthCode(ctx, code, verifier)
	if err == nil {
		t.Error("expected error reusing auth code")
	}
}

func TestExchangeAuthCodeWrongVerifier(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	_, err := k.CreateUser(ctx, CreateUserRequest{
		Handle:   "@dave",
		Email:    "dave@example.com",
		Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	verifier, _ := GenerateCodeVerifier()
	challenge := CodeChallenge(verifier)

	redirect, _ := k.StartAuthCode(ctx, "@dave", "pass", challenge, "")
	var code string
	for i := 0; i < len(redirect); i++ {
		if i+5 <= len(redirect) && redirect[i:i+5] == "code=" {
			code = redirect[i+5:]
			break
		}
	}

	_, _, err = k.ExchangeAuthCode(ctx, code, "wrong-verifier")
	if err == nil {
		t.Error("expected error with wrong code_verifier")
	}
}

func TestRefreshAccessToken(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	_, err := k.CreateUser(ctx, CreateUserRequest{
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
