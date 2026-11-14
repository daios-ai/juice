// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
)

// bootSuperuser first-boots @sys on env.k, marks it the configured superuser, and returns a
// @sys bearer token. The superuser verbs are ordinary TCP routes now (gated by
// requireSuperuserMW), so tests drive them against flagServer like any other endpoint.
func bootSuperuser(t *testing.T, env *testEnv) string {
	t.Helper()
	ctx := context.Background()
	if err := env.k.FirstBoot(ctx, "sys-pass", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.k.SetConfig(ctx, configKeySuperuser, "sys"); err != nil {
		t.Fatal(err)
	}
	tok, err := loginTokenFor(env.k, ctx, "sys", "sys-pass")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// tcpDo sends a request to the test TCP server (flagServer), attaching the bearer token when set.
func tcpDo(t *testing.T, token, method, path string, body any) ([]byte, int) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, flagServer+path, rdr)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode
}

// TestAdminDepositOverTCP: a superuser deposit over the public TCP API mutates a real balance.
func TestAdminDepositOverTCP(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	suTok := bootSuperuser(t, env)

	recipient, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "rcpt@k", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, status := tcpDo(t, suTok, "POST", "/v1/admin/users/rcpt@k/deposit",
		map[string]any{"amount": 500, "ref": "test-payment"})
	if status != http.StatusOK {
		t.Fatalf("deposit status %d: %s", status, body)
	}
	u, err := env.k.ReadUser(ctx, recipient.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Available != 500 {
		t.Errorf("available after deposit: got %d, want 500", u.Available)
	}
	// Recording the same payment again moves nothing and answers with the entry that recorded it —
	// a reply, not a crash: the handler renders whatever the kernel returns, so the kernel must
	// return something.
	body2, status2 := tcpDo(t, suTok, "POST", "/v1/admin/users/rcpt@k/deposit",
		map[string]any{"amount": 500, "ref": "test-payment"})
	if status2 != http.StatusOK {
		t.Fatalf("replayed deposit: status %d: %s", status2, body2)
	}
	if strings.TrimSpace(string(body2)) == "" || string(body2) != string(body) {
		t.Errorf("replay must answer with the same entry:\n first  %s\n second %s", body, body2)
	}
	if u, _ := env.k.ReadUser(ctx, recipient.ID); u.Available != 500 {
		t.Errorf("replay moved money: %d", u.Available)
	}
}

// TestPeerRosterIsAPlainArrayWhenEmpty: a list with nothing in it is `[]`, never `null` (API.md
// R6). The roster was the one list that reached the wire as a nil slice.
func TestPeerRosterIsAPlainArrayWhenEmpty(t *testing.T) {
	env := newTestEnv(t)
	suTok := bootSuperuser(t, env)
	body, status := tcpDo(t, suTok, "GET", "/v1/admin/peers", nil)
	if status != http.StatusOK {
		t.Fatalf("peers: status %d: %s", status, body)
	}
	if got := strings.TrimSpace(string(body)); got != "[]" {
		t.Fatalf("empty roster: got %s, want []", got)
	}
}

// TestAdminDepositToAPeerIsRefused: a peer holds no account here (P10, D14). What a peer owes
// closes when it pays, which no operator records by hand, so a deposit never names one. And a
// refusal writes nothing: a kernel becomes known from a verified resolve or a signed inbound call,
// never from a money command that failed.
func TestAdminDepositToAPeerIsRefused(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	suTok := bootSuperuser(t, env)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyB64 := base64.RawURLEncoding.EncodeToString(pub)
	if err := env.k.KnowKernel(ctx, keyB64); err != nil {
		t.Fatal(err)
	}
	if body, status := tcpDo(t, suTok, "POST", "/v1/admin/users/"+keyB64+"/deposit",
		map[string]any{"amount": 300, "ref": "test-payment"}); status == http.StatusOK {
		t.Fatalf("a deposit to a peer was accepted: %s", body)
	}

	// A key this kernel has never seen: the refusal must leave no kernel row behind it.
	stranger, _, _ := ed25519.GenerateKey(rand.Reader)
	strangerKey := base64.RawURLEncoding.EncodeToString(stranger)
	if body, status := tcpDo(t, suTok, "POST", "/v1/admin/users/"+strangerKey+"/deposit",
		map[string]any{"amount": 300, "ref": "test-payment-2"}); status == http.StatusOK {
		t.Fatalf("a deposit to an unknown kernel was accepted: %s", body)
	}
	if kr, _ := env.k.ReadKernel(ctx, strangerKey); kr != nil {
		t.Error("a refused deposit made a kernel known, which only a verified resolve or a signed call may do")
	}
}

// TestAdminRenameOverTCP: a superuser rename over the public TCP API vacates the old handle,
// which a fresh account then reuses.
func TestAdminRenameOverTCP(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	suTok := bootSuperuser(t, env)

	bob, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "bob@k", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, status := tcpDo(t, suTok, "POST", "/v1/admin/users/bob@k/rename",
		map[string]any{"new_name": "bob-retired@k"})
	if status != http.StatusOK {
		t.Fatalf("rename status %d: %s", status, body)
	}
	if got, err := env.k.ReadUserByHandle(ctx, "bob-retired"); err != nil || got.ID != bob.ID {
		t.Errorf("renamed handle does not resolve to bob: %v", err)
	}
	// The freed @bob is reusable by a distinct fresh account.
	fresh, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "bob@k", Password: "pw",
	})
	if err != nil {
		t.Fatalf("reuse freed handle: %v", err)
	}
	if fresh.ID == bob.ID {
		t.Errorf("reused handle must be a distinct account")
	}
}

