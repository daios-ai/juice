// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/rail"
	"github.com/daios-ai/juice/script"
	"github.com/daios-ai/juice/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// ---- CLI test helpers ----

// testNet is the play network these tests sign on, matching what a kernel serves by default.
var testNet = kernel.Network{Name: "play", Digest: "ef1fac03f5f78ca42dfa05b9eb975b5e0944e013ed1eb5ea30a2be9328e34a67"}

// newTestStore opens a fresh database that closes with the test.
func newTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// testConfig is the policy every test kernel starts from: the play network and the token secret
// the test's environment names.
func testConfig(secret string) kernel.Config {
	cfg := kernel.DefaultConfig()
	cfg.Network = testNet
	cfg.TokenSecret = secret
	return cfg
}

// testEconomy is the money policy every test kernel starts from. The lottery is off, so every
// obligation is paid exactly and the arithmetic a test asserts is the one it wrote; a test about
// the draw turns it on deliberately.
func testEconomy() kernel.Economy {
	econ := kernel.DefaultEconomy()
	// Exact payment, so a test asserting an amount gets the one it wrote.
	econ.CreditLimit, econ.Lottery = 100000, 0
	return econ
}

// newKernel builds a kernel from cfg and the adapters in deps, with a discarded logger and the
// manual rail, so what every test wires the same way is wired once.
func newKernel(cfg kernel.Config, deps kernel.Dependencies) *kernel.Kernel {
	deps.Config, deps.Logger = cfg, log.Discard()
	if deps.Economy == (kernel.Economy{}) {
		deps.Economy = testEconomy()
	}
	k := kernel.New(deps)
	k.SetRail(rail.NewManual())
	return k
}

// newRef mints the reference a deposit records. Every crossing names the payment it stands for, so
// a test that funds an account twice must name two payments (U3).
func newRef() string { return "test:" + uuid.NewString() }

type testEnv struct {
	db  *store.DB
	k   *kernel.Kernel
	dir string
}

const cmdTestIssuerID = "00000000-0000-0000-0000-000000000001"

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "test.db")

	db, err := store.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Seed the issuer user so receipt FK constraints pass and buildReceipt can sign.
	hash, _ := kernel.HashPassword("issuer-pass")
	issuer := &kernel.Account{
		ID: cmdTestIssuerID, Handle: "@_test_issuer",
		PasswordHash: hash, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.CreateUser(context.Background(), issuer); err != nil {
		t.Fatalf("newTestEnv: seed issuer: %v", err)
	}

	_, signingKey, _ := ed25519.GenerateKey(rand.Reader)
	// A real kernel answers /health with its key and its network, and a client reads that banner
	// before it will scale money or release a credential. A test kernel that cannot is not standing
	// in for one.
	if err := db.SetConfig(context.Background(), configKeySigningPublic,
		base64.RawURLEncoding.EncodeToString(signingKey.Public().(ed25519.PublicKey))); err != nil {
		t.Fatalf("newTestEnv: signing public key: %v", err)
	}

	cfg := testConfig("cli-test-secret")
	cfg.IssuerUserID, cfg.FeeRecipientID, cfg.SigningKey = cmdTestIssuerID, cmdTestIssuerID, signingKey
	// Credential encryption is mandatory (§8): the production binary always wires a box,
	// so tests do too. Without it, creating/activating an action with upstream auth fails closed.
	box, _ := newAESGCMBox(make([]byte, 32))
	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, auth: newAuthenticator(box, db, true, cfg.ScriptTimeout), allowLocal: true}
	exec := script.New(script.Config{TimeoutMS: cfg.ScriptTimeout.Milliseconds(), MemoryBytes: cfg.ScriptMemory})
	k := newKernel(cfg, kernel.Dependencies{Store: db, Scripts: exec, HTTP: httpExec})
	k.SetSecretBox(box)
	t.Setenv("JUICE_SECRET_KEY", "cli-test-secret")

	t.Cleanup(func() { db.Close() })

	origDB := dbPath
	dbPath = dbFile
	t.Cleanup(func() { dbPath = origDB })

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	// User-facing CLI commands are HTTP clients now: point them at a server backed by env.k, and
	// select a login on it, since every command acts as one.
	ts := mountTestServer(t, k)
	origServer := flagServer
	flagServer = ts.URL
	t.Cleanup(func() { flagServer = origServer })
	selectTestLogin(t, "tester@test", ts.URL)

	return &testEnv{db: db, k: k, dir: dir}
}

// selectTestLogin records a kernel at endpoint and selects a login on it, which is what `kernel
// add` and `auth login` do between them. Tests that store a token need one, because a token is
// stored under the login that holds it.
func selectTestLogin(t *testing.T, name, endpoint string) {
	t.Helper()
	l, err := parseLogin(name)
	if err != nil {
		t.Fatal(err)
	}
	old := flagAs
	flagAs = ""
	t.Setenv("JUICE_AS", "")
	t.Cleanup(func() { flagAs = old })
	cfg := loadClientConfig()
	cfg.Kernels[l.Kernel] = &kernelRec{Endpoint: strings.TrimRight(endpoint, "/"), Network: "play"}
	cfg.Current = l.String()
	if err := saveClientConfig(cfg); err != nil {
		t.Fatalf("select test login: %v", err)
	}
	// A login is a login once it holds a session: `auth login` stores one, and a selected name
	// with no credentials is what a command refuses. Tests that need a real token overwrite this.
	if err := withCredentials(l, func(c *credentials) (bool, error) {
		if c.Token == "" {
			c.Token = "test-session"
		}
		return true, nil
	}); err != nil {
		t.Fatalf("select test login: %v", err)
	}
}

// mountTestServer starts an httptest server exposing the full route set backed by k, for
// tests that drive user-facing CLI commands (which are HTTP clients).
func mountTestServer(t *testing.T, k *kernel.Kernel) *httptest.Server {
	t.Helper()
	srv := &server{kernel: k, log: log.Discard()}
	r := chi.NewRouter()
	r.Post("/v1/auth/token", srv.postToken)
	r.Post("/v1/auth/authorize", srv.postAuthorize)
	r.Post("/v1/auth/refresh", srv.postRefresh)
	r.Post("/v1/auth/logout", srv.postLogout)
	r.Post("/v1/users", srv.postUser)
	registerRoutes(r, srv)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

func execTestCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	resetClient() // one client per command, as rootCmd's PersistentPreRun gives a real invocation
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// ---- user ----

// TestUserCreateMismatchedPassword pins that `user create` runs the confirm-twice
// prompt: two differing entries abort with ErrInvalidInput before any user is created.
func TestUserCreateMismatchedPassword(t *testing.T) {
	env := newTestEnv(t)
	stubPasswordPrompts(t, "s3cret", "typo")

	// No --password flag, so the command falls through to the interactive prompt.
	_, err := execTestCmd(t, userCreateCmd(), "newuser")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("mismatch: want ErrInvalidInput, got %v", err)
	}
	if _, err := env.db.ReadUserByHandle(context.Background(), "newuser"); !errors.Is(err, kernel.ErrNotFound) {
		t.Fatalf("no user should be created on mismatch, got %v", err)
	}
}

func TestUserCreate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "testuser",
		Password: "testpass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle != "testuser" {
		t.Errorf("handle: got %q, want @testuser", u.Handle)
	}
	if u.PasswordHash == "testpass" {
		t.Error("password must be hashed")
	}
}

func TestUserReadByHandle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "readtest",
		Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	u, err := env.k.ReadUserByHandle(ctx, "readtest")
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle != "readtest" {
		t.Errorf("handle: got %q, want @readtest", u.Handle)
	}
}

func TestUserDuplicateHandleFails(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	req := kernel.CreateUserRequest{Handle: "dup", Password: "p"}
	if _, err := env.k.CreateUser(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := env.k.CreateUser(ctx, req); err == nil {
		t.Error("expected error for duplicate handle")
	}
}

func TestUserMe(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "meuser", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := env.k.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Handle != "meuser" {
		t.Errorf("handle: got %q, want @meuser", got.Handle)
	}
}

func TestUserUpdateDescription(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "upddesc", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	desc := "weather tools provider"
	got, err := env.k.UpdateUser(ctx, u.ID, kernel.UpdateUserRequest{Description: &desc})
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != desc {
		t.Errorf("description after update: got %q, want %q", got.Description, desc)
	}

	stored, err := env.k.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Description != desc {
		t.Errorf("stored description: got %q, want %q", stored.Description, desc)
	}
}

func TestUserUpdatePassword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "updpass", Password: "oldpass",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := env.k.UpdateUser(ctx, u.ID, kernel.UpdateUserRequest{
		CurrentPassword: "oldpass",
		NewPassword:     "newpass",
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := loginTokensFor(env.k, ctx, "updpass", "oldpass"); err == nil {
		t.Error("old password should be rejected after change")
	}
	if _, _, err := loginTokensFor(env.k, ctx, "updpass", "newpass"); err != nil {
		t.Errorf("new password should work: %v", err)
	}
}

func TestUserUpdatePasswordWrongCurrent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "wrongpass", Password: "correct",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = env.k.UpdateUser(ctx, u.ID, kernel.UpdateUserRequest{
		CurrentPassword: "wrong",
		NewPassword:     "newpass",
	})
	if err == nil {
		t.Fatal("expected error for wrong current password")
	}
	if !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Errorf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestUserUpdateNoFields(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "nofields", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = env.k.UpdateUser(ctx, u.ID, kernel.UpdateUserRequest{})
	if err == nil {
		t.Fatal("expected error when no fields provided")
	}
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUserUpdateProxyUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	proxy := &kernel.Account{
		ID:              "proxy-id-1",
		KernelPublicKey: "dGVzdGtleQ==",
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	if err := env.db.UpsertKernel(ctx, "dGVzdGtleQ==", "remote-peer", "", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := env.db.CreateUser(ctx, proxy); err != nil {
		t.Fatal(err)
	}

	desc := "x"
	_, err := env.k.UpdateUser(ctx, proxy.ID, kernel.UpdateUserRequest{Description: &desc})
	if err == nil {
		t.Fatal("expected error for proxy user")
	}
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState, got %v", err)
	}
}

// ---- action ----

var minSchema = map[string]any{"type": "object", "properties": map[string]any{}}

func TestActionCreateAndToggle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "actowner", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	a, err := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "cli-action",
		Kind:         kernel.KindHTTP,
		Source:       "http://example.com",
		Description:  "test action",
		InputSchema:  minSchema,
		OutputSchema: minSchema,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Active {
		t.Error("new action should be inactive")
	}

	if err := env.k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	updated, _ := env.k.ReadAction(ctx, a.ID)
	if !updated.Active {
		t.Error("action should be active after enable")
	}

	if err := env.k.SetActive(ctx, owner.ID, a.ID, false); err != nil {
		t.Fatal(err)
	}
	updated2, _ := env.k.ReadAction(ctx, a.ID)
	if updated2.Active {
		t.Error("action should be inactive after disable")
	}
}

func TestActionPriceUpdateDeactivates(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "priceowner", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "priced",
		Kind:         kernel.KindHTTP,
		Price:        10,
		Source:       "http://example.com",
		Description:  "test action",
		InputSchema:  minSchema,
		OutputSchema: minSchema,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}

	price := int64(20)
	updated, err := env.k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{
		ID:    a.ID,
		Price: &price,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Active {
		t.Fatal("price update should deactivate action")
	}
	if updated.Price != price {
		t.Fatalf("price: got %d, want %d", updated.Price, price)
	}
}

func TestActionDelete(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "delowner", Password: "pass",
	})
	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "to-delete",
		Kind: kernel.KindHTTP, Source: "http://example.com",
	})
	if err := env.k.DeleteAction(ctx, owner.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.k.ReadAction(ctx, a.ID); err == nil {
		t.Error("expected error reading deleted action")
	}
}

