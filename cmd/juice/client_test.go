// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
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

// stubServer starts an httptest server, points the CLI client at it via flagServer, and
// isolates credential storage in a temp home. Everything resets at test end.
func stubServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	return stubKernel(t, 0, h)
}

// stubKernel is stubServer with the world's decimals named: a kernel whose money has decimal
// places is what shows whether an amount was written in the world's unit or in base units (D20).
func stubKernel(t *testing.T, decimals uint8, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	// The identity banner is what a client reads before it trusts a server or scales its money, so
	// a stub kernel answers it; everything else is the test's own handler.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "handle": "stub", "public_key": "stub-key",
				"network": "play", "decimals": decimals, "symbol": "credits",
			})
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	old := flagServer
	flagServer = srv.URL
	t.Cleanup(func() { flagServer = old })
	t.Setenv("JUICE_HOME", t.TempDir())
	selectTestLogin(t, "tester@stub", srv.URL)
	return srv
}

func TestServerBaseURL(t *testing.T) {
	// A profile file left by a real install must not decide what this test sees.
	t.Setenv("JUICE_HOME", t.TempDir())
	oldServer := flagServer
	t.Cleanup(func() { flagServer = oldServer })

	// --server is the only override: no env var, no config key (§14). With nothing selected there
	// is no address at all — a client that has not been told where to go says so rather than
	// dialling localhost, which could be a kernel its caller never named.
	flagServer = ""
	t.Setenv("JUICE_SERVER", "http://env:2")
	if got := freshClient().base; got != "" {
		t.Fatalf("no login selected: got %q, want no address", got)
	}
	flagServer = "http://flag:3/"
	if got := freshClient().base; got != "http://flag:3" {
		t.Fatalf("flag: got %q", got)
	}
}

func TestAPICallSuccess(t *testing.T) {
	stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/thing" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "n": 5})
	})
	var out struct {
		OK bool `json:"ok"`
		N  int  `json:"n"`
	}
	if err := freshClient().call(context.Background(), "GET", "/v1/thing", nil, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.N != 5 {
		t.Fatalf("got %+v", out)
	}
}

func TestAPICallErrorMapsCode(t *testing.T) {
	stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no such action", "code": "not_found"})
	})
	err := freshClient().call(context.Background(), "GET", "/v1/x", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if kernel.KernelErrorCode(err) != "not_found" {
		t.Fatalf("code: %s", kernel.KernelErrorCode(err))
	}
	if exitCodeFor(err) != 4 {
		t.Fatalf("exit code: %d", exitCodeFor(err))
	}
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Fatal("errors.Is(ErrNotFound) should hold")
	}
}

// TestExitCodesAreStableAndDistinct: every error class a script branches on has its own code (§14),
// so a caller can act on the outcome without parsing prose. terms_changed in particular must not
// share the catch-all 1 with an internal error: it means re-quote and retry.
func TestExitCodesAreStableAndDistinct(t *testing.T) {
	want := map[string]int{
		"unauthenticated": 2, "unauthorized": 3, "not_found": 4, "invalid_input": 5,
		"schema_violation": 5, "insufficient_funds": 6, "timeout": 7, "grant_required": 8,
		"peer_unreachable": 9, "peer_unfunded": 10, "terms_changed": 11,
	}
	for code, exit := range want {
		err := (&kernel.KernelError{Code: code, Message: code}).Wrap("x")
		if got := exitCodeFor(err); got != exit {
			t.Errorf("%s: exit code %d, want %d", code, got, exit)
		}
	}
	if got := exitCodeFor(kernel.ErrInternal.Wrap("boom")); got != 1 {
		t.Errorf("internal error exit code %d, want the catch-all 1", got)
	}
}

// TestUnauthenticatedWithNoTokenSuggestsLogin: the server can only say "missing bearer token",
// which tells the user nothing to do. With no token stored, the CLI answers with the actionable
// local hint instead (§14).
func TestUnauthenticatedWithNoTokenSuggestsLogin(t *testing.T) {
	stubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing bearer token", "code": "unauthenticated"})
	})
	t.Setenv("JUICE_HOME", t.TempDir()) // no records, no session
	err := freshClient().call(context.Background(), "GET", "/v1/me", nil, nil)
	if err == nil || !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
	if !strings.Contains(err.Error(), "juice auth login") {
		t.Errorf("the hint must name the command to run, got %q", err.Error())
	}
}