// TestAdminSuperuserGate proves the two-factor gate on the TCP routes: no bearer token is
// rejected (authMiddleware), and a valid but non-superuser token is rejected (requireSuperuserMW).
func TestAdminSuperuserGate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	_ = bootSuperuser(t, env)

	if _, status := tcpDo(t, "", "GET", "/v1/admin/users", nil); status != http.StatusUnauthorized {
		t.Errorf("no token: got status %d, want 401", status)
	}

	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "regular@k", Password: "pw",
	}); err != nil {
		t.Fatal(err)
	}
	regTok, err := loginTokenFor(env.k, ctx, "regular", "pw")
	if err != nil {
		t.Fatal(err)
	}
	body, status := tcpDo(t, regTok, "POST", "/v1/admin/users/regular/deposit",
		map[string]any{"amount": 1, "ref": "test-payment"})
	if status == http.StatusOK {
		t.Fatalf("non-superuser deposit should be rejected, got 200")
	}
	if !strings.Contains(string(body), "superuser") {
		t.Errorf("expected superuser-required error, got: %s", body)
	}
}

// A user is named by address and principal, never by a bare id field: only GET /v1/me answers with
// the caller's own (D20). The operator's own routes were writing the account row verbatim.
func TestOperatorRoutesWithholdAccountIDs(t *testing.T) {
	env := newTestEnv(t)
	suTok := bootSuperuser(t, env)
	u, err := env.k.CreateUser(context.Background(), kernel.CreateUserRequest{Handle: "shown@k", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/admin/users", "/v1/admin/users/shown@k"} {
		body, status := tcpDo(t, suTok, "GET", path, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, status, body)
		}
		if strings.Contains(string(body), `"id"`) || strings.Count(string(body), u.ID) != strings.Count(string(body), u.ID+"@") {
			t.Errorf("%s carries the account id outside its principal: %s", path, body)
		}
		if !strings.Contains(string(body), `"principal":"`+u.ID+"@") {
			t.Errorf("%s does not name the account by its principal: %s", path, body)
		}
		if !strings.Contains(string(body), "shown") {
			t.Errorf("%s should still name the account: %s", path, body)
		}
	}
}

// A list with nothing in it is `[]`, never `null` (API.md R6). These two are built outside the
// store, so the store's guarantee does not reach them.
func TestListsBuiltOutsideTheStoreAreArrays(t *testing.T) {
	env := newTestEnv(t)
	suTok := bootSuperuser(t, env)
	body, status := tcpDo(t, suTok, "GET", "/v1/admin/kernel", nil)
	if status != http.StatusOK {
		t.Fatalf("identity: status %d: %s", status, body)
	}
	if strings.Contains(string(body), `"addrs":null`) {
		t.Errorf("addrs answered null with no transport: %s", body)
	}
}

