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
	// parentTrace is the trace that created the onward step — what gateTrace's parent must be.
	parentTrace string
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
		k: k, db: db, sysID: sys.ID, step: step, parentTrace: tr.ID,
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
	if first["status"] != "success" {
		t.Errorf("expected status=success on a clean fire, got %v", first["status"])
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

// A contributor whose onward action FAILS still fired: it resumed the continuation, and the
// failure is recorded in that continuation's own transaction. Reporting fired=false here would be
// indistinguishable from losing the race, so the workflow could not tell "nobody ran it" from
// "it ran and failed" — the misclassification the outcome contract exists to remove.
func TestGates_FiredIsTrueWhenTheOnwardActionFails(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()

	// Point the onward step at a native whose handler always fails, so the completion commits a
	// FAILURE transaction rather than being rejected before one exists.
	f.k.RegisterNativeHandler("boom", func(context.Context, map[string]any, string, string, string, string, string) (map[string]any, error) {
		return nil, kernel.ErrExecutionFailed.Wrap("upstream exploded")
	})
	sys, _ := f.k.ReadUserByHandle(ctx, "@sys")
	boom := seedAction(t, f.db, sys.ID, "boom", "always fails")
	boom.Kind = kernel.KindNative
	if err := f.db.UpdateAction(ctx, boom); err != nil {
		t.Fatal(err)
	}
	step, err := f.k.CreateStep(ctx, f.parentTrace, boom.ID, json.RawMessage(`{}`), sys.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	out, err := executeRace(ctx, map[string]any{"step_id": step.ID}, f.sysID, f.gateTrace, f.k)
	if err != nil {
		t.Fatalf("race over a failing onward action should not error: %v", err)
	}
	if out["fired"] != true {
		t.Fatalf("expected fired=true (the continuation ran and failed), got %v", out)
	}
	if out["tx_id"] == nil || out["tx_id"] == "" {
		t.Errorf("expected the onward failure transaction's id, got %v", out["tx_id"])
	}
	// Scenario (review finding 6): fired=true alone reads as success. The workflow must be able to
	// see that the continuation FAILED without going and inspecting the transaction itself.
	if out["status"] != "failure" {
		t.Errorf("expected status=failure alongside fired=true, got %v", out)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Error("expected the onward failure message to be reported")
	}
}

// Every way a gate refuses to act, in one table. The confinement cases are the confused-deputy
// regression: subcalls share a process, so a process-wide rule would let anyone executing there
// resume a continuation another provider parked, with input of their choosing.
func TestGates_RejectBadTargets(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		args  func(f *gateFixture) map[string]any
		trace func(f *gateFixture) string // nil = the fixture's own gate trace
		want  error
	}{
		{"missing step_id",
			func(*gateFixture) map[string]any { return map[string]any{} },
			nil, kernel.ErrInvalidInput},
		{"unknown gate trace",
			func(f *gateFixture) map[string]any { return map[string]any{"step_id": f.step.ID} },
			func(*gateFixture) string { return uuid.New().String() }, kernel.ErrInvalidState},
		{"step created by another trace in the same process",
			func(f *gateFixture) map[string]any {
				return map[string]any{"step_id": f.step.ID, "input": map[string]any{"forged": true}}
			},
			func(f *gateFixture) string { return f.siblingTrace }, kernel.ErrUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			trace := f.gateTrace
			if tc.trace != nil {
				trace = tc.trace(f)
			}
			args := tc.args(f)
			if _, err := executeRace(ctx, args, f.sysID, trace, f.k); !errors.Is(err, tc.want) {
				t.Errorf("race: got %v, want %v", err, tc.want)
			}
			args["need"] = float64(1)
			if _, err := executeJoin(ctx, args, f.sysID, trace, f.k, f.db); !errors.Is(err, tc.want) {
				t.Errorf("join: got %v, want %v", err, tc.want)
			}
			// A refused gate never touches the onward step.
			if step, err := f.k.ReadStep(ctx, f.sysID, f.step.ID); err == nil && step.Status != kernel.StepWaiting {
				t.Errorf("expected the onward step to remain waiting, got %s", step.Status)
			}
		})
	}
}

