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

	// All config entries must be present.
	for _, key := range []string{"superuser_handle", "signing_public_key", "signing_private_key", "jwt_secret"} {
		v, err := k.GetConfig(ctx, key)
		if err != nil || v == "" {
			t.Errorf("config %q missing after FirstBoot: %v", key, err)
		}
	}

	// jwt_secret must be a 64-char hex string (32 bytes).
	jwtSecret, _ := k.GetConfig(ctx, "jwt_secret")
	if len(jwtSecret) != 64 {
		t.Errorf("jwt_secret length = %d, want 64", len(jwtSecret))
	}

	// @sys user must exist.
	u, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil || u == nil {
		t.Fatalf("@sys not found after FirstBoot: %v", err)
	}

	// Second call must be a no-op (idempotent) and preserve the same secret.
	if err := k.FirstBoot(ctx, "secret"); err != nil {
		t.Errorf("second FirstBoot should be idempotent, got: %v", err)
	}
	jwtSecret2, _ := k.GetConfig(ctx, "jwt_secret")
	if jwtSecret2 != jwtSecret {
		t.Error("jwt_secret changed across idempotent FirstBoot calls")
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
	return kernel.New(db, nil, nil, nil, cfg, log.Discard())
}

func sysSpec(name string) sysNativeSpec {
	for _, s := range buildSysNativeSpecs(DefaultServerConfig().Native) {
		if s.name == name {
			return s
		}
	}
	panic("sysNativeSpec not found: " + name)
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

	spec := sysSpec("lookup")

	// First call: creates the action.
	if err := ensureSysNative(ctx, k, u.Handle, spec); err != nil {
		t.Fatalf("first ensureSysNative(lookup): %v", err)
	}
	a, err := k.ReadActionByOwnerName(ctx, u.ID, "lookup")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Active {
		t.Fatal("lookup should be active after ensureSysNative")
	}

	// Second call: idempotent — must also enforce grant-all.
	if err := ensureSysNative(ctx, k, u.Handle, spec); err != nil {
		t.Fatalf("second ensureSysNative(lookup): %v", err)
	}
	a, err = k.ReadActionByOwnerName(ctx, u.ID, "lookup")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Public {
		t.Error("lookup should be public after idempotent ensureSysNative")
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

	spec := sysSpec("llm/chat")

	// First call: creates the action.
	if err := ensureSysNative(ctx, k, u.Handle, spec); err != nil {
		t.Fatalf("first ensureSysNative(llm/chat): %v", err)
	}
	a, err := k.ReadActionByOwnerName(ctx, u.ID, "llm/chat")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Active {
		t.Fatal("llm/chat should be active after ensureSysNative")
	}

	// Second call: idempotent — must also enforce grant-all.
	if err := ensureSysNative(ctx, k, u.Handle, spec); err != nil {
		t.Fatalf("second ensureSysNative(llm/chat): %v", err)
	}
	a, err = k.ReadActionByOwnerName(ctx, u.ID, "llm/chat")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Public {
		t.Error("llm/chat should be public after idempotent ensureSysNative")
	}
}

func TestBootstrapRejectsKeyMismatch(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	// Run first boot to generate a valid key pair.
	if err := k.FirstBoot(ctx, "pass"); err != nil {
		t.Fatal(err)
	}

	// Tamper: store a different public key (32 zero bytes, base64url).
	badPub := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := k.SetConfig(ctx, configKeySigningPublic, badPub); err != nil {
		t.Fatal(err)
	}

	// bootstrap must reject the mismatch.
	if err := bootstrap(k, DefaultServerConfig().Native); err == nil {
		t.Error("expected error for mismatched signing keys, got nil")
	}
}

func TestEnsureSysMakeIdempotent(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetConfig(ctx, configKeySuperuser, u.Handle); err != nil {
		t.Fatal(err)
	}

	spec := sysSpec("make")
	if err := ensureSysNative(ctx, k, u.Handle, spec); err != nil {
		t.Fatalf("first ensureSysNative(make): %v", err)
	}
	a, err := k.ReadActionByOwnerName(ctx, u.ID, "make")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Active {
		t.Fatal("@sys/make should be active after ensureSysNative")
	}
	if a.Price != 20 {
		t.Errorf("@sys/make price = %d, want 20", a.Price)
	}

	if err := ensureSysNative(ctx, k, u.Handle, spec); err != nil {
		t.Fatalf("second ensureSysNative(make): %v", err)
	}
	a2, err := k.ReadActionByOwnerName(ctx, u.ID, "make")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != a2.ID {
		t.Error("idempotent ensureSysMake must not create a second action")
	}
	if !a2.Public {
		t.Error("@sys/make should be public after idempotent ensureSysMake")
	}
}

func TestEnsureSysNativeReconcilesSchema(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetConfig(ctx, configKeySuperuser, u.Handle); err != nil {
		t.Fatal(err)
	}

	// Register with a stale schema that does not match the spec.
	stale := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"old_field": map[string]any{"type": "string", "description": "stale field"},
		},
	}
	a, err := k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
		OwnerUserID:  u.ID,
		Name:         "lookup",
		Kind:         kernel.KindNative,
		Price:        0,
		Description:  "old description",
		InputSchema:  stale,
		OutputSchema: stale,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.ActivateNativeAction(ctx, a.ID, "old description", stale, stale, 0); err != nil {
		t.Fatal(err)
	}

	// ensureSysNative must correct drift for all specs, not just @sys/make.
	spec := sysSpec("lookup")
	if err := ensureSysNative(ctx, k, u.Handle, spec); err != nil {
		t.Fatalf("ensureSysNative: %v", err)
	}

	got, err := k.ReadActionByOwnerName(ctx, u.ID, "lookup")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != spec.description {
		t.Errorf("description = %q, want %q", got.Description, spec.description)
	}
	props, _ := got.InputSchema["properties"].(map[string]any)
	if props == nil || props["query"] == nil {
		t.Error("input schema not reconciled: missing 'query' property")
	}
}