func TestActionShowPrivate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "show-owner", Password: "pass",
	})
	stranger, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "show-stranger", Password: "pass",
	})
	_ = stranger

	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "show-svc",
		Kind: kernel.KindHTTP, Source: "http://example.com",
	})

	ownerTok, _ := loginTokenFor(env.k, ctx, "show-owner", "pass")
	if err := saveToken(ownerTok); err != nil {
		t.Fatal(err)
	}
	if _, err := execTestCmd(t, actionShowCmd(), a.ID); err != nil {
		t.Errorf("owner: unexpected error: %v", err)
	}

	strangerTok, _ := loginTokenFor(env.k, ctx, "show-stranger", "pass")
	if err := saveToken(strangerTok); err != nil {
		t.Fatal(err)
	}
	if _, err := execTestCmd(t, actionShowCmd(), a.ID); err == nil {
		t.Error("stranger: expected error, got nil")
	}
}

// TestActionCreateSchemasAndAuthFromFile covers the @file.json convention (API.md C9) for
// --input-schema/--output-schema/--auth, and confirms the stored auth secret never surfaces.
func TestActionCreateSchemasAndAuthFromFile(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "authowner", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := loginTokenFor(env.k, ctx, "authowner", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	schemaFile := filepath.Join(dir, "in.json")
	if err := os.WriteFile(schemaFile, []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const secret = "s3cr3t-token-value"
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"scheme":"bearer","secrets":{"token":"`+secret+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execTestCmd(t, actionCreateCmd(), "/svc",
		"--kind", "http", "--source", "https://example.com",
		"--description", "svc", "--price", "0",
		"--input-schema", "@"+schemaFile,
		"--auth", "@"+authFile)
	if err != nil {
		t.Fatalf("action create with @file flags: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Error("auth secret leaked in action create output")
	}

	a, err := env.k.ReadActionByOwnerName(ctx, owner.ID, "/svc")
	if err != nil {
		t.Fatal(err)
	}
	if a.InputSchema["type"] != "object" {
		t.Errorf("input schema not loaded from file: %#v", a.InputSchema)
	}
	if a.AuthJSON == "" {
		t.Error("auth config not stored from --auth @file")
	}
}

// TestActionCreateFromArtifact creates a wasm action from a base64 pre-compiled
// artifact (the shape @sys/tinygo/compile returns) via the --artifact flag.
func TestActionCreateFromArtifact(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "wasmowner", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := loginTokenFor(env.k, ctx, "wasmowner", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	// A valid WASM artifact (the script package's minimal echo module), base64-encoded.
	wasm, _, _ := (&script.FakeCompiler{}).CompileSource(ctx, []byte("x"))
	b64 := base64.StdEncoding.EncodeToString(wasm)

	if _, err := execTestCmd(t, actionCreateCmd(), "/echo",
		"--kind", "wasm", "--artifact", b64,
		"--description", "echo", "--price", "0"); err != nil {
		t.Fatalf("action create --artifact: %v", err)
	}

	a, err := env.k.ReadActionByOwnerName(ctx, owner.ID, "/echo")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != kernel.KindWasm {
		t.Errorf("kind = %q, want wasm", a.Kind)
	}
	if a.ArtifactHash == "" {
		t.Error("ArtifactHash should be computed from the --artifact bytes")
	}
}

// TestActionCreateHTTPMethodParam verifies --method and --param survive the HTTP client
// round-trip into the stored source (the create request now carries them, §8).
func TestActionCreateHTTPMethodParam(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "httpowner", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := loginTokenFor(env.k, ctx, "httpowner", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := execTestCmd(t, actionCreateCmd(), "/search",
		"--kind", "http", "--source", "https://api.example.com/search",
		"--method", "GET", "--param", "q:query",
		"--description", "search", "--price", "0"); err != nil {
		t.Fatalf("action create with --method/--param: %v", err)
	}
	a, err := env.k.ReadActionByOwnerName(ctx, owner.ID, "/search")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Source, `"GET"`) || !strings.Contains(a.Source, `"q"`) {
		t.Fatalf("stored source missing method/param binding: %s", a.Source)
	}
}

func TestActionImportOpenAPI(t *testing.T) {
	const spec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	specSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(spec))
	}))
	defer specSrv.Close()

	env := newTestEnv(t)
	t.Setenv("JUICE_ALLOW_LOCAL_SOURCES", "true")

	_, err := env.k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "cli-import-owner", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := loginTokenFor(env.k, context.Background(), "cli-import-owner", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	if _, err := execTestCmd(t, actionImportCmd(), "mail", specSrv.URL+"/spec.json"); err != nil {
		t.Fatalf("action import: %v", err)
	}

	actions, err := env.k.ListAllActions(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range actions {
		if a.Name == "mail/sayHello" {
			found = true
		}
	}
	if !found {
		t.Error("expected mail/sayHello in actions after import")
	}

	// The document was named once: a re-import needs only the application's own name.
	if _, err := execTestCmd(t, actionImportCmd(), "mail"); err != nil {
		t.Fatalf("re-import by name: %v", err)
	}
}

// TestActionTreeVerbsCLI: the CLI hands the target over as typed, so one command reaches a whole
// application and an id still reaches exactly one row.
func TestActionTreeVerbsCLI(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "treeowner", Password: "pass"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := loginTokenFor(env.k, ctx, "treeowner", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) *kernel.Action {
		a, cerr := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
			OwnerUserID: owner.ID, Name: name, Kind: kernel.KindHTTP,
			Source: "http://example.com", Description: "an action",
			InputSchema: minSchema, OutputSchema: minSchema,
		})
		if cerr != nil {
			t.Fatal(cerr)
		}
		return a
	}
	root, member, sibling := mk("mail"), mk("mail/send"), mk("mailer")

	out := captureStdout(t, func() error {
		_, err := execTestCmd(t, actionEnableCmd(), "treeowner/mail")
		return err
	})
	if !strings.Contains(out, "treeowner/mail/send") || strings.Contains(out, "treeowner/mailer") {
		t.Errorf("enable must name the rows it touched and no others: %q", out)
	}
	for _, a := range []*kernel.Action{root, member} {
		got, _ := env.k.ReadAction(ctx, a.ID)
		if !got.Active {
			t.Errorf("%s should be enabled by the tree command", got.Name)
		}
	}
	if got, _ := env.k.ReadAction(ctx, sibling.ID); got.Active {
		t.Error("mailer is outside the mail tree and must be untouched")
	}

	// An id still names exactly one row.
	if _, err := execTestCmd(t, actionDisableCmd(), member.ID); err != nil {
		t.Fatalf("disable by id: %v", err)
	}
	if got, _ := env.k.ReadAction(ctx, member.ID); got.Active {
		t.Error("disable by id did not take effect")
	}
	if got, _ := env.k.ReadAction(ctx, root.ID); !got.Active {
		t.Error("disable by id must not reach the rest of the tree")
	}

	if _, err := execTestCmd(t, actionDeleteCmd(), "treeowner/mail"); err != nil {
		t.Fatalf("delete tree: %v", err)
	}
	for _, a := range []*kernel.Action{root, member} {
		if got, _ := env.k.ReadAction(ctx, a.ID); got != nil {
			t.Errorf("%s should be deleted", a.Name)
		}
	}
	if got, _ := env.k.ReadAction(ctx, sibling.ID); got == nil {
		t.Error("mailer must survive deletion of the mail tree")
	}
}

func TestActionListActive(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "listowner", Password: "pass",
	})
	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "listed",
		Kind:         kernel.KindHTTP,
		Source:       "http://example.com",
		Description:  "test action",
		InputSchema:  minSchema,
		OutputSchema: minSchema,
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)
	pub := kernel.VisibilityPublic
	_, _ = env.k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pub})

	actions, err := env.k.ListVisibleActions(ctx, false, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, act := range actions {
		if act.ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Error("active public action should appear in ListActions")
	}
}

func TestStatsInitializedOnActivation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "statsowner", Password: "p",
	})
	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "svc",
		Kind:         kernel.KindHTTP,
		Source:       "http://x.com",
		Description:  "test action",
		InputSchema:  minSchema,
		OutputSchema: minSchema,
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	stats, err := env.k.ReadStats(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats == nil {
		t.Error("expected stats after activation")
	}
	if stats.Uses != 0 {
		t.Errorf("expected 0 uses for fresh action, got %d", stats.Uses)
	}
}

