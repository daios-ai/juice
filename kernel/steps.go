package kernel

import (
	"context"
	"encoding/json"
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

	callerWalletKind := CallerTrace
	callerWalletID := ""
	if stepID != "" {
		// Completion trace: BeginStepCall already released the parent lock; refund to process.
		callerWalletKind = CallerStep
	} else if trace.ParentTraceID == nil {
		callerWalletKind = CallerProcess
		callerWalletID = process.ID
	} else {
		callerWalletID = *trace.ParentTraceID
	}

	now := time.Now().UTC()
	ktx := &Transaction{
		ID:           uuid.New().String(),
		ProcessID:    trace.ProcessID,
		TraceID:      trace.ID,
		OwnerUserID:  process.OwnerUserID,
		CallerUserID: trace.CallerUserID,
		TargetUserID: trace.ActionOwnerID,
		ActionID:     trace.ActionID,
		ActionName:   action.Name,
		Status:       TxFailure,
		Gross:        trace.Available + trace.Locked,
		Reason:       reason,
		StartedAt:    trace.CreatedAt,
		EndedAt:      now,
		ReplyJSON:    json.RawMessage("null"),
	}

	recoverErr := ErrInternal.Wrap(reason)
	req := CallRequest{ProcessID: trace.ProcessID, StepID: stepID}
	return k.settleFailedCall(ctx, logger, ktx, trace.ID, callerWalletID, callerWalletKind, req, action, 0, recoverErr)
}

// CreateStep creates a new waiting step. The step records a future Call that a designated caller can resume.
func (k *Kernel) CreateStep(ctx context.Context, callerID, processID string, parentTraceID *string, nextActionID string, partialArgs, inputSchema json.RawMessage, requiredCallerID string) (*Step, error) {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return nil, err
	}
	process, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return nil, ErrNotFound.Wrap("process not found")
	}
	if process.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}
	// §10: parent_trace_id is always required; it is the funding source for the parked price.
	if parentTraceID == nil {
		return nil, ErrInvalidInput.Wrap("parent_trace_id is required")
	}
	parent, err := k.store.ReadTrace(ctx, *parentTraceID)
	if err != nil {
		return nil, ErrNotFound.Wrap("parent trace not found")
	}
	// §4 precondition: parent trace must belong to the same process.
	if parent.ProcessID != processID {
		return nil, ErrUnauthorized.Wrap("parent trace belongs to a different process")
	}
	// Process-use authority: C = P, or trace-scoped (parent trace's action_owner_id = C).
	if process.OwnerUserID != callerID && parent.ActionOwnerID != callerID {
		return nil, ErrUnauthorized.Wrap("caller is not authorized to use this process")
	}
	action, err := k.store.ReadAction(ctx, nextActionID)
	if err != nil {
		return nil, ErrNotFound.Wrap("next action not found")
	}
	if !canCall(process.OwnerUserID, action) {
		if !action.Active {
			return nil, ErrInvalidState.Wrap("next action is inactive")
		}
		return nil, ErrUnauthorized.Wrap("process owner cannot call next action")
	}
	if _, err := k.store.ReadUser(ctx, requiredCallerID); err != nil {
		return nil, ErrNotFound.Wrap("required_caller_user_id not found")
	}
	var normErr error
	partialArgs, normErr = normalizeJSONObject(partialArgs, "partial_args")
	if normErr != nil {
		return nil, normErr
	}
	inputSchema, normErr = normalizeJSONObject(inputSchema, "input_schema")
	if normErr != nil {
		return nil, normErr
	}
	var schemaMap map[string]any
	if err := json.Unmarshal(inputSchema, &schemaMap); err == nil {
		if err := ValidateSchema(schemaMap); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	step := &Step{
		ID:                   uuid.New().String(),
		ProcessID:            processID,
		ParentTraceID:        parentTraceID,
		RequiredCallerUserID: requiredCallerID,
		NextActionID:         nextActionID,
		PartialArgs:          partialArgs,
		InputSchema:          inputSchema,
		Price:                action.Price,
		Status:               StepWaiting,
		CreatedAt:            now,
	}
	if err := k.store.CreateStep(ctx, step); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("step.created", "step_id", step.ID, "process_id", processID, "status", "success")
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
	step, err := k.store.ReadStep(ctx, stepID)
	if err != nil {
		return nil, err
	}
	if step.Status != StepWaiting {
		return nil, ErrInvalidState.Wrap("step is not waiting")
	}
	process, err := k.store.ReadProcess(ctx, step.ProcessID)
	if err != nil {
		return nil, ErrNotFound.Wrap("process not found")
	}
	if process.Status != ProcessOpen {
		return nil, ErrInvalidState.Wrap("process is closed")
	}
	if callerID != step.RequiredCallerUserID {
		return nil, ErrUnauthorized.Wrap("only required_caller_user_id may complete this step")
	}
	// Parse and validate input.
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	var inputArgs map[string]any
	if err := json.Unmarshal(input, &inputArgs); err != nil {
		return nil, ErrInvalidInput.Wrap("input must be a JSON object")
	}
	if len(step.InputSchema) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(step.InputSchema, &schema); err == nil && len(schema) > 0 {
			if err := ValidateInput(schema, inputArgs); err != nil {
				return nil, err
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

	// Look up next action to get owner+name for Call dispatch.
	action, err := k.store.ReadAction(ctx, step.NextActionID)
	if err != nil {
		return nil, ErrNotFound.Wrap("next action not found")
	}
	owner, err := k.store.ReadUser(ctx, action.OwnerUserID)
	if err != nil {
		return nil, ErrNotFound.Wrap("next action owner not found")
	}

	// BeginStepCall atomically marks the step as running, releases its parked price
	// from parent_trace.locked, and creates a new trace with available=step.price.
	stepTrace := &Trace{
		ID:            uuid.New().String(),
		ProcessID:     step.ProcessID,
		ParentTraceID: step.ParentTraceID,
		ActionOwnerID: action.OwnerUserID,
		ActionID:      action.ID,
		CallerUserID:  callerID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := k.store.BeginStepCall(ctx, stepID, stepTrace); err != nil {
		return nil, err
	}

	reply, callErr := k.Call(ctx, CallRequest{
		CallerID:      callerID,
		ProcessID:     step.ProcessID,
		ParentTraceID: stepTrace.ID,
		TargetUserID:  owner.ID,
		ActionName:    action.Name,
		Args:          args,
		StepID:        stepID,
	})
	if callErr != nil {
		// If Call failed before creating a transaction, re-park the price and reset to waiting
		// so the step can be retried. BeginStepCall already released the parent trace lock and
		// created the completion trace; ResetStepAndRepark undoes that accounting.
		// If CommitFailedCall already ran (step is done), this is a no-op.
		_ = k.store.ResetStepAndRepark(ctx, stepID)
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
	process, err := k.store.ReadProcess(ctx, step.ProcessID)
	if err == nil && process.OwnerUserID == callerID {
		return true
	}
	if u, err := k.store.ReadUser(ctx, callerID); err == nil && k.isUserSuperuser(ctx, u) {
		return true
	}
	return false
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
