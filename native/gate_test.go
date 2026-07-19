package native

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/store"
	"github.com/google/uuid"
)

// gateFixture builds a kernel with @sys owning a sink action, a funded process, and one onward
// step parked for @sys — the shape both gates operate on. It returns the store (which is also
// the GateStore), @sys's id, the process id, and the onward step.
type gateFixture struct {
	k     *kernel.Kernel
	db    *store.DB
	sysID string
	step  *kernel.Step
	// gateTrace is a child of the trace that created the onward step — the shape a real gate
	// executes in, since a step completion's trace has the creating trace as its parent.
	gateTrace string
	// siblingTrace is a child of a DIFFERENT trace in the SAME process: what foreign code funded
	// by the same process owner looks like. It must not be able to fire the onward step.
	siblingTrace string
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(t.TempDir() + "/gate.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	k := newTestKernelOn(t, db)
	sys := seedOwner(t, db, "@sys")
	// The onward action must be one that actually executes, so a fired gate settles a real
	// transaction: a native sink, exactly as production parks @sys/sink behind a gate.
	sink := seedAction(t, db, sys.ID, "sink", "universal sink")
	sink.Kind = kernel.KindNative
	if err := db.UpdateAction(ctx, sink); err != nil {
		t.Fatalf("make sink native: %v", err)
	}
	RegisterSinkHandler(k)

	// @sys funds the process, so the parked onward step is @sys's own money — exactly the
	// production shape, where the workflow action's owner creates the step from its trace.
	if err := db.CreateLedgerEntry(ctx, &kernel.LedgerEntry{
		ID: uuid.New().String(), OperatorUserID: sys.ID, ToUserID: sys.ID,
		Amount: 1000, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("fund @sys: %v", err)
	}

	p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: sys.ID, Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
	tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ActionOwnerID: sys.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, tr, sys.ID, 0); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}

	step, err := k.CreateStep(ctx, tr.ID, sink.ID, json.RawMessage(`{}`), sys.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	child := func(parent string) string {
		t.Helper()
		c := &kernel.Trace{
			ID: uuid.New().String(), ProcessID: p.ID, ParentTraceID: &parent,
			ActionOwnerID: sys.ID, ActionID: sink.ID, CreatedAt: time.Now().UTC(),
		}
		if err := db.BeginSubcall(ctx, parent, c, 0); err != nil {
			t.Fatalf("BeginSubcall: %v", err)
		}
		return c.ID
	}
	// A contractor's own trace, then a gate invoked from inside it: same process as the onward
	// step, different creator.
	contractor := child(tr.ID)
	return &gateFixture{
		k: k, db: db, sysID: sys.ID, step: step,
		gateTrace:    child(tr.ID),
		siblingTrace: child(contractor),
	}
}

func TestRace_FirstContributorFiresOnce(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()
	args := map[string]any{"step_id": f.step.ID}

	first, err := executeRace(ctx, args, f.sysID, f.gateTrace, f.k)
	if err != nil {
		t.Fatalf("first race: %v", err)
	}
	if first["fired"] != true {
		t.Fatalf("expected first contributor to fire, got %v", first)
	}
	if first["tx_id"] == "" || first["tx_id"] == nil {
		t.Errorf("expected a tx_id on the winning contribution, got %v", first["tx_id"])
	}

	// The loser sees fired=false, not an error: losing a race is the normal outcome.
	second, err := executeRace(ctx, args, f.sysID, f.gateTrace, f.k)
	if err != nil {
		t.Fatalf("second race: %v", err)
	}
	if second["fired"] != false {
		t.Errorf("expected second contributor not to fire, got %v", second)
	}

	step, err := f.k.ReadStep(ctx, f.sysID, f.step.ID)
	if err != nil {
		t.Fatalf("ReadStep: %v", err)
	}
	if step.Status != kernel.StepDone {
		t.Errorf("expected onward step done, got %s", step.Status)
	}
}

// Concurrent contributors must resolve to exactly one winner: the store's waiting→running CAS
// is the test-and-set, so race needs no lock of its own.
func TestRace_ConcurrentContributorsFireExactlyOnce(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	fired := make([]bool, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			out, err := executeRace(ctx, map[string]any{"step_id": f.step.ID}, f.sysID, f.gateTrace, f.k)
			if err != nil {
				errs[i] = err
				return
			}
			fired[i], _ = out["fired"].(bool)
		}(i)
	}
	wg.Wait()

	winners := 0
	for i := range fired {
		if errs[i] != nil {
			t.Fatalf("contributor %d: %v", i, errs[i])
		}
		if fired[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("expected exactly 1 winner, got %d", winners)
	}
}

func TestRace_RejectsStepFromAnotherProcess(t *testing.T) {
	f := newGateFixture(t)
	orphan := &kernel.Trace{ID: uuid.New().String()}
	_, err := executeRace(context.Background(), map[string]any{"step_id": f.step.ID},
		f.sysID, orphan.ID, f.k)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState for an unknown gate trace, got %v", err)
	}
}

