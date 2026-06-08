package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

// setupWasmAction creates a WASM action with the given input schema (as JSON string).
func setupWasmAction(t *testing.T, st kernel.Store, ownerID, name, inputSchemaJSON string, price int64) *kernel.Action {
	t.Helper()
	schema := make(map[string]any)
	if inputSchemaJSON != "" {
		if err := json.Unmarshal([]byte(inputSchemaJSON), &schema); err != nil {
			t.Fatalf("setupWasmAction: unmarshal schema: %v", err)
		}
	}
	a := &kernel.Action{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Name:        name,
		Kind:        kernel.KindWasm,
		Active:      true,
		Public:      true,
		Price:       price,
		Source:      "fake-wasm",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// setupStep creates a step in the store directly (bypassing kernel auth).
func setupStep(t *testing.T, st kernel.Store, processID, nextActionID, requiredCallerID string, partialArgs, inputSchema json.RawMessage) *kernel.Step {
	t.Helper()
	if len(partialArgs) == 0 {
		partialArgs = json.RawMessage("{}")
	}
	if len(inputSchema) == 0 {
		inputSchema = json.RawMessage("{}")
	}
	step := &kernel.Step{
		ID:                   uuid.New().String(),
		ProcessID:            processID,
		RequiredCallerUserID: requiredCallerID,
		NextActionID:         nextActionID,
		PartialArgs:          partialArgs,
		InputSchema:          inputSchema,
		Status:               kernel.StepWaiting,
		CreatedAt:            time.Now().UTC(),
	}
	if err := st.CreateStep(context.Background(), step); err != nil {
		t.Fatal(err)
	}
	return step
}

func TestStepCreateReturnsWaitingStep(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@sc-owner", 500)
	action := setupAction(t, st, owner.ID, "sc-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)
	caller := setupUser(t, st, "@sc-caller", 0)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if step.Status != kernel.StepWaiting {
		t.Errorf("expected status=waiting, got %s", step.Status)
	}
	if step.ID == "" {
		t.Error("expected non-empty step ID")
	}
	if step.ProcessID != p.ID {
		t.Errorf("process_id mismatch")
	}
	if step.RequiredCallerUserID != caller.ID {
		t.Errorf("required_caller_user_id mismatch")
	}
}

func TestStepCompleteMergesArgs(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"merged":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@merge-owner", 500)
	caller := setupUser(t, st, "@merge-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "merge-action", "", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	partialArgs := json.RawMessage(`{"from_partial":"A","shared":"partial-val"}`)
	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, partialArgs, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// input overrides "shared" key
	input := json.RawMessage(`{"from_input":"B","shared":"input-val"}`)
	reply, err := k.CompleteStep(ctx, caller.ID, step.ID, input)
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
	if reply == nil || reply.TxID == "" {
		t.Error("expected TxID in reply")
	}
}

func TestStepCompleteInputValidatedAgainstInputSchema(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@schema-owner", 500)
	caller := setupUser(t, st, "@schema-caller", 0)
	action := setupAction(t, st, owner.ID, "schema-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	inputSchema := json.RawMessage(`{"type":"object","properties":{"required_field":{"type":"string"}},"required":["required_field"]}`)
	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, inputSchema, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// input missing required_field → should be rejected
	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Errorf("expected ErrSchemaViolation for schema violation, got %v", err)
	}
}

func TestStepCompleteWrongCallerReturnsErrUnauthorized(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@wrong-owner", 500)
	rightCaller := setupUser(t, st, "@wrong-right-caller", 0)
	wrongCaller := setupUser(t, st, "@wrong-wrong-caller", 0)
	action := setupAction(t, st, owner.ID, "wrong-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, rightCaller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	_, err = k.CompleteStep(ctx, wrongCaller.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for wrong caller, got %v", err)
	}
}

func TestStepCompleteRunningOrDoneReturnsErrInvalidState(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@state-owner", 500)
	caller := setupUser(t, st, "@state-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "state-action", "", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// Complete it once
	if _, err := k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("first CompleteStep: %v", err)
	}

	// Try again — should be done now
	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState for done step, got %v", err)
	}
}

func TestStepCompleteSetsDoneOnExecutionFailure(t *testing.T) {
	st := newTestStore(t)
	// Action runs but always fails at execution time — CommitFailedCall fires, creating a failure tx.
	k := newTestKernelWithScripts(st, &fakeScriptExec{err: kernel.ErrExecutionFailed.Wrap("simulated failure")})
	ctx := context.Background()

	owner := setupUser(t, st, "@reset-owner", 500)
	caller := setupUser(t, st, "@reset-caller", 0)
	action := setupAction(t, st, owner.ID, "reset-action", 0)
	action.Kind = kernel.KindWasm
	_ = st.UpdateAction(ctx, action)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected CompleteStep to return error on action failure")
	}

	// Requirements: Call completion (success or failure) atomically records status=done + tx_id.
	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepDone {
		t.Errorf("expected step.status=done after execution failure, got %s", got.Status)
	}
	if got.TxID == nil {
		t.Error("expected tx_id to be set after execution failure")
	}
}

func TestStepCompleteResetsToWaitingOnPreTransactionReject(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@prereject-owner", 0) // zero funds
	caller := setupUser(t, st, "@prereject-caller", 0)
	action := setupAction(t, st, owner.ID, "prereject-action", 100) // costs 100, owner has 0
	action.Kind = kernel.KindWasm
	_ = st.UpdateAction(ctx, action)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 0) // no funds in process either

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected CompleteStep to fail when process has insufficient funds")
	}

	// Call rejected before creating a transaction: step must be reset to waiting for retry.
	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepWaiting {
		t.Errorf("expected step.status=waiting after pre-transaction reject, got %s", got.Status)
	}
	if got.TxID != nil {
		t.Error("expected tx_id to be nil after pre-transaction reject")
	}
}

