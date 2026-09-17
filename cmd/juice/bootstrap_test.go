// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/native"
	"github.com/daios-ai/juice/rail"
	"github.com/daios-ai/juice/store"
)

// TestServeRequiresAKernelName: the kernel is named positionally, so there is no path on which a
// kernel is created unnamed. Cobra refuses the bare command; what may name one is
// TestKernelNameValidation's subject.
func TestServeRequiresAKernelName(t *testing.T) {
	if _, err := execTestCmd(t, kernelServeCmd()); err == nil {
		t.Fatal("juice serve with no name was accepted")
	}
}

// TestFirstBootConfigAsksOrRefuses: a kernel joins one network for life, so first boot takes the
// world from what the operator already wrote, and refuses off a terminal rather than choosing.
func TestFirstBootConfigAsksOrRefuses(t *testing.T) {
	home := t.TempDir()
	// No file and nobody to ask: creating a kernel is the operator's act, so it is refused, and the
	// refusal says what to write and where.
	_, err := firstBootConfig("acme", home)
	if err == nil {
		t.Fatal("a headless first boot with no configuration was accepted")
	}
	for _, want := range []string{"no kernel named acme", "no terminal", `"world"`, filepath.Join(home, "config.json")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %s: %v", want, err)
		}
	}

	// A file with no world is consent to create, but not an answer to the one question that cannot
	// be revised.
	seeded := filepath.Join(home, "config.json")
	if err := os.WriteFile(seeded, []byte(`{"kernel_handle":"acme"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := firstBootConfig("acme", home); err == nil || !strings.Contains(err.Error(), `"world"`) {
		t.Errorf("a seeded file with no world must be refused, naming the key: %v", err)
	}

	// Pre-seeded in full, as a headless install does it: the answers are taken and nothing is asked.
	if err := os.WriteFile(seeded, []byte(`{"world":"play"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := firstBootConfig("acme", home)
	if err != nil {
		t.Fatalf("seeded first boot: %v", err)
	}
	if cfg.World != "play" || cfg.KernelHandle != "acme" {
		t.Fatalf("config: world=%q handle=%q", cfg.World, cfg.KernelHandle)
	}
	if cfg.CredentialsKey == "" {
		t.Error("first boot must mint the credentials key, since nothing later may write the file")
	}
	// Asking writes nothing: the caller writes, under the lock that makes one kernel one server.
	if entries, rerr := os.ReadDir(home); rerr != nil || len(entries) != 1 {
		t.Errorf("first boot must leave only the file it was given: %v %v", entries, rerr)
	}
	// What it produces must survive the strict loader, since that is what every later boot reads.
	if err := writeConfig(seeded, cfg); err != nil {
		t.Fatal(err)
	}
	if again, lerr := LoadConfig(seeded); lerr != nil || again.World != "play" {
		t.Fatalf("reload: %+v %v", again.World, lerr)
	}
}

// TestWorldForReadsTheRecord: a kernel's network is fixed the first time and read from its own
// database ever after, so one made before `world` was written into config.json goes on serving.
func TestWorldForReadsTheRecord(t *testing.T) {
	ctx := context.Background()
	play, err := rail.Load("play")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	fresh := func() *store.DB {
		db, derr := store.Open(filepath.Join(t.TempDir(), "juice.db"))
		if derr != nil {
			t.Fatal(derr)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	recorded := func(digest string) *store.DB {
		db := fresh()
		if serr := db.SetConfig(ctx, configKeyWorldDigest, digest); serr != nil {
			t.Fatal(serr)
		}
		return db
	}

	// The record answers, with nothing in the configuration to ask.
	if w, werr := worldFor(ctx, recorded(play.Network().Digest), "", cfgPath); werr != nil || w.Name != "play" {
		t.Errorf("recorded network: %q %v", w.Name, werr)
	}
	// A configuration that disagrees with the record is refused, naming both.
	test, err := rail.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = worldFor(ctx, recorded(test.Network().Digest), "play", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "test") || !strings.Contains(err.Error(), "play") {
		t.Errorf("a configuration against the record must be refused, naming both: %v", err)
	}
	// A digest this build cannot place needs the world file named, and says so.
	_, err = worldFor(ctx, recorded("00ff"), "", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "does not ship") {
		t.Errorf("an unplaceable digest must be refused: %v", err)
	}
	// A database that predates the rail belongs to play, and may not be moved onto a token world.
	made := fresh()
	if serr := made.SetConfig(ctx, configKeySuperuser, "sys"); serr != nil {
		t.Fatal(serr)
	}
	if w, werr := worldFor(ctx, made, "", cfgPath); werr != nil || w.Name != "play" {
		t.Errorf("pre-rail database: %q %v", w.Name, werr)
	}
	if _, werr := worldFor(ctx, made, "test", cfgPath); werr == nil {
		t.Error("a pre-rail database must not be bound to a token world")
	}
	// Nothing recorded and nothing configured is a first boot with no answer: refused, naming key
	// and file rather than falling back to a network nobody chose.
	_, err = worldFor(ctx, fresh(), "", cfgPath)
	if err == nil || !strings.Contains(err.Error(), `"world"`) || !strings.Contains(err.Error(), cfgPath) {
		t.Errorf("an unanswered first boot must be refused, naming the key and the file: %v", err)
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
	db := newTestStore(t)
	cfg := testConfig("bootstrap-test-secret")
	return newKernel(cfg, kernel.Dependencies{Store: db})
}

// sysSpec returns the shipped native.Spec for name, with the configured default price (§9/§14).
func sysSpec(name string) (native.Spec, int64) {
	for _, s := range native.All(native.Deps{}) {
		if s.Name == name {
			return s, DefaultServerConfig().Native.PriceOf(name)
		}
	}
	panic("native spec not found: " + name)
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

	spec, price := sysSpec("lookup")

	// First call: creates the action.
	if err := ensureSysNative(ctx, k, u.Handle, spec, price); err != nil {
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
	if err := ensureSysNative(ctx, k, u.Handle, spec, price); err != nil {
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

	spec, price := sysSpec("llm/chat")

	// First call: creates the action.
	if err := ensureSysNative(ctx, k, u.Handle, spec, price); err != nil {
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
	if err := ensureSysNative(ctx, k, u.Handle, spec, price); err != nil {
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
	if err := bootstrap(k, DefaultServerConfig().Native, native.All(native.Deps{}), testNet); err == nil {
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
	spec, price := sysSpec("lookup")
	if err := ensureSysNative(ctx, k, u.Handle, spec, price); err != nil {
		t.Fatalf("ensureSysNative: %v", err)
	}

	got, err := k.ReadActionByOwnerName(ctx, u.ID, "lookup")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != spec.Description {
		t.Errorf("description = %q, want %q", got.Description, spec.Description)
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
	db := newTestStore(t)
	cfg := testConfig("bootstrap-test-secret")
	newBuild := func(withWidget bool) *kernel.Kernel {
		k := newKernel(cfg, kernel.Dependencies{Store: db})
		if withWidget {
			k.RegisterNativeHandler("widget", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return map[string]any{}, nil
			})
		}
		return k
	}
	schema := map[string]any{"type": "object"}
	spec := native.Spec{Name: "widget", Description: "a widget native", InputSchema: schema, OutputSchema: schema,
		Handler: func(native.Host) kernel.NativeFunc { return nil }}
	const widgetPrice int64 = 7

	// Build 1 ships "widget".
	k := newBuild(true)
	if err := k.FirstBoot(ctx, "secret", ""); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}
	su, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureSysNative(ctx, k, "sys", spec, widgetPrice); err != nil {
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
	if err := ensureSysNative(ctx, kBack, "sys", spec, widgetPrice); err != nil {
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

// namedKernel gives bootstrap the network name it requires before writing anything (§12), and
// restores the package-global config afterwards.
func namedKernel(t *testing.T, name string) {
	t.Helper()
	saved := globalCfg.KernelHandle
	globalCfg.KernelHandle = name
	t.Cleanup(func() { globalCfg.KernelHandle = saved })
}

func TestBootstrapRegistersTinyGoCompile(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)
	namedKernel(t, "test-kernel")

	if err := k.FirstBoot(ctx, "secret", ""); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}
	if err := bootstrap(k, DefaultServerConfig().Native, native.All(native.Deps{}), testNet); err != nil {
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
// empty action catalog (§13).
func TestBootstrapNativesAreLocalAndNotGossiped(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)
	namedKernel(t, "test-kernel")

	if err := k.FirstBoot(ctx, "secret", ""); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}
	if err := bootstrap(k, DefaultServerConfig().Native, native.All(native.Deps{}), testNet); err != nil {
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
	spec, price := sysSpec("lookup")
	price = 0
	if err := ensureSysNative(ctx, k, u.Handle, spec, price); err != nil {
		t.Fatalf("initial ensureSysNative: %v", err)
	}

	// Re-run with price 7 — must reconcile.
	price = 7
	if err := ensureSysNative(ctx, k, u.Handle, spec, price); err != nil {
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

// A chain world used to stop first boot with a question nobody could answer from the words on
// screen. The shipped worlds name a node, so there is nothing left to ask; a world written without
// one still asks, and still refuses off a terminal rather than guessing.
func TestTheEndpointIsAskedOnlyWhenTheWorldNamesNone(t *testing.T) {
	t.Run("a shipped chain world asks nothing", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, "config.json"),
			[]byte(`{"world":"test"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := firstBootConfig("acme", home)
		if err != nil {
			t.Fatalf("a chain world with a node in it still demanded one: %v", err)
		}
		if cfg.RailRPC != "" {
			t.Errorf("first boot wrote an endpoint nobody typed: %q", cfg.RailRPC)
		}
		w, err := rail.Load(cfg.World)
		if err != nil || w.RPC == "" {
			t.Fatalf("the shipped world names no node: %+v %v", w.RPC, err)
		}
	})

	t.Run("a world of one's own with no node is asked, and refused headless", func(t *testing.T) {
		home := t.TempDir()
		world := filepath.Join(home, "mine.json")
		raw, err := os.ReadFile(filepath.Join("..", "..", "rail", "worlds", "test.json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		doc["name"] = "mine"
		doc["rpc"] = ""
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(world, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "config.json"),
			[]byte(`{"world":`+strconv.Quote(world)+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = firstBootConfig("acme", home)
		if err == nil {
			t.Fatal("a world naming no node was accepted with no endpoint and no terminal")
		}
		for _, want := range []string{`"rail_rpc"`, "no terminal"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must name %s: %v", want, err)
			}
		}
	})
}
