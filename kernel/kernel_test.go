// SPDX-License-Identifier: AGPL-3.0-only

package kernel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/rail"
	"github.com/daios-ai/juice/store"
	"github.com/google/uuid"
)

func TestMain(m *testing.M) {
	kernel.SetBcryptCostForTesting(4) // bcrypt.MinCost
	kernel.SetMinPasswordLenForTesting(1)
	os.Exit(m.Run())
}

// testIssuerUserID is a fixed sentinel user ID inserted into every test store.
// All test kernels use this as their IssuerUserID so that receipt FK constraints pass.
const testIssuerUserID = "00000000-0000-0000-0000-000000000001"

// testNet is the play network every test signs on. The kernels these helpers build carry it too, so
// a signature made in a test verifies in the kernel under test rather than by coincidence.
var testNet = kernel.Network{Name: "play", Digest: "ef1fac03f5f78ca42dfa05b9eb975b5e0944e013ed1eb5ea30a2be9328e34a67"}

// newRef mints the reference a deposit records. Every crossing names the payment it stands for, so
// a test that funds an account twice must name two payments (U3).
func newRef() string { return "test:" + uuid.NewString() }

func newTestStore(t testing.TB) kernel.Store {
	t.Helper()
	return newTestStoreAt(t, t.TempDir()+"/test.db")
}

// newTestStoreAt opens the store at a known path, for a test that must reach the file behind the
// store — to corrupt a row the way a disk or an operator could, which no store method may do.
func newTestStoreAt(t testing.TB, path string) kernel.Store {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Seed the issuer user so receipt FK constraints are satisfied.
	hash, err := kernel.HashPassword("issuer-password")
	if err != nil {
		t.Fatalf("newTestStore: hash password: %v", err)
	}
	issuer := &kernel.Account{
		ID:           testIssuerUserID,
		Handle:       "@_test_issuer",
		PasswordHash: hash,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := db.CreateUser(context.Background(), issuer); err != nil {
		t.Fatalf("newTestStore: seed issuer user: %v", err)
	}
	return db
}

// testConfig is the policy every test kernel starts from; a test with another policy edits the copy.
func testConfig() kernel.Config {
	cfg := kernel.DefaultConfig()
	cfg.Network = testNet
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	return cfg
}

// testEconomy is the money policy every test kernel starts from; a test with another policy edits
// the copy and passes it in deps. The lottery is off, so every obligation is paid exactly and the
// arithmetic a test asserts is the one it wrote — a test about the draw turns it on deliberately.
func testEconomy() kernel.Economy {
	econ := kernel.DefaultEconomy()
	// Exact payment, so a test asserting an amount gets the one it wrote; a test about the draw
	// turns the lottery on deliberately.
	econ.CreditLimit, econ.Lottery = 1000, 0
	return econ
}

// newKernel builds a kernel from cfg and the adapters in deps. What every test wires the same way —
// the logger, a double that also speaks federation, the manual rail (every world runs the same
// money rules, and play's finalized facts are the operator's own records, D23) — is wired here, so
// a new kernel dependency is added once rather than at each construction site.
func newKernel(cfg kernel.Config, deps kernel.Dependencies) *kernel.Kernel {
	deps.Config = cfg
	if deps.Economy == (kernel.Economy{}) {
		deps.Economy = testEconomy()
	}
	if deps.Logger == nil {
		deps.Logger = log.Default()
	}
	if fc, ok := deps.HTTP.(kernel.FederationClient); ok && deps.Federation == nil {
		deps.Federation = fc
	}
	k := kernel.New(deps)
	k.SetRail(rail.NewManual())
	return k
}

// mustResolve reads the action a federated call names, the way the inbound handler does before it
// hands the row to the kernel.
func mustResolve(t *testing.T, k *kernel.Kernel, ctx context.Context, ownerID, name string) *kernel.Action {
	t.Helper()
	owner, err := k.ReadUser(ctx, ownerID)
	if err != nil {
		t.Fatalf("read owner: %v", err)
	}
	a, err := k.ResolveAction(ctx, owner.Handle+"/"+name)
	if err != nil {
		t.Fatalf("resolve %s/%s: %v", owner.Handle, name, err)
	}
	return a
}

func newTestKernel(st kernel.Store) *kernel.Kernel {
	return newKernel(testConfig(), kernel.Dependencies{Store: st})
}

func newTestKernelWithScripts(st kernel.Store, exec kernel.ScriptExecutor) *kernel.Kernel {
	return newKernel(testConfig(), kernel.Dependencies{Store: st, Scripts: exec})
}

// testSigningKey is the identity every test kernel signs as. It is one key for the whole package,
// not a fresh one per config: a kernel rebuilt in a test — after a restart, a config change, a
// repricing — is the same kernel, and a peer's receipt naming it as the buyer must still verify
// there (P5). A test that needs a SECOND identity generates its own key, as a peer is anyway.
var theTestSigningKey = mustGenerateKey()

func testSigningKey() ed25519.PrivateKey { return theTestSigningKey }

func mustGenerateKey() ed25519.PrivateKey {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return priv
}

func setupUser(t *testing.T, st kernel.Store, handle string, balance int64) *kernel.Account {
	t.Helper()
	hash, err := kernel.HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	u := &kernel.Account{
		ID:           uuid.New().String(),
		Handle:       handle,
		PasswordHash: hash,
		Available:    balance,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// assertUserBalance checks a user's available and locked wallet fields for exact equality.
// Deliberately plain: tests that assert non-negativity or available+locked==sum invariants
// keep those checks inline, since those are the property under test.
func assertUserBalance(t *testing.T, st kernel.Store, userID string, wantAvail, wantLocked int64) {
	t.Helper()
	u, err := st.ReadUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("assertUserBalance: ReadUser: %v", err)
	}
	if u.Available != wantAvail {
		t.Errorf("user.available got %d, want %d", u.Available, wantAvail)
	}
	if u.Locked != wantLocked {
		t.Errorf("user.locked got %d, want %d", u.Locked, wantLocked)
	}
}

func setupAction(t *testing.T, st kernel.Store, ownerID, name string, price int64) *kernel.Action {
	t.Helper()
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Name:        name,
		Kind:        kernel.KindNative,
		Active:      true,
		Price:       price,
		Source:      "native",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// setupLocalAction is setupAction with local visibility, so a step parked for any local required
// caller can be completed (caller-scoped CanCall, §4). setupAction stays private for tests that
// assert a non-owner cannot call it.
func setupLocalAction(t *testing.T, st kernel.Store, ownerID, name string, price int64) *kernel.Action {
	t.Helper()
	a := setupAction(t, st, ownerID, name, price)
	a.Visibility = kernel.VisibilityLocal
	if err := st.UpdateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// setupSys creates the @sys superuser.
// Call this in any test that invokes RegisterRemoteKernel or ImportRemoteAction.
func setupSys(t *testing.T, _ *kernel.Kernel, st kernel.Store) *kernel.Account {
	t.Helper()
	u := setupUser(t, st, "sys", 0)
	// The operator account is named by config, which is how the rail's crossings find it (D23).
	if err := st.SetConfig(context.Background(), "superuser_handle", "sys"); err != nil {
		t.Fatalf("setupSys: %v", err)
	}
	return u
}

func setupProcess(t *testing.T, st kernel.Store, ownerID string, funds int64) *kernel.Process {
	t.Helper()
	// Use BeginRun (the single production path) to create the process+root trace atomically.
	// Create a minimal dummy action at the requested price so BeginRun has something to bind to.
	dummyAction := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  ownerID,
		Name:         "setup-" + uuid.New().String(),
		Kind:         kernel.KindNative,
		Active:       true,
		Price:        funds,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), dummyAction); err != nil {
		t.Fatalf("setupProcess: create action: %v", err)
	}
	p, _ := beginTestRun(t, st, ownerID, dummyAction)
	return p
}

// beginTestRun atomically creates a process and root trace funded with action.Price,
// mirroring what BeginRun does in production. The caller must have action.Price available.
func beginTestRun(t *testing.T, st kernel.Store, callerID string, action *kernel.Action) (*kernel.Process, *kernel.Trace) {
	t.Helper()
	ctx := context.Background()
	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: callerID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	tr := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: action.OwnerUserID,
		ActionID:      action.ID,
		CallerUserID:  callerID,
		CreatedAt:     time.Now().UTC(),
	}
	// Mirror beginRun: a remote-proxy root trace carries its dispatch key, which the settlement path
	// compares against a receipt's tx_id to tell a signed rejection from an execution (§6 P4), and
	// the record that froze every pricing input for it, which settlement reads and nothing else does.
	if action.Kind == kernel.KindRemoteProxy {
		key := uuid.New().String()
		tr.IdempotencyKey = &key
		econ := testEconomy()
		mp := action.Price
		if action.BasePrice != nil {
			mp = *action.BasePrice
		}
		var rbps int64
		if action.RemoteBPS != nil {
			rbps = *action.RemoteBPS
		}
		tr.DispatchJSON = kernel.DispatchRecordForTest(mp, action.Price, rbps, econ.ImportBPS, econ.Lottery, "")
	}
	if err := st.BeginRun(ctx, p, tr, callerID, action.Price, 0, 0); err != nil {
		t.Fatalf("beginTestRun: %v", err)
	}
	return p, tr
}

// setupOrphanTrace atomically creates a zero-price process+trace via BeginRun, mirroring
// the production entry point. The trace keeps the process open (no tx) for CreateStep/Call.
// actionOwnerID sets action_owner_id on the trace (used for non-owner authority checks).
func setupOrphanTrace(t *testing.T, st kernel.Store, ownerID, actionOwnerID, callerID string) (*kernel.Process, *kernel.Trace) {
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
		ActionOwnerID: actionOwnerID,
		CallerUserID:  callerID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := st.BeginRun(ctx, p, tr, ownerID, 0, 0, 0); err != nil {
		t.Fatalf("setupOrphanTrace: %v", err)
	}
	return p, tr
}

type fakeScriptExec struct {
	result string
	err    error
	calls  int
}

type panicScriptExec struct{}

func (p *panicScriptExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (p *panicScriptExec) Execute(_ context.Context, _ []byte, _ []byte, _ kernel.HostFunctions) ([]byte, error) {
	panic("simulated wasm panic")
}

func (f *fakeScriptExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (f *fakeScriptExec) Execute(_ context.Context, _ []byte, input []byte, _ kernel.HostFunctions) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.result != "" {
		return []byte(f.result), nil
	}
	return input, nil
}

// ---- User tests ----

func TestCreateNativeActionRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "owner", 0)
	_, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "native-attempt",
		Kind:        kernel.KindNative,
	})
	if err == nil {
		t.Error("expected error creating native action via CreateAction, got nil")
	}
}

func TestNativeActionNormalLifecycleRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "sys", 0)
	a, err := k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "native",
		Kind:        kernel.KindNative,
	})
	if err != nil {
		t.Fatal(err)
	}

	price := int64(1)
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Price: &price}); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("UpdateAction native error: got %v, want ErrUnauthorized", err)
	}
	if err := k.SetActive(ctx, owner.ID, a.ID, true); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("SetActive native error: got %v, want ErrUnauthorized", err)
	}
	if err := k.DeleteAction(ctx, owner.ID, a.ID); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("DeleteAction native error: got %v, want ErrUnauthorized", err)
	}
}

// b64Box is a test SecretBox that base64-encodes plaintext so the stored ciphertext does
// not literally contain the secret, letting tests assert auth_json is sealed at rest.
type b64Box struct{}

func (b64Box) Seal(_, plaintext string) (string, error) {
	return base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}
func (b64Box) Open(_, ciphertext string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(ciphertext)
	return string(b), err
}

// TestAuthCredentialsRequireSecretBox: storing or activating upstream auth without a SecretBox
// fails closed (§8 — encrypted at rest, no plaintext fallback); with a box, auth_json is sealed.
func TestAuthCredentialsRequireSecretBox(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	owner := setupUser(t, st, "svcowner", 0)
	req := kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "svc",
		Kind:         kernel.KindHTTP,
		Source:       "https://example.com",
		Description:  "svc",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Auth:         &kernel.AuthInput{Scheme: "bearer", Secrets: map[string]any{"token": "s3cret-token"}},
	}

	// No box: creation is refused.
	if _, err := newTestKernel(st).CreateAction(ctx, owner.ID, req); !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("create with no box: got %v, want ErrInvalidState", err)
	}

	// With a box: creation succeeds and auth_json is sealed (never the raw secret).
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	a, err := k.CreateAction(ctx, owner.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.AuthJSON == "" || strings.Contains(a.AuthJSON, "s3cret-token") {
		t.Errorf("auth_json not sealed: %q", a.AuthJSON)
	}

	// A fresh box-less kernel over the same store must refuse activation.
	if err := newTestKernel(st).SetActive(ctx, owner.ID, a.ID, true); !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("activate with no box: got %v, want ErrInvalidState", err)
	}
}

// TestActivateWasmActionFromArtifactOnly covers a wasm action registered with a
// pre-compiled artifact (e.g. @sys/tinygo/compile output via `action create
// --artifact`) and no stored TinyGo source: the artifact satisfies activation.
func TestActivateWasmActionFromArtifactOnly(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()
	owner := setupUser(t, st, "wasm-owner", 0)

	a, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "from-artifact",
		Kind:         kernel.KindWasm,
		Price:        0,
		Description:  "compiled out of band",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		WasmArtifact: base64.StdEncoding.EncodeToString([]byte("wat")),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if a.Source != "" {
		t.Fatalf("expected empty Source for artifact-only action, got %q", a.Source)
	}

	if err := k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatalf("activating artifact-only wasm action should succeed: %v", err)
	}
	got, err := k.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Active {
		t.Error("action should be active")
	}
	if got.ArtifactHash == "" {
		t.Error("artifact hash should be computed at activation")
	}
}

func TestActivateNativeActionBootstrapPath(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "sys", 0)
	a, err := k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "native",
		Kind:        kernel.KindNative,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Active {
		t.Fatal("registered native action should start inactive")
	}
	desc := "A native action"
	in := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string", "description": "x"}}}
	out := map[string]any{"type": "object"}
	if err := k.ActivateNativeAction(ctx, a.ID, desc, in, out, 0, ""); err != nil {
		t.Fatal(err)
	}
	active, err := k.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !active.Active {
		t.Fatal("ActivateNativeAction should activate native action")
	}
	if active.Description != desc {
		t.Errorf("description = %q, want %q", active.Description, desc)
	}
	if stats, err := k.ReadStats(ctx, a.ID); err != nil || stats == nil {
		t.Fatalf("ActivateNativeAction should initialize stats, stats=%v err=%v", stats, err)
	}
}

