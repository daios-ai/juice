package native

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/daios-ai/juice/kernel"
)

// Step gates: @sys/step/race and @sys/step/join.
//
// A Step is a one-shot funded continuation whose resolver is a named principal (§10). That
// covers "wait for one thing" directly; these two natives cover the two ways a workflow waits
// for several. Both are ordinary actions parked into as the *target* of contributor steps, and
// both resume an onward step the workflow author created — so the continuation's price is parked
// once, on the onward step, not once per contributor.
//
//	race: the first contributor to complete resumes the onward step; the rest report fired=false.
//	      Stateless — the waiting→running CAS in the store IS the test-and-set (§5 BeginStepCall).
//	join: each contributor increments a counter; the one that reaches `need` resumes it.
//	      Needs the one small piece of state a barrier inherently is, held in a native-owned table.
//
// Confinement: a gate may only resume an onward step created by the trace that invoked it (see
// gateTarget). Without that, any caller could fire any step whose id they learned, spending funds
// another party reserved. A consequence worth knowing: a top-level `juice run @sys/step/race` can
// never fire anything, because a root trace has no parent — gates are reachable only from inside
// the workflow that created the onward step.

// GateStore is the state @sys/step/join needs: a counter per onward step. Defined here and
// implemented by the store so the kernel never learns about it — a native's state is the
// native's business (§9: natives interact with the platform only through injected dependencies).
type GateStore interface {
	IncrementStepGate(ctx context.Context, stepID string, need int) (have, storedNeed int, err error)
	DeleteStepGate(ctx context.Context, stepID string) error
}

func RegisterRaceHandler(k *kernel.Kernel) {
	k.RegisterNativeHandler("step/race", func(ctx context.Context, args map[string]any, targetID, _, _, _, traceID string) (map[string]any, error) {
		return executeRace(ctx, args, targetID, traceID, k)
	})
}

func RegisterJoinHandler(k *kernel.Kernel, gates GateStore) {
	k.RegisterNativeHandler("step/join", func(ctx context.Context, args map[string]any, targetID, _, _, _, traceID string) (map[string]any, error) {
		return executeJoin(ctx, args, targetID, traceID, k, gates)
	})
}

// gateTarget resolves the onward step named in args and confines it to the trace that invoked
// this gate. sysID is @sys, the action owner: gates resume the onward step as themselves, so the
// onward step must also have been created with required_caller = @sys.
//
// The confinement is deliberately *creator*-scoped, not process-scoped. Subcalls share a process,
// so a process-wide check would let any participant — including the process owner — fire a
// continuation that someone else's contractor parked, with input of their choosing, spending the
// funds that contractor reserved. That is a confused deputy of exactly the kind caller-scoped
// CanCall exists to prevent (§4). Requiring the onward step to have been parked from the same
// trace that invoked the gate binds both to one workflow: a step completion's trace has the gate
// step's creating trace as its parent, and a subcall's has the calling action's trace, so in both
// shapes `invoker` below is "whoever created me". Same trace implies same process, so this
// subsumes the process check rather than adding to it.
func gateTarget(ctx context.Context, args map[string]any, sysID, traceID string, k *kernel.Kernel) (*kernel.Step, error) {
	stepID, _ := args["step_id"].(string)
	if stepID == "" {
		return nil, kernel.ErrInvalidInput.Wrap("step_id is required")
	}
	step, err := k.ReadStep(ctx, sysID, stepID)
	if err != nil {
		return nil, err
	}
	self, err := k.ReadTrace(ctx, traceID)
	if err != nil {
		return nil, kernel.ErrInvalidState.Wrap("gate trace not found")
	}
	if self.ParentTraceID == nil {
		return nil, kernel.ErrUnauthorized.Wrap("a gate cannot be invoked as a root call")
	}
	if step.ParentTraceID == nil || *step.ParentTraceID != *self.ParentTraceID {
		return nil, kernel.ErrUnauthorized.Wrap("step was not created by this gate's caller")
	}
	// A gate resumes the onward step as @sys, so @sys must be its required caller. Checking here
	// names the actual mistake; without it the workflow author sees a bare "unauthorized" raised
	// one layer down, from inside the completion, and attributed to the contributor.
	if step.RequiredCallerUserID != sysID {
		return nil, kernel.ErrUnauthorized.Wrap("a gate's onward step must have @sys as its required caller")
	}
	return step, nil
}