func TestLookupWithoutEmbedderDegrades(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// With no embedder, lookup degrades to lexical ranking instead of erroring (§9).
	if _, err := env.k.Lookup(ctx, kernel.LookupRequest{Query: "test", Limit: 5}); err != nil {
		t.Errorf("lookup without an embedder should not error, got %v", err)
	}
}

// ---- process ----

func setupProcessCmd(t *testing.T, env *testEnv, ownerID string, funds int64) (*kernel.Process, *kernel.Trace) {
	t.Helper()
	ctx := context.Background()
	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	tr := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: ownerID,
		CallerUserID:  ownerID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := env.db.BeginRun(ctx, p, tr, ownerID, funds, 0, 0); err != nil {
		t.Fatalf("setupProcessCmd: %v", err)
	}
	return p, tr
}

func TestProcessStartFundEnd(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.Account{
		ID:        "user-proc-test",
		Handle:    "proctest",
		Available: 2000,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	hash, _ := kernel.HashPassword("pass")
	owner.PasswordHash = hash
	_ = env.db.CreateUser(ctx, owner)

	p, _ := setupProcessCmd(t, env, owner.ID, 500)
	proc, err := env.db.ReadProcess(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// With BeginRun, process.available=0 (funds are in the root trace).
	if proc.Available != 0 || proc.Status != kernel.ProcessOpen {
		t.Errorf("process initial state: available=%d status=%s, want 0/open", proc.Available, proc.Status)
	}

	if err := env.k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	p3, _ := env.k.ReadProcess(ctx, owner.ID, p.ID)
	if p3.Status != kernel.ProcessClosed {
		t.Error("process should be closed after end")
	}
}

func TestProcessList(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "list-proc", Password: "p",
	})
	other, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "list-proc-other", Password: "p",
	})

	setupProcessCmd(t, env, owner.ID, 0)
	setupProcessCmd(t, env, owner.ID, 0)
	setupProcessCmd(t, env, other.ID, 0)

	processes, err := env.k.ListProcesses(ctx, owner.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 2 {
		t.Errorf("ListProcesses: got %d, want 2", len(processes))
	}
	for _, p := range processes {
		if p.OwnerUserID != owner.ID {
			t.Errorf("unexpected owner %s", p.OwnerUserID)
		}
	}
}

func TestProcessNegativeFundsFails(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "negfund", Password: "p",
	})
	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: owner.ID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	tr := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: owner.ID,
		CallerUserID:  owner.ID,
		CreatedAt:     time.Now().UTC(),
	}
	err := env.db.BeginRun(ctx, p, tr, owner.ID, -1, 0, 0)
	if err == nil {
		t.Error("expected error creating process with negative funds")
	}
}

func TestProcessEndReturnsBalance(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.Account{
		ID:        "balance-return-user",
		Handle:    "baltest",
		Available: 1000,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	hash, _ := kernel.HashPassword("pass")
	owner.PasswordHash = hash
	_ = env.db.CreateUser(ctx, owner)

	p, _ := setupProcessCmd(t, env, owner.ID, 400)

	u, _ := env.db.ReadUser(ctx, owner.ID)
	if u.Available != 600 {
		t.Errorf("owner balance after CreateProcess: got %d, want 600", u.Available)
	}

	if err := env.k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	u2, _ := env.db.ReadUser(ctx, owner.ID)
	if u2.Available != 1000 {
		t.Errorf("owner balance after end: got %d, want 1000", u2.Available)
	}
}

// ---- step ----

func assertActionRef(t *testing.T, v any) {
	t.Helper()
	s, ok := v.(string)
	if !ok || s == "" {
		t.Errorf("action field is not a non-empty string: %v", v)
		return
	}
	if strings.HasPrefix(s, "@") {
		t.Errorf("action field must be a bare owner/name reference (no @ sigil), got %q", s)
	}
	if !strings.Contains(s, "/") {
		t.Errorf("action field must contain a /, got %q", s)
	}
}

func newStepBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func createStepAction(t *testing.T, srv *httptest.Server, backendURL, ownerTok, handle, name string) (string, string) {
	t.Helper()
	emptySchema := map[string]any{"type": "object", "properties": map[string]any{}}
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": name, "kind": "http", "price": 0, "source": backendURL,
		"description":   "test step action",
		"input_schema":  emptySchema,
		"output_schema": emptySchema,
	}, ownerTok)
	var act map[string]any
	decodeResponse(t, cr, &act)
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create action %s: expected 201, got %d", name, cr.StatusCode)
	}
	id := act["id"].(string)
	er := httpDo(t, srv, "POST", "/v1/actions/enable", map[string]any{"target": id}, ownerTok)
	er.Body.Close()
	if er.StatusCode != http.StatusOK {
		t.Fatalf("enable action %s: expected 200, got %d", name, er.StatusCode)
	}
	httpDo(t, srv, "PUT", "/v1/actions", map[string]any{"target": id, "visibility": "public"}, ownerTok).Body.Close()
	return id, handle + "/" + name
}

func TestServeCreateStep(t *testing.T) {
	backend := newStepBackend(t)
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "cs-create-owner")
	makeUser(t, k, "cs-create-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "cs-create-owner", "cs-create-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	resp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action":          actionID,
		"required_caller": "cs-create-caller",
		"partial_args":    map[string]any{"preset": "val"},
	}, ownerTok)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("POST /v1/steps: expected 201, got %d", resp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, resp, &step)
	if step["id"] == nil || step["id"] == "" {
		t.Error("expected step id in response")
	}
	if step["status"] != "waiting" {
		t.Errorf("expected status=waiting, got %v", step["status"])
	}
	if step["action"] == nil || step["action"] == "" {
		t.Error("expected computed action field in POST /v1/steps response")
	}
	assertActionRef(t, step["action"])
}

func TestServeListSteps(t *testing.T) {
	backend := newStepBackend(t)
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "sl-steps-owner")
	makeUser(t, k, "sl-steps-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "sl-steps-owner", "sl-steps-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	for range 2 {
		r := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
			"trace_id":        traceID,
			"action":          actionID,
			"required_caller": "sl-steps-caller",
			"partial_args":    map[string]any{},
		}, ownerTok)
		if r.StatusCode != http.StatusCreated {
			r.Body.Close()
			t.Fatalf("create step: expected 201, got %d", r.StatusCode)
		}
		r.Body.Close()
	}

	resp := httpDo(t, srv, "GET", "/v1/steps", nil, ownerTok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /v1/steps: expected 200, got %d", resp.StatusCode)
	}
	var steps []map[string]any
	decodeResponse(t, resp, &steps)
	if len(steps) < 2 {
		t.Errorf("expected at least 2 steps, got %d", len(steps))
	}
	for _, s := range steps {
		assertActionRef(t, s["action"])
	}

	resp2 := httpDo(t, srv, "GET", "/v1/steps?process_id="+pid, nil, ownerTok)
	if resp2.StatusCode != http.StatusOK {
		resp2.Body.Close()
		t.Fatalf("GET /v1/steps?process_id: expected 200, got %d", resp2.StatusCode)
	}
	var filtered []map[string]any
	decodeResponse(t, resp2, &filtered)
	if len(filtered) < 2 {
		t.Errorf("expected at least 2 steps by process_id filter, got %d", len(filtered))
	}

	resp3 := httpDo(t, srv, "GET", "/v1/steps?status=waiting", nil, ownerTok)
	if resp3.StatusCode != http.StatusOK {
		resp3.Body.Close()
		t.Fatalf("GET /v1/steps?status=waiting: expected 200, got %d", resp3.StatusCode)
	}
	var waitingSteps []map[string]any
	decodeResponse(t, resp3, &waitingSteps)
	for _, s := range waitingSteps {
		if s["status"] != "waiting" {
			t.Errorf("list with status=waiting returned step with status=%v", s["status"])
		}
	}

	// limit bounds the step page (previously GET /v1/steps was unbounded at every layer).
	resp4 := httpDo(t, srv, "GET", "/v1/steps?limit=1", nil, ownerTok)
	if resp4.StatusCode != http.StatusOK {
		resp4.Body.Close()
		t.Fatalf("GET /v1/steps?limit=1: expected 200, got %d", resp4.StatusCode)
	}
	var limited []map[string]any
	decodeResponse(t, resp4, &limited)
	if len(limited) != 1 {
		t.Errorf("limit=1: want 1 step, got %d", len(limited))
	}
}

// TestCLIListPaginationFlags is the thin-wire guard that `process list`, `step list`, and
// `user ledger` register and forward --limit/--offset (a missing flag would make cobra error).
func TestCLIListPaginationFlags(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "page-cli", Password: "pass",
	}); err != nil {
		t.Fatal(err)
	}
	tok, _ := loginTokenFor(env.k, ctx, "page-cli", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	if _, err := execTestCmd(t, processListCmd(), "--limit", "1", "--offset", "0"); err != nil {
		t.Errorf("process list --limit/--offset: %v", err)
	}
	if _, err := execTestCmd(t, stepListCmd(), "--limit", "1", "--offset", "0"); err != nil {
		t.Errorf("step list --limit/--offset: %v", err)
	}
	if _, err := execTestCmd(t, userLedgerCmd(), "--limit", "1", "--offset", "0"); err != nil {
		t.Errorf("user ledger --limit/--offset: %v", err)
	}
}