// TestAPICallUnreachable: a kernel that does not answer is one condition with one code, whichever
// command met it and whatever it wanted — the CLI used to report a money-unit problem here and an
// unreachable peer there, for the same dead server (§14).
func TestAPICallUnreachable(t *testing.T) {
	old := flagServer
	t.Cleanup(func() { flagServer = old })
	flagServer = "http://127.0.0.1:1" // nothing listening
	t.Setenv("HOME", t.TempDir())
	for _, c := range []struct {
		name string
		call func(*client) error
	}{
		{"a request", func(c *client) error { return c.call(context.Background(), "GET", "/v1/x", nil, nil) }},
		{"reading the money unit", func(c *client) error { _, err := c.network(context.Background()); return err }},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(freshClient())
			if err == nil || kernel.KernelErrorCode(err) != "peer_unreachable" {
				t.Fatalf("expected peer_unreachable, got %v", err)
			}
			if !strings.Contains(err.Error(), "cannot reach") {
				t.Errorf("the message does not say what happened: %q", err)
			}
		})
	}
}

func TestAPICallRefreshOn401(t *testing.T) {
	stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/refresh":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "new-access", "refresh_token": "new-refresh"})
		case "/v1/thing":
			if r.Header.Get("Authorization") != "Bearer new-access" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "expired", "code": "unauthenticated"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	})
	if err := saveToken("stale"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefreshToken("rt"); err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := freshClient().call(context.Background(), "GET", "/v1/thing", nil, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatal("expected ok after refresh-and-retry")
	}
	if tok, _ := loadToken(); tok != "new-access" {
		t.Fatalf("rotated token not persisted: %q", tok)
	}
}

// TestRunCommandPostsToServer proves `run` reads the action, pins the terms that read returned,
// and posts {action, args, quote_hash} to /v1/run — so the price a caller was shown is the price
// the call is authorised at (U8).
func TestRunCommandPostsToServer(t *testing.T) {
	var gotAction, gotPin string
	var gotArgs map[string]any
	stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/actions") {
			// The read the client makes before it commits money: the reference resolves to a
			// row, and the row states the terms.
			if r.URL.Query().Get("ref") != "" {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "act-1", "action": "a/b"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "act-1", "quote_hash": "h-1", "price": 5})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/run" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			Action    string         `json:"action"`
			Args      map[string]any `json:"args"`
			QuoteHash string         `json:"quote_hash"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotAction, gotArgs, gotPin = req.Action, req.Args, req.QuoteHash
		_ = json.NewEncoder(w).Encode(map[string]any{"tx_id": "tx-1", "result": map[string]any{"ok": true}})
	})
	if _, err := execTestCmd(t, runCmd(), "a/b", `{"x":1}`); err != nil {
		t.Fatal(err)
	}
	if gotAction != "a/b" {
		t.Fatalf("action = %q", gotAction)
	}
	if gotPin != "h-1" {
		t.Errorf("quote_hash = %q, want the hash the client read (h-1)", gotPin)
	}
	if gotArgs["x"].(float64) != 1 {
		t.Fatalf("args = %v", gotArgs)
	}
}

// TestActionCreateBinaryWasmRoutesToArtifact verifies a binary (non-UTF-8) wasm --source is
// sent base64-encoded via wasm_artifact, not in the JSON source string (which would corrupt it).
func TestActionCreateBinaryWasmRoutesToArtifact(t *testing.T) {
	binary := []byte{0x00, 0x61, 0x73, 0x6d, 0x80, 0xff} // 0x80/0xff → invalid UTF-8
	f := filepath.Join(t.TempDir(), "m.wasm")
	if err := os.WriteFile(f, binary, 0o644); err != nil {
		t.Fatal(err)
	}
	var gotSource, gotArtifact string
	stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Source       string `json:"source"`
			WasmArtifact string `json:"wasm_artifact"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotSource, gotArtifact = req.Source, req.WasmArtifact
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "action": "a/m"})
	})
	if _, err := execTestCmd(t, actionCreateCmd(), "m",
		"--kind", "wasm", "--source", f, "--price", "0", "--description", "d"); err != nil {
		t.Fatal(err)
	}
	if gotSource != "" {
		t.Fatalf("binary wasm must not ride in source: %q", gotSource)
	}
	if gotArtifact != base64.StdEncoding.EncodeToString(binary) {
		t.Fatalf("wasm_artifact = %q, want base64 of the binary module", gotArtifact)
	}
}

