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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// fakeGrantStore is an in-memory kernel.GrantStore for engine tests.
type fakeGrantStore struct {
	mu      sync.Mutex
	grants  map[string]*kernel.Grant // key: grantor|action
	rotated map[string]string        // grant id → new sealed token
	deleted map[string]bool          // key: grantor|action
}

func newFakeGrantStore() *fakeGrantStore {
	return &fakeGrantStore{grants: map[string]*kernel.Grant{}, rotated: map[string]string{}, deleted: map[string]bool{}}
}
func (f *fakeGrantStore) ReadGrant(_ context.Context, grantor, action string) (*kernel.Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.grants[grantor+"|"+action]; g != nil {
		return g, nil
	}
	return nil, kernel.ErrNotFound.Wrap("no grant")
}
func (f *fakeGrantStore) UpdateGrantRefreshToken(_ context.Context, id, sealed string) error {
	f.mu.Lock()
	f.rotated[id] = sealed
	f.mu.Unlock()
	return nil
}
func (f *fakeGrantStore) DeleteGrant(_ context.Context, grantor, action string) error {
	f.mu.Lock()
	f.deleted[grantor+"|"+action] = true
	delete(f.grants, grantor+"|"+action)
	f.mu.Unlock()
	return nil
}

func testBox(t *testing.T) *aesGCMBox {
	t.Helper()
	box, err := newAESGCMBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// TestOAuthClientCredentialsCached: the engine exchanges client credentials once and caches the
// access token; a credential change (new auth_json fingerprint) or a forced refresh re-fetches.
func TestOAuthClientCredentialsCached(t *testing.T) {
	var hits int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" {
			http.Error(w, "bad grant", 400)
			return
		}
		hits++
		json.NewEncoder(w).Encode(map[string]any{"access_token": "cc-tok", "expires_in": 3600})
	}))
	defer provider.Close()

	eng := newAuthenticator(testBox(t), newFakeGrantStore(), true, time.Second)
	auth := &kernel.AuthInput{
		Scheme:  kernel.AuthSchemeOAuthClientCreds,
		Config:  map[string]any{"token_url": provider.URL, "client_id": "c"},
		Secrets: map[string]any{"client_secret": "s"},
	}
	action := &kernel.Action{ID: "act-cc", Name: "svc", AuthJSON: "seal-1"}
	ctx := context.Background()

	tok, err := eng.token(ctx, action, "", auth, false)
	if err != nil || tok != "cc-tok" {
		t.Fatalf("first token: %q %v", tok, err)
	}
	if _, _ = eng.token(ctx, action, "", auth, false); hits != 1 {
		t.Fatalf("second call not cached: hits=%d", hits)
	}
	// Forced refresh (401 path) re-fetches.
	_, _ = eng.token(ctx, action, "", auth, true)
	if hits != 2 {
		t.Fatalf("forced refresh did not re-fetch: hits=%d", hits)
	}
	// A credential change invalidates the cache.
	action.AuthJSON = "seal-2"
	_, _ = eng.token(ctx, action, "", auth, false)
	if hits != 3 {
		t.Fatalf("credential change did not invalidate cache: hits=%d", hits)
	}
}

// TestOAuthJWTBearer: the engine signs a valid RS256 assertion the provider can verify (RFC 7523).
func TestOAuthJWTBearer(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pemBytes}))

	var verified bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, "bad grant", 400)
			return
		}
		if verifyRS256(r.Form.Get("assertion"), &priv.PublicKey) {
			verified = true
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "jwt-tok", "expires_in": 3600})
	}))
	defer provider.Close()

	eng := newAuthenticator(testBox(t), newFakeGrantStore(), true, time.Second)
	auth := &kernel.AuthInput{
		Scheme:  kernel.AuthSchemeOAuthJWTBearer,
		Config:  map[string]any{"token_url": provider.URL, "client_id": "svc@acct"},
		Secrets: map[string]any{"private_key": keyPEM},
	}
	tok, err := eng.token(context.Background(), &kernel.Action{ID: "act-jwt", AuthJSON: "s"}, "", auth, false)
	if err != nil || tok != "jwt-tok" {
		t.Fatalf("jwt-bearer token: %q %v", tok, err)
	}
	if !verified {
		t.Error("provider did not verify a valid RS256 assertion")
	}
}