func TestServeGetStep(t *testing.T) {
	backend := newStepBackend(t)
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "gs-steps-owner")
	_, callerTok := makeUser(t, k, "gs-steps-caller")
	_, unrelTok := makeUser(t, k, "gs-steps-unrelated")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "gs-steps-owner", "gs-steps-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action":          actionID,
		"required_caller": "gs-steps-caller",
		"partial_args":    map[string]any{},
	}, ownerTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	sid := step["id"].(string)

	r1 := httpDo(t, srv, "GET", "/v1/steps/"+sid, nil, ownerTok)
	if r1.StatusCode != http.StatusOK {
		r1.Body.Close()
		t.Errorf("owner GET /v1/steps/%s: expected 200, got %d", sid, r1.StatusCode)
	} else {
		var got map[string]any
		decodeResponse(t, r1, &got)
		if got["id"] != sid {
			t.Error("step id mismatch in read response")
		}
		if got["action"] == nil || got["action"] == "" {
			t.Error("expected computed action field in GET /v1/steps/{id} response")
		}
		assertActionRef(t, got["action"])
	}

	r2 := httpDo(t, srv, "GET", "/v1/steps/"+sid, nil, callerTok)
	if r2.StatusCode != http.StatusOK {
		r2.Body.Close()
		t.Errorf("caller GET /v1/steps/%s: expected 200, got %d", sid, r2.StatusCode)
	} else {
		r2.Body.Close()
	}

	r3 := httpDo(t, srv, "GET", "/v1/steps/"+sid, nil, unrelTok)
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusForbidden {
		t.Errorf("unrelated GET /v1/steps/%s: expected 403, got %d", sid, r3.StatusCode)
	}
}

func TestServeCompleteStepMissingArgs(t *testing.T) {
	backend := newStepBackend(t)
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "csmiss-owner")
	_, callerTok := makeUser(t, k, "csmiss-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "csmiss-owner", "csmiss-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action":          actionID,
		"required_caller": "csmiss-caller",
		"partial_args":    map[string]any{},
	}, ownerTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	sid := step["id"].(string)

	resp := httpDo(t, srv, "POST", "/v1/steps/"+sid+"/complete", map[string]any{
		"not_args": "value",
	}, callerTok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("complete missing args: expected 422, got %d", resp.StatusCode)
	}
}

func TestServeCompleteStep(t *testing.T) {
	backend := newStepBackend(t)
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "cs2-owner")
	_, callerTok := makeUser(t, k, "cs2-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "cs2-owner", "cs2-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id":        traceID,
		"action":          actionID,
		"required_caller": "cs2-caller",
		"partial_args":    map[string]any{"from_partial": "A"},
	}, ownerTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	sid := step["id"].(string)

	complResp := httpDo(t, srv, "POST", "/v1/steps/"+sid+"/complete", map[string]any{
		"args": map[string]any{"from_caller": "B"},
	}, callerTok)
	if complResp.StatusCode != http.StatusOK {
		complResp.Body.Close()
		t.Fatalf("complete step: expected 200, got %d", complResp.StatusCode)
	}
	var reply map[string]any
	decodeResponse(t, complResp, &reply)
	if reply["tx_id"] == nil || reply["tx_id"] == "" {
		t.Error("expected tx_id in complete step reply")
	}
	if reply["step_id"] != sid {
		t.Errorf("expected step_id=%s in reply, got %v", sid, reply["step_id"])
	}

	getResp := httpDo(t, srv, "GET", "/v1/steps/"+sid, nil, ownerTok)
	var doneStep map[string]any
	decodeResponse(t, getResp, &doneStep)
	if doneStep["status"] != "done" {
		t.Errorf("expected step status=done after complete, got %v", doneStep["status"])
	}
}

// TestStepCompleteFileArg verifies the positional json argument of `step complete` honours the
// @file convention (API.md C9): a missing file is reported as a read error before any kernel
// call, rather than the literal bytes "file" being shipped as the step input.
func TestStepCompleteFileArg(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "sc-caller", Password: "pass",
	}); err != nil {
		t.Fatal(err)
	}
	tok, _ := loginTokenFor(env.k, ctx, "sc-caller", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(t.TempDir(), "absent.json")
	_, err := execTestCmd(t, stepCompleteCmd(), "no-such-step", "@"+missing)
	if err == nil || !strings.Contains(err.Error(), "read file") {
		t.Errorf("expected file-read error for @missing-file, got %v", err)
	}
}

// ---- tx ----

func TestTransactionListEmpty(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "txowner", Password: "p",
	})

	txs, err := env.k.ListTransactions(ctx, owner.ID, kernel.TxFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 0 {
		t.Errorf("expected 0 transactions, got %d", len(txs))
	}
}

func TestTransactionRate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "rateowner", Password: "p",
	})
	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "rateable",
		Kind: kernel.KindHTTP, Source: "http://example.com",
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	p, _ := setupProcessCmd(t, env, owner.ID, 0)

	_, err := env.k.RateTransaction(ctx, owner.ID, "nonexistent-tx", 1, nil)
	if err == nil {
		t.Error("expected error rating nonexistent transaction")
	}
	_ = p
}

func TestTransactionRating(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "rater", Password: "p",
	})
	p, _ := setupProcessCmd(t, env, owner.ID, 0)

	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "tx-rate-svc",
		Kind:        kernel.KindHTTP,
		Source:      "http://example.com",
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	txs, _ := env.k.ListTransactions(ctx, owner.ID, kernel.TxFilter{Limit: 10})
	if len(txs) != 0 {
		t.Errorf("expected 0 txs before any call, got %d", len(txs))
	}

	_ = p
}

// ---- run ----

func TestCallClosedProcess(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.Account{
		ID:        uuid.New().String(),
		Handle:    "call-owner",
		Available: 1000,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	hash, _ := kernel.HashPassword("pass")
	owner.PasswordHash = hash
	_ = env.db.CreateUser(ctx, owner)

	p, tr := setupProcessCmd(t, env, owner.ID, 100)
	_ = env.k.EndProcess(ctx, owner.ID, p.ID)

	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "echo",
		Kind: kernel.KindHTTP, Source: "http://example.com/echo",
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	_, err := env.k.Subcall(ctx, kernel.SubcallRequest{
		CallerID:      owner.ID,
		ParentTraceID: tr.ID,
		ActionRef:     owner.Handle + "/echo",
		Args:          map[string]any{},
	})
	if err == nil {
		t.Error("expected error calling on closed process")
	}
}

func TestCallInsufficientFunds(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "poorowner", Password: "p",
	})

	// Description and schemas are what make the action activatable (§7); without them SetActive
	// fails and the run would be refused as inactive long before its price is ever weighed.
	a, err := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "expensive",
		Kind: kernel.KindHTTP, Source: "http://x.com", Price: 100,
		Description: "costs more than the caller has",
		InputSchema: minSchema, OutputSchema: minSchema,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}

	_, err = env.k.Run(ctx, kernel.RunRequest{CallerID: owner.ID, ActionRef: "poorowner/expensive", Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrInsufficientFunds) {
		t.Errorf("a caller who cannot afford the price: got %v, want ErrInsufficientFunds", err)
	}
}

// ---- remote ----

func newRemoteTestKernel(t *testing.T) (*kernel.Kernel, *store.DB) {
	t.Helper()
	db := newTestStore(t)
	t.Setenv("JUICE_SECRET_KEY", "remote-test-secret")
	k := newKernel(testConfig("remote-test-secret"), kernel.Dependencies{Store: db})

	if err := k.FirstBoot(t.Context(), "sys-pass", ""); err != nil {
		t.Fatal(err)
	}
	return k, db
}

func TestRemoteImport(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	const actionID = "action-remote-id"
	m := kernel.ActionManifest{
		ActionID:     actionID,
		OwnerHandle:  "import-remote",
		Name:         "greet",
		Description:  "says hello",
		Kind:         kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef",
		UpdatedAt:    time.Now(),
	}
	sig, err := testNet.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			json.NewEncoder(w).Encode([]map[string]string{{"ID": actionID, "Name": "greet"}})
		}
	}))
	defer remote.Close()

	remoteUser, err := k.EnsureKernelAccount(t.Context(), pubB64)
	if err != nil {
		t.Fatal(err)
	}

	// Cold resolve caches and activates the proxy (§8): the sole import path.
	if _, err := k.ImportPeerAction(t.Context(), remoteUser.ID, m); err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}

	actions, err := k.ListAllActions(t.Context(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range actions {
		if a.Name == "import-remote/greet" { // owner-qualified: addressed @import-remote.import-remote/greet
			found = true
		}
	}
	if !found {
		t.Error("expected imported action import-remote/greet to appear in @import-remote's actions")
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it printed.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if runErr != nil {
		t.Fatalf("render fn returned error: %v", runErr)
	}
	return buf.String()
}

// TestQuietPrintsIdentifiersOnly: --quiet means one thing on every command, reads included (§14 C8)
// — the resource id and nothing else, one per line, so output pipes into the next command. It was
// previously honoured on three creation commands and silently ignored on every read, which makes
// the flag unusable in a script.
func TestQuietPrintsIdentifiersOnly(t *testing.T) {
	old := flagQuiet
	flagQuiet = true
	t.Cleanup(func() { flagQuiet = old })

	t.Run("detail view prints the id", func(t *testing.T) {
		out := captureStdout(t, func() error {
			return emit([]byte(`{"id":"act-1","action":"bob/echo","price":10,"description":"d"}`), output{})
		})
		if out != "act-1\n" {
			t.Errorf("emit --quiet = %q, want %q", out, "act-1\n")
		}
	})

	t.Run("a response naming no resource prints nothing", func(t *testing.T) {
		out := captureStdout(t, func() error { return emit([]byte(`{"status":"ok"}`), output{}) })
		if out != "" {
			t.Errorf("--quiet must suppress a response with no resource id, got %q", out)
		}
	})

	t.Run("list prints one id per line", func(t *testing.T) {
		var ids []string
		stubServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "act-1", "action": "bob/echo", "price": 1},
				{"id": "act-2", "action": "bob/other", "price": 2},
			})
		})
		out := captureStdout(t, func() error {
			_, err := execTestCmd(t, actionListCmd())
			return err
		})
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line != "" {
				ids = append(ids, line)
			}
		}
		if len(ids) != 2 || ids[0] != "act-1" || ids[1] != "act-2" {
			t.Errorf("action list --quiet = %v, want [act-1 act-2]", ids)
		}
	})
}

