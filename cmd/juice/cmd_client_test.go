// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// TestKernelAddNamesAndPinsWithoutSelecting: registering a kernel records where it answers and the
// identity that answered, under the nickname it advertises unless the operator gives a name. It
// selects nothing — where you are is a login, and there is none yet.
func TestKernelAddNamesAndPinsWithoutSelecting(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")

	out, err := execTestCmd(t, kernelAddCmd(), srv.URL)
	if err != nil {
		t.Fatalf("add: %v %s", err, out)
	}
	cfg := loadClientConfig()
	k := cfg.Kernels["k"] // healthServer advertises the nickname "k"
	if k == nil || k.Endpoint != srv.URL || k.PublicKey != "KEY-A" || k.Network != "play" {
		t.Fatalf("not recorded under its nickname: %+v", cfg.Kernels)
	}
	if cfg.Current != "" {
		t.Errorf("adding a kernel selected %q", cfg.Current)
	}

	// A name of the operator's own is taken as given.
	if _, err := execTestCmd(t, kernelAddCmd(), srv.URL, "work"); err != nil {
		t.Fatalf("add under a name: %v", err)
	}
	if loadClientConfig().Kernels["work"] == nil {
		t.Error("the given name was not used")
	}
}

// TestKernelAddIsIdempotentButNotACoup: adding a kernel already known, on the same terms, changes
// nothing and succeeds — a harness may register before every login. A different kernel under a name
// already held is refused, because a nickname is a label and not a proof of anything.
func TestKernelAddIsIdempotentButNotACoup(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")
	if _, err := execTestCmd(t, kernelAddCmd(), srv.URL, "work"); err != nil {
		t.Fatal(err)
	}
	recordLogin(t, "alice@work", srv.URL, "KEY-A")
	out := captureStdout(t, func() error {
		_, err := execTestCmd(t, kernelAddCmd(), srv.URL, "work")
		return err
	})
	if !strings.Contains(out, "already known") {
		t.Errorf("a repeat add must say it changed nothing: %q", out)
	}

	other := healthServer(t, "KEY-B", "DIGEST-A", "play")
	resetHealthCache()
	if _, err := execTestCmd(t, kernelAddCmd(), other.URL, "work"); err == nil {
		t.Fatal("another kernel took a name already held")
	}
	if got := loadClientConfig().Kernels["work"]; got.PublicKey != "KEY-A" {
		t.Errorf("the refused add changed the record: %+v", got)
	}

	// The same kernel at a new address is that kernel: a restart on another port keeps its name and
	// its logins, because a session belongs to whoever issued it and the key says that is this one.
	if err := saveToken("SECRET"); err != nil {
		t.Fatal(err)
	}
	moved := healthServer(t, "KEY-A", "DIGEST-A", "play")
	resetHealthCache()
	if _, err := execTestCmd(t, kernelAddCmd(), moved.URL, "work"); err != nil {
		t.Fatalf("a kernel that moved was refused its own name: %v", err)
	}
	if got := loadClientConfig().Kernels["work"]; got.Endpoint != moved.URL {
		t.Errorf("the new address was not recorded: %+v", got)
	}
	if tok, err := loadToken(); err != nil || tok != "SECRET" {
		t.Errorf("a move logged the operator out: %q %v", tok, err)
	}
}

// TestKernelForgetTakesTheLoginsWithIt: a client that no longer knows a kernel holds no credential
// that could be sent to it, and is not left selected on it. Nothing of the kernel's own is touched.
func TestKernelForgetTakesTheLoginsWithIt(t *testing.T) {
	clientHomeFor(t)
	recordLogin(t, "alice@work", "http://kernel:4040", "KEY")
	if err := saveToken("SECRET"); err != nil {
		t.Fatal(err)
	}
	if _, err := execTestCmd(t, kernelForgetCmd(), "work", "--yes"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	cfg := loadClientConfig()
	if cfg.Kernels["work"] != nil {
		t.Error("the record survived")
	}
	if cfg.Current != "" {
		t.Errorf("still selected: %q", cfg.Current)
	}
	if _, err := os.Stat(credPath(t, "alice@work")); !os.IsNotExist(err) {
		t.Errorf("the credential survived: %v", err)
	}
	if _, err := execTestCmd(t, kernelForgetCmd(), "work", "--yes"); err == nil {
		t.Error("forgetting an unknown kernel must be refused")
	}
}

// TestKernelListMarksWhereYouAre: the list is what this client knows, and the mark is the kernel
// the selected login acts through — the one answer an operator with two kernels needs.
func TestKernelListMarksWhereYouAre(t *testing.T) {
	clientHomeFor(t)
	cfg := loadClientConfig()
	cfg.Kernels["work"] = &kernelRec{Endpoint: "http://work:4040", PublicKey: "KEY-W", Network: "play"}
	cfg.Kernels["lab"] = &kernelRec{Endpoint: "http://lab:4040", PublicKey: "KEY-L", Network: "test"}
	cfg.Current = "alice@lab"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error {
		_, err := execTestCmd(t, kernelListCmd())
		return err
	})
	for _, want := range []string{"KERNEL", "work", "lab", "http://lab:4040", "KEY-W"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing omits %q:\n%s", want, out)
		}
	}
	// Which kernel a bare command acts through is a column, in words, like everything else a list
	// says about a row (§14).
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "lab") && !strings.Contains(line, "yes") {
			t.Errorf("the selected kernel is not marked: %q", line)
		}
		if strings.HasPrefix(line, "work") && strings.Contains(line, "yes") {
			t.Errorf("an unselected kernel is marked: %q", line)
		}
	}
}

