package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// ---- OAuth token engine (§8) ----
//
// The engine sits behind the upstream-auth seam. It fetches bearer tokens for the three OAuth
// schemes and caches access tokens in memory only (never persisted). Owner-held schemes
// (client-credentials, jwt-bearer) key their cache by action; the delegated scheme keys by
// (grantor, action) and reads the process owner's Grant — that lookup IS the binding rule
// (§8): the token applies only when grant.grantor_user_id == ownerUserID and the grant names
// this exact action. Refresh-token rotation is persisted back through the GrantStore; a
// provider `invalid_grant` deletes the Grant so the next call re-consents.

// oauthErrInvalidGrant marks a token-endpoint response whose `error` is `invalid_grant`
// (the stored refresh token is dead). The delegated path deletes the Grant on this.
var oauthErrInvalidGrant = errors.New("oauth: invalid_grant")

type oauthEngine struct {
	box        kernel.SecretBox
	grants     kernel.GrantStore
	allowLocal bool
	timeout    time.Duration

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
	fp        string // fingerprint of action.AuthJSON, so a credential change invalidates the entry
}

func newOAuthEngine(box kernel.SecretBox, grants kernel.GrantStore, allowLocal bool, timeout time.Duration) *oauthEngine {
	return &oauthEngine{box: box, grants: grants, allowLocal: allowLocal, timeout: timeout, cache: map[string]cachedToken{}}
}

// oauthConfig is the provider configuration parsed from an action's AuthInput.
type oauthConfig struct {
	tokenURL      string
	authURL       string
	deviceAuthURL string
	clientID      string
	clientSecret  string
	scopes        string
	privateKeyPEM string
	audience      string
	subject       string
	keyID         string
}

func parseOAuthConfig(auth *kernel.AuthInput) oauthConfig {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	return oauthConfig{
		tokenURL:      str(auth.Config, "token_url"),
		authURL:       str(auth.Config, "auth_url"),
		deviceAuthURL: str(auth.Config, "device_auth_url"),
		clientID:      str(auth.Config, "client_id"),
		clientSecret:  str(auth.Secrets, "client_secret"),
		scopes:        str(auth.Config, "scopes"),
		privateKeyPEM: str(auth.Secrets, "private_key"),
		audience:      str(auth.Config, "audience"),
		subject:       str(auth.Config, "subject"),
		keyID:         str(auth.Config, "key_id"),
	}
}

// form seeds a token-endpoint request with the grant type, client id, and (when present) the
// client secret — the fields common to the client-credentials, refresh, and auth-code exchanges.
func (cfg oauthConfig) form(grantType string) url.Values {
	f := url.Values{}
	f.Set("grant_type", grantType)
	f.Set("client_id", cfg.clientID)
	if cfg.clientSecret != "" {
		f.Set("client_secret", cfg.clientSecret)
	}
	return f
}

// tokenResponse is the standard OAuth 2.0 token-endpoint payload.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
}

// token returns a bearer access token for the action's OAuth scheme. When refresh is true any
// cached token is discarded first (used on a 401 retry). Access tokens are cached with a 30s
// safety margin on their expiry.
func (e *oauthEngine) token(ctx context.Context, action *kernel.Action, ownerUserID string, auth *kernel.AuthInput, refresh bool) (string, error) {
	cfg := parseOAuthConfig(auth)
	var key string
	switch auth.Scheme {
	case kernel.AuthSchemeOAuthClientCreds:
		key = "cc|" + action.ID
	case kernel.AuthSchemeOAuthJWTBearer:
		key = "jwt|" + action.ID
	case kernel.AuthSchemeOAuthDelegated:
		key = "del|" + ownerUserID + "|" + action.ID
	default:
		return "", kernel.ErrInvalidState.Wrapf("not an OAuth scheme: %q", auth.Scheme)
	}
	fp := fingerprint(action.AuthJSON)

	if !refresh {
		e.mu.Lock()
		if c, ok := e.cache[key]; ok && c.fp == fp && time.Now().Before(c.expiresAt) {
			e.mu.Unlock()
			return c.token, nil
		}
		e.mu.Unlock()
	}

	var tr *tokenResponse
	var err error
	switch auth.Scheme {
	case kernel.AuthSchemeOAuthClientCreds:
		tr, err = e.exchangeClientCredentials(ctx, cfg)
	case kernel.AuthSchemeOAuthJWTBearer:
		tr, err = e.exchangeJWTBearer(ctx, cfg)
	case kernel.AuthSchemeOAuthDelegated:
		tr, err = e.exchangeDelegated(ctx, action, ownerUserID, cfg)
	}
	if err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", kernel.ErrExecutionFailed.Wrap("token endpoint returned no access token")
	}
	exp := time.Now().Add(60 * time.Second)
	if tr.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - 30*time.Second)
	}
	e.mu.Lock()
	e.cache[key] = cachedToken{token: tr.AccessToken, expiresAt: exp, fp: fp}
	e.mu.Unlock()
	return tr.AccessToken, nil
}

