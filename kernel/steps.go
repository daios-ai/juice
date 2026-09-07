package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

// Recover settles interrupted calls and re-parks crashed step completions.
// Must be called after SetSigningKey (buildReceipt requires the platform signing key).
// readOpenProcess reads a process and rejects a closed one — §4 precondition 2, the gate every
// spend path shares (a closed process can neither call nor park work).
func (k *Kernel) readOpenProcess(ctx context.Context, processID string) (*Process, error) {
	process, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return nil, ErrNotFound.Wrap("process not found")
	}
	if process.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}
	return process, nil
}

func (k *Kernel) Recover(ctx context.Context) error {
	logger := k.log.With(ctx)

	// 0: Retry pending remote dispatches first. If the remote kernel is reachable and
	// the call committed, we settle cleanly here rather than force-failing via recoverTrace.
	// Any still-pending remote traces are force-failed by settleFailedCall when their
	// orphan parent traces are settled in phase C below.
	k.RetryPendingRemoteDispatches(ctx)

	// A: Re-park empty step-completion traces; collect non-empty ones for phase C.
	// Empty means no subcall started (trace.locked==0, trace.available==step.price, no settled subtx).
	orphanSteps, err := k.store.ListOrphanRunningSteps(ctx)
	if err != nil {
		return err
	}
	stepByTrace := map[string]string{} // completionTraceID → stepID for non-empty completions
	for _, row := range orphanSteps {
		isEmpty := !row.HasSettled && row.TraceLocked == 0 && row.TraceAvailable == row.Price
		if isEmpty {
			if err := k.store.ResetStepAndRepark(ctx, row.StepID); err != nil {
				logger.Error("recover.step_repark_failed", "step_id", row.StepID, "error", err)
			}
		} else {
			stepByTrace[row.CompletionTraceID] = row.StepID
		}
	}

	// C: Settle orphan traces deepest-first. Non-empty step-completion traces are included;
	// stepByTrace routes them to CallerStep semantics so the refund goes to process.available.
	// Children settle before parents, so a child's refund reaches the parent before the parent settles.
	traces, err := k.store.ListOrphanTraces(ctx)
	if err != nil {
		return err
	}
	for _, trace := range traces {
		stepID := stepByTrace[trace.ID]
		if err := k.recoverTrace(ctx, logger, trace, "interrupted", stepID); err != nil {
			logger.Error("recover.trace_failed", "trace_id", trace.ID, "error", err)
		}
	}

	// B: Reset remaining running steps. After C, non-empty completion steps have tx_id set,
	// so only truly pre-BeginStepCall-crash steps (status=running, tx_id=null) remain here.
	if err := k.store.ResetRunningSteps(ctx); err != nil {
		return err
	}
	return nil
}

// newTraceFailureTx builds the failure Transaction skeleton shared by the two crash-recovery
// paths (recoverTrace here and remote-dispatch recovery in federation.go): the role-law fields
// derived from an orphaned trace and its process. Callers fill in path-specific extras
// (ParentTraceID, RemoteActionID, Reason, ReplyJSON).
func newTraceFailureTx(trace *Trace, process *Process, action *Action, gross int64, now time.Time) *Transaction {
	return &Transaction{
		ID:           uuid.New().String(),
		ProcessID:    trace.ProcessID,
		TraceID:      trace.ID,
		OwnerUserID:  process.OwnerUserID,
		CallerUserID: trace.CallerUserID,
		TargetUserID: trace.ActionOwnerID,
		ActionID:     trace.ActionID,
		ActionName:   action.Name,
		Status:       TxFailure,
		Gross:        gross,
		StartedAt:    trace.CreatedAt,
		EndedAt:      now,
	}
}