// TestKernelHealthNeedsNoLogin: what a server says about itself is public, and reading it is how a
// client decides whether to trust an address at all — so it works before anyone has logged in.
func TestKernelHealthNeedsNoLogin(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")
	if _, err := execTestCmd(t, kernelAddCmd(), srv.URL, "work"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error {
		_, err := execTestCmd(t, kernelHealthCmd(), "work")
		return err
	})
	if !strings.Contains(out, "network play") {
		t.Errorf("health does not report the network: %q", out)
	}
	if _, err := execTestCmd(t, kernelHealthCmd(), "nosuch"); err == nil {
		t.Error("health on an unknown kernel must be refused")
	}
}

// loginTokenFor returns just the access token from the canonical PKCE flow.
func loginTokenFor(k *kernel.Kernel, ctx context.Context, handle, password string) (string, error) {
	access, _, err := loginTokensFor(k, ctx, handle, password)
	return access, err
}

// loginTokensFor drives the canonical PKCE flow (§12): authorize, then exchange the code.
func loginTokensFor(k *kernel.Kernel, ctx context.Context, handle, password string) (access, refresh string, err error) {
	verifier, err := kernel.GenerateCodeVerifier()
	if err != nil {
		return "", "", err
	}
	redirect, err := k.StartAuthCode(ctx, handle, password, kernel.CodeChallenge(verifier), "")
	if err != nil {
		return "", "", err
	}
	return k.ExchangeAuthCode(ctx, strings.TrimPrefix(redirect, "?code="), verifier, "")
}

// TestRecoveryKeyDerivation pins that the CLI's seed-phrase derivation is deterministic and that a
// challenge it signs verifies against the enrolled public key under the kernel's exact payload —
// the CLI↔kernel signature-domain contract for §12 recovery.
func TestRecoveryKeyDerivation(t *testing.T) {
	mnemonic, pubB64, err := generateRecovery()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := deriveRecoveryKey(mnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)); got != pubB64 {
		t.Fatalf("derivation not deterministic: %s vs %s", got, pubB64)
	}

	const nonce = "test-nonce"
	sigB64, err := signRecoveryChallenge(testNet, priv, nonce)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(sigB64)
	payload, _ := testNet.RecoveryChallengeSigningBytes(nonce)
	pub, _ := base64.RawURLEncoding.DecodeString(pubB64)
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		t.Error("recovery challenge signature did not verify against the enrolled key")
	}

	if _, err := deriveRecoveryKey("not a valid recovery phrase"); err == nil {
		t.Error("an invalid mnemonic should be rejected")
	}
}

