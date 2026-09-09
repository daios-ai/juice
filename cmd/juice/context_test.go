package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// healthServer serves one identity banner, the thing a client records a kernel by.
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

// credPath is credentialPath for a test that has already decided the name is a name.
func credPath(t *testing.T, name string) string {
	t.Helper()
	p, err := credentialPath(name)
	if err != nil {
		t.Fatalf("credentialPath(%q): %v", name, err)
	}
	return p
}

// recordKernel records one kernel and one context pointing at it, as `juice use --endpoint` would.
func recordKernel(t *testing.T, endpoint, pub string) {
	t.Helper()
	cfg := loadClientConfig()
	cfg.Kernels[defaultInstance] = &kernelRec{Endpoint: endpoint, PublicKey: pub, Network: "play"}
	cfg.Contexts[defaultInstance] = &contextRec{Kernel: defaultInstance}
	cfg.Current = defaultInstance
	if err := saveClientConfig(cfg); err != nil {
		t.Fatalf("record context: %v", err)
	}
}

func resetHealthCache() {
	healthMu.Lock()
	healthCache = map[string]*serverHealth{}
	healthMu.Unlock()
}

// clientHomeFor isolates the client's records in a temp installation root and clears the per-run
// health cache, so one test's server identity is never reused by the next.
func clientHomeFor(t *testing.T) {
	t.Helper()
	t.Setenv("JUICE_HOME", t.TempDir())
	t.Setenv("JUICE_CONTEXT", "")
	old := flagContext
	flagContext = ""
	t.Cleanup(func() { flagContext = old })
	resetHealthCache()
}

