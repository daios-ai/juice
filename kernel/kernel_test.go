package kernel

import (
	"context"
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
	return New(st, nil, nil, cfg, log.Default())
}

func newTestKernelWithScripts(st Store, exec ScriptExecutor) *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.FeeBPS = 2000
	return New(st, exec, nil, cfg, log.Default())
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
}

func (f *fakeScriptExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (f *fakeScriptExec) Execute(_ context.Context, _ []byte, input []byte, _ HostFunctions) ([]byte, error) {
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