// releasesPage answers like the releases page: a redirect naming the newest tag, or, with an empty
// tag, a page that names none.
func releasesPage(t *testing.T, tag *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *tag == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Location", "https://github.com/daios-ai/juice/releases/tag/"+*tag)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The newest release is the tag the releases page redirects to, read without following it; a page
// naming none, an unreachable page and a spent deadline are each an error, never a release.
func TestLatestReleaseReadsTheRedirect(t *testing.T) {
	tag := "v0.17.4"
	page := releasesPage(t, &tag)
	got, err := latestRelease(context.Background(), page.URL, true)
	if err != nil || got != "v0.17.4" {
		t.Fatalf("latestRelease = %q, %v; want v0.17.4", got, err)
	}
	tag = ""
	if got, err := latestRelease(context.Background(), page.URL, true); err == nil {
		t.Errorf("a page naming no release answered %q", got)
	}
	tag = "v0.17.4"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := latestRelease(ctx, page.URL, true); err == nil {
		t.Errorf("a spent deadline answered %q", got)
	}
	page.Close()
	if got, err := latestRelease(context.Background(), page.URL, true); err == nil {
		t.Errorf("an unreachable page answered %q", got)
	}
}

func TestReleaseVersionAcceptsOnlyReleases(t *testing.T) {
	for _, s := range []string{"v0.17.4", "v1.0.0", "v10.20.30"} {
		if _, _, _, ok := releaseVersion(s); !ok {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"dev", "", "v0.17.4-3-gabc", "v0.17.4-dirty", "v0.17.4-rc1", "0.17.4", "v0.17", "v0.17.4.1", "v0.017.4", "v-1.0.0"} {
		if _, _, _, ok := releaseVersion(s); ok {
			t.Errorf("%q accepted as a release", s)
		}
	}
}

func TestNewerThan(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.17.4", "v0.17.2", true},
		{"v0.18.0", "v0.17.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.17.10", "v0.17.9", true},
		{"v0.17.2", "v0.17.2", false},
		{"v0.17.2", "v0.17.4", false},
		{"v0.17.4", "dev", false},
		{"dev", "v0.17.4", false},
		{"v0.17.4", "v0.17.2-3-gabc", false},
	} {
		if got := newerThan(c.a, c.b); got != c.want {
			t.Errorf("newerThan(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A newer release is logged once per tag; an unreachable page keeps the tag last read and logs no
// warning.
func TestReleaseCheckLogsEachNewerTagOnce(t *testing.T) {
	orig := version
	version = "v0.17.2"
	t.Cleanup(func() { version = orig })
	tag := "v0.17.4"
	page := releasesPage(t, &tag)
	c := &releaseCheck{url: page.URL, allowLocal: true}
	out := captureStderr(t, func() {
		logger, err := log.New(log.Config{Level: "info", Format: "json"})
		if err != nil {
			t.Fatal(err)
		}
		c.check(context.Background(), logger)
		c.check(context.Background(), logger)
		tag = "v0.17.5"
		c.check(context.Background(), logger)
		page.Close()
		c.check(context.Background(), logger)
	})
	if n := strings.Count(out, "release.available"); n != 2 {
		t.Errorf("release.available logged %d times, want once per tag (2):\n%s", n, out)
	}
	if !strings.Contains(out, `"latest":"v0.17.5"`) {
		t.Errorf("the second tag was not logged:\n%s", out)
	}
	if strings.Contains(out, "check_failed") {
		t.Errorf("an unreachable page was logged at info or above:\n%s", out)
	}
	if got := c.Latest(); got != "v0.17.5" {
		t.Errorf("Latest = %q after an unreachable page, want the last tag read", got)
	}
}

// admin kernel show carries the running build always, and the newest release only while the
// kernel checks for one.
func TestIdentityCarriesVersionAndLatestRelease(t *testing.T) {
	env := newTestEnv(t)
	suTok := bootSuperuser(t, env)
	read := func() map[string]any {
		body, status := tcpDo(t, suTok, "GET", "/v1/admin/kernel", nil)
		if status != http.StatusOK {
			t.Fatalf("identity: status %d: %s", status, body)
		}
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := read()
	if out["version"] != version {
		t.Errorf("version = %v, want %q", out["version"], version)
	}
	if _, ok := out["latest_release"]; ok {
		t.Errorf("latest_release present with no check: %v", out)
	}
	checking := httptest.NewServer(mountFullRouter(&server{kernel: env.k, log: log.Discard(),
		release: &releaseCheck{latest: "v0.17.4"}}))
	t.Cleanup(checking.Close)
	orig := flagServer
	flagServer = checking.URL
	t.Cleanup(func() { flagServer = orig })
	if got := read()["latest_release"]; got != "v0.17.4" {
		t.Errorf("latest_release = %v, want v0.17.4", got)
	}
}