// recoverTrace settles a single orphan trace as a failure with the given reason.
// stepID is non-empty only for step-completion traces; it causes CommitFailedCall to
// use CallerStep wallet semantics (parent lock already released) and mark the step done.
func (k *Kernel) recoverTrace(ctx context.Context, logger *log.Logger, trace *Trace, reason, stepID string) error {
	process, err := k.store.ReadProcess(ctx, trace.ProcessID)
	if err != nil {
		return err
	}
	action, err := k.store.ReadAction(ctx, trace.ActionID)
	if err != nil || action == nil {
		action = &Action{ID: trace.ActionID, Name: "unknown", OwnerUserID: trace.ActionOwnerID}
	}

	// Completion trace (stepID set): BeginStepCall already released the parent lock, so the
	// refund routes to the process via CallerStep — same routing as a live call.
	callerWalletID, callerWalletKind := callerWalletFor(stepID, process.ID, trace.ParentTraceID)

	ktx := newTraceFailureTx(trace, process, action, trace.Available+trace.Locked, time.Now().UTC())
	ktx.Reason = reason
	ktx.ReplyJSON = json.RawMessage("null")
	// An inbound cross-kernel call is settled by a receipt the caller verifies against the
	// arguments it sent (P5). The crashed process lost them; the record it served kept them.
	if trace.IdempotencyRecordID != nil {
		if rec, err := k.store.ReadIdempotencyRecordByID(ctx, *trace.IdempotencyRecordID); err == nil && rec.ArgsJSON != "" {
			ktx.ArgsJSON = json.RawMessage(rec.ArgsJSON)
		}
	}

	recoverErr := ErrInternal.Wrap(reason)
	req := callRequest{StepID: stepID}
	// Force-failing a trace here (EndProcess, or crash recovery) is the final settlement for any
	// inbound cross-kernel record it serves, so thread the id through: otherwise the peer that
	// requested the work is answered "duplicate in flight" until the record expires and never
	// learns the call resolved. Read from the trace, so this holds for every action kind — not
	// only remote proxies, which are the only traces carrying a dispatch payload.
	if trace.IdempotencyRecordID != nil {
		req.IdempotencyRecordID = *trace.IdempotencyRecordID
	}
	_, settleErr := k.settleFailedCall(ctx, logger, ktx, trace, callerWalletID, callerWalletKind, req, action, 0, recoverErr)
	return settleErr
}

// CreateStep creates a new waiting step. The step records a future Call that a designated caller can resume.
// The creating authority is derived from Trace(traceID).action_owner_id (implicit for in-execution creation).
// For external creation (POST /v1/steps) the service layer must enforce precondition-4 before calling this.
func (k *Kernel) CreateStep(ctx context.Context, traceID, actionID string, partialArgs json.RawMessage, requiredCallerID, requiredCallerRemoteID string) (*Step, error) {
	if traceID == "" {
		return nil, ErrInvalidInput.Wrap("trace_id is required")
	}
	parent, err := k.store.ReadTrace(ctx, traceID)
	if err != nil {
		return nil, ErrNotFound.Wrap("parent trace not found")
	}
	if _, err := k.readOpenProcess(ctx, parent.ProcessID); err != nil {
		return nil, err
	}
	action, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, ErrNotFound.Wrap("action not found")
	}
	// Creation parks money, so it is a funding boundary: a legacy proxy heals here, before its
	// frozen total is parked and its completion dispatches a wrong seller price (§16).
	if action, err = k.ensureBasePrice(ctx, action); err != nil {
		return nil, err
	}
	// A step is a partially applied future Call: the creator names the target, so visibility binds
	// here against the creating trace's action owner (§4 binding rule) — completion re-checks only
	// liveness, never visibility, so the completer needs no sight of a target the creator captured.
	creator, err := k.store.ReadUser(ctx, parent.ActionOwnerID)
	if err != nil {
		return nil, ErrNotFound.Wrap("creator not found")
	}
	if !canCall(creator, action) {
		if !action.Active {
			return nil, ErrInvalidState.Wrap("action is inactive")
		}
		return nil, ErrUnauthorized.Wrap("creator cannot call action")
	}
	// The required caller must resolve to a real account so the step is completable (§10).
	rc, err := k.store.ReadUser(ctx, requiredCallerID)
	if err != nil {
		return nil, ErrNotFound.Wrap("required caller not found")
	}
	// A remote required caller (§13) names a peer's proxy user as the accounting/routing account and
	// the completer's stable user_id on that peer kernel; completion then demands a home-kernel
	// step_auth attestation naming that id, so a remote handle rename never mis-addresses the step.
	var remoteID *string
	if requiredCallerRemoteID != "" {
		if !rc.IsPeer() {
			return nil, ErrInvalidInput.Wrap("a remote required caller must be a peer proxy user")
		}
		remoteID = &requiredCallerRemoteID
	}
	var normErr error
	partialArgs, normErr = normalizeJSONObject(partialArgs, "partial_args")
	if normErr != nil {
		return nil, normErr
	}
	now := time.Now().UTC()
	step := &Step{
		ID:                     uuid.New().String(),
		ParentTraceID:          &traceID,
		RequiredCallerUserID:   requiredCallerID,
		RequiredCallerRemoteID: remoteID,
		ActionID:               actionID,
		PartialArgs:            partialArgs,
		Price:                  action.Price,
		Status:                 StepWaiting,
		CreatedAt:              now,
	}
	// CreateStep is a funding boundary (§16 Price Snapshot Pattern): the price is parked now and may
	// settle long after import_bps changes, so the rate is frozen alongside it. Proxies only — a
	// local action's price carries no import fee.
	if action.Kind == KindRemoteProxy {
		ibps := k.econ.ImportBPS
		step.ImportBPS = &ibps
	}
	// The park moves action.Price from the funding trace's available into locked; lock the trace
	// so it cannot interleave with that trace's settlement taxable-read (§9 composition fence).
	mu := k.traceLock(traceID)
	mu.Lock()
	err = k.store.CreateStep(ctx, step)
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("step.created", "step_id", step.ID, "trace_id", traceID, "status", "success")
	return step, nil
}