// TestDelegatedTokenBinding: the delegated token resolves via the process owner's grant; a
// different owner (no grant) gets the grant-required error. Rotation and invalid_grant are handled.
func TestDelegatedTokenBinding(t *testing.T) {
	box := testBox(t)
	gs := newFakeGrantStore()
	action := &kernel.Action{ID: "act-d", Name: "inbox", AuthJSON: "s"}

	// ownerA has a grant; its refresh token is sealed with AAD grantor|action.
	sealed, _ := box.Seal("ownerA|"+action.ID, "ref-plain")
	gs.grants["ownerA|"+action.ID] = &kernel.Grant{ID: "g1", GrantorUserID: "ownerA", ActionID: action.ID, RefreshToken: sealed}

	var rotate bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "ref-plain" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
			return
		}
		out := map[string]any{"access_token": "acc", "expires_in": 3600}
		if rotate {
			out["refresh_token"] = "ref-rotated"
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer provider.Close()

	eng := newAuthenticator(box, gs, true, time.Second)
	// refFn is wired in main from k.ActionRef; stub it so the grant-required error carries the
	// qualified @owner/name (the bug being guarded: dispatch sites used the bare action name).
	eng.refFn = func(context.Context, string) string { return "@sys/inbox" }
	auth := &kernel.AuthInput{Scheme: kernel.AuthSchemeOAuthDelegated,
		Config: map[string]any{"token_url": provider.URL, "auth_url": provider.URL + "/a", "client_id": "c"}}
	ctx := context.Background()

	// Binding match: ownerA gets a token.
	if tok, err := eng.token(ctx, action, "ownerA", auth, false); err != nil || tok != "acc" {
		t.Fatalf("ownerA token: %q %v", tok, err)
	}
	// Binding mismatch: ownerB has no grant → grant-required error carrying the qualified ref.
	if _, err := eng.token(ctx, action, "ownerB", auth, true); err == nil || !errors.Is(err, kernel.ErrGrantRequired) {
		t.Fatalf("ownerB: got %v, want grant-required", err)
	} else if got := grantMeta(err); got != "@sys/inbox" {
		t.Errorf("ownerB grant-required meta[action] = %q, want @sys/inbox", got)
	}

	// Rotation persists the new refresh token.
	rotate = true
	if _, err := eng.token(ctx, action, "ownerA", auth, true); err != nil {
		t.Fatalf("rotation token: %v", err)
	}
	if gs.rotated["g1"] == "" {
		t.Error("rotation was not persisted")
	}

	// invalid_grant deletes the grant.
	delete(gs.grants, "ownerA|"+action.ID)
	sealedBad, _ := box.Seal("ownerC|"+action.ID, "wrong")
	gs.grants["ownerC|"+action.ID] = &kernel.Grant{ID: "g2", GrantorUserID: "ownerC", ActionID: action.ID, RefreshToken: sealedBad}
	if _, err := eng.token(ctx, action, "ownerC", auth, true); err == nil || !errors.Is(err, kernel.ErrGrantRequired) {
		t.Fatalf("invalid_grant: got %v, want grant-required", err)
	} else if got := grantMeta(err); got != "@sys/inbox" {
		t.Errorf("invalid_grant grant-required meta[action] = %q, want @sys/inbox", got)
	}
	if !gs.deleted["ownerC|"+action.ID] {
		t.Error("invalid_grant did not delete the grant")
	}
}

