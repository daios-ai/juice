package native

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

func seedUserWithBalance(t *testing.T, st kernel.Store, handle string, balance int64) *kernel.User {
	t.Helper()
	hash, _ := kernel.HashPassword("pw")
	u := &kernel.User{
		ID: uuid.New().String(), Handle: handle,
		PasswordHash: hash, Available: balance,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("seedUserWithBalance %s: %v", handle, err)
	}
	return u
}

func seedSysWithSink(t *testing.T, st kernel.Store) *kernel.Action {
	t.Helper()
	sys := seedOwner(t, st, "@sys")
	return seedAction(t, st, sys.ID, "sink", "universal sink")
}

func TestExecuteMessage_MissingTo(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeMessage(context.Background(), map[string]any{
		"message": "hello",
	}, "o", "", k)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteMessage_MissingMessage(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeMessage(context.Background(), map[string]any{
		"to": "@alice",
	}, "o", "", k)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteMessage_UnknownRecipient(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeMessage(context.Background(), map[string]any{
		"to": "@nobody", "message": "hello",
	}, "o", "", k)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteMessage_CreatesStep(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	sink := seedSysWithSink(t, st)
	caller := seedUserWithBalance(t, st, "@caller", 1000)
	recipient := seedOwner(t, st, "@recipient")

	// Create process and root trace atomically via BeginRun (mirrors production).
	p := &kernel.Process{
		ID:          uuid.New().String(),
		OwnerUserID: caller.ID,
		Status:      kernel.ProcessOpen,
		CreatedAt:   time.Now().UTC(),
	}
	// The message action executes as @sys, so its trace's action owner — the step creator whose
	// visibility is checked at CreateStep (§10 binding rule) — is @sys, the sink's owner.
	rootTrace := &kernel.Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: sink.OwnerUserID,
		CreatedAt:     time.Now().UTC(),
	}
	if err := st.BeginRun(ctx, p, rootTrace, caller.ID, 0); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}

	result, err := executeMessage(ctx, map[string]any{
		"to": "@recipient", "message": "hello",
	}, caller.ID, rootTrace.ID, k)
	if err != nil {
		t.Fatalf("executeMessage: %v", err)
	}

	stepID, ok := result["step_id"].(string)
	if !ok || stepID == "" {
		t.Fatalf("expected step_id string, got %v", result["step_id"])
	}

	step, err := k.ReadStep(ctx, caller.ID, stepID)
	if err != nil {
		t.Fatalf("ReadStep: %v", err)
	}
	if step.RequiredCallerUserID != recipient.ID {
		t.Errorf("expected required_caller_user_id=%s, got %s", recipient.ID, step.RequiredCallerUserID)
	}
	var pa map[string]any
	if err := json.Unmarshal(step.PartialArgs, &pa); err != nil {
		t.Fatalf("unmarshal partial_args: %v", err)
	}
	if pa["message"] != "hello" {
		t.Errorf("expected partial_args.message=hello, got %v", pa["message"])
	}
}