// ReadStep returns a step if the caller has read access.
func (k *Kernel) ReadStep(ctx context.Context, callerID, stepID string) (*Step, error) {
	step, err := k.store.ReadStep(ctx, stepID)
	if err != nil {
		return nil, err
	}
	if !k.canReadStep(ctx, callerID, step) {
		return nil, ErrUnauthorized.Wrap("read permission denied")
	}
	return step, nil
}

// ListSteps returns steps visible to the caller.
func (k *Kernel) ListSteps(ctx context.Context, callerID, processID, status string, limit, offset int) ([]*Step, error) {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	return k.store.ListSteps(ctx, callerID, processID, status, k.isUserSuperuser(ctx, u), limit, offset)
}

// ListStepsAwaitingCaller returns the waiting steps callerID is the required caller of, oldest
// first — "what awaits me". Unlike ListSteps it takes no superuser widening: the question is
// scoped to one user by construction, and the federation step protocol (§13) answers it for a
// peer, which must never be able to widen its view of another kernel's steps.
func (k *Kernel) ListStepsAwaitingCaller(ctx context.Context, callerID string, limit int) ([]*Step, error) {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return nil, err
	}
	return k.store.ListStepsAwaitingCaller(ctx, callerID, limit)
}

// CompleteStep resumes a waiting step by merging caller input with partial_args and executing the next call.
// No superuser exception: only required_caller_user_id may complete the step.
//
// Outcome contract — callers must read these, never re-read the step's status, which is racy and
// cannot distinguish "another completer claimed it" from "my own resumed call is still in flight":
//
//	err == nil                            the step was resumed and settled; reply is the result
//	reply != nil (with err)               a transaction committed for THIS completion and then
//	                                      failed; reply carries its TxID/ReceiptID (same contract
//	                                      as Call and RunFederated)
//	errors.Is(err, ErrStepNotClaimed)     this completion never took the step — already claimed
//	                                      or already resolved by someone else; nothing happened
//	errors.Is(err, ErrTimeout)            claimed, dispatched to a peer, and awaiting its receipt;
//	                                      the step stays running for RetryPendingRemoteDispatches
//	otherwise (reply == nil)              rejected before anything settled; the step is waiting again
func (k *Kernel) CompleteStep(ctx context.Context, callerID, stepID string, input json.RawMessage) (*StepReply, error) {
	return k.completeStep(ctx, callerID, stepID, input, "", "")
}

// CompleteStepInTrace resumes a step from inside a running execution — a WASM juice.step_complete or
// the HTTP capability twin. Authority there is the executing trace, not a session: the caller may only
// complete a step its own trace parked, so an endpoint holding a capability cannot reach steps living
// in another process (§9 "no other trace"). Session and federated completion are unaffected: their
// authority is the account itself, which is exactly what required_caller names.
func (k *Kernel) CompleteStepInTrace(ctx context.Context, callerID, traceID, stepID string, input json.RawMessage) (*StepReply, error) {
	if traceID == "" {
		return nil, ErrUnauthorized.Wrap("in-execution completion requires an authorizing trace")
	}
	return k.completeStep(ctx, callerID, stepID, input, "", traceID)
}

