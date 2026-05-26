package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
)

func newTestKernel(t *testing.T) *kernel.Kernel {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "bootstrap_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "bootstrap-test-secret"
	return kernel.New(db, nil, nil, cfg, log.Discard())
}

func TestEnsureSysLookupIdempotent(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	// Create a superuser manually.
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@su", Email: "su@sys", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetConfig(ctx, configKeySuperuser, u.Handle); err != nil {
		t.Fatal(err)
	}

	// First call: creates the action.
	if err := ensureSysLookup(ctx, k, u.Handle); err != nil {
		t.Fatalf("first ensureSysLookup: %v", err)
	}

	// Second call: idempotent.
	if err := ensureSysLookup(ctx, k, u.Handle); err != nil {
		t.Fatalf("second ensureSysLookup: %v", err)
	}
}
