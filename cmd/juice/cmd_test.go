package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/script"
	"github.com/daios-ai/juice/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// ---- CLI test helpers ----

type testEnv struct {
	db  *store.DB
	k   *kernel.Kernel
	dir string
}

const cmdTestIssuerID = "00000000-0000-0000-0000-000000000001"

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Seed the issuer user so receipt FK constraints pass and buildReceipt can sign.
	hash, _ := kernel.HashPassword("issuer-pass")
	issuer := &kernel.User{
		ID: cmdTestIssuerID, Handle: "@_test_issuer", Email: "issuer@test.internal",
		PasswordHash: hash, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.CreateUser(context.Background(), issuer); err != nil {
		t.Fatalf("newTestEnv: seed issuer: %v", err)
	}

	_, signingKey, _ := ed25519.GenerateKey(rand.Reader)

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "cli-test-secret"
	cfg.IssuerUserID = cmdTestIssuerID
	cfg.FeeRecipientID = cmdTestIssuerID
	cfg.SigningKey = signingKey
	cfg.AllowLocalSources = true // CLI-command tests import specs from loopback httptest servers
	// Credential encryption is mandatory (§8): the production binary always wires a box,
	// so tests do too. Without it, creating/activating an action with upstream auth fails closed.
	box, _ := newAESGCMBox(make([]byte, 32))
	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, auth: newAuthenticator(box, db, true, cfg.ScriptTimeout), allowLocal: true}
	exec := script.New(script.Config{TimeoutMS: cfg.ScriptTimeout.Milliseconds(), MemoryBytes: cfg.ScriptMemory})
	k := kernel.New(db, exec, httpExec, nil, cfg, log.Discard())
	k.SetSecretBox(box)
	t.Setenv("JUICE_SECRET_KEY", "cli-test-secret")

	t.Cleanup(func() { db.Close() })

	origDB := flagDB
	flagDB = dbPath
	t.Cleanup(func() { flagDB = origDB })

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	// User-facing CLI commands are HTTP clients now: point them at a server backed by env.k.
	ts := mountTestServer(t, k)
	origServer := flagServer
	flagServer = ts.URL
	t.Cleanup(func() { flagServer = origServer })

	return &testEnv{db: db, k: k, dir: dir}
}

// mountTestServer starts an httptest server exposing the full route set backed by k, for
// tests that drive user-facing CLI commands (which are HTTP clients).
func mountTestServer(t *testing.T, k *kernel.Kernel) *httptest.Server {
	t.Helper()
	srv := &server{kernel: k, log: log.Discard()}
	r := chi.NewRouter()
	r.Post("/v1/auth/token", srv.postTokenMulti)
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
	_, err := execTestCmd(t, userCreateCmd(), "@newuser", "new@example.com")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("mismatch: want ErrInvalidInput, got %v", err)
	}
	if _, err := env.db.ReadUserByHandle(context.Background(), "@newuser"); !errors.Is(err, kernel.ErrNotFound) {
		t.Fatalf("no user should be created on mismatch, got %v", err)
	}
}

func TestUserCreate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "@testuser",
		Email:    "test@example.com",
		Password: "testpass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle != "@testuser" {
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
		Handle:   "@readtest",
		Email:    "read@example.com",
		Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	u, err := env.k.ReadUserByHandle(ctx, "@readtest")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "read@example.com" {
		t.Errorf("email: got %q, want read@example.com", u.Email)
	}
}

func TestUserDuplicateHandleFails(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	req := kernel.CreateUserRequest{Handle: "@dup", Email: "a@b.com", Password: "p"}
	if _, err := env.k.CreateUser(ctx, req); err != nil {
		t.Fatal(err)
	}
	req.Email = "c@d.com"
	if _, err := env.k.CreateUser(ctx, req); err == nil {
		t.Error("expected error for duplicate handle")
	}
}

func TestUserMe(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@meuser", Email: "me@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := env.k.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Handle != "@meuser" {
		t.Errorf("handle: got %q, want @meuser", got.Handle)
	}
	if got.Email != "me@example.com" {
		t.Errorf("email: got %q, want me@example.com", got.Email)
	}
}