// CompleteStepFederated resumes a step on behalf of a peer, threading the inbound cross-kernel
// idempotency record (§13) so the commit that settles the call completes that record atomically —
// including a settlement that only happens later, via the remote-dispatch retry loop. Mirrors
// RunFederated, which does the same for an inbound call.
func (k *Kernel) CompleteStepFederated(ctx context.Context, callerID, stepID string, input json.RawMessage, idempotencyRecordID string) (*StepReply, error) {
	return k.completeStep(ctx, callerID, stepID, input, idempotencyRecordID, "")
}

// StepRemoteRequiredCaller returns a step's required remote-caller id (nil = a local/kernel-level
// required caller), for the federation completion path to enforce the §13 step_auth attestation.
func (k *Kernel) StepRemoteRequiredCaller(ctx context.Context, stepID string) (*string, error) {
	step, err := k.store.ReadStep(ctx, stepID)
	if err != nil {
		return nil, err
	}
	return step.RequiredCallerRemoteID, nil
}

func (k *Kernel) completeStep(ctx context.Context, callerID, stepID string, input json.RawMessage, idempotencyRecordID, authorizingTraceID string) (*StepReply, error) {
	caller, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	step, err := k.store.ReadStep(ctx, stepID)
	if err != nil {
		return nil, err
	}
	if step.Status != StepWaiting {
		return nil, ErrInvalidState.Wrap("step is not waiting").Because(ErrStepNotClaimed)
	}
	// Derive process from the step's parent trace.
	if step.ParentTraceID == nil {
		return nil, ErrInvalidState.Wrap("step has no parent trace")
	}
	parentTrace, err := k.store.ReadTrace(ctx, *step.ParentTraceID)
	if err != nil {
		return nil, ErrNotFound.Wrap("parent trace not found")
	}
	if _, err := k.readOpenProcess(ctx, parentTrace.ProcessID); err != nil {
		return nil, err
	}
	if callerID != step.RequiredCallerUserID {
		return nil, ErrUnauthorized.Wrap("only the step's required caller may complete it")
	}
	// In-execution completion is confined to the trace that authorized it (§10): a capability or WASM
	// host may resume only a step its own trace parked. Without this, holding one trace's capability
	// would reach every step addressed to that action's owner anywhere in the kernel — spending a
	// third party's parked funds. Session and federated callers pass "" and are unaffected.
	if authorizingTraceID != "" && *step.ParentTraceID != authorizingTraceID {
		return nil, ErrUnauthorized.Wrap("step belongs to another trace")
	}

	// Look up action early — needed for derived allowed schema validation.
	action, err := k.store.ReadAction(ctx, step.ActionID)
	if err != nil {
		return nil, ErrNotFound.Wrap("action not found")
	}

	// Parse and validate input against derived allowed schema: action.input_schema \ keys(partial_args).
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	var inputArgs map[string]any
	if err := json.Unmarshal(input, &inputArgs); err != nil {
		return nil, ErrInvalidInput.Wrap("input must be a JSON object")
	}
	allowedSchema := DeriveAllowedSchema(action.InputSchema, step.PartialArgs)
	if len(allowedSchema) > 0 {
		if err := ValidateInput(allowedSchema, inputArgs); err != nil {
			return nil, err
		}
	}
	// Enforce the allowed-input rule §10: allowed input = action.input_schema \ keys(partial_args).
	// This only constrains keys when the schema declares properties; an unconstrained schema imposes
	// no restriction (the merge rule governs collisions there). The derived-schema check above misses
	// the all-bound case: when partial_args binds every declared property, DeriveAllowedSchema reduces
	// properties to {}, which ValidateInput treats as accept-any — letting a completer supply an
	// undeclared key or, worse, overwrite a creator-fixed value. Reject before any state mutation so
	// the step stays waiting and no action failure is recorded.
	if declared, ok := action.InputSchema["properties"].(map[string]any); ok && len(declared) > 0 {
		var partial map[string]any
		if len(step.PartialArgs) > 0 {
			_ = json.Unmarshal(step.PartialArgs, &partial)
		}
		for key := range inputArgs {
			_, isDeclared := declared[key]
			_, isBound := partial[key]
			if !isDeclared || isBound {
				return nil, ErrSchemaViolation.Wrapf("input key %q is not an allowed completion key", key)
			}
		}
	}
	mergedArgs, err := mergeArgs(step.PartialArgs, input)
	if err != nil {
		return nil, ErrInvalidInput.Wrap("could not merge args")
	}
	var args map[string]any
	if err := json.Unmarshal(mergedArgs, &args); err != nil {
		return nil, ErrInvalidInput.Wrap("merged args are not a valid JSON object")
	}

	// Value transfer (§13): a Step whose action bears the transfer effect delivers value on completion.
	// Stage it over the merged args so BeginStepCall locks the value from the completer's own balance
	// atomic with claiming the step, and settlement credits the beneficiary — the completer funds the
	// value, the step's execution price stays creator-parked. A peer completer is refused (value is
	// local to a kernel), and a non-transfer step stages nothing.
	eff, err := k.prepareTransferEffect(ctx, caller.IsPeer(), action, args)
	if err != nil {
		return nil, err
	}
	// BeginStepCall atomically marks the step as running, releases its parked price
	// from parent_trace.locked, and creates a new trace with available=step.price.
	stepTrace := &Trace{
		ID:            uuid.New().String(),
		ProcessID:     parentTrace.ProcessID,
		ParentTraceID: step.ParentTraceID,
		ActionOwnerID: action.OwnerUserID,
		ActionID:      action.ID,
		CallerUserID:  callerID,
		CreatedAt:     time.Now().UTC(),
	}
	if eff != nil {
		// No PremiumBPS snapshot: the completer pays no execution premium either, since the step's price
		// was creator-parked rather than funded across the wire.
		stepTrace.Value, stepTrace.ValueTo = eff.Amount, eff.Dest
	}
	// Same as a root call (kernel.go): the inbound record rides on the trace so any settlement
	// completes it, for every action kind rather than only remote proxies.
	if idempotencyRecordID != "" {
		stepTrace.IdempotencyRecordID = &idempotencyRecordID
	}
	// For remote-proxy actions, generate and persist the idempotency key and dispatch
	// payload atomically with the trace creation. BeginStepCall passes these through
	// insertTraceTx so they land in the DB before any network dispatch. This ensures
	// that on restart, ListPendingRemoteTraces finds the trace and RetryPendingRemoteDispatches
	// can resume with the same idempotency key — matching the §5 recovery guarantee.
	if action.Kind == KindRemoteProxy {
		key := uuid.New().String()
		stepTrace.IdempotencyKey = &key
		// Gross is the price the step PARKED, not the action's current one — those differ once the
		// catalog reprices — and the rate is the one frozen at creation, so a fee change between
		// parking and completion cannot move this step's arithmetic (§16). A pre-041 step has no
		// snapshot and settles from live config, as it does today.
		ibps := k.econ.ImportBPS
		if step.ImportBPS != nil {
			ibps = *step.ImportBPS
		}
		if err := k.prepareDispatch(ctx, stepTrace, action, args, stepID, step.Price, ibps); err != nil {
			return nil, err
		}
	}
	// A lost waiting→running CAS already carries ErrStepNotClaimed from the store, which marks
	// only the two genuine claim races. Deliberately NOT relabelled here: BeginStepCall also
	// reports a park-invariant violation as ErrInvalidState, and a blanket relabel would present
	// that ledger corruption to a gate as an ordinary lost race and silently drop the contribution.
	if err := k.store.BeginStepCall(ctx, stepID, stepTrace); err != nil {
		return nil, err
	}

	reply, callErr := k.call(ctx, callRequest{
		CallerID:            callerID,
		ExistingTraceID:     stepTrace.ID, // BeginStepCall pre-created and funded this completion trace
		Action:              action,
		Args:                args,
		StepID:              stepID, // marks the step done at commit + selects CallerStep wallet
		IdempotencyRecordID: idempotencyRecordID,
	})
	if callErr != nil {
		// Order matters. A non-nil reply means Call committed a transaction, and that is decided
		// BEFORE any error classification: ErrTimeout arrives from two unrelated places — a WASM
		// execution timeout, which settles and charges like any other failure, and a parked remote
		// dispatch, which commits nothing. Testing the error first (as this did) reports a settled,
		// charged completion as though nothing had happened, losing its transaction ids and telling
		// the federation handler to leave an already-completed idempotency record pending.
		if reply != nil {
			// Committed: the step is already done via CommitFailedCall, so no re-park.
			return &StepReply{CallReply: reply, StepID: stepID}, callErr
		}
		// Nothing committed. A remote dispatch may still be in flight: leave the step running so
		// RetryPendingRemoteDispatches can recover it via ListPendingRemoteTraces. Do NOT re-park —
		// that would delete the pending trace and lose the retry state.
		if errors.Is(callErr, ErrTimeout) {
			return nil, callErr
		}
		// Rejected before anything settled: re-park and reset to waiting so the step can be retried.
		if resetErr := k.store.ResetStepAndRepark(ctx, stepID); resetErr != nil {
			k.log.With(ctx).Error("step.reset_failed", "step_id", stepID, "error", resetErr, "call_error", callErr)
		}
		return nil, callErr
	}
	k.log.With(ctx).Info("step.completed", "step_id", stepID, "tx_id", reply.TxID, "status", "success")
	return &StepReply{CallReply: reply, StepID: stepID}, nil
}