// TestRecoveryCeremony pins the enrollment sequence (§12): the phrase is displayed before the
// commit callback runs, acknowledgment gates the commit when a reader is supplied, headless
// (nil ack) never pauses or prompts, and a failed commit announces that the phrase is dead.
func TestRecoveryCeremony(t *testing.T) {
	t.Run("headless commits without pausing", func(t *testing.T) {
		var out strings.Builder
		var committed string
		if err := runRecoveryCeremony(&out, nil, "Recovery phrase", func(pub string) error {
			committed = pub
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(committed)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			t.Errorf("commit did not receive a valid recovery public key: %q", committed)
		}
		if !strings.Contains(out.String(), "shown only once") {
			t.Error("notice missing from output")
		}
		if strings.Contains(out.String(), "Press Enter") {
			t.Error("headless ceremony must not prompt for acknowledgment")
		}
	})

	t.Run("acknowledgment gates the commit", func(t *testing.T) {
		r, w := io.Pipe()
		var out safeBuilder
		committed := make(chan string, 1)
		done := make(chan error, 1)
		go func() {
			done <- runRecoveryCeremony(&out, r, "sys recovery phrase", func(pub string) error {
				committed <- pub
				return nil
			})
		}()
		select {
		case <-committed:
			t.Fatal("commit ran before acknowledgment")
		case <-time.After(50 * time.Millisecond):
		}
		if !strings.Contains(out.String(), "sys recovery phrase") {
			t.Error("phrase notice not shown while awaiting acknowledgment")
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		<-committed
	})

	t.Run("failed commit announces the phrase is dead", func(t *testing.T) {
		var out strings.Builder
		wantErr := kernel.ErrInvalidState.Wrap("boom")
		err := runRecoveryCeremony(&out, nil, "Recovery phrase", func(string) error { return wantErr })
		if err != wantErr {
			t.Fatalf("commit error not returned: %v", err)
		}
		if !strings.Contains(out.String(), "NOT enrolled") {
			t.Error("discard warning missing after failed commit")
		}
	})
}

// safeBuilder is a strings.Builder safe for the ceremony goroutine and the asserting test to share.
type safeBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAuthLoginLogout(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "clitest",
		Password: "clipass",
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, _, err := loginTokensFor(env.k, ctx, "clitest", "clipass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadToken()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != tok {
		t.Error("loaded token does not match saved token")
	}

	if err := removeToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(); err == nil {
		t.Error("expected error after logout")
	}
}

func TestAuthWrongPassword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "wrongpass",
		Password: "correct",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loginTokenFor(env.k, ctx, "wrongpass", "wrong"); err == nil {
		t.Error("expected error for wrong password")
	}
}

func TestRevokeRefreshToken(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "revoke-user", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, rt, err := loginTokensFor(env.k, ctx, "revoke-user", "pass")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.k.RevokeRefreshToken(ctx, rt); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}

	// Rotating a revoked token must fail.
	if _, _, err := env.k.RefreshAccessToken(ctx, rt); err == nil {
		t.Error("expected error refreshing with revoked token")
	}

	// Revoking again must fail.
	if err := env.k.RevokeRefreshToken(ctx, rt); err == nil {
		t.Error("expected error revoking already-revoked token")
	}
}

// Token verification for authenticated commands is enforced server-side (authMiddleware)
// and covered in serve_test.go / control_test.go.

// TestLoginSelectsAndLogoutUnselects drives the commands an operator actually types: logging in
// names the account and its kernel in one word, stores the session under that name, and acts as it
// from then on; logging out ends it and leaves nothing selected, so the next command says so rather
// than acting as whoever else is logged in.
func TestLoginSelectsAndLogoutUnselects(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "alice", Password: "alicepass"}); err != nil {
		t.Fatal(err)
	}
	// newTestEnv selects tester@test; the kernel record is what `kernel add` would have written.
	if _, err := execTestCmd(t, loginCmd(), "alice@test", "--password", "alicepass"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := loadClientConfig().Current; got != "alice@test" {
		t.Fatalf("login did not select what it authenticated: %q", got)
	}
	if tok, err := loadToken(); err != nil || tok == "" {
		t.Fatalf("no session stored: %q %v", tok, err)
	}
	if c := readCredentials(login{Handle: "alice", Kernel: "test"}); c.PrincipalID == "" {
		t.Error("the login did not record which account it holds")
	}

	// A login this client does not hold cannot be switched to, and a password is not asked for.
	if _, err := execTestCmd(t, authUseCmd(), "bob@test"); err == nil {
		t.Error("switching to a login not held was accepted")
	}

	if _, err := execTestCmd(t, logoutCmd()); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if got := loadClientConfig().Current; got != "" {
		t.Errorf("logout left %q selected", got)
	}
	for _, l := range logins() {
		if l.Handle == "alice" {
			t.Errorf("logout kept alice's credentials: %v", logins())
		}
	}
	if _, err := freshClient().identity(); err == nil {
		t.Error("a command after logout must say there is no login")
	}
}

// TestLoginNeedsItsKernelNamed: a login is an account at a kernel, so the kernel is named every
// time — there is no bare form that would depend on whatever was selected before, and no kernel
// this client has not registered.
func TestLoginNeedsItsKernelNamed(t *testing.T) {
	newTestEnv(t)
	if _, err := execTestCmd(t, loginCmd(), "alice", "--password", "x"); err == nil {
		t.Error("a bare handle was accepted")
	}
	if _, err := execTestCmd(t, loginCmd(), "alice@nosuch", "--password", "x"); err == nil {
		t.Error("an unregistered kernel was accepted")
	}
}