// `need` must be a positive integer, and the first contribution fixes it.
func TestJoin_RejectsBadNeed(t *testing.T) {
	ctx := context.Background()
	for _, need := range []any{nil, float64(0), "three"} {
		f := newGateFixture(t)
		args := map[string]any{"step_id": f.step.ID}
		if need != nil {
			args["need"] = need
		}
		if _, err := executeJoin(ctx, args, f.sysID, f.gateTrace, f.k, f.db); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("need=%v: got %v, want ErrInvalidInput", need, err)
		}
	}
	f := newGateFixture(t)
	if _, err := executeJoin(ctx, map[string]any{"step_id": f.step.ID, "need": float64(3)},
		f.sysID, f.gateTrace, f.k, f.db); err != nil {
		t.Fatalf("first contribution: %v", err)
	}
	if _, err := executeJoin(ctx, map[string]any{"step_id": f.step.ID, "need": float64(5)},
		f.sysID, f.gateTrace, f.k, f.db); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("changed threshold: got %v, want ErrInvalidInput", err)
	}
}

// The gate row's whole lifecycle, by the onward step's state. The rule is one condition: the row
// is spent only when the barrier can never need its count again. Keeping it wrongly costs one row;
// dropping it wrongly costs every accumulated contribution, so every non-terminal state keeps it.
func TestJoin_GateRowLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// setup returns the onward step the barrier targets, already in the state under test.
		setup    func(t *testing.T, f *gateFixture) string
		wantKept bool
		reason   string
	}{
		{
			name: "fire fails before anything settles",
			setup: func(t *testing.T, f *gateFixture) string {
				// Deactivating after parking makes the completion fail CanCall — rejected before
				// any transaction, so the step resets to waiting and fire returns a real error.
				sys, _ := f.k.ReadUserByHandle(ctx, "@sys")
				target := seedAction(t, f.db, sys.ID, "later-disabled", "disabled after parking")
				step, err := f.k.CreateStep(ctx, f.parentTrace, target.ID, json.RawMessage(`{}`), sys.ID)
				if err != nil {
					t.Fatal(err)
				}
				target.Active = false
				if err := f.db.UpdateAction(ctx, target); err != nil {
					t.Fatal(err)
				}
				return step.ID
			},
			wantKept: true,
			reason:   "a later contribution must be able to re-attempt the fire",
		},
		{
			name: "onward step is in flight",
			setup: func(t *testing.T, f *gateFixture) string {
				sys, _ := f.k.ReadUserByHandle(ctx, "@sys")
				target := seedAction(t, f.db, sys.ID, "inflight-target", "claimed elsewhere")
				step, err := f.k.CreateStep(ctx, f.parentTrace, target.ID, json.RawMessage(`{}`), sys.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.db.ExecForTest(ctx, `UPDATE steps SET status='running' WHERE id=?`, step.ID); err != nil {
					t.Fatal(err)
				}
				return step.ID
			},
			wantKept: true,
			reason:   "this barrier's own dispatch may yet leave the step completable",
		},
		{
			name: "onward step already resolved",
			setup: func(t *testing.T, f *gateFixture) string {
				// Fire it via race, so the join below arrives late to a done step.
				if _, err := executeRace(ctx, map[string]any{"step_id": f.step.ID}, f.sysID, f.gateTrace, f.k); err != nil {
					t.Fatal(err)
				}
				return f.step.ID
			},
			wantKept: false,
			reason:   "nothing will read the row again; keeping it leaks one row per late contribution",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			stepID := tc.setup(t, f)
			args := map[string]any{"step_id": stepID, "need": float64(1)}

			out, err := executeJoin(ctx, args, f.sysID, f.gateTrace, f.k, f.db)
			if tc.wantKept && tc.name == "fire fails before anything settles" {
				if err == nil {
					t.Fatal("expected the fire to fail")
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			} else if fired, _ := out["fired"].(bool); fired {
				t.Fatalf("expected fired=false for a non-completable step, got %v", out)
			}

			// Re-opening the gate reveals whether the row survived: have=2 means it was kept.
			have, _, err := f.db.IncrementStepGate(ctx, stepID, 1)
			if err != nil {
				t.Fatalf("IncrementStepGate: %v", err)
			}
			if kept := have > 1; kept != tc.wantKept {
				t.Errorf("row kept = %v, want %v — %s", kept, tc.wantKept, tc.reason)
			}
		})
	}
}