// fire resumes the onward step as @sys, reporting whether THIS contributor was the one that
// resumed it. It reads CompleteStep's outcome contract directly; re-reading the step's status
// would be racy and could not tell a lost race from this gate's own dispatch still being in
// flight — the two need opposite handling.
//
//	resumed and settled            → fired, status success
//	resumed, then the onward call
//	  failed (reply non-nil)       → still fired, status failure + the message: the continuation
//	                                 ran, so this contributor did its job, but reporting only
//	                                 fired=true would read as success and the workflow would
//	                                 proceed as though the continuation had worked
//	another contributor won        → fired=false, not an error: that is the normal outcome for
//	                                 every contributor but one
//	anything else                  → propagate, including an in-flight ErrTimeout, so a dispatch
//	                                 awaiting a peer's receipt is never mistaken for a lost race
func fire(ctx context.Context, k *kernel.Kernel, sysID, stepID string, input json.RawMessage, extra map[string]any) (map[string]any, error) {
	out := map[string]any{"fired": false}
	for key, v := range extra {
		out[key] = v
	}
	reply, err := k.CompleteStep(ctx, sysID, stepID, input)
	switch {
	case err != nil && errors.Is(err, kernel.ErrStepNotClaimed):
		return out, nil
	case err != nil && reply == nil:
		return nil, err
	}
	out["fired"] = true
	out["tx_id"] = reply.TxID
	out["trace_id"] = reply.TraceID
	out["status"] = string(kernel.TxSuccess)
	if err != nil {
		out["status"] = string(kernel.TxFailure)
		out["error"] = err.Error()
	}
	return out, nil
}

func executeRace(ctx context.Context, args map[string]any, sysID, traceID string, k *kernel.Kernel) (map[string]any, error) {
	step, err := gateTarget(ctx, args, sysID, traceID, k)
	if err != nil {
		return nil, err
	}
	input := json.RawMessage("{}")
	if raw, ok := args["input"]; ok {
		b, err := json.Marshal(raw)
		if err != nil {
			return nil, kernel.ErrInvalidInput.Wrap("input must be a JSON object")
		}
		input = b
	}
	return fire(ctx, k, sysID, step.ID, input, nil)
}

func executeJoin(ctx context.Context, args map[string]any, sysID, traceID string, k *kernel.Kernel, gates GateStore) (map[string]any, error) {
	step, err := gateTarget(ctx, args, sysID, traceID, k)
	if err != nil {
		return nil, err
	}
	need, ok := intArg(args["need"])
	if !ok || need < 1 {
		return nil, kernel.ErrInvalidInput.Wrap("need must be a positive integer")
	}

	have, storedNeed, err := gates.IncrementStepGate(ctx, step.ID, need)
	if err != nil {
		return nil, err
	}
	counts := map[string]any{"have": have, "need": storedNeed}
	if have < storedNeed {
		out := map[string]any{"fired": false}
		for key, v := range counts {
			out[key] = v
		}
		return out, nil
	}
	// Threshold reached (>= not ==, so a crash between increment and fire is healed by the next
	// contribution). The onward step is resumed with {} — its arguments were bound in partial_args
	// at creation.
	//
	// On a genuine fire failure the row is deliberately KEPT: it still holds have >= need, so any
	// later contribution re-attempts the fire. Deleting it would discard every accumulated
	// contribution and guarantee the barrier could never complete. If no contributor remains, the
	// onward step simply stays waiting and process closure refunds it, like any failed continuation.
	out, err := fire(ctx, k, sysID, step.ID, json.RawMessage("{}"), counts)
	if err != nil {
		return nil, err
	}
	// The row is spent once the onward step can never need it again: this contribution fired it, or
	// the step is terminally resolved (done/cancelled) by someone else. It is KEPT while the step
	// is still waiting — a failed fire that a later contribution can retry — and while it is
	// running, which covers this barrier's own in-flight dispatch that may yet leave the step
	// re-completable. Reading the status is safe here in a way it is not for reporting: it decides
	// only whether to drop a spent row, never what this contributor is told.
	//
	// Deleting solely on `fired` would leak: a late contribution to an already-resolved barrier
	// recreates the row via IncrementStepGate and nothing would ever remove it, since the cascade
	// only fires if the step row itself is deleted, which normal operation never does.
	if fired, _ := out["fired"].(bool); fired || gateStepIsResolved(ctx, k, sysID, step.ID) {
		_ = gates.DeleteStepGate(ctx, step.ID)
	}
	return out, nil
}

// gateStepIsResolved reports whether the onward step has reached a terminal state, so its barrier
// row is dead weight. An unreadable step is treated as unresolved: keeping a row costs one row,
// dropping one wrongly costs every accumulated contribution.
func gateStepIsResolved(ctx context.Context, k *kernel.Kernel, sysID, stepID string) bool {
	s, err := k.ReadStep(ctx, sysID, stepID)
	if err != nil {
		return false
	}
	return s.Status == kernel.StepDone || s.Status == kernel.StepCancelled
}

// intArg accepts the numeric shapes JSON decoding produces (float64) plus the integer forms a
// Go caller or a schema-coerced value may supply.
func intArg(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n == float64(int(n))
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