// TestDelegatedBearerTokenApplied: a delegated_bearer action applies the process owner's stored
// static token into the configured header, defaulting to Authorization: Bearer and covering the
// GitHub `token`, GitLab Private-Token, and X-Api-Key placements (§8).
func TestDelegatedBearerTokenApplied(t *testing.T) {
	box := testBox(t)
	cases := []struct{ name, header, template, readHeader, want string }{
		{"default", "", "", "Authorization", "Bearer ghp_x"},
		{"github", "", "token {token}", "Authorization", "token ghp_x"},
		{"gitlab", "Private-Token", "{token}", "Private-Token", "ghp_x"},
		{"apikey", "X-Api-Key", "{token}", "X-Api-Key", "ghp_x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get(tc.readHeader)
				json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}))
			defer srv.Close()

			cfg := map[string]any{}
			if tc.header != "" {
				cfg["header"] = tc.header
			}
			if tc.template != "" {
				cfg["template"] = tc.template
			}
			auth := kernel.AuthInput{Scheme: kernel.AuthSchemeDelegatedBearer, Config: cfg}
			authJSON, _ := json.Marshal(auth)
			action := &kernel.Action{ID: "act-db-" + tc.name, Source: httpSrc(srv.URL, "POST")}
			var err error
			if action.AuthJSON, err = box.Seal(action.ID, string(authJSON)); err != nil {
				t.Fatalf("seal auth: %v", err)
			}
			gs := newFakeGrantStore()
			sealed, _ := box.Seal("ownerA|"+action.ID, "ghp_x")
			gs.grants["ownerA|"+action.ID] = &kernel.Grant{ID: "g", GrantorUserID: "ownerA", ActionID: action.ID, RefreshToken: sealed}

			eng := newAuthenticator(box, gs, true, time.Second)
			exec := &httpActionExecutor{auth: eng}
			if _, err := exec.Execute(context.Background(), action, map[string]any{}, "ownerA"); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got != tc.want {
				t.Errorf("%s = %q, want %q", tc.readHeader, got, tc.want)
			}
		})
	}
}

// TestDelegatedBearerBindingMismatch: a process owner without a grant is rejected with the typed
// grant-required error at dispatch, and no static token is applied (§8 binding rule).
func TestDelegatedBearerBindingMismatch(t *testing.T) {
	box := testBox(t)
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	auth := kernel.AuthInput{Scheme: kernel.AuthSchemeDelegatedBearer}
	authJSON, _ := json.Marshal(auth)
	action := &kernel.Action{ID: "act-db-mm", Source: httpSrc(srv.URL, "POST")}
	action.AuthJSON, _ = box.Seal(action.ID, string(authJSON))

	gs := newFakeGrantStore()
	sealed, _ := box.Seal("ownerA|"+action.ID, "ghp_x")
	gs.grants["ownerA|"+action.ID] = &kernel.Grant{ID: "g", GrantorUserID: "ownerA", ActionID: action.ID, RefreshToken: sealed}

	eng := newAuthenticator(box, gs, true, time.Second)
	eng.refFn = func(context.Context, string) string { return "@sys/x" }
	exec := &httpActionExecutor{auth: eng}

	// ownerB holds no grant → grant-required, request never sent.
	if _, err := exec.Execute(context.Background(), action, map[string]any{}, "ownerB"); !errors.Is(err, kernel.ErrGrantRequired) {
		t.Fatalf("ownerB: got %v, want ErrGrantRequired", err)
	}
	if reached {
		t.Error("upstream was called despite missing grant")
	}
}

// grantMeta extracts Meta["action"] from a grant-required error.
func grantMeta(err error) string {
	var ke *kernel.KernelError
	if errors.As(err, &ke) {
		return ke.Meta["action"]
	}
	return ""
}

