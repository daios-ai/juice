package native

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

type stubNotifier struct {
	called  bool
	lastTo  string
	lastSub string
	err     error
}

func (s *stubNotifier) Notify(_ context.Context, to, subject, _ string) error {
	s.called = true
	s.lastTo = to
	s.lastSub = subject
	return s.err
}

func seedUserWithBalance(t *testing.T, st kernel.Store, handle string, balance int64) *kernel.User {
	t.Helper()
	hash, _ := kernel.HashPassword("pw")
	u := &kernel.User{
		ID: uuid.New().String(), Handle: handle, Email: handle + "@test",
		PasswordHash: hash, Available: balance,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("seedUserWithBalance %s: %v", handle, err)
	}
	return u
}

func TestExecuteMessage_MissingTo(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeMessage(context.Background(), map[string]any{
		"message":     "hello",
		"next_action": "@alice/act",
	}, "c", "o", "p", "", k, nil)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing to, got %v", err)
	}
}

func TestExecuteMessage_MissingMessage(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeMessage(context.Background(), map[string]any{
		"to":          "@alice",
		"next_action": "@alice/act",
	}, "c", "o", "p", "", k, nil)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing message, got %v", err)
	}
}

func TestExecuteMessage_MissingNextAction(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeMessage(context.Background(), map[string]any{
		"to":      "@alice",
		"message": "hello",
	}, "c", "o", "p", "", k, nil)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing next_action, got %v", err)
	}
}

func TestExecuteMessage_UnknownRecipient(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeMessage(context.Background(), map[string]any{
		"to":          "@nobody",
		"message":     "hello",
		"next_action": "@alice/act",
	}, "c", "o", "p", "", k, nil)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for unknown recipient, got %v", err)
	}
}

func TestExecuteMessage_InvalidNextAction(t *testing.T) {
	k, st := newLookupTestKernel(t)
	recipient := seedOwner(t, st, "@recipient")

	_, err := executeMessage(context.Background(), map[string]any{
		"to":          recipient.Handle,
		"message":     "hello",
		"next_action": "not-a-ref",
	}, "c", "o", "p", "", k, nil)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for invalid next_action ref, got %v", err)
	}
}

func TestExecuteMessage_CreatesStepAndDeliversNotification(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	caller := seedUserWithBalance(t, st, "@caller", 1000)
	recipient := seedOwner(t, st, "@recipient")
	action := seedAction(t, st, caller.ID, "greet", "greets someone")

	p, root, err := k.StartProcess(ctx, caller.ID, caller.ID, 100)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	notifier := &stubNotifier{}
	result, err := executeMessage(ctx, map[string]any{
		"to":          "@recipient",
		"message":     "please review",
		"next_action": "@caller/greet",
		"subject":     "Review request",
	}, caller.ID, caller.ID, p.ID, root.ID, k, notifier)
	if err != nil {
		t.Fatalf("executeMessage: %v", err)
	}

	stepID, ok := result["step_id"].(string)
	if !ok || stepID == "" {
		t.Fatalf("expected step_id string, got %v", result["step_id"])
	}
	delivered, ok := result["delivered"].(bool)
	if !ok || !delivered {
		t.Errorf("expected delivered=true, got %v", result["delivered"])
	}

	// Verify the step has the correct required_caller_user_id.
	step, err := k.ReadStep(ctx, caller.ID, stepID)
	if err != nil {
		t.Fatalf("ReadStep: %v", err)
	}
	if step.RequiredCallerUserID != recipient.ID {
		t.Errorf("expected required_caller_user_id=%s, got %s", recipient.ID, step.RequiredCallerUserID)
	}
	if step.NextActionID != action.ID {
		t.Errorf("expected next_action_id=%s, got %s", action.ID, step.NextActionID)
	}

	// Verify notification was sent to recipient's email.
	if notifier.lastTo != recipient.Email {
		t.Errorf("expected notification to %s, got %s", recipient.Email, notifier.lastTo)
	}
	if notifier.lastSub != "Review request" {
		t.Errorf("expected subject 'Review request', got %s", notifier.lastSub)
	}
}

func TestExecuteMessage_NilNotifier_StillCreatesStep(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	caller := seedUserWithBalance(t, st, "@caller2", 1000)
	seedOwner(t, st, "@recipient2")
	seedAction(t, st, caller.ID, "act", "does something")

	p, root, err := k.StartProcess(ctx, caller.ID, caller.ID, 100)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	result, err := executeMessage(ctx, map[string]any{
		"to":          "@recipient2",
		"message":     "please review",
		"next_action": "@caller2/act",
	}, caller.ID, caller.ID, p.ID, root.ID, k, nil)
	if err != nil {
		t.Fatalf("executeMessage: %v", err)
	}
	if result["step_id"] == "" {
		t.Error("expected step_id even with nil notifier")
	}
	if result["delivered"] != false {
		t.Errorf("expected delivered=false with nil notifier, got %v", result["delivered"])
	}
}