// Regression, confused deputy: confinement is creator-scoped, not process-scoped. Subcalls share
// a process, so a process-wide check would let anyone executing in the process — including the
// process owner — fire a continuation someone else's contractor parked, with input of their
// choosing, spending the funds that contractor reserved.
func TestGates_RejectStepCreatedByAnotherTraceInSameProcess(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()
	args := map[string]any{"step_id": f.step.ID, "input": map[string]any{"forged": true}}

	_, err := executeRace(ctx, args, f.sysID, f.siblingTrace, f.k)
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("race: expected ErrUnauthorized for a foreign creator, got %v", err)
	}
	_, err = executeJoin(ctx, map[string]any{"step_id": f.step.ID, "need": float64(1)},
		f.sysID, f.siblingTrace, f.k, f.db)
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("join: expected ErrUnauthorized for a foreign creator, got %v", err)
	}

	// The onward step is untouched: still waiting, still funded.
	step, err := f.k.ReadStep(ctx, f.sysID, f.step.ID)
	if err != nil {
		t.Fatalf("ReadStep: %v", err)
	}
	if step.Status != kernel.StepWaiting {
		t.Errorf("expected the onward step to remain waiting, got %s", step.Status)
	}
}

func TestRace_MissingStepID(t *testing.T) {
	f := newGateFixture(t)
	_, err := executeRace(context.Background(), map[string]any{}, f.sysID, f.gateTrace, f.k)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestJoin_FiresAtThreshold(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()
	args := map[string]any{"step_id": f.step.ID, "need": float64(3)}

	for i := 1; i <= 2; i++ {
		out, err := executeJoin(ctx, args, f.sysID, f.gateTrace, f.k, f.db)
		if err != nil {
			t.Fatalf("contribution %d: %v", i, err)
		}
		if out["fired"] != false {
			t.Fatalf("contribution %d fired early: %v", i, out)
		}
		if out["have"] != i {
			t.Errorf("contribution %d: have=%v, want %d", i, out["have"], i)
		}
	}

	out, err := executeJoin(ctx, args, f.sysID, f.gateTrace, f.k, f.db)
	if err != nil {
		t.Fatalf("final contribution: %v", err)
	}
	if out["fired"] != true {
		t.Fatalf("expected the third contribution to fire, got %v", out)
	}

	step, _ := f.k.ReadStep(ctx, f.sysID, f.step.ID)
	if step.Status != kernel.StepDone {
		t.Errorf("expected onward step done, got %s", step.Status)
	}
	// The gate row is dropped once fired, so a barrier leaves no residue.
	have, _, err := f.db.IncrementStepGate(ctx, f.step.ID, 3)
	if err != nil {
		t.Fatalf("post-fire increment: %v", err)
	}
	if have != 1 {
		t.Errorf("expected gate row deleted after firing (have=1 on re-open), got have=%d", have)
	}
}

func TestJoin_NeedMismatchRejected(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()
	if _, err := executeJoin(ctx, map[string]any{"step_id": f.step.ID, "need": float64(3)},
		f.sysID, f.gateTrace, f.k, f.db); err != nil {
		t.Fatalf("first contribution: %v", err)
	}
	_, err := executeJoin(ctx, map[string]any{"step_id": f.step.ID, "need": float64(5)},
		f.sysID, f.gateTrace, f.k, f.db)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput on a changed threshold, got %v", err)
	}
}

func TestJoin_InvalidNeed(t *testing.T) {
	f := newGateFixture(t)
	for _, need := range []any{nil, float64(0), "three"} {
		args := map[string]any{"step_id": f.step.ID}
		if need != nil {
			args["need"] = need
		}
		_, err := executeJoin(context.Background(), args, f.sysID, f.gateTrace, f.k, f.db)
		if !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("need=%v: expected ErrInvalidInput, got %v", need, err)
		}
	}
}

// A barrier whose onward step is already gone (cancelled, or won by something else) reports
// fired=false and clears its row rather than erroring.
func TestJoin_AlreadyResolvedStepClearsGate(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()
	if _, err := executeRace(ctx, map[string]any{"step_id": f.step.ID}, f.sysID, f.gateTrace, f.k); err != nil {
		t.Fatalf("pre-fire via race: %v", err)
	}
	out, err := executeJoin(ctx, map[string]any{"step_id": f.step.ID, "need": float64(1)},
		f.sysID, f.gateTrace, f.k, f.db)
	if err != nil {
		t.Fatalf("join on resolved step: %v", err)
	}
	if out["fired"] != false {
		t.Errorf("expected fired=false on an already-resolved step, got %v", out)
	}
}

func TestIntArg(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int
		ok   bool
	}{
		{float64(3), 3, true},
		{3, 3, true},
		{int64(3), 3, true},
		{float64(3.5), 0, false},
		{"3", 0, false},
		{nil, 0, false},
	} {
		got, ok := intArg(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("intArg(%#v) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