// TestPrintTextParity asserts that the field view surfaces every field the canonical JSON
// (what the HTTP API returns) carries — the CLI/HTTP parity invariant (§14). It also
// checks that structured values are rendered as indented JSON.
func TestPrintTextParity(t *testing.T) {
	objects := []any{
		enrichAction(&kernel.Kernel{}, &kernel.Action{
			ID: "a1", OwnerUserID: "u1", OwnerHandle: "alice", Name: "weather",
			Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic, Price: 5,
			Description:  "current weather",
			InputSchema:  map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
		}, newAccountCache(&kernel.Kernel{}, context.Background())),
		&kernel.TransactionView{Transaction: &kernel.Transaction{ID: "t1", Status: "success", Gross: 10, Net: 8, Fee: 2}},
		&stepWithAction{Step: &kernel.Step{ID: "s1", Status: "waiting"}, Action: "alice/weather"},
	}
	for _, obj := range objects {
		// Canonical key set from the marshaled object (what HTTP would send).
		raw, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		text := captureStdout(t, func() error { return printFields(raw, nil, kernel.Network{}) })
		for key := range fields {
			if !strings.Contains(text, key+":") {
				t.Errorf("%T text output missing field %q\n%s", obj, key, text)
			}
		}
	}

	// Structured values must appear as indented JSON, not be dropped.
	schemas, err := json.Marshal(enrichAction(&kernel.Kernel{}, &kernel.Action{
		ID: "a1", Name: "x", Kind: kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
		OutputSchema: map[string]any{"type": "object"},
	}, newAccountCache(&kernel.Kernel{}, context.Background())))
	if err != nil {
		t.Fatal(err)
	}
	text := captureStdout(t, func() error { return printFields(schemas, nil, kernel.Network{}) })
	if !strings.Contains(text, "input_schema: {") || !strings.Contains(text, `"type": "object"`) {
		t.Errorf("input_schema not rendered as indented JSON:\n%s", text)
	}
}

// TestDirectorySelector: the run grant-required hint groups an action by its directory (§8).
func TestDirectorySelector(t *testing.T) {
	cases := map[string]string{
		"alice/mail/send": "alice/mail",
		"alice/send":      "alice/send",
		"a/x/y/z":         "a/x/y",
		"noslash":         "noslash",
	}
	for in, want := range cases {
		if got := directorySelector(in); got != want {
			t.Errorf("directorySelector(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCLIActionRatings drives `juice action ratings <owner/name>` end to end: a caller runs a
// public action and rates it, and the owner reads the projection through the CLI. Exercises the
// every-command rule (§14) and confirms the human view surfaces the value and note.
func TestCLIActionRatings(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(backend.Close)

	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "rate-owner", Password: "pass"})
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := kernel.HashPassword("pass")
	caller := &kernel.Account{
		ID: uuid.NewString(), Handle: "rate-caller", PasswordHash: hash, Available: 100,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := env.db.CreateUser(ctx, caller); err != nil {
		t.Fatal(err)
	}

	a, err := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "svc", Kind: kernel.KindHTTP, Source: backend.URL,
		Description: "rate me", InputSchema: minSchema, OutputSchema: minSchema, Price: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	pub := kernel.VisibilityPublic
	if _, err := env.k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pub}); err != nil {
		t.Fatal(err)
	}
	if err := env.k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}

	reply, err := env.k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: "rate-owner/svc", Args: map[string]any{}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	note := "reliable"
	if _, err := env.k.RateTransaction(ctx, caller.ID, reply.TxID, 1, &note); err != nil {
		t.Fatalf("rate: %v", err)
	}

	tok, _ := loginTokenFor(env.k, ctx, "rate-owner", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error {
		_, err := execTestCmd(t, actionRatingsCmd(), "rate-owner/svc")
		return err
	})
	if !strings.Contains(out, "1") || !strings.Contains(out, note) {
		t.Errorf("action ratings output missing value/note: %q", out)
	}

	// A public action's ratings are public evidence, readable with no session at all (§11) — the
	// reference resolution the CLI performs first must not put a login in front of them. No
	// session means no login selected: a login that IS selected and holds none is not a stranger,
	// it is a broken session, and says so rather than reading as one (D20).
	cfg := loadClientConfig()
	cfg.Current = ""
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	anon := captureStdout(t, func() error {
		_, err := execTestCmd(t, actionRatingsCmd(), "rate-owner/svc")
		return err
	})
	if !strings.Contains(anon, note) {
		t.Errorf("anonymous action ratings on a public action: %q", anon)
	}
}

// TestActionImportNameAndRootReference: `action import <name>` lands one document as one
// application, and every action subcommand reaches it by its own name — the reference travels
// untouched to the server, so the CLI holds no naming rules of its own.
func TestActionImportNameAndRootReference(t *testing.T) {
	const spec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/":{"get":{"operationId":"index","description":"the application","parameters":[{"name":"q","in":"query","description":"query","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}},"/hello":{"get":{"operationId":"greet","description":"says hello","parameters":[{"name":"name","in":"query","description":"who","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	specSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(spec))
	}))
	defer specSrv.Close()

	env := newTestEnv(t)
	ctx := context.Background()
	t.Setenv("JUICE_ALLOW_LOCAL_SOURCES", "true")

	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "app-cli-owner", Password: "pass"}); err != nil {
		t.Fatal(err)
	}
	tok, _ := loginTokenFor(env.k, ctx, "app-cli-owner", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	if _, err := execTestCmd(t, actionImportCmd(), "mail", specSrv.URL+"/spec.json"); err != nil {
		t.Fatalf("action import: %v", err)
	}
	names := map[string]bool{}
	actions, err := env.k.ListAllActions(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		names[a.Name] = true
	}
	if !names["mail/index"] || !names["mail/greet"] {
		t.Fatalf("imported under the application name: got %v", names)
	}

	// The group answers to its own name, and so does the owner's root once one exists.
	if _, err := execTestCmd(t, actionShowCmd(), "app-cli-owner/mail"); err != nil {
		t.Errorf("action show on the group root: %v", err)
	}
	ownerID := actions[0].OwnerUserID
	if _, err := env.k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: "index",
		Kind: kernel.KindHTTP, Source: "http://example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := execTestCmd(t, actionShowCmd(), "app-cli-owner"); err != nil {
		t.Errorf("action show on the owner root: %v", err)
	}
	// The same document under a second name is an independent application, and a second document
	// under an occupied name is refused — both decided by the server, not the CLI.
	if _, err := execTestCmd(t, actionImportCmd(), "inbox", specSrv.URL+"/spec.json"); err != nil {
		t.Errorf("a second installation of one document: %v", err)
	}
	if _, err := execTestCmd(t, actionImportCmd(), "mail", specSrv.URL+"/other.json"); err == nil {
		t.Error("re-binding a name to another document must be refused")
	}
}

// An act that cannot be undone defaults to no. A bare Enter on a prompt about money must not move
// it, declining must be an error so a script does not read silence as success, and off a terminal
// there is nobody to ask.
func TestConfirmDefaultsToNo(t *testing.T) {
	orig := interactiveTTY
	interactiveTTY = func() bool { return true }
	t.Cleanup(func() { interactiveTTY = orig })

	for _, tc := range []struct{ typed, want string }{
		{"\n", "cancelled"}, {"n\n", "cancelled"}, {"no\n", "cancelled"},
		{"y\n", ""}, {"YES\n", ""},
	} {
		withStdin(t, tc.typed)
		err := confirm("Send everything?", false)
		if tc.want == "" && err != nil {
			t.Errorf("typed %q: got %v, want acceptance", tc.typed, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("typed %q: got %v, want %q", tc.typed, err, tc.want)
		}
	}
	// --yes is the only way to confirm where nobody can be asked.
	if err := confirm("Send everything?", true); err != nil {
		t.Errorf("--yes: %v", err)
	}
	interactiveTTY = func() bool { return false }
	if err := confirm("Send everything?", false); err == nil {
		t.Error("a confirmation with no terminal to ask on was assumed")
	}
}

// withStdin points os.Stdin at the given text for one check.
func withStdin(t *testing.T, text string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(text); err != nil {
		t.Fatal(err)
	}
	w.Close()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig; r.Close() })
}

// TestEmitFollowsOneOutputPolicy pins the single rule every command's output obeys (§14 C8):
// --json is the server's body exactly as it arrived, --quiet is ids one per line — a list yields
// one per row, and a reply naming no resource yields nothing — and otherwise the command renders
// it. One function decides this for every command, so no command can drift from the promise.
func TestEmitFollowsOneOutputPolicy(t *testing.T) {
	one := []byte(`{"id":"a-1","price":10}`)
	many := []byte(`[{"id":"a-1"},{"id":"a-2"}]`)
	run := []byte(`{"tx_id":"t-9","result":{}}`)
	said := output{human: func([]byte) error { fmt.Println("done."); return nil }}
	for _, c := range []struct {
		name        string
		json, quiet bool
		body        []byte
		out         output
		want        string
	}{
		{"json relays the body", true, false, one, output{}, "{\n  \"id\": \"a-1\",\n  \"price\": 10\n}\n"},
		{"json of an empty reply is nothing", true, false, nil, said, ""},
		{"json ignores the command's own rendering", true, false, many, said, "[\n  {\n    \"id\": \"a-1\"\n  },\n  {\n    \"id\": \"a-2\"\n  }\n]\n"},
		{"quiet prints one resource's id", false, true, one, output{}, "a-1\n"},
		{"quiet prints a list's ids, one per line", false, true, many, output{}, "a-1\na-2\n"},
		{"quiet prints the field the command names", false, true, run, output{id: "tx_id"}, "t-9\n"},
		{"quiet prints nothing for a reply naming no resource", false, true, []byte(`{"status":"ok"}`), said, ""},
		{"quiet prints nothing for an empty reply", false, true, nil, said, ""},
		{"the command renders its own view", false, false, many, said, "done.\n"},
		{"a rendering runs on an empty reply too", false, false, nil, said, "done.\n"},
		{"without one, every field the reply carries is shown", false, false, one, output{}, "  id: a-1\n  price: 10\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			oldJSON, oldQuiet := flagJSON, flagQuiet
			flagJSON, flagQuiet = c.json, c.quiet
			t.Cleanup(func() { flagJSON, flagQuiet = oldJSON, oldQuiet })
			if got := captureStdout(t, func() error { return emit(c.body, c.out) }); got != c.want {
				t.Errorf("emit = %q, want %q", got, c.want)
			}
		})
	}
}