// canReadStep returns true if the caller may read the step.
func (k *Kernel) canReadStep(ctx context.Context, callerID string, step *Step) bool {
	if callerID == step.RequiredCallerUserID {
		return true
	}
	if step.ParentTraceID != nil {
		if trace, err := k.store.ReadTrace(ctx, *step.ParentTraceID); err == nil {
			if process, err := k.store.ReadProcess(ctx, trace.ProcessID); err == nil {
				if process.OwnerUserID == callerID {
					return true
				}
			}
		}
	}
	if u, err := k.store.ReadUser(ctx, callerID); err == nil && k.isUserSuperuser(ctx, u) {
		return true
	}
	return false
}

// DeriveAllowedSchema returns the subset of actionSchema that is not already covered by partialArgs.
// Properties and required fields whose keys appear in partialArgs are removed. It is the completer's
// allowed input (§10) and is surfaced on the step read view so a required caller can complete without
// separately reading a private target action.
func DeriveAllowedSchema(actionSchema map[string]any, partialArgs json.RawMessage) map[string]any {
	if len(actionSchema) == 0 {
		return actionSchema
	}
	var partial map[string]any
	if len(partialArgs) > 0 {
		_ = json.Unmarshal(partialArgs, &partial)
	}
	if len(partial) == 0 {
		return actionSchema
	}
	result := make(map[string]any, len(actionSchema))
	for k, v := range actionSchema {
		result[k] = v
	}
	if props, ok := result["properties"].(map[string]any); ok {
		newProps := make(map[string]any, len(props))
		for k, v := range props {
			if _, bound := partial[k]; !bound {
				newProps[k] = v
			}
		}
		result["properties"] = newProps
	}
	if req, ok := result["required"].([]any); ok {
		var newReq []any
		for _, r := range req {
			if s, ok := r.(string); ok {
				if _, bound := partial[s]; !bound {
					newReq = append(newReq, s)
				}
			}
		}
		result["required"] = newReq
	}
	return result
}

// normalizeJSONObject defaults an empty value to "{}" and rejects non-object JSON.
func normalizeJSONObject(raw json.RawMessage, field string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, ErrInvalidInput.Wrapf("%s must be a JSON object", field)
	}
	return raw, nil
}

// mergeArgs performs a shallow merge of base and override JSON objects.
// Keys in override overwrite keys in base.
func mergeArgs(base, override json.RawMessage) (json.RawMessage, error) {
	if len(base) == 0 {
		base = json.RawMessage("{}")
	}
	if len(override) == 0 {
		override = json.RawMessage("{}")
	}
	var b, o map[string]any
	if err := json.Unmarshal(base, &b); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(override, &o); err != nil {
		return nil, err
	}
	for k, v := range o {
		b[k] = v
	}
	return json.Marshal(b)
}
