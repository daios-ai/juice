// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/spf13/pflag"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/native"
	"github.com/daios-ai/juice/rail"
	"github.com/daios-ai/juice/store"
)

// TestServeRequiresAWorld: the world is named positionally, so there is no path on which a kernel
// is created without one. Cobra refuses the bare command; what may name one is
// TestKernelNameValidation's subject.
func TestServeRequiresAWorld(t *testing.T) {
	if _, err := execTestCmd(t, kernelServeCmd()); err == nil {
		t.Fatal("juice kernel serve with no world was accepted")
	}
}

// testWorld is the world a first-boot test is answered with: `kernel serve` has already read it
// from the installation's worlds directory by the time the configuration is gathered.
func testWorld(t *testing.T) rail.World {
	t.Helper()
	dir := t.TempDir()
	if err := rail.Install(dir, rail.Worlds()); err != nil {
		t.Fatal(err)
	}
	w, err := rail.Load(dir, "play")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// TestFirstBootConfigAsksOrRefuses: creating a kernel is the operator's act, so first boot takes
// what they already wrote and refuses off a terminal rather than choosing for them. The name this
// kernel goes by on the network is the one answer the world cannot supply, since every kernel on a
// network shares its name.
func TestFirstBootConfigAsksOrRefuses(t *testing.T) {
	w := testWorld(t)
	home := t.TempDir()
	// No file and nobody to ask: refused, and the refusal says what to write and where.
	_, err := firstBootConfig(w, home)
	if err == nil {
		t.Fatal("a headless first boot with no configuration was accepted")
	}
	for _, want := range []string{"no kernel on play here", "no terminal", "--kernel-handle", filepath.Join(home, "config.json")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %s: %v", want, err)
		}
	}

	// A file that says nothing about the name is consent to create, but not an answer.
	seeded := filepath.Join(home, "config.json")
	if err := os.WriteFile(seeded, []byte(`{"log_level":"debug"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := firstBootConfig(w, home); err == nil ||
		!strings.Contains(err.Error(), `"kernel_handle"`) || !strings.Contains(err.Error(), "--kernel-handle") {
		t.Errorf("a seeded file with no name must be refused, naming the key and the option: %v", err)
	}

	// Pre-seeded in full, as a headless install does it: the answers are taken and nothing is asked.
	if err := os.WriteFile(seeded, []byte(`{"kernel_handle":"acme"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := firstBootConfig(w, home)
	if err != nil {
		t.Fatalf("seeded first boot: %v", err)
	}
	if cfg.KernelHandle != "acme" {
		t.Fatalf("config: handle=%q", cfg.KernelHandle)
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
	if again, lerr := LoadConfig(seeded); lerr != nil || again.KernelHandle != "acme" {
		t.Fatalf("reload: %+v %v", again.KernelHandle, lerr)
	}

	// The name is held to the rule peers apply to it, so a kernel cannot take one they would refuse.
	if err := os.WriteFile(seeded, []byte(`{"kernel_handle":"two@names"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := firstBootConfig(w, home); err == nil {
		t.Error("a name no peer would accept was written down")
	}
	// localhost names this kernel on every kernel, so no kernel is given it (D15).
	if err := os.WriteFile(seeded, []byte(`{"kernel_handle":"localhost"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := firstBootConfig(w, home); err == nil || !strings.Contains(err.Error(), "localhost names this kernel") {
		t.Errorf("a kernel named localhost: %v", err)
	}
}

// TestCheckNetworkReadsTheRecord: a kernel serves one network for life. The database records which,
// and a world that is not it is refused before anything is opened — naming both fingerprints, since
// nothing maps one back to the world that produced it.
func TestCheckNetworkReadsTheRecord(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := rail.Install(dir, rail.Worlds()); err != nil {
		t.Fatal(err)
	}
	play, err := rail.Load(dir, "play")
	if err != nil {
		t.Fatal(err)
	}
	other, err := rail.Load(dir, "arbitrum-sepolia")
	if err != nil {
		t.Fatal(err)
	}
	fresh := func() *store.DB {
		db, derr := store.Open(filepath.Join(t.TempDir(), "juice.db"))
		if derr != nil {
			t.Fatal(derr)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	recorded := func(fingerprint string) *store.DB {
		db := fresh()
		if serr := db.SetConfig(ctx, configKeyWorldFingerprint, fingerprint); serr != nil {
			t.Fatal(serr)
		}
		return db
	}

	// The world it was created on: served.
	if err := checkNetwork(ctx, recorded(play.Network().Fingerprint), play); err != nil {
		t.Errorf("a kernel was refused its own network: %v", err)
	}
	// Another: refused, naming the network it holds, the world asked for, and that world's network.
	err = checkNetwork(ctx, recorded(play.Network().Fingerprint), other)
	if err == nil {
		t.Fatal("a kernel was served on a network it was not created on")
	}
	for _, want := range []string{play.Network().Fingerprint, "arbitrum-sepolia", other.Network().Fingerprint} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %s: %v", want, err)
		}
	}
	// A database with no record is a first boot: nothing to disagree with.
	if err := checkNetwork(ctx, fresh(), other); err != nil {
		t.Errorf("a first boot was refused: %v", err)
	}
}

func TestFirstBootAtomic(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	if err := k.FirstBoot(ctx, "secret", "", testKeyOf(k)); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}

	// All config entries must be present.
	for _, key := range []string{"superuser_handle", "signing_public_key", "jwt_secret"} {
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
	if err := k.FirstBoot(ctx, "secret", "", testKeyOf(k)); err != nil {
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
		Handle: "sys@k", Password: "pass",
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
		Handle: "sys@k", Password: "pass",
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

	key := testKeyOf(k)
	if err := k.FirstBoot(ctx, "pass", "", key); err != nil {
		t.Fatal(err)
	}

	// Tamper: the database now says it is another kernel.
	other, _, _ := genKey()
	if err := k.SetConfig(ctx, configKeySigningPublic, base64.RawURLEncoding.EncodeToString(other)); err != nil {
		t.Fatal(err)
	}

	// bootstrap must reject the mismatch, and a kernel given no key at all.
	if err := bootstrap(k, key, DefaultServerConfig().Native, native.All(native.Deps{}), testNet); err == nil {
		t.Error("expected error for mismatched signing keys, got nil")
	}
	if err := bootstrap(k, nil, DefaultServerConfig().Native, native.All(native.Deps{}), testNet); err == nil {
		t.Error("a kernel was served with no key")
	}
}

func TestEnsureSysNativeReconcilesSchema(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "sys@k", Password: "pass",
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
		Title:        "Test action",
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
	if err := k.ActivateNativeAction(ctx, a.ID, "Test native", "old description", stale, stale, 0, ""); err != nil {
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
			k.RegisterNativeHandler("widget", func(_ context.Context, _ map[string]any, _, _, _, _, _ string, _ kernel.Parties) (map[string]any, error) {
				return map[string]any{}, nil
			})
		}
		return k
	}
	schema := map[string]any{"type": "object"}
	spec := native.Spec{Title: "Test native", Name: "widget", Description: "a widget native", InputSchema: schema, OutputSchema: schema,
		Handler: func(native.Host) kernel.NativeFunc { return nil }}
	const widgetPrice int64 = 7

	// Build 1 ships "widget".
	k := newBuild(true)
	if err := k.FirstBoot(ctx, "secret", "", testKeyOf(k)); err != nil {
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

	if err := k.FirstBoot(ctx, "secret", "", testKeyOf(k)); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}
	if err := bootstrap(k, testKeyOf(k), DefaultServerConfig().Native, native.All(native.Deps{}), testNet); err != nil {
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
	// Registered at the configured price, whatever the schedule says it is.
	if want := DefaultServerConfig().Native.PriceOf("tinygo/compile"); a.Price != want {
		t.Errorf("sys/tinygo/compile price = %d, want the configured %d", a.Price, want)
	}
}

// The platform stdlib is local, so a stock kernel exposes none of it across federation: every native
// is registered local (§9) and a fresh kernel — which owns nothing but natives — therefore gossips an
// empty action catalog (§13).
func TestBootstrapNativesAreLocalAndNotGossiped(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)
	namedKernel(t, "test-kernel")

	if err := k.FirstBoot(ctx, "secret", "", testKeyOf(k)); err != nil {
		t.Fatalf("FirstBoot: %v", err)
	}
	if err := bootstrap(k, testKeyOf(k), DefaultServerConfig().Native, native.All(native.Deps{}), testNet); err != nil {
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

	g, err := k.GetGossip(ctx, kernel.GossipRequest{})
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

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "sys@k", Password: "pass"})
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

// A command line says everything a file says, so it is equally the operator's instruction to make
// this kernel: a headless install can create one without writing a file first. What it does not say
// is still refused — the name this kernel goes by is nobody else's to give.
// bindServeFlags parses args as `kernel serve`'s settings, as the command line would, and returns
// the undo.
func bindServeFlags(t *testing.T, args ...string) func() {
	t.Helper()
	fs := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	serveOverride = DefaultServerConfig()
	bindConfigFlags(fs, &serveOverride)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	serveFlags = fs
	return func() { serveFlags = nil }
}

func TestFirstBootTakesTheCommandLineAsConsent(t *testing.T) {
	bind := func(args ...string) func() { return bindServeFlags(t, args...) }

	w := testWorld(t)
	home := t.TempDir()
	done := bind("--kernel-handle", "acme")
	cfg, err := firstBootConfig(w, home)
	done()
	if err != nil {
		t.Fatalf("a first boot answered entirely on the command line was refused: %v", err)
	}
	if cfg.KernelHandle != "acme" {
		t.Fatalf("config: handle=%q", cfg.KernelHandle)
	}
	if _, serr := os.Stat(filepath.Join(home, "config.json")); !os.IsNotExist(serr) {
		t.Error("first boot wrote the file itself; the caller writes it under the home's lock")
	}

	// Settings that leave the name unanswered do not answer it, and there is no terminal to ask.
	done = bind("--log-level", "debug")
	_, err = firstBootConfig(w, t.TempDir())
	done()
	if err == nil || !strings.Contains(err.Error(), "--kernel-handle") {
		t.Fatalf("a first boot with no name given must be refused, naming the option: %v", err)
	}
}

// headless makes this test run as a boot with nobody at a terminal.
func headless(t *testing.T) {
	t.Helper()
	old := interactiveTTY
	interactiveTTY = func() bool { return false }
	t.Cleanup(func() { interactiveTTY = old })
}

// testPhrase is the phrase every BIP-39 implementation is tested against, and the one a local
// development chain funds: its first account is published, so a derivation that reaches it is the
// standard one.
const testPhrase = "test test test test test test test test test test test junk"

// freshHome gives a test a kernel home of its own, so nothing it reads or writes is the machine's.
func freshHome(t *testing.T) {
	t.Helper()
	t.Setenv("JUICE_HOME", t.TempDir())
	old := worldName
	worldName = "play"
	t.Cleanup(func() { worldName = old })
}

// TestDeriveKernelKey: a phrase yields one key forever, by the path every Ethereum wallet uses for
// its first account, so the same phrase opens the same address there (P1).
func TestDeriveKernelKey(t *testing.T) {
	for phrase, want := range map[string]string{
		testPhrase: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about": "0x9858EfFD232B4033E47d90003D41EC34EcaEda94",
	} {
		key, err := deriveKernelKey(phrase)
		if err != nil {
			t.Fatal(err)
		}
		if got := ethcrypto.PubkeyToAddress(key.PublicKey).Hex(); got != want {
			t.Errorf("%q derives %s, want %s", phrase, got, want)
		}
	}
	key, _ := deriveKernelKey(testPhrase)
	if got := hex.EncodeToString(ethcrypto.FromECDSA(key)); got != "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80" {
		t.Errorf("the test phrase's key is %s", got)
	}
	// Spacing is not part of a phrase: the same words give the same kernel key and the same
	// recovery key however they are spaced. A word that is not one is refused.
	const respaced = "  test test test test test test\ttest test test test test   junk "
	spaced, err := deriveKernelKey(respaced)
	if err != nil || !spaced.Equal(key) {
		t.Errorf("the same words spaced differently are another key: %v", err)
	}
	plain, _ := deriveRecoveryKey(testPhrase)
	if got, err := deriveRecoveryKey(respaced); err != nil || !got.Equal(plain) {
		t.Errorf("the same words spaced differently are another recovery key: %v", err)
	}
	for _, bad := range []string{"", "test test test", "test test test test test test test test test test test test"} {
		if _, err := deriveKernelKey(bad); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("deriveKernelKey(%q) = %v, want ErrInvalidInput", bad, err)
		}
	}
	// The superuser's recovery key comes from the same phrase and is not the kernel's key.
	recovery, err := deriveRecoveryKey(testPhrase)
	if err != nil || bytes.Equal(recovery.Seed(), ethcrypto.FromECDSA(key)) {
		t.Errorf("the recovery key must be its own key: %v", err)
	}
}

// TestFirstBootCredentials: off a terminal each thing a kernel is created with has one source, its
// variable, and a refusal names the variable. The phrase is the kernel: it is never made up where
// the only place to show it is a log, and a home that already holds a key takes only the phrase
// that key came from.
func TestFirstBootCredentials(t *testing.T) {
	headless(t)
	kernel.SetMinPasswordLenForTesting(8)
	t.Cleanup(func() { kernel.SetMinPasswordLenForTesting(1) })
	want, _ := deriveKernelKey(testPhrase)
	other, _, _ := genKey()
	_ = other
	_, another, _ := genKey()

	cases := []struct {
		name, password, phrase string
		held                   *ecdsa.PrivateKey
		refusal                string
	}{
		{"no password", "", testPhrase, nil, envBootstrapPassword},
		{"short password", "short12", testPhrase, nil, envBootstrapPassword},
		{"no phrase", "pass1234", "", nil, envBootstrapPhrase},
		{"not a phrase", "pass1234", "eleven words are not a phrase at all in any language", nil, envBootstrapPhrase},
		{"a phrase", "pass1234", testPhrase, nil, ""},
		{"an unfinished kernel and its phrase", "pass1234", testPhrase, want, ""},
		{"an unfinished kernel and another phrase", "pass1234", testPhrase, another, envBootstrapPhrase},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			freshHome(t)
			if c.held != nil {
				if err := os.MkdirAll(kernelHome(), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := rail.StoreKey(kernelKeyPath(), c.held); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(envBootstrapPassword, c.password)
			t.Setenv(envBootstrapPhrase, c.phrase)
			var got bootCredentials
			var err error
			shown := captureStderr(t, func() { got, err = firstBootCredentials(rail.World{Name: "play", Rail: rail.RailManual}) })
			if strings.Contains(shown, c.phrase) && c.phrase != "" {
				t.Errorf("the phrase was printed: %q", shown)
			}
			if c.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), c.refusal) {
					t.Fatalf("want a refusal naming %s, got %v", c.refusal, err)
				}
				return
			}
			recovery, _ := deriveRecoveryKey(testPhrase)
			if err != nil || got.password != c.password || !got.key.Equal(want) ||
				got.recoveryKey != base64.RawURLEncoding.EncodeToString(recovery.Public().(ed25519.PublicKey)) {
				t.Fatalf("got %+v, %v", got, err)
			}
			if !strings.Contains(shown, kernel.PublicKey(want)) {
				t.Errorf("a restore must say which kernel the phrase is: %q", shown)
			}
		})
	}
}

// TestFirstBootAtATerminal: at a terminal the phrase is asked. None creates a new kernel, whose
// phrase is shown once before anything is written and is the one its key and its superuser's
// recovery key come from; one given restores the kernel it belongs to, and on a chain the boot
// says the address that kernel is paid at.
func TestFirstBootAtATerminal(t *testing.T) {
	freshHome(t)
	old := interactiveTTY
	interactiveTTY = func() bool { return true }
	oldAsk, oldShow := promptMnemonic, showPhrase
	t.Cleanup(func() { interactiveTTY, promptMnemonic, showPhrase = old, oldAsk, oldShow })
	t.Setenv(envBootstrapPassword, "secret")
	t.Setenv(envBootstrapPhrase, "")

	var asked, shown string
	promptMnemonic = func(prompt string) (string, error) { asked = prompt; return "", nil }
	showPhrase = func(_, mnemonic string) error { shown = mnemonic; return nil }
	created, err := firstBootCredentials(rail.World{Name: "play", Rail: rail.RailManual})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked, "Enter to create a new kernel") {
		t.Errorf("asked %q", asked)
	}
	key, kerr := deriveKernelKey(shown)
	recovery, rerr := deriveRecoveryKey(shown)
	if kerr != nil || rerr != nil || !created.key.Equal(key) ||
		created.recoveryKey != base64.RawURLEncoding.EncodeToString(recovery.Public().(ed25519.PublicKey)) {
		t.Fatalf("the phrase shown (%q) is not the one the kernel was made from: %v %v", shown, kerr, rerr)
	}
	if _, err := os.Stat(kernelHome()); !os.IsNotExist(err) {
		t.Errorf("asking wrote the home: %v", err)
	}

	shown = ""
	promptMnemonic = func(string) (string, error) { return testPhrase, nil }
	var restored bootCredentials
	said := captureStderr(t, func() {
		restored, err = firstBootCredentials(rail.World{Name: "polygon", Rail: rail.RailEVM})
	})
	want, _ := deriveKernelKey(testPhrase)
	if err != nil || !restored.key.Equal(want) || shown != "" {
		t.Fatalf("restore: %v, a phrase shown %q", err, shown)
	}
	if !strings.Contains(said, kernel.PublicKey(want)) || !strings.Contains(said, "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266") {
		t.Errorf("a restore on a chain must say the kernel and its address: %q", said)
	}

	// A home that holds a key was not finished being created: it is not offered a new kernel.
	if err := os.MkdirAll(kernelHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rail.StoreKey(kernelKeyPath(), want); err != nil {
		t.Fatal(err)
	}
	promptMnemonic = func(prompt string) (string, error) { asked = prompt; return "", nil }
	if _, err := firstBootCredentials(rail.World{Name: "play", Rail: rail.RailManual}); err == nil || strings.Contains(asked, "create a new kernel") {
		t.Errorf("an unfinished kernel was offered a new one (asked %q): %v", asked, err)
	}
}

// TestFirstBootRecordsTheKernel: first boot enrolls the superuser's recovery key and binds the
// database to the kernel's key, and to no other afterwards.
func TestFirstBootRecordsTheKernel(t *testing.T) {
	ctx := context.Background()
	k := newTestKernel(t)
	key := testKeyOf(k)
	recovery, _ := deriveRecoveryKey(testPhrase)
	recoveryKey := base64.RawURLEncoding.EncodeToString(recovery.Public().(ed25519.PublicKey))
	captureStderr(t, func() {
		if err := firstBoot(ctx, k, bootCredentials{password: "secret", key: key, recoveryKey: recoveryKey}); err != nil {
			t.Fatal(err)
		}
	})
	if sys, err := k.ReadUserByHandle(ctx, superuserHandle); err != nil || sys.RecoveryPublicKey != recoveryKey {
		t.Fatalf("the phrase's recovery key was not enrolled: %+v %v", sys, err)
	}
	if got := k.SelfKey(ctx); got != kernel.PublicKey(key) {
		t.Errorf("the database is kernel %s, want %s", got, kernel.PublicKey(key))
	}
	if v, _ := k.GetConfig(ctx, "signing_private_key"); v != "" {
		t.Error("the key was written into the database")
	}
	_, stranger, _ := genKey()
	if err := k.FirstBoot(ctx, "secret", recoveryKey, stranger); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("a database was booted under another kernel's key: %v", err)
	}
	if err := bootstrap(k, stranger, DefaultServerConfig().Native, native.All(native.Deps{}), testNet); err == nil {
		t.Error("a database was served under another kernel's key")
	}
}

// TestFirstBootRefusalWritesNothing: what a kernel is created with is refused before the home is
// written, as a missing name is, so the boot can be run again as if it never happened.
func TestFirstBootRefusalWritesNothing(t *testing.T) {
	headless(t)
	t.Setenv("JUICE_HOME", t.TempDir())
	t.Setenv(envBootstrapPassword, "secret")
	t.Setenv(envBootstrapPhrase, "not a phrase")
	old := worldName
	t.Cleanup(func() { worldName = old })
	t.Cleanup(bindServeFlags(t, "--kernel-handle", "acme"))

	err := runServer("play")
	if err == nil || !strings.Contains(err.Error(), envBootstrapPhrase) {
		t.Fatalf("want a refusal naming %s, got %v", envBootstrapPhrase, err)
	}
	if entries, rerr := os.ReadDir(kernelHome()); !os.IsNotExist(rerr) {
		t.Errorf("the refused boot wrote its home: %v %v", entries, rerr)
	}
}

// TestAKernelWithoutItsKeyIsRefusedUntouched: a home whose database exists without the key file is
// a kernel made before one key served as identity and address, or one whose key was lost. Either
// way it is refused before the database is opened, so nothing in it changes.
func TestAKernelWithoutItsKeyIsRefusedUntouched(t *testing.T) {
	headless(t)
	t.Setenv("JUICE_HOME", t.TempDir())
	old := worldName
	t.Cleanup(func() { worldName = old })
	t.Cleanup(bindServeFlags(t))
	worldName = "play"
	if err := os.MkdirAll(kernelHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(kernelHome(), "juice.db")
	if err := os.WriteFile(db, []byte("an older kernel's database"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runServer("play")
	if err == nil || !strings.Contains(err.Error(), rail.KeyFile) {
		t.Fatalf("want a refusal naming %s, got %v", rail.KeyFile, err)
	}
	if got, _ := os.ReadFile(db); string(got) != "an older kernel's database" {
		t.Errorf("the refused boot touched the database: %q", got)
	}
}

// TestKernelWorldsListsWhatServeReads: `kernel worlds` is the list of what `kernel serve` would
// read, before any kernel has served — the shipped worlds from the binary until they are written,
// the files here once they are, an operator's edit and addition included — and it writes nothing. A
// file serve would refuse, by its name or by what it says, is listed with serve's error and nothing
// else, so a program choosing a world can tell it apart; a worlds directory that cannot be read is
// an error, never a listing with the operator's files missing.
// TestKernelSetupWritesTheInstallationFiles: setup, which the installer runs, leaves the schemas
// and the shipped worlds and creates no kernel and no client records. Run again, as every serve
// does, it restores the schemas and keeps an operator's edited world.
func TestKernelSetupWritesTheInstallationFiles(t *testing.T) {
	root := testHome(t)
	if !kernelSetupCmd().Hidden {
		t.Error("kernel setup is for installers and must be hidden from help")
	}
	// The exact command install.sh runs, through registration and argument parsing.
	keepRootTree(t)
	if out, err := execTestCmd(t, rootCmd, "kernel", "setup"); err != nil {
		t.Fatalf("juice kernel setup: %v %s", err, out)
	}
	schema := filepath.Join(root, "schemas", "config.schema.json")
	generated, err := os.ReadFile(schema)
	if err != nil {
		t.Fatalf("no config schema after setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "schemas", "world.schema.json")); err != nil {
		t.Errorf("no world schema after setup: %v", err)
	}
	shipped, err := fs.ReadDir(rail.Worlds(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range shipped {
		if _, err := os.Stat(filepath.Join(root, "worlds", e.Name())); err != nil {
			t.Errorf("shipped world %s not written: %v", e.Name(), err)
		}
	}
	for _, d := range []string{"kernels", "client"} {
		if _, err := os.Stat(filepath.Join(root, d)); !os.IsNotExist(err) {
			t.Errorf("setup created %s/", d)
		}
	}

	edited := []byte(`{"rail":"manual","decimals":6,"symbol":"fUSD","description":"my own words","seeds":[]}`)
	world := filepath.Join(root, "worlds", "play.json")
	if err := os.WriteFile(world, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(schema, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setup(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(world); string(got) != string(edited) {
		t.Errorf("setup overwrote an edited world: %s", got)
	}
	if got, _ := os.ReadFile(schema); string(got) != string(generated) {
		t.Error("setup did not restore the generated schema")
	}
}

func TestKernelWorldsListsWhatServeReads(t *testing.T) {
	root := testHome(t)
	dir := filepath.Join(root, "worlds")

	rows := func() []map[string]any {
		t.Helper()
		got, err := listWorlds(dir)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	byName := func(rs []map[string]any, name string) map[string]any {
		for _, r := range rs {
			if r["world"] == name {
				return r
			}
		}
		t.Fatalf("no row for %s in %v", name, rs)
		return nil
	}

	// Nothing has served: the four shipped worlds, read from the binary, no file named.
	fresh := rows()
	if len(fresh) != 4 {
		t.Fatalf("fresh installation lists %d worlds: %v", len(fresh), fresh)
	}
	for _, r := range fresh {
		if r["file"] != "" || r["error"] != nil || r["description"] == "" || r["symbol"] == "" {
			t.Errorf("a shipped world not yet written must list from the binary with no file: %v", r)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("listing the worlds wrote the worlds directory")
	}

	// Served once: the files are here, and what they say is what is listed — an edit and an addition.
	if err := rail.Install(dir, rail.Worlds()); err != nil {
		t.Fatal(err)
	}
	edited := []byte(`{"rail":"manual","decimals":6,"symbol":"fUSD","description":"my own words","seeds":[]}`)
	if err := os.WriteFile(filepath.Join(dir, "play.json"), edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mine.json"), edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"rail":"manual","nope":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a+b.json"), edited, 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory bearing a world's name is what serve would open and fail on, so it is listed with
	// that failure rather than passed over for the shipped copy.
	if err := os.Remove(filepath.Join(dir, "polygon.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "polygon.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	served := rows()
	if len(served) != 7 {
		t.Fatalf("listed %d worlds: %v", len(served), served)
	}
	if r := byName(served, "play"); r["description"] != "my own words" || r["file"] != filepath.Join(dir, "play.json") {
		t.Errorf("an edited shipped world must be listed as the file says: %v", r)
	}
	if r := byName(served, "mine"); r["description"] != "my own words" || r["error"] != nil {
		t.Errorf("an operator's own world must be listed: %v", r)
	}
	for _, name := range []string{"broken", "a+b", "polygon"} {
		r := byName(served, name)
		if r["error"] == nil || r["description"] != nil || r["symbol"] != nil || r["rail"] != nil {
			t.Errorf("a file serve would refuse must carry serve's error and nothing else: %v", r)
		}
	}
	if r := byName(served, "a+b"); !strings.Contains(r["error"].(string), "letters, digits") {
		t.Errorf("a name serve refuses must be refused as a name: %v", r["error"])
	}

	// The command's three outputs: the list, the rows as built, the names alone.
	out := captureStdout(t, func() error { _, err := execTestCmd(t, kernelWorldsCmd()); return err })
	for _, want := range []string{"WORLD", "my own words", "unknown field", "mine", "is a directory"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list omits %q:\n%s", want, out)
		}
	}
	oldJSON, oldQuiet := flagJSON, flagQuiet
	t.Cleanup(func() { flagJSON, flagQuiet = oldJSON, oldQuiet })
	flagJSON = true
	out = captureStdout(t, func() error { _, err := execTestCmd(t, kernelWorldsCmd()); return err })
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || len(parsed) != 7 {
		t.Errorf("--json is the rows as built: %v %s", err, out)
	}
	flagJSON, flagQuiet = false, true
	out = captureStdout(t, func() error { _, err := execTestCmd(t, kernelWorldsCmd()); return err })
	if out != "a+b\narbitrum-one\narbitrum-sepolia\nbroken\nmine\nplay\npolygon\n" {
		t.Errorf("--quiet is the names, one per line: %q", out)
	}
	flagQuiet = false

	// A directory that is here and cannot be read is an error, not a listing of shipped worlds alone.
	if os.Getuid() != 0 {
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		if _, err := listWorlds(dir); err == nil {
			t.Error("an unreadable worlds directory listed as though it were absent")
		}
	}
}