// exchangeDelegated resolves the process owner's grant (the binding rule), decrypts the refresh
// token, and exchanges it. Rotation persists the new refresh token; invalid_grant deletes the
// grant and returns the same typed grant-required error the pre-lock check uses.
func (e *oauthEngine) exchangeDelegated(ctx context.Context, action *kernel.Action, ownerUserID string, cfg oauthConfig) (*tokenResponse, error) {
	if e.grants == nil {
		return nil, kernel.ErrInvalidState.Wrap("grant store not configured")
	}
	g, err := e.grants.ReadGrant(ctx, ownerUserID, action.ID)
	if err != nil {
		if errors.Is(err, kernel.ErrNotFound) {
			return nil, kernel.ErrGrantRequired.Wrapf("grant required for %s", action.Name).WithMeta("action", action.Name)
		}
		return nil, err
	}
	if e.box == nil {
		return nil, kernel.ErrInvalidState.Wrap("credential encryption not configured")
	}
	refreshToken, err := e.box.Open(ownerUserID+"|"+action.ID, g.RefreshToken)
	if err != nil {
		return nil, kernel.ErrInvalidState.Wrap("stored refresh token could not be decrypted")
	}
	tr, err := e.exchangeRefresh(ctx, cfg, refreshToken)
	if err != nil {
		if errors.Is(err, oauthErrInvalidGrant) {
			_ = e.grants.DeleteGrant(ctx, ownerUserID, action.ID)
			return nil, kernel.ErrGrantRequired.Wrapf("grant required for %s", action.Name).WithMeta("action", action.Name)
		}
		return nil, err
	}
	// Provider rotation: persist the new refresh token (resealed) so the next call uses it.
	if tr.RefreshToken != "" && tr.RefreshToken != refreshToken {
		if sealed, serr := e.box.Seal(ownerUserID+"|"+action.ID, tr.RefreshToken); serr == nil {
			_ = e.grants.UpdateGrantRefreshToken(ctx, g.ID, sealed)
		}
	}
	return tr, nil
}

func (e *oauthEngine) exchangeClientCredentials(ctx context.Context, cfg oauthConfig) (*tokenResponse, error) {
	form := cfg.form("client_credentials")
	if cfg.scopes != "" {
		form.Set("scope", cfg.scopes)
	}
	return e.postToken(ctx, cfg.tokenURL, form)
}

func (e *oauthEngine) exchangeRefresh(ctx context.Context, cfg oauthConfig, refreshToken string) (*tokenResponse, error) {
	form := cfg.form("refresh_token")
	form.Set("refresh_token", refreshToken)
	return e.postToken(ctx, cfg.tokenURL, form)
}

// exchangeAuthCode completes the authorization-code flow (called by the consent broker).
func (e *oauthEngine) exchangeAuthCode(ctx context.Context, cfg oauthConfig, code, redirectURI, verifier string) (*tokenResponse, error) {
	form := cfg.form("authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	return e.postToken(ctx, cfg.tokenURL, form)
}

// exchangeJWTBearer builds an RS256 assertion and exchanges it (RFC 7523).
func (e *oauthEngine) exchangeJWTBearer(ctx context.Context, cfg oauthConfig) (*tokenResponse, error) {
	assertion, err := buildJWTAssertion(cfg)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)
	if cfg.scopes != "" {
		form.Set("scope", cfg.scopes)
	}
	return e.postToken(ctx, cfg.tokenURL, form)
}

// startDeviceAuth requests a device+user code from the provider's device-authorization endpoint.
func (e *oauthEngine) startDeviceAuth(ctx context.Context, cfg oauthConfig) (*deviceAuthResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.clientID)
	if cfg.scopes != "" {
		form.Set("scope", cfg.scopes)
	}
	body, status, err := e.postForm(ctx, cfg.deviceAuthURL, form)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, kernel.ErrExecutionFailed.Wrapf("device auth endpoint returned %d", status)
	}
	var dr deviceAuthResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrap("device auth response is not valid JSON")
	}
	if dr.Interval <= 0 {
		dr.Interval = 5
	}
	return &dr, nil
}

// pollDeviceToken performs one poll of the token endpoint for a device grant. It returns
// (nil, nil) while authorization is still pending; a non-nil tokenResponse on success.
func (e *oauthEngine) pollDeviceToken(ctx context.Context, cfg oauthConfig, deviceCode string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", deviceCode)
	form.Set("client_id", cfg.clientID)
	tr, err := e.postToken(ctx, cfg.tokenURL, form)
	if err != nil {
		return nil, err
	}
	switch tr.Error {
	case "authorization_pending", "slow_down":
		return nil, nil
	case "":
		return tr, nil
	default:
		return nil, kernel.ErrExecutionFailed.Wrapf("device authorization failed: %s", tr.Error)
	}
}