func TestPruneOrphanedNativeActions(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "sys", 0)

	// Only "keep" has a registered handler.
	k.RegisterNativeHandler("keep", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return map[string]any{}, nil
	})

	in := map[string]any{"type": "object"}
	out := map[string]any{"type": "object"}
	register := func(name string) string {
		a, err := k.RegisterNativeAction(ctx, kernel.CreateActionRequest{OwnerUserID: owner.ID, Name: name, Kind: kernel.KindNative})
		if err != nil {
			t.Fatal(err)
		}
		if err := k.ActivateNativeAction(ctx, a.ID, name+" native", in, out, 0, ""); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	keepID := register("keep")
	register("gone") // handler-less orphan (e.g. a native removed from the build)

	// A non-native (http) action with no handler must never be touched by the native prune.
	httpAct := &kernel.Action{
		ID: "http-weather-1", OwnerUserID: owner.ID, Name: "weather", Kind: kernel.KindHTTP,
		Active: true, Source: "http://x.example", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, httpAct); err != nil {
		t.Fatal(err)
	}

	pruned, err := k.PruneOrphanedNativeActions(ctx)
	if err != nil {
		t.Fatalf("PruneOrphanedNativeActions: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != "gone" {
		t.Fatalf("pruned = %v, want [gone]", pruned)
	}
	// "gone" is soft-deleted; "keep" survives active; "weather" (non-native) is untouched.
	if _, err := k.ReadActionByOwnerName(ctx, owner.ID, "gone"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("gone should be soft-deleted (ErrNotFound), got %v", err)
	}
	if a, err := k.ReadAction(ctx, keepID); err != nil || !a.Active {
		t.Errorf("keep should survive active, err=%v", err)
	}
	if _, err := k.ReadActionByOwnerName(ctx, owner.ID, "weather"); err != nil {
		t.Errorf("non-native weather must be untouched, got %v", err)
	}

	// Idempotent: a second prune removes nothing (soft-deleted rows drop out of ListNativeActions).
	if again, err := k.PruneOrphanedNativeActions(ctx); err != nil || len(again) != 0 {
		t.Errorf("second prune should be a no-op, got %v err=%v", again, err)
	}
}

// A native's row is created once and reconciled on every boot after that. What an operator reads at
// info is the creation and any boot that corrected something; the no-op reconciliations are debug,
// or a restart buries the ready line under the whole stdlib (§14).
func TestNativeBootLogsOnlyDurableChanges(t *testing.T) {
	in := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string", "description": "x"}}}
	out := map[string]any{"type": "object"}
	newIn := map[string]any{"type": "object", "properties": map[string]any{"y": map[string]any{"type": "integer", "description": "y"}}}

	setup := func(t *testing.T, level string) (*kernel.Kernel, string, string) {
		t.Helper()
		st := newTestStore(t)
		logPath := filepath.Join(t.TempDir(), "boot.log")
		logger, err := log.New(log.Config{Level: level, FilePath: logPath, Format: "json"})
		if err != nil {
			t.Fatal(err)
		}
		k := newKernel(testConfig(), kernel.Dependencies{Store: st, Logger: logger})
		owner := setupUser(t, st, "sys", 0)
		a, err := k.RegisterNativeAction(context.Background(), kernel.CreateActionRequest{
			OwnerUserID: owner.ID, Name: "native-log", Kind: kernel.KindNative,
		})
		if err != nil {
			t.Fatal(err)
		}
		return k, a.ID, logPath
	}
	count := func(t *testing.T, path, event string) int {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), event)
	}

	ctx := context.Background()
	k, id, logPath := setup(t, "info")
	if got := count(t, logPath, "action.registered_native"); got != 1 {
		t.Errorf("registering a native logged %d info lines, want 1", got)
	}
	// Activating an inactive row is a state change, so the first boot reports it.
	if err := k.ActivateNativeAction(ctx, id, "desc", in, out, 0, ""); err != nil {
		t.Fatal(err)
	}
	if got := count(t, logPath, "action.native_enabled"); got != 1 {
		t.Fatalf("first activation logged %d info lines, want 1", got)
	}
	if got := count(t, logPath, `"name":"native-log"`); got != 2 {
		t.Errorf("native log lines naming the action: %d, want 2", got)
	}
	// Every later boot passes the same spec and must say nothing at info.
	for i := 0; i < 3; i++ {
		if err := k.ActivateNativeAction(ctx, id, "desc", in, out, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := count(t, logPath, "action.native_enabled"); got != 1 {
		t.Errorf("unchanged reconciliation logged %d info lines, want 1", got)
	}
	// A build that changes a price or a contract has changed the row, and says so.
	if err := k.ActivateNativeAction(ctx, id, "desc", in, out, 7, ""); err != nil {
		t.Fatal(err)
	}
	if got := count(t, logPath, "action.native_enabled"); got != 2 {
		t.Errorf("price correction logged %d info lines, want 2", got)
	}
	if err := k.ActivateNativeAction(ctx, id, "desc", newIn, out, 7, ""); err != nil {
		t.Fatal(err)
	}
	if got := count(t, logPath, "action.native_enabled"); got != 3 {
		t.Errorf("schema correction logged %d info lines, want 3", got)
	}

	// The quiet reconciliation is still recorded, one level down.
	kd, idd, debugPath := setup(t, "debug")
	for i := 0; i < 2; i++ {
		if err := kd.ActivateNativeAction(ctx, idd, "desc", in, out, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := count(t, debugPath, "action.native_enabled"); got != 2 {
		t.Errorf("debug log has %d native_enabled lines, want 2", got)
	}
}

func TestActivateNativeActionReconcilesSchema(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "sys", 0)
	a, err := k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "native-reconcile",
		Kind:         kernel.KindNative,
		Description:  "old",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string", "description": "x"}}},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}

	newIn := map[string]any{"type": "object", "properties": map[string]any{"y": map[string]any{"type": "integer", "description": "y"}}}
	newOut := map[string]any{"type": "object", "properties": map[string]any{"z": map[string]any{"type": "string", "description": "z"}}}
	if err := k.ActivateNativeAction(ctx, a.ID, "new desc", newIn, newOut, 0, ""); err != nil {
		t.Fatalf("ActivateNativeAction: %v", err)
	}

	got, err := k.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "new desc" {
		t.Errorf("description = %q, want %q", got.Description, "new desc")
	}
	if props, _ := got.InputSchema["properties"].(map[string]any); props == nil || props["y"] == nil {
		t.Error("input schema not reconciled")
	}
	if props, _ := got.OutputSchema["properties"].(map[string]any); props == nil || props["z"] == nil {
		t.Error("output schema not reconciled")
	}
}

func TestActivateNativeActionRejectsSchemaWithoutDescriptions(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "sys", 0)
	a, err := k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "native-bad",
		Kind:        kernel.KindNative,
	})
	if err != nil {
		t.Fatal(err)
	}
	badIn := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"type": "string"}, // missing description
		},
	}
	if err := k.ActivateNativeAction(ctx, a.ID, "desc", badIn, nil, 0, ""); !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Fatalf("ActivateNativeAction with missing schema descriptions: got %v, want ErrSchemaViolation", err)
	}
}

func TestActivateNativeActionReconcilesPrice(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "sys", 0)
	in := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string", "description": "x"}}}
	out := map[string]any{"type": "object"}
	a, err := k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "native-price",
		Kind:        kernel.KindNative,
		Price:       0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.ActivateNativeAction(ctx, a.ID, "desc", in, out, 5, ""); err != nil {
		t.Fatalf("ActivateNativeAction: %v", err)
	}
	got, err := k.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Price != 5 {
		t.Errorf("price = %d, want 5", got.Price)
	}
}

func TestCreateUser(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "alice",
		Password: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle != "alice" {
		t.Errorf("handle: got %q, want %q", u.Handle, "alice")
	}
	if u.PasswordHash == "secret" {
		t.Error("password should be hashed")
	}
}

func TestNormalizeHandle(t *testing.T) {
	// NormalizeHandle trims whitespace only; it never strips a sigil — a "@"-prefixed input
	// survives so ValidateHandle can reject it (handles are bare, §3, §14).
	cases := []struct{ in, want string }{
		{"bob", "bob"},
		{"  carol  ", "carol"},
		{" @dave ", "@dave"},
		{"", ""},
		{"@", "@"},
	}
	for _, c := range cases {
		if got := kernel.NormalizeHandle(c.in); got != c.want {
			t.Errorf("NormalizeHandle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCreateUserRejectsSigilHandle(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	// A bare handle is stored as-is (whitespace trimmed).
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "  carol  ", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle != "carol" {
		t.Errorf("stored handle = %q, want %q", u.Handle, "carol")
	}
	if got, err := k.ReadUserByHandle(ctx, "carol"); err != nil || got == nil || got.ID != u.ID {
		t.Errorf("ReadUserByHandle(carol): got %v err %v", got, err)
	}

	// Handles are bare (§3, §14): a sigil-prefixed handle is rejected, not rewritten — as are a bare
	// "@" and an empty handle. A later "@carol" lookup must also miss (it is not the same user).
	for _, bad := range []string{"@carol", "@", "  ", ""} {
		if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: bad, Password: "secret"}); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("CreateUser(handle=%q): want ErrInvalidInput, got %v", bad, err)
		}
	}
	if got, _ := k.ReadUserByHandle(ctx, "@carol"); got != nil {
		t.Errorf("ReadUserByHandle(@carol) must miss, got %v", got)
	}
}

func TestLogin(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	_, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "bob",
		Password: "mypass",
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := loginToken(k, ctx, "bob", "mypass")
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Error("expected non-empty token")
	}

	subjectID, err := k.VerifyToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := k.ReadUserByHandle(ctx, "bob")
	if subjectID != u.ID {
		t.Errorf("token subject: got %q, want %q", subjectID, u.ID)
	}

	if _, err := loginToken(k, ctx, "bob", "wrong"); err == nil {
		t.Error("expected error for wrong password")
	}
}

func TestLoginRejectsRemotePeer(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := loginToken(k, ctx, "peer", "remote"); err == nil {
		t.Error("Login must reject remote kernel peers")
	}
}

// ---- Process tests ----

func TestStartAndEndProcess(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "owner", 1000)

	p := setupProcess(t, st, owner.ID, 500)

	// With BeginRun, the process holds available=0 (funds are in the root trace).
	proc, err := st.ReadProcess(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proc.Available != 0 || proc.Status != kernel.ProcessOpen {
		t.Errorf("process initial state: available=%d status=%s, want 0/open", proc.Available, proc.Status)
	}

	u, _ := st.ReadUser(ctx, owner.ID)
	if u.Available != 500 {
		t.Errorf("owner balance after funding: got %d, want 500 (500 locked)", u.Available)
	}

	if err := k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	u, _ = st.ReadUser(ctx, owner.ID)
	if u.Available != 1000 {
		t.Errorf("owner balance after end: got %d, want 1000", u.Available)
	}

	if err := k.EndProcess(ctx, owner.ID, p.ID); err == nil {
		t.Error("expected error ending closed process")
	}
}

func TestReadProcessUnauthorized(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "alice-proc", 500)
	bob := setupUser(t, st, "bob-proc", 0)

	p := setupProcess(t, st, alice.ID, 100)

	// Owner can read.
	if _, err := k.ReadProcess(ctx, alice.ID, p.ID); err != nil {
		t.Errorf("owner ReadProcess: %v", err)
	}
	// Non-owner must be rejected.
	if _, err := k.ReadProcess(ctx, bob.ID, p.ID); err == nil {
		t.Error("ReadProcess must reject non-owner")
	}
}

func TestProcessOwnerID(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "alice-owner-id", 500)
	p := setupProcess(t, st, alice.ID, 100)

	// Resolves the owner id unauthorized (no caller argument) — the required-caller view path
	// depends on this not enforcing process-read authority.
	if got := k.ProcessOwnerID(ctx, p.ID); got != alice.ID {
		t.Errorf("ProcessOwnerID: got %q, want %q", got, alice.ID)
	}
	// Unknown process → "".
	if got := k.ProcessOwnerID(ctx, "no-such-process"); got != "" {
		t.Errorf("ProcessOwnerID(unknown): got %q, want \"\"", got)
	}
}

func TestProcessAvailablePlusLockedInvariant(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 2000)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "svc",
		Kind:        kernel.KindWasm,
		Active:      true,
		Price:       100,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, tr := beginTestRun(t, st, alice.ID, a)

	checkInvariant := func(tag string, wantSum int64) {
		t.Helper()
		proc, err := st.ReadProcess(ctx, p.ID)
		if err != nil {
			t.Fatalf("%s: ReadProcess: %v", tag, err)
		}
		if proc.Locked < 0 {
			t.Errorf("%s: locked is negative: %d", tag, proc.Locked)
		}
		if proc.Available < 0 {
			t.Errorf("%s: available is negative: %d", tag, proc.Available)
		}
		if wantSum >= 0 && proc.Available+proc.Locked != wantSum {
			t.Errorf("%s: available(%d)+locked(%d)=%d, want %d",
				tag, proc.Available, proc.Locked, proc.Available+proc.Locked, wantSum)
		}
	}

	checkInvariant("initial", 100)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// After call, process auto-closes (quiescent). proc.available=0, proc.locked=0.
	checkInvariant("after auto-close", 0)
}

func TestUserLockedBalanceInvariant(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 1000)

	assertUserBalance(t, st, alice.ID, 1000, 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "svc",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, alice.ID, a)

	if _, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID,
		TargetUserID: alice.ID, ActionName: "svc", Args: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	// After call, process auto-closes (quiescent): remaining funds return to alice.
	// user.locked must reach 0 (no open processes), user.available stays non-negative.
	u, _ := st.ReadUser(ctx, alice.ID)
	if u.Locked != 0 {
		t.Errorf("after auto-close: user.locked got %d, want 0", u.Locked)
	}
	if u.Available < 0 {
		t.Errorf("after auto-close: user.available is negative: %d", u.Available)
	}
}

// ---- Stats tests ----

func TestIncrementalMean(t *testing.T) {
	var mean float64
	for i, v := range []float64{10, 20, 30} {
		mean = kernel.IncrementalMean(mean, int64(i), v)
	}
	if math.Abs(mean-20) > 1e-9 {
		t.Errorf("expected mean 20, got %f", mean)
	}
}

func TestUpdateStats(t *testing.T) {
	s := kernel.DefaultStats("action-1")
	now := time.Now()

	success := &kernel.Transaction{
		Status:    kernel.TxSuccess,
		Gross:     100,
		StartedAt: now.Add(-500 * time.Millisecond),
		EndedAt:   now,
	}
	kernel.UpdateStats(s, success, 0.5)

	if s.Uses != 1 {
		t.Errorf("uses: got %d, want 1", s.Uses)
	}
	if s.Successes != 1 || s.Failures != 0 {
		t.Errorf("successes=%d failures=%d, want 1/0", s.Successes, s.Failures)
	}
	if math.Abs(s.LatencyEstimate-0.5) > 1e-6 {
		t.Errorf("latency_estimate: got %f, want 0.5", s.LatencyEstimate)
	}

	kernel.UpdateStats(s, &kernel.Transaction{Status: kernel.TxFailure, StartedAt: now, EndedAt: now}, 0.2)
	if s.Uses != 2 || s.Failures != 1 {
		t.Errorf("after failure: uses=%d failures=%d, want 2/1", s.Uses, s.Failures)
	}
}

var _ = log.Default

func TestCreateHTTPActionRejectsSSRFURL(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "owner", 0)
	_, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "webhook",
		Kind:        kernel.KindHTTP,
		Source:      "http://169.254.169.254/latest/meta-data/",
	})
	if err == nil {
		t.Error("expected error creating HTTP action with SSRF URL")
	}
}

func TestRateTransactionUpdatesActionStats(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	buyer := setupUser(t, st, "rate-buyer", 1000)
	provider := setupUser(t, st, "rate-provider", 0)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: provider.ID,
		Name:        "rate-svc",
		Kind:        kernel.KindWasm,
		Active:      true,
		Visibility:  kernel.VisibilityPublic,
		Price:       0,
		Source:      "wat",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, buyer.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: buyer.ID, ExistingTraceID: tr.ID,
		TargetUserID: provider.ID, ActionName: "rate-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	const rating = 1.0
	if _, err := k.RateTransaction(ctx, buyer.ID, reply.TxID, rating, nil); err != nil {
		t.Fatalf("RateTransaction: %v", err)
	}

	stats, err := k.ReadStats(ctx, a.ID)
	if err != nil {
		t.Fatalf("ReadStats: %v", err)
	}
	if stats == nil {
		t.Fatal("expected stats after rating, got nil")
	}
	if stats.RatingCount != 1 {
		t.Errorf("RatingCount: got %d, want 1", stats.RatingCount)
	}
	if stats.RatingEstimate != rating {
		t.Errorf("RatingEstimate: got %f, want %f", stats.RatingEstimate, rating)
	}
}

// The rating domain is {0,1} (§11) and the kernel is its only owner: a non-binary value is
// refused before any authorization or lookup, so no caller can record one.
func TestRateTransactionRejectsNonBinaryValue(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	for _, v := range []float64{-1, 0.5, 2} {
		_, err := k.RateTransaction(ctx, "any-caller", "any-tx", v, nil)
		ke, ok := err.(*kernel.KernelError)
		if !ok || ke.Code != "invalid_input" {
			t.Errorf("RateTransaction(%v): expected invalid_input, got %v", v, err)
		}
	}
}

func TestRateTransactionAlreadyRatedRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	buyer := setupUser(t, st, "rerate-buyer", 500)
	provider := setupUser(t, st, "rerate-provider", 0)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: provider.ID,
		Name:        "rerate-svc",
		Kind:        kernel.KindWasm,
		Active:      true,
		Visibility:  kernel.VisibilityPublic,
		Price:       0,
		Source:      "wat",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, buyer.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: buyer.ID, ExistingTraceID: tr.ID,
		TargetUserID: provider.ID, ActionName: "rerate-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if _, err := k.RateTransaction(ctx, buyer.ID, reply.TxID, 1.0, nil); err != nil {
		t.Fatalf("first RateTransaction: %v", err)
	}
	_, err = k.RateTransaction(ctx, buyer.ID, reply.TxID, 0.0, nil)
	if err == nil {
		t.Fatal("expected error on second rating, got nil")
	}
	ke, ok := err.(*kernel.KernelError)
	if !ok || ke.Code != "invalid_input" {
		t.Errorf("expected invalid_input error, got %v", err)
	}
}