// TestGrantBrokerCodeFlow: authorize URL carries PKCE + state; foreign callers are rejected;
// any http(s) redirect (loopback or hosted) is accepted while a bad scheme is rejected; complete
// exchanges the code for a refresh token.
func TestGrantBrokerCodeFlow(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code_verifier") != "" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "ref-1"})
			return
		}
		http.Error(w, "bad", 400)
	}))
	defer provider.Close()

	eng := newAuthenticator(testBox(t), newFakeGrantStore(), true, time.Second)
	broker := newGrantBroker(eng)
	auth := &kernel.AuthInput{Scheme: kernel.AuthSchemeOAuthDelegated,
		Config: map[string]any{"auth_url": provider.URL + "/auth", "token_url": provider.URL, "client_id": "cid", "scopes": "read"}}
	ctx := context.Background()

	// A bad-scheme redirect is rejected; a hosted https redirect is accepted (the provider, not
	// the kernel, validates redirect targets).
	if _, err := broker.start(ctx, "u1", "act", auth, "ftp://nope/cb", "code"); err == nil {
		t.Error("bad-scheme redirect_uri accepted")
	}
	if res, err := broker.start(ctx, "u1", "act", auth, "https://app.example/callback", "code"); err != nil {
		t.Errorf("hosted https redirect rejected: %v", err)
	} else if !strings.Contains(res.AuthorizeURL, "redirect_uri=https%3A%2F%2Fapp.example%2Fcallback") {
		t.Errorf("hosted redirect not embedded in authorize_url: %s", res.AuthorizeURL)
	}

	res, err := broker.start(ctx, "u1", "act", auth, "http://127.0.0.1:9999/callback", "code")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, want := range []string{"code_challenge=", "code_challenge_method=S256", "state=" + res.State, "response_type=code"} {
		if !strings.Contains(res.AuthorizeURL, want) {
			t.Errorf("authorize_url missing %q: %s", want, res.AuthorizeURL)
		}
	}

	// A different user cannot complete this consent.
	if _, err := broker.complete(ctx, res.State, "attacker", "code"); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("foreign complete: got %v, want ErrUnauthorized", err)
	}
	// The rightful user completes and gets the refresh token.
	cr, err := broker.complete(ctx, res.State, "u1", "the-code")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if cr.Status != "complete" || cr.ActionID != "act" || cr.Refresh != "ref-1" {
		t.Fatalf("complete result = %+v", cr)
	}
	// State is single-use.
	if _, err := broker.complete(ctx, res.State, "u1", "the-code"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("reused state: got %v, want ErrNotFound", err)
	}
}

// TestBrokerExpiredState: a consent past its TTL is not found.
func TestBrokerExpiredState(t *testing.T) {
	broker := newGrantBroker(newAuthenticator(testBox(t), newFakeGrantStore(), true, time.Second))
	auth := &kernel.AuthInput{Scheme: kernel.AuthSchemeOAuthDelegated,
		Config: map[string]any{"auth_url": "https://p.example/a", "token_url": "https://p.example/t", "client_id": "c"}}
	res, err := broker.start(context.Background(), "u1", "act", auth, "http://127.0.0.1:1/callback", "code")
	if err != nil {
		t.Fatal(err)
	}
	broker.mu.Lock()
	broker.pending[res.State].expiresAt = time.Now().Add(-time.Minute)
	broker.mu.Unlock()
	if _, err := broker.complete(context.Background(), res.State, "u1", "c"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("expired state: got %v, want ErrNotFound", err)
	}
}

// TestDeviceFlowPolling: startDeviceAuth returns a user code; pollDeviceToken reports pending then
// success.
func TestDeviceFlowPolling(t *testing.T) {
	var polls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if strings.HasSuffix(r.URL.Path, "/device") {
			json.NewEncoder(w).Encode(map[string]any{"device_code": "dev-1", "user_code": "WXYZ", "verification_uri": "https://p.example/activate", "interval": 1})
			return
		}
		polls++
		if polls < 2 {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "ref-dev"})
	}))
	defer provider.Close()

	eng := newAuthenticator(testBox(t), newFakeGrantStore(), true, time.Second)
	cfg := oauthConfig{deviceAuthURL: provider.URL + "/device", tokenURL: provider.URL + "/token", clientID: "c"}
	ctx := context.Background()

	dr, err := eng.startDeviceAuth(ctx, cfg)
	if err != nil || dr.UserCode != "WXYZ" {
		t.Fatalf("startDeviceAuth: %+v %v", dr, err)
	}
	if tr, _ := eng.pollDeviceToken(ctx, cfg, dr.DeviceCode); tr != nil {
		t.Fatal("first poll should be pending")
	}
	tr, err := eng.pollDeviceToken(ctx, cfg, dr.DeviceCode)
	if err != nil || tr == nil || tr.RefreshToken != "ref-dev" {
		t.Fatalf("second poll: %+v %v", tr, err)
	}
}

// TestUnknownSchemeFailsClosed: the authenticator errors on an unrecognized scheme rather than
// sending the request unauthenticated (§8).
func TestUnknownSchemeFailsClosed(t *testing.T) {
	err := newAuthenticator(nil, nil, false, 0).applyStatic(&kernel.AuthInput{Scheme: "mystery"}, map[string]string{}, nil)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("unknown scheme: got %v, want ErrInvalidState", err)
	}
}

// ---- RS256 test helpers ----

func verifyRS256(assertion string, pub *rsa.PublicKey) bool {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) == nil
}