type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

// postToken POSTs a form to the token endpoint and parses the standard token response. An OAuth
// error response with `error: invalid_grant` is surfaced as oauthErrInvalidGrant; other explicit
// errors (except the device pending states, handled by the caller) become ErrExecutionFailed.
func (e *oauthEngine) postToken(ctx context.Context, tokenURL string, form url.Values) (*tokenResponse, error) {
	body, status, err := e.postForm(ctx, tokenURL, form)
	if err != nil {
		return nil, err
	}
	var tr tokenResponse
	if len(body) > 0 {
		_ = json.Unmarshal(body, &tr)
	}
	if tr.Error == "invalid_grant" {
		return nil, oauthErrInvalidGrant
	}
	if tr.AccessToken == "" && tr.Error == "" && (status < 200 || status >= 300) {
		return nil, kernel.ErrExecutionFailed.Wrapf("token endpoint returned %d", status)
	}
	return &tr, nil
}

// postForm sends an application/x-www-form-urlencoded POST through the SSRF-guarded client.
func (e *oauthEngine) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	if endpoint == "" {
		return nil, 0, kernel.ErrInvalidState.Wrap("OAuth endpoint not configured")
	}
	if err := validatePublicURL(endpoint, e.allowLocal); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, kernel.ErrInvalidInput.Wrapf("invalid token URL: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := newHTTPClient(e.timeout, e.allowLocal).Do(req)
	if err != nil {
		return nil, 0, kernel.ErrExecutionFailed.Wrapf("token request failed: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, kernel.ErrExecutionFailed.Wrap("could not read token response")
	}
	return b, resp.StatusCode, nil
}

// fingerprint is a short stable hash of an action's sealed auth, used as a cache-invalidation key
// so a credential change (which re-seals auth_json) forces a fresh token fetch.
func fingerprint(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:8])
}

// ---- RS256 JWT assertion (RFC 7523) ----