func TestRateTransactionOwnerCallingOwnActionCanRate(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "self-rater", 500)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: owner.ID,
		Name:        "self-svc",
		Kind:        kernel.KindWasm,
		Active:      true,
		Price:       0,
		Source:      "wat",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, owner.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: owner.ID, ExistingTraceID: tr.ID,
		TargetUserID: owner.ID, ActionName: "self-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	rating, err := k.RateTransaction(ctx, owner.ID, reply.TxID, 1.0, nil)
	if err != nil {
		t.Fatalf("expected owner to be able to rate own action output, got: %v", err)
	}
	if rating == nil {
		t.Fatal("expected non-nil Rating")
	}
}

// TestCallPreconditionOrderTraceBeforeAction verifies §4 ordering: parent trace existence
// (step 3) must be checked before action existence (step 5).
// C == P, invalid ParentTraceID, non-existent action → must return ErrInvalidInput for
// the trace, not ErrNotFound for the action.
func TestCallPreconditionOrderTraceBeforeAction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "ptrace-owner", 100)
	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:      owner.ID,
		ParentTraceID: "nonexistent-trace-id",
		TargetUserID:  owner.ID,
		ActionName:    "no-such-action",
		Args:          map[string]any{},
	})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing parent trace (step 3 before step 5), got: %v", err)
	}
}

// TestCallPreconditionOrderActionAfterValidTrace verifies §4 ordering: with a valid parent
// trace, a missing action returns ErrNotFound (step 5).
func TestCallPreconditionOrderActionAfterValidTrace(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "ptrace-owner2", 100)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "dummy",
		Kind: kernel.KindNative, Active: true, Price: 0,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatalf("create action: %v", err)
	}
	_, tr := beginTestRun(t, st, owner.ID, a)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:      owner.ID,
		ParentTraceID: tr.ID,
		TargetUserID:  owner.ID,
		ActionName:    "no-such-action",
		Args:          map[string]any{},
	})
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing action (after valid trace), got: %v", err)
	}
}

// ---- Deposit enforcement tests ----

func TestDepositNonSuperuserRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	su := setupUser(t, st, "sys", 0)
	regular := setupUser(t, st, "regular", 0)
	recipient := setupUser(t, st, "recipient", 0)

	// Superuser can deposit.
	if _, err := k.Deposit(ctx, su.ID, recipient.ID, 100, "ok", newRef()); err != nil {
		t.Fatalf("superuser deposit: %v", err)
	}
	// Regular user cannot deposit.
	if _, err := k.Deposit(ctx, regular.ID, recipient.ID, 100, "bad", newRef()); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for non-superuser deposit, got %v", err)
	}
}

func TestRenameUser(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	su := setupUser(t, st, "sys", 0)
	regular := setupUser(t, st, "regular", 0)
	bob := setupUser(t, st, "bob", 0)

	// Non-superuser cannot rename.
	if _, err := k.RenameUser(ctx, regular.ID, bob.ID, "x"); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("non-superuser rename: got %v, want ErrUnauthorized", err)
	}
	// The superuser's own handle cannot be renamed (bound to config.superuser_handle).
	if _, err := k.RenameUser(ctx, su.ID, su.ID, "root"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("rename superuser: got %v, want ErrInvalidInput", err)
	}
	// A rename onto a handle another account already holds is rejected.
	if _, err := k.RenameUser(ctx, su.ID, bob.ID, "regular"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("rename onto taken handle: got %v, want ErrInvalidInput", err)
	}

	// Superuser renames bob aside, vacating @bob.
	out, err := k.RenameUser(ctx, su.ID, bob.ID, "bob-retired")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if out.Handle != "bob-retired" {
		t.Errorf("returned handle: got %q, want @bob-retired", out.Handle)
	}
	if got, err := k.ReadUserByHandle(ctx, "bob-retired"); err != nil || got.ID != bob.ID {
		t.Errorf("new handle does not resolve to bob: %v", err)
	}
	// The freed @bob is reusable by a fresh account, which inherits nothing of bob's identity.
	fresh, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "bob", Password: "password"})
	if err != nil {
		t.Fatalf("reuse freed handle: %v", err)
	}
	if fresh.ID == bob.ID {
		t.Errorf("reused handle must be a distinct account, got same id %s", fresh.ID)
	}
}

func TestAdjustmentExternalKeyIdempotent(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	su := setupUser(t, st, "sys", 0)
	recipient := setupUser(t, st, "recipient", 0)

	balance := func() int64 {
		u, err := k.ReadUser(ctx, recipient.ID)
		if err != nil {
			t.Fatalf("read user: %v", err)
		}
		return u.Available
	}

	// Same external_key credits once; the replay returns the original record.
	first, err := k.Deposit(ctx, su.ID, recipient.ID, 100, "wire", "wire-1")
	if err != nil {
		t.Fatalf("first deposit: %v", err)
	}
	replay, err := k.Deposit(ctx, su.ID, recipient.ID, 100, "wire", "wire-1")
	if err != nil {
		t.Fatalf("replay deposit: %v", err)
	}
	if replay.ID != first.ID || replay.Amount != first.Amount {
		t.Errorf("replay returned a new record: got %+v, want id=%s amount=%d", replay, first.ID, first.Amount)
	}
	if balance() != 100 {
		t.Errorf("balance after same-key replay: got %d, want 100 (credited once)", balance())
	}

	// Distinct keys each apply.
	if _, err := k.Deposit(ctx, su.ID, recipient.ID, 50, "", "wire-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Deposit(ctx, su.ID, recipient.ID, 50, "", "wire-3"); err != nil {
		t.Fatal(err)
	}
	if balance() != 200 {
		t.Errorf("balance after two distinct keys: got %d, want 200", balance())
	}

	// Empty key never dedups: both apply.
	if _, err := k.Deposit(ctx, su.ID, recipient.ID, 10, "", newRef()); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Deposit(ctx, su.ID, recipient.ID, 10, "", newRef()); err != nil {
		t.Fatal(err)
	}
	if balance() != 220 {
		t.Errorf("balance after two empty-key deposits: got %d, want 220", balance())
	}

	// A withdrawal carries the caller's own id, so a reply lost in transit is safe to ask for
	// again: the same id returns the same row and moves nothing, even after the first debit has
	// taken the balance below the amount (U51).
	id := uuid.NewString()
	wd, err := k.Withdraw(ctx, recipient.ID, id, 200, "redeem")
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if balance() != 20 {
		t.Fatalf("balance after withdraw: got %d, want 20", balance())
	}
	again, err := k.Withdraw(ctx, recipient.ID, id, 200, "redeem")
	if err != nil {
		t.Fatalf("replaying a withdrawal must not fail: %v", err)
	}
	if again.ID != wd.ID {
		t.Errorf("replay made a second withdrawal: got %s, want %s", again.ID, wd.ID)
	}
	if balance() != 20 {
		t.Errorf("balance after replay: got %d, want 20 (debited once)", balance())
	}
	// The same id on other terms is a different intention, and is refused rather than guessed at.
	if _, err := k.Withdraw(ctx, recipient.ID, id, 5, "redeem"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("same id, other amount: want ErrInvalidInput, got %v", err)
	}
	if _, err := k.Withdraw(ctx, recipient.ID, uuid.NewString(), 999, ""); !errors.Is(err, kernel.ErrInsufficientFunds) {
		t.Errorf("withdrawing more than the balance: want ErrInsufficientFunds, got %v", err)
	}
}

func TestTransfer(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 100)
	bob := setupUser(t, st, "bob", 0)

	// Happy path: debit sender, credit recipient, one ledger entry from→to.
	e, err := k.Transfer(ctx, alice.ID, bob.ID, 30, "gift", "")
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if e.FromUserID != alice.ID || e.ToUserID != bob.ID || e.OperatorUserID != alice.ID || e.Amount != 30 {
		t.Errorf("ledger entry: got %+v, want from=%s to=%s operator=%s amount=30", e, alice.ID, bob.ID, alice.ID)
	}
	assertUserBalance(t, st, alice.ID, 70, 0)
	assertUserBalance(t, st, bob.ID, 30, 0)

	// Insufficient funds: rejected, balances unchanged.
	if _, err := k.Transfer(ctx, alice.ID, bob.ID, 1000, "", ""); !errors.Is(err, kernel.ErrInsufficientFunds) {
		t.Errorf("over-balance transfer: got %v, want ErrInsufficientFunds", err)
	}
	assertUserBalance(t, st, alice.ID, 70, 0)
	assertUserBalance(t, st, bob.ID, 30, 0)

	// Non-positive amount and self-transfer rejected.
	if _, err := k.Transfer(ctx, alice.ID, bob.ID, 0, "", ""); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("zero amount: got %v, want ErrInvalidInput", err)
	}
	if _, err := k.Transfer(ctx, alice.ID, alice.ID, 10, "", ""); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("self-transfer: got %v, want ErrInvalidInput", err)
	}

	// Peer/proxy recipient (kernel_public_key set) rejected.
	if err := st.UpsertKernel(ctx, "cGVlci1rZXk", "peer", "", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	peer := &kernel.Account{
		ID: uuid.New().String(), KernelPublicKey: "cGVlci1rZXk", Available: 0,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateUser(ctx, peer); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Transfer(ctx, alice.ID, peer.ID, 10, "", ""); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("transfer to peer: got %v, want ErrInvalidInput", err)
	}

	// Suspended recipient rejected; suspended caller rejected (ErrUnauthenticated).
	carol := setupUser(t, st, "carol", 0)
	if err := st.SuspendUser(ctx, carol.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Transfer(ctx, alice.ID, carol.ID, 10, "", ""); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("transfer to suspended recipient: got %v, want ErrInvalidInput", err)
	}
	if err := st.SuspendUser(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Transfer(ctx, alice.ID, bob.ID, 10, "", ""); !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Errorf("suspended caller: got %v, want ErrUnauthenticated", err)
	}
}

func TestTransferIdempotent(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 100)
	bob := setupUser(t, st, "bob", 0)

	first, err := k.Transfer(ctx, alice.ID, bob.ID, 40, "invoice", "inv-1")
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	replay, err := k.Transfer(ctx, alice.ID, bob.ID, 40, "invoice", "inv-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.ID != first.ID {
		t.Errorf("replay returned a new record: got %s, want %s", replay.ID, first.ID)
	}
	// Moved once, not twice.
	assertUserBalance(t, st, alice.ID, 60, 0)
	assertUserBalance(t, st, bob.ID, 40, 0)
}

// An idempotency token a client chose names that client's own movement and nothing else. The kernel
// prefixes every key it mints; the caller's was the one name in that column nobody owned, so a
// client could hand back a key it had merely read and be told money moved that never did, or occupy
// a name the rail would later need for a real payment.
func TestATransferKeyNamesOnlyItsOwnCallersMovement(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	su := setupUser(t, st, "sys", 0)
	alice := setupUser(t, st, "alice", 100)
	bob := setupUser(t, st, "bob", 0)
	mallory := setupUser(t, st, "mallory", 100)

	// A payment the kernel booked under its own key.
	if _, err := k.Deposit(ctx, su.ID, bob.ID, 50, "", "pay-1"); err != nil {
		t.Fatal(err)
	}
	entries, err := st.ListLedgerByUser(ctx, bob.ID, 0, 0)
	if err != nil || len(entries) == 0 {
		t.Fatalf("ledger: %d %v", len(entries), err)
	}
	kernelKey := entries[0].ExternalKey
	if kernelKey == "" {
		t.Fatal("the kernel's own entry carries no key to try")
	}

	// A stranger naming it is not answered with it, and moves nothing.
	before, _ := st.ReadUser(ctx, mallory.ID)
	if _, err := k.Transfer(ctx, mallory.ID, bob.ID, 10, "", kernelKey); err != nil {
		t.Fatalf("a caller's key lives in the caller's namespace, so this is an ordinary transfer: %v", err)
	}
	after, _ := st.ReadUser(ctx, mallory.ID)
	if after.Available != before.Available-10 {
		t.Errorf("the transfer did not move: %d then %d", before.Available, after.Available)
	}

	// Two callers may choose one word without naming each other's movement.
	if _, err := k.Transfer(ctx, alice.ID, bob.ID, 20, "", "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Transfer(ctx, mallory.ID, bob.ID, 30, "", "shared"); err != nil {
		t.Fatalf("another caller's key must not be taken: %v", err)
	}
	assertUserBalance(t, st, alice.ID, 80, 0)
	assertUserBalance(t, st, mallory.ID, 60, 0)

	// One caller reusing their own key on other terms is refused, not answered with the old entry.
	if _, err := k.Transfer(ctx, alice.ID, bob.ID, 25, "", "shared"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("the same key on other terms must be refused, got %v", err)
	}
	assertUserBalance(t, st, alice.ID, 80, 0)
}

func TestListLedger(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	su := setupUser(t, st, "sys", 0)
	alice := setupUser(t, st, "alice", 0)
	bob := setupUser(t, st, "bob", 0)

	if _, err := k.Deposit(ctx, su.ID, alice.ID, 100, "", newRef()); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Transfer(ctx, alice.ID, bob.ID, 30, "", ""); err != nil {
		t.Fatal(err)
	}

	// Alice sees her deposit (to) and her outbound transfer (from): two entries.
	aliceLedger, err := k.ListLedger(ctx, alice.ID, 50, 0)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	if len(aliceLedger) != 2 {
		t.Fatalf("alice ledger: got %d entries, want 2", len(aliceLedger))
	}
	// Bob sees only the inbound transfer (to): one entry.
	bobLedger, err := k.ListLedger(ctx, bob.ID, 50, 0)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	if len(bobLedger) != 1 || bobLedger[0].ToUserID != bob.ID || bobLedger[0].FromUserID != alice.ID {
		t.Errorf("bob ledger: got %+v, want one from-alice→to-bob entry", bobLedger)
	}
}

// ---- Receipt tests ----

func TestReceiptCreatedWithCall(t *testing.T) {
	st := newTestStore(t)
	su := setupUser(t, st, "sys", 0)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	caller := setupUser(t, st, "rcpt-caller", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: caller.ID, Name: "rcpt-svc",
		Kind: kernel.KindWasm, Active: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, caller.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: caller.ID, ActionName: "rcpt-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	r, err := st.ReadReceiptByTxID(ctx, reply.TxID)
	if err != nil {
		t.Fatalf("ReadReceiptByTxID: %v", err)
	}
	if r.TxID != reply.TxID {
		t.Errorf("receipt.TxID: got %q, want %q", r.TxID, reply.TxID)
	}
	if r.IssuerUserID != su.ID {
		t.Errorf("receipt.IssuerUserID: got %q, want %q", r.IssuerUserID, su.ID)
	}
}

func TestReceiptCreatedWithFailedCall(t *testing.T) {
	st := newTestStore(t)
	su := setupUser(t, st, "sys", 0)
	k := newTestKernelWithScripts(st, &fakeScriptExec{err: kernel.ErrExecutionFailed.Wrap("boom")})
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	owner := setupUser(t, st, "fail-owner", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "fail-svc",
		Kind: kernel.KindWasm, Active: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, tr := beginTestRun(t, st, owner.ID, a)
	reply, _ := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: owner.ID, ExistingTraceID: tr.ID,
		TargetUserID: owner.ID, ActionName: "fail-svc", Args: map[string]any{},
	})

	// Find the failed transaction.
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	if len(txs) == 0 {
		t.Fatal("expected a transaction for failed call")
	}
	_ = reply

	var failTxID string
	for _, tx := range txs {
		if tx.Status == kernel.TxFailure {
			failTxID = tx.ID
			break
		}
	}
	if failTxID == "" {
		t.Fatal("expected a failed transaction")
	}

	r, err := st.ReadReceiptByTxID(ctx, failTxID)
	if err != nil {
		t.Fatalf("ReadReceiptByTxID for failed call: %v", err)
	}
	if r.Status != kernel.TxFailure {
		t.Errorf("receipt.Status: got %q, want %q", r.Status, kernel.TxFailure)
	}
	// reply_hash must be the JCS hash of the literal JSON null stored in the transaction.
	// JCS of null serializes to the 4-byte literal "null".
	nullHash := sha256.Sum256([]byte("null"))
	wantReplyHash := fmt.Sprintf("%x", nullHash)
	if r.ReplyHash != wantReplyHash {
		t.Errorf("receipt.ReplyHash for failed call: got %q, want %q (hash of JSON null)", r.ReplyHash, wantReplyHash)
	}
}

