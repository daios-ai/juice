// SPDX-License-Identifier: AGPL-3.0-only

package kernel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/store"
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
		Visibility:  kernel.VisibilityPublic,
		Price:       price,
		Source:      "fake-wasm",
		InputSchema: schema,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// setupStep creates a step in the store directly (bypassing kernel auth).
func setupStep(t *testing.T, st kernel.Store, parentTraceID, actionID, requiredCallerID string, partialArgs json.RawMessage) *kernel.Step {
	t.Helper()
	if len(partialArgs) == 0 {
		partialArgs = json.RawMessage("{}")
	}
	step := &kernel.Step{
		ID:                   uuid.New().String(),
		ParentTraceID:        &parentTraceID,
		RequiredCallerUserID: requiredCallerID,
		ActionID:             actionID,
		PartialArgs:          partialArgs,
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

	owner := setupUser(t, st, "sc-owner", 500)
	action := setupLocalAction(t, st, owner.ID, "sc-action", 0)
	caller := setupUser(t, st, "sc-caller", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if step.Status != kernel.StepWaiting {
		t.Errorf("expected status=waiting, got %s", step.Status)
	}
	if step.ID == "" {
		t.Error("expected non-empty step ID")
	}
	if step.RequiredCallerUserID != caller.ID {
		t.Errorf("required_caller_user_id mismatch")
	}
}

func TestStepCompleteMergesArgs(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"merged":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "merge-owner", 500)
	caller := setupUser(t, st, "merge-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "merge-action", "", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	partialArgs := json.RawMessage(`{"from_partial":"A","shared":"partial-val"}`)
	step, err := k.CreateStep(ctx, trID, action.ID, partialArgs, kernel.RequiredCaller{UserID: caller.ID})
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

	owner := setupUser(t, st, "schema-owner", 500)
	caller := setupUser(t, st, "schema-caller", 0)
	// Action carries the input schema; CompleteStep validates against it.
	action := setupWasmAction(t, st, owner.ID, "schema-action",
		`{"type":"object","properties":{"required_field":{"type":"string"}},"required":["required_field"]}`, 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
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

	owner := setupUser(t, st, "wrong-owner", 500)
	rightCaller := setupUser(t, st, "wrong-right-caller", 0)
	wrongCaller := setupUser(t, st, "wrong-wrong-caller", 0)
	action := setupLocalAction(t, st, owner.ID, "wrong-action", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: rightCaller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	_, err = k.CompleteStep(ctx, wrongCaller.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for wrong caller, got %v", err)
	}
}

// TestCompleteStepInTraceIsTraceConfined: in-execution completion (the HTTP capability and the WASM
// host) is confined to the trace that authorized it (§9 "no other trace"). Being the required caller
// is not enough — otherwise a capability minted for one process would fire steps parked in another
// user's process, spending funds that user committed. The same caller completing through its own
// trace still succeeds, and the session path (CompleteStep) is untouched.
func TestCompleteStepInTraceIsTraceConfined(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	victim := setupUser(t, st, "confine-victim", 500)
	mallory := setupUser(t, st, "confine-mallory", 500)
	action := setupWasmAction(t, st, victim.ID, "confine-action", "", 0)

	// A step in VICTIM's process, addressed to mallory.
	_, victimTrace := setupOrphanTrace(t, st, victim.ID, victim.ID, victim.ID)
	step, err := k.CreateStep(ctx, victimTrace.ID, action.ID, nil, kernel.RequiredCaller{UserID: mallory.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	// A trace of mallory's own, standing in for the one a capability would name.
	_, mallorysTrace := setupOrphanTrace(t, st, mallory.ID, mallory.ID, mallory.ID)

	if _, err := k.CompleteStepInTrace(ctx, mallory.ID, mallorysTrace.ID, step.ID, json.RawMessage(`{}`)); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("a foreign trace must not complete the step, got %v", err)
	}
	if s, _ := st.ReadStep(ctx, step.ID); s.Status != kernel.StepWaiting {
		t.Errorf("a refused completion must leave the step waiting, got %s", s.Status)
	}
	if _, err := k.CompleteStepInTrace(ctx, mallory.ID, "", step.ID, json.RawMessage(`{}`)); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("an empty authorizing trace must be refused, got %v", err)
	}
	// The authorizing trace IS the step's parent: ordinary in-execution completion still works.
	if _, err := k.CompleteStepInTrace(ctx, mallory.ID, victimTrace.ID, step.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("completion from the parking trace must succeed, got %v", err)
	}
}

func TestStepCompleteRunningOrDoneReturnsErrInvalidState(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "state-owner", 500)
	caller := setupUser(t, st, "state-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "state-action", "", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
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

	owner := setupUser(t, st, "reset-owner", 500)
	caller := setupUser(t, st, "reset-caller", 0)
	action := setupLocalAction(t, st, owner.ID, "reset-action", 0)
	action.Kind = kernel.KindWasm
	_ = st.UpdateAction(ctx, action)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
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

func TestStepTxIDRecordedAtomicallyWithStatusDone(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "txid-owner", 500)
	caller := setupUser(t, st, "txid-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "txid-action", "", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
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

	owner := setupUser(t, st, "trace-owner", 500)
	caller := setupUser(t, st, "trace-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "trace-action", "", 0)

	// Create an orphan trace (no tx) so the process stays open.
	_, orphan := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	parentTraceID := orphan.ID

	step, err := k.CreateStep(ctx, parentTraceID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// ParentTraceID stored on step
	if step.ParentTraceID == nil || *step.ParentTraceID != parentTraceID {
		t.Errorf("expected ParentTraceID=%q, got %v", parentTraceID, step.ParentTraceID)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
}

func TestCanListStepProcessOwnerSeesOwnStep(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "list-owner", 500)
	caller := setupUser(t, st, "list-caller", 0)
	action := setupLocalAction(t, st, owner.ID, "list-action", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	steps, err := k.ListSteps(ctx, owner.ID, "", "", 50, 0)
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

	owner := setupUser(t, st, "caller-list-owner", 500)
	caller := setupUser(t, st, "caller-list-caller", 0)
	action := setupLocalAction(t, st, owner.ID, "caller-list-action", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	steps, err := k.ListSteps(ctx, caller.ID, "", "", 50, 0)
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

	owner := setupUser(t, st, "unrel-owner", 500)
	caller := setupUser(t, st, "unrel-caller", 0)
	unrelated := setupUser(t, st, "unrelated", 0)
	action := setupLocalAction(t, st, owner.ID, "unrel-action", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	steps, err := k.ListSteps(ctx, unrelated.ID, "", "", 50, 0)
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

	owner := setupUser(t, st, "read-step-owner", 500)
	caller := setupUser(t, st, "read-step-caller", 0)
	unrelated := setupUser(t, st, "read-step-unrelated", 0)
	action := setupLocalAction(t, st, owner.ID, "read-step-action", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
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

	owner := setupUser(t, st, "reset-bs-owner", 100)
	caller := setupUser(t, st, "reset-bs-caller", 0)
	action := setupLocalAction(t, st, owner.ID, "reset-bs-action", 0)
	p, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, _ := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})

	// Manually claim the step via BeginStepCall to simulate a crash mid-execution (step running, no tx).
	stepTrace := &kernel.Trace{
		ID:        uuid.New().String(),
		ProcessID: p.ID,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.BeginStepCall(ctx, step.ID, stepTrace); err != nil {
		t.Fatalf("BeginStepCall: %v", err)
	}
	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepRunning {
		t.Fatalf("expected running after BeginStepCall, got %s", got.Status)
	}

	// The store transition startup recovery drives in phase B (§5) restores it to waiting.
	// Recovery through Kernel.Recover is covered end-to-end by
	// TestRecoverReparkEmptyStepCompletionTrace, which supplies a settled parent trace;
	// this fixture's parent is deliberately orphaned, so Recover would fail it and cancel
	// the step instead — the very behaviour TestRecoverWithOrphanParentAndPendingChild pins.
	if err := st.ResetRunningSteps(ctx); err != nil {
		t.Fatalf("ResetRunningSteps: %v", err)
	}
	got, _ = st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepWaiting {
		t.Errorf("expected waiting after recovery, got %s", got.Status)
	}
}

func TestWaitingStepOnClosedProcessIsNonCompletable(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "closed-owner", 500)
	caller := setupUser(t, st, "closed-caller", 0)
	action := setupLocalAction(t, st, owner.ID, "closed-action", 0)
	p, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// Close the process — waiting step must become cancelled.
	if err := k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatalf("EndProcess: %v", err)
	}

	updated, err := k.ReadStep(ctx, owner.ID, step.ID)
	if err != nil {
		t.Fatalf("ReadStep after EndProcess: %v", err)
	}
	if updated.Status != kernel.StepCancelled {
		t.Errorf("expected status cancelled, got %s", updated.Status)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState for cancelled step, got %v", err)
	}
}

func TestStepWithoutTxIDIsNeverDone(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	owner := setupUser(t, st, "notxid-owner", 100)
	caller := setupUser(t, st, "notxid-caller", 0)
	action := setupLocalAction(t, st, owner.ID, "notxid-action", 0)

	// Use a real trace (FK constraint) — orphan trace gives us a valid parent.
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID
	step := &kernel.Step{
		ID:                   uuid.New().String(),
		ParentTraceID:        &trID,
		RequiredCallerUserID: caller.ID,
		ActionID:             action.ID,
		PartialArgs:          json.RawMessage("{}"),
		Status:               kernel.StepWaiting,
		CreatedAt:            time.Now().UTC(),
	}

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

	owner := setupUser(t, st, "crossproc-owner", 500)
	caller := setupUser(t, st, "crossproc-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "crossproc-action", "", 0)

	// Use an orphan trace as the "foreign" parent — simulates a step parked during an active call.
	_, orphan := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	foreignTraceID := orphan.ID

	step, err := k.CreateStep(ctx, foreignTraceID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
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

	owner := setupUser(t, st, "merge2-owner", 500)
	caller := setupUser(t, st, "merge2-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "merge2-action", "", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	partialArgs := json.RawMessage(`{"key":"from-partial","other":"base"}`)
	step, _ := k.CreateStep(ctx, trID, action.ID, partialArgs, kernel.RequiredCaller{UserID: caller.ID})

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

// TestStepCompleteRejectsOverrideOfBoundKeyAllBound verifies that when partial_args binds every
// declared property (so the derived allowed schema has empty properties), the completer cannot
// supply a key that overwrites a creator-fixed value. The completion is rejected before any state
// mutation: the step stays waiting and no transaction is recorded (§10, allowed-input rule).
func TestStepCompleteRejectsOverrideOfBoundKeyAllBound(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "allbound-owner", 500)
	caller := setupUser(t, st, "allbound-caller", 0)
	// Schema declares exactly one property; partial_args binds it, so the derived allowed schema is empty.
	action := setupWasmAction(t, st, owner.ID, "allbound-action",
		`{"type":"object","properties":{"x":{"type":"string"}}}`, 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)

	step, err := k.CreateStep(ctx, tr.ID, action.ID, json.RawMessage(`{"x":"creator-fixed"}`), kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{"x":"attacker"}`))
	if !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Fatalf("expected ErrSchemaViolation for overriding a bound key, got %v", err)
	}

	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepWaiting {
		t.Errorf("step should remain waiting after rejected completion, got %s", got.Status)
	}
	if got.TxID != nil {
		t.Errorf("rejected completion must not record a transaction, got tx_id %v", *got.TxID)
	}
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: tr.ProcessID})
	if len(txs) != 0 {
		t.Errorf("expected no transactions for a rejected completion, got %d", len(txs))
	}
}

// TestStepCompleteAllowsDisjointInputAllBound is the negative control for the all-bound guard:
// with every property bound and an empty input, the completion still succeeds. The guard fires
// only on actual key collisions/undeclared keys, never on a no-extra-input completion.
func TestStepCompleteAllowsDisjointInputAllBound(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "allbound2-owner", 500)
	caller := setupUser(t, st, "allbound2-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "allbound2-action",
		`{"type":"object","properties":{"x":{"type":"string"}}}`, 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)

	step, err := k.CreateStep(ctx, tr.ID, action.ID, json.RawMessage(`{"x":"creator-fixed"}`), kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	if _, err := k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CompleteStep with empty input should succeed, got %v", err)
	}

	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepDone {
		t.Errorf("step should be done, got %s", got.Status)
	}
}

// TestStepCompleteGrossEqualsStepPriceAcrossPriceChange verifies the completion transaction records
// the parked step.price snapshot as gross, even when the action's price changed (deactivate → re-enable
// with a new price) while the step waited. BeginStepCall funds the completion trace with step.price, so
// gross must equal that, not the action's current price (§10).
func TestStepCompleteGrossEqualsStepPriceAcrossPriceChange(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "gross-owner", 500)
	caller := setupUser(t, st, "gross-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "gross-action", "", 100)

	// Fund a root trace with the action's price (100) and snapshot step.price = 100 at creation.
	_, tr := beginTestRun(t, st, owner.ID, action)
	step, err := k.CreateStep(ctx, tr.ID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if step.Price != 100 {
		t.Fatalf("expected step.price snapshot 100, got %d", step.Price)
	}

	// Simulate the action being re-enabled with a higher price after the step was created.
	action.Price = 500
	if err := st.UpdateAction(ctx, action); err != nil {
		t.Fatalf("UpdateAction: %v", err)
	}

	reply, err := k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}

	tx, err := st.ReadTransaction(ctx, reply.TxID)
	if err != nil {
		t.Fatalf("ReadTransaction: %v", err)
	}
	if tx.Gross != 100 {
		t.Errorf("gross must equal the parked step.price snapshot 100, got %d (current action price 500)", tx.Gross)
	}
	if tx.Gross != tx.Net+tx.Fee {
		t.Errorf("settlement invariant violated: gross %d != net %d + fee %d", tx.Gross, tx.Net, tx.Fee)
	}
}

// TestCreateStepTraceAuthority verifies that an action owner who is not the process owner
// can create a step when they own the executing action in the parent trace (F3 fix).
func TestCreateStepTraceAuthority(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	processOwner := setupUser(t, st, "trace-proc-owner", 500)
	actionOwner := setupUser(t, st, "trace-act-owner", 0)
	nextUser := setupUser(t, st, "trace-next-user", 0)

	// Next action the step will invoke.
	nextAction := setupLocalAction(t, st, processOwner.ID, "trace-next-action", 0)

	// Create an orphan trace owned by actionOwner (action_owner_id = actionOwner.ID).
	_, orphan := setupOrphanTrace(t, st, processOwner.ID, actionOwner.ID, processOwner.ID)
	parentTraceID := orphan.ID

	// Any caller can create a step; service layer enforces trace authority. Kernel just checks action/process.
	step, err := k.CreateStep(ctx, parentTraceID, nextAction.ID, nil, kernel.RequiredCaller{UserID: nextUser.ID})
	if err != nil {
		t.Fatalf("CreateStep with trace authority: %v", err)
	}
	if step == nil || step.Status != kernel.StepWaiting {
		t.Fatal("expected a waiting step")
	}
}

// TestCreateStepTraceAuthorityWrongProcess verifies that a trace from a closed process
// cannot be used to create a new step (the process is already closed).
func TestCreateStepTraceAuthorityWrongProcess(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	procOwner := setupUser(t, st, "xproc-step-owner", 500)
	actionOwner := setupUser(t, st, "xproc-step-actowner", 0)
	nextUser := setupUser(t, st, "xproc-step-next", 0)

	action := setupWasmAction(t, st, actionOwner.ID, "xproc-step-action", "", 0)
	nextAction := setupLocalAction(t, st, procOwner.ID, "xproc-step-next-action", 0)

	_, tr1 := beginTestRun(t, st, procOwner.ID, action)

	// Call completes; p1 auto-closes after Run finishes.
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        procOwner.ID,
		ExistingTraceID: tr1.ID,
		TargetUserID:    actionOwner.ID,
		ActionName:      action.Name,
		Args:            map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	p1TraceID := reply.TraceID

	// p1 is now closed; CreateStep using p1's trace must fail with ErrInvalidState.
	_, err = k.CreateStep(ctx, p1TraceID, nextAction.ID, nil, kernel.RequiredCaller{UserID: nextUser.ID})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState for closed-process trace, got %v", err)
	}
}

func TestCreateStepRejectsNonObjectPartialArgs(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "pa-owner", 500)
	caller := setupUser(t, st, "pa-caller", 0)
	action := setupLocalAction(t, st, owner.ID, "pa-action", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	_, err := k.CreateStep(ctx, trID, action.ID, json.RawMessage(`"not-an-object"`), kernel.RequiredCaller{UserID: caller.ID})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for non-object partial_args, got %v", err)
	}
}

// TestCreateStepEmptyTraceIDReturnsErrInvalidInput verifies that an empty trace_id is rejected.
func TestCreateStepEmptyTraceIDReturnsErrInvalidInput(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "nil-pt-owner", 500)
	caller := setupUser(t, st, "nil-pt-caller", 0)
	action := setupLocalAction(t, st, owner.ID, "nil-pt-action", 0)

	_, err := k.CreateStep(ctx, "", action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty traceID, got %v", err)
	}
}

// setupPrivateWasmAction creates an active private WASM action owned by ownerID.
func setupPrivateWasmAction(t *testing.T, st kernel.Store, ownerID, name string) *kernel.Action {
	t.Helper()
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: ownerID, Name: name, Kind: kernel.KindWasm,
		Active: true, Visibility: kernel.VisibilityPrivate, Price: 0, Source: "fake-wasm",
		InputSchema: map[string]any{"type": "object"}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// TestCreateStepChecksCreatorNotRequiredCaller: visibility is bound at creation against the
// creating trace's action owner (§4 binding rule, §10), NOT the required caller. A provider may
// park its own private action for a customer who could never call it directly; the customer then
// completes it — visibility is not re-checked at completion, exactly as a closure over a private
// function is invocable by whoever holds it.
func TestCreateStepChecksCreatorNotRequiredCaller(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()

	owner := setupUser(t, st, "creator-priv-owner", 500)
	customer := setupUser(t, st, "creator-priv-customer", 0)
	action := setupPrivateWasmAction(t, st, owner.ID, "private-fulfillment")

	// The creator is the trace's action owner (= owner), who CAN call the private action.
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)

	// canCall(customer, action) is false (private, non-owner) — the OLD rule rejected this.
	step, err := k.CreateStep(ctx, tr.ID, action.ID, nil, kernel.RequiredCaller{UserID: customer.ID})
	if err != nil {
		t.Fatalf("CreateStep parking a private action for a non-owner: %v", err)
	}

	// The customer completes it despite being unable to see the target: completion re-checks
	// only liveness, never visibility.
	reply, err := k.CompleteStep(ctx, customer.ID, step.ID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CompleteStep by required caller who cannot see the target: %v", err)
	}
	if reply == nil || reply.TxID == "" {
		t.Fatal("expected a settled completion")
	}
}

// TestCreateStepRejectedWhenCreatorCannotCall: the binding check is real — a creator that cannot
// see the target is rejected at creation, even if the required caller could.
func TestCreateStepRejectedWhenCreatorCannotCall(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	actionOwner := setupUser(t, st, "cc-action-owner", 0)
	creator := setupUser(t, st, "cc-creator", 500)
	action := setupPrivateWasmAction(t, st, actionOwner.ID, "cc-private")

	// The trace's action owner is `creator`, who is NOT the action owner — cannot see the private action.
	// The required caller is the action owner, who could call it: irrelevant under the binding rule.
	_, tr := setupOrphanTrace(t, st, creator.ID, creator.ID, creator.ID)

	_, err := k.CreateStep(ctx, tr.ID, action.ID, nil, kernel.RequiredCaller{UserID: actionOwner.ID})
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized when the creator cannot call the action, got %v", err)
	}
}

// TestStepCompletionIgnoresVisibilityNarrowing: narrowing an action to private after a step is
// parked no longer bricks the step. The OLD rule reset it to waiting forever (funds parked, no
// refund); the binding rule completes it, since the target was captured at creation.
func TestStepCompletionIgnoresVisibilityNarrowing(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()

	owner := setupUser(t, st, "narrow-owner", 500)
	customer := setupUser(t, st, "narrow-customer", 0)
	action := setupWasmAction(t, st, owner.ID, "narrow-action", "", 0) // public

	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	step, err := k.CreateStep(ctx, tr.ID, action.ID, nil, kernel.RequiredCaller{UserID: customer.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// Narrow to private after parking: the customer can no longer see it.
	action.Visibility = kernel.VisibilityPrivate
	if err := st.UpdateAction(ctx, action); err != nil {
		t.Fatalf("UpdateAction: %v", err)
	}

	reply, err := k.CompleteStep(ctx, customer.ID, step.ID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CompleteStep after visibility narrowed: %v", err)
	}
	if reply == nil || reply.TxID == "" {
		t.Fatal("expected a settled completion despite the narrowed visibility")
	}
}

// TestStepCompletionResetsOnDeactivatedAction: liveness still gates completion. Deactivating the
// target resets the step to waiting with its price parked (§5, §10) — the distinction the binding
// rule preserves: visibility is bound once, liveness is checked at every dispatch.
func TestStepCompletionResetsOnDeactivatedAction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()

	owner := setupUser(t, st, "deact-owner", 500)
	customer := setupUser(t, st, "deact-customer", 0)
	action := setupWasmAction(t, st, owner.ID, "deact-action", "", 0)

	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	step, err := k.CreateStep(ctx, tr.ID, action.ID, nil, kernel.RequiredCaller{UserID: customer.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	action.Active = false
	if err := st.UpdateAction(ctx, action); err != nil {
		t.Fatalf("UpdateAction: %v", err)
	}

	_, err = k.CompleteStep(ctx, customer.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState completing a deactivated action, got %v", err)
	}
	// The step is reset to waiting, price still parked.
	got, err := k.ReadStep(ctx, owner.ID, step.ID)
	if err != nil {
		t.Fatalf("ReadStep: %v", err)
	}
	if got.Status != kernel.StepWaiting {
		t.Errorf("step status = %q, want waiting (reset after a liveness failure)", got.Status)
	}
}

// setupStepWithCompletionTrace sets up the state just after a BeginStepCall (step is running,
// completion trace exists, no tx). Simulates a crash mid-execution.
// Returns the step and its completion trace.
func setupStepWithCompletionTrace(t *testing.T, st kernel.Store, k *kernel.Kernel, ownerID string, price int64) (*kernel.Step, *kernel.Trace) {
	t.Helper()
	ctx := context.Background()
	action := setupLocalAction(t, st, ownerID, "recovery-action-"+uuid.New().String(), price)
	caller := setupUser(t, st, "recovery-caller-"+uuid.New().String(), 0)

	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: ownerID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	root := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: ownerID,
		ActionID:      action.ID,
		CallerUserID:  ownerID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := st.BeginRun(ctx, p, root, ownerID, price, 0, 0); err != nil {
		t.Fatalf("setupStepWithCompletionTrace: BeginRun: %v", err)
	}
	ptID := root.ID
	step, err := k.CreateStep(ctx, ptID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("setupStepWithCompletionTrace: CreateStep: %v", err)
	}
	ct := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: ownerID,
		ActionID:      action.ID,
		CallerUserID:  caller.ID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := st.BeginStepCall(ctx, step.ID, ct); err != nil {
		t.Fatalf("setupStepWithCompletionTrace: BeginStepCall: %v", err)
	}
	return step, ct
}

func TestRecoverReparkEmptyStepCompletionTrace(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "repark-owner", 100)
	step, _ := setupStepWithCompletionTrace(t, st, k, owner.ID, 100)

	// Completion trace is empty (available==price, locked==0): re-park path.
	if err := k.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// After re-park, root trace recovery cancels the step (not done, no tx_id).
	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepCancelled {
		t.Errorf("step.status=%s after Recover; want cancelled (re-park path)", got.Status)
	}
	if got.TxID != nil {
		t.Errorf("step.TxID=%v; want nil (re-park path does not settle the step)", got.TxID)
	}

	// Wallet invariant: all 100 credits restored to owner.
	u, _ := st.ReadUser(ctx, owner.ID)
	if u.Available+u.Locked != 100 {
		t.Errorf("wallet: available=%d locked=%d, want sum=100", u.Available, u.Locked)
	}
	if u.Available < 0 || u.Locked < 0 {
		t.Errorf("negative user balance: available=%d locked=%d", u.Available, u.Locked)
	}
}

func TestRecoverSettlesNonEmptyStepCompletionTrace(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "settle-owner", 200)
	step, ct := setupStepWithCompletionTrace(t, st, k, owner.ID, 200)

	// Make the completion trace non-empty: lock funds via a subcall.
	// This causes HasSettled=true so Recover settles rather than re-parks.
	ctID := ct.ID
	child := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     ct.ProcessID,
		ParentTraceID: &ctID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := st.BeginSubcall(ctx, ct.ID, child, 50); err != nil {
		t.Fatalf("BeginSubcall: %v", err)
	}

	if err := k.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Recover must settle the step (done, tx_id set), not re-park it.
	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepDone {
		t.Errorf("step.status=%s after Recover; want done (settle path)", got.Status)
	}
	if got.TxID == nil {
		t.Error("step.TxID should be non-nil after settlement")
	}

	// No negative balances.
	u, _ := st.ReadUser(ctx, owner.ID)
	if u.Available < 0 || u.Locked < 0 {
		t.Errorf("negative user balance after Recover: available=%d locked=%d", u.Available, u.Locked)
	}
}

// TestEndProcessFailsRunningStep verifies that force-closing a process with a running
// step-completion trace settles that completion as a failed CALL (transaction + receipt),
// not a silent balance drain: the step ends `done` with a tx_id, a failure transaction and
// receipt exist, the process closes, and the owner's funds are fully restored.
func TestEndProcessFailsRunningStep(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "ep-fail-owner", 100)
	step, ct := setupStepWithCompletionTrace(t, st, k, owner.ID, 100)
	processID := ct.ProcessID

	if err := k.EndProcess(ctx, owner.ID, processID); err != nil {
		t.Fatalf("EndProcess: %v", err)
	}

	// Step must be done with a tx_id (settled as a failed call), NOT cancelled.
	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepDone {
		t.Errorf("step.status=%s after EndProcess; want done (failed-call settlement)", got.Status)
	}
	if got.TxID == nil {
		t.Fatal("step.TxID must be set atomically with done")
	}

	// A failure transaction and a receipt must explain the balance change (audit conservation).
	tx, err := st.ReadTransaction(ctx, *got.TxID)
	if err != nil {
		t.Fatalf("ReadTransaction: %v", err)
	}
	if tx.Status != kernel.TxFailure {
		t.Errorf("tx.status=%s, want failure", tx.Status)
	}
	if tx.TraceID != ct.ID {
		t.Errorf("tx.trace_id=%s, want completion trace %s", tx.TraceID, ct.ID)
	}
	if _, err := st.ReadReceiptByTxID(ctx, *got.TxID); err != nil {
		t.Errorf("ReadReceiptByTxID: %v (every committed call must have a receipt)", err)
	}

	// Process closed; owner fully restored, no negative balances.
	proc, _ := st.ReadProcess(ctx, processID)
	if proc.Status != kernel.ProcessClosed {
		t.Errorf("process.status=%s, want closed", proc.Status)
	}
	assertUserBalance(t, st, owner.ID, 100, 0)
}

// failUnsettledListStore wraps a Store and fails ListUnsettledTracesForProcess for one
// process, simulating a store error mid-close.
type failUnsettledListStore struct {
	kernel.Store
	failProcessID string
	err           error
}

func (s *failUnsettledListStore) ListUnsettledTracesForProcess(ctx context.Context, processID string) ([]*kernel.Trace, error) {
	if processID == s.failProcessID {
		return nil, s.err
	}
	return s.Store.ListUnsettledTracesForProcess(ctx, processID)
}

// TestEndProcessAbortsOnSettlementError verifies that a store failure while enumerating
// unsettled traces aborts the close: the process must stay open and the owner's funds stay
// parked, never closed-and-credited with a half-settled subtree (the all-or-nothing audit
// guarantee, §5).
func TestEndProcessAbortsOnSettlementError(t *testing.T) {
	st := newTestStore(t)
	failErr := kernel.ErrInternal.Wrap("injected unsettled-list failure")
	fs := &failUnsettledListStore{Store: st, err: failErr}
	k := newTestKernel(fs)
	ctx := context.Background()

	owner := setupUser(t, st, "ep-abort-owner", 100)
	_, ct := setupStepWithCompletionTrace(t, st, k, owner.ID, 100)
	processID := ct.ProcessID
	fs.failProcessID = processID // inject only after setup

	if err := k.EndProcess(ctx, owner.ID, processID); err == nil {
		t.Fatal("EndProcess must abort when unsettled-trace enumeration fails")
	}

	// Process must remain open; owner's funds must stay parked (not returned).
	proc, _ := st.ReadProcess(ctx, processID)
	if proc.Status != kernel.ProcessOpen {
		t.Errorf("process.status=%s after aborted EndProcess; want open (not closed/credited)", proc.Status)
	}
	u, _ := st.ReadUser(ctx, owner.ID)
	if u.Locked == 0 {
		t.Errorf("owner.locked=0 after aborted EndProcess; funds must stay parked, not returned")
	}
}

// TestEndProcessFailsNonEmptyRunningStep is TestEndProcessFailsRunningStep with a settled
// subcall beneath the running completion trace: the settled subcall stays paid, the remainder
// refunds up, the completion settles as failure, and balances stay conserved (no negatives).
func TestEndProcessFailsNonEmptyRunningStep(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "ep-fail-ne-owner", 200)
	step, ct := setupStepWithCompletionTrace(t, st, k, owner.ID, 200)
	processID := ct.ProcessID

	// Lock funds into a child subcall so the completion trace is non-empty (HasSettled=true).
	ctID := ct.ID
	child := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     ct.ProcessID,
		ParentTraceID: &ctID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := st.BeginSubcall(ctx, ct.ID, child, 50); err != nil {
		t.Fatalf("BeginSubcall: %v", err)
	}

	if err := k.EndProcess(ctx, owner.ID, processID); err != nil {
		t.Fatalf("EndProcess: %v", err)
	}

	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepDone || got.TxID == nil {
		t.Errorf("step after EndProcess: status=%s tx_id=%v, want done with tx_id", got.Status, got.TxID)
	}
	tx, err := st.ReadTransaction(ctx, *got.TxID)
	if err != nil {
		t.Fatalf("ReadTransaction: %v", err)
	}
	if tx.Status != kernel.TxFailure {
		t.Errorf("tx.status=%s, want failure", tx.Status)
	}

	proc, _ := st.ReadProcess(ctx, processID)
	if proc.Status != kernel.ProcessClosed {
		t.Errorf("process.status=%s, want closed", proc.Status)
	}
	u, _ := st.ReadUser(ctx, owner.ID)
	if u.Available < 0 || u.Locked < 0 {
		t.Errorf("negative owner balance: available=%d locked=%d", u.Available, u.Locked)
	}
	if u.Available+u.Locked != 200 {
		t.Errorf("owner wallet sum=%d, want 200 (conservation)", u.Available+u.Locked)
	}
}

// TestStepCompleteRemoteProxyPersistsIdempotencyKey verifies Fix 3A: CompleteStep generates
// and persists idempotency_key and dispatch_json on the completion trace before dispatching.
func TestStepCompleteRemoteProxyPersistsIdempotencyKey(t *testing.T) {
	st := newTestStore(t)
	// Empty receiptJSON → parseAndVerifyRemoteReceipt returns ErrTimeout.
	k := newTestKernelWithHTTP(st, &fakeFederationHTTP{receiptJSON: ""})
	ctx := context.Background()

	owner := setupUser(t, st, "rp-idem-owner", 0)
	remoteAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID,
		Name: "rp-idem-action", Kind: kernel.KindRemoteProxy,
		Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		Source:    "https://remote.example.com/v1/federation/call?action=@owner/rp-idem-action&counterparty=us",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, remoteAction); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	caller := setupUser(t, st, "rp-idem-caller", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, remoteAction.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("expected ErrTimeout from remote-proxy step, got %v", err)
	}

	got, _ := st.ReadStep(ctx, step.ID)
	if got.CompletionTraceID == nil {
		t.Fatal("step.completion_trace_id must be set after BeginStepCall")
	}
	ct, err := st.ReadTrace(ctx, *got.CompletionTraceID)
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}
	if ct.IdempotencyKey == nil || *ct.IdempotencyKey == "" {
		t.Error("completion trace must have non-nil idempotency_key for retry recovery")
	}
	if ct.DispatchJSON == nil || *ct.DispatchJSON == "" {
		t.Error("completion trace must have non-nil dispatch_json for retry recovery")
	}
}

// TestStepCompleteRemoteProxyMissingExecutorSettlesFailure verifies the step-completion variant of the
// missing-FederationExecutor fix: instead of leaving the step running with stranded funds, the completion
// settles as a failure — the step is marked done with a tx, and the recorded gross is the parked step.price
// snapshot (exercising the Bug 2 fix in the same path), not the action's current price.
func TestStepCompleteRemoteProxyMissingExecutorSettlesFailure(t *testing.T) {
	st := newTestStore(t)
	// fakeSuccessHTTP implements HTTPExecutor but NOT FederationExecutor → triggers the !ok branch.
	k := newTestKernelWithHTTP(st, &fakeSuccessHTTP{})
	ctx := context.Background()

	procOwner := setupUser(t, st, "rpme2-procowner", 50)
	completer := setupUser(t, st, "rpme2-completer", 0)
	remoteAct := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: procOwner.ID,
		Name: "rpme2-action", Kind: kernel.KindRemoteProxy,
		Active: true, Visibility: kernel.VisibilityPublic, Price: 50,
		Source:    "https://remote.example.com/v1/federation/call?action=@rpme2-procowner/rpme2-action&counterparty=us",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, remoteAct); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	// Fund a root trace with 50 and park step.price=50 from it.
	_, root := beginTestRun(t, st, procOwner.ID, remoteAct)
	step, err := k.CreateStep(ctx, root.ID, remoteAct.ID, nil, kernel.RequiredCaller{UserID: completer.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	_, err = k.CompleteStep(ctx, completer.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState for missing federation executor, got %v", err)
	}

	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepDone {
		t.Errorf("step should be done after settled failure, got %s", got.Status)
	}
	if got.TxID == nil {
		t.Fatal("settled failure must record a tx_id on the step")
	}
	tx, err := st.ReadTransaction(ctx, *got.TxID)
	if err != nil {
		t.Fatalf("ReadTransaction: %v", err)
	}
	if tx.Status != kernel.TxFailure {
		t.Errorf("transaction status: got %q, want failure", tx.Status)
	}
	if tx.Gross != 50 {
		t.Errorf("gross must be the parked step.price snapshot 50, got %d", tx.Gross)
	}
}

// stagePendingRemoteChild funds a root call and hangs one dispatched remote-proxy subcall under it,
// left exactly as a dispatch that has not been answered: an idempotency key, a frozen pricing
// record, and no transaction. `age` backdates the dispatch so a test can drive the pending bound.
func stagePendingRemoteChild(t *testing.T, st kernel.Store, prefix string, parentPrice, childPrice int64, age time.Duration) (*kernel.Account, *kernel.Process, *kernel.Trace, *kernel.Trace) {
	t.Helper()
	ctx := context.Background()
	owner := setupUser(t, st, prefix+"-owner", 0)
	remoteAct := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID,
		Name: prefix + "-remote-act", Kind: kernel.KindRemoteProxy,
		Active: true, Visibility: kernel.VisibilityLocal, Price: childPrice,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, remoteAct); err != nil {
		t.Fatalf("CreateAction remote proxy: %v", err)
	}
	caller := setupUser(t, st, prefix+"-caller", parentPrice)
	parentAct := setupLocalAction(t, st, owner.ID, prefix+"-parent-act", parentPrice)
	p, root := beginTestRun(t, st, caller.ID, parentAct)

	ikey := uuid.New().String()
	child := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: owner.ID,
		ActionID:      remoteAct.ID,
		CallerUserID:  owner.ID,
		CreatedAt:     time.Now().UTC().Add(-age),
		// Settlement reads every pricing input from the dispatch record and nowhere else (§13).
		IdempotencyKey: &ikey,
		DispatchJSON:   kernel.DispatchRecordForTest(childPrice, childPrice, 0, 0, 0, ""),
	}
	if err := st.BeginSubcall(ctx, root.ID, child, childPrice); err != nil {
		t.Fatalf("BeginSubcall: %v", err)
	}
	return caller, p, root, child
}

// unsettledTraces names the traces in a process that no transaction has answered yet.
func unsettledTraces(t *testing.T, st kernel.Store, processID string) []string {
	t.Helper()
	traces, err := st.ListUnsettledTracesForProcess(context.Background(), processID)
	if err != nil {
		t.Fatalf("ListUnsettledTracesForProcess: %v", err)
	}
	ids := make([]string, len(traces))
	for i, tr := range traces {
		ids[i] = tr.ID
	}
	return ids
}

// TestParentFailureLeavesDispatchedChildPending pins D3's order against a dispatched child: the
// remote call may have executed abroad, so an interrupted parent neither presumes it dead nor
// settles ahead of it. Both wait; the process stays open on them; nothing is refunded for work the
// seller may already be owed for.
func TestParentFailureLeavesDispatchedChildPending(t *testing.T) {
	st := newTestStore(t)
	// No receipt from the peer: the dispatch stays unanswered on every retry.
	k := newTestKernelWithHTTP(st, &fakeFederationHTTP{receiptJSON: ""})
	ctx := context.Background()

	const parentPrice, childPrice = 100, 60
	caller, p, root, child := stagePendingRemoteChild(t, st, "pfc", parentPrice, childPrice, 0)

	// Recovery records the orphaned parent's outcome (interrupted) and retries — never settles —
	// the child; the parent settles only after it (D3).
	if err := k.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	got := unsettledTraces(t, st, p.ID)
	if len(got) != 2 || (got[0] != child.ID && got[1] != child.ID) {
		t.Fatalf("unsettled traces after recovery: got %v, want the parent waiting on its dispatched child %s", got, child.ID)
	}
	if tr, _ := st.ReadTrace(ctx, root.ID); tr.OutcomeJSON == nil {
		t.Fatal("the interrupted parent recorded no outcome to settle with later")
	}
	proc, err := st.ReadProcess(ctx, p.ID)
	if err != nil {
		t.Fatalf("ReadProcess: %v", err)
	}
	if proc.Status != kernel.ProcessOpen {
		t.Errorf("process status %q, want open while a call still awaits its receipt", proc.Status)
	}
	// The money is conserved and still reserved: nothing has come back to the caller, because
	// nothing has been settled that could return it.
	u, _ := st.ReadUser(ctx, caller.ID)
	if u.Available+u.Locked != parentPrice {
		t.Errorf("caller wallet: available=%d locked=%d, want sum=%d", u.Available, u.Locked, parentPrice)
	}
	if u.Locked != parentPrice {
		t.Errorf("caller.locked=%d, want the whole %d still reserved on the open process", u.Locked, parentPrice)
	}
	// Nothing has been released yet: the parent's own budget waits with it.
	if proc.Available+proc.Locked != parentPrice {
		t.Errorf("process holds available=%d locked=%d, want the whole %d while both wait", proc.Available, proc.Locked, parentPrice)
	}
}

// blockingHTTP executes an http action only once released: a call caught mid-execution. With
// fail set, the release is a failure.
type blockingHTTP struct {
	release chan struct{}
	fail    bool
}

func (b *blockingHTTP) Execute(ctx context.Context, _ *kernel.Action, _ map[string]any, _, _ string) (map[string]any, error) {
	select {
	case <-b.release:
		if b.fail {
			return nil, errors.New("child blew up")
		}
		return map[string]any{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// composingExec is a script whose action makes one subcall through the real host and then fails
// while that subcall is still executing — the composition race, exactly as a script would produce
// it. The test closes `proceed` once it has seen the child start.
type composingExec struct {
	fakeScriptExec
	child   string
	proceed chan struct{}
}

func (c *composingExec) Execute(_ context.Context, _ []byte, _ []byte, host kernel.HostFunctions) ([]byte, error) {
	go func() { _, _ = host.Call(context.Background(), c.child, []byte(`{}`)) }()
	<-c.proceed
	return nil, errors.New("parent blew up")
}

// TestInboundFailureChargesOnlyWhatChildrenSettled is the money behind D3's order, on the side
// where it is money: a call served to a peer fails while a child is running, and the receipt the
// buyer settles on is signed only once the child's outcome is known — nothing charged when the
// child fails, the child's price when it succeeds. Signed at the parent's failure it would have
// charged the child's allocation either way, and the seller would have kept the refund too.
func TestInboundFailureChargesOnlyWhatChildrenSettled(t *testing.T) {
	const parentPrice, childPrice = 100, 40
	for name, tc := range map[string]struct {
		childFails bool
		wantCharge int64
	}{
		"child fails":    {true, 0},
		"child succeeds": {false, childPrice},
	} {
		t.Run(name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			provider := setupUser(t, st, "icc-provider", 1000)
			if err := st.UpsertKernel(ctx, "aWNjLXBlZXI", "icc-peer", "", "", "", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			peer := &kernel.Account{ID: uuid.New().String(), KernelPublicKey: "aWNjLXBlZXI", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			if err := st.CreateUser(ctx, peer); err != nil {
				t.Fatal(err)
			}
			parent := &kernel.Action{ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "icc-parent", Kind: kernel.KindWasm,
				Active: true, Visibility: kernel.VisibilityPublic, Price: parentPrice, Source: "wat",
				InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			child := &kernel.Action{ID: uuid.New().String(), OwnerUserID: provider.ID, Name: "icc-child", Kind: kernel.KindHTTP,
				Active: true, Visibility: kernel.VisibilityLocal, Price: childPrice, Source: "https://child.example/run",
				InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			for _, a := range []*kernel.Action{parent, child} {
				if err := st.CreateAction(ctx, a); err != nil {
					t.Fatal(err)
				}
			}
			exec := &composingExec{child: provider.Handle + "/" + child.Name, proceed: make(chan struct{})}
			childHTTP := &blockingHTTP{release: make(chan struct{}), fail: tc.childFails}
			k := newKernel(testConfig(), kernel.Dependencies{Store: st, Scripts: exec, HTTP: childHTTP})
			rec := &kernel.IdempotencyRecord{ID: uuid.New().String(), IdempotencyKey: "icc-" + name, CounterpartyUserID: peer.ID,
				CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
			if err := st.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
				t.Fatal(err)
			}

			type outcome struct {
				reply *kernel.CallReply
				err   error
			}
			done := make(chan outcome, 1)
			go func() {
				r, err := k.RunFederated(ctx, peer.ID, provider.ID, parent.Name, map[string]any{}, rec.ID, kernel.BuyerTerms{})
				done <- outcome{r, err}
			}()
			// The child has started when two traces of the process are unsettled.
			var procID string
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) && procID == "" {
				if procs, _ := st.ListProcesses(ctx, provider.ID, 10, 0); len(procs) == 1 {
					if len(unsettledTraces(t, st, procs[0].ID)) == 2 {
						procID = procs[0].ID
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if procID == "" {
				t.Fatal("the child never started")
			}
			close(exec.proceed)
			got := <-done
			if got.err == nil || !got.reply.Deferred() {
				t.Fatalf("the served call must fail with its outcome deferred: reply=%+v err=%v", got.reply, got.err)
			}
			if r, _ := st.ReadIdempotencyRecordByID(ctx, rec.ID); r.Status != "pending" {
				t.Fatalf("the peer's record must stay pending until the charge is final, got %q", r.Status)
			}

			close(childHTTP.release)
			var parentTx *kernel.Transaction
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && parentTx == nil; {
				txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: procID})
				for _, tx := range txs {
					if tx.TraceID == got.reply.TraceID {
						parentTx = tx
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if parentTx == nil {
				t.Fatal("the parent never settled after its child did")
			}
			receipt, err := st.ReadReceiptByTxID(ctx, parentTx.ID)
			if err != nil || receipt == nil {
				t.Fatalf("no receipt for the settled parent: %v", err)
			}
			if receipt.Charge != tc.wantCharge {
				t.Errorf("the buyer is charged %d, want %d: what the child consumed and nothing else", receipt.Charge, tc.wantCharge)
			}
			if r, _ := st.ReadIdempotencyRecordByID(ctx, rec.ID); r.Status != "complete" {
				t.Errorf("the peer's record is %q, want complete once the charge is final", r.Status)
			} else if !strings.Contains(r.ResultJSON, `"code":"`+kernel.ErrExecutionFailed.Code+`"`) {
				t.Errorf("a deferred failure replays as %s, want the class it failed with (%s)", r.ResultJSON, kernel.ErrExecutionFailed.Code)
			}
			if proc, _ := st.ReadProcess(ctx, procID); proc.Status != kernel.ProcessClosed {
				t.Errorf("process %q, want closed", proc.Status)
			}
		})
	}
}

// TestParentFailureLeavesExecutingChildAlone pins §5's one-settler rule against the case that
// makes it necessary: a capability callback has funded a local child and is executing it when
// the parent fails. The parent settles alone. The child finishes, is paid for the work it did,
// and its refund reroutes to the process, which then closes whole.
func TestParentFailureLeavesExecutingChildAlone(t *testing.T) {
	st := newTestStore(t)
	child := &blockingHTTP{release: make(chan struct{})}
	k := newKernel(testConfig(), kernel.Dependencies{Store: st,
		Scripts: &fakeScriptExec{err: errors.New("parent blew up")}, HTTP: child})
	ctx := context.Background()

	const parentPrice, childPrice = 100, 40
	owner := setupUser(t, st, "pec-owner", 0)
	caller := setupUser(t, st, "pec-caller", parentPrice)
	parentAct := setupWasmAction(t, st, owner.ID, "pec-parent", "", parentPrice)
	childAct := &kernel.Action{ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "pec-child", Kind: kernel.KindHTTP,
		Active: true, Visibility: kernel.VisibilityLocal, Price: childPrice, Source: "https://child.example/run",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := st.CreateAction(ctx, childAct); err != nil {
		t.Fatal(err)
	}
	p, root := beginTestRun(t, st, caller.ID, parentAct)

	// The callback: a subcall on the parent's trace, as the parent action's owner, mid-execution.
	done := make(chan error, 1)
	go func() {
		_, err := k.TestCall(ctx, kernel.TestCallRequest{CallerID: owner.ID, ParentTraceID: root.ID,
			TargetUserID: owner.ID, ActionName: childAct.Name, Args: map[string]any{}})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(unsettledTraces(t, st, p.ID)) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := unsettledTraces(t, st, p.ID); len(got) != 2 {
		t.Fatalf("the child never started: unsettled=%v", got)
	}

	// The parent fails while the child is still running.
	if _, err := k.TestCall(ctx, kernel.TestCallRequest{CallerID: caller.ID, Action: parentAct,
		Args: map[string]any{}, ExistingTraceID: root.ID}); err == nil {
		t.Fatal("parent call must fail")
	}
	if got := unsettledTraces(t, st, p.ID); len(got) != 2 {
		t.Fatalf("the parent must wait for its executing child, not settle ahead of it: unsettled=%v", got)
	}
	if proc, _ := st.ReadProcess(ctx, p.ID); proc.Status != kernel.ProcessOpen {
		t.Fatalf("process closed over an executing child")
	}

	// The child finishes: paid for its work; its settlement settles the waiting parent.
	close(child.release)
	if err := <-done; err != nil {
		t.Fatalf("the executing child must settle on its own: %v", err)
	}
	if got := unsettledTraces(t, st, p.ID); len(got) != 0 {
		t.Fatalf("still unsettled: %v", got)
	}
	if proc, _ := st.ReadProcess(ctx, p.ID); proc.Status != kernel.ProcessClosed {
		t.Errorf("process %q, want closed once every call has settled", proc.Status)
	}
	net, fee := testEconomy().Fee(childPrice)
	if u, _ := st.ReadUser(ctx, owner.ID); u.Available != net {
		t.Errorf("the child's provider holds %d, want its net %d: work done was not paid", u.Available, net)
	}
	if u, _ := st.ReadUser(ctx, caller.ID); u.Available != parentPrice-childPrice || u.Locked != 0 {
		t.Errorf("caller available=%d locked=%d, want %d and 0", u.Available, u.Locked, parentPrice-childPrice)
	}
	if u, _ := st.ReadUser(ctx, testIssuerUserID); u.Available < fee {
		t.Errorf("the fee recipient holds %d, want at least the child's fee %d", u.Available, fee)
	}
}

// TestParentFailureLeavesRunningStepAlone: a claimed step's price has left the parent's lock and
// funds its completion trace, so it is a call in flight with its own settler (§5). A parent failing
// meanwhile cancels only waiting steps; the running one completes, is paid, and its refund follows
// the settled parent's to the process.
func TestParentFailureLeavesRunningStepAlone(t *testing.T) {
	st := newTestStore(t)
	target := &blockingHTTP{release: make(chan struct{})}
	k := newKernel(testConfig(), kernel.Dependencies{Store: st,
		Scripts: &fakeScriptExec{err: errors.New("parent blew up")}, HTTP: target})
	ctx := context.Background()

	const parentPrice, stepPrice = 100, 40
	owner := setupUser(t, st, "prs-owner", 0)
	caller := setupUser(t, st, "prs-caller", parentPrice)
	completer := setupUser(t, st, "prs-completer", 0)
	parentAct := setupWasmAction(t, st, owner.ID, "prs-parent", "", parentPrice)
	stepAct := &kernel.Action{ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "prs-step", Kind: kernel.KindHTTP,
		Active: true, Visibility: kernel.VisibilityLocal, Price: stepPrice, Source: "https://step.example/run",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := st.CreateAction(ctx, stepAct); err != nil {
		t.Fatal(err)
	}
	p, root := beginTestRun(t, st, caller.ID, parentAct)
	step, err := k.CreateStep(ctx, root.ID, stepAct.ID, json.RawMessage(`{}`), kernel.RequiredCaller{UserID: completer.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// The completer claims the step and is executing it.
	done := make(chan error, 1)
	go func() {
		_, err := k.CompleteStep(ctx, completer.ID, step.ID, json.RawMessage(`{}`))
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := st.ReadStep(ctx, step.ID); s != nil && s.Status == kernel.StepRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s, _ := st.ReadStep(ctx, step.ID); s == nil || s.Status != kernel.StepRunning {
		t.Fatalf("the completion never claimed the step")
	}

	// The creating call fails while the step is running.
	if _, err := k.TestCall(ctx, kernel.TestCallRequest{CallerID: caller.ID, Action: parentAct,
		Args: map[string]any{}, ExistingTraceID: root.ID}); err == nil {
		t.Fatal("parent call must fail")
	}
	if s, _ := st.ReadStep(ctx, step.ID); s.Status != kernel.StepRunning {
		t.Fatalf("the parent's failure touched a running step: status %q", s.Status)
	}
	if proc, _ := st.ReadProcess(ctx, p.ID); proc.Status != kernel.ProcessOpen {
		t.Fatal("process closed over a running step")
	}
	if txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID}); len(txs) != 0 {
		t.Fatalf("the parent settled ahead of the running step: %+v", txs)
	}

	// The step completes: paid, done; its settlement settles the parent, whose refund excludes
	// the price the step consumed; the process closes whole.
	close(target.release)
	if err := <-done; err != nil {
		t.Fatalf("the running step must settle on its own: %v", err)
	}
	parentTx, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID, TargetUserID: owner.ID})
	found := false
	for _, tx := range parentTx {
		if tx.TraceID == root.ID {
			found = true
			if tx.Refund != parentPrice-stepPrice {
				t.Errorf("parent refund=%d, want %d: the completed step's price is consumed", tx.Refund, parentPrice-stepPrice)
			}
		}
	}
	if !found {
		t.Fatal("the parent never settled after its step completed")
	}
	if s, _ := st.ReadStep(ctx, step.ID); s.Status != kernel.StepDone || s.TxID == nil {
		t.Errorf("step %q with tx %v, want done with its transaction", s.Status, s.TxID)
	}
	if proc, _ := st.ReadProcess(ctx, p.ID); proc.Status != kernel.ProcessClosed {
		t.Errorf("process %q, want closed", proc.Status)
	}
	net, _ := testEconomy().Fee(stepPrice)
	if u, _ := st.ReadUser(ctx, owner.ID); u.Available != net {
		t.Errorf("the step's provider holds %d, want its net %d", u.Available, net)
	}
	if u, _ := st.ReadUser(ctx, caller.ID); u.Available != parentPrice-stepPrice || u.Locked != 0 {
		t.Errorf("caller available=%d locked=%d, want %d and 0: the step's price was refunded twice or never", u.Available, u.Locked, parentPrice-stepPrice)
	}
}

// TestFundingRefusesSettledTraceAtAnyPrice: the rules a spend depends on live in the funding
// statement (§6, §9). A settled trace, or a closed process, funds nothing — zero included, which
// no balance check alone would refuse — so a capability that expired between its check and its
// write can start no work and park no step.
func TestFundingRefusesSettledTraceAtAnyPrice(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{}`})
	ctx := context.Background()
	owner := setupUser(t, st, "frs-owner", 0)
	caller := setupUser(t, st, "frs-caller", 10)
	parentAct := setupWasmAction(t, st, owner.ID, "frs-parent", "", 10)
	free := setupLocalAction(t, st, owner.ID, "frs-free", 0)
	p, root := beginTestRun(t, st, caller.ID, parentAct)
	if _, err := k.TestCall(ctx, kernel.TestCallRequest{CallerID: caller.ID, Action: parentAct, Args: map[string]any{}, ExistingTraceID: root.ID}); err != nil {
		t.Fatalf("settling the parent: %v", err)
	}
	if proc, _ := st.ReadProcess(ctx, p.ID); proc.Status != kernel.ProcessClosed {
		t.Fatal("fixture: the process should have closed")
	}
	_, err := k.TestCall(ctx, kernel.TestCallRequest{CallerID: owner.ID, ParentTraceID: root.ID,
		TargetUserID: owner.ID, ActionName: free.Name, Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("a free subcall on a settled trace: want ErrInvalidState, got %v", err)
	}
	if _, err := k.CreateStep(ctx, root.ID, free.ID, json.RawMessage(`{}`), kernel.RequiredCaller{UserID: owner.ID}); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("a free step on a settled trace: want ErrInvalidState, got %v", err)
	}
	if traces, _ := st.ListTraces(ctx, p.ID); len(traces) != 1 {
		t.Errorf("a trace was funded under a settled parent: %d traces", len(traces))
	}
	if steps, _ := st.ListSteps(ctx, owner.ID, p.ID, "", true, 10, 0); len(steps) != 0 {
		t.Errorf("a step was parked in a closed process: %d", len(steps))
	}
}

// claimsDuringSnapshot is a store that claims a waiting step the moment forced closure has taken
// its snapshot of unsettled traces: the exact window between the settlement pass and the close.
type claimsDuringSnapshot struct {
	kernel.Store
	stepID string
	armed  bool
}

func (c *claimsDuringSnapshot) ListUnsettledTracesForProcess(ctx context.Context, processID string) ([]*kernel.Trace, error) {
	traces, err := c.Store.ListUnsettledTracesForProcess(ctx, processID)
	if err == nil && c.armed {
		c.armed = false
		ct := &kernel.Trace{ID: uuid.New().String(), ProcessID: processID, CreatedAt: time.Now().UTC()}
		if step, rerr := c.Store.ReadStep(ctx, c.stepID); rerr == nil {
			ct.ActionOwnerID, ct.CallerUserID, ct.ActionID = step.RequiredCallerUserID, step.RequiredCallerUserID, step.ActionID
		}
		if berr := c.Store.BeginStepCall(ctx, c.stepID, ct); berr != nil {
			return nil, berr
		}
	}
	return traces, err
}

// TestEndProcessRefusesToCloseOverACallStartedMeanwhile: forced closure settles what it saw and
// then closes — and a completion that started in between is in flight. The close refuses in the
// same transaction that would have committed it, so no refund can ever land in a closed process;
// ending again settles the newcomer and returns everything to the owner.
func TestEndProcessRefusesToCloseOverACallStartedMeanwhile(t *testing.T) {
	st := newTestStore(t)
	race := &claimsDuringSnapshot{Store: st}
	k := newTestKernel(race)
	ctx := context.Background()
	const price = 100
	owner := setupUser(t, st, "epr-owner", price)
	completer := setupUser(t, st, "epr-completer", 0)
	act := setupLocalAction(t, st, owner.ID, "epr-act", price)
	stepAct := setupLocalAction(t, st, owner.ID, "epr-step", 40)
	p, root := beginTestRun(t, st, owner.ID, act)
	step, err := k.CreateStep(ctx, root.ID, stepAct.ID, json.RawMessage(`{}`), kernel.RequiredCaller{UserID: completer.ID})
	if err != nil {
		t.Fatal(err)
	}
	race.stepID, race.armed = step.ID, true

	err = k.EndProcess(ctx, owner.ID, p.ID)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("closing over a call that started meanwhile: want ErrInvalidState, got %v", err)
	}
	if proc, _ := st.ReadProcess(ctx, p.ID); proc.Status != kernel.ProcessOpen {
		t.Fatal("the process closed over a running step")
	}
	if s, _ := st.ReadStep(ctx, step.ID); s.Status != kernel.StepRunning {
		t.Fatalf("the claimed step is %q, want running", s.Status)
	}
	if u, _ := st.ReadUser(ctx, owner.ID); u.Available+u.Locked != price {
		t.Fatalf("money moved on a refused close: available=%d locked=%d", u.Available, u.Locked)
	}

	// Asked again, the closure settles the newcomer too and returns everything.
	if err := k.EndProcess(ctx, owner.ID, p.ID); err != nil {
		t.Fatalf("second EndProcess: %v", err)
	}
	if proc, _ := st.ReadProcess(ctx, p.ID); proc.Status != kernel.ProcessClosed {
		t.Errorf("process %q, want closed", proc.Status)
	}
	if u, _ := st.ReadUser(ctx, owner.ID); u.Available != price || u.Locked != 0 {
		t.Errorf("owner available=%d locked=%d, want %d and 0", u.Available, u.Locked, price)
	}
}

// reparkFails / settleFails are stores whose one recovery step fails with errDiskGone: recovery
// must stop THERE, which the returned error must say by wrapping it.
var errDiskGone = errors.New("disk gone")

type reparkFails struct{ kernel.Store }

func (reparkFails) ResetStepAndRepark(context.Context, string) error { return errDiskGone }

type settleFails struct{ kernel.Store }

func (settleFails) CommitFailedCall(context.Context, *kernel.Transaction, func(int64) (*kernel.Receipt, error), string, string, string, string, int64, *kernel.Stats, string, string, string) error {
	return errDiskGone
}

// TestRecoveryAbortsOnAFailedMoneyStep: a recovery that cannot re-park or settle refuses to
// continue — and so the boot fails — rather than sweep on and mark a step waiting whose price is
// still in an unreferenced completion trace, a step nothing could ever complete again (G4).
func TestRecoveryAbortsOnAFailedMoneyStep(t *testing.T) {
	cases := map[string]struct {
		wrap  func(kernel.Store) kernel.Store
		abort error // the failure recovery must stop at, as the returned error reports it
		check func(t *testing.T, st kernel.Store, step *kernel.Step, ct *kernel.Trace)
	}{
		// The re-park fails first: nothing after it ran, so the step was not flipped to waiting
		// over a price still sitting in its completion trace.
		"re-park fails": {func(s kernel.Store) kernel.Store { return reparkFails{s} }, errDiskGone,
			func(t *testing.T, st kernel.Store, step *kernel.Step, ct *kernel.Trace) {
				if s, _ := st.ReadStep(context.Background(), step.ID); s.Status != kernel.StepRunning {
					t.Errorf("step %q, want still running for the next attempt", s.Status)
				}
				if settled, _ := st.TraceHasTransaction(context.Background(), ct.ID); settled {
					t.Error("recovery went on to settle the completion trace after the re-park failed")
				}
			}},
		// The re-park succeeds and the orphaned parent's settlement fails: the boot fails and the
		// parent stays unsettled for the next attempt, never presumed done.
		// (A settlement wraps its store failure as an internal error, the class the boot reports.)
		"settle fails": {func(s kernel.Store) kernel.Store { return settleFails{s} }, kernel.ErrInternal,
			func(t *testing.T, st kernel.Store, step *kernel.Step, _ *kernel.Trace) {
				if settled, _ := st.TraceHasTransaction(context.Background(), *step.ParentTraceID); settled {
					t.Error("the parent trace was marked settled by a settlement that failed")
				}
			}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st := newTestStore(t)
			k := newTestKernel(st)
			owner := setupUser(t, st, "rab-owner", 100)
			step, ct := setupStepWithCompletionTrace(t, st, k, owner.ID, 100)
			if err := newTestKernel(tc.wrap(st)).Recover(context.Background()); !errors.Is(err, tc.abort) {
				t.Fatalf("recovery must abort at the failed money step and say so; got %v", err)
			}
			tc.check(t, st, step, ct)
		})
	}
}

// TestRecoveredParentChargesWhatChildrenConsumed: a parent interrupted by a crash records no
// allocation, and what is left on its trace no longer shows what its settled children kept.
// Recovery must reconstruct the allocation from both, or an inbound call recovered after a crash
// would sign a charge that omits the work its children were paid for.
func TestRecoveredParentChargesWhatChildrenConsumed(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	const parentPrice, childPrice = 100, 40
	owner := setupUser(t, st, "rcc-owner", 0)
	caller := setupUser(t, st, "rcc-caller", parentPrice)
	parentAct := setupLocalAction(t, st, owner.ID, "rcc-parent", parentPrice)
	childAct := setupLocalAction(t, st, owner.ID, "rcc-child", childPrice)
	p, root := beginTestRun(t, st, caller.ID, parentAct)
	child := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ActionOwnerID: owner.ID, ActionID: childAct.ID,
		CallerUserID: owner.ID, CreatedAt: time.Now().UTC()}
	if err := st.BeginSubcall(ctx, root.ID, child, childPrice); err != nil {
		t.Fatal(err)
	}
	// The child settled successfully before the crash: its price left the parent for good.
	childTx := &kernel.Transaction{ID: uuid.New().String(), ProcessID: p.ID, TraceID: child.ID, ParentTraceID: root.ID,
		OwnerUserID: caller.ID, CallerUserID: owner.ID, TargetUserID: owner.ID, ActionID: childAct.ID,
		Status: kernel.TxSuccess, Gross: childPrice, Net: childPrice, StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC()}
	rc := &kernel.Receipt{ID: uuid.New().String(), IssuerUserID: testIssuerUserID, TxID: childTx.ID, TraceID: child.ID,
		ActionID: childAct.ID, Status: kernel.TxSuccess, Gross: childPrice, Net: childPrice, Charge: childPrice, CreatedAt: time.Now().UTC()}
	if err := st.CommitCall(ctx, childTx, rc, child.ID, root.ID, kernel.CallerTrace, owner.ID, testIssuerUserID, childPrice, 0, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := k.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	var parentTx *kernel.Transaction
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	for _, tx := range txs {
		if tx.TraceID == root.ID {
			parentTx = tx
		}
	}
	if parentTx == nil {
		t.Fatal("recovery did not settle the interrupted parent")
	}
	if parentTx.Gross != parentPrice || parentTx.Refund != parentPrice-childPrice {
		t.Errorf("recovered parent gross=%d refund=%d, want %d and %d", parentTx.Gross, parentTx.Refund, parentPrice, parentPrice-childPrice)
	}
	if receipt, _ := st.ReadReceiptByTxID(ctx, parentTx.ID); receipt == nil || receipt.Charge != childPrice {
		t.Errorf("recovered parent charges %v, want the %d its child kept", receipt, childPrice)
	}
}

// TestSettleReadyIsTheSafetyNet: a parent whose outcome is recorded and whose last child has
// settled without the climb that should have followed — a cancelled request, a crash — is
// settled by the next sweep, which the retry ticker and startup both run. Nothing depends on
// the climb having happened.
func TestSettleReadyIsTheSafetyNet(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{err: errors.New("parent blew up")})
	ctx := context.Background()
	const parentPrice, childPrice = 100, 40
	owner := setupUser(t, st, "srs-owner", 0)
	caller := setupUser(t, st, "srs-caller", parentPrice)
	parentAct := setupWasmAction(t, st, owner.ID, "srs-parent", "", parentPrice)
	childAct := setupLocalAction(t, st, owner.ID, "srs-child", childPrice)
	p, root := beginTestRun(t, st, caller.ID, parentAct)
	child := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ActionOwnerID: owner.ID, ActionID: childAct.ID,
		CallerUserID: owner.ID, CreatedAt: time.Now().UTC()}
	if err := st.BeginSubcall(ctx, root.ID, child, childPrice); err != nil {
		t.Fatal(err)
	}
	// The parent fails with the child in flight: its outcome is recorded by the refused commit.
	if _, err := k.TestCall(ctx, kernel.TestCallRequest{CallerID: caller.ID, Action: parentAct, Args: map[string]any{}, ExistingTraceID: root.ID}); err == nil {
		t.Fatal("parent must fail")
	}
	if tr, _ := st.ReadTrace(ctx, root.ID); tr.OutcomeJSON == nil {
		t.Fatal("the refused commit did not record the outcome")
	}
	if ready, _ := st.ListReadyTraces(ctx); len(ready) != 0 {
		t.Fatalf("nothing is ready while the child is in flight, got %d", len(ready))
	}
	// The child settles through the store alone — no kernel, no climb — as a lost pass would leave it.
	settleTrace(t, st, p.ID, child.ID, caller.ID, kernel.CallerTrace, root.ID, childPrice, kernel.TxFailure)
	if ready, _ := st.ListReadyTraces(ctx); len(ready) != 1 || ready[0].ID != root.ID {
		t.Fatalf("the parent must be ready once its last child settled, got %v", ready)
	}
	k.SettleReady(ctx)
	if settled, _ := st.TraceHasTransaction(ctx, root.ID); !settled {
		t.Fatal("the sweep did not settle the ready parent")
	}
	if proc, _ := st.ReadProcess(ctx, p.ID); proc.Status != kernel.ProcessClosed {
		t.Errorf("process %q, want closed", proc.Status)
	}
	if u, _ := st.ReadUser(ctx, caller.ID); u.Available != parentPrice || u.Locked != 0 {
		t.Errorf("caller available=%d locked=%d, want %d and 0", u.Available, u.Locked, parentPrice)
	}
}

// settleTrace settles a trace through the store the way the kernel would, for tests that need a
// child settled with no kernel in the loop.
func settleTrace(t *testing.T, st kernel.Store, processID, traceID, ownerID, walletKind, walletID string, gross int64, status kernel.TxStatus) {
	t.Helper()
	ctx := context.Background()
	tx := &kernel.Transaction{ID: uuid.New().String(), ProcessID: processID, TraceID: traceID, OwnerUserID: ownerID,
		CallerUserID: ownerID, TargetUserID: ownerID, Status: status, Gross: gross, Reason: "test",
		StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC()}
	build := func(refund int64) (*kernel.Receipt, error) {
		return &kernel.Receipt{ID: uuid.New().String(), IssuerUserID: testIssuerUserID, TxID: tx.ID, TraceID: traceID,
			Status: status, Gross: gross, Charge: gross - refund, CreatedAt: time.Now().UTC()}, nil
	}
	if err := st.CommitFailedCall(ctx, tx, build, traceID, walletID, walletKind, testIssuerUserID, gross, nil, "", "interrupted", ""); err != nil {
		t.Fatalf("settleTrace: %v", err)
	}
}

// TestRecoveryHonoursARecordedOutcome: a call that succeeded while a child was in flight recorded
// that success; a restart must not overwrite it with "interrupted" merely because the trace has no
// transaction yet. When the child settles, the parent settles as the success it was.
func TestRecoveryHonoursARecordedOutcome(t *testing.T) {
	st := newTestStore(t)
	fake := &fakeFederationHTTP{receiptJSON: ""}
	k := newKernel(testConfig(), kernel.Dependencies{Store: st, HTTP: fake, Scripts: &fakeScriptExec{result: `{"ok":true}`}})
	ctx := context.Background()
	const parentPrice, childPrice = 100, 60
	caller, p, root, child := stagePendingRemoteChild(t, st, "rho", parentPrice, childPrice, time.Hour)
	// The parent succeeds while its dispatched child is pending: the commit is refused and the
	// success is recorded. (stagePendingRemoteChild's parent is a local action; run it as wasm.)
	parentAct, _ := st.ReadAction(ctx, root.ActionID)
	parentAct.Kind = kernel.KindWasm
	if reply, err := k.TestCall(ctx, kernel.TestCallRequest{CallerID: caller.ID, Action: parentAct, Args: map[string]any{}, ExistingTraceID: root.ID}); err != nil || !reply.Deferred() {
		t.Fatalf("want a deferred success, got reply=%+v err=%v", reply, err)
	}
	// A restart: recovery sees an orphan parent with a child in flight.
	if err := k.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	var o kernel.TraceOutcome
	tr, _ := st.ReadTrace(ctx, root.ID)
	if tr.OutcomeJSON == nil || json.Unmarshal([]byte(*tr.OutcomeJSON), &o) != nil || o.Status != kernel.TxSuccess {
		t.Fatalf("recovery overwrote the recorded success: %v", tr.OutcomeJSON)
	}
	// The child expires; the sweep settles the parent as what it recorded.
	cfg := testConfig()
	cfg.RemotePendingMaxAge = time.Minute
	newKernel(cfg, kernel.Dependencies{Store: st, HTTP: &fakeFederationHTTP{receiptJSON: ""}}).RetryPendingRemoteDispatches(ctx)
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	for _, tx := range txs {
		if tx.TraceID == root.ID && tx.Status != kernel.TxSuccess {
			t.Errorf("the parent settled as %q (%s), want the success it recorded", tx.Status, tx.Reason)
		}
	}
	if settled, _ := st.TraceHasTransaction(ctx, root.ID); !settled {
		t.Fatal("the parent never settled")
	}
	_ = child
}

// TestDeferredParentSettlesAfterItsChild is the other half: once the pending bound expires, the
// child settles, and its settlement settles the parent whose outcome was waiting on it (D3). The
// parent's refund then includes what the child gave back, so its receipt charges nothing that was
// not consumed — and the caller is made whole through the ordinary refund, no rerouting.
func TestDeferredParentSettlesAfterItsChild(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithHTTP(st, &fakeFederationHTTP{receiptJSON: ""})
	ctx := context.Background()

	const parentPrice, childPrice = 100, 60
	caller, p, root, child := stagePendingRemoteChild(t, st, "prr", parentPrice, childPrice, time.Hour)

	// First the parent's outcome is recorded, with the child still dispatched and unanswered.
	if err := k.Recover(ctx); err != nil {
		t.Fatalf("Recover (parent defers): %v", err)
	}
	if got := unsettledTraces(t, st, p.ID); len(got) != 2 {
		t.Fatalf("parent and child must both be unsettled while the child is pending, unsettled=%v", got)
	}
	// Then the pending bound expires and the child settles, after its parent. A kernel with a
	// shorter bound is the same running server one hour later.
	cfg := testConfig()
	cfg.RemotePendingMaxAge = time.Minute
	expire := newKernel(cfg, kernel.Dependencies{Store: st, HTTP: &fakeFederationHTTP{receiptJSON: ""}})
	expire.RetryPendingRemoteDispatches(ctx)

	if got := unsettledTraces(t, st, p.ID); len(got) != 0 {
		t.Fatalf("unsettled traces after the bound expired: got %v, want none", got)
	}
	var rootTx *kernel.Transaction
	if txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID}); true {
		for _, tx := range txs {
			if tx.TraceID == root.ID {
				rootTx = tx
			}
		}
	}
	if rootTx == nil {
		t.Fatal("the parent must have settled once its child did")
	}
	if rootTx.Refund != parentPrice || rootTx.Gross-rootTx.Refund != 0 {
		t.Errorf("parent refund=%d gross=%d: the child's refund must be in the parent's refund and out of its charge", rootTx.Refund, rootTx.Gross)
	}
	proc, err := st.ReadProcess(ctx, p.ID)
	if err != nil {
		t.Fatalf("ReadProcess: %v", err)
	}
	if proc.Status != kernel.ProcessClosed {
		t.Errorf("process status %q, want closed once nothing is outstanding", proc.Status)
	}
	u, _ := st.ReadUser(ctx, caller.ID)
	if u.Available != parentPrice || u.Locked != 0 {
		t.Errorf("caller wallet: available=%d locked=%d, want %d and 0", u.Available, u.Locked, parentPrice)
	}
	_ = child
}

// TestStepCompleteRemoteProxyTimeoutLeavesStepRunning verifies Fix 3B: CompleteStep does not
// call ResetStepAndRepark on ErrTimeout, leaving the step running for RetryPendingRemoteDispatches.
func TestStepCompleteRemoteProxyTimeoutLeavesStepRunning(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithHTTP(st, &fakeFederationHTTP{receiptJSON: ""})
	ctx := context.Background()

	owner := setupUser(t, st, "rp-running-owner", 0)
	remoteAction := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID,
		Name: "rp-running-action", Kind: kernel.KindRemoteProxy,
		Active: true, Visibility: kernel.VisibilityPublic, Price: 0,
		Source:    "https://remote.example.com/v1/federation/call?action=@owner/rp-running-action&counterparty=us",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, remoteAction); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	caller := setupUser(t, st, "rp-running-caller", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, remoteAction.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	_, _ = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))

	// Step must remain running — not reset to waiting — so retry can find the pending trace.
	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepRunning {
		t.Errorf("step.status after ErrTimeout: got %s, want running", got.Status)
	}
	// Completion trace must not be deleted (retry needs idempotency state).
	if got.CompletionTraceID == nil {
		t.Error("step.completion_trace_id must not be nil after ErrTimeout")
	}
	if _, err := st.ReadTrace(ctx, *got.CompletionTraceID); err != nil {
		t.Errorf("completion trace must still exist after ErrTimeout: %v", err)
	}
}

// TestStepCompleteSuspendedCallerRejectedBeforeMutation verifies that a suspended
// required_caller_user_id is rejected before BeginStepCall mutates state.
func TestStepCompleteSuspendedCallerRejectedBeforeMutation(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "susp-owner", 500)
	caller := setupUser(t, st, "susp-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "susp-action", "", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)
	trID := tr.ID

	step, err := k.CreateStep(ctx, trID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	// Suspend the caller before they complete the step.
	if err := st.SuspendUser(ctx, caller.ID); err != nil {
		t.Fatalf("SuspendUser: %v", err)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if !errors.Is(err, kernel.ErrUnauthenticated) {
		t.Errorf("expected ErrUnauthenticated for suspended caller, got %v", err)
	}

	// Step must still be waiting — no state mutation occurred.
	got, _ := st.ReadStep(ctx, step.ID)
	if got.Status != kernel.StepWaiting {
		t.Errorf("step.status after suspended caller: got %s, want waiting", got.Status)
	}
}

// CompleteStep's outcome contract (§10): callers must be able to tell "someone else claimed it"
// from "my resumed call failed" from "nothing settled" WITHOUT re-reading the step's status, which
// is racy and cannot see an in-flight dispatch. These assertions pin that contract.
func TestCompleteStepClaimFailureIsMarkedAndStillInvalidState(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "claim-owner", 500)
	caller := setupUser(t, st, "claim-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "claim-action", "", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)

	step, err := k.CreateStep(ctx, tr.ID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if _, err := k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("first completion: %v", err)
	}

	// The second attempt never takes the step.
	reply, err := k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected the second completion to fail")
	}
	if reply != nil {
		t.Errorf("a completion that never claimed the step must return a nil reply, got %+v", reply)
	}
	if !errors.Is(err, kernel.ErrStepNotClaimed) {
		t.Errorf("expected ErrStepNotClaimed, got %v", err)
	}
	// The marker must not change what crosses a process or kernel boundary.
	if code := kernel.KernelErrorCode(err); code != kernel.ErrInvalidState.Code {
		t.Errorf("code = %q, want %q", code, kernel.ErrInvalidState.Code)
	}
	if status := kernel.HTTPStatus(err); status != kernel.ErrInvalidState.HTTP {
		t.Errorf("status = %d, want %d", status, kernel.ErrInvalidState.HTTP)
	}
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Error("the outer error must still match ErrInvalidState")
	}
}

// A rejection before anything settles is NOT a claim race: conflating the two would make a gate
// silently swallow a genuine error as "someone else won".
func TestCompleteStepRejectionIsNotAClaimFailure(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "rej-owner", 500)
	caller := setupUser(t, st, "rej-caller", 0)
	stranger := setupUser(t, st, "rej-stranger", 0)
	action := setupWasmAction(t, st, owner.ID, "rej-action", "", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)

	step, err := k.CreateStep(ctx, tr.ID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	reply, err := k.CompleteStep(ctx, stranger.ID, step.ID, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected the wrong caller to be refused")
	}
	if reply != nil {
		t.Errorf("nothing settled, so the reply must be nil, got %+v", reply)
	}
	if errors.Is(err, kernel.ErrStepNotClaimed) {
		t.Error("a wrong-caller rejection is not a claim race and must not carry ErrStepNotClaimed")
	}
}

// Scenario (review finding 4): BeginStepCall returns ErrInvalidState for three distinct
// conditions, one of which — "step park invariant violated: parent trace locked < step price" —
// is a LEDGER CORRUPTION, not a claim race. Blanket-marking the whole code as ErrStepNotClaimed
// makes a gate report that corruption as a normal lost race and drop the contribution silently.
// The marker must be attached only where a claim genuinely lost.
func TestParkInvariantViolationIsNotAClaimFailure(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithScripts(st, &fakeScriptExec{result: `{"ok":true}`})
	ctx := context.Background()

	owner := setupUser(t, st, "inv-owner", 500)
	caller := setupUser(t, st, "inv-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "inv-action", "", 10)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)

	db, ok := st.(*store.DB)
	if !ok {
		t.Skip("needs the concrete store to break the invariant")
	}
	if err := db.ExecForTest(ctx, `UPDATE traces SET available=100 WHERE id=?`, tr.ID); err != nil {
		t.Fatalf("fund trace: %v", err)
	}
	step, err := k.CreateStep(ctx, tr.ID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	// Break the parked-funds invariant behind the kernel's back: the step is still waiting, but
	// its parent trace no longer holds the locked price.
	_ = ok
	if err := db.ExecForTest(ctx, `UPDATE traces SET locked=0 WHERE id=?`, tr.ID); err != nil {
		t.Fatalf("corrupt trace locked: %v", err)
	}

	_, err = k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected the park-invariant violation to fail the completion")
	}
	if errors.Is(err, kernel.ErrStepNotClaimed) {
		t.Errorf("a park-invariant violation is corruption, not a lost claim: %v", err)
	}
}

// failReadTraceStore fails ReadTrace once armed. Arming it from inside script execution puts the
// failure exactly where the real one occurs: after the action ran, at the post-execution taxable
// read — the point where Call settles a failure and used to discard the receipt.
type failReadTraceStore struct {
	kernel.Store
	armed bool
}

func (s *failReadTraceStore) ReadTrace(ctx context.Context, id string) (*kernel.Trace, error) {
	if s.armed {
		return nil, errors.New("injected post-execution read failure")
	}
	return s.Store.ReadTrace(ctx, id)
}

// armingScriptExec runs the script, then arms the store so the NEXT ReadTrace fails.
type armingScriptExec struct {
	store  *failReadTraceStore
	result string
}

func (a *armingScriptExec) Compile(_ context.Context, source []byte) ([]byte, string, error) {
	return source, "fakehash", nil
}

func (a *armingScriptExec) Execute(_ context.Context, _ []byte, _ []byte, _ kernel.HostFunctions) ([]byte, error) {
	a.store.armed = true
	return []byte(a.result), nil
}

// Scenario (review finding 1, root cause): Call's post-execution ReadTrace failure settles a
// FAILURE transaction — charging the caller and completing any idempotency record — and used to
// return a nil reply. Everything built on "non-nil reply means a transaction committed" was
// therefore wrong on this path: the federation handler deleted a record the kernel had already
// completed, leaving a charged peer unable to retrieve its outcome. The contract must hold here.
func TestCallReturnsTheCommittedReplyWhenPostExecutionReadFails(t *testing.T) {
	raw := newTestStore(t)
	st := &failReadTraceStore{Store: raw}
	exec := &armingScriptExec{store: st, result: `{"ok":true}`}
	k := newTestKernelWithScripts(st, exec)
	ctx := context.Background()

	owner := setupUser(t, st, "fail-owner", 500)
	caller := setupUser(t, st, "fail-caller", 500)
	action := setupWasmAction(t, st, owner.ID, "fail-action", "", 10)

	reply, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: "fail-owner/fail-action", Args: map[string]any{}})
	if err == nil {
		t.Fatal("expected the injected read failure to surface")
	}
	if reply == nil {
		t.Fatal("a settled failure must return its committed transaction, not nil")
	}
	if reply.TxID == "" || reply.ReceiptID == "" {
		t.Errorf("expected tx and receipt ids on the committed reply, got %+v", reply)
	}
	// The transaction really is committed and charged — this is why the reply must carry it.
	st.armed = false
	txs, listErr := raw.ListTransactions(ctx, kernel.TxFilter{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(txs) != 1 || txs[0].ID != reply.TxID || txs[0].Status != kernel.TxFailure {
		t.Errorf("expected exactly the reported failure transaction, got %+v", txs)
	}
	_ = action
}

// Scenario (review finding 3): ErrTimeout reaches completeStep from two unrelated places — a WASM
// execution timeout, which settles and CHARGES like any other failure, and a parked remote
// dispatch, which commits nothing. Classifying on the error before the reply conflated them, so a
// settled, charged completion was reported as "nothing happened yet": its transaction ids were
// lost, and the federation handler left an already-completed idempotency record pending.
func TestCompleteStepReportsACommittedWasmTimeout(t *testing.T) {
	st := newTestStore(t)
	// Must wrap context.DeadlineExceeded: that is what executeScript classifies as ErrTimeout
	// (call.go), and a bare ErrTimeout would be re-classified as ErrExecutionFailed and never
	// reach the branch under test.
	k := newTestKernelWithScripts(st, &fakeScriptExec{err: fmt.Errorf("run: %w", context.DeadlineExceeded)})
	ctx := context.Background()

	owner := setupUser(t, st, "to-owner", 500)
	caller := setupUser(t, st, "to-caller", 0)
	action := setupWasmAction(t, st, owner.ID, "to-action", "", 0)
	_, tr := setupOrphanTrace(t, st, owner.ID, owner.ID, owner.ID)

	step, err := k.CreateStep(ctx, tr.ID, action.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	reply, err := k.CompleteStep(ctx, caller.ID, step.ID, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected the timeout to surface")
	}
	if reply == nil {
		t.Fatal("a WASM timeout commits a failure transaction, so its reply must be returned")
	}
	if reply.TxID == "" {
		t.Errorf("expected the committed transaction's id, got %+v", reply)
	}
	// It is genuinely settled: the step is done, not left running for a retry that will never come.
	done, err := k.ReadStep(ctx, owner.ID, step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != kernel.StepDone {
		t.Errorf("step status = %s, want done", done.Status)
	}
}

// TestCreateStepHealsLegacyProxy: parking money is a funding boundary, so a proxy imported before
// the seller's price was stored heals BEFORE its price is parked (§16). Otherwise the step parks a
// frozen total and its completion dispatches the local total as the seller's price, which makes the
// peer's valid receipt fail the charge==mp check and quarantine.
func TestCreateStepHealsLegacyProxy(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	m := kernel.ActionManifest{
		ActionID: "ra-step-legacy", OwnerID: "remote-bob", OwnerHandle: "bob", Name: "greet",
		RemoteBPS: 500, Description: "greet", Kind: kernel.KindHTTP, Price: 100,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	m.Signature, _ = testNet.SignManifest(priv, &m)
	k := newTestKernelWithHTTP(st, &fakeFederationHTTP{resolveManifest: &m})

	a, err := k.ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil {
		t.Fatal(err)
	}
	// Rewind to the pre-041 shape: the total is stored, the seller's price is not.
	a.BasePrice = nil
	if err := st.UpdateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	owner := setupUser(t, st, "step-legacy-owner", 1000)
	caller := setupUser(t, st, "step-legacy-caller", 0)
	local := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "host", Kind: kernel.KindHTTP,
		Active: true, Visibility: kernel.VisibilityPublic, Description: "host",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, local); err != nil {
		t.Fatal(err)
	}
	p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: owner.ID, Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
	root := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ActionOwnerID: owner.ID, CreatedAt: time.Now().UTC()}
	if err := st.BeginRun(ctx, p, root, owner.ID, 500, 0, 0); err != nil {
		t.Fatal(err)
	}

	step, err := k.CreateStep(ctx, root.ID, a.ID, nil, kernel.RequiredCaller{UserID: caller.ID})
	if err != nil {
		t.Fatalf("CreateStep against a legacy proxy: %v", err)
	}
	healed, err := st.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if healed.BasePrice == nil || *healed.BasePrice != 100 {
		t.Fatalf("the row must heal before parking, got base price %v", healed.BasePrice)
	}
	if step.Price != 111 { // sr=105, import 500bps → 111
		t.Errorf("parked price = %d, want 111", step.Price)
	}
	if step.ImportBPS == nil || *step.ImportBPS != 500 {
		t.Errorf("step must freeze the fee it was funded under, got %v", step.ImportBPS)
	}
}