// TestClientRecordsRoundTrip pins the file contract: a kernel, a context on it and that context's
// credentials are three records, what is written comes back, the credential file alone is 0600 in
// a 0700 directory, and the atomic write leaves no temp file behind.
func TestClientRecordsRoundTrip(t *testing.T) {
	clientHomeFor(t)

	cfg := loadClientConfig()
	if cfg.Current != "default" {
		t.Fatalf("fresh client: current is %q, want default", cfg.Current)
	}
	wantK := &kernelRec{Endpoint: "http://kernel:4040", PublicKey: "KEY", WorldDigest: "DIGEST",
		Network: "play", Decimals: 2}
	cfg.Kernels["prod"] = wantK
	cfg.Contexts["prod"] = &contextRec{Kernel: "prod", Handle: "alice", PrincipalID: "uid-1"}
	cfg.Current = "prod"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := saveToken("tok"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefreshToken("ref"); err != nil {
		t.Fatal(err)
	}

	if di, err := os.Stat(clientHome()); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("client dir mode: got %v, %v", di.Mode().Perm(), err)
	}
	fi, err := os.Stat(credPath(t, "prod"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode: got %v, want 0600", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(clientHome())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 { // config.json and credentials/
		t.Errorf("atomic write left files behind: %v", entries)
	}
	// The records a person reads carry no secret; only the credential file does.
	blob, err := os.ReadFile(clientConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"tok", "ref"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("config.json holds the credential %q", secret)
		}
	}

	got := loadClientConfig()
	if got.Current != "prod" || *got.Kernels["prod"] != *wantK {
		t.Fatalf("round trip: got %+v", got.Kernels["prod"])
	}
	if c := got.Contexts["prod"]; c.Handle != "alice" || c.PrincipalID != "uid-1" {
		t.Fatalf("context round trip: got %+v", c)
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

// TestContextEnvOverride pins JUICE_CONTEXT: it selects the context for one invocation, including
// where that invocation's credentials are stored, without switching the current one.
func TestContextEnvOverride(t *testing.T) {
	clientHomeFor(t)
	t.Setenv("JUICE_CONTEXT", "other")

	if _, name, _, _ := activeContext(); name != "other" {
		t.Fatalf("addressed context: got %q, want other", name)
	}
	if err := saveToken("elsewhere"); err != nil {
		t.Fatal(err)
	}
	if cfg := loadClientConfig(); cfg.Current != "default" {
		t.Errorf("current context changed to %q", cfg.Current)
	}
	if _, err := os.Stat(credPath(t, "other")); err != nil {
		t.Errorf("credentials not stored under the addressed context: %v", err)
	}
	if _, err := os.Stat(credPath(t, "default")); err == nil {
		t.Error("the unaddressed context was written to")
	}
}

// TestTwoContextsOnOneKernelHoldSeparateSessions pins the cardinality that makes an installation
// shareable: one kernel, two logins, two sessions. Rotating one leaves the other untouched, which
// is why an agent and a person can work on the same kernel without invalidating each other.
func TestTwoContextsOnOneKernelHoldSeparateSessions(t *testing.T) {
	clientHomeFor(t)
	cfg := loadClientConfig()
	cfg.Kernels["k"] = &kernelRec{Endpoint: "http://kernel:4040", PublicKey: "KEY"}
	cfg.Contexts["person"] = &contextRec{Kernel: "k"}
	cfg.Contexts["agent"] = &contextRec{Kernel: "k"}
	cfg.Current = "person"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}

	t.Setenv("JUICE_CONTEXT", "person")
	if err := saveRefreshToken("PERSON-REF"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUICE_CONTEXT", "agent")
	if err := saveRefreshToken("AGENT-REF"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefreshToken("AGENT-REF-2"); err != nil { // the agent rotates
		t.Fatal(err)
	}

	t.Setenv("JUICE_CONTEXT", "person")
	if got, _ := loadRefreshToken(); got != "PERSON-REF" {
		t.Fatalf("one session's rotation reached another: got %q", got)
	}
	// Both sessions address the one kernel record, so a change of address moves both at once.
	if _, _, c, k := activeContext(); c.Kernel != "k" || k.Endpoint != "http://kernel:4040" {
		t.Fatalf("context does not resolve its kernel: %+v %+v", c, k)
	}
}

// TestConcurrentRefreshIsSerialized pins what the session lock is for: two processes sharing one
// context do not both spend the refresh token. The second to arrive finds the first already
// rotated and takes what it stored.
func TestConcurrentRefreshIsSerialized(t *testing.T) {
	clientHomeFor(t)

	var mu sync.Mutex
	var issued int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/refresh" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		if body.RefreshToken != "REF" {
			// A rotated-away refresh token must never be presented: that is the race the lock closes.
			http.Error(w, "stale refresh token", http.StatusUnauthorized)
			return
		}
		issued++
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "NEW", "refresh_token": "REF-2"})
	}))
	t.Cleanup(srv.Close)
	recordKernel(t, srv.URL, "KEY")
	old := flagServer
	flagServer = srv.URL
	t.Cleanup(func() { flagServer = old })

	if err := saveToken("OLD"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefreshToken("REF"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]bool, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = refreshToken(context.Background(), "OLD")
		}(i)
	}
	wg.Wait()

	if issued != 1 {
		t.Fatalf("the refresh token was spent %d times, want once", issued)
	}
	for i, ok := range results {
		if !ok {
			t.Errorf("caller %d was told to give up though a valid token was available", i)
		}
	}
	if tok, _ := loadToken(); tok != "NEW" {
		t.Fatalf("stored access token: got %q, want the rotated one", tok)
	}
}

// TestUseRecordsThenRefusesAnotherKernel: once a context records a kernel's key, a different kernel
// answering that address is refused rather than silently adopted, and a refused switch records
// nothing.
func TestUseRecordsThenRefusesAnotherKernel(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")

	if err := useContext(context.Background(), "prod", srv.URL, ""); err != nil {
		t.Fatalf("first use: %v", err)
	}
	cfg := loadClientConfig()
	if cfg.Current != "prod" {
		t.Fatalf("current: got %q, want prod", cfg.Current)
	}
	if k := cfg.Kernels["prod"]; k.PublicKey != "KEY-A" || k.WorldDigest != "DIGEST-A" || k.Network != "play" {
		t.Fatalf("nothing recorded: %+v", k)
	}

	// Same address, different kernel: switching back must refuse.
	other := healthServer(t, "KEY-B", "DIGEST-A", "play")
	cfg.Kernels["prod"].Endpoint = other.URL
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	resetHealthCache()
	if err := useContext(context.Background(), "prod", "", ""); err == nil {
		t.Fatal("expected a refusal for a different kernel key")
	}
	if got := loadClientConfig().Kernels["prod"].PublicKey; got != "KEY-A" {
		t.Fatalf("refused switch still re-recorded the key: %q", got)
	}
}