func TestReadTransactionPartyAccess(t *testing.T) {
	st := newTestStore(t)
	su := setupUser(t, st, "sys", 0)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	owner := setupUser(t, st, "provider", 500) // seller
	caller := setupUser(t, st, "buyer", 500)   // buyer
	other := setupUser(t, st, "other", 500)    // non-party
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "pvd-svc",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 10, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, caller.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: owner.ID, ActionName: "pvd-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Process owner, call caller (same here), and action owner may all read the full transaction.
	if _, err := k.ReadTransaction(ctx, caller.ID, reply.TxID); err != nil {
		t.Errorf("process owner ReadTransaction: %v", err)
	}
	if _, err := k.ReadTransaction(ctx, owner.ID, reply.TxID); err != nil {
		t.Errorf("action owner ReadTransaction: %v", err)
	}
	if _, err := k.ReadTransaction(ctx, su.ID, reply.TxID); err != nil {
		t.Errorf("superuser ReadTransaction: %v", err)
	}
	// A non-party gets ErrNotFound; existence is not leaked.
	if _, err := k.ReadTransaction(ctx, other.ID, reply.TxID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("non-party ReadTransaction: got %v, want ErrNotFound", err)
	}
}

func TestCallRequiresReceiptSigningBeforeExecution(t *testing.T) {
	st := newTestStore(t)
	exec := &fakeScriptExec{result: `{"ok":true}`}
	k := newTestKernelWithScripts(st, exec)
	k.SetSigningKey(nil, "issuer-id")
	ctx := context.Background()

	owner := setupUser(t, st, "no-receipt-owner", 100)
	a := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "no-receipt",
		Kind:         kernel.KindWasm,
		Active:       true,
		Price:        10,
		Source:       "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	_, err := k.Run(ctx, kernel.RunRequest{CallerID: owner.ID, ActionRef: "no-receipt-owner/no-receipt", Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}
	if exec.calls != 0 {
		t.Fatalf("action executed despite missing receipt signing: %d calls", exec.calls)
	}
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{OwnerUserID: owner.ID, Limit: 10})
	if len(txs) != 0 {
		t.Fatalf("transaction created despite missing receipt signing: %d", len(txs))
	}
}

func TestManifestSigningRequiresConfiguredKey(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	k.SetSigningKey(nil, "issuer-id")
	ctx := context.Background()

	owner := setupUser(t, st, "manifest-owner", 0)
	a := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "manifest",
		Kind:         kernel.KindHTTP,
		Active:       true,
		Visibility:   kernel.VisibilityPublic,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Source:       "https://example.com/call",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	_, err := k.GetActionManifest(ctx, a.ID)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState without signing key, got %v", err)
	}
}

// ---- Rating record tests ----

func TestRatingRecordCreated(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	buyer := setupUser(t, st, "rr-buyer", 500)
	provider := setupUser(t, st, "rr-provider", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "rr-svc",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, buyer.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: buyer.ID, ExistingTraceID: tr.ID,
		TargetUserID: provider.ID, ActionName: "rr-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if _, err := k.RateTransaction(ctx, buyer.ID, reply.TxID, 1.0, nil); err != nil {
		t.Fatalf("RateTransaction: %v", err)
	}

	// Rating is in the ratings table, not on the transaction row.
	tx, _ := st.ReadTransaction(ctx, reply.TxID)
	if tx == nil {
		t.Fatal("expected transaction")
	}

	rating, err := st.ReadRatingByTxID(ctx, reply.TxID)
	if err != nil {
		t.Fatalf("ReadRatingByTxID: %v", err)
	}
	if rating.Rating != 1.0 {
		t.Errorf("rating.Rating: got %f, want 1.0", rating.Rating)
	}
	if rating.RaterUserID != buyer.ID {
		t.Errorf("rating.RaterUserID: got %q, want %q", rating.RaterUserID, buyer.ID)
	}
}

func TestRatingDuplicateRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	buyer := setupUser(t, st, "dup-buyer", 500)
	provider := setupUser(t, st, "dup-provider", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "dup-svc",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, buyer.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: buyer.ID, ExistingTraceID: tr.ID,
		TargetUserID: provider.ID, ActionName: "dup-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if _, err := k.RateTransaction(ctx, buyer.ID, reply.TxID, 1.0, nil); err != nil {
		t.Fatalf("first RateTransaction: %v", err)
	}
	_, err = k.RateTransaction(ctx, buyer.ID, reply.TxID, 0.0, nil)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for duplicate rating, got %v", err)
	}
	// The rule is "one rating per transaction" (§11); the message says that, not the store
	// operation whose unique index caught it — internals never reach a user-facing error (§14).
	if msg := err.Error(); !strings.Contains(msg, "already rated") || strings.Contains(msg, "insert rating") {
		t.Errorf("duplicate-rating message = %q, want the rule, not the store operation", msg)
	}
}

func TestTransactionViewEmbeddedRating(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	buyer := setupUser(t, st, "tv-buyer", 500)
	provider := setupUser(t, st, "tv-provider", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "tv-svc",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	_, tr := beginTestRun(t, st, buyer.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: buyer.ID, ExistingTraceID: tr.ID,
		TargetUserID: provider.ID, ActionName: "tv-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Before rating: ReadTransaction returns a view with Rating == nil.
	viewBefore, err := k.ReadTransaction(ctx, buyer.ID, reply.TxID)
	if err != nil {
		t.Fatalf("ReadTransaction before rating: %v", err)
	}
	if viewBefore.Rating != nil {
		t.Errorf("expected nil Rating before rating, got %+v", viewBefore.Rating)
	}

	// After rating with note: view embeds value and note.
	note := "worked perfectly"
	if _, err := k.RateTransaction(ctx, buyer.ID, reply.TxID, 1.0, &note); err != nil {
		t.Fatalf("RateTransaction: %v", err)
	}

	viewAfter, err := k.ReadTransaction(ctx, buyer.ID, reply.TxID)
	if err != nil {
		t.Fatalf("ReadTransaction after rating: %v", err)
	}
	if viewAfter.Rating == nil {
		t.Fatal("expected non-nil Rating after rating")
	}
	if viewAfter.Rating.Value != 1.0 {
		t.Errorf("Rating.Value: got %f, want 1.0", viewAfter.Rating.Value)
	}
	if viewAfter.Rating.Note == nil || *viewAfter.Rating.Note != note {
		t.Errorf("Rating.Note: got %v, want %q", viewAfter.Rating.Note, note)
	}

	// ListTransactions also returns the embedded rating.
	views, err := k.ListTransactions(ctx, buyer.ID, kernel.TxFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(views) == 0 {
		t.Fatal("expected at least one transaction")
	}
	found := false
	for _, v := range views {
		if v.ID == reply.TxID {
			found = true
			if v.Rating == nil {
				t.Error("list view: expected non-nil Rating")
			} else if v.Rating.Value != 1.0 {
				t.Errorf("list view Rating.Value: got %f, want 1.0", v.Rating.Value)
			}
		}
	}
	if !found {
		t.Error("rated transaction not found in list")
	}
}

// TestTransactionViewAttachesRating proves transaction listings return the canonical
// TransactionView shape: nil rating before, embedded rating after (§14).
func TestTransactionViewAttachesRating(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice", 1000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "svc",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	reply, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice/svc", Args: map[string]any{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	views, err := k.ListTransactions(ctx, alice.ID, kernel.TxFilter{Limit: 50})
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("want 1 view, got %d", len(views))
	}
	if views[0].Rating != nil {
		t.Errorf("unrated tx should have nil rating, got %+v", views[0].Rating)
	}

	if _, err := k.RateTransaction(ctx, alice.ID, reply.TxID, 1, nil); err != nil {
		t.Fatalf("RateTransaction: %v", err)
	}
	views, err = k.ListTransactions(ctx, alice.ID, kernel.TxFilter{Limit: 50})
	if err != nil {
		t.Fatalf("ListTransactions after rating: %v", err)
	}
	if views[0].Rating == nil || views[0].Rating.Value != 1 {
		t.Errorf("rated tx view should carry rating value 1, got %+v", views[0].Rating)
	}
}

// ---- Zero-credit process tests ----

func TestZeroCreditProcess(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	owner := setupUser(t, st, "zero-owner", 100)

	p := setupProcess(t, st, owner.ID, 0)

	u, _ := st.ReadUser(ctx, owner.ID)
	if u.Available != 100 {
		t.Errorf("user.available after 0-fund process: got %d, want 100", u.Available)
	}
	if p.Available != 0 {
		t.Errorf("process.available: got %d, want 0", p.Available)
	}
}

// ---- Action soft-delete tests ----

func TestDeleteActionSoftDelete(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "sd-owner", 0)

	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: owner.ID,
		Name:        "sd-svc",
		Kind:        kernel.KindHTTP,
		Active:      true,
		Price:       0,
		Source:      "http://example.com",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	if err := k.DeleteAction(ctx, owner.ID, a.ID); err != nil {
		t.Fatalf("DeleteAction: %v", err)
	}

	// Action should no longer be readable.
	_, err := k.ReadAction(ctx, a.ID)
	if err == nil {
		t.Error("expected error reading deleted action, got nil")
	}

	// Action should not appear in listings.
	list, _ := st.ListAllActions(ctx, 100, 0)
	for _, listed := range list {
		if listed.ID == a.ID {
			t.Error("deleted action should not appear in ListAllActions")
		}
	}
}

// decodeHTTPSource unmarshals an action's stored source as an HTTPSource.
func decodeHTTPSource(t *testing.T, src string) kernel.HTTPSource {
	t.Helper()
	var s kernel.HTTPSource
	if err := json.Unmarshal([]byte(src), &s); err != nil {
		t.Fatalf("source is not HTTPSource JSON: %v (%s)", err, src)
	}
	return s
}

func TestCreateHTTPActionBuildsStructuredSource(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "hsrc-owner", 0)

	a, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "weather",
		Kind:        kernel.KindHTTP,
		Source:      "https://api.example.com/weather/{city}",
		Method:      "get",
		Params:      []kernel.HTTPParam{{Name: "city", In: "path"}},
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	s := decodeHTTPSource(t, a.Source)
	if s.Type != "http" {
		t.Errorf("type: got %q, want http", s.Type)
	}
	if s.Method != "GET" {
		t.Errorf("method: got %q, want GET (uppercased)", s.Method)
	}
	if s.BaseURL != "https://api.example.com" {
		t.Errorf("base_url: got %q", s.BaseURL)
	}
	if s.Path != "/weather/{city}" {
		t.Errorf("path: got %q, want /weather/{city}", s.Path)
	}
	if len(s.Params) != 1 || s.Params[0].In != "path" {
		t.Errorf("params: got %+v", s.Params)
	}
}

func TestCreateHTTPActionDefaultsToPOST(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "hpost-owner", 0)

	a, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "hook", Kind: kernel.KindHTTP,
		Source: "https://api.example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if m := decodeHTTPSource(t, a.Source).Method; m != "POST" {
		t.Errorf("default method: got %q, want POST", m)
	}
}

func TestCreateHTTPActionRejectsBadMethodAndParam(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "hbad-owner", 0)

	_, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "bad-method", Kind: kernel.KindHTTP,
		Source: "https://api.example.com", Method: "FETCH",
	})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("bad method: got %v, want ErrInvalidInput", err)
	}

	_, err = k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "bad-param", Kind: kernel.KindHTTP,
		Source: "https://api.example.com", Params: []kernel.HTTPParam{{Name: "x", In: "header"}},
	})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("bad param 'in': got %v, want ErrInvalidInput", err)
	}
}

func TestUpdateHTTPActionMergesSource(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "hmerge-owner", 0)

	a, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "svc", Kind: kernel.KindHTTP,
		Source: "https://api.example.com/v1", Method: "GET",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	// Change only the method; base_url/path must be preserved.
	newMethod := "POST"
	upd, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Method: &newMethod})
	if err != nil {
		t.Fatalf("UpdateAction: %v", err)
	}
	s := decodeHTTPSource(t, upd.Source)
	if s.Method != "POST" {
		t.Errorf("method: got %q, want POST", s.Method)
	}
	if s.BaseURL != "https://api.example.com" || s.Path != "/v1" {
		t.Errorf("base/path not preserved: base=%q path=%q", s.BaseURL, s.Path)
	}
	if upd.Active {
		t.Error("source change must deactivate the action")
	}
}

func TestSetActiveRequiresDescription(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "desc-owner", 0)

	a, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "nodesc",
		Kind:         kernel.KindHTTP,
		Source:       "http://example.com",
		Price:        0,
		Description:  "",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = k.SetActive(ctx, owner.ID, a.ID, true)
	if err == nil {
		t.Fatal("expected error activating action with empty description")
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Code != "invalid_state" {
		t.Errorf("want ErrInvalidState, got: %v", err)
	}
}

func TestActivationRejectsPropertyMissingDescription(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "desc-prop-owner", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "nodesc-prop",
		Kind: kernel.KindHTTP, Source: "http://example.com", Active: false,
		Description: "My service",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"}, // no description
			},
		},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	if err := k.SetActive(ctx, owner.ID, a.ID, true); err == nil {
		t.Error("expected error activating action with schema property missing description")
	}

	// Adding a description to the property allows activation.
	a.InputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "the search query"},
		},
	}
	_ = st.UpdateAction(ctx, a)
	if err := k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Errorf("SetActive with described property: %v", err)
	}
}

func TestSetActiveValidatesWasm(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	owner := setupUser(t, st, "alice", 0)

	// WASM action with placeholder source.
	a, err := newTestKernel(st).CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "wasm-act",
		Kind:         kernel.KindWasm,
		Source:       "invalid-wasm",
		Price:        0,
		Description:  "test",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Kernel with no script executor: activation must be rejected.
	kNoScripts := newTestKernel(st)
	if err := kNoScripts.SetActive(ctx, owner.ID, a.ID, true); err == nil {
		t.Error("expected error activating wasm action without script executor")
	}
}

func TestSetActiveValidatesSchemas(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	owner := setupUser(t, st, "alice", 0)

	// Create action via store directly with a nil schema to bypass CreateAction validation.
	a := &kernel.Action{
		ID: "schema-test", OwnerUserID: owner.ID, Name: "no-schema",
		Kind: kernel.KindHTTP, Source: "http://example.com", Active: false, Price: 0,
	}
	_ = st.CreateAction(ctx, a)

	k := newTestKernel(st)
	if err := k.SetActive(ctx, owner.ID, a.ID, true); err == nil {
		t.Error("expected error activating action with nil input schema")
	}
}

// minOpenAPISpec is a valid minimal OpenAPI 3.x spec with one GET /hello operation.
// servers[0].url is a public hostname so activation SSRF checks pass without AllowLocalSources.
// The operation includes a query parameter to satisfy the input-contract requirement.
const minOpenAPISpec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

// ---- #12 supervision authority tests ----

func TestCreateActionSubjectMismatchRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	userA := setupUser(t, st, "user-a", 0)
	userB := setupUser(t, st, "user-b", 0)

	_, err := k.CreateAction(ctx, userA.ID, kernel.CreateActionRequest{
		OwnerUserID: userB.ID,
		Name:        "action",
		Kind:        kernel.KindHTTP,
		Source:      "http://example.com",
	})
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized when subject != owner, got %v", err)
	}
}

func newTestKernelWithHTTP(st kernel.Store, http kernel.HTTPExecutor) *kernel.Kernel {
	return newKernel(testConfig(), kernel.Dependencies{Store: st, HTTP: http})
}

// ---- Remote proxy execution test ----

// fakeFederationHTTP implements HTTPExecutor and FederationExecutor for Call() tests.
// fakeSuccessHTTP is a minimal HTTPExecutor that returns an empty result for any Execute call.
type fakeSuccessHTTP struct{}

func (f *fakeSuccessHTTP) Execute(_ context.Context, _ *kernel.Action, _ map[string]any, _, _ string) (map[string]any, error) {
	return map[string]any{}, nil
}

