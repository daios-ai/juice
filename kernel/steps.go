package kernel

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ResetRunningSteps resets running steps (status=running, tx_id=null) back to waiting. Called at startup.
func (k *Kernel) ResetRunningSteps(ctx context.Context) error {
	return k.store.ResetRunningSteps(ctx)
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
	u, err := k.store.ReadUser(ctx, callerID)
	if err != nil {
		return nil, ErrUnauthenticated.Wrap("user not found")
	}
	if process.OwnerUserID != callerID && !k.isUserSuperuser(ctx, u) {
		return nil, ErrUnauthorized.Wrap("caller is not the process owner")
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
	if len(partialArgs) == 0 {
		partialArgs = json.RawMessage("{}")
	}
	if len(inputSchema) == 0 {
		inputSchema = json.RawMessage("{}")
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
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return nil, err
	}
	u, err := k.store.ReadUser(ctx, callerID)
	if err != nil {
		return nil, ErrUnauthenticated.Wrap("user not found")
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

	parentTraceID := ""
	if step.ParentTraceID != nil {
		parentTraceID = *step.ParentTraceID
	}

	if err := k.store.ClaimStep(ctx, stepID); err != nil {
		return nil, err
	}

	reply, callErr := k.Call(ctx, CallRequest{
		CallerID:       callerID,
		ProcessID:      step.ProcessID,
		ParentTraceID:  parentTraceID,
		TargetUserID:   owner.ID,
		ActionName:     action.Name,
		Args:           args,
		StepCompletion: true,
	})
	if callErr != nil {
		// Reset to waiting so the step can be retried.
		_ = k.store.ResetStep(ctx, stepID)
		return nil, callErr
	}

	if err := k.store.CompleteStep(ctx, stepID, reply.TxID); err != nil {
		k.log.With(ctx).Error("step.complete_record_failed", "step_id", stepID, "tx_id", reply.TxID, "error", err)
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
