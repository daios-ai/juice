package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// healthServer serves one identity banner, the thing a client pins a profile to.
func healthServer(t *testing.T, key, digest, network string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			// Anything else reports whether the client offered its token.
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": r.Header.Get("Authorization")})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "handle": "k", "public_key": key,
			"network": network, "network_digest": digest, "decimals": 0,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// clientHomeFor isolates the profile file in a temp $JUICE_HOME and clears the per-run health
// cache, so one test's server identity is never reused by the next.
func clientHomeFor(t *testing.T) {
	t.Helper()
	t.Setenv("JUICE_HOME", t.TempDir())
	t.Setenv("JUICE_PROFILE", "")
	healthMu.Lock()
	healthCache = map[string]*serverHealth{}
	healthMu.Unlock()
}

// TestProfileRoundTrip pins the file contract: what is written comes back, the file holding bearer
// tokens is 0600 in a 0700 directory, and the atomic write leaves no temp file behind.
func TestProfileRoundTrip(t *testing.T) {
	clientHomeFor(t)

	ps := loadProfiles()
	if ps.Active != "default" {
		t.Fatalf("fresh client: active is %q, want default", ps.Active)
	}
	want := &profile{Endpoint: "http://kernel:4040", PublicKey: "KEY", WorldDigest: "DIGEST",
		Network: "play", Decimals: 2, Token: "tok", RefreshToken: "ref"}
	ps.Profiles["prod"] = want
	ps.Active = "prod"
	if err := saveProfiles(ps); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(profilesPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("profiles.json mode: got %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(clientHome())
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("client dir mode: got %v, want 0700", di.Mode().Perm())
	}
	entries, err := os.ReadDir(clientHome())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("atomic write left %d files behind: %v", len(entries)-1, entries)
	}

	got := loadProfiles()
	if got.Active != "prod" || *got.Profiles["prod"] != *want {
		t.Fatalf("round trip: got %+v", got.Profiles["prod"])
	}
	if tok, err := loadToken(); err != nil || tok != "tok" {
		t.Fatalf("loadToken: got %q, %v", tok, err)
	}
	if err := removeToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(); err == nil {
		t.Error("expected an error after removeToken")
	}
}

// TestProfileEnvOverride pins JUICE_PROFILE: it selects the profile for one invocation, including
// where that invocation's token is stored, without switching the active one.
func TestProfileEnvOverride(t *testing.T) {
	clientHomeFor(t)
	t.Setenv("JUICE_PROFILE", "other")

	if _, name, _ := activeProfile(); name != "other" {
		t.Fatalf("addressed profile: got %q, want other", name)
	}
	if err := saveToken("elsewhere"); err != nil {
		t.Fatal(err)
	}
	ps := loadProfiles()
	if ps.Active != "default" {
		t.Errorf("active profile changed to %q", ps.Active)
	}
	if ps.Profiles["other"].Token != "elsewhere" {
		t.Errorf("token stored in the wrong profile: %+v", ps.Profiles)
	}
	if p := ps.Profiles["default"]; p != nil && p.Token != "" {
		t.Errorf("default profile was written to: %+v", p)
	}
}

// TestUsePinsThenRefusesAnotherKernel pins trust on first use and the refusal that follows: once a
// profile records a kernel's key, a different kernel answering that address is refused rather than
// silently adopted.
func TestUsePinsThenRefusesAnotherKernel(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")

	if err := useProfile(context.Background(), "prod", srv.URL); err != nil {
		t.Fatalf("first use: %v", err)
	}
	ps := loadProfiles()
	if ps.Active != "prod" {
		t.Fatalf("active: got %q, want prod", ps.Active)
	}
	if p := ps.Profiles["prod"]; p.PublicKey != "KEY-A" || p.WorldDigest != "DIGEST-A" || p.Network != "play" {
		t.Fatalf("nothing pinned: %+v", p)
	}

	// Same address, different kernel: switching back must refuse.
	other := healthServer(t, "KEY-B", "DIGEST-A", "play")
	ps.Profiles["prod"].Endpoint = other.URL
	if err := saveProfiles(ps); err != nil {
		t.Fatal(err)
	}
	err := useProfile(context.Background(), "prod", "")
	if err == nil {
		t.Fatal("expected a refusal for a different public key")
	}
	if got := loadProfiles().Profiles["prod"].PublicKey; got != "KEY-A" {
		t.Fatalf("refused switch still repinned the key: %q", got)
	}
}

// TestUseRefusesAnotherNetwork pins the second half of the check: the same kernel serving a
// different network is refused too, since every signature it makes is bound to that network.
func TestUseRefusesAnotherNetwork(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-B", "mainnet")

	ps := loadProfiles()
	ps.Profiles["prod"] = &profile{Endpoint: srv.URL, PublicKey: "KEY-A", WorldDigest: "DIGEST-A", Network: "play"}
	if err := saveProfiles(ps); err != nil {
		t.Fatal(err)
	}
	if err := useProfile(context.Background(), "prod", ""); err == nil {
		t.Fatal("expected a refusal for a different network digest")
	}
	if loadProfiles().Active == "prod" {
		t.Error("refused switch still became active")
	}
}

// TestProfileTokenTravelsOnlyToThePinnedKernel is the point of the profile: --server addresses another
// server, and the login goes with it only when that server proves it is the kernel the login
// TestCredentialsGoOnlyToTheProfileEndpoint: the access token and the refresh token are credentials
// for one kernel, and they travel to that kernel's pinned address and nowhere else. A server that
// answers with the pinned public key has only claimed an identity, not proved one, so it gets
// nothing; and a 401 from a stranger must not be answered with the refresh token either.
func TestCredentialsGoOnlyToTheProfileEndpoint(t *testing.T) {
	clientHomeFor(t)
	pinned := healthServer(t, "KEY-A", "DIGEST-A", "play")
	impostor := healthServer(t, "KEY-A", "DIGEST-A", "play") // copies the pinned identity

	ps := loadProfiles()
	ps.Profiles["default"] = &profile{Endpoint: pinned.URL, PublicKey: "KEY-A",
		WorldDigest: "DIGEST-A", Network: "play", Token: "SECRET", RefreshToken: "REFRESH"}
	if err := saveProfiles(ps); err != nil {
		t.Fatal(err)
	}
	old := flagServer
	t.Cleanup(func() { flagServer = old })
	seen := func(url string) string {
		flagServer = url
		var out struct {
			Auth string `json:"auth"`
		}
		if err := apiCall(context.Background(), "GET", "/v1/thing", nil, &out); err != nil {
			t.Fatal(err)
		}
		return out.Auth
	}
	if got := seen(pinned.URL); got != "Bearer SECRET" {
		t.Errorf("pinned address: got %q, want the token", got)
	}
	if got := seen(impostor.URL); got != "" {
		t.Errorf("a server presenting the pinned key received %q; a claim is not a proof", got)
	}

	// A stranger that answers 401 is not offered the refresh token.
	var refreshHits int
	stranger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/refresh" {
			refreshHits++
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(stranger.Close)
	flagServer = stranger.URL
	_ = apiCall(context.Background(), "GET", "/v1/thing", nil, nil)
	if refreshHits != 0 {
		t.Errorf("the refresh token was posted to a stranger %d time(s)", refreshHits)
	}
}

// TestAmountRoundTrip pins the decimal conversion: exact both ways, and closed to anything that
// would silently change what is paid.
func TestAmountRoundTrip(t *testing.T) {
	cases := []struct {
		in       string
		decimals uint8
		want     int64
		text     string
	}{
		{"5", 0, 5, "5"},
		{"1000000", 0, 1000000, "1000000"},
		{"1.25", 2, 125, "1.25"},
		{"1.2", 2, 120, "1.20"},
		{"0.01", 2, 1, "0.01"},
		{"12", 2, 1200, "12.00"},
		{"0.000001", 6, 1, "0.000001"},
	}
	for _, tc := range cases {
		got, err := parseAmount(tc.in, tc.decimals)
		if err != nil || got != tc.want {
			t.Fatalf("parseAmount(%q, %d): got %d, %v; want %d", tc.in, tc.decimals, got, err, tc.want)
		}
		if text := formatAmount(got, tc.decimals); text != tc.text {
			t.Errorf("formatAmount(%d, %d): got %q, want %q", got, tc.decimals, text, tc.text)
		}
		if back, err := parseAmount(formatAmount(got, tc.decimals), tc.decimals); err != nil || back != got {
			t.Errorf("round trip of %d: got %d, %v", got, back, err)
		}
	}

	bad := []struct {
		in       string
		decimals uint8
	}{
		{"1.5", 0},                  // more decimals than this money has
		{"0.001", 2},                // one place too many
		{"-5", 0},                   // negative
		{"-1.25", 2},                // negative
		{"0", 2},                    // nothing to move
		{"", 0},                     // empty
		{"abc", 0},                  // not a number
		{"1.2.3", 2},                // malformed
		{"1e3", 0},                  // exponent notation is not a decimal amount
		{" 1 000", 0},               // separators
		{"99999999999999999999", 0}, // beyond int64
	}
	for _, tc := range bad {
		if got, err := parseAmount(tc.in, tc.decimals); err == nil {
			t.Errorf("parseAmount(%q, %d): accepted, yielding %d", tc.in, tc.decimals, got)
		}
	}
}

// TestRepointingAProfileForgetsItsLogin: a login belongs to the address it was made at. Pointing a
// profile somewhere new starts it with no credentials, whatever the new server says about itself —
// a server that echoes the pinned key has only claimed to be the same kernel.
func TestRepointingAProfileForgetsItsLogin(t *testing.T) {
	clientHomeFor(t)
	pinned := healthServer(t, "KEY-A", "DIGEST-A", "play")
	impostor := healthServer(t, "KEY-A", "DIGEST-A", "play")

	ps := loadProfiles()
	ps.Profiles["default"] = &profile{Endpoint: pinned.URL, PublicKey: "KEY-A",
		WorldDigest: "DIGEST-A", Network: "play", Token: "SECRET", RefreshToken: "REFRESH"}
	if err := saveProfiles(ps); err != nil {
		t.Fatal(err)
	}
	if err := useProfile(context.Background(), "default", impostor.URL); err != nil {
		t.Fatalf("re-point: %v", err)
	}
	_, _, p := activeProfile()
	if p.Token != "" || p.RefreshToken != "" {
		t.Fatalf("credentials survived a change of address: token=%q refresh=%q", p.Token, p.RefreshToken)
	}
	if p.Endpoint != impostor.URL {
		t.Errorf("endpoint not updated: %q", p.Endpoint)
	}
}