type fakeFederationHTTP struct {
	result          map[string]any
	receiptJSON     string
	httpStatus      int  // 0 → 200 (kept so existing tests read as success)
	notDispatched   bool // simulate a provably-never-sent dispatch (§13)
	resolveManifest *kernel.ActionManifest
	resolveUserID   string
	resolveHandle   string
	resolveErr      error
	// The rail identity a resolve reply carries: where the peer is paid, and its own proof of it.
	resolveRailAddress string
	resolveRailProof   string
	// When rejectSignKey is set, ExecuteFederation returns a signed zero-charge rejection receipt —
	// naming no transaction and naming the request it refuses, exactly how a real serving kernel
	// refuses a call (§13, P5) — so a test can drive an ACTUAL rejection settlement.
	rejectSignKey  ed25519.PrivateKey
	rejectActionID string
	rejectArgsHash string
	// signKey and buyerKey make the fake sign as the peer does: every receipt it returns names the
	// request it answers — the key the kernel minted at dispatch, and the buying kernel — and is
	// signed over those fields. A test cannot write them itself, because the key does not exist
	// until the call is dispatched (P5). Set them with signsAs.
	signKey  ed25519.PrivateKey
	buyerKey string
	// rejectRefreshProxy makes that rejection the contract-mismatch kind (§8 If-Match): the peer's
	// terms moved under our cached row, so it tells us to re-resolve rather than refusing outright.
	rejectRefreshProxy bool
	// resolvedRefs records every resolve request the kernel actually sent, as "owner/name" — a
	// root request has an empty name, so it reads as "owner/" (§13).
	resolvedRefs []string
	// sentAction / sentIdempotencyKey record what the kernel actually put on the wire (§13: the
	// peer's stable action id, under the key parked with the dispatch); sentCommitment and
	// sentLottery record the draw it committed to (P10).
	sentAction         string
	sentIdempotencyKey string
	sentCommitment     string
	sentLottery        int64
	// revealed is every draw the kernel announced to a seller, and revealErr makes that announcement
	// fail so a test can watch the worker resend it.
	revealed  []kernel.RevealPayload
	revealErr error
	// Outbound step protocol (§13): the canned list/complete replies, and what the kernel sent.
	stepListBody      string
	stepBody          string
	stepStatus        int
	stepNotDispatched bool
	stepInput         string
	stepForUserID     string
}

// signsAs makes the fake answer as the peer whose key priv is, for calls bought by k: the receipts
// it returns are signed by priv and bound to k as the counterparty.
func (f *fakeFederationHTTP) signsAs(k *kernel.Kernel, priv ed25519.PrivateKey) {
	f.signKey, f.buyerKey = priv, k.PublicKeyB64()
}

func (f *fakeFederationHTTP) ResolveRemoteAction(_ context.Context, _, owner, name string) (*kernel.ResolvedAction, error) {
	f.resolvedRefs = append(f.resolvedRefs, owner+"/"+name)
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	if f.resolveManifest == nil {
		return nil, nil
	}
	// A world without payment addresses carries none, which is what the manual rail these tests run
	// on reports; a test about paying a peer supplies one.
	return &kernel.ResolvedAction{Manifest: f.resolveManifest,
		RailAddress: f.resolveRailAddress, RailProof: f.resolveRailProof}, nil
}

func (f *fakeFederationHTTP) ResolveRemoteUser(_ context.Context, _, _ string) (string, string, error) {
	if f.resolveErr != nil {
		return "", "", f.resolveErr
	}
	return f.resolveUserID, f.resolveHandle, nil
}

// revealed records every draw this double was asked to announce, so a test can assert what the
// buyer told the seller without a transport.
func (f *fakeFederationHTTP) Reveal(_ context.Context, _ string, p kernel.RevealPayload, _ string) error {
	if f.revealErr != nil {
		return f.revealErr
	}
	f.revealed = append(f.revealed, p)
	return nil
}

// stepStatus/stepBody/stepNotDispatched drive the outbound step protocol (§13); zero values make
// every unrelated test see an unreachable peer, which no call path consults.
func (f *fakeFederationHTTP) CompletePeerStep(_ context.Context, _, _, _, _, _ string, input []byte, forUserID, _, _ string, _ bool) (int, []byte, bool, error) {
	f.stepInput, f.stepForUserID = string(input), forUserID
	return f.stepReply()
}

func (f *fakeFederationHTTP) ListPeerSteps(_ context.Context, _, _, _, _ string) (int, []byte, bool, error) {
	if f.stepListBody == "" {
		return 0, nil, true, nil // no listing configured: peer unreachable, so no payment descriptor
	}
	return 200, []byte(f.stepListBody), false, nil
}

func (f *fakeFederationHTTP) stepReply() (int, []byte, bool, error) {
	if f.stepBody == "" {
		return 0, nil, f.stepNotDispatched, kernel.ErrPeerUnreachable.Wrap("step transport failure")
	}
	status := f.stepStatus
	if status == 0 {
		status = 200
	}
	return status, []byte(f.stepBody), false, nil
}

func (f *fakeFederationHTTP) Execute(_ context.Context, _ *kernel.Action, _ map[string]any, _, _ string) (map[string]any, error) {
	return nil, kernel.ErrInvalidState.Wrap("not used in federation tests")
}

func (f *fakeFederationHTTP) ExecuteFederation(_ context.Context, _, actionID, _, idempotencyKey, commitment string, lottery int64, _ map[string]any) (kernel.FederationResult, error) {
	f.sentAction, f.sentIdempotencyKey = actionID, idempotencyKey
	f.sentCommitment, f.sentLottery = commitment, lottery
	if f.notDispatched {
		return kernel.FederationResult{NotDispatched: true}, nil
	}
	if f.rejectSignKey != nil {
		now := time.Now().UTC()
		// A refusal names no transaction and names the request it refuses (P5).
		r := &kernel.Receipt{
			ID: uuid.New().String(), ActionID: f.rejectActionID,
			IdempotencyKey: idempotencyKey, Counterparty: f.buyerKey,
			ArgsHash: f.rejectArgsHash, Status: kernel.TxFailure, Reason: "counterparty denied",
			RefreshProxy: f.rejectRefreshProxy, StartedAt: now, CreatedAt: now,
		}
		payload, _ := testNet.ReceiptSigningBytes(r)
		r.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.rejectSignKey, payload))
		b, _ := json.Marshal(r)
		status := f.httpStatus // 403 by default: a refusal, not a funding condition
		if status == 0 {
			status = 403
		}
		return kernel.FederationResult{ReceiptJSON: string(b), HTTPStatus: status}, nil
	}
	result := f.result
	if result == nil {
		result = map[string]any{}
	}
	status := f.httpStatus
	if status == 0 {
		status = 200
	}
	if f.signKey != nil && f.receiptJSON != "" {
		var r kernel.Receipt
		if err := json.Unmarshal([]byte(f.receiptJSON), &r); err == nil {
			r.IdempotencyKey, r.Counterparty = idempotencyKey, f.buyerKey
			r.Signature = ""
			payload, _ := testNet.ReceiptSigningBytes(&r)
			r.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.signKey, payload))
			b, _ := json.Marshal(&r)
			f.receiptJSON = string(b)
		}
	}
	return kernel.FederationResult{Result: result, ReceiptJSON: f.receiptJSON, HTTPStatus: status}, nil
}

// jcsHashForTest computes SHA-256(JCS(jsonStr)) using the exported CanonicalJSON.
func jcsHashForTest(t *testing.T, jsonStr string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(jsonStr), &v); err != nil {
		t.Fatal(err)
	}
	canonical, err := kernel.CanonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", h)
}

// signReceiptForTest signs a Receipt using the same method as the kernel's signReceipt:
// Ed25519 over CanonicalJSON of the receipt with Signature cleared. A receipt carrying an
// obligation gets the seller's half of the draw, since without one it is not settleable (P10).
func signReceiptForTest(t *testing.T, key ed25519.PrivateKey, r *kernel.Receipt) string {
	t.Helper()
	if r.Charge+r.Premium > 0 && r.Nonce == "" {
		r.Nonce = "0011223344556677889900aabbccddeeff00112233445566778899aabbccddee"
	}
	payload, err := testNet.ReceiptSigningBytes(r)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
}

// TestSuperuserListTransactions verifies that @sys can list transactions where it is not a party.
func TestSuperuserListTransactions(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	sys := setupSys(t, k, st)
	buyer := setupUser(t, st, "su-buyer", 500)
	provider := setupUser(t, st, "su-provider", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "su-svc",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	_, tr := beginTestRun(t, st, buyer.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: buyer.ID, ExistingTraceID: tr.ID,
		TargetUserID: provider.ID, ActionName: "su-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// @sys is not buyer, provider, or process owner — confirm it is not a party.
	if sys.ID == buyer.ID || sys.ID == provider.ID {
		t.Fatal("test invariant broken: @sys must not be a party to this transaction")
	}
	// ListTransactions as @sys must include the transaction.
	views, err := k.ListTransactions(ctx, sys.ID, kernel.TxFilter{Limit: 20})
	if err != nil {
		t.Fatalf("ListTransactions as @sys: %v", err)
	}
	found := false
	for _, v := range views {
		if v.ID == reply.TxID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sys ListTransactions did not return transaction %s", reply.TxID)
	}
}

func TestReadCallableAction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "owner", 0)
	other := setupUser(t, st, "other", 0)

	baseSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": map[string]any{"type": "string", "description": "x"}},
	}

	pubAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "pub-action",
		Kind: kernel.KindNative, Active: true, Visibility: kernel.VisibilityPublic,
		Description: "public", Source: "native",
		InputSchema: baseSchema, OutputSchema: baseSchema,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	privAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "priv-action",
		Kind: kernel.KindNative, Active: true, Visibility: kernel.VisibilityPrivate,
		Description: "private", Source: "native",
		InputSchema: baseSchema, OutputSchema: baseSchema,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	for _, a := range []*kernel.Action{pubAction, privAction} {
		if err := st.CreateAction(ctx, a); err != nil {
			t.Fatalf("create action %s: %v", a.Name, err)
		}
	}

	t.Run("public action visible to any process owner", func(t *testing.T) {
		a, err := k.ReadCallableAction(ctx, "owner"+"/"+"pub-action", other.ID)
		if err != nil {
			t.Fatalf("expected public action to be callable by other: %v", err)
		}
		if a.ID != pubAction.ID {
			t.Errorf("wrong action returned")
		}
	})

	t.Run("private action visible only to its owner process", func(t *testing.T) {
		a, err := k.ReadCallableAction(ctx, "owner"+"/"+"priv-action", owner.ID)
		if err != nil {
			t.Fatalf("expected private action callable by own process: %v", err)
		}
		if a.ID != privAction.ID {
			t.Errorf("wrong action returned")
		}
	})

	t.Run("private action not callable by foreign process", func(t *testing.T) {
		_, err := k.ReadCallableAction(ctx, "owner"+"/"+"priv-action", other.ID)
		if err == nil {
			t.Error("expected error: private action should not be callable by other process")
		}
	})

	t.Run("unknown action returns not-found error", func(t *testing.T) {
		_, err := k.ReadCallableAction(ctx, "owner"+"/"+"no-such-action", owner.ID)
		if err == nil {
			t.Error("expected error for missing action")
		}
	})
}

func TestRunInputSchemaRejectionLeavesNoProcess(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice-run-schema", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "schema-guarded",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		InputSchema:  map[string]any{"type": "object", "required": []any{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string", "description": "required field"}}},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	_, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice-run-schema/schema-guarded", Args: map[string]any{"wrong_field": "x"}})
	if !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Fatalf("bad args: got %v, want ErrSchemaViolation", err)
	}

	got, _ := st.ReadUser(ctx, alice.ID)
	if got.Locked != 0 {
		t.Errorf("user.Locked=%d after schema rejection, want 0 (no process should be created)", got.Locked)
	}
	procs, _ := st.ListProcesses(ctx, alice.ID, 10, 0)
	if len(procs) != 0 {
		t.Errorf("expected no processes after schema rejection, got %d", len(procs))
	}
}

func TestRunNoSigningKeyLeavesNoProcess(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	k.SetSigningKey(nil, "issuer-id")
	ctx := context.Background()

	bob := setupUser(t, st, "bob-run-nokey", 200)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: bob.ID, Name: "no-key",
		Kind: kernel.KindWasm, Active: true, Price: 50,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	_, err := k.Run(ctx, kernel.RunRequest{CallerID: bob.ID, ActionRef: "bob-run-nokey/no-key", Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}

	got, _ := st.ReadUser(ctx, bob.ID)
	if got.Locked != 0 {
		t.Errorf("user.Locked=%d after signing-key rejection, want 0 (no process should be created)", got.Locked)
	}
	procs, _ := st.ListProcesses(ctx, bob.ID, 10, 0)
	if len(procs) != 0 {
		t.Errorf("expected no processes after signing-key rejection, got %d", len(procs))
	}
}

func TestRunDoesNotCreateProcessForInactiveAction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()

	alice := setupUser(t, st, "alice-run-inactive", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "inactive-act",
		Kind: kernel.KindWasm, Active: false, Price: 100,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	_, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "alice-run-inactive/inactive-act", Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState for inactive action, got %v", err)
	}

	got, _ := st.ReadUser(ctx, alice.ID)
	if got.Locked != 0 {
		t.Errorf("user.Locked=%d after inactive action rejection, want 0 (no process created)", got.Locked)
	}
	procs, _ := st.ListProcesses(ctx, alice.ID, 10, 0)
	if len(procs) != 0 {
		t.Errorf("expected no processes after inactive action rejection, got %d", len(procs))
	}
}

func TestRunFederatedDoesNotCreateProcessOnInsufficientBalance(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()

	target := setupUser(t, st, "target-fed-bal", 0)
	caller := setupUser(t, st, "caller-fed-bal", 50) // balance < action price
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: target.ID, Name: "fed-bal-act",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 100,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	_, err := k.RunFederated(ctx, caller.ID, a, map[string]any{}, "", kernel.BuyerTerms{})
	if !errors.Is(err, kernel.ErrInsufficientFunds) {
		t.Fatalf("expected ErrInsufficientFunds, got %v", err)
	}

	got, _ := st.ReadUser(ctx, caller.ID)
	if got.Locked != 0 {
		t.Errorf("user.Locked=%d after insufficient balance, want 0 (no process created)", got.Locked)
	}
	procs, _ := st.ListProcesses(ctx, caller.ID, 10, 0)
	if len(procs) != 0 {
		t.Errorf("expected no processes after insufficient balance, got %d", len(procs))
	}
}

