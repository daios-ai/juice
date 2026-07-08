package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

// ResetRunningSteps resets running steps (status=running, tx_id=null) back to waiting. Called at startup.
func (k *Kernel) ResetRunningSteps(ctx context.Context) error {
	return k.store.ResetRunningSteps(ctx)
}

// Recover settles interrupted calls and re-parks crashed step completions.
// Must be called after SetSigningKey (buildReceipt requires the platform signing key).
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

	recoverErr := ErrInternal.Wrap(reason)
	req := CallRequest{StepID: stepID}
	_, settleErr := k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, 0, recoverErr)
	return settleErr
}

// CreateStep creates a new waiting step. The step records a future Call that a designated caller can resume.
// The creating authority is derived from Trace(traceID).action_owner_id (implicit for in-execution creation).
// For external creation (POST /v1/steps) the service layer must enforce precondition-4 before calling this.
func (k *Kernel) CreateStep(ctx context.Context, traceID, actionID string, partialArgs json.RawMessage, requiredCallerID string) (*Step, error) {
	if traceID == "" {
		return nil, ErrInvalidInput.Wrap("trace_id is required")
	}
	parent, err := k.store.ReadTrace(ctx, traceID)
	if err != nil {
		return nil, ErrNotFound.Wrap("parent trace not found")
	}
	process, err := k.store.ReadProcess(ctx, parent.ProcessID)
	if err != nil {
		return nil, ErrNotFound.Wrap("process not found")
	}
	if process.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}
	action, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, ErrNotFound.Wrap("action not found")
	}
	if !canCall(process.OwnerUserID, action) {
		if !action.Active {
			return nil, ErrInvalidState.Wrap("action is inactive")
		}
		return nil, ErrUnauthorized.Wrap("process owner cannot call action")
	}
	if _, err := k.store.ReadUser(ctx, requiredCallerID); err != nil {
		return nil, ErrNotFound.Wrap("required_caller_user_id not found")
	}
	var normErr error
	partialArgs, normErr = normalizeJSONObject(partialArgs, "partial_args")
	if normErr != nil {
		return nil, normErr
	}
	now := time.Now().UTC()
	step := &Step{
		ID:                   uuid.New().String(),
		ParentTraceID:        &traceID,
		RequiredCallerUserID: requiredCallerID,
		ActionID:             actionID,
		PartialArgs:          partialArgs,
		Price:                action.Price,
		Status:               StepWaiting,
		CreatedAt:            now,
	}
	if err := k.store.CreateStep(ctx, step); err != nil {
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
func (k *Kernel) ListSteps(ctx context.Context, callerID, processID, status string) ([]*Step, error) {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	return k.store.ListSteps(ctx, callerID, processID, status, k.isUserSuperuser(ctx, u))
}

// CompleteStep resumes a waiting step by merging caller input with partial_args and executing the next call.
// No superuser exception: only required_caller_user_id may complete the step.
func (k *Kernel) CompleteStep(ctx context.Context, callerID, stepID string, input json.RawMessage) (*StepReply, error) {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return nil, err
	}
	step, err := k.store.ReadStep(ctx, stepID)
	if err != nil {
		return nil, err
	}
	if step.Status != StepWaiting {
		return nil, ErrInvalidState.Wrap("step is not waiting")
	}
	// Derive process from the step's parent trace.
	if step.ParentTraceID == nil {
		return nil, ErrInvalidState.Wrap("step has no parent trace")
	}
	parentTrace, err := k.store.ReadTrace(ctx, *step.ParentTraceID)
	if err != nil {
		return nil, ErrNotFound.Wrap("parent trace not found")
	}
	process, err := k.store.ReadProcess(ctx, parentTrace.ProcessID)
	if err != nil {
		return nil, ErrNotFound.Wrap("process not found")
	}
	if process.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}
	if callerID != step.RequiredCallerUserID {
		return nil, ErrUnauthorized.Wrap("only required_caller_user_id may complete this step")
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
	// For remote-proxy actions, generate and persist the idempotency key and dispatch
	// payload atomically with the trace creation. BeginStepCall passes these through
	// insertTraceTx so they land in the DB before any network dispatch. This ensures
	// that on restart, ListPendingRemoteTraces finds the trace and RetryPendingRemoteDispatches
	// can resume with the same idempotency key — matching the §5 recovery guarantee.
	if action.Kind == KindRemoteProxy {
		key := uuid.New().String()
		stepTrace.IdempotencyKey = &key
		stepTrace.DispatchJSON = marshalDispatch(args, stepID, k.remoteManifestPrice(action.Price))
	}
	if err := k.store.BeginStepCall(ctx, stepID, stepTrace); err != nil {
		return nil, err
	}

	reply, callErr := k.Call(ctx, CallRequest{
		CallerID:        callerID,
		ExistingTraceID: stepTrace.ID, // BeginStepCall pre-created and funded this completion trace
		Action:          action,
		Args:            args,
		StepID:          stepID, // marks the step done at commit + selects CallerStep wallet
	})
	if callErr != nil {
		// Remote-proxy timeout: the completion trace has idempotency_key set and the
		// remote dispatch may already be in flight. Leave the step running so that
		// RetryPendingRemoteDispatches can recover it via ListPendingRemoteTraces.
		// Do NOT re-park — that would delete the pending trace and lose retry state.
		if errors.Is(callErr, ErrTimeout) {
			return nil, callErr
		}
		// Non-timeout: Call failed before creating a transaction. Re-park and reset to waiting so
		// the step can be retried. A no-op if CommitFailedCall already ran (step done); a non-nil
		// error is a genuine store failure worth logging (callErr is still returned).
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
