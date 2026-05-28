package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
)

func TestFirstBootAtomic(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	if err := k.FirstBoot(ctx, "secret"); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}

	// All three config entries must be present.
	for _, key := range []string{"superuser_handle", "signing_public_key", "signing_private_key"} {
		v, err := k.GetConfig(ctx, key)
		if err != nil || v == "" {
			t.Errorf("config %q missing after FirstBoot: %v", key, err)
		}
	}

	// @sys user must exist.
	u, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil || u == nil {
		t.Fatalf("@sys not found after FirstBoot: %v", err)
	}

	// Second call must be a no-op (idempotent).
	if err := k.FirstBoot(ctx, "secret"); err != nil {
		t.Errorf("second FirstBoot should be idempotent, got: %v", err)
	}
}

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
	return kernel.New(db, nil, nil, nil, nil, cfg, log.Discard())
}

func TestEnsureSysLookupIdempotent(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	// Create a superuser manually.
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "pass",
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
	a, err := k.ReadActionByOwnerName(ctx, u.ID, "/lookup")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Active {
		t.Fatal("/lookup should be active after ensureSysLookup")
	}

	// Second call: idempotent — must also enforce grant-all.
	if err := ensureSysLookup(ctx, k, u.Handle); err != nil {
		t.Fatalf("second ensureSysLookup: %v", err)
	}
	a, err = k.ReadActionByOwnerName(ctx, u.ID, "/lookup")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Public {
		t.Error("/lookup should be public after idempotent ensureSysLookup")
	}
}

func TestEnsureSysLLMChatIdempotent(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// First call: creates the action.
	if err := ensureSysLLMChat(ctx, k, u.Handle); err != nil {
		t.Fatalf("first ensureSysLLMChat: %v", err)
	}
	a, err := k.ReadActionByOwnerName(ctx, u.ID, "/llm/chat")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Active {
		t.Fatal("/llm/chat should be active after ensureSysLLMChat")
	}

	// Second call: idempotent — must also enforce grant-all.
	if err := ensureSysLLMChat(ctx, k, u.Handle); err != nil {
		t.Fatalf("second ensureSysLLMChat: %v", err)
	}
	a, err = k.ReadActionByOwnerName(ctx, u.ID, "/llm/chat")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Public {
		t.Error("/llm/chat should be public after idempotent ensureSysLLMChat")
	}
}

func TestBootstrapSuperuserAtomic(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	// BootstrapSuperuser must atomically create user + config.
	_, err := k.BootstrapSuperuser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "secret",
	}, configKeySuperuser)
	if err != nil {
		t.Fatalf("BootstrapSuperuser: %v", err)
	}

	// Config must be set.
	handle, err := k.GetConfig(ctx, configKeySuperuser)
	if err != nil || handle != "@sys" {
		t.Errorf("superuser config: got %q %v, want @sys nil", handle, err)
	}

	// User must be readable.
	u, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil || u == nil {
		t.Fatalf("superuser user not found: %v", err)
	}

	// Second call must be idempotent (handle already exists).
	_, err = k.BootstrapSuperuser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "secret",
	}, configKeySuperuser)
	if err != nil {
		t.Errorf("second BootstrapSuperuser: expected idempotent, got %v", err)
	}
}