// TestRunFederatedLocalActionDenied: an inbound peer call to a local-visibility action is denied
// (§4/§13), the twin of the public-only manifest rule — a peer authenticates as a key account, so
// canCall's local branch rejects it. The denial is a precondition failure (no process/transaction),
// which the federation handler turns into a signed zero-charge rejection receipt.
func TestRunFederatedLocalActionDenied(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()

	target := setupUser(t, st, "target-local-fed", 0)
	// A peer proxy user: a set kernel_public_key makes it a key account (a peer), funded so the denial is
	// on visibility, not balance.
	if err := st.UpsertKernel(ctx, "cGVlci1sb2NhbC1mZWQ", "peer-local-fed", "", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	peer := &kernel.Account{
		ID: uuid.New().String(), KernelPublicKey: "cGVlci1sb2NhbC1mZWQ", Available: 1000,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateUser(ctx, peer); err != nil {
		t.Fatal(err)
	}
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: target.ID, Name: "local-fed-act",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityLocal, Price: 10,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	_, err := k.RunFederated(ctx, peer.ID, a, map[string]any{}, "", kernel.BuyerTerms{})
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("peer calling a local action: want ErrUnauthorized, got %v", err)
	}
	procs, _ := st.ListProcesses(ctx, peer.ID, 10, 0)
	if len(procs) != 0 {
		t.Errorf("expected no process for a denied inbound local call, got %d", len(procs))
	}
}

// TestRateInboundForeignCallRefused: a call this kernel served to a peer is funded and therefore
// owned by the seller (§6 role law), so the plain buyer check would let a provider rate its own
// work and gossip it as trade evidence. The payer is on the other kernel and rates its own proxy
// transaction there. A step a peer completes here stays the local payer's to rate.
func TestRateInboundForeignCallRefused(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	provider := setupUser(t, st, "rate-fed-provider", 0)
	if err := st.UpsertKernel(ctx, "cmF0ZS1mZWQtcGVlcg", "rate-fed-peer", "", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	peer := &kernel.Account{
		ID: uuid.New().String(), KernelPublicKey: "cmF0ZS1mZWQtcGVlcg",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateUser(ctx, peer); err != nil {
		t.Fatal(err)
	}
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "rate-fed-act",
		Kind: kernel.KindWasm, Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		Source:      "wat",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	// Exactly as the federation handler admits a call: the inbound record exists before it runs.
	rec := &kernel.IdempotencyRecord{
		ID: uuid.New().String(), IdempotencyKey: "rate-fed-key", CounterpartyUserID: peer.ID,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := st.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	reply, err := k.RunFederated(ctx, peer.ID, a, map[string]any{}, rec.ID, kernel.BuyerTerms{})
	if err != nil {
		t.Fatalf("RunFederated: %v", err)
	}
	tx, err := st.ReadTransaction(ctx, reply.TxID)
	if err != nil {
		t.Fatalf("ReadTransaction: %v", err)
	}
	if tx.OwnerUserID != provider.ID {
		t.Fatalf("inbound call owner: got %q, want the seller %q", tx.OwnerUserID, provider.ID)
	}
	if _, err := k.RateTransaction(ctx, provider.ID, reply.TxID, 1.0, nil); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("provider rating its own served call: want ErrUnauthorized, got %v", err)
	}
	// Retention purges the peer: its account row loses the kernel key but stays as ledger anchor
	// (§13). The gate must not reopen for a transaction that has outlived its peer.
	if err := st.PurgePeerCascade(ctx, peer.ID); err != nil {
		t.Fatalf("PurgePeerCascade: %v", err)
	}
	if anon, _ := st.ReadUser(ctx, peer.ID); anon == nil || anon.IsPeer() {
		t.Fatalf("purge did not anonymize the peer account; the test would prove nothing")
	}
	if _, err := k.RateTransaction(ctx, provider.ID, reply.TxID, 1.0, nil); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("provider rating a served call after the peer was purged: want ErrUnauthorized, got %v", err)
	}
	if ratings, lerr := st.ListRatings(ctx, a.ID, 10, 0); lerr == nil && len(ratings) != 0 {
		t.Errorf("a refused rating still recorded %d row(s)", len(ratings))
	}
}

// TestActionRatingsAdmitTradeBackedPeerRatings: a buyer who paid abroad rates on the kernel that
// paid, and that rating reaches the provider's public projection only on D16's link — the rater's
// kernel names one of this kernel's receipts for the action, and that receipt's call was made by
// that very kernel. A rating attached to someone else's trade, to no trade, or by an equivocating
// issuer is not admitted; a local rating sits beside the admitted one, each naming its source.
func TestActionRatingsAdmitTradeBackedPeerRatings(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	provider := setupUser(t, st, "prt-provider", 0)
	const peerKey, strangerKey = "cHJ0LXBlZXI", "cHJ0LXN0cmFuZ2Vy"
	for _, key := range []string{peerKey, strangerKey} {
		if err := st.UpsertKernel(ctx, key, "n-"+key, "", "", "", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		acct := &kernel.Account{ID: uuid.New().String(), KernelPublicKey: key, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		if err := st.CreateUser(ctx, acct); err != nil {
			t.Fatal(err)
		}
	}
	peer, _ := st.ReadAccountByKernelKey(ctx, peerKey)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "prt-act", Kind: kernel.KindWasm,
		Active: true, Visibility: kernel.VisibilityPublic, Source: "wat",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	// The peer's call, served here: its receipt is what a remote rating must name.
	rec := &kernel.IdempotencyRecord{ID: uuid.New().String(), IdempotencyKey: "prt-key", CounterpartyUserID: peer.ID,
		CreatedAt: time.Now().UTC()}
	if _, err := st.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	reply, err := k.RunFederated(ctx, peer.ID, a, map[string]any{}, rec.ID, kernel.BuyerTerms{})
	if err != nil {
		t.Fatalf("RunFederated: %v", err)
	}
	served, _ := st.ReadReceiptByTxID(ctx, reply.TxID)
	servedHash, err := kernel.ReceiptHash(served)
	if err != nil {
		t.Fatal(err)
	}
	self := k.PublicKeyB64()
	now := time.Now().UTC().Truncate(time.Second)
	rows := []*kernel.EvidenceRow{
		ratingEvidence(t, peerKey, self, a.ID, "peer-own-1", servedHash, 1, now.Add(-time.Minute)), // admitted
		ratingEvidence(t, strangerKey, self, a.ID, "str-own-1", servedHash, 0, now),                // not the caller
		ratingEvidence(t, peerKey, self, a.ID, "peer-own-2", "not-our-receipt", 0, now),            // no trade
		ratingEvidence(t, peerKey, self, "other-action", "peer-own-3", servedHash, 0, now),         // other action
	}
	for _, e := range rows {
		if err := st.UpsertEvidence(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// A local payer's rating, newer, sits beside it.
	buyer := setupUser(t, st, "prt-buyer", 0)
	_, tr := beginTestRun(t, st, buyer.ID, a)
	local, err := k.TestCall(ctx, kernel.TestCallRequest{CallerID: buyer.ID, ExistingTraceID: tr.ID, TargetUserID: provider.ID, ActionName: a.Name, Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.RateTransaction(ctx, buyer.ID, local.TxID, 1.0, nil); err != nil {
		t.Fatal(err)
	}

	got, err := k.ActionRatings(ctx, a, 50, 0)
	if err != nil {
		t.Fatalf("ActionRatings: %v", err)
	}
	if len(got) != 2 || got[0].Source != "local" || got[1].Source != "peer" || got[1].Value != 1 {
		t.Fatalf("want [local, peer(1)] newest first, got %+v", got)
	}
	if page, _ := k.ActionRatings(ctx, a, 1, 1); len(page) != 1 || page[0].Source != "peer" {
		t.Errorf("paging over the merged projection: got %+v", page)
	}
	// A peer mints its own row keys, so it can gossip any number of rows naming our one receipt:
	// agreeing rows are one rating.
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, peerKey, self, a.ID, "peer-own-5", servedHash, 1, now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	peerRatings := func() int {
		got, _ := k.ActionRatings(ctx, a, 50, 0)
		n := 0
		for _, r := range got {
			if r.Source == "peer" {
				n++
			}
		}
		return n
	}
	if n := peerRatings(); n != 1 {
		t.Errorf("two agreeing rows on one trade: want 1 peer rating, got %d", n)
	}
	// A trade is judged with all its rows. A stored row outside the contract — a value that is no
	// rating, or a note over the bound — voids it, as does an equivocated row: the issuer has told
	// two stories about that trade. The same rule the inspect view applies.
	long := strings.Repeat("n", 1025)
	overlong := ratingEvidence(t, peerKey, self, a.ID, "peer-own-6", servedHash, 1, now)
	overlong.RatingJSON = strings.Replace(overlong.RatingJSON, `"note":null`, `"note":"`+long+`"`, 1)
	if err := st.UpsertEvidence(ctx, overlong); err != nil {
		t.Fatal(err)
	}
	if n := peerRatings(); n != 0 {
		t.Errorf("a stored overlong note on the trade: want it void, got %d peer rating(s)", n)
	}
	// Equivocation as the store detects it, on a fresh trade of another action so it is judged alone.
	fresh := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "prt-act-2", Kind: kernel.KindWasm,
		Active: true, Visibility: kernel.VisibilityPublic, Source: "wat",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	rec2 := &kernel.IdempotencyRecord{ID: uuid.New().String(), IdempotencyKey: "prt-key-2", CounterpartyUserID: peer.ID,
		CreatedAt: time.Now().UTC()}
	if _, err := st.InsertPendingIdempotencyRecord(ctx, rec2); err != nil {
		t.Fatal(err)
	}
	reply2, err := k.RunFederated(ctx, peer.ID, fresh, map[string]any{}, rec2.ID, kernel.BuyerTerms{})
	if err != nil {
		t.Fatalf("RunFederated: %v", err)
	}
	served2, _ := st.ReadReceiptByTxID(ctx, reply2.TxID)
	hash2, _ := kernel.ReceiptHash(served2)
	// The note bound holds on read, alone: one stored row, agreeing with nothing but itself.
	tooLong := ratingEvidence(t, peerKey, self, fresh.ID, "peer-own-8", hash2, 1, now)
	tooLong.RatingJSON = strings.Replace(tooLong.RatingJSON, `"note":null`, `"note":"`+long+`"`, 1)
	if err := st.UpsertEvidence(ctx, tooLong); err != nil {
		t.Fatal(err)
	}
	if got, _ := k.ActionRatings(ctx, fresh, 50, 0); len(got) != 0 {
		t.Fatalf("a stored overlong note, alone on its trade: want not admitted, got %+v", got)
	}
	// Equivocation as the store detects it — two ratings under one row key — voids a trade even
	// beside an honest row. On a third trade, so it is judged alone.
	third := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "prt-act-3", Kind: kernel.KindWasm,
		Active: true, Visibility: kernel.VisibilityPublic, Source: "wat",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, third); err != nil {
		t.Fatal(err)
	}
	rec3 := &kernel.IdempotencyRecord{ID: uuid.New().String(), IdempotencyKey: "prt-key-3", CounterpartyUserID: peer.ID,
		CreatedAt: time.Now().UTC()}
	if _, err := st.InsertPendingIdempotencyRecord(ctx, rec3); err != nil {
		t.Fatal(err)
	}
	reply3, err := k.RunFederated(ctx, peer.ID, third, map[string]any{}, rec3.ID, kernel.BuyerTerms{})
	if err != nil {
		t.Fatalf("RunFederated: %v", err)
	}
	served3, _ := st.ReadReceiptByTxID(ctx, reply3.TxID)
	hash3, _ := kernel.ReceiptHash(served3)
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, peerKey, self, third.ID, "peer-own-9", hash3, 1, now)); err != nil {
		t.Fatal(err)
	}
	if got, _ := k.ActionRatings(ctx, third, 50, 0); len(got) != 1 || got[0].Source != "peer" {
		t.Fatalf("third trade: want its one peer rating, got %+v", got)
	}
	for _, val := range []float64{1, 0} {
		if err := st.UpsertEvidence(ctx, ratingEvidence(t, peerKey, self, third.ID, "peer-own-10", hash3, val, now)); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := k.ActionRatings(ctx, third, 50, 0); len(got) != 0 {
		t.Errorf("an equivocated row on the trade voids it even beside an honest one, got %+v", got)
	}

}

// TestCallUsesActionIDNotOwnerName verifies Fix 1: when Call is invoked with ActionID set
// (the root call path from beginRun), it loads the action by ID rather than by owner/name.
// This prevents a race where the original action is deleted and a new action is created under
// the same name between BeginRun and Call — the old code would silently execute the new action.
// With the fix, deleting the funded action causes Call to return ErrNotFound (clean failure),
// not to re-resolve by owner/name (which could rebind to a replacement).
func TestCallBindsToPassedAction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "pid-owner", 200)
	actionA := setupWasmAction(t, st, owner.ID, "pid-act", "", 100)

	// Fund a process+trace for action A.
	_, tr := beginTestRun(t, st, owner.ID, actionA)

	// Delete action A from the store, simulating the race window where the funded action
	// disappears or is replaced. Call must bind to the passed Action snapshot regardless.
	if err := st.DeleteAction(ctx, actionA.ID); err != nil {
		t.Fatalf("DeleteAction: %v", err)
	}

	// Call with the pre-resolved Action (as beginRun does for root calls): it executes the exact
	// action snapshot without re-reading the store, so deletion does not turn into a spurious
	// owner/name re-lookup. The call succeeds and binds to actionA.
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        owner.ID,
		Action:          actionA,
		Args:            map[string]any{},
		ExistingTraceID: tr.ID,
	})
	if err != nil {
		t.Fatalf("Call with passed Action should succeed, got %v", err)
	}
	txn, err := st.ReadTransaction(ctx, reply.TxID)
	if err != nil {
		t.Fatalf("ReadTransaction: %v", err)
	}
	if txn.ActionID != actionA.ID {
		t.Errorf("call bound to wrong action: got %q, want %q", txn.ActionID, actionA.ID)
	}
}

// TestCallLogsTxID verifies that a real call emits log lines carrying tx_id (§14 required
// field). Regression guard: the logger reads tx_id from context but nothing wired it until
// Call() began calling log.WithTxID.
func TestCallLogsTxID(t *testing.T) {
	st := newTestStore(t)
	logPath := filepath.Join(t.TempDir(), "call.log")
	logger, err := log.New(log.Config{Level: "info", FilePath: logPath, Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	k := newKernel(testConfig(), kernel.Dependencies{Store: st, Scripts: &fakeScriptExec{result: `{"ok":true}`}, Logger: logger})

	ctx := context.Background()
	alice := setupUser(t, st, "alice", 2000)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "paid", Kind: kernel.KindWasm,
		Active: true, Price: 100, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_, tr := beginTestRun(t, st, alice.ID, a)

	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: alice.ID, ExistingTraceID: tr.ID, TargetUserID: alice.ID,
		ActionName: "paid", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), reply.TxID) {
		t.Errorf("call log missing tx_id %q; content: %s", reply.TxID, string(data))
	}
}

// TestUnsafeHostAndIP covers the single-source-of-truth SSRF predicate (§7/§9).
func TestUnsafeHostAndIP(t *testing.T) {
	unsafe := []string{"", "10.0.0.1", "192.168.1.1", "169.254.0.1", "172.16.0.1"}
	for _, h := range unsafe {
		if !kernel.UnsafeHost(h) {
			t.Errorf("UnsafeHost(%q) = false, want true", h)
		}
	}
	// Loopback is permitted by default (§7/§9), so it is no longer unsafe.
	safe := []string{"example.com", "8.8.8.8", "1.1.1.1", "api.stripe.com", "localhost", "127.0.0.1", "::1"}
	for _, h := range safe {
		if kernel.UnsafeHost(h) {
			t.Errorf("UnsafeHost(%q) = true, want false", h)
		}
	}
	if kernel.UnsafeIP(net.ParseIP("8.8.8.8")) {
		t.Error("UnsafeIP(8.8.8.8) = true, want false")
	}
	if kernel.UnsafeIP(net.ParseIP("127.0.0.1")) {
		t.Error("UnsafeIP(127.0.0.1) = true, want false (loopback permitted by default)")
	}
	if !kernel.UnsafeIP(net.ParseIP("192.168.1.1")) {
		t.Error("UnsafeIP(192.168.1.1) = false, want true")
	}
	// The single SSRF rejection error always names the escape hatch (item 3).
	if msg := kernel.ErrUnsafeSourceURL("x").Error(); !strings.Contains(msg, "allow_local_sources") {
		t.Errorf("ErrUnsafeSourceURL message %q should name allow_local_sources", msg)
	}
}

// Ensure fmt is used.
var _ = fmt.Sprintf

// TestListProcessesSuperuserWidening proves supervision is scope: the superuser sees every
// process and may read any, while an ordinary user stays scoped to their own (mirrors the
// existing ListTransactions/ListSteps widening).
func TestListProcessesSuperuserWidening(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	sys := setupSys(t, k, st)
	alice := setupUser(t, st, "alice", 100)
	bob := setupUser(t, st, "bob", 0)
	proc := setupProcess(t, st, alice.ID, 0)

	has := func(callerID string) bool {
		ps, err := k.ListProcesses(ctx, callerID, 50, 0)
		if err != nil {
			t.Fatalf("ListProcesses(%s): %v", callerID, err)
		}
		for _, p := range ps {
			if p.ID == proc.ID {
				return true
			}
		}
		return false
	}
	if !has(alice.ID) {
		t.Error("alice should see her own process")
	}
	if !has(sys.ID) {
		t.Error("sys should see @alice's process (superuser widening)")
	}
	if has(bob.ID) {
		t.Error("bob must not see @alice's process")
	}

	if _, err := k.ReadProcess(ctx, sys.ID, proc.ID); err != nil {
		t.Errorf("sys ReadProcess should succeed: %v", err)
	}
	if _, err := k.ReadProcess(ctx, bob.ID, proc.ID); err == nil {
		t.Error("bob ReadProcess of @alice's process should fail")
	}
}

// ---- Delegated OAuth grants (§8) ----

// createDelegatedAction creates and activates a private kind=http action using the oauth_delegated
// scheme, owned by ownerID. The kernel must have a SecretBox installed.
func createDelegatedAction(t *testing.T, k *kernel.Kernel, ownerID, name string, price int64) *kernel.Action {
	t.Helper()
	ctx := context.Background()
	a, err := k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID:  ownerID,
		Name:         name,
		Kind:         kernel.KindHTTP,
		Price:        price,
		Source:       "https://provider.example/api",
		Description:  "delegated svc",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Auth: &kernel.AuthInput{
			Scheme: kernel.AuthSchemeOAuthDelegated,
			Config: map[string]any{
				"auth_url":  "https://provider.example/auth",
				"token_url": "https://provider.example/token",
				"client_id": "cid",
				"scopes":    "read",
			},
		},
	})
	if err != nil {
		t.Fatalf("create delegated action: %v", err)
	}
	if err := k.SetActive(ctx, ownerID, a.ID, true); err != nil {
		t.Fatalf("activate delegated action: %v", err)
	}
	full, _ := k.ReadAction(ctx, a.ID)
	return full
}