// TestUseRefusesAnotherNetwork pins the second half of the check: the same kernel serving a
// different network is refused too, since every signature it makes is bound to that network.
func TestUseRefusesAnotherNetwork(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-B", "mainnet")

	cfg := loadClientConfig()
	cfg.Kernels["prod"] = &kernelRec{Endpoint: srv.URL, PublicKey: "KEY-A", WorldDigest: "DIGEST-A", Network: "play"}
	cfg.Contexts["prod"] = &contextRec{Kernel: "prod"}
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := useContext(context.Background(), "prod", "", ""); err == nil {
		t.Fatal("expected a refusal for a different network digest")
	}
	if loadClientConfig().Current == "prod" {
		t.Error("refused switch still became current")
	}
}

// TestCredentialsGoOnlyToTheContextEndpoint: the access token and the refresh token belong to one
// kernel, and they travel to that kernel's recorded address and nowhere else. A server that answers
// with the recorded public key has only claimed an identity, not proved one, so it gets nothing;
// and a 401 from a stranger must not be answered with the refresh token either.
func TestCredentialsGoOnlyToTheContextEndpoint(t *testing.T) {
	clientHomeFor(t)
	pinned := healthServer(t, "KEY-A", "DIGEST-A", "play")
	impostor := healthServer(t, "KEY-A", "DIGEST-A", "play") // copies the recorded identity

	recordKernel(t, pinned.URL, "KEY-A")
	if err := saveToken("SECRET"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefreshToken("REFRESH"); err != nil {
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
		t.Errorf("recorded address: got %q, want the token", got)
	}
	if got := seen(impostor.URL); got != "" {
		t.Errorf("a server presenting the recorded key received %q; a claim is not a proof", got)
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

// TestRepointingAContextForgetsItsLogin: a login belongs to the address it was made at. Pointing a
// context somewhere new starts it with no credentials, whatever the new server says about itself.
func TestRepointingAContextForgetsItsLogin(t *testing.T) {
	clientHomeFor(t)
	pinned := healthServer(t, "KEY-A", "DIGEST-A", "play")
	elsewhere := healthServer(t, "KEY-A", "DIGEST-A", "play")

	recordKernel(t, pinned.URL, "KEY-A")
	if err := saveToken("SECRET"); err != nil {
		t.Fatal(err)
	}
	if err := useContext(context.Background(), "default", elsewhere.URL, ""); err != nil {
		t.Fatalf("re-point: %v", err)
	}
	if _, err := loadToken(); err == nil {
		t.Fatal("credentials survived a change of address")
	}
	if _, _, _, k := activeContext(); k.Endpoint != elsewhere.URL {
		t.Errorf("endpoint not updated: %q", k.Endpoint)
	}
}

// TestRepointingAKernelForgetsEveryLoginOnIt: several contexts resolve through one kernel record,
// so pointing that kernel somewhere new must strand all of their logins, not only the one being
// switched. Otherwise a second context would carry its token to whatever now answers.
func TestRepointingAKernelForgetsEveryLoginOnIt(t *testing.T) {
	clientHomeFor(t)
	pinned := healthServer(t, "KEY-A", "DIGEST-A", "play")
	elsewhere := healthServer(t, "KEY-B", "DIGEST-A", "play")

	cfg := loadClientConfig()
	cfg.Kernels["work"] = &kernelRec{Endpoint: pinned.URL, PublicKey: "KEY-A", WorldDigest: "DIGEST-A", Network: "play"}
	cfg.Contexts["person"] = &contextRec{Kernel: "work", Handle: "alice"}
	cfg.Contexts["agent"] = &contextRec{Kernel: "work", Handle: "bot"}
	cfg.Current = "person"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"person", "agent"} {
		t.Setenv("JUICE_CONTEXT", name)
		if err := saveToken("SECRET-" + name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("JUICE_CONTEXT", "")

	if err := useContext(context.Background(), "person", elsewhere.URL, ""); err != nil {
		t.Fatalf("re-point: %v", err)
	}
	for _, name := range []string{"person", "agent"} {
		t.Setenv("JUICE_CONTEXT", name)
		if tok, err := loadToken(); err == nil {
			t.Errorf("context %s kept %q across a change of address", name, tok)
		}
		if _, _, c, _ := activeContext(); c.Handle != "" {
			t.Errorf("context %s still claims to be logged in as %q", name, c.Handle)
		}
	}
}

// TestAMistypedEndpointChangesNothing: `use --endpoint` is a repoint, and a repoint that cannot be
// reached is a refusal. The records must not move and the logins must survive, or a typo would
// leave a client pointing at its old kernel with every session on it destroyed.
func TestAMistypedEndpointChangesNothing(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")
	cfg := loadClientConfig()
	cfg.Kernels["work"] = &kernelRec{Endpoint: srv.URL, PublicKey: "KEY-A", WorldDigest: "DIGEST-A", Network: "play"}
	cfg.Contexts["person"] = &contextRec{Kernel: "work", Handle: "alice"}
	cfg.Contexts["agent"] = &contextRec{Kernel: "work", Handle: "bot"}
	cfg.Current = "person"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"person", "agent"} {
		t.Setenv("JUICE_CONTEXT", n)
		if err := saveToken("TOK-" + n); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("JUICE_CONTEXT", "")

	if err := useContext(context.Background(), "person", "http://127.0.0.1:1", ""); err == nil {
		t.Fatal("an unreachable endpoint was accepted")
	}
	after := loadClientConfig()
	if k := after.Kernels["work"]; k.Endpoint != srv.URL || k.PublicKey != "KEY-A" {
		t.Fatalf("a refused repoint moved the record: %+v", k)
	}
	for _, n := range []string{"person", "agent"} {
		t.Setenv("JUICE_CONTEXT", n)
		if _, err := loadToken(); err != nil {
			t.Errorf("a refused repoint logged %s out: %v", n, err)
		}
		if after.Contexts[n].Handle == "" {
			t.Errorf("a refused repoint forgot who %s was", n)
		}
	}
}

// TestUseAddsSecondLoginOnOneKernel pins the workflow for a second principal: --kernel names a
// kernel this client already knows, so a second context needs no second address.
func TestUseAddsSecondLoginOnOneKernel(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")
	if err := useContext(context.Background(), "person", srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	if err := useContext(context.Background(), "agent", "", "person"); err != nil {
		t.Fatalf("second login: %v", err)
	}
	cfg := loadClientConfig()
	if cfg.Contexts["agent"].Kernel != "person" {
		t.Fatalf("second context points at %q", cfg.Contexts["agent"].Kernel)
	}
	if len(cfg.Kernels) != 1 {
		t.Errorf("a second login must not mint a second kernel: %v", cfg.Kernels)
	}
	if cfg.Current != "agent" {
		t.Errorf("current: got %q", cfg.Current)
	}
	if err := useContext(context.Background(), "other", "", "nosuch"); err == nil {
		t.Error("a context on an unknown kernel must be refused")
	}
}

// TestContextNameCannotEscapeItsDirectory: a context is one file under credentials/, so a label
// that is not a single name is refused where a name becomes a path. Without this, a crafted
// JUICE_CONTEXT points the credential write at the client's own records and destroys them.
func TestContextNameCannotEscapeItsDirectory(t *testing.T) {
	clientHomeFor(t)
	cfg := loadClientConfig()
	cfg.Kernels["real"] = &kernelRec{Endpoint: "http://kernel:4040", PublicKey: "KEY"}
	cfg.Contexts["real"] = &contextRec{Kernel: "real"}
	cfg.Current = "real"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(clientConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"../config", "a/b", ".hidden", "", strings.Repeat("x", 65)} {
		if _, err := credentialPath(bad); err == nil {
			t.Errorf("credentialPath(%q) was allowed", bad)
		}
		t.Setenv("JUICE_CONTEXT", bad)
		if bad != "" { // an empty value selects the current context rather than naming one
			if err := saveToken("PWNED"); err == nil {
				t.Errorf("a credential was stored under the name %q", bad)
			}
			if err := useContext(context.Background(), bad, "http://x", ""); err == nil {
				t.Errorf("use accepted the name %q", bad)
			}
		}
	}
	after, err := os.ReadFile(clientConfigPath())
	if err != nil {
		t.Fatalf("the client's records were destroyed: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("the client's records were rewritten:\n%s", after)
	}
}

// TestLegacyProfilesMigrate pins the client's own migration: each profile becomes a kernel, a
// context and that context's credential file; the old file is kept under a new name so nothing is
// destroyed by a client that ran once.
func TestLegacyProfilesMigrate(t *testing.T) {
	clientHomeFor(t)
	if err := os.MkdirAll(clientHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(clientHome(), "profiles.json")
	blob := `{"active":"prod","profiles":{
	  "prod":{"endpoint":"http://kernel:4040","public_key":"KEY","world_digest":"DIGEST","network":"play","decimals":2,"token":"TOK","refresh_token":"REF"},
	  "spare":{"endpoint":"http://other:4040","public_key":"KEY2"}}}`
	if err := os.WriteFile(legacy, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := loadClientConfig()
	if cfg.Current != "prod" {
		t.Fatalf("current: got %q, want prod", cfg.Current)
	}
	k := cfg.Kernels["prod"]
	if k == nil || k.Endpoint != "http://kernel:4040" || k.PublicKey != "KEY" || k.Decimals != 2 {
		t.Fatalf("kernel not migrated: %+v", k)
	}
	if c := cfg.Contexts["prod"]; c == nil || c.Kernel != "prod" {
		t.Fatalf("context not migrated: %+v", c)
	}
	t.Setenv("JUICE_CONTEXT", "prod")
	if tok, err := loadToken(); err != nil || tok != "TOK" {
		t.Fatalf("login not migrated: %q %v", tok, err)
	}
	if rt, err := loadRefreshToken(); err != nil || rt != "REF" {
		t.Fatalf("refresh token not migrated: %q %v", rt, err)
	}
	if _, err := os.Stat(credPath(t, "spare")); err == nil {
		t.Error("a profile holding no login must not get a credential file")
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Error("the old file was left in place; it must be renamed so it migrates once")
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Errorf("the old file was destroyed rather than kept: %v", err)
	}
	// Running again reads the new records and does not migrate a second time.
	if again := loadClientConfig(); again.Current != "prod" || len(again.Kernels) != 2 {
		t.Fatalf("second load: %+v", again)
	}
}

// TestExistingRecordsAreNeverOverwrittenByMigration: a client that already has its records keeps
// them, whatever an old profiles.json beside them says.
func TestExistingRecordsAreNeverOverwrittenByMigration(t *testing.T) {
	clientHomeFor(t)
	cfg := loadClientConfig()
	cfg.Kernels["mine"] = &kernelRec{Endpoint: "http://mine:4040", PublicKey: "MINE"}
	cfg.Contexts["mine"] = &contextRec{Kernel: "mine"}
	cfg.Current = "mine"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(clientHome(), "profiles.json")
	if err := os.WriteFile(legacy, []byte(`{"active":"old","profiles":{"old":{"endpoint":"http://old:1"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadClientConfig()
	if got.Current != "mine" || got.Kernels["old"] != nil {
		t.Fatalf("existing records were overwritten: %+v", got)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Error("the old file was touched though nothing was migrated")
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