// TestTheFieldViewScalesOnlyTheFieldsItIsGiven: money is written and read in one unit, the world's
// (D20), so a field view shows an amount the way the command that asked for it is written. Which
// fields those are is named by the command, never guessed from a field's name — an action's own
// arguments and results ride inside these replies, and a result that happens to carry "price" is
// the action's own number, not this kernel's money.
func TestTheFieldViewScalesOnlyTheFieldsItIsGiven(t *testing.T) {
	body := []byte(`{"price":1500000,"result":{"price":1500000},"uses":1500000}`)
	net := kernel.Network{Name: "play", Decimals: 6, Symbol: "credits"}
	got := captureStdout(t, func() error { return printFields(body, moneyAction, net) })
	if !strings.Contains(got, "price: 1.50 credits") {
		t.Errorf("a named money field was not written in the world's unit:\n%s", got)
	}
	if !strings.Contains(got, `"price": 1500000`) {
		t.Errorf("a field inside the action's own result was rewritten as money:\n%s", got)
	}
	if !strings.Contains(got, "uses: 1500000") {
		t.Errorf("a field the command did not name was rewritten as money:\n%s", got)
	}
}

// TestMoneyIsShownTheWayItIsWritten closes the loop over HTTP: a price given the way this kernel
// writes money comes back the same way, while --json stays in the base units a program counts in.
func TestMoneyIsShownTheWayItIsWritten(t *testing.T) {
	stubKernel(t, 6, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "a-1", "action": "bob/echo", "price": 1500000})
	})
	for _, c := range []struct {
		name string
		json bool
		want string
	}{
		{"a person reads it in the world's unit", false, "price: 1.50 credits"},
		{"a program reads the base units", true, `"price": 1500000`},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := flagJSON
			flagJSON = c.json
			t.Cleanup(func() { flagJSON = old })
			got := captureStdout(t, func() error {
				return freshClient().emit("GET", "/v1/actions/a-1", nil, output{money: moneyAction})
			})
			if !strings.Contains(got, c.want) {
				t.Errorf("want %q in:\n%s", c.want, got)
			}
		})
	}
}

// TestTheUnitIsReadBeforeTheRequest: a command that will show money reads the world's unit first,
// so a unit it cannot read refuses before anything is sent. Reading it afterwards would report
// "nothing was sent" about a write that had already committed.
func TestTheUnitIsReadBeforeTheRequest(t *testing.T) {
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		sent = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "w-1", "amount": 5})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("JUICE_HOME", t.TempDir())
	resetHealthCache()
	t.Cleanup(resetHealthCache)
	old := flagServer
	flagServer = srv.URL
	t.Cleanup(func() { flagServer = old })
	selectTestLogin(t, "tester@stub", srv.URL)

	if err := freshClient().emit("POST", "/v1/withdrawals", map[string]any{"amount": 5}, output{money: moneyRail}); err == nil {
		t.Fatal("a command that could not read the world's unit went ahead anyway")
	}
	if sent {
		t.Error("the request was sent before the unit it would be shown in could be read")
	}
}

// TestTheAdviceOnAnErrorNamesCommandsThatExist: what a failure tells someone to do next is the
// whole value of the message, and it is written in one place (§14). Advice naming a command that
// has since been deleted — or renamed — is worse than none, so every command any of it names is
// resolved against the command tree.
func TestTheAdviceOnAnErrorNamesCommandsThatExist(t *testing.T) {
	parked := &kernel.KernelError{Code: "timeout", Meta: map[string]string{"process_id": "p-1"}}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"a call needing consent", kernel.ErrGrantRequired.Wrap("x").WithMeta("action", "bob/echo"), "user connect"},
		{"a peer that is offline", kernel.ErrPeerUnreachable.Wrap("x").WithMeta("peer", "other"), "nothing was charged"},
		{"a peer that will not serve on credit", kernel.ErrPeerUnfunded.Wrap("x").WithMeta("peer", "other"), "other declined"},
		{"terms that changed under a pin", &kernel.KernelError{Code: "terms_changed", Meta: map[string]string{"quote_hash": "h", "price": "2"}}, "--quote-hash h"},
		{"money parked on a peer", parked, "process show"},
		{"money parked, with how long it has waited", parked.WithMeta("pending_since", "2026-01-01T00:00:00Z"), "since 2026-01-01T00:00:00Z"},
		{"a call that ran and failed", (&kernel.KernelError{Code: "execution_failed", Meta: map[string]string{"tx_id": "t-1", "charge": "0"}}), "tx show t-1"},
		{"a fault in juice", kernel.ErrInternal.Wrap("x"), "--verbose"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hint := remedy(c.err)
			if hint == "" {
				t.Fatal("a failure an operator must act on said nothing")
			}
			if !strings.Contains(strings.ToLower(hint), strings.ToLower(c.want)) {
				t.Errorf("the advice does not mention %q:\n%s", c.want, hint)
			}
			for _, named := range namedCommands(hint) {
				if !resolves(named) {
					t.Errorf("the advice names `juice %s`, which is not a command:\n%s", strings.Join(named, " "), hint)
				}
			}
		})
	}
}

// namedCommands pulls every `juice ...` the text tells the operator to run, as the words following
// it: a flag, a dash, or a reference ends one, since nothing past that addresses a command.
func namedCommands(hint string) [][]string {
	var out [][]string
	for _, field := range strings.Split(hint, "juice ")[1:] {
		var words []string
		for _, w := range strings.Fields(field) {
			w = strings.Trim(w, "`.,;:")
			if w == "" || w == "—" || strings.HasPrefix(w, "-") || strings.ContainsAny(w, "/@") {
				break
			}
			words = append(words, w)
		}
		if len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

// resolves walks the command tree by name: every word must be a subcommand until one that has none
// is reached, after which the rest are its arguments. Cobra's own Find answers "the deepest command
// I could match", which would accept `admin deposit` for as long as `admin` exists — the very kind
// of stale advice this checks for.
func resolves(words []string) bool {
	c := rootCmd
	for _, w := range words {
		if !c.HasSubCommands() {
			return true // a leaf: what follows is an argument to it
		}
		next := (*cobra.Command)(nil)
		for _, sub := range c.Commands() {
			if sub.Name() == w || sub.HasAlias(w) {
				next = sub
				break
			}
		}
		if next == nil {
			return false
		}
		c = next
	}
	return c.Runnable()
}

// TestAdminKernelShowRelaysEveryFieldTheServerSent: the identity view is read whole — an operator
// checks a kernel's network digest here before believing anything else it says. The CLI once kept
// its own copy of the response shape, and the field it had not copied vanished from --json.
func TestAdminKernelShowRelaysEveryFieldTheServerSent(t *testing.T) {
	body := map[string]any{
		"handle": "acme", "public_key": "KEY", "network": "play",
		"network_digest": "5e0dcafe", "lottery": 1000000, "lottery_max": 5000000,
		"credit_limit": 500000000, "exposure": 0, "fee_bps": 2000, "remote_bps": 500, "import_bps": 500,
	}
	stubKernel(t, 6, func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(body) })
	old := flagJSON
	flagJSON = true
	t.Cleanup(func() { flagJSON = old })

	out := captureStdout(t, func() error { _, err := execTestCmd(t, identityCmd()); return err })
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json did not print JSON: %v\n%s", err, out)
	}
	for field := range body {
		if _, ok := got[field]; !ok {
			t.Errorf("--json dropped %q, which the server sent:\n%s", field, out)
		}
	}
}

// TestAWithdrawalIsNamedByTheCaller: the server returns the row a withdrawal id already made
// rather than making a second one (U51), so repeating the command with the same id after a lost
// reply recovers the first withdrawal. That is only true if the id is the caller's to repeat.
func TestAWithdrawalIsNamedByTheCaller(t *testing.T) {
	var ids []string
	stubKernel(t, 6, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			ids = append(ids, req.ID)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "w-1", "amount": 5000000, "status": "pending"})
	})
	for i := 0; i < 2; i++ {
		if _, err := execTestCmd(t, userWithdrawCmd(), "5", "--yes", "--id", "7a3c"); err != nil {
			t.Fatalf("withdraw: %v", err)
		}
	}
	if len(ids) != 2 || ids[0] != "7a3c" || ids[1] != "7a3c" {
		t.Fatalf("the id the caller gave was not the one sent: %v", ids)
	}
	for i := 0; i < 2; i++ {
		if _, err := execTestCmd(t, userWithdrawCmd(), "5", "--yes"); err != nil {
			t.Fatalf("withdraw: %v", err)
		}
	}
	if len(ids) != 4 || ids[2] == ids[3] || ids[2] == "" {
		t.Fatalf("without --id every run must mint its own: %v", ids)
	}
}

// TestOnlyTheOutputPolicyReadsTheOutputFlags: "--json and --quiet mean the same thing on every
// command" is a promise about the whole surface, and it holds only while one function decides it.
// Each command that branched on the flags itself was a command that had drifted — one printing
// prose under --json, another ignoring --quiet — so a new branch anywhere else is the defect
// returning, not a detail.
func TestOnlyTheOutputPolicyReadsTheOutputFlags(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "main.go" { // main.go declares them
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "flagJSON") && !strings.Contains(line, "flagQuiet") {
				continue
			}
			if f == "cmd.go" {
				continue // the policy itself
			}
			t.Errorf("%s:%d decides its own output instead of leaving it to emit:\n%s", f, i+1, line)
		}
	}
}