// TestValidateAuthInputSchemes: CreateAction rejects an unknown scheme and missing required keys
// (fail closed at write time, §8), and accepts a well-formed oauth_delegated config.
func TestValidateAuthInputSchemes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "authcfg", 0)

	base := func(auth *kernel.AuthInput) kernel.CreateActionRequest {
		return kernel.CreateActionRequest{
			OwnerUserID: owner.ID, Name: "svc-" + auth.Scheme, Kind: kernel.KindHTTP,
			Source: "https://provider.example/api", Description: "d",
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			Auth: auth,
		}
	}

	// Unknown scheme.
	if _, err := k.CreateAction(ctx, owner.ID, base(&kernel.AuthInput{Scheme: "totp"})); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("unknown scheme: got %v, want ErrInvalidInput", err)
	}
	// client-credentials missing client_secret.
	if _, err := k.CreateAction(ctx, owner.ID, base(&kernel.AuthInput{
		Scheme: kernel.AuthSchemeOAuthClientCreds,
		Config: map[string]any{"token_url": "https://p.example/t", "client_id": "c"},
	})); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("client-creds missing secret: got %v, want ErrInvalidInput", err)
	}
	// delegated missing token_url.
	if _, err := k.CreateAction(ctx, owner.ID, base(&kernel.AuthInput{
		Scheme: kernel.AuthSchemeOAuthDelegated,
		Config: map[string]any{"auth_url": "https://p.example/a", "client_id": "c"},
	})); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("delegated missing token_url: got %v, want ErrInvalidInput", err)
	}
	// Valid delegated config.
	if _, err := k.CreateAction(ctx, owner.ID, base(&kernel.AuthInput{
		Scheme: kernel.AuthSchemeOAuthDelegated,
		Config: map[string]any{"auth_url": "https://p.example/a", "token_url": "https://p.example/t", "client_id": "c"},
	})); err != nil {
		t.Errorf("valid delegated config: unexpected error %v", err)
	}
}

// TestGrantRequiredRejectsBeforeLock: running an oauth_delegated action with no grant is rejected
// before any funds are locked and before any transaction/process exists (§8 lazy consent).
func TestGrantRequiredRejectsBeforeLock(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "dlg-owner", 1000)

	a := createDelegatedAction(t, k, owner.ID, "inbox", 100)

	_, err := k.Run(ctx, kernel.RunRequest{CallerID: owner.ID, ActionRef: owner.Handle + "/" + a.Name, Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrGrantRequired) {
		t.Fatalf("run without grant: got %v, want ErrGrantRequired", err)
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Meta["action"] != owner.Handle+"/"+a.Name {
		t.Errorf("error does not carry structured action meta: %+v", err)
	}
	// No funds locked, no process created.
	u, _ := k.ReadUser(ctx, owner.ID)
	if u.Available != 1000 || u.Locked != 0 {
		t.Errorf("balance moved: available=%d locked=%d, want 1000/0", u.Available, u.Locked)
	}
	procs, _ := k.ListProcesses(ctx, owner.ID, 100, 0)
	if len(procs) != 0 {
		t.Errorf("process created despite pre-lock rejection: %d", len(procs))
	}
}

// grantVia mints consent through the canonical selector path (§8): the consent plan supplies the
// provider group and its scope union, CreateGrants stores one Connection and mints the grants. It
// replaces the deleted single-action CreateGrant/AttachBearerGrant shortcuts.
func grantVia(k *kernel.Kernel, ctx context.Context, callerID, selector, token string) error {
	plan, err := k.ConsentPlan(ctx, callerID, selector)
	if err != nil {
		return err
	}
	if len(plan.Groups) != 1 {
		return kernel.ErrInvalidInput.Wrapf("selector %q spans %d provider groups", selector, len(plan.Groups))
	}
	g := plan.Groups[0]
	ids := make([]string, 0, len(g.Actions))
	for _, a := range g.Actions {
		ids = append(ids, a.ActionID)
	}
	scopesJSON := ""
	if len(g.Scopes) > 0 {
		b, _ := json.Marshal(g.Scopes)
		scopesJSON = string(b)
	}
	_, err = k.CreateGrants(ctx, callerID, g.ProviderKey, ids, token, scopesJSON)
	return err
}

// revokeVia revokes one action's consent through the selector path (§8).
func revokeVia(k *kernel.Kernel, ctx context.Context, callerID, selector string) error {
	_, err := k.RevokeGrantsBySelector(ctx, callerID, selector)
	return err
}

// TestCreateGrantListAndRevoke: a grant is created token-free-visible and revocable (§8).
func TestCreateGrantListAndRevoke(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "grantsvc", 0)
	a := createDelegatedAction(t, k, owner.ID, "svc", 0)

	if err := grantVia(k, ctx, owner.ID, owner.Handle+"/"+a.Name, "refresh-xyz"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	views, err := k.ListGrantViews(ctx, owner.ID)
	if err != nil || len(views) != 1 {
		t.Fatalf("ListGrantViews: %v, n=%d", err, len(views))
	}
	if views[0].Action != owner.Handle+"/"+a.Name {
		t.Errorf("view action = %q, want %q", views[0].Action, owner.Handle+"/"+a.Name)
	}
	if views[0].Scopes != "read" {
		t.Errorf("view scopes = %v, want read", views[0].Scopes)
	}
	// The refresh token never surfaces in the view (marshal it and check).
	if b, _ := json.Marshal(views[0]); strings.Contains(string(b), "refresh-xyz") {
		t.Errorf("grant view leaked the refresh token: %s", b)
	}
	// The grant carries the oauth-prefixed provider_key of its backing connection, and it equals
	// the connection view's key — the join a client uses to group grants by account (§8).
	if !strings.HasPrefix(views[0].ProviderKey, "oauth:") {
		t.Errorf("grant provider_key = %q, want oauth: prefix", views[0].ProviderKey)
	}
	conns, err := k.ListConnectionViews(ctx, owner.ID)
	if err != nil || len(conns) != 1 {
		t.Fatalf("ListConnectionViews: %v, n=%d", err, len(conns))
	}
	if conns[0].ProviderKey != views[0].ProviderKey {
		t.Errorf("join broken: connection key %q != grant key %q", conns[0].ProviderKey, views[0].ProviderKey)
	}

	if err := revokeVia(k, ctx, owner.ID, owner.Handle+"/"+a.Name); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if views, _ := k.ListGrantViews(ctx, owner.ID); len(views) != 0 {
		t.Errorf("grant survived revoke: %d", len(views))
	}
}

// TestDeactivatingUpdateRevokesGrants: a deactivating update (price change) drops standing grants,
// while a plain disable keeps them — consent binds to the contract, not the enabled bit (§8).
func TestDeactivatingUpdateRevokesGrants(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "revsvc", 0)
	a := createDelegatedAction(t, k, owner.ID, "svc", 0)

	mkGrant := func() {
		if err := grantVia(k, ctx, owner.ID, owner.Handle+"/"+a.Name, "refresh"); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	grantCount := func() int { v, _ := k.ListGrantViews(ctx, owner.ID); return len(v) }

	// Plain disable keeps the grant.
	mkGrant()
	if err := k.SetActive(ctx, owner.ID, a.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if grantCount() != 1 {
		t.Errorf("plain disable dropped the grant; want kept")
	}
	_ = k.SetActive(ctx, owner.ID, a.ID, true)

	// A deactivating update (price change) revokes it.
	newPrice := int64(50)
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Price: &newPrice}); err != nil {
		t.Fatalf("UpdateAction: %v", err)
	}
	if grantCount() != 0 {
		t.Errorf("deactivating update did not revoke the grant")
	}
}

// TestUpdateActionVisibility: an invalid visibility value is rejected, and a valid visibility change
// does not deactivate the action (§4/§7 — only source/schema/price/kind changes deactivate).
func TestUpdateActionVisibility(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	owner := setupUser(t, st, "vis-owner", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "vis",
		Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPrivate, Price: 0,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Source: "https://example.com/x", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	bad := kernel.ActionVisibility("banana")
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &bad}); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("invalid visibility: want ErrInvalidInput, got %v", err)
	}

	local := kernel.VisibilityLocal
	got, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &local})
	if err != nil {
		t.Fatalf("valid visibility change: %v", err)
	}
	if got.Visibility != kernel.VisibilityLocal {
		t.Errorf("visibility: got %q, want local", got.Visibility)
	}
	if !got.Active {
		t.Error("a visibility change must not deactivate the action")
	}
}

// TestManifestExcludesDelegatedAction: an oauth_delegated action is never served as a manifest,
// since a remote peer can never complete a browser consent (§8/§13).
func TestManifestExcludesDelegatedAction(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "mfsvc", 0)
	a := createDelegatedAction(t, k, owner.ID, "svc", 0)
	pub := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pub}); err != nil {
		t.Fatalf("make public: %v", err)
	}
	if _, err := k.GetActionManifest(ctx, a.ID); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("delegated action served a manifest: got %v, want ErrUnauthorized", err)
	}
}

// ---- delegated_bearer: per-caller static token (§8) ----

// createBearerAction creates and activates a private kind=http action using the delegated_bearer
// scheme, owned by ownerID.
func createBearerAction(t *testing.T, k *kernel.Kernel, ownerID, name string, price int64) *kernel.Action {
	t.Helper()
	ctx := context.Background()
	a, err := k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID:  ownerID,
		Name:         name,
		Kind:         kernel.KindHTTP,
		Price:        price,
		Source:       "https://provider.example/api",
		Description:  "bearer svc",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Auth:         &kernel.AuthInput{Scheme: kernel.AuthSchemeDelegatedBearer},
	})
	if err != nil {
		t.Fatalf("create bearer action: %v", err)
	}
	if err := k.SetActive(ctx, ownerID, a.ID, true); err != nil {
		t.Fatalf("activate bearer action: %v", err)
	}
	full, _ := k.ReadAction(ctx, a.ID)
	return full
}

// TestDelegatedBearerValidation: delegated_bearer takes no owner-side secret and, if a value
// template is given, it must name the {token} placeholder; a header/template config is accepted and
// the zero-config form is valid (§8).
func TestDelegatedBearerValidation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "bearercfg", 0)

	base := func(name string, auth *kernel.AuthInput) kernel.CreateActionRequest {
		return kernel.CreateActionRequest{
			OwnerUserID: owner.ID, Name: name, Kind: kernel.KindHTTP,
			Source: "https://provider.example/api", Description: "d",
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			Auth: auth,
		}
	}
	// A secret is rejected: the per-caller token is a grant, not owner-held.
	if _, err := k.CreateAction(ctx, owner.ID, base("db-sec", &kernel.AuthInput{
		Scheme: kernel.AuthSchemeDelegatedBearer, Secrets: map[string]any{"token": "x"},
	})); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("secret present: got %v, want ErrInvalidInput", err)
	}
	// A template without {token} is rejected.
	if _, err := k.CreateAction(ctx, owner.ID, base("db-tmpl", &kernel.AuthInput{
		Scheme: kernel.AuthSchemeDelegatedBearer, Config: map[string]any{"template": "Bearer nope"},
	})); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("bad template: got %v, want ErrInvalidInput", err)
	}
	// A well-formed header/template config is accepted.
	if _, err := k.CreateAction(ctx, owner.ID, base("db-ok", &kernel.AuthInput{
		Scheme: kernel.AuthSchemeDelegatedBearer,
		Config: map[string]any{"header": "Private-Token", "template": "{token}"},
	})); err != nil {
		t.Errorf("valid config: unexpected error %v", err)
	}
	// The zero-config form is valid (defaults to Authorization: Bearer).
	if _, err := k.CreateAction(ctx, owner.ID, base("db-bare", &kernel.AuthInput{
		Scheme: kernel.AuthSchemeDelegatedBearer,
	})); err != nil {
		t.Errorf("zero-config: unexpected error %v", err)
	}
}

// TestBearerGrantRequiredBeforeLock: running a delegated_bearer action with no grant is rejected
// before funds lock and before any transaction/process exists — same lazy consent as OAuth (§8).
func TestBearerGrantRequiredBeforeLock(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "br-owner", 1000)
	a := createBearerAction(t, k, owner.ID, "inbox", 100)

	_, err := k.Run(ctx, kernel.RunRequest{CallerID: owner.ID, ActionRef: owner.Handle + "/" + a.Name, Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrGrantRequired) {
		t.Fatalf("run without grant: got %v, want ErrGrantRequired", err)
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Meta["action"] != owner.Handle+"/"+a.Name {
		t.Errorf("error does not carry structured action meta: %+v", err)
	}
	if u, _ := k.ReadUser(ctx, owner.ID); u.Available != 1000 || u.Locked != 0 {
		t.Errorf("balance moved: available=%d locked=%d, want 1000/0", u.Available, u.Locked)
	}
	if procs, _ := k.ListProcesses(ctx, owner.ID, 100, 0); len(procs) != 0 {
		t.Errorf("process created despite pre-lock rejection: %d", len(procs))
	}
}

// TestAttachBearerGrant: the direct token-store creates a token-free-visible, revocable grant and
// refuses an oauth_delegated action (which must use the browser flow) (§8).
func TestAttachBearerGrant(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "br-attach", 0)
	bearer := createBearerAction(t, k, owner.ID, "svc", 0)

	if _, err := k.AttachBearerGrants(ctx, owner.ID, owner.Handle+"/"+bearer.Name, "", "ghp_secret"); err != nil {
		t.Fatalf("AttachBearerGrants: %v", err)
	}
	views, err := k.ListGrantViews(ctx, owner.ID)
	if err != nil || len(views) != 1 {
		t.Fatalf("ListGrantViews: %v n=%d", err, len(views))
	}
	if views[0].Action != owner.Handle+"/"+bearer.Name {
		t.Errorf("view action = %q, want %q", views[0].Action, owner.Handle+"/"+bearer.Name)
	}
	if b, _ := json.Marshal(views[0]); strings.Contains(string(b), "ghp_secret") {
		t.Errorf("grant view leaked the token: %s", b)
	}
	// A delegated_bearer grant carries a bearer-prefixed provider_key equal to its connection's key.
	if !strings.HasPrefix(views[0].ProviderKey, "bearer:") {
		t.Errorf("bearer grant provider_key = %q, want bearer: prefix", views[0].ProviderKey)
	}
	if conns, err := k.ListConnectionViews(ctx, owner.ID); err != nil || len(conns) != 1 || conns[0].ProviderKey != views[0].ProviderKey {
		t.Errorf("join broken: conns=%v err=%v grant key=%q", conns, err, views[0].ProviderKey)
	}

	// An oauth_delegated action cannot be connected with a raw token.
	oauth := createDelegatedAction(t, k, owner.ID, "oauthsvc", 0)
	if _, err := k.AttachBearerGrants(ctx, owner.ID, owner.Handle+"/"+oauth.Name, "", "raw"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("attach to oauth_delegated: got %v, want ErrNotFound", err)
	}

	// Revocation drops it (the oauth action never gained a grant).
	if err := revokeVia(k, ctx, owner.ID, owner.Handle+"/"+bearer.Name); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if views, _ := k.ListGrantViews(ctx, owner.ID); len(views) != 0 {
		t.Errorf("bearer grant survived revoke: %d", len(views))
	}
}

// TestManifestExcludesBearerAction: a delegated_bearer action is never served as a manifest — a
// remote peer's proxy user can never hold a per-caller token (§8/§13).
func TestManifestExcludesBearerAction(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "br-mf", 0)
	a := createBearerAction(t, k, owner.ID, "svc", 0)
	pub := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pub}); err != nil {
		t.Fatalf("make public: %v", err)
	}
	if _, err := k.GetActionManifest(ctx, a.ID); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("bearer action served a manifest: got %v, want ErrUnauthorized", err)
	}
}