func TestUserUpdateEmail(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@updemail", Email: "old@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := env.k.UpdateUser(ctx, u.ID, kernel.UpdateUserRequest{Email: "new@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "new@example.com" {
		t.Errorf("email after update: got %q, want new@example.com", got.Email)
	}

	stored, err := env.k.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Email != "new@example.com" {
		t.Errorf("stored email: got %q, want new@example.com", stored.Email)
	}
}

func TestUserUpdatePassword(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@updpass", Email: "updpass@example.com", Password: "oldpass",
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

	if _, _, err := env.k.LoginWithRefresh(ctx, "@updpass", "oldpass"); err == nil {
		t.Error("old password should be rejected after change")
	}
	if _, _, err := env.k.LoginWithRefresh(ctx, "@updpass", "newpass"); err != nil {
		t.Errorf("new password should work: %v", err)
	}
}

func TestUserUpdatePasswordWrongCurrent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	u, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@wrongpass", Email: "wrongpass@example.com", Password: "correct",
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
		Handle: "@nofields", Email: "nofields@example.com", Password: "pass",
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

	proxy := &kernel.User{
		ID:        "proxy-id-1",
		Handle:    "@remote-peer",
		Email:     "@remote-peer@remote",
		PublicKey: "dGVzdGtleQ==",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := env.db.CreateUser(ctx, proxy); err != nil {
		t.Fatal(err)
	}

	_, err := env.k.UpdateUser(ctx, proxy.ID, kernel.UpdateUserRequest{Email: "x@x.com"})
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
		Handle: "@actowner", Email: "actowner@example.com", Password: "pass",
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
		Handle: "@priceowner", Email: "price@example.com", Password: "pass",
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
		Handle: "@delowner", Email: "del@example.com", Password: "pass",
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
		Handle: "@show-owner", Email: "show-owner@example.com", Password: "pass",
	})
	stranger, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@show-stranger", Email: "show-stranger@example.com", Password: "pass",
	})
	_ = stranger

	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "show-svc",
		Kind: kernel.KindHTTP, Source: "http://example.com",
	})

	ownerTok, _ := env.k.Login(ctx, "@show-owner", "pass")
	if err := saveToken(ownerTok); err != nil {
		t.Fatal(err)
	}
	if _, err := execTestCmd(t, actionShowCmd(), a.ID); err != nil {
		t.Errorf("owner: unexpected error: %v", err)
	}

	strangerTok, _ := env.k.Login(ctx, "@show-stranger", "pass")
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
		Handle: "@authowner", Email: "authowner@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := env.k.Login(ctx, "@authowner", "pass")
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
		Handle: "@wasmowner", Email: "wasmowner@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := env.k.Login(ctx, "@wasmowner", "pass")
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
		Handle: "@httpowner", Email: "httpowner@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := env.k.Login(ctx, "@httpowner", "pass")
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
		Handle: "@cli-import-owner", Email: "cliimport@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := env.k.Login(context.Background(), "@cli-import-owner", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	if _, err := execTestCmd(t, actionImportCmd(), specSrv.URL+"/spec.json"); err != nil {
		t.Fatalf("action import: %v", err)
	}

	actions, err := env.k.ListAllActions(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range actions {
		if a.Name == "sayHello" {
			found = true
		}
	}
	if !found {
		t.Error("expected sayHello in actions after import")
	}
}

func TestActionUnimportOpenAPI(t *testing.T) {
	const spec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	specSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(spec))
	}))
	defer specSrv.Close()

	env := newTestEnv(t)
	t.Setenv("JUICE_ALLOW_LOCAL_SOURCES", "true")

	_, err := env.k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle: "@cli-unimport-owner", Email: "cliunimport@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := env.k.Login(context.Background(), "@cli-unimport-owner", "pass")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	specURL := specSrv.URL + "/spec.json"
	if _, err := execTestCmd(t, actionImportCmd(), specURL); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := execTestCmd(t, actionUnimportCmd(), specURL); err != nil {
		t.Fatalf("unimport: %v", err)
	}
}

func TestActionListActive(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@listowner", Email: "list@example.com", Password: "pass",
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
	pub := true
	_, _ = env.k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pub})

	actions, err := env.k.ListPublicActions(ctx, 10, 0)
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
		Handle: "@statsowner", Email: "s@e.com", Password: "p",
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
	if err := env.db.BeginRun(ctx, p, tr, ownerID, funds); err != nil {
		t.Fatalf("setupProcessCmd: %v", err)
	}
	return p, tr
}

func TestProcessStartFundEnd(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.User{
		ID:        "user-proc-test",
		Handle:    "@proctest",
		Email:     "proc@example.com",
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
		Handle: "@list-proc", Email: "lp@example.com", Password: "p",
	})
	other, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@list-proc-other", Email: "lpo@example.com", Password: "p",
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
		Handle: "@negfund", Email: "nf@example.com", Password: "p",
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
	err := env.db.BeginRun(ctx, p, tr, owner.ID, -1)
	if err == nil {
		t.Error("expected error creating process with negative funds")
	}
}