// TestArtifactFileIsEncoded: --artifact accepts a path, and a path names bytes — the same rule
// --source follows — so the file is always encoded. The routing must NOT depend on whether the
// bytes happen to parse as UTF-8: a minimal WASM module is entirely below 0x80 (`\0asm\1\0\0\0`),
// so a content-sniffing rule sends one module encoded and the next one raw. Literal base64 is the
// non-file case.
func TestArtifactFileIsEncoded(t *testing.T) {
	dir := t.TempDir()
	send := func(t *testing.T, artifact string) string {
		t.Helper()
		var got string
		stubServer(t, func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				WasmArtifact string `json:"wasm_artifact"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			got = req.WasmArtifact
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "action": "a/m"})
		})
		if _, err := execTestCmd(t, actionCreateCmd(), "m",
			"--kind", "wasm", "--artifact", artifact, "--price", "0", "--description", "d"); err != nil {
			t.Fatal(err)
		}
		return got
	}
	write := func(name string, b []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// The case a UTF-8 check gets wrong: a valid module whose every byte is ASCII-range.
	asciiModule := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	if got, want := send(t, write("ascii.wasm", asciiModule)), base64.StdEncoding.EncodeToString(asciiModule); got != want {
		t.Errorf("an all-ASCII wasm module must still be encoded: got %q, want %q", got, want)
	}
	// And the case it gets right, so both go down one path.
	highModule := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x80, 0xff}
	if got, want := send(t, write("high.wasm", highModule)), base64.StdEncoding.EncodeToString(highModule); got != want {
		t.Errorf("a non-UTF-8 wasm module must be encoded: got %q, want %q", got, want)
	}
	// A literal base64 value is not a path, so it passes through untouched.
	lit := base64.StdEncoding.EncodeToString(asciiModule)
	if got := send(t, lit); got != lit {
		t.Errorf("a literal base64 value must pass through: got %q, want %q", got, lit)
	}
}

// TestActionUpdateSendsArtifact verifies `action update --artifact` carries the base64 artifact
// via wasm_artifact on the PUT, symmetric with create (item 2).
func TestActionUpdateSendsArtifact(t *testing.T) {
	artifactB64 := base64.StdEncoding.EncodeToString([]byte{0x00, 0x61, 0x73, 0x6d})
	var gotMethod, gotPath, gotArtifact string
	stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		var req struct {
			WasmArtifact string `json:"wasm_artifact"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotArtifact = req.WasmArtifact
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "act1", "action": "a/m"}})
	})
	if _, err := execTestCmd(t, actionUpdateCmd(), "act1", "--artifact", artifactB64); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/actions" {
		t.Fatalf("got %s %s, want PUT /v1/actions", gotMethod, gotPath)
	}
	if gotArtifact != artifactB64 {
		t.Fatalf("wasm_artifact = %q, want %q", gotArtifact, artifactB64)
	}
}

func TestErrorFromResponseNonJSON(t *testing.T) {
	err := errorFromResponse(500, []byte("boom"))
	if kernel.KernelErrorCode(err) != "internal" {
		t.Fatalf("code %s", kernel.KernelErrorCode(err))
	}
	if err.Error() != "boom" {
		t.Fatalf("msg %q", err.Error())
	}
}

