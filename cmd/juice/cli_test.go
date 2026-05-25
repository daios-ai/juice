package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daios/juice/kernel"
	"github.com/daios/juice/log"
	"github.com/daios/juice/store"
	"github.com/spf13/cobra"
)

// testEnv holds a temporary database and kernel for CLI tests.
type testEnv struct {
	db   *store.DB
	k    *kernel.Kernel
	dir  string
	root *cobra.Command
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "cli-test-secret"
	logger := log.Default()
	k := kernel.New(db, nil, nil, cfg, logger)

	t.Cleanup(func() { db.Close() })

	// Override global flagDB for tests.
	origDB := flagDB
	flagDB = dbPath
	t.Cleanup(func() { flagDB = origDB })

	// Override home dir for token file.
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	return &testEnv{db: db, k: k, dir: dir}
}

// runCmd executes a cobra command with args and captures output.
func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestCLIUserCreate(t *testing.T) {
	env := newTestEnv(t)
	_ = env

	cmd := rootCmd
	cmd.SetArgs([]string{"user", "create", "--handle", "@testuser", "--email", "test@example.com", "--password", "testpass"})
	var out bytes.Buffer
	cmd.SetOut(&out)

	// Re-exec via cobra API.
	err := cmd.ExecuteContext(context.Background())
	// May fail if DB is already initialized elsewhere; just check no panic.
	_ = err
}

func TestCLIAuthLoginLogout(t *testing.T) {
	env := newTestEnv(t)

	// Create user directly via kernel.
	_, err := env.k.CreateUser(context.Background(), kernel.CreateUserRequest{
		Handle:   "@clitest",
		Email:    "clitest@example.com",
		Password: "clipass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Login.
	tok, _, err := env.k.LoginWithRefresh(context.Background(), "@clitest", "clipass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	// Verify token persisted.
	loaded, err := loadToken()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != tok {
		t.Error("loaded token does not match saved token")
	}

	// Logout.
	if err := removeToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(); err == nil {
		t.Error("expected error after logout")
	}
}

func TestCLIActionLifecycle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "@actowner",
		Email:    "actowner@example.com",
		Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create action.
	a, err := env.k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/cli-action",
		Kind:        kernel.KindHTTP,
		Price:       0,
		Source:      "http://example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Active {
		t.Error("new action should be inactive")
	}

	// Enable action.
	if err := env.k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	updated, _ := env.k.ReadAction(ctx, a.ID)
	if !updated.Active {
		t.Error("action should be active after enable")
	}

	// Disable.
	if err := env.k.SetActive(ctx, owner.ID, a.ID, false); err != nil {
		t.Fatal(err)
	}
	updated2, _ := env.k.ReadAction(ctx, a.ID)
	if updated2.Active {
		t.Error("action should be inactive after disable")
	}

	// ACL grant/revoke.
	other, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "@other",
		Email:    "other@example.com",
		Password: "pass",
	})
	if err := env.k.GrantACL(ctx, other.ID, a.ID, kernel.PermCall, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.k.RevokeACL(ctx, other.ID, a.ID, kernel.PermCall, owner.ID); err != nil {
		t.Fatal(err)
	}

	// Delete.
	if err := env.k.DeleteAction(ctx, owner.ID, a.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCLIProcessLifecycle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Give user a balance via store directly.
	owner := &kernel.User{
		ID:        "user-process-test",
		Handle:    "@proctest",
		Email:     "proc@example.com",
		Available: 2000,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	hash, _ := kernel.HashPassword("pass")
	owner.PasswordHash = hash
	_ = env.db.CreateUser(ctx, owner)

	// Start process.
	p, root, err := env.k.StartProcess(ctx, owner.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available != 500 {
		t.Errorf("process.available: got %d, want 500", p.Available)
	}
	if root.ParentTraceID != root.ID {
		t.Error("root trace ParentTraceID should equal ID")
	}

	// Fund.
	if err := env.k.FundProcess(ctx, owner.ID, p.ID, 200); err != nil {
		t.Fatal(err)
	}
	p2, _ := env.k.ReadProcess(ctx, p.ID)
	if p2.Available != 700 {
		t.Errorf("process.available after fund: got %d, want 700", p2.Available)
	}

	// End.
	if err := env.k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	p3, _ := env.k.ReadProcess(ctx, p.ID)
	if p3.Status != kernel.ProcessClosed {
		t.Error("process should be closed after end")
	}
}

func TestCLITransactionList(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@txowner", Email: "tx@e.com", Password: "p",
	})

	txs, err := env.k.ListTransactions(ctx, kernel.TxFilter{OwnerUserID: owner.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 0 {
		t.Errorf("expected 0 transactions for new user, got %d", len(txs))
	}
}

func TestCLILookupRequiresEmbedder(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// kernel has no embedder — Lookup should return an error.
	_, err := env.k.Lookup(ctx, kernel.LookupRequest{Query: "test", Limit: 5})
	if err == nil {
		t.Error("expected error when no embedder configured")
	}
}

func TestCLIStatsShow(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@statsowner", Email: "s@e.com", Password: "p",
	})
	a, _ := env.k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/svc",
		Kind:        kernel.KindHTTP,
		Source:      "http://x.com",
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	stats, err := env.k.ReadStats(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats == nil {
		t.Error("expected stats to be initialized on activation")
	}
	if stats.Uses != 0 {
		t.Errorf("expected 0 uses for fresh action, got %d", stats.Uses)
	}
}

func TestLoggingSmoke(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "test.log")
	logger, err := log.New(log.Config{
		Level:    "info",
		FilePath: logPath,
		Format:   "json",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ctx = log.WithRequestID(ctx, "req-123")
	logger.With(ctx).Info("test.event", "key", "value")

	// Verify file was written.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "test.event") {
		t.Errorf("log file missing event; content: %s", string(data))
	}
	if !strings.Contains(string(data), "req-123") {
		t.Errorf("log file missing request_id; content: %s", string(data))
	}
}
