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
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
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
	if got := serverBaseURL(); got != "" {
		t.Fatalf("no login selected: got %q, want no address", got)
	}
	flagServer = "http://flag:3/"
	if got := serverBaseURL(); got != "http://flag:3" {
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
	if err := apiCall(context.Background(), "GET", "/v1/thing", nil, &out); err != nil {
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
	err := apiCall(context.Background(), "GET", "/v1/x", nil, nil)
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
	t.Setenv("HOME", t.TempDir()) // no token file
	err := apiCall(context.Background(), "GET", "/v1/me", nil, nil)
	if err == nil || !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
	if !strings.Contains(err.Error(), "juice auth login") {
		t.Errorf("the hint must name the command to run, got %q", err.Error())
	}
}

func TestAPICallUnreachable(t *testing.T) {
	old := flagServer
	t.Cleanup(func() { flagServer = old })
	flagServer = "http://127.0.0.1:1" // nothing listening
	t.Setenv("HOME", t.TempDir())
	err := apiCall(context.Background(), "GET", "/v1/x", nil, nil)
	if err == nil || kernel.KernelErrorCode(err) != "invalid_state" {
		t.Fatalf("expected invalid_state, got %v", err)
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
	if err := apiCall(context.Background(), "GET", "/v1/thing", nil, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatal("expected ok after refresh-and-retry")
	}
	if tok, _ := loadToken(); tok != "new-access" {
		t.Fatalf("rotated token not persisted: %q", tok)
	}
}

// TestRunCommandPostsToServer proves the converted `run` command marshals {action, args}
// and posts to /v1/run.
func TestRunCommandPostsToServer(t *testing.T) {
	var gotAction string
	var gotArgs map[string]any
	stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/run" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			Action string         `json:"action"`
			Args   map[string]any `json:"args"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotAction, gotArgs = req.Action, req.Args
		_ = json.NewEncoder(w).Encode(map[string]any{"tx_id": "tx-1", "result": map[string]any{"ok": true}})
	})
	if _, err := execTestCmd(t, runCmd(), "a/b", `{"x":1}`); err != nil {
		t.Fatal(err)
	}
	if gotAction != "a/b" {
		t.Fatalf("action = %q", gotAction)
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