// TestActionAuthInfo: the read-path accessor reports the scheme name and requires-grant flag for
// every auth kind, and ("", false) for a no-auth action — never config or secrets (§8).
func TestActionAuthInfo(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "authinfo", 0)
	read := func(id string) *kernel.Action { a, _ := k.ReadAction(ctx, id); return a }

	mk := func(name string, auth *kernel.AuthInput) *kernel.Action {
		a, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
			OwnerUserID: owner.ID, Name: name, Kind: kernel.KindHTTP, Source: "https://provider.example/api",
			Description: "d", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			Auth: auth,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return read(a.ID)
	}

	cases := []struct {
		name       string
		action     *kernel.Action
		wantScheme string
		wantGrant  bool
	}{
		{"delegated_bearer", mk("db", &kernel.AuthInput{Scheme: kernel.AuthSchemeDelegatedBearer}), "delegated_bearer", true},
		{"oauth_delegated", createDelegatedAction(t, k, owner.ID, "od", 0), "oauth_delegated", true},
		{"basic", mk("basic", &kernel.AuthInput{Scheme: kernel.AuthSchemeBasic, Secrets: map[string]any{"username": "u", "password": "p"}}), "basic", false},
		{"no_auth", mk("plain", nil), "", false},
	}
	for _, tc := range cases {
		s, g := k.ActionAuthInfo(tc.action)
		if s != tc.wantScheme || g != tc.wantGrant {
			t.Errorf("%s: ActionAuthInfo = (%q,%v), want (%q,%v)", tc.name, s, g, tc.wantScheme, tc.wantGrant)
		}
	}
}

// createBearerActionSrc creates and activates a delegated_bearer action with a custom source, so
// tests can place actions on distinct provider hosts (distinct connection provider_keys, §8).
func createBearerActionSrc(t *testing.T, k *kernel.Kernel, ownerID, name, source string) *kernel.Action {
	t.Helper()
	ctx := context.Background()
	a, err := k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: name, Kind: kernel.KindHTTP, Source: source,
		Description: "bearer svc", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Auth: &kernel.AuthInput{Scheme: kernel.AuthSchemeDelegatedBearer},
	})
	if err != nil {
		t.Fatalf("create bearer action: %v", err)
	}
	if err := k.SetActive(ctx, ownerID, a.ID, true); err != nil {
		t.Fatalf("activate bearer action: %v", err)
	}
	full, _ := k.ReadAction(ctx, a.ID)
	return full
}

// createOAuthActionScopes creates and activates an oauth_delegated action sharing one provider app
// (same token_url + client_id) but with the given scopes, so a directory groups into one connection.
func createOAuthActionScopes(t *testing.T, k *kernel.Kernel, ownerID, name, scopes string) *kernel.Action {
	return createOAuthActionSrc(t, k, ownerID, name, scopes, "https://provider.example/api")
}

// createOAuthActionSrc creates an oauth_delegated action sharing one provider app (token_url +
// client_id) but with a caller-chosen source (resource server) host — used to exercise the §8
// destination binding.
func createOAuthActionSrc(t *testing.T, k *kernel.Kernel, ownerID, name, scopes, source string) *kernel.Action {
	t.Helper()
	ctx := context.Background()
	a, err := k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: name, Kind: kernel.KindHTTP, Source: source,
		Description: "oauth svc", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Auth: &kernel.AuthInput{Scheme: kernel.AuthSchemeOAuthDelegated, Config: map[string]any{
			"auth_url": "https://provider.example/auth", "token_url": "https://provider.example/token",
			"client_id": "cid", "scopes": scopes,
		}},
	})
	if err != nil {
		t.Fatalf("create oauth action: %v", err)
	}
	if err := k.SetActive(ctx, ownerID, a.ID, true); err != nil {
		t.Fatalf("activate oauth action: %v", err)
	}
	full, _ := k.ReadAction(ctx, a.ID)
	return full
}

// TestOAuthDestinationBindingBlocksConfusedDeputy: an action with a legitimate provider's
// token_url|client_id but an attacker-controlled source host derives a DISTINCT provider_key, so it
// cannot ride a victim's existing connection via the instant-grant path, and the consent plan
// surfaces the true destination host (§8 confused-deputy defense, F1).
func TestOAuthDestinationBindingBlocksConfusedDeputy(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	victim := setupUser(t, st, "victim", 0)
	good := createOAuthActionSrc(t, k, victim.ID, "good/read", "read", "https://api.provider.example/v1")
	evil := createOAuthActionSrc(t, k, victim.ID, "evil/read", "read", "https://evil.attacker.com/x")

	// Victim legitimately connects the good action → a covering connection for the provider's domain.
	const goodPK = "oauth:https://provider.example/token|cid|provider.example"
	if _, err := k.CreateGrants(ctx, victim.ID, goodPK, []string{good.ID}, "refresh", `["read"]`); err != nil {
		t.Fatalf("connect good: %v", err)
	}

	// The malicious action derives a different provider_key (attacker.com) → not covered → the
	// silent instant-grant (no-token) path is refused. Without this bind it would have ridden the
	// victim's connection and exfiltrated the minted token to evil.attacker.com.
	const evilPK = "oauth:https://provider.example/token|cid|attacker.com"
	if _, err := k.CreateGrants(ctx, victim.ID, evilPK, []string{evil.ID}, "", `["read"]`); err == nil {
		t.Fatal("malicious action rode the victim's connection via instant-grant (confused deputy!)")
	}

	// The consent plan for the malicious action reveals where the credential would actually go.
	plan, err := k.ConsentPlan(ctx, victim.ID, "victim/evil")
	if err != nil {
		t.Fatalf("ConsentPlan: %v", err)
	}
	shown := ""
	for _, g := range plan.Groups {
		shown += strings.Join(g.Destinations, ",")
	}
	if !strings.Contains(shown, "evil.attacker.com") {
		t.Errorf("consent plan did not surface the destination host: %q", shown)
	}
}

// TestConnectBearerSelectorBatch: a directory of delegated_bearer actions sharing one provider host
// connects with one token into one connection and N grants; a later sibling instant-grants (§8).
func TestConnectBearerSelectorBatch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "chatco", 0)
	_ = createBearerAction(t, k, owner.ID, "chat/send", 0)
	_ = createBearerAction(t, k, owner.ID, "chat/history", 0)

	plan, err := k.ConsentPlan(ctx, owner.ID, "chatco/chat")
	if err != nil {
		t.Fatalf("ConsentPlan: %v", err)
	}
	if len(plan.Groups) != 1 || len(plan.Groups[0].Actions) != 2 {
		t.Fatalf("plan groups = %+v, want one group of two", plan.Groups)
	}
	if plan.Groups[0].Connected {
		t.Error("group should not be connected before consent")
	}

	grants, err := k.AttachBearerGrants(ctx, owner.ID, "chatco/chat", "", "ghp_x")
	if err != nil || len(grants) != 2 {
		t.Fatalf("AttachBearerGrants: %v n=%d", err, len(grants))
	}
	conns, _ := k.ListConnectionViews(ctx, owner.ID)
	if len(conns) != 1 || conns[0].Actions != 2 {
		t.Fatalf("connections = %+v, want one with two actions", conns)
	}

	plan, _ = k.ConsentPlan(ctx, owner.ID, "chatco/chat")
	g := plan.Groups[0]
	if !g.Connected || !g.Covered {
		t.Error("group should be connected and covered after consent")
	}
	for _, a := range g.Actions {
		if !a.Granted {
			t.Errorf("%s not granted", a.Action)
		}
	}

	// A later sibling: instant-grant with no token (bearer covered because the connection exists).
	react := createBearerAction(t, k, owner.ID, "chat/react", 0)
	if _, err := k.CreateGrants(ctx, owner.ID, "bearer:provider.example", []string{react.ID}, "", ""); err != nil {
		t.Fatalf("instant bearer grant: %v", err)
	}
	plan, _ = k.ConsentPlan(ctx, owner.ID, "chatco/chat")
	if len(plan.Groups[0].Actions) != 3 {
		t.Fatalf("expected 3 actions after adding react, got %d", len(plan.Groups[0].Actions))
	}
	for _, a := range plan.Groups[0].Actions {
		if !a.Granted {
			t.Errorf("after instant grant %s still ungranted", a.Action)
		}
	}
}

// TestConnectOAuthUnionScopes: two oauth actions sharing one app group into one connection whose
// scopes are the union; a sibling within the union instant-grants, one outside it does not (§8).
func TestConnectOAuthUnionScopes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "gco", 0)
	a1 := createOAuthActionScopes(t, k, owner.ID, "g/read", "read")
	a2 := createOAuthActionScopes(t, k, owner.ID, "g/write", "write")
	const pk = "oauth:https://provider.example/token|cid|provider.example"

	plan, err := k.ConsentPlan(ctx, owner.ID, "gco/g")
	if err != nil {
		t.Fatalf("ConsentPlan: %v", err)
	}
	if len(plan.Groups) != 1 || strings.Join(plan.Groups[0].Scopes, ",") != "read,write" {
		t.Fatalf("group scopes = %+v, want [read write] union", plan.Groups)
	}

	// One consent covering the union grants both actions against one connection.
	grants, err := k.CreateGrants(ctx, owner.ID, pk, []string{a1.ID, a2.ID}, "refresh-tok", `["read","write"]`)
	if err != nil || len(grants) != 2 {
		t.Fatalf("CreateGrants union: %v n=%d", err, len(grants))
	}
	if conns, _ := k.ListConnectionViews(ctx, owner.ID); len(conns) != 1 || conns[0].Actions != 2 {
		t.Fatalf("connections = %+v, want one with two actions", conns)
	}

	// A sibling needing only "read" is covered → instant grant, no token.
	a3 := createOAuthActionScopes(t, k, owner.ID, "g/peek", "read")
	if _, err := k.CreateGrants(ctx, owner.ID, pk, []string{a3.ID}, "", `["read"]`); err != nil {
		t.Errorf("covered sibling should instant-grant: %v", err)
	}
	// A sibling needing "admin" is not covered → instant grant refused.
	a4 := createOAuthActionScopes(t, k, owner.ID, "g/admin", "admin")
	if _, err := k.CreateGrants(ctx, owner.ID, pk, []string{a4.ID}, "", `["admin"]`); err == nil {
		t.Error("uncovered sibling instant-grant should be refused")
	}
}

// TestRevokeSelectorAndAccount: disconnect by selector removes only matching grants (connections
// survive, listed unused); disconnect by account cascades the connection and its grants (§8).
func TestRevokeSelectorAndAccount(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	owner := setupUser(t, st, "multi", 0)
	_ = createBearerAction(t, k, owner.ID, "chat/send", 0)                              // bearer:provider.example
	_ = createBearerActionSrc(t, k, owner.ID, "mail/inbox", "https://mail.example/api") // bearer:mail.example

	if _, err := k.AttachBearerGrants(ctx, owner.ID, "multi/chat", "", "t1"); err != nil {
		t.Fatalf("connect chat: %v", err)
	}
	if _, err := k.AttachBearerGrants(ctx, owner.ID, "multi/mail", "", "t2"); err != nil {
		t.Fatalf("connect mail: %v", err)
	}

	// Disconnect chat by selector: chat grant gone, mail grant survives, both connections remain.
	revoked, err := k.RevokeGrantsBySelector(ctx, owner.ID, "multi/chat")
	if err != nil || len(revoked) != 1 {
		t.Fatalf("RevokeGrantsBySelector: %v n=%d", err, len(revoked))
	}
	mailPlan, _ := k.ConsentPlan(ctx, owner.ID, "multi/mail")
	if !mailPlan.Groups[0].Actions[0].Granted {
		t.Error("mail grant should survive a chat-selector revoke")
	}
	conns, _ := k.ListConnectionViews(ctx, owner.ID)
	if len(conns) != 2 {
		t.Fatalf("both connections should survive grant revoke, got %d", len(conns))
	}
	unused := 0
	for _, c := range conns {
		if c.Unused {
			unused++
		}
	}
	if unused != 1 {
		t.Errorf("exactly one connection (chat) should be unused, got %d", unused)
	}

	// Disconnect mail by account: connection and its grant cascade away.
	refs, err := k.RevokeConnection(ctx, owner.ID, "bearer:mail.example")
	if err != nil || len(refs) != 1 {
		t.Fatalf("RevokeConnection: %v n=%d", err, len(refs))
	}
	if _, err := k.RevokeConnection(ctx, owner.ID, "bearer:mail.example"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("re-revoking absent connection: got %v, want ErrNotFound", err)
	}
}

// TestUpdateRevokesGrantsRegardlessOfActiveState freezes the rule that standing consent never
// outlives the contract it was given for. A grant survives a plain disable on purpose, so the
// revocation cannot be conditional on the action having been active: otherwise an owner could
// disable an action, repoint it at another upstream host, re-enable it, and the caller's
// credential would travel to a host nobody consented to.
func TestUpdateRevokesGrantsRegardlessOfActiveState(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	ctx := context.Background()

	owner := setupUser(t, st, "revoke-owner", 0)
	grantor := setupUser(t, st, "revoke-caller", 0)

	newDelegatedAction := func(name string) *kernel.Action {
		a, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
			OwnerUserID: owner.ID, Name: name, Kind: kernel.KindHTTP,
			Source: "https://api.example.com/one", Description: "delegated",
			InputSchema:  map[string]any{"type": "object", "description": "in"},
			OutputSchema: map[string]any{"type": "object", "description": "out"},
			Auth:         &kernel.AuthInput{Scheme: kernel.AuthSchemeDelegatedBearer},
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	consent := func(a *kernel.Action) {
		t.Helper()
		g := &kernel.Grant{ID: uuid.New().String(), GrantorUserID: grantor.ID, ActionID: a.ID, CreatedAt: time.Now().UTC()}
		if err := st.CreateOrReplaceGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	granted := func(a *kernel.Action) bool {
		t.Helper()
		_, err := st.ReadGrant(ctx, grantor.ID, a.ID)
		return err == nil
	}

	// A disable leaves consent standing: the contract has not moved.
	kept := newDelegatedAction("kept")
	consent(kept)
	if err := k.SetActive(ctx, owner.ID, kept.ID, false); err != nil {
		t.Fatal(err)
	}
	if !granted(kept) {
		t.Error("a plain disable must not revoke consent")
	}

	// Repointing that disabled action at another host revokes it, even though it was already off.
	moved := "https://other.example.com/two"
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: kept.ID, Source: &moved}); err != nil {
		t.Fatal(err)
	}
	if granted(kept) {
		t.Error("consent survived a source change on a disabled action")
	}

	// The same holds for a schema and a price change on a disabled action.
	for _, tc := range []struct {
		name string
		req  func(id string) kernel.UpdateActionRequest
	}{
		{"schema", func(id string) kernel.UpdateActionRequest {
			return kernel.UpdateActionRequest{ID: id, InputSchema: map[string]any{"type": "object", "description": "new"}}
		}},
		{"price", func(id string) kernel.UpdateActionRequest {
			p := int64(9)
			return kernel.UpdateActionRequest{ID: id, Price: &p}
		}},
	} {
		a := newDelegatedAction("disabled-" + tc.name)
		consent(a)
		if _, err := k.UpdateAction(ctx, owner.ID, tc.req(a.ID)); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if granted(a) {
			t.Errorf("consent survived a %s change on a never-activated action", tc.name)
		}
	}

	// A description is quoted, not executed: it resets stats but leaves consent alone.
	desc := newDelegatedAction("described")
	consent(desc)
	text := "a new description"
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: desc.ID, Description: &text}); err != nil {
		t.Fatal(err)
	}
	if !granted(desc) {
		t.Error("a description change must not revoke consent")
	}
}

// assertLedgerExplainsBalances is the ledger's own check on the accounts table: every movement
// between two accounts is a posting, so what an account holds — spendable or locked inside an open
// process — is exactly what its postings say arrived less what they say left (D4, G1). It reads
// nothing but public store methods, so any settlement path can be followed by it.
func assertLedgerExplainsBalances(t *testing.T, st kernel.Store, userIDs ...string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range userIDs {
		u, err := st.ReadUser(ctx, id)
		if err != nil {
			t.Fatalf("ledger invariant: ReadUser %s: %v", id, err)
		}
		entries, err := st.ListLedgerByUser(ctx, id, 1000, 0)
		if err != nil {
			t.Fatalf("ledger invariant: ListLedgerByUser %s: %v", id, err)
		}
		var posted int64
		for _, e := range entries {
			if e.ToUserID == id {
				posted += e.Amount
			}
			if e.FromUserID == id {
				posted -= e.Amount
			}
		}
		if got := u.Available + u.Locked; got != posted {
			t.Errorf("ledger invariant for %s: available+locked=%d, postings sum to %d", u.Handle, got, posted)
		}
	}
}