func TestStepTxIDRecordedAtomicallyWithStatusDone(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@txid-owner", 500)
	caller := setupUser(t, st, "@txid-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "txid-action", "", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	reply, err := k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}

	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepDone {
		t.Errorf("expected status=done, got %s", got.Status)
	}
	if got.TxID == nil {
		t.Fatal("expected tx_id to be set after CompleteStep")
	}
	if *got.TxID != reply.TxID {
		t.Errorf("step.tx_id=%q, reply.TxID=%q — mismatch", *got.TxID, reply.TxID)
	}
}

func TestStepCompletionTraceParentTraceID(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@trace-owner", 500)
	caller := setupUser(t, st, "@trace-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "trace-action", "", 0)
	p, root, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, &root.ID, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// ParentTraceID stored on step
	if step.ParentTraceID == nil || *step.ParentTraceID != root.ID {
		t.Errorf("expected ParentTraceID=%q, got %v", root.ID, step.ParentTraceID)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
}

func TestStepCompletionTraceProcessID(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@procid-owner", 500)
	caller := setupUser(t, st, "@procid-caller", 0)
	action := setupAction(t, st, owner.ID, "procid-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if step.ProcessID != p.ID {
		t.Errorf("step.ProcessID=%q, want %q", step.ProcessID, p.ID)
	}
}

func TestCanListStepProcessOwnerSeesOwnStep(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@list-owner", 500)
	caller := setupUser(t, st, "@list-caller", 0)
	action := setupAction(t, st, owner.ID, "list-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	steps, err := k.ListSteps(ctx, owner.ID, "", "")
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	found := false
	for _, s := range steps {
		if s.ID == step.ID {
			found = true
		}
	}
	if !found {
		t.Error("process owner should see step in ListSteps")
	}
}

func TestCanListStepRequiredCallerSeesStep(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@caller-list-owner", 500)
	caller := setupUser(t, st, "@caller-list-caller", 0)
	action := setupAction(t, st, owner.ID, "caller-list-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	steps, err := k.ListSteps(ctx, caller.ID, "", "")
	if err != nil {
		t.Fatalf("ListSteps by caller: %v", err)
	}
	found := false
	for _, s := range steps {
		if s.ID == step.ID {
			found = true
		}
	}
	if !found {
		t.Error("required_caller_user_id should see step in ListSteps")
	}
}

func TestCanListStepUnrelatedUserDenied(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@unrel-owner", 500)
	caller := setupUser(t, st, "@unrel-caller", 0)
	unrelated := setupUser(t, st, "@unrelated", 0)
	action := setupAction(t, st, owner.ID, "unrel-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	steps, err := k.ListSteps(ctx, unrelated.ID, "", "")
	if err != nil {
		t.Fatalf("ListSteps for unrelated: %v", err)
	}
	for _, s := range steps {
		if s.ID == step.ID {
			t.Error("unrelated user should not see step in ListSteps")
		}
	}
}

func TestCanReadStepSameRulesAsCanListStep(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@read-step-owner", 500)
	caller := setupUser(t, st, "@read-step-caller", 0)
	unrelated := setupUser(t, st, "@read-step-unrelated", 0)
	action := setupAction(t, st, owner.ID, "read-step-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// owner can read
	if _, err := k.ReadStep(ctx, owner.ID, step.ID); err != nil {
		t.Errorf("owner ReadStep: %v", err)
	}
	// caller can read
	if _, err := k.ReadStep(ctx, caller.ID, step.ID); err != nil {
		t.Errorf("caller ReadStep: %v", err)
	}
	// unrelated cannot read
	_, err = k.ReadStep(ctx, unrelated.ID, step.ID)
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for unrelated user, got %v", err)
	}
}

func TestBootstrapResetsRunningStepsToWaiting(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@reset-bs-owner", 100)
	caller := setupUser(t, st, "@reset-bs-caller", 0)
	action := setupAction(t, st, owner.ID, "reset-bs-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, _ := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)

	// Manually claim (simulate crash mid-execution without tx)
	if err := st.ClaimStep(ctx, step.ID); err != nil {
		t.Fatalf("ClaimStep: %v", err)
	}
	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepRunning {
		t.Fatalf("expected running after ClaimStep, got %s", got.Status)
	}

	// ResetRunningSteps restores to waiting
	if err := k.ResetRunningSteps(ctx); err != nil {
		t.Fatalf("ResetRunningSteps: %v", err)
	}
	got, _ = st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepWaiting {
		t.Errorf("expected waiting after ResetRunningSteps, got %s", got.Status)
	}
}

func TestWaitingStepOnClosedProcessIsNonCompletable(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@closed-owner", 500)
	caller := setupUser(t, st, "@closed-caller", 0)
	action := setupAction(t, st, owner.ID, "closed-action", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	step, err := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// Close the process
	if err := k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatalf("EndProcess: %v", err)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState for closed process, got %v", err)
	}
}

func TestStepWithoutTxIDIsNeverDone(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	owner := setupUser(t, st, "@notxid-owner", 100)
	caller := setupUser(t, st, "@notxid-caller", 0)
	action := setupAction(t, st, owner.ID, "notxid-action", 0)

	step := &kernel.Step{
		ID:                   uuid.New().String(),
		ProcessID:            uuid.New().String(), // non-existent process is OK for this invariant test
		RequiredCallerUserID: caller.ID,
		NextActionID:         action.ID,
		PartialArgs:          json.RawMessage("{}"),
		InputSchema:          json.RawMessage("{}"),
		Status:               kernel.StepWaiting,
		CreatedAt:            time.Now().UTC(),
	}
	// Create a real process for FK constraint
	k := newTestKernel(st)
	p, _, _ := k.StartProcess(context.Background(), owner.ID, owner.ID, 0)
	step.ProcessID = p.ID

	if err := st.CreateStep(ctx, step); err != nil {
		t.Fatal(err)
	}

	got, _ := st.ReadStep(ctx, step.ID)
	if got.TxID != nil {
		t.Error("new step should have nil tx_id")
	}
	if got.Status == kernel.StepDone {
		t.Error("step without tx_id must not be done")
	}
}

func TestStepCompletionTraceCrossProcessParentRef(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@crossproc-owner", 500)
	caller := setupUser(t, st, "@crossproc-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "crossproc-action", "", 0)
	p, root, _ := k.StartProcess(ctx, owner.ID, owner.ID, 200)

	// Use the root trace as a "foreign" parent trace ID (it exists in the DB).
	// This simulates a step whose parent trace comes from a different call in the same system.
	foreignTraceID := root.ID

	step, err := k.CreateStep(ctx, owner.ID, p.ID, &foreignTraceID, action.ID, nil, nil, caller.ID)
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if step.ParentTraceID == nil || *step.ParentTraceID != foreignTraceID {
		t.Errorf("parent_trace_id not stored correctly: %v", step.ParentTraceID)
	}

	reply, err := k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
	if reply.TxID == "" {
		t.Error("expected TxID in reply")
	}
}

func TestMergeArgsInputKeysOverwritePartialArgs(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "@merge2-owner", 500)
	caller := setupUser(t, st, "@merge2-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "merge2-action", "", 0)
	p, _, _ := k.StartProcess(ctx, owner.ID, owner.ID, 100)

	partialArgs := json.RawMessage(`{"key":"from-partial","other":"base"}`)
	step, _ := k.CreateStep(ctx, owner.ID, p.ID, nil, action.ID, partialArgs, nil, caller.ID)

	// input's "key" should win over partial's "key"
	input := json.RawMessage(`{"key":"from-input"}`)
	_, err := k.CompleteStep(ctx, caller.ID, step.ID, input)
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}

	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepDone {
		t.Errorf("step should be done, got %s", got.Status)
	}
}

// TestCreateStepTraceAuthority verifies that an action owner who is not the process owner
// can create a step when they own the executing action in the parent trace (F3 fix).
func TestCreateStepTraceAuthority(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	processOwner := setupUser(t, st, "@trace-proc-owner", 500)
	actionOwner := setupUser(t, st, "@trace-act-owner", 0)
	nextUser := setupUser(t, st, "@trace-next-user", 0)

	// Action owned by actionOwner, public so processOwner can call it.
	action := setupWasmAction(t, st, actionOwner.ID, "trace-action", "", 0)

	// Next action the step will invoke.
	nextAction := setupAction(t, st, processOwner.ID, "trace-next-action", 0)

	p, _, _ := k.StartProcess(ctx, processOwner.ID, processOwner.ID, 100)

	// Call the action to create a trace where action_owner_id = actionOwner.ID
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID:     processOwner.ID,
		ProcessID:    p.ID,
		TargetUserID: actionOwner.ID,
		ActionName:   action.Name,
		Args:         map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call to create parent trace: %v", err)
	}
	parentTraceID := reply.TraceID

	// actionOwner (not process owner) can create a step using the parent trace for authority.
	step, err := k.CreateStep(ctx, actionOwner.ID, p.ID, &parentTraceID, nextAction.ID, nil, nil, nextUser.ID)
	if err != nil {
		t.Fatalf("CreateStep with trace authority: %v", err)
	}
	if step == nil || step.Status != kernel.StepWaiting {
		t.Fatal("expected a waiting step")
	}
}

// TestCreateStepTraceAuthorityWrongProcess verifies that trace-scoped authority does not
// grant cross-process step creation (parent trace must be in the same process).
func TestCreateStepTraceAuthorityWrongProcess(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	procOwner := setupUser(t, st, "@xproc-step-owner", 500)
	actionOwner := setupUser(t, st, "@xproc-step-actowner", 0)
	nextUser := setupUser(t, st, "@xproc-step-next", 0)

	action := setupWasmAction(t, st, actionOwner.ID, "xproc-step-action", "", 0)
	nextAction := setupAction(t, st, procOwner.ID, "xproc-step-next-action", 0)

	p1, _, _ := k.StartProcess(ctx, procOwner.ID, procOwner.ID, 100)
	p2, _, _ := k.StartProcess(ctx, procOwner.ID, procOwner.ID, 100)

	// Create a trace in p1 owned by actionOwner.
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID:     procOwner.ID,
		ProcessID:    p1.ID,
		TargetUserID: actionOwner.ID,
		ActionName:   action.Name,
		Args:         map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	p1TraceID := reply.TraceID

	// actionOwner tries to create a step in p2 using the p1 trace — must fail.
	_, err = k.CreateStep(ctx, actionOwner.ID, p2.ID, &p1TraceID, nextAction.ID, nil, nil, nextUser.ID)
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for cross-process trace authority, got %v", err)
	}
}