func TestBootstrapRegistersMake(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	if err := k.FirstBoot(ctx, "secret"); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}
	if err := bootstrap(k, DefaultServerConfig().Native); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	a, err := k.ReadActionByOwnerName(ctx, sys.ID, "make")
	if err != nil {
		t.Fatalf("@sys/make not registered after bootstrap: %v", err)
	}
	if !a.Active {
		t.Error("@sys/make should be active after bootstrap")
	}
	if !a.Public {
		t.Error("@sys/make should be public after bootstrap")
	}
	if a.Kind != kernel.KindNative {
		t.Errorf("@sys/make kind = %q, want native", a.Kind)
	}
	if a.Price != 20 {
		t.Errorf("@sys/make price = %d, want 20", a.Price)
	}
	if a.OwnerUserID != sys.ID {
		t.Errorf("@sys/make owner = %q, want sys ID", a.OwnerUserID)
	}
}

func TestEnsureSysNativeReconcilesPrice(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "@sys", Email: "sys@sys", Password: "pass"})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetConfig(ctx, configKeySuperuser, u.Handle); err != nil {
		t.Fatal(err)
	}

	// Create the action with price 0.
	spec := sysSpec("lookup")
	spec.price = 0
	if err := ensureSysNative(ctx, k, u.Handle, spec); err != nil {
		t.Fatalf("initial ensureSysNative: %v", err)
	}

	// Re-run with price 7 — must reconcile.
	spec.price = 7
	if err := ensureSysNative(ctx, k, u.Handle, spec); err != nil {
		t.Fatalf("reconcile ensureSysNative: %v", err)
	}

	a, err := k.ReadActionByOwnerName(ctx, u.ID, "lookup")
	if err != nil {
		t.Fatal(err)
	}
	if a.Price != 7 {
		t.Errorf("price after reconcile = %d, want 7", a.Price)
	}
}