// TestAListOfResourcesReadsLikeOne: a reply of several rows is several resources, so it is shown
// as resources — money included. A command whose reply happens to be a list (updating a whole
// path of actions, listing withdrawals) was printing its amounts in base units while the same
// reply for one row printed them in the world's unit.
func TestAListOfResourcesReadsLikeOne(t *testing.T) {
	net := kernel.Network{Name: "play", Decimals: 6, Symbol: "credits"}
	got := captureStdout(t, func() error {
		return printFields([]byte(`[{"id":"a-1","price":1500000},{"id":"a-2","price":2000000}]`), moneyAction, net)
	})
	if !strings.Contains(got, "price: 1.50 credits") || !strings.Contains(got, "price: 2.00 credits") {
		t.Errorf("a list's money was not written in the world's unit:\n%s", got)
	}
	if !strings.Contains(got, "id: a-1") || !strings.Contains(got, "id: a-2") {
		t.Errorf("a list lost its rows:\n%s", got)
	}
	// Something that is not resources at all still prints as it arrived.
	plain := captureStdout(t, func() error { return printFields([]byte(`[1,2,3]`), moneyAction, net) })
	if !strings.Contains(plain, "1,") && !strings.Contains(plain, "1\n") {
		t.Errorf("a plain list was not printed: %q", plain)
	}
}

// TestQuietFindsTheResourcesInsideAReplyThatWrapsThem: a peer answers with its page of steps beside
// whether more are waiting, so the ids are one field in. The command names that field rather than
// having every reply searched for something id-shaped.
func TestQuietFindsTheResourcesInsideAReplyThatWrapsThem(t *testing.T) {
	old := flagQuiet
	flagQuiet = true
	t.Cleanup(func() { flagQuiet = old })
	body := []byte(`{"steps":[{"id":"s-1"},{"id":"s-2"}],"truncated":false}`)
	if got := captureStdout(t, func() error { return emit(body, output{rows: "steps"}) }); got != "s-1\ns-2\n" {
		t.Errorf("--quiet = %q, want the two step ids", got)
	}
	// Unnamed, the same reply names no resource of its own and prints nothing.
	if got := captureStdout(t, func() error { return emit(body, output{}) }); got != "" {
		t.Errorf("--quiet went looking for ids: %q", got)
	}
}

// TestOnlyAHumanViewCostsAHealthRead: --json and --quiet carry base units, so a read that prints no
// money for a person must not turn a working reply into a failed /health. A write that shows what
// it moved still reads the unit before it acts.
func TestOnlyAHumanViewCostsAHealthRead(t *testing.T) {
	var banners int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			banners++
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "a-1", "action": "bob/echo", "price": 1500000}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("JUICE_HOME", t.TempDir())
	resetHealthCache()
	t.Cleanup(resetHealthCache)
	old := flagServer
	flagServer = srv.URL
	t.Cleanup(func() { flagServer = old })
	selectTestLogin(t, "tester@stub", srv.URL)

	for _, f := range []*bool{&flagJSON, &flagQuiet} {
		resetHealthCache()
		banners = 0
		oldFlag := *f
		*f = true
		_, err := execTestCmd(t, actionListCmd())
		*f = oldFlag
		if err != nil {
			t.Errorf("a read that prints no money was refused because the banner was: %v", err)
		}
		if banners != 0 {
			t.Errorf("the banner was read %d times for output that carries base units", banners)
		}
	}
}

// TestARunThatAuthorizesOnTheWayStillAnswersOnce: a run that meets a consent it can settle inline
// does two things and answers with one — its own reply. The connection is a step on the way, so it
// is reported as progress; anything else leaves --json printing two documents where a program
// expects one, and nothing downstream can read it (§14 C8).
func TestARunThatAuthorizesOnTheWayStillAnswersOnce(t *testing.T) {
	runs := 0
	stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Query().Get("ref") != "":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "act-1", "action": "bob/echo"}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/actions/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "act-1", "quote_hash": "h", "price": 1})
		case r.URL.Path == "/v1/run":
			runs++
			if runs == 1 {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "authorization required", "code": "grant_required",
					"meta": map[string]string{"action": "bob/echo"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tx_id": "t-1", "result": map[string]any{"ok": true}})
		case strings.HasPrefix(r.URL.Path, "/v1/grants/plan"):
			_ = json.NewEncoder(w).Encode(kernel.ConsentPlan{Groups: []kernel.ConsentGroup{{
				Provider: "chat", ProviderKey: "pk", Scheme: kernel.AuthSchemeDelegatedBearer,
				Connected: true, Covered: true,
				Actions: []kernel.ConsentAction{{Action: "bob/echo"}},
			}}})
		default: // POST /v1/grants
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "granted", "actions": []string{"bob/echo"}})
		}
	})
	// Somebody is at the terminal and says yes, which is what makes the run settle the consent
	// itself rather than printing the command to run.
	oldTTY := interactiveTTY
	interactiveTTY = func() bool { return true }
	t.Cleanup(func() { interactiveTTY = oldTTY })
	in, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Two answers: the price this run pins, then the authorization it turns out to need.
	if _, err := w.WriteString("y\ny\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = in
	t.Cleanup(func() { os.Stdin = oldStdin })

	oldJSON := flagJSON
	flagJSON = true
	t.Cleanup(func() { flagJSON = oldJSON })
	out := captureStdout(t, func() error { _, err := execTestCmd(t, runCmd(), "bob/echo"); return err })

	dec := json.NewDecoder(strings.NewReader(out))
	var reply map[string]any
	if err := dec.Decode(&reply); err != nil {
		t.Fatalf("--json did not print JSON: %v\n%s", err, out)
	}
	if reply["tx_id"] != "t-1" {
		t.Errorf("the one document is not the run's reply: %v", reply)
	}
	if err := dec.Decode(&reply); err == nil {
		t.Errorf("a second document followed the reply; stdout carries one:\n%s", out)
	}
	if runs != 2 {
		t.Errorf("the run was not retried after the consent: %d calls", runs)
	}
}

// TestAVerbReadsOrWrites: a word means one thing. `user address` registers and `user withdraw`
// pays; what each of them used to show with no argument is a verb of its own, so no command turns
// from a read into a write because an argument appeared.
func TestAVerbReadsOrWrites(t *testing.T) {
	var paths []string
	stubKernel(t, 6, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/withdrawals") && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "w-1", "amount": 5000000}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u", "rail_address": "0xabc", "amount": 1000000})
	})
	for _, c := range []struct {
		name string
		cmd  func() *cobra.Command
		args []string
	}{
		{"showing an address is not this verb's job", userAddressCmd, nil},
		{"withdrawing needs the amount it moves", userWithdrawCmd, nil},
		{"listing withdrawals takes no amount", userWithdrawalsCmd, []string{"5"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := execTestCmd(t, c.cmd(), c.args...); err == nil {
				t.Error("the command accepted an argument list it has no meaning for")
			}
		})
	}
	paths = nil
	if _, err := execTestCmd(t, userWithdrawalsCmd()); err != nil {
		t.Fatalf("withdrawals: %v", err)
	}
	if len(paths) != 1 || !strings.HasPrefix(paths[0], "GET /v1/withdrawals") {
		t.Errorf("withdrawals read %v, want one GET of the list", paths)
	}
}

// TestAdminRoutesNameTheirNoun: the route says which namespace the target belongs to, so the
// server never has to guess and a parameter never has to carry it. A user route answers for users
// alone, and nothing answers under the old unversioned prefix.
func TestAdminRoutesNameTheirNoun(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "carol", Password: "password123"}); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := env.k.BindPetname(ctx, key, "shop", true); err != nil {
		t.Fatal(err)
	}
	sys, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: kernel.SuperuserHandle, Password: "sys-pass"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := loginTokenFor(env.k, ctx, sys.Handle, "sys-pass")
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, method, path string
		want               int
	}{
		{"a user under users", "POST", "/v1/admin/users/carol/suspend", http.StatusOK},
		{"a peer under users", "POST", "/v1/admin/users/shop/suspend", http.StatusNotFound},
		{"a peer under peers", "POST", "/v1/admin/peers/shop/suspend", http.StatusOK},
		{"a user under peers", "POST", "/v1/admin/peers/carol/suspend", http.StatusNotFound},
		{"the old prefix answers nothing", "POST", "/control/users/carol/suspend", http.StatusNotFound},
		{"nor does the old deposit", "POST", "/control/deposit", http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			body, status := tcpDo(t, tok, c.method, c.path, map[string]any{})
			if status != c.want {
				t.Errorf("%s %s: status %d, want %d: %s", c.method, c.path, status, c.want, body)
			}
		})
	}
	// What a verb changed is what it answers with, so an acknowledgement is worth reading and a
	// caller needs no second request to see the result (API.md R5).
	body, status := tcpDo(t, tok, "POST", "/v1/admin/users/carol/unsuspend", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("unsuspend: %d %s", status, body)
	}
	shown, _ := tcpDo(t, tok, "GET", "/v1/admin/users/carol", nil)
	if string(body) != string(shown) {
		t.Errorf("a change answers with something other than the account:\n changed %s\n shown   %s", body, shown)
	}
}

