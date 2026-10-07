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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// configFixture gives the test server a saved configuration as initConfig leaves one: a home with
// the shipped endpoint files, a config.json holding both kinds of secret, and this run's record of
// it. It returns the file's path.
func configFixture(t *testing.T) string {
	t.Helper()
	home := llmHome(t)
	cfg := DefaultServerConfig()
	cfg.CredentialsKey = "sealed"
	cfg.Native.LLM.Endpoints = map[string]LLMEndpointConfig{"ollama": {Key: "s3cret"}}
	path := filepath.Join(home, "config.json")
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	oldPath, oldTag, oldOver := resolvedConfigPath, configBootETag, configOverridden
	resolvedConfigPath, configBootETag, configOverridden = path, etagOf(raw), []string{}
	t.Cleanup(func() { resolvedConfigPath, configBootETag, configOverridden = oldPath, oldTag, oldOver })
	return path
}

// configView is the reply of GET and PATCH /v1/admin/kernel/config.
type configView struct {
	Config         map[string]any `json:"config"`
	Secrets        []string       `json:"secrets"`
	Overridden     []string       `json:"overridden"`
	PendingRestart bool           `json:"pending_restart"`
}

// configDo sends one request to /v1/admin/kernel/config as a client would, with the If-Match it
// holds, and returns the reply, its status and its ETag.
func configDo(t *testing.T, tok, method, body, ifMatch string) ([]byte, int, string) {
	t.Helper()
	req, err := http.NewRequest(method, flagServer+"/v1/admin/kernel/config", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Authorization", "Bearer "+tok)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, resp.Header.Get("ETag")
}

// The superuser reads and changes the saved configuration through the API, and the kernel is the
// file's writer (D20): a read withholds every secret and names it, says what this run overrides and
// whether the file has changed since the start; a change is a merge patch held to a start's rules —
// alone and under this run's overrides — before anything is written, so a refused one leaves the
// file byte-identical, and an accepted one keeps every secret and the credentials key.
func TestKernelConfigOverTheAPI(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	tok := bootSuperuser(t, env)
	path := configFixture(t)
	file := func() []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	field := func(keys ...string) any {
		var v any
		_ = json.Unmarshal(file(), &v)
		for _, k := range keys {
			v = v.(map[string]any)[k]
		}
		return v
	}
	read := func() (configView, string) {
		t.Helper()
		body, status, etag := configDo(t, tok, "GET", "", "")
		if status != http.StatusOK {
			t.Fatalf("GET: %d %s", status, body)
		}
		var v configView
		_ = json.Unmarshal(body, &v)
		return v, etag
	}

	v, etag := read()
	secrets := []string{"credentials_key", "native.llm.endpoints.ollama.key"}
	if v.Config["fee_bps"] != float64(2000) || v.Config["credentials_key"] != nil || !slices.Equal(v.Secrets, secrets) ||
		v.Overridden == nil || len(v.Overridden) != 0 || v.PendingRestart {
		t.Fatalf("view: %+v", v)
	}
	if ep := v.Config["native"].(map[string]any)["llm"].(map[string]any)["endpoints"].(map[string]any)["ollama"].(map[string]any); ep["key"] != nil {
		t.Fatalf("an endpoint's key was returned: %v", ep)
	}
	if len(etag) != 66 || etag[0] != '"' || etag[65] != '"' {
		t.Fatalf("ETag %q is not a strong, quoted tag", etag)
	}

	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "regular@k", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	regTok, err := loginTokenFor(env.k, ctx, "regular", "pw")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "PATCH"} {
		if _, status, _ := configDo(t, regTok, method, `{"fee_bps":1}`, ""); status != http.StatusForbidden {
			t.Errorf("%s by a user: %d; want 403", method, status)
		}
	}

	before := file()
	for _, tc := range []struct {
		name, patch, ifMatch string
		status               int
	}{
		{"unknown key", `{"bogus":1}`, "", http.StatusUnprocessableEntity},
		{"bad value", `{"fee_bps":20000}`, "", http.StatusUnprocessableEntity},
		{"wrong type", `{"native":5}`, "", http.StatusUnprocessableEntity},
		{"not an object", `[1]`, "", http.StatusUnprocessableEntity},
		{"null", `null`, "", http.StatusUnprocessableEntity},
		{"names the credentials key", `{"credentials_key":"other"}`, "", http.StatusUnprocessableEntity},
		{"nulls the credentials key", `{"credentials_key":null}`, "", http.StatusUnprocessableEntity},
		{"stale If-Match", `{"fee_bps":1}`, `"0000"`, http.StatusPreconditionFailed},
		{"stale If-Match list", `{"fee_bps":1}`, `"0000", "1111"`, http.StatusPreconditionFailed},
	} {
		body, status, _ := configDo(t, tok, "PATCH", tc.patch, tc.ifMatch)
		if status != tc.status {
			t.Errorf("%s: %d %s; want %d", tc.name, status, body, tc.status)
		}
		if status == http.StatusPreconditionFailed && !strings.Contains(string(body), "precondition_failed") {
			t.Errorf("%s: %s; want the precondition_failed code", tc.name, body)
		}
		if !bytes.Equal(file(), before) {
			t.Fatalf("%s: a refused patch changed the file", tc.name)
		}
	}
	// Valid alone but not under this run's flags, and the reverse: both refused.
	undo := bindServeFlags(t, "--lottery", "4000000")
	if body, status, _ := configDo(t, tok, "PATCH", `{"lottery_max":3000000}`, ""); status != http.StatusUnprocessableEntity ||
		!strings.Contains(string(body), "this run") {
		t.Errorf("a file valid alone but not under --lottery: %d %s", status, body)
	}
	undo()
	undo = bindServeFlags(t, "--lottery", "0")
	if _, status, _ := configDo(t, tok, "PATCH", `{"lottery_max":500000}`, ""); status != http.StatusUnprocessableEntity {
		t.Errorf("a file invalid alone was saved because --lottery masks it: %d", status)
	}
	undo()
	if !bytes.Equal(file(), before) {
		t.Fatal("a refused patch changed the file")
	}

	// A change under a correct If-Match: the file holds it, both secrets survive, a runtime-only
	// credentials key never reaches the disk, and the view says a restart is waiting.
	t.Setenv("JUICE_CREDENTIALS_KEY", "runtime-only")
	body, status, etag2 := configDo(t, tok, "PATCH", `{"native":{"time":{"price":7}},"lottery":123}`, etag)
	if status != http.StatusOK {
		t.Fatalf("PATCH: %d %s", status, body)
	}
	var after configView
	_ = json.Unmarshal(body, &after)
	if !after.PendingRestart || !slices.Equal(after.Secrets, secrets) || etag2 == etag {
		t.Fatalf("after a change: %+v, ETag %q", after, etag2)
	}
	if field("native", "time", "price") != float64(7) || field("lottery") != float64(123) ||
		field("credentials_key") != "sealed" || field("native", "llm", "endpoints", "ollama", "key") != "s3cret" {
		t.Fatalf("file after the change: %s", file())
	}
	// null restores the default, and a stale tag is stale.
	if _, status, _ := configDo(t, tok, "PATCH", `{"lottery":null}`, etag); status != http.StatusPreconditionFailed {
		t.Errorf("the tag of the previous file still matched: %d", status)
	}
	if _, status, _ := configDo(t, tok, "PATCH", `{"lottery":null}`, etag2); status != http.StatusOK || field("lottery") != nil {
		t.Errorf("null did not restore the default: %d, lottery %v", status, field("lottery"))
	}
	// If-Match as RFC 9110 reads it: a list holding the file's tag, and `*`, each admit.
	_, etag3 := read()
	for _, m := range []string{`"0000", ` + etag3, "*"} {
		if _, status, _ := configDo(t, tok, "PATCH", `{"fee_bps":2000}`, m); status != http.StatusOK {
			t.Errorf("If-Match %q: %d; want 200", m, status)
		}
	}
	if v, _ := read(); !v.PendingRestart {
		t.Error("a changed file does not say a restart is waiting")
	}
	// A file that is two documents, or no object, is refused whole, by a start and by a patch
	// alike, never read in part or as nothing.
	for name, bad := range map[string][]byte{"two documents": append(file(), []byte("{}")...), "null": []byte("null\n"), "empty": nil} {
		if err := os.WriteFile(path, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := parseConfig(bad); err == nil {
			t.Errorf("%s parsed", name)
		}
		if body, status, _ := configDo(t, tok, "PATCH", `{"fee_bps":1}`, ""); status != http.StatusConflict {
			t.Errorf("a patch over %s: %d %s; want 409", name, status, body)
		}
	}
}

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

// TestAdminDepositToAPeerIsRefused: a peer account is identity, never a wallet (P10, D14). What a
// peer owes closes when it pays, which no operator records by hand, so a deposit never names one —
// refused rather than preloading a balance no path would ever spend. And a refusal writes nothing:
// a peer this kernel has never met still does not exist afterwards, since a peer relationship comes
// from a verified resolve or a signed inbound call, never from a money command that failed.
func TestAdminDepositToAPeerIsRefused(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	suTok := bootSuperuser(t, env)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := env.k.EnsureKernelAccount(ctx, keyB64)
	if err != nil {
		t.Fatal(err)
	}

	// Addressed by key or by petname, and with or without an amount, it is the same refusal.
	body, status := tcpDo(t, suTok, "POST", "/v1/admin/users/"+keyB64+"/deposit",
		map[string]any{"amount": 300, "ref": "test-payment"})
	if status == http.StatusOK {
		t.Fatalf("a bare deposit to a peer was accepted: %s", body)
	}
	u, err := env.k.ReadUser(ctx, peer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Available != 0 || u.Locked != 0 {
		t.Errorf("peer row after the refusal: %d/%d, want 0/0 — a peer account holds no money on any path",
			u.Available, u.Locked)
	}

	// A key this kernel has never seen: the refusal must leave no account behind it.
	stranger, _, _ := ed25519.GenerateKey(rand.Reader)
	strangerKey := base64.RawURLEncoding.EncodeToString(stranger)
	if body, status := tcpDo(t, suTok, "POST", "/v1/admin/users/"+strangerKey+"/deposit",
		map[string]any{"amount": 300, "ref": "test-payment-2"}); status == http.StatusOK {
		t.Fatalf("a deposit to an unknown kernel was accepted: %s", body)
	}
	if acct, _ := env.k.ReadAccountByKernelKey(ctx, strangerKey); acct != nil {
		t.Error("a refused deposit provisioned a peer account, which only a verified resolve or a signed call may do")
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

// A user is addressed by handle, never by an id: only GET /v1/me answers with the caller's own
// (D20). The operator's own routes were writing the account row verbatim.
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
		if strings.Contains(string(body), u.ID) {
			t.Errorf("%s carries the raw account id: %s", path, body)
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
