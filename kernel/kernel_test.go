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
	"os"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
	"github.com/google/uuid"
)

func TestMain(m *testing.M) {
	kernel.SetBcryptCostForTesting(4) // bcrypt.MinCost
	os.Exit(m.Run())
}

// testIssuerUserID is a fixed sentinel user ID inserted into every test store.
// All test kernels use this as their IssuerUserID so that receipt FK constraints pass.
const testIssuerUserID = "00000000-0000-0000-0000-000000000001"

func newTestStore(t testing.TB) kernel.Store {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Seed the issuer user so receipt FK constraints are satisfied.
	hash, err := kernel.HashPassword("issuer-password")
	if err != nil {
		t.Fatalf("newTestStore: hash password: %v", err)
	}
	issuer := &kernel.User{
		ID:           testIssuerUserID,
		Handle:       "@_test_issuer",
		Email:        "issuer@test.internal",
		PasswordHash: hash,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := db.CreateUser(context.Background(), issuer); err != nil {
		t.Fatalf("newTestStore: seed issuer user: %v", err)
	}
	return db
}

func newTestKernel(st kernel.Store) *kernel.Kernel {
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	return kernel.New(st, nil, nil, nil, cfg, log.Default())
}

func newTestKernelWithScripts(st kernel.Store, exec kernel.ScriptExecutor) *kernel.Kernel {
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	return kernel.New(st, exec, nil, nil, cfg, log.Default())
}

func testSigningKey() ed25519.PrivateKey {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return priv
}

func setupUser(t *testing.T, st kernel.Store, handle string, balance int64) *kernel.User {
	t.Helper()
	hash, err := kernel.HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	u := &kernel.User{
		ID:           uuid.New().String(),
		Handle:       handle,
		Email:        handle + "@example.com",
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

// setupSys creates the @sys superuser.
// Call this in any test that invokes RegisterRemoteKernel or ImportRemoteAction.
func setupSys(t *testing.T, _ *kernel.Kernel, st kernel.Store) *kernel.User {
	t.Helper()
	return setupUser(t, st, "@sys", 0)
}

func setupProcess(t *testing.T, st kernel.Store, ownerID string, funds int64) *kernel.Process {
	t.Helper()
	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	if err := st.CreateProcess(context.Background(), p, ownerID, funds); err != nil {
		t.Fatalf("setupProcess: %v", err)
	}
	return p
}

// setupOrphanTrace creates an uncommitted root trace (no tx) in the process.
// The trace keeps the process quiescent-open so CreateStep / Call can still run against it.
// actionOwnerID is recorded as the trace's action_owner_id (used for non-owner authority checks).
func setupOrphanTrace(t *testing.T, st kernel.Store, processID, actionOwnerID, callerID string) *kernel.Trace {
	t.Helper()
	tr := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     processID,
		ActionOwnerID: actionOwnerID,
		CallerUserID:  callerID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := st.BeginRootCall(context.Background(), processID, tr, 0); err != nil {
		t.Fatalf("setupOrphanTrace: %v", err)
	}
	return tr
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

	owner := setupUser(t, st, "@owner", 0)
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

	owner := setupUser(t, st, "@sys", 0)
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

func TestActivateNativeActionBootstrapPath(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@sys", 0)
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
	if err := k.ActivateNativeAction(ctx, a.ID, desc, in, out, 0); err != nil {
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

func TestActivateNativeActionReconcilesSchema(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@sys", 0)
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
	if err := k.ActivateNativeAction(ctx, a.ID, "new desc", newIn, newOut, 0); err != nil {
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

	owner := setupUser(t, st, "@sys", 0)
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
	if err := k.ActivateNativeAction(ctx, a.ID, "desc", badIn, nil, 0); !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Fatalf("ActivateNativeAction with missing schema descriptions: got %v, want ErrSchemaViolation", err)
	}
}

func TestActivateNativeActionReconcilesPrice(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@sys", 0)
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
	if err := k.ActivateNativeAction(ctx, a.ID, "desc", in, out, 5); err != nil {
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
		Handle:   "@alice",
		Email:    "alice@example.com",
		Password: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Handle != "@alice" {
		t.Errorf("handle: got %q, want %q", u.Handle, "@alice")
	}
	if u.PasswordHash == "secret" {
		t.Error("password should be hashed")
	}
}

func TestLogin(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	_, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   "@bob",
		Email:    "bob@example.com",
		Password: "mypass",
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := k.Login(ctx, "@bob", "mypass")
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
	u, _ := k.ReadUserByHandle(ctx, "@bob")
	if subjectID != u.ID {
		t.Errorf("token subject: got %q, want %q", subjectID, u.ID)
	}

	if _, err := k.Login(ctx, "@bob", "wrong"); err == nil {
		t.Error("expected error for wrong password")
	}
}

func TestLoginRejectsRemotePeer(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, err := k.AddPeer(ctx, sys.ID, "@peer", base64.RawURLEncoding.EncodeToString(pub), "https://peer.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := k.Login(ctx, "@peer", "remote"); err == nil {
		t.Error("Login must reject remote kernel peers")
	}
}

// ---- Process tests ----

func TestStartAndEndProcess(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@owner", 1000)

	p := setupProcess(t, st, owner.ID, 500)
	proc, err := st.ReadProcess(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proc.Available != 500 {
		t.Errorf("process.available: got %d, want 500", proc.Available)
	}

	u, _ := st.ReadUser(ctx, owner.ID)
	if u.Available != 500 {
		t.Errorf("owner balance after funding: got %d, want 500", u.Available)
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

	alice := setupUser(t, st, "@alice-proc", 500)
	bob := setupUser(t, st, "@bob-proc", 0)

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

func TestProcessAvailablePlusLockedInvariant(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 2000)
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

	p := setupProcess(t, st, alice.ID, 500)

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

	checkInvariant("initial", 500)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, IsRootCall: true,
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

	alice := setupUser(t, st, "@alice", 1000)

	checkUser := func(tag string, wantAvail, wantLocked int64) {
		t.Helper()
		u, err := st.ReadUser(ctx, alice.ID)
		if err != nil {
			t.Fatalf("%s: ReadUser: %v", tag, err)
		}
		if u.Available != wantAvail {
			t.Errorf("%s: user.available got %d, want %d", tag, u.Available, wantAvail)
		}
		if u.Locked != wantLocked {
			t.Errorf("%s: user.locked got %d, want %d", tag, u.Locked, wantLocked)
		}
	}

	checkUser("initial", 1000, 0)

	// CreateProcess: deducts 500 from user.available → user.locked.
	p := setupProcess(t, st, alice.ID, 500)
	checkUser("after CreateProcess(500)", 500, 500)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "svc",
		Kind: kernel.KindWasm, Active: true, Price: 100,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	if _, err := k.Call(ctx, kernel.CallRequest{
		CallerID: alice.ID, ProcessID: p.ID, IsRootCall: true,
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

	owner := setupUser(t, st, "@owner", 0)
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

	buyer := setupUser(t, st, "@rate-buyer", 1000)
	provider := setupUser(t, st, "@rate-provider", 0)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: provider.ID,
		Name:        "rate-svc",
		Kind:        kernel.KindWasm,
		Active:      true,
		Public:      true,
		Price:       0,
		Source:      "wat",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p := setupProcess(t, st, buyer.ID, 100)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: buyer.ID, ProcessID: p.ID, IsRootCall: true,
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

func TestRateTransactionAlreadyRatedRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	buyer := setupUser(t, st, "@rerate-buyer", 500)
	provider := setupUser(t, st, "@rerate-provider", 0)
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: provider.ID,
		Name:        "rerate-svc",
		Kind:        kernel.KindWasm,
		Active:      true,
		Public:      true,
		Price:       0,
		Source:      "wat",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p := setupProcess(t, st, buyer.ID, 100)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: buyer.ID, ProcessID: p.ID, IsRootCall: true,
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

	owner := setupUser(t, st, "@self-rater", 500)
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

	p := setupProcess(t, st, owner.ID, 100)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: owner.ID, ProcessID: p.ID, IsRootCall: true,
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

// TestCallPreconditionOrderParentTraceAfterAction verifies §4 ordering: action
// existence (step 4) must be checked before parent trace existence (step 8).
// C == P, invalid ParentTraceID, non-existent action → must return ErrNotFound
// for the action, not ErrInvalidInput for the trace.
func TestCallPreconditionOrderParentTraceAfterAction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@ptrace-owner", 100)
	p := setupProcess(t, st, owner.ID, 50)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID:      owner.ID,
		ProcessID:     p.ID,
		ParentTraceID: "nonexistent-trace-id",
		TargetUserID:  owner.ID,
		ActionName:    "no-such-action",
		Args:          map[string]any{},
	})
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing action (before trace check), got: %v", err)
	}
}

// ---- Deposit enforcement tests ----

func TestDepositNonSuperuserRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	su := setupUser(t, st, "@sys", 0)
	regular := setupUser(t, st, "@regular", 0)
	recipient := setupUser(t, st, "@recipient", 0)

	// Superuser can deposit.
	if _, err := k.Deposit(ctx, su.ID, recipient.ID, 100, "ok"); err != nil {
		t.Fatalf("superuser deposit: %v", err)
	}
	// Regular user cannot deposit.
	if _, err := k.Deposit(ctx, regular.ID, recipient.ID, 100, "bad"); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for non-superuser deposit, got %v", err)
	}
}

// ---- Receipt tests ----

func TestReceiptCreatedWithCall(t *testing.T) {
	st := newTestStore(t)
	su := setupUser(t, st, "@sys", 0)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	caller := setupUser(t, st, "@rcpt-caller", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: caller.ID, Name: "rcpt-svc",
		Kind: kernel.KindWasm, Active: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p := setupProcess(t, st, caller.ID, 200)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall: true,
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
	su := setupUser(t, st, "@sys", 0)
	k := newTestKernelWithScripts(st, &fakeScriptExec{err: kernel.ErrExecutionFailed.Wrap("boom")})
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	owner := setupUser(t, st, "@fail-owner", 500)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "fail-svc",
		Kind: kernel.KindWasm, Active: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p := setupProcess(t, st, owner.ID, 200)
	reply, _ := k.Call(ctx, kernel.CallRequest{
		CallerID: owner.ID, ProcessID: p.ID, IsRootCall: true,
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
	su := setupUser(t, st, "@sys", 0)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	owner := setupUser(t, st, "@provider", 500) // seller
	caller := setupUser(t, st, "@buyer", 500)   // buyer
	other := setupUser(t, st, "@other", 500)    // non-party
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "pvd-svc",
		Kind: kernel.KindWasm, Active: true, Public: true, Price: 10, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p := setupProcess(t, st, caller.ID, 100)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall: true,
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

	owner := setupUser(t, st, "@no-receipt-owner", 100)
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
	p := setupProcess(t, st, owner.ID, 50)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: owner.ID, ProcessID: p.ID, IsRootCall: true,
		TargetUserID: owner.ID, ActionName: "no-receipt", Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}
	if exec.calls != 0 {
		t.Fatalf("action executed despite missing receipt signing: %d calls", exec.calls)
	}
	got, _ := st.ReadProcess(ctx, p.ID)
	if got.Available != 50 || got.Locked != 0 {
		t.Fatalf("funds changed before receipt precondition: available=%d locked=%d", got.Available, got.Locked)
	}
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	if len(txs) != 0 {
		t.Fatalf("transaction created despite missing receipt signing: %d", len(txs))
	}
}

func TestManifestSigningRequiresConfiguredKey(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	k.SetSigningKey(nil, "issuer-id")
	ctx := context.Background()

	owner := setupUser(t, st, "@manifest-owner", 0)
	a := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "manifest",
		Kind:         kernel.KindHTTP,
		Active:       true,
		Public:       true,
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

	buyer := setupUser(t, st, "@rr-buyer", 500)
	provider := setupUser(t, st, "@rr-provider", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "rr-svc",
		Kind: kernel.KindWasm, Active: true, Public: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p := setupProcess(t, st, buyer.ID, 100)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: buyer.ID, ProcessID: p.ID, IsRootCall: true,
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

	buyer := setupUser(t, st, "@dup-buyer", 500)
	provider := setupUser(t, st, "@dup-provider", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "dup-svc",
		Kind: kernel.KindWasm, Active: true, Public: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p := setupProcess(t, st, buyer.ID, 100)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: buyer.ID, ProcessID: p.ID, IsRootCall: true,
		TargetUserID: provider.ID, ActionName: "dup-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if _, err := k.RateTransaction(ctx, buyer.ID, reply.TxID, 1.0, nil); err != nil {
		t.Fatalf("first RateTransaction: %v", err)
	}
	if _, err := k.RateTransaction(ctx, buyer.ID, reply.TxID, 0.0, nil); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for duplicate rating, got %v", err)
	}
}

func TestTransactionViewEmbeddedRating(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	buyer := setupUser(t, st, "@tv-buyer", 500)
	provider := setupUser(t, st, "@tv-provider", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "tv-svc",
		Kind: kernel.KindWasm, Active: true, Public: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p := setupProcess(t, st, buyer.ID, 100)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: buyer.ID, ProcessID: p.ID, IsRootCall: true,
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

// ---- Zero-credit process tests ----

func TestZeroCreditProcess(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	owner := setupUser(t, st, "@zero-owner", 100)

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

	owner := setupUser(t, st, "@sd-owner", 0)

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

func TestSetActiveRequiresDescription(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "@desc-owner", 0)

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
	owner := setupUser(t, st, "@desc-prop-owner", 0)

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
	owner := setupUser(t, st, "@alice", 0)

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
	owner := setupUser(t, st, "@alice", 0)

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

	userA := setupUser(t, st, "@user-a", 0)
	userB := setupUser(t, st, "@user-b", 0)

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


// ---- OpenAPI well-known ownership proof tests ----

// fakeURLFetcher implements both HTTPExecutor and URLFetcher for testing ownership proof.
type fakeURLFetcher struct {
	wellKnown map[string]string // URL -> response body
}

func (f *fakeURLFetcher) Execute(_ context.Context, _ *kernel.Action, _ map[string]any) (map[string]any, error) {
	return nil, kernel.ErrInvalidState.Wrap("not used in tests")
}

func (f *fakeURLFetcher) FetchURL(_ context.Context, rawURL string) ([]byte, error) {
	if body, ok := f.wellKnown[rawURL]; ok {
		return []byte(body), nil
	}
	return nil, kernel.ErrNotFound.Wrapf("URL not found: %s", rawURL)
}

func newTestKernelWithHTTP(st kernel.Store, http kernel.HTTPExecutor) *kernel.Kernel {
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	return kernel.New(st, nil, http, nil, cfg, log.Default())
}

// ---- Remote proxy execution test ----

// fakeFederationHTTP implements HTTPExecutor and FederationExecutor for Call() tests.
// fakeSuccessHTTP is a minimal HTTPExecutor that returns an empty result for any Execute call.
type fakeSuccessHTTP struct{}

func (f *fakeSuccessHTTP) Execute(_ context.Context, _ *kernel.Action, _ map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

type fakeFederationHTTP struct {
	result      map[string]any
	receiptJSON string
}

func (f *fakeFederationHTTP) Execute(_ context.Context, _ *kernel.Action, _ map[string]any) (map[string]any, error) {
	return nil, kernel.ErrInvalidState.Wrap("not used in federation tests")
}

func (f *fakeFederationHTTP) ExecuteFederation(_ context.Context, _, _ string, _ map[string]any) (kernel.FederationResult, error) {
	result := f.result
	if result == nil {
		result = map[string]any{}
	}
	return kernel.FederationResult{Result: result, ReceiptJSON: f.receiptJSON, HTTPStatus: 200}, nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
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
// Ed25519 over CanonicalJSON of the receipt with Signature cleared.
func signReceiptForTest(t *testing.T, key ed25519.PrivateKey, r *kernel.Receipt) string {
	t.Helper()
	cp := *r
	cp.Signature = ""
	payload, err := kernel.CanonicalJSON(cp)
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
	buyer := setupUser(t, st, "@su-buyer", 500)
	provider := setupUser(t, st, "@su-provider", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "su-svc",
		Kind: kernel.KindWasm, Active: true, Public: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	p := setupProcess(t, st, buyer.ID, 100)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: buyer.ID, ProcessID: p.ID, IsRootCall: true,
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
		t.Errorf("@sys ListTransactions did not return transaction %s", reply.TxID)
	}
}

func TestReadCallableAction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@owner", 0)
	other := setupUser(t, st, "@other", 0)

	baseSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": map[string]any{"type": "string", "description": "x"}},
	}

	pubAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "pub-action",
		Kind: kernel.KindNative, Active: true, Public: true,
		Description: "public", Source: "native",
		InputSchema: baseSchema, OutputSchema: baseSchema,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	privAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "priv-action",
		Kind: kernel.KindNative, Active: true, Public: false,
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
		a, err := k.ReadCallableAction(ctx, "@owner", "pub-action", other.ID)
		if err != nil {
			t.Fatalf("expected public action to be callable by other: %v", err)
		}
		if a.ID != pubAction.ID {
			t.Errorf("wrong action returned")
		}
	})

	t.Run("private action visible only to its owner process", func(t *testing.T) {
		a, err := k.ReadCallableAction(ctx, "@owner", "priv-action", owner.ID)
		if err != nil {
			t.Fatalf("expected private action callable by own process: %v", err)
		}
		if a.ID != privAction.ID {
			t.Errorf("wrong action returned")
		}
	})

	t.Run("private action not callable by foreign process", func(t *testing.T) {
		_, err := k.ReadCallableAction(ctx, "@owner", "priv-action", other.ID)
		if err == nil {
			t.Error("expected error: private action should not be callable by other process")
		}
	})

	t.Run("unknown action returns not-found error", func(t *testing.T) {
		_, err := k.ReadCallableAction(ctx, "@owner", "no-such-action", owner.ID)
		if err == nil {
			t.Error("expected error for missing action")
		}
	})
}

func TestRunInputSchemaRejectionLeavesNoProcess(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice-run-schema", 500)
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

	_, err := k.Run(ctx, alice.ID, "@alice-run-schema/schema-guarded", map[string]any{"wrong_field": "x"})
	if err == nil {
		t.Fatal("expected schema validation error")
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

	bob := setupUser(t, st, "@bob-run-nokey", 200)
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

	_, err := k.Run(ctx, bob.ID, "@bob-run-nokey/no-key", map[string]any{})
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

// Ensure fmt is used.
var _ = fmt.Sprintf