func TestProcessEndReturnsBalance(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner := &kernel.User{
		ID:        "balance-return-user",
		Handle:    "@baltest",
		Email:     "bal@example.com",
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
	if strings.HasPrefix(s, "@@") {
		t.Errorf("action field has double @: %q", s)
	}
	if !strings.HasPrefix(s, "@") {
		t.Errorf("action field must start with @, got %q", s)
	}
	if strings.Count(s, "/") != 1 {
		t.Errorf("action field must contain exactly one /, got %q", s)
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
	er := httpDo(t, srv, "POST", "/v1/actions/"+id+"/enable", nil, ownerTok)
	er.Body.Close()
	if er.StatusCode != http.StatusOK {
		t.Fatalf("enable action %s: expected 200, got %d", name, er.StatusCode)
	}
	httpDo(t, srv, "PUT", "/v1/actions/"+id, map[string]any{"public": true}, ownerTok).Body.Close()
	return id, handle + "/" + name
}

func TestServeCreateStep(t *testing.T) {
	backend := newStepBackend(t)
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@cs-create-owner")
	makeUser(t, k, "@cs-create-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@cs-create-owner", "cs-create-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	resp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id": traceID,
		"action_id": actionID,
		"required_caller": "@cs-create-caller",
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

	ownerID, ownerTok := makeUser(t, k, "@sl-steps-owner")
	makeUser(t, k, "@sl-steps-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@sl-steps-owner", "sl-steps-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	for range 2 {
		r := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
			"trace_id": traceID,
			"action_id": actionID,
			"required_caller": "@sl-steps-caller",
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
}

func TestServeGetStep(t *testing.T) {
	backend := newStepBackend(t)
	srv, k, db := newTestHTTPServerFull(t)
	defer srv.Close()

	ownerID, ownerTok := makeUser(t, k, "@gs-steps-owner")
	_, callerTok := makeUser(t, k, "@gs-steps-caller")
	_, unrelTok := makeUser(t, k, "@gs-steps-unrelated")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@gs-steps-owner", "gs-steps-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id": traceID,
		"action_id": actionID,
		"required_caller": "@gs-steps-caller",
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

	ownerID, ownerTok := makeUser(t, k, "@csmiss-owner")
	_, callerTok := makeUser(t, k, "@csmiss-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@csmiss-owner", "csmiss-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id": traceID,
		"action_id": actionID,
		"required_caller": "@csmiss-caller",
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

	ownerID, ownerTok := makeUser(t, k, "@cs2-owner")
	_, callerTok := makeUser(t, k, "@cs2-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@cs2-owner", "cs2-svc")

	p := setupProcessHTTP(t, db, ownerID, 0)
	pid := p.ID
	traceID := setupTraceForProcess(t, db, pid)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"trace_id": traceID,
		"action_id": actionID,
		"required_caller": "@cs2-caller",
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
// call, rather than the literal bytes "@file" being shipped as the step input.
func TestStepCompleteFileArg(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if _, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@sc-caller", Email: "sc-caller@example.com", Password: "pass",
	}); err != nil {
		t.Fatal(err)
	}
	tok, _ := env.k.Login(ctx, "@sc-caller", "pass")
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
		Handle: "@txowner", Email: "tx@e.com", Password: "p",
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
		Handle: "@rateowner", Email: "ro@example.com", Password: "p",
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
		Handle: "@rater", Email: "r@e.com", Password: "p",
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

	owner := &kernel.User{
		ID:        uuid.New().String(),
		Handle:    "@call-owner",
		Email:     "co@example.com",
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

	_, err := env.k.Call(ctx, kernel.CallRequest{
		CallerID:        owner.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    owner.ID,
		ActionName:      "echo",
		Args:            map[string]any{},
	})
	if err == nil {
		t.Error("expected error calling on closed process")
	}
}

func TestCallInsufficientFunds(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@poorowner", Email: "poor@example.com", Password: "p",
	})

	_, err := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "expensive",
		Kind: kernel.KindHTTP, Source: "http://x.com", Price: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := env.k.ReadActionByOwnerName(ctx, owner.ID, "expensive")
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	_, err = env.k.Run(ctx, owner.ID, "@poorowner/expensive", map[string]any{})
	if err == nil {
		t.Error("expected insufficient funds error")
	}
}

// ---- remote ----

func remoteTestPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(pub)
}

func newRemoteTestKernel(t *testing.T) (*kernel.Kernel, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "remote_test.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	t.Setenv("JUICE_SECRET_KEY", "remote-test-secret")

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "remote-test-secret"
	k := kernel.New(db, nil, nil, nil, cfg, log.Discard())

	if err := k.FirstBoot(t.Context(), "sys-pass"); err != nil {
		t.Fatal(err)
	}

	origDB := flagDB
	flagDB = dbPath
	t.Cleanup(func() { flagDB = origDB })

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
		OwnerHandle:  "@import-remote",
		Name:         "greet",
		Description:  "says hello",
		Kind:         kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef",
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
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

	sys, err := k.ReadUserByHandle(t.Context(), "@sys")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := k.AddPeer(t.Context(), sys.ID, "@import-remote", pubB64); err != nil {
		t.Fatal(err)
	}

	if _, err := k.ReconcileRemoteAction(t.Context(), sys.ID, "@import-remote", "greet", &m); err != nil {
		t.Fatalf("ReconcileRemoteAction: %v", err)
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

func TestRemoteImportDisappearedDeactivatesProxy(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	const actionID = "disappear-action-id"
	m := kernel.ActionManifest{
		ActionID:     actionID,
		OwnerHandle:  "@disappear-remote",
		Name:         "bye",
		Description:  "going away",
		Kind:         kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef",
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	serveAction := true
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/manifest") {
			json.NewEncoder(w).Encode(m)
			return
		}
		if serveAction {
			json.NewEncoder(w).Encode([]map[string]string{{"ID": actionID, "Name": "bye"}})
		} else {
			json.NewEncoder(w).Encode([]map[string]string{})
		}
	}))
	defer remote.Close()

	sys, err := k.ReadUserByHandle(t.Context(), "@sys")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.AddPeer(t.Context(), sys.ID, "@disappear-remote", pubB64); err != nil {
		t.Fatal(err)
	}

	r, err := k.ReconcileRemoteAction(t.Context(), sys.ID, "@disappear-remote", "bye", &m)
	if err != nil {
		t.Fatalf("initial import: %v", err)
	}
	if len(r.Created) > 0 {
		// Enable the proxy so the deactivation assertion below is meaningful.
		if err := k.SetActive(t.Context(), sys.ID, r.Created[0].ID, true); err != nil {
			t.Fatalf("SetActive: %v", err)
		}
	}

	serveAction = false // action gone from remote; passing nil manifest deactivates the proxy.
	// The local action is owner-qualified (disappear-remote/bye); deactivation is by that name.
	if _, err := k.ReconcileRemoteAction(t.Context(), sys.ID, "@disappear-remote", "disappear-remote/bye", nil); err != nil {
		t.Fatalf("reimport after disappearance: %v", err)
	}

	actions, err := k.ListAllActions(t.Context(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.Name == "disappear-remote/bye" && a.Active {
			t.Error("expected local proxy to be deactivated after remote action disappeared")
		}
	}
}

func TestRemoteUnimport(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	sys, err := k.ReadUserByHandle(t.Context(), "@sys")
	if err != nil {
		t.Fatal(err)
	}
	remoteUser, err := k.AddPeer(t.Context(), sys.ID, "@unimport-peer", pubB64)
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "unimport-action-id",
		OwnerHandle:  "@unimport-peer",
		Name:         "greet",
		Description:  "greet action",
		Kind:         kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef",
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	if _, err := k.ImportRemoteAction(t.Context(), sys.ID, remoteUser.ID, m); err != nil {
		t.Fatalf("ImportRemoteAction: %v", err)
	}

	if _, err := k.UnimportRemoteAction(t.Context(), sys.ID, "@unimport-peer", "unimport-peer/greet"); err != nil {
		t.Fatalf("UnimportRemoteAction: %v", err)
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

// TestPrintTextParity asserts that printText surfaces every field the canonical JSON
// (what the HTTP API returns) carries — the CLI/HTTP parity invariant (§14). It also
// checks that structured values are rendered as indented JSON.
func TestPrintTextParity(t *testing.T) {
	objects := []any{
		enrichAction(&kernel.Kernel{}, &kernel.Action{
			ID: "a1", OwnerUserID: "u1", OwnerHandle: "@alice", Name: "weather",
			Kind: kernel.KindHTTP, Active: true, Public: true, Price: 5,
			Description:  "current weather",
			InputSchema:  map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
		}),
		&kernel.TransactionView{Transaction: &kernel.Transaction{ID: "t1", Status: "success", Gross: 10, Net: 8, Fee: 2}},
		&stepWithAction{Step: &kernel.Step{ID: "s1", Status: "waiting"}, Action: "@alice/weather"},
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
		text := captureStdout(t, func() error { return printText(obj) })
		for key := range fields {
			if !strings.Contains(text, key+":") {
				t.Errorf("%T text output missing field %q\n%s", obj, key, text)
			}
		}
	}

	// Structured values must appear as indented JSON, not be dropped.
	text := captureStdout(t, func() error {
		return printText(enrichAction(&kernel.Kernel{}, &kernel.Action{
			ID: "a1", Name: "x", Kind: kernel.KindHTTP,
			InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
			OutputSchema: map[string]any{"type": "object"},
		}))
	})
	if !strings.Contains(text, "input_schema: {") || !strings.Contains(text, `"type": "object"`) {
		t.Errorf("input_schema not rendered as indented JSON:\n%s", text)
	}
}
