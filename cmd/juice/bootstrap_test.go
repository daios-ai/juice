package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
)

// TestFirstBootRequiresKernelName: a headless first boot with no kernel name configured must fail
// (never silently name the kernel); providing the name via env lets it boot and persists it.
func TestFirstBootRequiresKernelName(t *testing.T) {
	saved := globalCfg.KernelHandle
	t.Cleanup(func() { globalCfg.KernelHandle = saved })
	t.Setenv("JUICE_BOOTSTRAP_PASSWORD", "pw")

	globalCfg.KernelHandle = ""
	if err := bootstrap(newTestKernel(t), DefaultServerConfig().Native); err == nil ||
		!strings.Contains(err.Error(), "kernel name is required") {
		t.Fatalf("headless boot with no name: want required-name error, got %v", err)
	}

	t.Setenv("JUICE_BOOTSTRAP_KERNEL_HANDLE", "acme")
	globalCfg.KernelHandle = ""
	if err := bootstrap(newTestKernel(t), DefaultServerConfig().Native); err != nil {
		t.Fatalf("boot with name via env: %v", err)
	}
	if globalCfg.KernelHandle != "acme" {
		t.Errorf("kernel handle = %q, want @acme", globalCfg.KernelHandle)
	}
}

func TestFirstBootAtomic(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	if err := k.FirstBoot(ctx, "secret", ""); err != nil {
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
	u, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil || u == nil {
		t.Fatalf("sys not found after FirstBoot: %v", err)
	}

	// Second call must be a no-op (idempotent) and preserve the same secret.
	if err := k.FirstBoot(ctx, "secret", ""); err != nil {
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
		Handle: "sys", Password: "pass",
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
	if a.Visibility != kernel.VisibilityLocal {
		t.Error("lookup should be local after idempotent ensureSysNative")
	}
}

func TestEnsureSysLLMChatIdempotent(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "sys", Password: "pass",
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
	if a.Visibility != kernel.VisibilityLocal {
		t.Error("llm/chat should be local after idempotent ensureSysNative")
	}
}

func TestBootstrapRejectsKeyMismatch(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	// Run first boot to generate a valid key pair.
	if err := k.FirstBoot(ctx, "pass", ""); err != nil {
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

func TestEnsureSysNativeReconcilesSchema(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "sys", Password: "pass",
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
	if err := k.ActivateNativeAction(ctx, a.ID, "old description", stale, stale, 0, ""); err != nil {
		t.Fatal(err)
	}

	// ensureSysNative must correct drift for all specs.
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

// TestBootstrapReRegistersPrunedNative proves the prune is clean: after a native is soft-deleted
// (its handler removed from the build), re-introducing it (handler + spec) re-registers it under the
// SAME desired name, active — the lingering soft-deleted row does not block it (partial unique index).
// Distinct kernel instances over one shared DB simulate successive builds (registered handlers differ).
func TestBootstrapReRegistersPrunedNative(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "reintro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "bootstrap-test-secret"
	newBuild := func(withWidget bool) *kernel.Kernel {
		k := kernel.New(db, nil, nil, nil, cfg, log.Discard())
		if withWidget {
			k.RegisterNativeHandler("widget", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return map[string]any{}, nil
			})
		}
		return k
	}
	schema := map[string]any{"type": "object"}
	spec := sysNativeSpec{name: "widget", price: 7, description: "a widget native", inputSchema: schema, outputSchema: schema}

	// Build 1 ships "widget".
	k := newBuild(true)
	if err := k.FirstBoot(ctx, "secret", ""); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}
	su, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureSysNative(ctx, k, "sys", spec); err != nil {
		t.Fatalf("ensureSysNative (build 1): %v", err)
	}
	first, err := k.ReadActionByOwnerName(ctx, su.ID, "widget")
	if err != nil {
		t.Fatalf("widget missing after build 1: %v", err)
	}
	oldID := first.ID

	// Build 2 DROPPED "widget": a kernel over the same DB with no widget handler → prune soft-deletes it.
	pruned, err := newBuild(false).PruneOrphanedNativeActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != "widget" {
		t.Fatalf("prune = %v, want [widget]", pruned)
	}
	if _, err := k.ReadActionByOwnerName(ctx, su.ID, "widget"); err == nil {
		t.Fatal("widget should be soft-deleted after prune")
	}

	// Build 3 RE-INTRODUCES "widget": handler back, ensure again → re-registers under the same name.
	kBack := newBuild(true)
	if err := ensureSysNative(ctx, kBack, "sys", spec); err != nil {
		t.Fatalf("ensureSysNative (reintroduce): %v", err)
	}
	again, err := kBack.ReadActionByOwnerName(ctx, su.ID, "widget")
	if err != nil {
		t.Fatalf("widget not re-registered after reintroduction: %v", err)
	}
	if again.Name != "widget" || !again.Active || again.Kind != kernel.KindNative {
		t.Errorf("reintroduced widget: name=%q active=%v kind=%q, want widget/active/native", again.Name, again.Active, again.Kind)
	}
	if again.ID == oldID {
		t.Error("expected a fresh row id after reintroduction (soft-deleted row is not reused)")
	}
	// Its handler is registered again, so a subsequent prune must leave it alone.
	if p2, err := kBack.PruneOrphanedNativeActions(ctx); err != nil || len(p2) != 0 {
		t.Errorf("prune after reintroduction should be a no-op, got %v err=%v", p2, err)
	}
}

func TestBootstrapRegistersTinyGoCompile(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	if err := k.FirstBoot(ctx, "secret", ""); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}
	if err := bootstrap(k, DefaultServerConfig().Native); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	sys, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	a, err := k.ReadActionByOwnerName(ctx, sys.ID, "tinygo/compile")
	if err != nil {
		t.Fatalf("sys/tinygo/compile not registered after bootstrap: %v", err)
	}
	if !a.Active || a.Visibility != kernel.VisibilityLocal {
		t.Errorf("sys/tinygo/compile should be active and local, got active=%v visibility=%v", a.Active, a.Visibility)
	}
	if a.Kind != kernel.KindNative {
		t.Errorf("sys/tinygo/compile kind = %q, want native", a.Kind)
	}
	if a.Price != 5 {
		t.Errorf("sys/tinygo/compile price = %d, want 5", a.Price)
	}
}

// The platform stdlib is local, so a stock kernel exposes none of it across federation: every native
// is registered local (§9) and a fresh kernel — which owns nothing but natives — therefore gossips an
// empty action catalog while still advertising sys as a first-party user (§13).
func TestBootstrapNativesAreLocalAndNotGossiped(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	if err := k.FirstBoot(ctx, "secret", ""); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}
	if err := bootstrap(k, DefaultServerConfig().Native); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	all, err := k.ListAllActions(ctx, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	natives := 0
	for _, a := range all {
		if a.Kind != kernel.KindNative {
			continue
		}
		natives++
		if a.Visibility != kernel.VisibilityLocal {
			t.Errorf("native sys/%s visibility = %q, want local", a.Name, a.Visibility)
		}
	}
	if natives == 0 {
		t.Fatal("bootstrap registered no native actions")
	}

	g, err := k.GetGossip(ctx, "", "")
	if err != nil {
		t.Fatalf("GetGossip: %v", err)
	}
	if len(g.ActionManifests) != 0 {
		t.Errorf("gossip served %d manifests, want 0 (natives are local)", len(g.ActionManifests))
	}
	if len(g.Users) != 1 || g.Users[0].Handle != "sys" {
		t.Errorf("gossip users = %+v, want sys alone", g.Users)
	}
}

func TestEnsureSysNativeReconcilesPrice(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "sys", Password: "pass"})
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
