package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
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

// credPath is credentialPath for a test that has already decided the login is a login.
func credPath(t *testing.T, name string) string {
	t.Helper()
	l, err := parseLogin(name)
	if err != nil {
		t.Fatalf("parseLogin(%q): %v", name, err)
	}
	p, err := credentialPath(l)
	if err != nil {
		t.Fatalf("credentialPath(%q): %v", name, err)
	}
	return p
}

// recordLogin records one kernel and selects a login on it, as `kernel add` and `auth login` would.
func recordLogin(t *testing.T, name, endpoint, pub string) {
	t.Helper()
	l, err := parseLogin(name)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loadClientConfig()
	cfg.Kernels[l.Kernel] = &kernelRec{Endpoint: endpoint, PublicKey: pub, Network: "play"}
	cfg.Current = l.String()
	if err := saveClientConfig(cfg); err != nil {
		t.Fatalf("record login: %v", err)
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
	t.Setenv("JUICE_AS", "")
	old := flagAs
	flagAs = ""
	t.Cleanup(func() { flagAs = old })
	resetHealthCache()
}

// TestClientRecordsRoundTrip pins the file contract: the kernels a client knows and the credentials
// of one login are separate records, what is written comes back, the credential file alone is 0600
// in a 0700 directory, and the atomic write leaves no temp file behind.
func TestClientRecordsRoundTrip(t *testing.T) {
	clientHomeFor(t)
	cfg := loadClientConfig()
	if cfg.Current != "" {
		t.Fatalf("a fresh client selects %q; it must select nothing", cfg.Current)
	}
	wantK := &kernelRec{Endpoint: "http://kernel:4040", PublicKey: "KEY", WorldDigest: "DIGEST",
		Network: "play", Decimals: 2}
	cfg.Kernels["prod"] = wantK
	cfg.Current = "alice@prod"
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
	fi, err := os.Stat(credPath(t, "alice@prod"))
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
	if got.Current != "alice@prod" || *got.Kernels["prod"] != *wantK {
		t.Fatalf("round trip: got %+v", got.Kernels["prod"])
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

// TestSelectionNamesBothHalves: a login says who and where at once, so what is selected fixes both
// the account a command acts as and the kernel it acts through.
func TestSelectionNamesBothHalves(t *testing.T) {
	clientHomeFor(t)
	recordLogin(t, "alice@prod", "http://kernel:4040", "KEY")
	l, k, err := selected()
	if err != nil {
		t.Fatal(err)
	}
	if l.Handle != "alice" || l.Kernel != "prod" || k.Endpoint != "http://kernel:4040" {
		t.Fatalf("selected: %+v %+v", l, k)
	}
	if serverBaseURL() != "http://kernel:4040" {
		t.Errorf("the selected login's kernel is not what commands address: %q", serverBaseURL())
	}
}

// TestSelectionNeverFallsBack: a selector that names no login here is an error. A misspelled --as
// must not quietly act as somebody else, and with nothing selected there is no address to guess at.
func TestSelectionNeverFallsBack(t *testing.T) {
	clientHomeFor(t)
	if _, _, err := selected(); err == nil {
		t.Fatal("a client with no login selected something")
	}
	if serverBaseURL() != "" {
		t.Errorf("a client with no login addresses %q", serverBaseURL())
	}
	recordLogin(t, "alice@prod", "http://kernel:4040", "KEY")
	flagAs = "alice@nosuch"
	t.Cleanup(func() { flagAs = "" })
	if _, _, err := selected(); err == nil {
		t.Error("--as naming an unknown kernel was accepted")
	}
	flagAs = "alice"
	if _, _, err := selected(); err == nil {
		t.Error("--as without a kernel was accepted")
	}
	// A kernel this client knows is not a login on it: knowing where a kernel is says nothing about
	// who you are there, so nothing is assumed.
	flagAs = ""
	cfg := loadClientConfig()
	cfg.Current = ""
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := selected(); err == nil {
		t.Error("a known kernel with no login was treated as one")
	}
}

// TestAsEnvOverride pins JUICE_AS: it names the login for one invocation, including where that
// invocation's credentials are read and written, without switching the selected one.
func TestAsEnvOverride(t *testing.T) {
	clientHomeFor(t)
	recordLogin(t, "alice@prod", "http://kernel:4040", "KEY")
	t.Setenv("JUICE_AS", "bot@prod")

	if l, _, err := selected(); err != nil || l.String() != "bot@prod" {
		t.Fatalf("addressed login: got %v, %v", l, err)
	}
	if err := saveToken("elsewhere"); err != nil {
		t.Fatal(err)
	}
	if cfg := loadClientConfig(); cfg.Current != "alice@prod" {
		t.Errorf("the selected login changed to %q", cfg.Current)
	}
	if _, err := os.Stat(credPath(t, "bot@prod")); err != nil {
		t.Errorf("credentials not stored under the addressed login: %v", err)
	}
	if _, err := os.Stat(credPath(t, "alice@prod")); err == nil {
		t.Error("the unaddressed login was written to")
	}
}

// TestTwoLoginsOnOneKernelHoldSeparateSessions pins the cardinality that makes an installation
// shareable: one kernel, two accounts, two sessions. Rotating one leaves the other untouched, which
// is why an agent and a person can work on the same kernel without invalidating each other.
func TestTwoLoginsOnOneKernelHoldSeparateSessions(t *testing.T) {
	clientHomeFor(t)
	recordLogin(t, "person@k", "http://kernel:4040", "KEY")

	if err := saveRefreshToken("PERSON-REF"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUICE_AS", "agent@k")
	if err := saveRefreshToken("AGENT-REF"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefreshToken("AGENT-REF-2"); err != nil { // the agent rotates
		t.Fatal(err)
	}

	t.Setenv("JUICE_AS", "person@k")
	if got, _ := loadRefreshToken(); got != "PERSON-REF" {
		t.Fatalf("one session's rotation reached another: got %q", got)
	}
	// Both logins resolve through the one kernel record, so a change of address moves both at once.
	if _, k, err := selected(); err != nil || k.Endpoint != "http://kernel:4040" {
		t.Fatalf("login does not resolve its kernel: %+v %v", k, err)
	}
	if held := logins(); len(held) != 2 {
		t.Errorf("logins(): got %v, want both", held)
	}
}

// TestConcurrentRefreshIsSerialized pins what the session lock is for: two processes sharing one
// login do not both spend the refresh token. The second to arrive finds the first already rotated
// and takes what it stored.
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
	recordLogin(t, "alice@k", srv.URL, "KEY")
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

// TestAddThenRefuseAnotherKernel: once a kernel's key is recorded, a different kernel answering
// that address is refused rather than silently adopted — on selection, and on a second `kernel add`
// under the same name — and a refusal records nothing.
func TestAddThenRefuseAnotherKernel(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")

	name, k, outcome, err := registerKernel(context.Background(), "", srv.URL)
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if name != "k" || outcome != "added" { // the name defaults to the nickname the kernel advertises
		t.Fatalf("add: name %q outcome %q", name, outcome)
	}
	if k.PublicKey != "KEY-A" || k.WorldDigest != "DIGEST-A" || k.Network != "play" {
		t.Fatalf("nothing recorded: %+v", k)
	}
	if cfg := loadClientConfig(); cfg.Current != "" {
		t.Errorf("adding a kernel selected %q; it must select nothing", cfg.Current)
	}
	// Adding the same kernel again, on the same terms, is a no-op rather than an error.
	if _, _, outcome, err := registerKernel(context.Background(), "k", srv.URL); err != nil || outcome != "already known" {
		t.Errorf("re-adding the same kernel: outcome %q, %v", outcome, err)
	}

	// Another kernel under a name already taken is refused, and the record stands.
	other := healthServer(t, "KEY-B", "DIGEST-A", "play")
	resetHealthCache()
	if _, _, _, err := registerKernel(context.Background(), "k", other.URL); err == nil {
		t.Fatal("a different kernel took a name already held")
	}
	if got := loadClientConfig().Kernels["k"]; got.PublicKey != "KEY-A" || got.Endpoint != srv.URL {
		t.Fatalf("a refused add changed the record: %+v", got)
	}
	// Selecting a login whose kernel now answers with another key is refused too.
	cfg := loadClientConfig()
	cfg.Kernels["k"].Endpoint = other.URL
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	resetHealthCache()
	if err := selectLogin(context.Background(), login{Handle: "alice", Kernel: "k"}); err == nil {
		t.Fatal("expected a refusal for a different kernel key")
	}
	if loadClientConfig().Current != "" {
		t.Error("a refused selection still became current")
	}
}

// TestSelectRefusesAnotherNetwork pins the second half of the check: the same kernel serving a
// different network is refused too, since every signature it makes is bound to that network.
func TestSelectRefusesAnotherNetwork(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-B", "mainnet")

	cfg := loadClientConfig()
	cfg.Kernels["prod"] = &kernelRec{Endpoint: srv.URL, PublicKey: "KEY-A", WorldDigest: "DIGEST-A", Network: "play"}
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := selectLogin(context.Background(), login{Handle: "alice", Kernel: "prod"}); err == nil {
		t.Fatal("expected a refusal for a different network digest")
	}
	if loadClientConfig().Current != "" {
		t.Error("refused switch still became current")
	}
}

// TestCredentialsGoOnlyToTheirKernelsAddress: the access token and the refresh token belong to one
// kernel, and they travel to that kernel's recorded address and nowhere else. A server that answers
// with the recorded public key has only claimed an identity, not proved one, so it gets nothing;
// and a 401 from a stranger must not be answered with the refresh token either.
func TestCredentialsGoOnlyToTheirKernelsAddress(t *testing.T) {
	clientHomeFor(t)
	pinned := healthServer(t, "KEY-A", "DIGEST-A", "play")
	impostor := healthServer(t, "KEY-A", "DIGEST-A", "play") // copies the recorded identity

	recordLogin(t, "alice@k", pinned.URL, "KEY-A")
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

// TestAMovedKernelKeepsItsLoginsAndAStrangerTakesNoName: a key is what says which kernel this is,
// so the same kernel at a new address is followed and keeps every session made on it — a session
// belongs to the kernel that issued it, and this is that kernel. A different kernel under a name
// already held is refused outright: taking the name would point every login on it at a stranger.
func TestAMovedKernelKeepsItsLoginsAndAStrangerTakesNoName(t *testing.T) {
	clientHomeFor(t)
	first := healthServer(t, "KEY-A", "DIGEST-A", "play")
	moved := healthServer(t, "KEY-A", "DIGEST-A", "play") // the same kernel, answering elsewhere
	stranger := healthServer(t, "KEY-B", "DIGEST-A", "play")

	recordLogin(t, "person@work", first.URL, "KEY-A")
	for _, name := range []string{"person@work", "agent@work"} {
		t.Setenv("JUICE_AS", name)
		if err := saveToken("SECRET-" + name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("JUICE_AS", "")

	_, _, outcome, err := registerKernel(context.Background(), "work", moved.URL)
	if err != nil {
		t.Fatalf("following a moved kernel: %v", err)
	}
	if !strings.Contains(outcome, "moved") {
		t.Errorf("outcome = %q, want it to say the kernel moved", outcome)
	}
	if k := loadClientConfig().Kernels["work"]; k.Endpoint != moved.URL || k.PublicKey != "KEY-A" {
		t.Errorf("the new address was not recorded: %+v", k)
	}
	for _, name := range []string{"person@work", "agent@work"} {
		t.Setenv("JUICE_AS", name)
		if tok, err := loadToken(); err != nil || tok != "SECRET-"+name {
			t.Errorf("login %s lost its session when its kernel moved: %q %v", name, tok, err)
		}
	}
	t.Setenv("JUICE_AS", "")

	resetHealthCache()
	if _, _, _, err := registerKernel(context.Background(), "work", stranger.URL); err == nil {
		t.Fatal("a different kernel took a name already held")
	}
	if k := loadClientConfig().Kernels["work"]; k.Endpoint != moved.URL || k.PublicKey != "KEY-A" {
		t.Errorf("a refused add moved the record: %+v", k)
	}
	for _, name := range []string{"person@work", "agent@work"} {
		t.Setenv("JUICE_AS", name)
		if _, err := loadToken(); err != nil {
			t.Errorf("a refused add logged %s out: %v", name, err)
		}
	}
}

// TestAMistypedEndpointChangesNothing: an address that cannot be reached is a refusal. The records
// must not move and the logins must survive, or a typo would leave a client pointing at nothing
// with every session on it destroyed.
func TestAMistypedEndpointChangesNothing(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")
	recordLogin(t, "person@work", srv.URL, "KEY-A")
	for _, n := range []string{"person@work", "agent@work"} {
		t.Setenv("JUICE_AS", n)
		if err := saveToken("TOK-" + n); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("JUICE_AS", "")

	if _, _, _, err := registerKernel(context.Background(), "work", "http://127.0.0.1:1"); err == nil {
		t.Fatal("an unreachable endpoint was accepted")
	}
	if k := loadClientConfig().Kernels["work"]; k.Endpoint != srv.URL || k.PublicKey != "KEY-A" {
		t.Fatalf("a refused repoint moved the record: %+v", k)
	}
	for _, n := range []string{"person@work", "agent@work"} {
		t.Setenv("JUICE_AS", n)
		if _, err := loadToken(); err != nil {
			t.Errorf("a refused repoint logged %s out: %v", n, err)
		}
	}
}

// TestASecondLoginNeedsNoSecondKernel: a second account on a kernel already known is a second
// credential file and nothing else — no second address, no second record.
func TestASecondLoginNeedsNoSecondKernel(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")
	if _, _, _, err := registerKernel(context.Background(), "work", srv.URL); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"person@work", "agent@work"} {
		l, err := parseLogin(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := selectLogin(context.Background(), l); err != nil {
			t.Fatalf("select %s: %v", name, err)
		}
		if err := saveToken("TOK-" + name); err != nil {
			t.Fatal(err)
		}
	}
	cfg := loadClientConfig()
	if len(cfg.Kernels) != 1 {
		t.Errorf("a second login minted a second kernel: %v", cfg.Kernels)
	}
	if cfg.Current != "agent@work" {
		t.Errorf("current: got %q", cfg.Current)
	}
	if err := selectLogin(context.Background(), login{Handle: "x", Kernel: "nosuch"}); err == nil {
		t.Error("a login on an unknown kernel must be refused")
	}
}

// TestForgettingAKernelTakesItsLoginsWithIt: a record this client no longer keeps leaves no
// credential behind that could be sent anywhere, and nothing is left selected on it.
func TestForgettingAKernelTakesItsLoginsWithIt(t *testing.T) {
	clientHomeFor(t)
	recordLogin(t, "alice@work", "http://kernel:4040", "KEY")
	if err := saveToken("SECRET"); err != nil {
		t.Fatal(err)
	}
	forgetLogins("work")
	if _, err := os.Stat(credPath(t, "alice@work")); !os.IsNotExist(err) {
		t.Errorf("the credential file survived: %v", err)
	}
}

// TestLoginNameCannotEscapeItsDirectory: a login is one file under credentials/, so a name that is
// not two ordinary names is refused where a name becomes a path. Without this, a crafted JUICE_AS
// points the credential write at the client's own records and destroys them.
func TestLoginNameCannotEscapeItsDirectory(t *testing.T) {
	clientHomeFor(t)
	recordLogin(t, "alice@real", "http://kernel:4040", "KEY")
	before, err := os.ReadFile(clientConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"../config@real", "a/b@real", ".hidden@real", "alice@../x", "alice", "@real", "alice@", strings.Repeat("x", 65) + "@real"} {
		if _, err := parseLogin(bad); err == nil {
			t.Errorf("parseLogin(%q) was allowed", bad)
		}
		t.Setenv("JUICE_AS", bad)
		if err := saveToken("PWNED"); err == nil {
			t.Errorf("a credential was stored under the name %q", bad)
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

// TestContextsMigrateToLogins pins the one-time conversion: a context that recorded whose login it
// held becomes that login and keeps its session; one that never logged in holds tokens nobody can
// name, so the file is kept aside rather than guessed at or deleted; and two contexts that would
// become one login never merge — the selected one keeps the name.
func TestContextsMigrateToLogins(t *testing.T) {
	clientHomeFor(t)
	if err := os.MkdirAll(credentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	blob := `{"current":"work","kernels":{"work":{"endpoint":"http://kernel:4040","public_key":"KEY","network":"play"}},
	  "contexts":{"work":{"kernel":"work","handle":"alice"},
	              "dup":{"kernel":"work","handle":"alice"},
	              "never":{"kernel":"work"}}}`
	if err := os.MkdirAll(clientHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientConfigPath(), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"work", "dup", "never"} {
		body := `{"token":"TOK-` + name + `"}`
		if err := os.WriteFile(filepath.Join(credentialsDir(), name+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cfg := loadClientConfig()
	if cfg.Current != "alice@work" {
		t.Fatalf("current: got %q, want alice@work", cfg.Current)
	}
	if k := cfg.Kernels["work"]; k == nil || k.PublicKey != "KEY" {
		t.Fatalf("kernel not carried over: %+v", k)
	}
	if tok, err := loadToken(); err != nil || tok != "TOK-work" {
		t.Fatalf("the selected context's session did not migrate: %q %v", tok, err)
	}
	for _, name := range []string{"dup", "never"} {
		if _, err := os.Stat(filepath.Join(credentialsDir(), name+".json.unmigrated")); err != nil {
			t.Errorf("%s was not kept aside: %v", name, err)
		}
	}
	if held := logins(); len(held) != 1 || held[0].String() != "alice@work" {
		t.Errorf("logins after migration: %v", held)
	}
	// Running again reads the new records and does not migrate a second time.
	if again := loadClientConfig(); again.Current != "alice@work" {
		t.Fatalf("second load: %+v", again)
	}
}

// TestLegacyProfilesMigrate pins the older conversion: each profile becomes a kernel with its
// pinned key, and the old file is kept under a new name so nothing is destroyed by a client that
// ran once. A profile never recorded whose session it held, so the session is kept aside.
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
	k := cfg.Kernels["prod"]
	if k == nil || k.Endpoint != "http://kernel:4040" || k.PublicKey != "KEY" || k.Decimals != 2 {
		t.Fatalf("kernel not migrated: %+v", k)
	}
	if cfg.Kernels["spare"] == nil {
		t.Error("a profile holding no login still names a kernel")
	}
	if _, err := os.Stat(filepath.Join(credentialsDir(), "prod.json.unmigrated")); err != nil {
		t.Errorf("the session was not kept aside: %v", err)
	}
	if _, err := os.Stat(filepath.Join(credentialsDir(), "spare.json")); err == nil {
		t.Error("a profile holding no login must not get a credential file")
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Error("the old file was left in place; it must be renamed so it migrates once")
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Errorf("the old file was destroyed rather than kept: %v", err)
	}
	// Running again reads the new records and does not migrate a second time.
	if again := loadClientConfig(); len(again.Kernels) != 2 {
		t.Fatalf("second load: %+v", again)
	}
}

// TestExistingRecordsAreNeverOverwrittenByMigration: a client that already has its records keeps
// them, whatever an old profiles.json beside them says.
func TestExistingRecordsAreNeverOverwrittenByMigration(t *testing.T) {
	clientHomeFor(t)
	cfg := loadClientConfig()
	cfg.Kernels["mine"] = &kernelRec{Endpoint: "http://mine:4040", PublicKey: "MINE"}
	cfg.Current = "alice@mine"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(clientHome(), "profiles.json")
	if err := os.WriteFile(legacy, []byte(`{"active":"old","profiles":{"old":{"endpoint":"http://old:1"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadClientConfig()
	if got.Current != "alice@mine" || got.Kernels["old"] != nil {
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
	}{
		{"5", 0, 5},
		{"1000000", 0, 1000000},
		{"1.25", 2, 125},
		{"1.2", 2, 120},
		{"0.01", 2, 1},
		{"12", 2, 1200},
		{"0.000001", 6, 1},
	}
	for _, tc := range cases {
		got, err := parseAmount(tc.in, tc.decimals)
		if err != nil || got != tc.want {
			t.Fatalf("parseAmount(%q, %d): got %d, %v; want %d", tc.in, tc.decimals, got, err, tc.want)
		}
		// What is shown pastes back in: the renderer and the parser are one round trip, which is
		// why the renderer writes no separators.
		net := kernel.Network{Decimals: tc.decimals}
		if back, err := parseAmount(net.Amount(got), tc.decimals); err != nil || back != got {
			t.Errorf("round trip of %s: got %d, %v", net.Amount(got), back, err)
		}
	}
	for _, bad := range []string{"", "-1", "0", "1.234", "1,000", "abc", "1e3", " 1 . 5"} {
		if _, err := parseAmount(bad, 2); err == nil {
			t.Errorf("parseAmount(%q) was accepted", bad)
		}
	}
	// A price is the same reading with one rule lifted: nothing is a price a provider may set, and
	// every way of writing nothing reads the same. Everything else stays refused, so a free action
	// and a malformed one are never confused.
	for _, zero := range []string{"0", "0.00", "0.000000"} {
		got, err := parseUnits(zero, 6)
		if err != nil || got != 0 {
			t.Errorf("parseUnits(%q, 6) = %d, %v; want 0", zero, got, err)
		}
	}
	for _, bad := range []string{"", "-1", "1.234", "abc"} {
		if _, err := parseUnits(bad, 2); err == nil {
			t.Errorf("parseUnits(%q) was accepted", bad)
		}
	}
}

// loadToken reads the selected login's access token. Production code asks tokenFor, which also
// decides whether the token may travel to the address in hand; the tests want the stored value
// alone, so this stays here rather than as an unused export.
func loadToken() (string, error) {
	l, _, err := selected()
	if err != nil {
		return "", err
	}
	if c := readCredentials(l); c.Token != "" {
		return c.Token, nil
	}
	return "", kernel.ErrUnauthenticated.Wrap("not logged in")
}

// The tests drive a session directly where production would log in and out; these keep that within
// the test binary rather than as production code no command calls.
func removeToken() error {
	return onSelected(func(c *credentials) (bool, error) { c.Token = ""; return true, nil })
}

func loadRefreshToken() (string, error) {
	l, _, err := selected()
	if err != nil {
		return "", err
	}
	if c := readCredentials(l); c.RefreshToken != "" {
		return c.RefreshToken, nil
	}
	return "", os.ErrNotExist
}

func removeRefreshToken() error {
	return onSelected(func(c *credentials) (bool, error) { c.RefreshToken = ""; return true, nil })
}

// TestAPasswordGoesOnlyToTheKernelItWasRecordedFor: logging in checks which kernel is answering
// before it asks for a password, let alone sends one. A server that has taken over a recorded
// address must not be handed the credential and refused afterwards.
func TestAPasswordGoesOnlyToTheKernelItWasRecordedFor(t *testing.T) {
	clientHomeFor(t)
	asked, reached := false, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "handle": "k", "public_key": "KEY-B", "network": "play", "network_digest": "D"})
			return
		}
		reached = true
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	t.Cleanup(srv.Close)

	cfg := loadClientConfig()
	cfg.Kernels["work"] = &kernelRec{Endpoint: srv.URL, PublicKey: "KEY-A", WorldDigest: "D", Network: "play"}
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	oldPrompt := promptPassword
	promptPassword = func(string) (string, error) { asked = true; return "secret", nil }
	t.Cleanup(func() { promptPassword = oldPrompt })
	oldServer := flagServer
	t.Cleanup(func() { flagServer = oldServer })

	if _, err := execTestCmd(t, loginCmd(), "alice@work"); err == nil {
		t.Fatal("a login on a kernel that is not the recorded one succeeded")
	}
	if asked {
		t.Error("the password was asked for before it was known where it would go")
	}
	if reached {
		t.Error("the password was sent to a server that is not the kernel recorded here")
	}
	// The same holds when the password needs no prompt: it is the sending that must not happen.
	flagServer = ""
	if _, err := execTestCmd(t, loginCmd(), "alice@work", "--password", "secret"); err == nil {
		t.Fatal("a login on a kernel that is not the recorded one succeeded")
	}
	if reached {
		t.Error("a password given on the command line was sent to the wrong kernel")
	}
}

// TestLoggingOutANamedLoginEndsItOnItsOwnKernel: a session lives on the kernel that issued it, so
// logging one out must reach that kernel — not whichever login happens to be selected here. When
// it cannot be reached the credentials still go, and the operator is told what is actually true:
// the session there may stand until it expires, and running the command again cannot end it.
func TestLoggingOutANamedLoginEndsItOnItsOwnKernel(t *testing.T) {
	clientHomeFor(t)
	revoked := ""
	home := healthServer(t, "KEY-A", "D-A", "play")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "handle": "other", "public_key": "KEY-B", "network": "play", "network_digest": "D-B"})
			return
		}
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		revoked = req.RefreshToken
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(other.Close)

	recordLogin(t, "alice@home", home.URL, "KEY-A")
	cfg := loadClientConfig()
	cfg.Kernels["away"] = &kernelRec{Endpoint: other.URL, PublicKey: "KEY-B", WorldDigest: "D-B", Network: "play"}
	cfg.Current = "alice@home"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUICE_AS", "bob@away")
	if err := saveRefreshToken("BOB-REFRESH"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUICE_AS", "")
	oldServer := flagServer
	flagServer = ""
	t.Cleanup(func() { flagServer = oldServer })

	if _, err := execTestCmd(t, logoutCmd(), "bob@away"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if revoked != "BOB-REFRESH" {
		t.Errorf("the session was not ended on its own kernel: revoked %q", revoked)
	}
	if _, err := os.Stat(credPath(t, "bob@away")); !os.IsNotExist(err) {
		t.Error("the credentials stayed on this computer")
	}
	if loadClientConfig().Current != "alice@home" {
		t.Error("logging out a named login changed which login is selected")
	}

	// A kernel that cannot be reached: the credentials still go, and the report says so.
	t.Setenv("JUICE_AS", "carol@away")
	if err := saveRefreshToken("CAROL-REFRESH"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUICE_AS", "")
	other.Close()
	flagServer = ""
	out := captureStdout(t, func() error { _, err := execTestCmd(t, logoutCmd(), "carol@away"); return err })
	if !strings.Contains(out, "could not be reached") || !strings.Contains(out, "may remain valid") {
		t.Errorf("an unrevoked session was reported as a clean logout: %q", out)
	}
	if _, err := os.Stat(credPath(t, "carol@away")); !os.IsNotExist(err) {
		t.Error("credentials this client will not send again were kept")
	}
}

// TestHealthRefusesSomethingThatIsNotAKernel: `kernel health` answers "which kernel is this?", so a
// 200 carrying anything else is a failure. It once printed `ok` with every field blank.
func TestHealthRefusesSomethingThatIsNotAKernel(t *testing.T) {
	clientHomeFor(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	t.Cleanup(srv.Close)
	cfg := loadClientConfig()
	cfg.Kernels["w"] = &kernelRec{Endpoint: srv.URL, Network: "play"}
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	out, err := execTestCmd(t, kernelHealthCmd(), "w")
	if err == nil {
		t.Fatalf("a server that is not a kernel reported healthy: %q", out)
	}
	if !strings.Contains(err.Error(), "not a juice kernel") {
		t.Errorf("unhelpful refusal: %v", err)
	}
}

// TestACommandWithNoLoginSaysSo: a command that shows money reads the world's unit before it acts,
// and with no login there is no address to read it from. What the caller must hear is which login
// to make — not that a unit could not be read, which is true but useless.
func TestACommandWithNoLoginSaysSo(t *testing.T) {
	clientHomeFor(t)
	old := flagServer
	flagServer = ""
	t.Cleanup(func() { flagServer = old })
	t.Setenv("JUICE_AS", "alice@nosuch")

	for _, c := range []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{"a read that shows money", userMeCmd(), nil},
		{"a list that shows money", actionListCmd(), nil},
		{"a write that takes money", userTransferCmd(), []string{"bob", "5"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := execTestCmd(t, c.cmd, c.args...)
			if err == nil {
				t.Fatal("a command with no login went ahead")
			}
			if !strings.Contains(err.Error(), "no kernel named nosuch") {
				t.Errorf("the refusal does not name what to fix: %v", err)
			}
		})
	}
}

// TestALoginRefusalSaysWhichRefusalItIs: a wrong password and a suspended account are different
// answers and lead to different remedies. The server knows which is which, so its words are what
// the operator reads; a client that rewrote both into one would send a suspended user to reset a
// password that was never wrong.
func TestALoginRefusalSaysWhichRefusalItIs(t *testing.T) {
	clientHomeFor(t)
	env := newTestEnv(t)
	if _, err := env.k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "alice", Password: "correct-horse"}); err != nil {
		t.Fatal(err)
	}
	base := flagServer
	cfg := loadClientConfig()
	cfg.Kernels["k"] = &kernelRec{Endpoint: base, Network: "play"}
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := execTestCmd(t, loginCmd(), "alice@k", "--password", "wrong"); err == nil ||
		!strings.Contains(err.Error(), "wrong user name or password") {
		t.Errorf("a wrong password did not say so: %v", err)
	}
	sys, err := env.k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: kernel.SuperuserHandle, Password: "sys-pass"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := env.k.ReadUserByHandle(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.k.SuspendUser(context.Background(), sys.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := execTestCmd(t, loginCmd(), "alice@k", "--password", "correct-horse"); err == nil ||
		!strings.Contains(err.Error(), "suspended") {
		t.Errorf("a suspended account was reported as a bad password: %v", err)
	}
}

// TestTheClientsOwnRecordsAnswerUnderBothFlags: "--json and --quiet mean the same thing on every
// command" covers the commands that write this client's own records too. They talk to no server —
// which is exactly why they were the ones still printing whatever they liked.
func TestTheClientsOwnRecordsAnswerUnderBothFlags(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "D-A", "play")
	old := flagServer
	flagServer = ""
	t.Cleanup(func() { flagServer = old })

	// Each command is run twice, so each restores what it consumes: a login to end, a kernel to
	// forget. The point is the two answers, not the second act.
	restore := func() {
		resetHealthCache()
		recordLogin(t, "alice@work", srv.URL, "KEY-A")
		t.Setenv("JUICE_AS", "alice@work")
		if err := saveRefreshToken("R"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("JUICE_AS", "")
		if _, _, _, err := registerKernel(context.Background(), "k2", srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		cmd  func() *cobra.Command
		args []string
		id   string // an id --quiet must print, one per line
	}{
		{"registering a kernel", kernelAddCmd, []string{srv.URL, "k2"}, "k2"},
		{"listing the kernels known", kernelListCmd, nil, "k2"},
		{"switching to a login held", authUseCmd, []string{"alice@work"}, "alice@work"},
		{"listing the logins held", authListCmd, nil, "alice@work"},
		{"ending a login", logoutCmd, []string{"alice@work"}, "alice@work"},
		{"forgetting a kernel", kernelForgetCmd, []string{"k2", "--yes"}, "k2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := func(flag *bool) string {
				restore()
				old := *flag
				*flag = true
				defer func() { *flag = old }()
				return captureStdout(t, func() error { _, err := execTestCmd(t, c.cmd(), c.args...); return err })
			}
			var reply any
			if out := run(&flagJSON); json.Unmarshal([]byte(out), &reply) != nil {
				t.Fatalf("--json did not print JSON:\n%s", out)
			}
			lines := strings.Fields(run(&flagQuiet))
			if len(lines) == 0 || !slices.Contains(lines, c.id) {
				t.Errorf("--quiet = %v, want it to name %q and nothing else", lines, c.id)
			}
		})
	}
}