func TestIsTimeoutErr(t *testing.T) {
	// A client-side timeout (context deadline) is classified as a timeout, even after the KernelError
	// wrapping doHTTP applies (message + Because cause).
	wrapped := kernel.ErrExecutionFailed.Wrapf("HTTP call failed: %v", context.DeadlineExceeded).Because(context.DeadlineExceeded)
	if !isTimeoutErr(wrapped) {
		t.Error("context.DeadlineExceeded should classify as timeout")
	}
	// A net.Error whose Timeout() is true also classifies.
	if !isTimeoutErr(&net.OpError{Op: "dial", Err: timeoutError{}}) {
		t.Error("net timeout should classify as timeout")
	}
	// A plain connection error does not.
	if isTimeoutErr(errors.New("connection refused")) {
		t.Error("connection refused must NOT classify as timeout")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// healthServer serves one identity banner, the thing a client records a kernel by.
func healthServer(t *testing.T, key, fingerprint, network string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			// Anything else reports whether the client offered its token.
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": r.Header.Get("Authorization")})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "handle": "k", "public_key": key,
			"network": network, "network_fingerprint": fingerprint, "decimals": 0,
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

// resetClient starts a fresh client, the way an invocation does: a test that changes the records,
// the selection or the server under a command is a new invocation as far as the client is
// concerned, and reads them again.
func resetClient() { cli = &client{} }

// resetHealthCache is resetClient under the name the banner cache had when it was a global.
func resetHealthCache() { resetClient() }

// freshClient is what an invocation gets: a client that reads the records as they are now.
func freshClient() *client { resetClient(); return cli.resolve() }

// answerYes puts a "y" on stdin, which is what someone at the terminal does.
func answerYes(t *testing.T) {
	t.Helper()
	in, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = in
	t.Cleanup(func() { os.Stdin = old })
}

// mustClientFor is clientFor on a login a test names, which must exist for the test to mean
// anything.
func mustClientFor(t *testing.T, name string) *client {
	t.Helper()
	_, c, err := namedClient(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// saveToken, saveRefreshToken and onSelected arrange a session under the selected login, which is
// how a test sets one up. Production writes a session in one place only — the login exchange,
// under the login it just proved (client.loginPKCE).
func saveToken(tok string) error {
	return onSelected(func(c *credentials) (bool, error) { c.Token = tok; return true, nil })
}

func saveRefreshToken(tok string) error {
	return onSelected(func(c *credentials) (bool, error) { c.RefreshToken = tok; return true, nil })
}

func onSelected(fn func(*credentials) (bool, error)) error {
	l, err := freshClient().identity()
	if err != nil {
		return err
	}
	return withCredentials(l, fn)
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
	wantK := &kernelRec{Endpoint: "http://kernel:4040", PublicKey: "KEY", WorldFingerprint: "DIGEST",
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
	c := freshClient()
	l, k, err := c.login, c.kernel, c.err
	if err != nil {
		t.Fatal(err)
	}
	if l.Handle != "alice" || l.Kernel != "prod" || k.Endpoint != "http://kernel:4040" {
		t.Fatalf("selected: %+v %+v", l, k)
	}
	if freshClient().base != "http://kernel:4040" {
		t.Errorf("the selected login's kernel is not what commands address: %q", freshClient().base)
	}
}

// TestSelectionNeverFallsBack: a selector that names no login here is an error. A misspelled --as
// must not quietly act as somebody else, and with nothing selected there is no address to guess at.
func TestSelectionNeverFallsBack(t *testing.T) {
	clientHomeFor(t)
	if _, err := freshClient().identity(); err == nil {
		t.Fatal("a client with no login selected something")
	}
	if freshClient().base != "" {
		t.Errorf("a client with no login addresses %q", freshClient().base)
	}
	recordLogin(t, "alice@prod", "http://kernel:4040", "KEY")
	flagAs = "alice@nosuch"
	t.Cleanup(func() { flagAs = "" })
	if _, err := freshClient().identity(); err == nil {
		t.Error("--as naming an unknown kernel was accepted")
	}
	flagAs = "alice"
	if _, err := freshClient().identity(); err == nil {
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
	if _, err := freshClient().identity(); err == nil {
		t.Error("a known kernel with no login was treated as one")
	}
}

// TestAsEnvOverride pins JUICE_AS: it names the login for one invocation, including where that
// invocation's credentials are read and written, without switching the selected one.
func TestAsEnvOverride(t *testing.T) {
	clientHomeFor(t)
	recordLogin(t, "alice@prod", "http://kernel:4040", "KEY")
	t.Setenv("JUICE_AS", "bot@prod")

	if l, err := freshClient().identity(); err != nil || l.String() != "bot@prod" {
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
	if c := freshClient(); c.err != nil || c.kernel.Endpoint != "http://kernel:4040" {
		t.Fatalf("login does not resolve its kernel: %+v %v", c.kernel, c.err)
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
	clients := make([]*client, len(results))
	for i := range clients {
		clients[i] = freshClient()
	}
	resetClient()
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := clients[i].refresh(context.Background(), "OLD")
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			results[i] = ok
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
	if k.PublicKey != "KEY-A" || k.WorldFingerprint != "DIGEST-A" || k.Network != "play" {
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
	if err := mustClientFor(t, "alice@k").selectLogin(context.Background()); err == nil {
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
	cfg.Kernels["prod"] = &kernelRec{Endpoint: srv.URL, PublicKey: "KEY-A", WorldFingerprint: "DIGEST-A", Network: "play"}
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mustClientFor(t, "alice@prod").selectLogin(context.Background()); err == nil {
		t.Fatal("expected a refusal for a different network fingerprint")
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
		if err := freshClient().call(context.Background(), "GET", "/v1/thing", nil, &out); err != nil {
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
	_ = freshClient().call(context.Background(), "GET", "/v1/thing", nil, nil)
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
		if err := mustClientFor(t, name).selectLogin(context.Background()); err != nil {
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
	if _, _, err := namedClient("x@nosuch"); err == nil {
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

// loadToken reads the selected login's access token. Production code asks the client, which also
// decides whether the token may travel to the address in hand; the tests want the stored value
// alone, so this stays here rather than as an unused export.
func loadToken() (string, error) {
	l, err := freshClient().identity()
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
	l, err := freshClient().identity()
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
				"status": "ok", "handle": "k", "public_key": "KEY-B", "network": "play", "network_fingerprint": "D"})
			return
		}
		reached = true
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	t.Cleanup(srv.Close)

	cfg := loadClientConfig()
	cfg.Kernels["work"] = &kernelRec{Endpoint: srv.URL, PublicKey: "KEY-A", WorldFingerprint: "D", Network: "play"}
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
				"status": "ok", "handle": "other", "public_key": "KEY-B", "network": "play", "network_fingerprint": "D-B"})
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
	cfg.Kernels["away"] = &kernelRec{Endpoint: other.URL, PublicKey: "KEY-B", WorldFingerprint: "D-B", Network: "play"}
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
	// A second kernel, because one key is one record: registering the first server again under
	// another name renames it rather than adding a second (D15).
	other := healthServer(t, "KEY-B", "D-B", "play")
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
		if _, _, _, err := registerKernel(context.Background(), "k2", other.URL); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		cmd  func() *cobra.Command
		args []string
		id   string // an id --quiet must print, one per line
	}{
		{"registering a kernel", kernelAddCmd, []string{other.URL, "k2"}, "k2"},
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

// TestALoginIsStoredUnderTheLoginItProved: logging in writes the session of the account it just
// authenticated — never of whatever --as or JUICE_AS happens to name. Storing it through the
// selection put one account's tokens, and then another account's principal id, into a third's
// file, after which a command naming one of them acted as another.
func TestALoginIsStoredUnderTheLoginItProved(t *testing.T) {
	clientHomeFor(t)
	var handles []string
	srv := loginServer(t, &handles)
	recordLogin(t, "bob@k", srv.URL, "KEY")
	t.Setenv("JUICE_AS", "bob@k")
	if err := saveToken("BOB-TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefreshToken("BOB-REFRESH"); err != nil {
		t.Fatal(err)
	}

	if _, err := execTestCmd(t, loginCmd(), "alice@k", "--password", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	alice, bob := readCredentials(login{"alice", "k"}), readCredentials(login{"bob", "k"})
	if alice.Token != "ALICE-ACCESS" || alice.RefreshToken != "ALICE-REFRESH" {
		t.Errorf("the session went somewhere else: %+v", alice)
	}
	if alice.PrincipalID != "alice-id" {
		t.Errorf("principal id beside the tokens: got %q, want alice's", alice.PrincipalID)
	}
	if bob.Token != "BOB-TOKEN" || bob.RefreshToken != "BOB-REFRESH" || bob.PrincipalID != "" {
		t.Errorf("the login named by JUICE_AS was written to: %+v", bob)
	}
	if got := loadClientConfig().Current; got != "alice@k" {
		t.Errorf("selected %q after logging in as alice@k", got)
	}
	// /v1/me was asked as Alice, which is the only reason her id is hers.
	if len(handles) == 0 || handles[len(handles)-1] != "ALICE-ACCESS" {
		t.Errorf("the principal was read as %v, not as the login just proved", handles)
	}
}

// loginServer answers the whole login exchange, recording the bearer each authenticated request
// arrives with — which is what says whom a request was made as.
func loginServer(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "handle": "k", "public_key": "KEY", "network": "play", "network_fingerprint": "D"})
		case "/v1/auth/authorize":
			// The real server redirects the browser to the client's loopback listener, which is
			// how the code reaches it; doHTTP follows that redirect.
			var req struct {
				RedirectURI string `json:"redirect_uri"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			http.Redirect(w, r, req.RedirectURI+"?code=CODE", http.StatusFound)
		case "/v1/auth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "ALICE-ACCESS", "refresh_token": "ALICE-REFRESH"})
		default:
			*seen = append(*seen, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "alice-id", "handle": "alice"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestACommandFinishesAsTheLoginItStartedWith: one command is one identity. A withdrawal reads the
// money unit, reads the account, asks, and only then pays — and another terminal switching logins
// between those requests must not make the prompt name one account and the payment act as another.
func TestACommandFinishesAsTheLoginItStartedWith(t *testing.T) {
	clientHomeFor(t)
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			// The switch happens between the unit read and everything after it.
			cfg := loadClientConfig()
			cfg.Current = "bob@k"
			_ = saveClientConfig(cfg)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "handle": "k", "public_key": "KEY",
				"network": "play", "network_fingerprint": "D", "decimals": 6, "symbol": "credits"})
			return
		}
		sent = append(sent, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u", "blockchain_address": "0xabc", "amount": 1000000})
	}))
	t.Cleanup(srv.Close)
	recordLogin(t, "alice@k", srv.URL, "KEY")
	if err := saveToken("ALICE"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUICE_AS", "bob@k")
	if err := saveToken("BOB"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JUICE_AS", "")
	cfg := loadClientConfig()
	cfg.Current = "alice@k"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}

	oldTTY := interactiveTTY
	interactiveTTY = func() bool { return true }
	t.Cleanup(func() { interactiveTTY = oldTTY })
	answerYes(t)

	var runErr error
	prompt := captureStderr(t, func() { _, runErr = execTestCmd(t, userWithdrawCmd(), "1") })
	if runErr != nil {
		t.Fatalf("withdraw: %v", runErr)
	}
	if !strings.Contains(prompt, "alice@k") {
		t.Errorf("the prompt named another login than the one the command runs as: %q", prompt)
	}
	for i, tok := range sent {
		if tok != "ALICE" {
			t.Fatalf("request %d was sent as %q, not as the login the command started with", i, tok)
		}
	}
	if len(sent) < 2 {
		t.Fatalf("the withdrawal made %d authenticated requests, expected the read and the payment", len(sent))
	}
}

// TestAMovedKernelMustAnswerWithItsNetworkToo: a record is kept for one kernel on one network, so
// following a move checks both. Checking the key alone let `kernel add` re-pin a kernel's network
// silently, and every login on it stayed pointed at money that now means something else.
func TestAMovedKernelMustAnswerWithItsNetworkToo(t *testing.T) {
	clientHomeFor(t)
	first := healthServer(t, "KEY-A", "DIGEST-A", "play")
	moved := healthServer(t, "KEY-A", "DIGEST-B", "mainnet") // the same key, another network
	recordLogin(t, "alice@work", first.URL, "KEY-A")
	if err := saveToken("TOK"); err != nil {
		t.Fatal(err)
	}
	cfg := loadClientConfig()
	cfg.Kernels["work"].WorldFingerprint = "DIGEST-A"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Both branches of an add must ask: the one that follows a move, and the one that finds the
	// record already pointing where it is told — which answered "already known" without looking.
	for _, at := range []struct{ name, recorded string }{
		{"following it to a new address", first.URL},
		{"finding it already at that address", moved.URL},
	} {
		t.Run(at.name, func(t *testing.T) {
			resetClient()
			cfg := loadClientConfig()
			cfg.Kernels["work"].Endpoint, cfg.Kernels["work"].WorldFingerprint = at.recorded, "DIGEST-A"
			if err := saveClientConfig(cfg); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := registerKernel(context.Background(), "work", moved.URL); err == nil {
				t.Fatal("a kernel serving another network was recorded")
			}
			if k := loadClientConfig().Kernels["work"]; k.WorldFingerprint != "DIGEST-A" {
				t.Errorf("a refused add re-pinned the network: %+v", k)
			}
			if tok, _ := loadToken(); tok != "TOK" {
				t.Error("a refused add logged the kernel's logins out")
			}
		})
	}
}

// TestARotatedTokenIsTakenNotSpent: the identity is fixed for a command; the tokens under it are
// not. Another process sharing the login may rotate first, and this one must take what the lock
// yields rather than spend a refresh token that no longer exists.
func TestARotatedTokenIsTakenNotSpent(t *testing.T) {
	clientHomeFor(t)
	refreshes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/refresh" {
			refreshes++
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "MINE", "refresh_token": "R2"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "public_key": "KEY", "network": "play"})
	}))
	t.Cleanup(srv.Close)
	recordLogin(t, "alice@k", srv.URL, "KEY")
	if err := saveToken("OLD"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefreshToken("R1"); err != nil {
		t.Fatal(err)
	}
	c := freshClient()

	// Another process rotates first: this client takes what it stored and spends nothing.
	if err := withCredentials(c.login, func(cr *credentials) (bool, error) {
		cr.Token, cr.RefreshToken = "THEIRS", "R9"
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.refresh(context.Background(), "OLD"); !ok || err != nil {
		t.Fatalf("a client told to give up though a valid token was waiting: %v", err)
	}
	if refreshes != 0 {
		t.Errorf("the refresh token was spent though another process had already rotated")
	}
	if c.creds.Token != "THEIRS" {
		t.Errorf("the client kept its own stale token: %q", c.creds.Token)
	}
	// Nobody else rotated: this one does, and keeps what it wrote.
	if ok, err := c.refresh(context.Background(), "THEIRS"); !ok || err != nil || refreshes != 1 {
		t.Fatalf("the client did not rotate when it had to: %d refreshes, %v", refreshes, err)
	}
	if c.creds.Token != "MINE" {
		t.Errorf("the client kept a token it had rotated away: %q", c.creds.Token)
	}
}

// TestARequestWithNowhereToGoSaysSo: a client with no address answers why, never with silence — a
// command that returned nil without sending would read as one that had succeeded.
func TestARequestWithNowhereToGoSaysSo(t *testing.T) {
	clientHomeFor(t)
	old := flagServer
	flagServer = ""
	t.Cleanup(func() { flagServer = old })

	if err := freshClient().call(context.Background(), "GET", "/v1/me", nil, nil); err == nil {
		t.Error("a request with no login and no --server reported success")
	}
	// A record whose address was lost is the same answer, not silence.
	recordLogin(t, "alice@k", "http://kernel:4040", "KEY")
	cfg := loadClientConfig()
	cfg.Kernels["k"].Endpoint = ""
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	err := freshClient().call(context.Background(), "GET", "/v1/me", nil, nil)
	if err == nil {
		t.Fatal("a kernel with no address reported success")
	}
	if !strings.Contains(err.Error(), "no address recorded") {
		t.Errorf("unhelpful refusal: %v", err)
	}
}

// TestANamedLoginIsNeverASilentStranger: `--as` names a login this client is asked to act as, so
// one it does not hold is a refusal — not a quieter request that reads whatever is public and
// reports success (D20). Naming none is the other case, and stays anonymous: that is how a kernel
// is read before anyone has an account on it.
func TestANamedLoginIsNeverASilentStranger(t *testing.T) {
	clientHomeFor(t)
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "handle": "k", "public_key": "KEY",
				"network": "play", "network_fingerprint": "D"})
			return
		}
		served++
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "a-1"}})
	}))
	t.Cleanup(srv.Close)
	recordLogin(t, "alice@k", srv.URL, "KEY")
	if err := saveToken("ALICE"); err != nil {
		t.Fatal(err)
	}
	old := flagServer
	t.Cleanup(func() { flagServer = old })

	for _, c := range []struct {
		name, as, server string
		wantSent         bool
	}{
		{"a login this client holds", "alice@k", "", true},
		{"a login it does not hold", "ghost@k", "", false},
		{"a kernel it does not know", "alice@nowhere", "", false},
		{"no login at all, addressed by --server", "", srv.URL, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			served, flagServer = 0, c.server
			t.Setenv("JUICE_AS", c.as)
			if c.as == "" {
				cfg := loadClientConfig()
				cfg.Current = ""
				if err := saveClientConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			err := freshClient().call(context.Background(), "GET", "/v1/actions", nil, nil)
			if c.wantSent {
				if err != nil {
					t.Fatalf("refused a request it should have made: %v", err)
				}
				if served != 1 {
					t.Errorf("the request was not sent")
				}
				return
			}
			if err == nil {
				t.Fatal("a login this client does not hold read as a stranger and reported success")
			}
			if served != 0 {
				t.Errorf("the request went out anyway, as nobody")
			}
		})
	}
}

// TestASessionThisClientCannotSaveIsRefused: a renewed session the file did not take is worse than
// an expired one — the next command would spend a refresh token that is no longer the stored one —
// so the command says so rather than succeeding on a token nobody else will see.
func TestASessionThisClientCannotSaveIsRefused(t *testing.T) {
	clientHomeFor(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "public_key": "KEY", "network": "play"})
		case "/v1/auth/refresh":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "NEW", "refresh_token": "R2"})
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "expired", "code": "unauthenticated"})
		}
	}))
	t.Cleanup(srv.Close)
	recordLogin(t, "alice@k", srv.URL, "KEY")
	if err := saveToken("OLD"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefreshToken("R1"); err != nil {
		t.Fatal(err)
	}
	c := freshClient()
	// The credential directory cannot be written: the rotation happens, the record does not.
	if err := os.Chmod(credentialsDir(), 0o500); err != nil {
		t.Skipf("cannot make the credential directory read-only here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(credentialsDir(), 0o700) })
	path, err := credentialPath(c.login)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Skip("cannot make the credential file read-only here")
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	err = c.call(context.Background(), "GET", "/v1/me", nil, nil)
	if err == nil {
		t.Fatal("a session that could not be saved was reported as a request that worked")
	}
	if !strings.Contains(err.Error(), "could not be saved") {
		t.Errorf("the refusal does not say what went wrong: %v", err)
	}
}

// TestAnExpiredSessionIsRenewedBeforeTheRequest: a login whose access token is gone still holds a
// session, so it is renewed and the request goes out as that login — never once as a stranger
// whose 401 happens to prompt the renewal afterwards.
func TestAnExpiredSessionIsRenewedBeforeTheRequest(t *testing.T) {
	clientHomeFor(t)
	var bearers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "public_key": "KEY", "network": "play"})
		case "/v1/auth/refresh":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "FRESH", "refresh_token": "R2"})
		default:
			bearers = append(bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "u"})
		}
	}))
	t.Cleanup(srv.Close)
	recordLogin(t, "alice@k", srv.URL, "KEY")
	if err := saveRefreshToken("R1"); err != nil { // a session with no access token left
		t.Fatal(err)
	}

	if err := freshClient().call(context.Background(), "GET", "/v1/me", nil, nil); err != nil {
		t.Fatalf("a renewable session was refused: %v", err)
	}
	if len(bearers) != 1 || bearers[0] != "FRESH" {
		t.Errorf("the request was made as %v, not as the renewed login", bearers)
	}
	if tok, _ := loadToken(); tok != "FRESH" {
		t.Errorf("the renewed session was not stored: %q", tok)
	}
}

// TestOneRecordPerKey: a kernel is its key, and this client holds one record for it. Registering a
// known kernel under another name renames what is there — logins included, since a session belongs
// to the kernel that issued it — rather than leaving two records for one kernel with its logins
// split between them (D15).
func TestOneRecordPerKey(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "D-A", "play")
	old := flagServer
	flagServer = ""
	t.Cleanup(func() { flagServer = old })

	if _, _, _, err := registerKernel(context.Background(), "work", srv.URL); err != nil {
		t.Fatal(err)
	}
	recordLogin(t, "alice@work", srv.URL, "KEY-A")
	t.Setenv("JUICE_AS", "alice@work")
	if err := saveRefreshToken("R"); err != nil { // the session file is what makes the login exist
		t.Fatal(err)
	}
	t.Setenv("JUICE_AS", "")

	name, _, outcome, err := registerKernel(context.Background(), "home", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if name != "home" || !strings.Contains(outcome, "renamed") {
		t.Fatalf("registering a known key under a new name: name=%q outcome=%q", name, outcome)
	}
	cfg := loadClientConfig()
	if cfg.Kernels["work"] != nil {
		t.Error("the old name still holds a record; one key is one record")
	}
	if cfg.Kernels["home"] == nil || cfg.Kernels["home"].PublicKey != "KEY-A" {
		t.Fatalf("the new name holds %+v", cfg.Kernels["home"])
	}
	if got := loginNames(t); !slices.Contains(got, "alice@home") {
		t.Errorf("logins after the rename = %v, want alice@home among them", got)
	}
	if cfg.Current != "alice@home" {
		t.Errorf("selected login = %q, want alice@home", cfg.Current)
	}

	// A name another kernel already holds is not taken, and nothing is moved on the way to finding
	// that out: the record and the logins of both kernels survive the refusal.
	other := healthServer(t, "KEY-B", "D-B", "play")
	if _, _, _, err := registerKernel(context.Background(), "spare", other.URL); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registerKernel(context.Background(), "spare", srv.URL); err == nil {
		t.Fatal("a name held by another kernel was taken")
	}
	cfg = loadClientConfig()
	if cfg.Kernels["spare"] == nil || cfg.Kernels["spare"].PublicKey != "KEY-B" {
		t.Errorf("the refused name no longer holds its own kernel: %+v", cfg.Kernels["spare"])
	}
	if cfg.Kernels["home"] == nil || cfg.Kernels["home"].PublicKey != "KEY-A" {
		t.Errorf("the kernel that was refused a rename lost its record: %+v", cfg.Kernels["home"])
	}
	if got := loginNames(t); !slices.Contains(got, "alice@home") {
		t.Errorf("logins after the refusal = %v, want alice@home still among them", got)
	}
}

// loginNames is what `auth list` would name, in the order it finds them.
func loginNames(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, l := range logins() {
		out = append(out, l.String())
	}
	return out
}

// TestAnUnreachableLocalKernelNamesItsWorld: the remedy for a kernel that does not answer is the
// command that starts it, and that command takes the network, not the name this client chose for
// it. A kernel somewhere else cannot be started by the reader and is told to wait instead.
func TestAnUnreachableLocalKernelNamesItsWorld(t *testing.T) {
	home := testHome(t)
	_ = home
	resetClient()
	cfg := loadClientConfig()
	cfg.Kernels["work"] = &kernelRec{Endpoint: "http://127.0.0.1:4999", PublicKey: "KEY-A", Network: "arbitrum-one"}
	cfg.Kernels["away"] = &kernelRec{Endpoint: "http://kernel.example.org:4040", PublicKey: "KEY-B", Network: "play"}
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	resetClient()

	local := remedy(unreachable("http://127.0.0.1:4999"))
	if local != "Start it with: juice kernel serve arbitrum-one" {
		t.Errorf("a local kernel's remedy must name the world serve takes: %q", local)
	}
	if remote := remedy(unreachable("http://kernel.example.org:4040")); strings.Contains(remote, "kernel serve") {
		t.Errorf("a kernel on another machine cannot be started here: %q", remote)
	}
}