// TestWithdrawalsPageByOffset: a list is paginated, so the rows past the first page are reachable.
// The handler read the limit and dropped the offset, which left older withdrawals unreachable.
func TestWithdrawalsPageByOffset(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	u, tok := makeUser(t, env.k, "payer")
	if err := env.db.SetRailAddress(ctx, u, "0xpayer", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	sys, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: kernel.SuperuserHandle, Password: "sys-pass"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.k.Deposit(ctx, sys.ID, u, 1000, "", newRef()); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		row, err := env.k.Withdraw(ctx, u, uuid.NewString(), 100, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.ID)
	}
	page := func(query string) []map[string]any {
		body, status := tcpDo(t, tok, "GET", "/v1/withdrawals"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("withdrawals%s: %d %s", query, status, body)
		}
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if all := page(""); len(all) != 3 {
		t.Fatalf("three withdrawals, got %d", len(all))
	}
	second := page("?limit=1&offset=1")
	if len(second) != 1 || second[0]["id"] != ids[1] {
		t.Errorf("the second page is not the second row: %v", second)
	}
	if last := page("?limit=1&offset=2"); len(last) != 1 || last[0]["id"] != ids[2] {
		t.Errorf("the last row is unreachable by paging: %v", last)
	}
}

// TestOnlyTheClientResolvesTheIdentity: one client per invocation is a promise the compiler cannot
// keep for us — Go has no visibility boundary inside a package — so it is kept here. A command that
// worked out for itself whom it acts as is how the identity a request carried, the account a prompt
// named, and the login a retry refreshed came to disagree.
//
// Two rules, because two things are being kept apart. No command may decide the selection: that is
// the client's, from the flags and the records, once. And no command but the ones whose subject IS
// the client's records — `kernel add|list|forget`, `auth login|use|list|logout` — may read or dial
// anything itself; those legitimately edit the records and dial an address before any identity
// exists. The server's own outbound HTTP (action execution, OpenAPI, sys/web) is not the client's
// and is not in scope.
func TestOnlyTheClientResolvesTheIdentity(t *testing.T) {
	selection := []string{"JUICE_AS", "flagServer", "flagAs"}
	records := []string{"loadClientConfig(", "readCredentials(", "doHTTP(", "http.Get(", "http.Post("}
	for _, c := range []struct {
		file      string
		forbidden []string
	}{
		{"cmd.go", append(selection, records...)},
		{"cmd_superuser.go", append(selection, records...)},
		{"connect.go", append(selection, records...)},
		{"cmd_client.go", selection}, // the commands over the records, which they may read
		{"main.go", records},         // where the flags are declared, and nothing else
	} {
		src, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, forbidden := range c.forbidden {
				if strings.Contains(line, forbidden) {
					t.Errorf("%s:%d resolves its own identity or transport (%s):\n%s", c.file, i+1, forbidden, line)
				}
			}
		}
	}
}

// depositKernel serves the two facts `user deposit` composes its answer from: the identity banner
// and the caller's own account. A chain world is the interesting case, so the banner carries an
// address, and the token is a parameter because an older kernel does not publish one.
func depositKernel(t *testing.T, network, kernelAddr, token, mine string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "handle": "bank", "public_key": "bank-key",
				"network": network, "decimals": 6, "symbol": "USDT",
				"token": token, "rail_address": kernelAddr,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "u-1", "handle": "alice", "available": 0, "locked": 0, "rail_address": mine})
	}))
	t.Cleanup(srv.Close)
	old := flagServer
	flagServer = srv.URL
	t.Cleanup(func() { flagServer = old })
	t.Setenv("JUICE_HOME", t.TempDir())
	selectTestLogin(t, "alice@bank", srv.URL)
	resetClient() // one client per invocation, as rootCmd's PersistentPreRun gives a real command
}

// A symbol names no token: one chain carries several stablecoins with one name and six decimals,
// and a payment in the wrong one is never credited. The contract is therefore what the command
// must print, and where the kernel does not publish one it must say so rather than leave a blank
// line under an instruction to send money.
func TestDepositNamesTheTokenItTakes(t *testing.T) {
	const (
		vault = "0x1111111111111111111111111111111111111111"
		token = "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9"
		mine  = "0x2222222222222222222222222222222222222222"
	)
	run := func(t *testing.T) string {
		t.Helper()
		return captureStdout(t, func() error {
			cmd := userDepositCmd()
			cmd.SetArgs(nil)
			return cmd.RunE(cmd, nil)
		})
	}

	t.Run("the contract is printed with the symbol beside it", func(t *testing.T) {
		depositKernel(t, "real", vault, token, mine)
		got := run(t)
		if !strings.Contains(got, token) {
			t.Errorf("the token contract is not named, so the depositor cannot tell which money to send:\n%s", got)
		}
		if !strings.Contains(got, "USDT") {
			t.Errorf("the symbol is not shown beside the contract:\n%s", got)
		}
		if !strings.Contains(got, vault) {
			t.Errorf("the kernel's address is missing:\n%s", got)
		}
	})

	t.Run("an unregistered sender is held, not credited to whoever sent it", func(t *testing.T) {
		depositKernel(t, "real", vault, token, mine)
		got := run(t)
		if !strings.Contains(got, "held") {
			t.Errorf("the command does not say an unattributed payment is held for the operator:\n%s", got)
		}
		if strings.Contains(got, "crediting itself") {
			t.Errorf("the command still claims an exchange would be credited:\n%s", got)
		}
	})

	t.Run("a kernel that publishes no token says so", func(t *testing.T) {
		depositKernel(t, "real", vault, "", mine)
		got := run(t)
		if !strings.Contains(got, "Ask the operator") {
			t.Errorf("an absent contract must be named, not left blank:\n%s", got)
		}
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "  ") && strings.TrimSpace(line) == "" {
				t.Errorf("an empty indented line reads as a contract that is not there:\n%q", got)
			}
		}
	})

	t.Run("play has nothing to send", func(t *testing.T) {
		depositKernel(t, "play", "", "", "")
		got := run(t)
		if !strings.Contains(got, "no addresses to send to") {
			t.Errorf("play must still explain that the operator records payments:\n%s", got)
		}
		if strings.Contains(got, "token") {
			t.Errorf("play names no token:\n%s", got)
		}
	})
}

// The token has to survive the trip: the kernel puts it in its banner, and the client's own view of
// the network is what every command reads. A field that decodes but is dropped here is invisible.
func TestClientNetworkCarriesTheToken(t *testing.T) {
	const token = "0x8e87deee3bf1efe27e8e96abf205bedf802ed568"
	depositKernel(t, "test", "0x3333333333333333333333333333333333333333", token, "")
	net, err := freshClient().network(context.Background())
	if err != nil {
		t.Fatalf("read network: %v", err)
	}
	if net.Token != token {
		t.Errorf("the token did not reach the client: %+v", net)
	}
	if net.Symbol != "USDT" || net.Decimals != 6 {
		t.Errorf("the money's shape did not survive with it: %+v", net)
	}
}

// TestAsIsRefusedWhereItMeansNothing: `--as` names who a command acts as, so the commands that act
// as nobody — this client's own address book and its logins — refuse it rather than accept it and
// ignore it. Every command in the tree is asked, so a command added later is covered by the rule
// rather than by a list somebody has to remember to extend (§14).
func TestAsIsRefusedWhereItMeansNothing(t *testing.T) {
	clientSide := map[string]bool{
		"juice kernel": true, "juice kernel add": true, "juice kernel list": true,
		"juice kernel health": true, "juice kernel forget": true, "juice kernel serve": true,
		"juice auth": true, "juice auth login": true, "juice auth use": true,
		"juice auth list": true, "juice auth logout": true, "juice auth recover": true,
		"juice user create": true,
	}
	old := flagAs
	flagAs = "someone@somewhere"
	t.Cleanup(func() { flagAs = old })

	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			walk(child)
		}
		if c.Name() == "help" || c.Name() == "completion" {
			return
		}
		err := checkGlobalFlags(c)
		refused := err != nil
		if refused != clientSide[c.CommandPath()] {
			if refused {
				t.Errorf("%s refuses --as, but it acts as a login", c.CommandPath())
			} else {
				t.Errorf("%s accepts --as, but it acts on this client's own records", c.CommandPath())
			}
		}
	}
	walk(rootCmd)
}

// A run spends money, so at a terminal the person is asked before it does, at the price this run
// pins — and says no by saying nothing. Off a terminal the price is stated and the run proceeds:
// a script has nobody to ask, and the pin is what guarantees the price it was quoted (U8, D20).
func TestARunAsksBeforeItSpendsAtATerminal(t *testing.T) {
	runs := 0
	stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Query().Get("ref") != "":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "act-1", "action": "bob/echo"}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/actions/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "act-1", "quote_hash": "h", "price": 1})
		default:
			runs++
			_ = json.NewEncoder(w).Encode(map[string]any{"tx_id": "t-1", "result": map[string]any{"ok": true}})
		}
	})
	answer := func(t *testing.T, text string) {
		t.Helper()
		in, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteString(text); err != nil {
			t.Fatal(err)
		}
		w.Close()
		old := os.Stdin
		os.Stdin = in
		t.Cleanup(func() { os.Stdin = old })
	}
	atTerminal := func(t *testing.T, yes bool) {
		t.Helper()
		old := interactiveTTY
		interactiveTTY = func() bool { return yes }
		t.Cleanup(func() { interactiveTTY = old })
	}

	t.Run("declined at a terminal spends nothing", func(t *testing.T) {
		runs = 0
		atTerminal(t, true)
		answer(t, "n\n")
		if _, err := execTestCmd(t, runCmd(), "bob/echo"); err == nil {
			t.Error("a declined run reported success")
		}
		if runs != 0 {
			t.Errorf("the call was made %d times after the person said no", runs)
		}
	})
	t.Run("accepted at a terminal runs", func(t *testing.T) {
		runs = 0
		atTerminal(t, true)
		answer(t, "y\n")
		if _, err := execTestCmd(t, runCmd(), "bob/echo"); err != nil {
			t.Fatalf("an accepted run failed: %v", err)
		}
		if runs != 1 {
			t.Errorf("the call ran %d times, want once", runs)
		}
	})
	t.Run("a script is told the price and proceeds", func(t *testing.T) {
		runs = 0
		atTerminal(t, false)
		if _, err := execTestCmd(t, runCmd(), "bob/echo"); err != nil {
			t.Fatalf("an unattended run failed: %v", err)
		}
		if runs != 1 {
			t.Errorf("the call ran %d times, want once", runs)
		}
	})
	t.Run("--yes skips the question", func(t *testing.T) {
		runs = 0
		atTerminal(t, true)
		answer(t, "") // nothing to read: the flag is the answer
		if _, err := execTestCmd(t, runCmd(), "bob/echo", "--yes"); err != nil {
			t.Fatalf("--yes did not skip the prompt: %v", err)
		}
		if runs != 1 {
			t.Errorf("the call ran %d times, want once", runs)
		}
	})
}
