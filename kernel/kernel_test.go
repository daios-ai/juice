package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

func newTestKernel(st Store) *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.FeeBPS = 2000
	cfg.IssuerUserID = "test-issuer-id"
	cfg.SigningKey = testSigningKey()
	return New(st, nil, nil, nil, nil, cfg, log.Default())
}

func newTestKernelWithScripts(st Store, exec ScriptExecutor) *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.FeeBPS = 2000
	cfg.IssuerUserID = "test-issuer-id"
	cfg.SigningKey = testSigningKey()
	return New(st, exec, nil, nil, nil, cfg, log.Default())
}


func testSigningKey() ed25519.PrivateKey {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return priv
}

func setupUser(t *testing.T, st *fakeStore, handle string, balance int64) *User {
	t.Helper()
	hash, err := HashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	u := &User{
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

func setupAction(t *testing.T, st *fakeStore, ownerID, name string, price int64) *Action {
	t.Helper()
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Name:        name,
		Kind:        KindNative,
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

func setupProcess(t *testing.T, k *Kernel, ownerID string, funds int64) (*Process, *Trace) {
	t.Helper()
	p, tr, err := k.StartProcess(context.Background(), ownerID, funds)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
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

func (p *panicScriptExec) Execute(_ context.Context, _ []byte, _ []byte, _ HostFunctions) ([]byte, error) {
	panic("simulated wasm panic")
}

func (f *fakeScriptExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (f *fakeScriptExec) Execute(_ context.Context, _ []byte, input []byte, _ HostFunctions) ([]byte, error) {
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
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@owner", 0)
	_, err := k.CreateAction(ctx, CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/native-attempt",
		Kind:        KindNative,
	})
	if err == nil {
		t.Error("expected error creating native action via CreateAction, got nil")
	}
}

func TestNativeActionNormalLifecycleRejected(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@sys", 0)
	a, err := k.RegisterNativeAction(ctx, CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/native",
		Kind:        KindNative,
	})
	if err != nil {
		t.Fatal(err)
	}

	price := int64(1)
	if _, err := k.UpdateAction(ctx, owner.ID, UpdateActionRequest{ID: a.ID, Price: &price}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("UpdateAction native error: got %v, want ErrUnauthorized", err)
	}
	if err := k.SetActive(ctx, owner.ID, a.ID, true); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("SetActive native error: got %v, want ErrUnauthorized", err)
	}
	if err := k.DeleteAction(ctx, owner.ID, a.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("DeleteAction native error: got %v, want ErrUnauthorized", err)
	}
}

func TestActivateNativeActionBootstrapPath(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@sys", 0)
	a, err := k.RegisterNativeAction(ctx, CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/native",
		Kind:        KindNative,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Active {
		t.Fatal("registered native action should start inactive")
	}
	if err := k.ActivateNativeAction(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	active, err := k.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !active.Active {
		t.Fatal("ActivateNativeAction should activate native action")
	}
	if stats, err := k.ReadStats(ctx, a.ID); err != nil || stats == nil {
		t.Fatalf("ActivateNativeAction should initialize stats, stats=%v err=%v", stats, err)
	}
}

func TestCreateUser(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	u, err := k.CreateUser(ctx, CreateUserRequest{
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
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	_, err := k.CreateUser(ctx, CreateUserRequest{
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
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, err := k.RegisterRemoteKernel(ctx, "@peer", base64.RawURLEncoding.EncodeToString(pub), "https://peer.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := k.Login(ctx, "@peer", "remote"); err == nil {
		t.Error("Login must reject remote kernel peers")
	}
}

// ---- Process tests ----

func TestStartAndEndProcess(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@owner", 1000)

	p, root, err := k.StartProcess(ctx, owner.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available != 500 {
		t.Errorf("process.available: got %d, want 500", p.Available)
	}
	if root.ParentTraceID != root.ID {
		t.Error("root trace must have ParentTraceID == ID")
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
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	alice := setupUser(t, st, "@alice-proc", 500)
	bob := setupUser(t, st, "@bob-proc", 0)

	p, _, err := k.StartProcess(ctx, alice.ID, 100)
	if err != nil {
		t.Fatal(err)
	}

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
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	alice := setupUser(t, st, "@alice", 2000)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: alice.ID,
		Name:        "/svc",
		Kind:        KindWasm,
		Active:      true,
		Price:       100,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, alice.ID, 500)

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

	_, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	proc, _ := st.ReadProcess(ctx, p.ID)
	if proc.Locked != 0 {
		t.Errorf("after call: locked must be 0, got %d", proc.Locked)
	}

	if err := k.FundProcess(ctx, alice.ID, p.ID, 300); err != nil {
		t.Fatal(err)
	}
	checkInvariant("after fund", -1)

	proc, _ = st.ReadProcess(ctx, p.ID)
	if proc.Locked != 0 {
		t.Errorf("after fund: locked must be 0, got %d", proc.Locked)
	}

	if err := k.EndProcess(ctx, alice.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	checkInvariant("after end", 0)
}

func TestUserLockedBalanceInvariant(t *testing.T) {
	st := newFakeStore()
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

	p, root, _ := k.StartProcess(ctx, alice.ID, 500)
	checkUser("after StartProcess(500)", 500, 500)

	if err := k.FundProcess(ctx, alice.ID, p.ID, 200); err != nil {
		t.Fatal(err)
	}
	checkUser("after FundProcess(200)", 300, 700)

	a := &Action{
		ID: uuid.New().String(), OwnerUserID: alice.ID, Name: "/svc",
		Kind: KindWasm, Active: true, Price: 100,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	if _, err := k.Call(ctx, CallRequest{
		SubjectID: alice.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: alice.ID, ActionName: "/svc", Args: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	// Call settles: user.locked decreases by gross (100); alice also receives net as target.
	u, _ := st.ReadUser(ctx, alice.ID)
	if u.Locked != 600 {
		t.Errorf("after Call: user.locked got %d, want 600", u.Locked)
	}
	if u.Locked < 0 {
		t.Errorf("after Call: user.locked is negative: %d", u.Locked)
	}

	if err := k.EndProcess(ctx, alice.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	// EndProcess returns process.available (600) to user; user.locked must reach 0.
	u, _ = st.ReadUser(ctx, alice.ID)
	if u.Locked != 0 {
		t.Errorf("after EndProcess: user.locked got %d, want 0", u.Locked)
	}
	if u.Available < 0 {
		t.Errorf("after EndProcess: user.available is negative: %d", u.Available)
	}
}

// ---- Stats tests ----

func TestIncrementalMean(t *testing.T) {
	var mean float64
	for i, v := range []float64{10, 20, 30} {
		mean = IncrementalMean(mean, int64(i), v)
	}
	if math.Abs(mean-20) > 1e-9 {
		t.Errorf("expected mean 20, got %f", mean)
	}
}

func TestUpdateStats(t *testing.T) {
	s := DefaultStats("action-1")
	now := time.Now()

	success := &Transaction{
		Status:    TxSuccess,
		Gross:     100,
		StartedAt: now.Add(-500 * time.Millisecond),
		EndedAt:   now,
	}
	UpdateStats(s, success, 0.5)

	if s.Uses != 1 {
		t.Errorf("uses: got %d, want 1", s.Uses)
	}
	if s.Successes != 1 || s.Failures != 0 {
		t.Errorf("successes=%d failures=%d, want 1/0", s.Successes, s.Failures)
	}
	if math.Abs(float64(s.PriceMean)-100) > 1e-6 {
		t.Errorf("price_mean: got %f, want 100", s.PriceMean)
	}
	if math.Abs(s.LatencyMean-0.5) > 1e-6 {
		t.Errorf("latency_mean: got %f, want 0.5", s.LatencyMean)
	}

	UpdateStats(s, &Transaction{Status: TxFailure, StartedAt: now, EndedAt: now}, 0.2)
	if s.Uses != 2 || s.Failures != 1 {
		t.Errorf("after failure: uses=%d failures=%d, want 2/1", s.Uses, s.Failures)
	}
	if math.Abs(float64(s.PriceMean)-100) > 1e-6 {
		t.Error("price_mean should not change on failure")
	}
}

func TestUpdateStatsZeroPrice(t *testing.T) {
	s := DefaultStats("action-zp")
	now := time.Now()

	// First call: price=100
	UpdateStats(s, &Transaction{Status: TxSuccess, Gross: 100, StartedAt: now, EndedAt: now}, 0.1)
	// Second call: price=0 (zero-price)
	UpdateStats(s, &Transaction{Status: TxSuccess, Gross: 0, StartedAt: now, EndedAt: now}, 0.1)

	if s.Successes != 2 {
		t.Fatalf("successes: got %d, want 2", s.Successes)
	}
	// Mean of [100, 0] = 50
	if math.Abs(float64(s.PriceMean)-50) > 1e-6 {
		t.Errorf("price_mean: got %f, want 50", s.PriceMean)
	}
}

var _ = log.Default

func TestValidateHTTPSourceSSRF(t *testing.T) {
	rejected := []string{
		"http://localhost/api",
		"http://127.0.0.1/secret",
		"http://::1/secret",
		"http://10.0.0.1/internal",
		"http://192.168.1.1/router",
		"http://172.16.0.1/internal",
		"http://169.254.169.254/latest/meta-data/",
		"ftp://example.com/file",
		"file:///etc/passwd",
		"://broken",
	}
	for _, u := range rejected {
		if err := validateHTTPSource(u, false); err == nil {
			t.Errorf("validateHTTPSource(%q): expected error, got nil", u)
		}
	}

	accepted := []string{
		"https://example.com/api",
		"http://example.com/webhook",
		"https://api.stripe.com/v1/charges",
	}
	for _, u := range accepted {
		if err := validateHTTPSource(u, false); err != nil {
			t.Errorf("validateHTTPSource(%q): unexpected error: %v", u, err)
		}
	}
}

func TestCreateHTTPActionRejectsSSRFURL(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@owner", 0)
	_, err := k.CreateAction(ctx, CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/webhook",
		Kind:        KindHTTP,
		Source:      "http://169.254.169.254/latest/meta-data/",
	})
	if err == nil {
		t.Error("expected error creating HTTP action with SSRF URL")
	}
}

// ---- Event / Listener tests ----

type failingSettleStore struct {
	*fakeStore
}

func (f *failingSettleStore) SettleEvent(_ context.Context, _, _ string) error {
	return ErrInternal.Wrap("injected settle failure")
}

func TestConsumeEventSettleFailureReturnsError(t *testing.T) {
	base := newFakeStore()
	failing := &failingSettleStore{fakeStore: base}
	k := newTestKernelWithScripts(failing, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, base, "@settle-owner", 1000)

	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: owner.ID,
		Name:        "/settle-svc",
		Kind:        KindWasm,
		Active:      true,
		Price:       10,
		Source:      "wat",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = base.CreateAction(ctx, a)

	l := &Listener{
		ID:             uuid.New().String(),
		OwnerUserID:    owner.ID,
		SourceUserID:   owner.ID,
		EventName:      "settle.test",
		TargetActionID: a.ID,
		Active:         true,
		CreatedAt:      time.Now().UTC(),
	}
	_ = base.CreateListener(ctx, l)

	e := &Event{
		ID:             uuid.New().String(),
		ListenerID:     l.ID,
		ArgsJSON:       `{}`,
		CausingTraceID: "",
		CreatedAt:      time.Now().UTC(),
	}
	_ = base.CreateEvent(ctx, e)

	p, _, _ := k.StartProcess(ctx, owner.ID, 500)

	_, err := k.ConsumeEvent(ctx, owner.ID, e.ID, p.ID)
	if err == nil {
		t.Fatal("expected error from failing SettleEvent, got nil")
	}
	var ke *KernelError
	if !errors.As(err, &ke) || ke.Code != "internal" {
		t.Errorf("want ErrInternal, got: %v", err)
	}
}

func TestDeleteListenerPurgesEventsAtomically(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@dl-owner", 100)
	a := setupAction(t, st, owner.ID, "/lookup", 0)

	l := &Listener{
		ID:             uuid.New().String(),
		OwnerUserID:    owner.ID,
		SourceUserID:   owner.ID,
		EventName:      "dl.test",
		TargetActionID: a.ID,
		Active:         true,
		CreatedAt:      time.Now().UTC(),
	}
	_ = st.CreateListener(ctx, l)

	pending1 := &Event{ID: uuid.New().String(), ListenerID: l.ID, ArgsJSON: `{}`, CreatedAt: time.Now().UTC()}
	pending2 := &Event{ID: uuid.New().String(), ListenerID: l.ID, ArgsJSON: `{}`, CreatedAt: time.Now().UTC()}
	inFlight := &Event{ID: uuid.New().String(), ListenerID: l.ID, ArgsJSON: `{}`, CreatedAt: time.Now().UTC()}
	_ = st.CreateEvent(ctx, pending1)
	_ = st.CreateEvent(ctx, pending2)
	_ = st.CreateEvent(ctx, inFlight)
	// Mark inFlight as consumed (in-flight — ConsumedAt set, TxID nil).
	_ = st.LockEvent(ctx, inFlight.ID)

	if err := k.DeleteListener(ctx, owner.ID, l.ID); err != nil {
		t.Fatalf("DeleteListener: %v", err)
	}

	got, _ := st.ReadListener(ctx, l.ID)
	if got.Active {
		t.Error("listener should be inactive after delete")
	}

	pending, _ := st.ListPendingEvents(ctx, l.ID)
	if len(pending) != 0 {
		t.Errorf("pending events after delete: got %d, want 0", len(pending))
	}

	// In-flight event is consumed (ConsumedAt set), so it is preserved.
	_, err := st.ReadEvent(ctx, inFlight.ID)
	if err != nil {
		t.Errorf("in-flight event should be preserved after delete: %v", err)
	}
}

func TestRateTransactionUpdatesActionStats(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@rate-owner", 1000)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: owner.ID,
		Name:        "/rate-svc",
		Kind:        KindWasm,
		Active:      true,
		Price:       0,
		Source:      "wat",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, owner.ID, 100)
	reply, err := k.Call(ctx, CallRequest{
		SubjectID: owner.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: owner.ID, ActionName: "/rate-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	const rating = 1.0
	if _, err := k.RateTransaction(ctx, owner.ID, reply.TxID, rating); err != nil {
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
	if stats.RatingMean != rating {
		t.Errorf("RatingMean: got %f, want %f", stats.RatingMean, rating)
	}
}

func TestRateTransactionAlreadyRatedRejected(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@rerate-owner", 500)
	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: owner.ID,
		Name:        "/rerate-svc",
		Kind:        KindWasm,
		Active:      true,
		Price:       0,
		Source:      "wat",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, owner.ID, 100)
	reply, err := k.Call(ctx, CallRequest{
		SubjectID: owner.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: owner.ID, ActionName: "/rerate-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if _, err := k.RateTransaction(ctx, owner.ID, reply.TxID, 1.0); err != nil {
		t.Fatalf("first RateTransaction: %v", err)
	}
	_, err = k.RateTransaction(ctx, owner.ID, reply.TxID, 0.0)
	if err == nil {
		t.Fatal("expected error on second rating, got nil")
	}
	ke, ok := err.(*KernelError)
	if !ok || ke.Code != "invalid_input" {
		t.Errorf("expected invalid_input error, got %v", err)
	}
}

// ---- Deposit enforcement tests ----

func TestDepositNonSuperuserRejected(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	su := setupUser(t, st, "@sys", 0)
	st.config["superuser_handle"] = "@sys"
	regular := setupUser(t, st, "@regular", 0)
	recipient := setupUser(t, st, "@recipient", 0)

	// Superuser can deposit.
	if _, err := k.Deposit(ctx, su.ID, recipient.ID, 100, "ok"); err != nil {
		t.Fatalf("superuser deposit: %v", err)
	}
	// Regular user cannot deposit.
	if _, err := k.Deposit(ctx, regular.ID, recipient.ID, 100, "bad"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for non-superuser deposit, got %v", err)
	}
}

// ---- Receipt tests ----

func TestReceiptCreatedWithCall(t *testing.T) {
	st := newFakeStore()
	su := setupUser(t, st, "@sys", 0)
	st.config["superuser_handle"] = "@sys"

	exec := &fakeScriptExec{result: `{"ok":true}`}
	k := newTestKernelWithScripts(st, exec)
	k.SetSigningKey(testSigningKey(), su.ID, "@sys")
	ctx := context.Background()

	caller := setupUser(t, st, "@rcpt-caller", 500)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: caller.ID, Name: "/rcpt-svc",
		Kind: KindWasm, Active: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_ = st.GrantACL(ctx, &ACLEntry{SubjectUserID: caller.ID, ActionID: a.ID, Permission: PermCall, CreatedAt: time.Now().UTC()})

	p, root, _ := k.StartProcess(ctx, caller.ID, 200)
	reply, err := k.Call(ctx, CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: caller.ID, ActionName: "/rcpt-svc", Args: map[string]any{},
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
	st := newFakeStore()
	su := setupUser(t, st, "@sys", 0)
	st.config["superuser_handle"] = "@sys"

	exec := &fakeScriptExec{err: ErrExecutionFailed.Wrap("boom")}
	k := newTestKernelWithScripts(st, exec)
	k.SetSigningKey(testSigningKey(), su.ID, "@sys")
	ctx := context.Background()

	owner := setupUser(t, st, "@fail-owner", 500)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/fail-svc",
		Kind: KindWasm, Active: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, owner.ID, 200)
	reply, _ := k.Call(ctx, CallRequest{
		SubjectID: owner.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: owner.ID, ActionName: "/fail-svc", Args: map[string]any{},
	})

	// Find the failed transaction.
	txs, _ := st.ListTransactions(ctx, TxFilter{ProcessID: p.ID})
	if len(txs) == 0 {
		t.Fatal("expected a transaction for failed call")
	}
	_ = reply

	var failTxID string
	for _, tx := range txs {
		if tx.Status == TxFailure {
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
	if r.Status != TxFailure {
		t.Errorf("receipt.Status: got %q, want %q", r.Status, TxFailure)
	}
}

func TestReceiptSigningRequiresConfiguredKey(t *testing.T) {
	k := newTestKernel(newFakeStore())
	k.SetSigningKey(nil, "issuer-id", "@sys")

	_, err := k.buildReceipt(&Transaction{
		ID:        "tx-id",
		TraceID:   "trace-id",
		ActionID:  "action-id",
		ArgsJSON:  `{}`,
		ReplyJSON: `{}`,
		Status:    TxSuccess,
		EndedAt:   time.Now().UTC(),
	})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState without signing key, got %v", err)
	}
}

func TestCallRequiresReceiptSigningBeforeExecution(t *testing.T) {
	st := newFakeStore()
	exec := &fakeScriptExec{result: `{"ok":true}`}
	k := newTestKernelWithScripts(st, exec)
	k.SetSigningKey(nil, "issuer-id", "@sys")
	ctx := context.Background()

	owner := setupUser(t, st, "@no-receipt-owner", 100)
	a := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "/no-receipt",
		Kind:         KindWasm,
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
	p, root, _ := k.StartProcess(ctx, owner.ID, 50)

	_, err := k.Call(ctx, CallRequest{
		SubjectID: owner.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: owner.ID, ActionName: "/no-receipt", Args: map[string]any{},
	})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}
	if exec.calls != 0 {
		t.Fatalf("action executed despite missing receipt signing: %d calls", exec.calls)
	}
	got, _ := st.ReadProcess(ctx, p.ID)
	if got.Available != 50 || got.Locked != 0 {
		t.Fatalf("funds changed before receipt precondition: available=%d locked=%d", got.Available, got.Locked)
	}
	txs, _ := st.ListTransactions(ctx, TxFilter{ProcessID: p.ID})
	if len(txs) != 0 {
		t.Fatalf("transaction created despite missing receipt signing: %d", len(txs))
	}
}

func TestManifestSigningRequiresConfiguredKey(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	k.SetSigningKey(nil, "issuer-id", "@sys")
	ctx := context.Background()

	owner := setupUser(t, st, "@manifest-owner", 0)
	a := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "/manifest",
		Kind:         KindHTTP,
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
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState without signing key, got %v", err)
	}
}

func TestRatingSigningRequiresConfiguredKey(t *testing.T) {
	_, err := signRating(nil, &Rating{
		ID:          "rating-id",
		RatedTxID:   "tx-id",
		RaterUserID: "user-id",
		Rating:      1,
		CreatedAt:   time.Now().UTC(),
	})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState without signing key, got %v", err)
	}
}

func TestRegisterRemoteKernelValidatesIdentity(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	if _, err := k.RegisterRemoteKernel(ctx, "@bad-key", "not-base64url", "https://remote.example.com"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for malformed public key, got %v", err)
	}

	shortKey := base64.RawURLEncoding.EncodeToString([]byte("short"))
	if _, err := k.RegisterRemoteKernel(ctx, "@short-key", shortKey, "https://remote.example.com"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for short public key, got %v", err)
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	validKey := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.RegisterRemoteKernel(ctx, "@bad-url", validKey, "ftp://remote.example.com"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for unsupported URL scheme, got %v", err)
	}
	if _, err := k.RegisterRemoteKernel(ctx, "@remote", validKey, "https://remote.example.com"); err != nil {
		t.Fatalf("valid remote kernel should register: %v", err)
	}
}

// ---- Remote proxy / manifest tests ----

func TestImportRemoteActionCreatesRemoteProxy(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.RegisterRemoteKernel(ctx, "@remote-peer", base64.RawURLEncoding.EncodeToString(pub), "https://remote.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := ActionManifest{
		ActionID:    "remote-action-id-1",
		OwnerHandle: "@remote-peer",
		Name:        "/sum",
		Kind:        KindHTTP,
		Price:       50,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	}
	sig, err := SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig
	result, err := k.ImportRemoteAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportRemoteAction: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(result.Created))
	}
	a := result.Created[0]
	if a.Kind != KindRemoteProxy {
		t.Errorf("kind: got %q, want %q", a.Kind, KindRemoteProxy)
	}
	if a.RemoteActionID != m.ActionID {
		t.Errorf("remote_action_id: got %q, want %q", a.RemoteActionID, m.ActionID)
	}
	if a.Price != 50 {
		t.Errorf("price: got %d, want 50", a.Price)
	}
}

func TestImportRemoteActionReimp(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.RegisterRemoteKernel(ctx, "@reimp-peer", base64.RawURLEncoding.EncodeToString(pub), "https://reimp.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := ActionManifest{
		ActionID:    "reimp-action-id",
		OwnerHandle: "@reimp-peer",
		Name:        "/calc",
		Kind:        KindHTTP,
		Price:       10,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	}
	sig, err := SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig
	firstResult, err := k.ImportRemoteAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if len(firstResult.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(firstResult.Created))
	}
	firstID := firstResult.Created[0].ID

	// Reimport with updated price — content hash changes → Updated.
	m.Price = 99
	sig2, err := SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig2
	secondResult, err := k.ImportRemoteAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if len(secondResult.Updated) != 1 {
		t.Fatalf("expected 1 updated action, got %d", len(secondResult.Updated))
	}
	second := secondResult.Updated[0]
	if second.ID != firstID {
		t.Error("reimport must preserve the same action ID")
	}
	if second.Price != 99 {
		t.Errorf("reimport price: got %d, want 99", second.Price)
	}
}

func TestImportRemoteActionRejectsInvalidSignature(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.RegisterRemoteKernel(ctx, "@bad-sig-peer", base64.RawURLEncoding.EncodeToString(pub), "https://badsig.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := ActionManifest{
		ActionID:    "bad-sig-action",
		OwnerHandle: "@bad-sig-peer",
		Name:        "/greet",
		Kind:        KindHTTP,
		Price:       0,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Signature:   "invalidsignature",
	}
	_, err = k.ImportRemoteAction(ctx, remoteUser.ID, m)
	if err == nil {
		t.Fatal("expected error for invalid manifest signature")
	}
}

func TestGetActionManifestIncludesActionID(t *testing.T) {
	st := newFakeStore()
	su := setupUser(t, st, "@sys", 0)
	st.config["superuser_handle"] = "@sys"
	k := newTestKernel(st)
	k.SetSigningKey(testSigningKey(), su.ID, "@sys")
	ctx := context.Background()

	owner := setupUser(t, st, "@manifest-owner2", 0)
	a := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "/manifest2",
		Kind:         KindHTTP,
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

	m, err := k.GetActionManifest(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetActionManifest: %v", err)
	}
	if m.ActionID != a.ID {
		t.Errorf("manifest.action_id: got %q, want %q", m.ActionID, a.ID)
	}
	if m.Signature == "" {
		t.Error("manifest signature must be set")
	}
}

// ---- Rating record tests ----

func TestRatingRecordCreated(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@rr-owner", 500)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/rr-svc",
		Kind: KindWasm, Active: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, owner.ID, 100)
	reply, err := k.Call(ctx, CallRequest{
		SubjectID: owner.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: owner.ID, ActionName: "/rr-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if _, err := k.RateTransaction(ctx, owner.ID, reply.TxID, 1.0); err != nil {
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
	if rating.RaterUserID != owner.ID {
		t.Errorf("rating.RaterUserID: got %q, want %q", rating.RaterUserID, owner.ID)
	}
}

func TestRatingDuplicateRejected(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@dup-owner", 500)
	a := &Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/dup-svc",
		Kind: KindWasm, Active: true, Price: 0, Source: "wat",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)

	p, root, _ := k.StartProcess(ctx, owner.ID, 100)
	reply, err := k.Call(ctx, CallRequest{
		SubjectID: owner.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: owner.ID, ActionName: "/dup-svc", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if _, err := k.RateTransaction(ctx, owner.ID, reply.TxID, 1.0); err != nil {
		t.Fatalf("first RateTransaction: %v", err)
	}
	if _, err := k.RateTransaction(ctx, owner.ID, reply.TxID, 0.0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for duplicate rating, got %v", err)
	}
}

// ---- Zero-credit process tests ----

func TestZeroCreditProcess(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@zero-owner", 100)

	p, _, err := k.StartProcess(ctx, owner.ID, 0)
	if err != nil {
		t.Fatalf("StartProcess with 0 funds: %v", err)
	}

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
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@sd-owner", 0)
	caller := setupUser(t, st, "@sd-caller", 0)

	a := &Action{
		ID:          uuid.New().String(),
		OwnerUserID: owner.ID,
		Name:        "/sd-svc",
		Kind:        KindHTTP,
		Active:      true,
		Price:       0,
		Source:      "http://example.com",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_ = st.GrantACL(ctx, &ACLEntry{SubjectUserID: caller.ID, ActionID: a.ID, Permission: PermCall, CreatedAt: time.Now().UTC()})

	if err := k.DeleteAction(ctx, owner.ID, a.ID); err != nil {
		t.Fatalf("DeleteAction: %v", err)
	}

	// Action should no longer be readable.
	_, err := k.ReadAction(ctx, a.ID)
	if err == nil {
		t.Error("expected error reading deleted action, got nil")
	}

	// Action should not appear in listings.
	list, _ := st.ListActions(ctx, false, 100, 0)
	for _, listed := range list {
		if listed.ID == a.ID {
			t.Error("deleted action should not appear in ListActions")
		}
	}

	// ACL entries should be purged.
	ok, _ := st.CheckACL(ctx, caller.ID, a.ID, PermCall)
	if ok {
		t.Error("ACL entry should be removed after action delete")
	}
}

func TestSetActiveValidatesWasm(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	owner := setupUser(t, st, "@alice", 0)

	// WASM action with placeholder source.
	a, err := newTestKernel(st).CreateAction(ctx, CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "/wasm-act",
		Kind:         KindWasm,
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
	st := newFakeStore()
	ctx := context.Background()
	owner := setupUser(t, st, "@alice", 0)

	// Create action via store directly with a nil schema to bypass CreateAction validation.
	a := &Action{
		ID: "schema-test", OwnerUserID: owner.ID, Name: "/no-schema",
		Kind: KindHTTP, Source: "http://example.com", Active: false, Price: 0,
	}
	_ = st.CreateAction(ctx, a)

	k := newTestKernel(st)
	if err := k.SetActive(ctx, owner.ID, a.ID, true); err == nil {
		t.Error("expected error activating action with nil input schema")
	}
}

// minOpenAPISpec is a valid minimal OpenAPI 3.x spec with one GET /hello operation.
// servers[0].url is a public hostname so activation SSRF checks pass without AllowLocalSources.
const minOpenAPISpec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

func TestImportOpenAPI(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-import-owner", 0)
	specURL := "https://spec.example.com/api.json"

	result, err := k.ImportOpenAPI(ctx, owner.ID, specURL, []byte(minOpenAPISpec))
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d (updated=%d unchanged=%d rejected=%d)",
			len(result.Created), len(result.Updated), len(result.Unchanged), len(result.Rejected))
	}
	a := result.Created[0]
	if a.Name != "@oapi-import-owner/sayHello" {
		t.Errorf("name: got %q, want %q", a.Name, "@oapi-import-owner/sayHello")
	}
	if a.Active {
		t.Error("imported action must be inactive")
	}
	var src OpenAPISource
	if err := json.Unmarshal([]byte(a.Source), &src); err != nil {
		t.Fatalf("action source is not valid OpenAPISource JSON: %v", err)
	}
	if src.OperationKey != "sayHello" {
		t.Errorf("operation_key: got %q, want %q", src.OperationKey, "sayHello")
	}

	// Re-import with identical spec → Unchanged.
	result2, err := k.ImportOpenAPI(ctx, owner.ID, specURL, []byte(minOpenAPISpec))
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if len(result2.Unchanged) != 1 || len(result2.Created) != 0 {
		t.Errorf("reimport: want 1 unchanged, got created=%d updated=%d unchanged=%d",
			len(result2.Created), len(result2.Updated), len(result2.Unchanged))
	}
}

func TestUnimportOpenAPI(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-unimport-owner", 0)
	specURL := "https://spec.example.com/api.json"

	if _, err := k.ImportOpenAPI(ctx, owner.ID, specURL, []byte(minOpenAPISpec)); err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}

	actions, err := k.UnimportOpenAPI(ctx, owner.ID, specURL, "")
	if err != nil {
		t.Fatalf("UnimportOpenAPI: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 deactivated action, got %d", len(actions))
	}

	// UnimportOpenAPI with name filter deactivates only the matching action.
	if _, err := k.ImportOpenAPI(ctx, owner.ID, specURL, []byte(minOpenAPISpec)); err != nil {
		t.Fatalf("reimport: %v", err)
	}
	actions2, err := k.UnimportOpenAPI(ctx, owner.ID, specURL, "sayHello")
	if err != nil {
		t.Fatalf("UnimportOpenAPI by name: %v", err)
	}
	if len(actions2) != 1 {
		t.Fatalf("expected 1 action for name filter, got %d", len(actions2))
	}
}

func TestOpenAPIActivation(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-activate-owner", 0)
	specURL := "https://spec.example.com/api.json"

	result, err := k.ImportOpenAPI(ctx, owner.ID, specURL, []byte(minOpenAPISpec))
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	a := result.Created[0]

	// Activation must succeed: base_url is api.example.com (public), schemas are valid.
	if err := k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	updated, err := k.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Active {
		t.Error("action should be active after SetActive(true)")
	}
}

func TestOpenAPIActivationRejectsPrivateBaseURL(t *testing.T) {
	st := newFakeStore()
	k := newTestKernel(st) // AllowLocalSources = false
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-private-owner", 0)

	// Craft an OpenAPISource with a private execution base URL.
	src := OpenAPISource{
		Type:          "openapi",
		SpecURL:       "https://spec.example.com/api.json",
		BaseURL:       "http://10.0.0.1",
		Method:        "GET",
		Path:          "/secret",
		OperationKey:  "getSecret",
		OperationHash: "hash",
	}
	srcBytes, _ := json.Marshal(src)
	a := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "@oapi-private-owner/getSecret",
		Kind:         KindHTTP,
		Source:       string(srcBytes),
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	if err := k.SetActive(ctx, owner.ID, a.ID, true); err == nil {
		t.Error("expected error activating action with private base URL, got nil")
	}
}
