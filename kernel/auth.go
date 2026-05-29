package kernel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

// HashPassword returns a bcrypt hash of the plain-text password.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", ErrInternal.Wrap("failed to hash password")
	}
	return string(b), nil
}

// CheckPassword returns true if plain matches the stored bcrypt hash.
func CheckPassword(plain, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// jwtClaims are the custom claims embedded in every token.
type jwtClaims struct {
	jwt.RegisteredClaims
}

// IssueToken creates a signed JWT for userID, valid for ttl.
func IssueToken(userID, secret string, ttl time.Duration) (string, error) {
	now := time.Now().UTC()
	claims := jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		return "", ErrInternal.Wrap("failed to sign token")
	}
	return s, nil
}

// VerifyToken parses and validates a JWT, returning the subject (user ID).
func VerifyToken(tokenStr, secret string) (string, error) {
	tok, err := jwt.ParseWithClaims(tokenStr, &jwtClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrUnauthenticated.Wrap("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil || !tok.Valid {
		return "", ErrUnauthenticated.Wrap("invalid or expired token")
	}
	claims, ok := tok.Claims.(*jwtClaims)
	if !ok || claims.Subject == "" {
		return "", ErrUnauthenticated.Wrap("token has no subject")
	}
	return claims.Subject, nil
}

// ---- PKCE helpers ----

// GenerateCodeVerifier returns a random 43-byte URL-safe base64 string.
func GenerateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", ErrInternal.Wrap("failed to generate code verifier")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CodeChallenge computes the S256 PKCE code challenge from a verifier.
func CodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// VerifyCodeChallenge checks that challenge == S256(verifier).
func VerifyCodeChallenge(verifier, challenge string) bool {
	computed := CodeChallenge(verifier)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// ---- Authorization code flow ----

const authCodeTTL = 10 * time.Minute

// StartAuthCode validates credentials, creates an auth code, and returns the
// redirect URI with the code appended as a query parameter.
func (k *Kernel) StartAuthCode(ctx context.Context, handle, password, codeChallenge, redirectURI string) (string, error) {
	if codeChallenge == "" {
		return "", ErrInvalidInput.Wrap("code_challenge is required")
	}
	u, err := k.store.ReadUserByHandle(ctx, handle)
	if err != nil {
		return "", ErrUnauthenticated.Wrap("invalid credentials")
	}
	if !CheckPassword(password, u.PasswordHash) {
		return "", ErrUnauthenticated.Wrap("invalid credentials")
	}
	if err := rejectSuspended(u); err != nil {
		return "", err
	}

	rawCode := make([]byte, 24)
	if _, err := rand.Read(rawCode); err != nil {
		return "", ErrInternal.Wrap("failed to generate auth code")
	}
	code := base64.RawURLEncoding.EncodeToString(rawCode)

	ac := &AuthCode{
		Code:          code,
		UserID:        u.ID,
		CodeChallenge: codeChallenge,
		RedirectURI:   redirectURI,
		ExpiresAt:     time.Now().UTC().Add(authCodeTTL),
	}
	if err := k.store.CreateAuthCode(ctx, ac); err != nil {
		return "", err
	}

	sep := "?"
	if len(redirectURI) > 0 {
		for _, c := range redirectURI {
			if c == '?' {
				sep = "&"
				break
			}
		}
	}
	return redirectURI + sep + "code=" + code, nil
}

// ExchangeAuthCode exchanges a PKCE auth code for access + refresh tokens.
func (k *Kernel) ExchangeAuthCode(ctx context.Context, code, codeVerifier string) (accessToken, refreshToken string, err error) {
	ac, err := k.store.ConsumeAuthCode(ctx, code)
	if err != nil {
		return "", "", ErrUnauthenticated.Wrap("invalid or expired auth code")
	}
	if !VerifyCodeChallenge(codeVerifier, ac.CodeChallenge) {
		return "", "", ErrUnauthenticated.Wrap("code_verifier does not match challenge")
	}
	if u, err := k.store.ReadUser(ctx, ac.UserID); err != nil {
		return "", "", err
	} else if err := rejectSuspended(u); err != nil {
		return "", "", err
	}

	accessToken, err = IssueToken(ac.UserID, k.cfg.TokenSecret, k.cfg.TokenTTL)
	if err != nil {
		return "", "", err
	}
	rt, err := k.issueRefreshToken(ctx, ac.UserID)
	if err != nil {
		return "", "", err
	}
	return accessToken, rt.Token, nil
}

// RefreshAccessToken rotates a refresh token and issues a new access token.
func (k *Kernel) RefreshAccessToken(ctx context.Context, oldRefreshToken string) (accessToken, newRefreshToken string, err error) {
	rt, err := k.store.RotateRefreshToken(ctx, oldRefreshToken)
	if err != nil {
		return "", "", ErrUnauthenticated.Wrap("invalid or expired refresh token")
	}
	if u, err := k.store.ReadUser(ctx, rt.UserID); err != nil {
		return "", "", err
	} else if err := rejectSuspended(u); err != nil {
		return "", "", err
	}
	accessToken, err = IssueToken(rt.UserID, k.cfg.TokenSecret, k.cfg.TokenTTL)
	if err != nil {
		return "", "", err
	}
	return accessToken, rt.Token, nil
}

const refreshTokenTTL = 30 * 24 * time.Hour

func (k *Kernel) issueRefreshToken(ctx context.Context, userID string) (*RefreshToken, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, ErrInternal.Wrap("failed to generate refresh token")
	}
	rt := &RefreshToken{
		Token:     uuid.New().String() + "-" + base64.RawURLEncoding.EncodeToString(raw),
		UserID:    userID,
		ExpiresAt: time.Now().UTC().Add(refreshTokenTTL),
		CreatedAt: time.Now().UTC(),
	}
	if err := k.store.CreateRefreshToken(ctx, rt); err != nil {
		return nil, err
	}
	return rt, nil
}

// LoginWithRefresh authenticates handle+password and returns both access and refresh tokens.
// RevokeRefreshToken revokes a refresh token, preventing further use.
func (k *Kernel) RevokeRefreshToken(ctx context.Context, token string) error {
	return k.store.RevokeRefreshToken(ctx, token)
}

func (k *Kernel) LoginWithRefresh(ctx context.Context, handle, password string) (accessToken, refreshToken string, err error) {
	u, err := k.store.ReadUserByHandle(ctx, handle)
	if err != nil {
		return "", "", ErrUnauthenticated.Wrap("invalid credentials")
	}
	if !CheckPassword(password, u.PasswordHash) {
		return "", "", ErrUnauthenticated.Wrap("invalid credentials")
	}
	if err := rejectSuspended(u); err != nil {
		return "", "", err
	}
	accessToken, err = IssueToken(u.ID, k.cfg.TokenSecret, k.cfg.TokenTTL)
	if err != nil {
		return "", "", err
	}
	rt, err := k.issueRefreshToken(ctx, u.ID)
	if err != nil {
		return "", "", err
	}
	k.log.With(ctx).Info("user.login", "user_id", u.ID)
	return accessToken, rt.Token, nil
}