func buildJWTAssertion(cfg oauthConfig) (string, error) {
	key, err := parseRSAPrivateKey(cfg.privateKeyPEM)
	if err != nil {
		return "", err
	}
	iss := cfg.clientID
	sub := cfg.subject
	if sub == "" {
		sub = iss
	}
	aud := cfg.audience
	if aud == "" {
		aud = cfg.tokenURL
	}
	now := time.Now()
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	if cfg.keyID != "" {
		header["kid"] = cfg.keyID
	}
	claims := map[string]any{
		"iss": iss,
		"sub": sub,
		"aud": aud,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	if cfg.scopes != "" {
		claims["scope"] = cfg.scopes
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", kernel.ErrInternal.Wrapf("sign assertion: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, kernel.ErrInvalidInput.Wrap("private_key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrap("private_key is not a supported RSA key")
	}
	key, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("private_key must be RSA (RS256)")
	}
	return key, nil
}

// ---- Consent broker (authorization-code + device flows) ----
//
// The broker holds short-lived in-memory PKCE/device state while a user consents in a browser.
// It performs the code→token exchange itself (so the refresh token never transits the client) and
// returns the refresh token to the service layer, which seals and stores it via kernel.CreateGrant.

const consentTTL = 10 * time.Minute

type grantBroker struct {
	engine  *oauthEngine
	mu      sync.Mutex
	pending map[string]*pendingConsent
}

type pendingConsent struct {
	grantorUserID string
	actionID      string
	verifier      string
	redirectURI   string
	flow          string // "code" | "device"
	cfg           oauthConfig
	expiresAt     time.Time

	// device flow: filled by the poller goroutine
	mu      sync.Mutex
	done    bool
	refresh string
	pollErr error
}

func newGrantBroker(engine *oauthEngine) *grantBroker {
	return &grantBroker{engine: engine, pending: map[string]*pendingConsent{}}
}

type startResult struct {
	State                   string `json:"state"`
	AuthorizeURL            string `json:"authorize_url,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	UserCode                string `json:"user_code,omitempty"`
	Interval                int    `json:"interval,omitempty"`
	ExpiresIn               int    `json:"expires_in,omitempty"`
}

func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", kernel.ErrInternal.Wrap("failed to generate state")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// start creates a pending consent and returns the browser-facing details. For the code flow the
// redirect URI must be loopback (the client hosts the listener); for the device flow the broker
// spawns a background poller.
func (b *grantBroker) start(ctx context.Context, grantorUserID, actionID string, auth *kernel.AuthInput, redirectURI, flow string) (*startResult, error) {
	cfg := parseOAuthConfig(auth)
	b.sweep()
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	pc := &pendingConsent{grantorUserID: grantorUserID, actionID: actionID, cfg: cfg, flow: flow, expiresAt: time.Now().Add(consentTTL)}

	if flow == "device" {
		dr, err := b.engine.startDeviceAuth(ctx, cfg)
		if err != nil {
			return nil, err
		}
		b.mu.Lock()
		b.pending[state] = pc
		b.mu.Unlock()
		go b.pollDevice(state, cfg, dr.DeviceCode, time.Duration(dr.Interval)*time.Second)
		return &startResult{
			State:                   state,
			VerificationURI:         dr.VerificationURI,
			VerificationURIComplete: dr.VerificationURIComplete,
			UserCode:                dr.UserCode,
			Interval:                dr.Interval,
			ExpiresIn:               dr.ExpiresIn,
		}, nil
	}

	// Authorization-code + PKCE. The kernel never dials redirect_uri — it only embeds it in the
	// authorize URL; the OAuth provider enforces its own registered-redirect allowlist (exact
	// match), so any http(s) target is accepted here. Local clients use a loopback listener;
	// hosted clients use their provider-registered callback URL.
	if u, perr := url.Parse(redirectURI); perr != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, kernel.ErrInvalidInput.Wrap("redirect_uri must be an http(s) URL")
	}
	verifier, err := kernel.GenerateCodeVerifier()
	if err != nil {
		return nil, err
	}
	pc.verifier = verifier
	pc.redirectURI = redirectURI
	authURL, err := buildAuthorizeURL(cfg, redirectURI, state, verifier)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.pending[state] = pc
	b.mu.Unlock()
	return &startResult{State: state, AuthorizeURL: authURL}, nil
}

type completeResult struct {
	Status   string
	ActionID string
	Refresh  string
}

// complete finishes a consent. callerID must equal the grantor. For the code flow it exchanges the
// code immediately; for the device flow it reports the poller's result (or pending). On success the
// pending entry is consumed and the refresh token is returned for the service layer to store.
func (b *grantBroker) complete(ctx context.Context, state, callerID, code string) (*completeResult, error) {
	b.mu.Lock()
	pc := b.pending[state]
	b.mu.Unlock()
	if pc == nil || time.Now().After(pc.expiresAt) {
		return nil, kernel.ErrNotFound.Wrap("consent state not found or expired")
	}
	if pc.grantorUserID != callerID {
		return nil, kernel.ErrUnauthorized.Wrap("consent belongs to another user")
	}

	if pc.flow == "device" {
		pc.mu.Lock()
		done, refresh, pollErr := pc.done, pc.refresh, pc.pollErr
		pc.mu.Unlock()
		if pollErr != nil {
			b.consume(state)
			return nil, pollErr
		}
		if !done {
			return &completeResult{Status: "pending"}, nil
		}
		b.consume(state)
		return &completeResult{Status: "complete", ActionID: pc.actionID, Refresh: refresh}, nil
	}

	tr, err := b.engine.exchangeAuthCode(ctx, pc.cfg, code, pc.redirectURI, pc.verifier)
	if err != nil {
		return nil, err
	}
	if tr.RefreshToken == "" {
		return nil, kernel.ErrExecutionFailed.Wrap("provider returned no refresh token")
	}
	b.consume(state)
	return &completeResult{Status: "complete", ActionID: pc.actionID, Refresh: tr.RefreshToken}, nil
}

func (b *grantBroker) pollDevice(state string, cfg oauthConfig, deviceCode string, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(consentTTL)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		b.mu.Lock()
		pc := b.pending[state]
		b.mu.Unlock()
		if pc == nil {
			return // consumed or swept
		}
		tr, err := b.engine.pollDeviceToken(context.Background(), cfg, deviceCode)
		if err != nil {
			pc.mu.Lock()
			pc.pollErr = err
			pc.mu.Unlock()
			return
		}
		if tr != nil && tr.RefreshToken != "" {
			pc.mu.Lock()
			pc.done = true
			pc.refresh = tr.RefreshToken
			pc.mu.Unlock()
			return
		}
	}
}

func (b *grantBroker) consume(state string) {
	b.mu.Lock()
	delete(b.pending, state)
	b.mu.Unlock()
}

func (b *grantBroker) sweep() {
	now := time.Now()
	b.mu.Lock()
	for s, pc := range b.pending {
		if now.After(pc.expiresAt) {
			delete(b.pending, s)
		}
	}
	b.mu.Unlock()
}

func buildAuthorizeURL(cfg oauthConfig, redirectURI, state, verifier string) (string, error) {
	u, err := url.Parse(cfg.authURL)
	if err != nil {
		return "", kernel.ErrInvalidInput.Wrapf("invalid auth_url: %v", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", kernel.CodeChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	if cfg.scopes != "" {
		q.Set("scope", cfg.scopes)
	}
	q.Set("access_type", "offline") // request a refresh token (Google et al.)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
