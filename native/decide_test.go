package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

type stubDecideChatter struct {
	call *kernel.ToolCall
	msg  *kernel.ChatMessage
	err  error
}

func (s *stubDecideChatter) ChatDecide(_ context.Context, _ []kernel.DecideMessage, _ []kernel.ToolDefinition) (*kernel.ToolCall, *kernel.ChatMessage, error) {
	return s.call, s.msg, s.err
}

var (
	lookupOK = func(_ context.Context, _, _ string, _ string) (*kernel.Action, error) {
		return &kernel.Action{
			Description: "search actions",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
				"required": []any{"query"},
			},
			Price: 0,
		}, nil
	}

	// resolveStub stands in for the kernel-qualified resolve path; bare-ref decide tests never call it.
	resolveStub = func(_ context.Context, _ string) (*kernel.Action, error) {
		return nil, kernel.ErrNotFound.Wrap("no remote resolve in test")
	}

	validDecideArgs = map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "find something"}},
		"actions":  []any{"sys/lookup"},
	}
)

func TestExecuteDecide_NilChatter(t *testing.T) {
	_, err := executeDecide(context.Background(), validDecideArgs, nil, lookupOK, resolveStub, "")
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState, got %v", err)
	}
}

func TestExecuteDecide_MissingMessages(t *testing.T) {
	_, err := executeDecide(context.Background(), map[string]any{
		"actions": validDecideArgs["actions"],
	}, &stubDecideChatter{}, lookupOK, resolveStub, "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteDecide_MissingActions(t *testing.T) {
	_, err := executeDecide(context.Background(), map[string]any{
		"messages": validDecideArgs["messages"],
	}, &stubDecideChatter{}, lookupOK, resolveStub, "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteDecide_InvalidActionRef(t *testing.T) {
	_, err := executeDecide(context.Background(), map[string]any{
		"messages": validDecideArgs["messages"],
		"actions":  []any{"noslash"},
	}, &stubDecideChatter{}, lookupOK, resolveStub, "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteDecide_UnknownAction(t *testing.T) {
	lookupErr := func(_ context.Context, _, _ string, _ string) (*kernel.Action, error) {
		return nil, kernel.ErrNotFound.Wrap("action not found")
	}
	_, err := executeDecide(context.Background(), validDecideArgs, &stubDecideChatter{}, lookupErr, resolveStub, "")
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestExecuteDecide_NoSelection(t *testing.T) {
	_, err := executeDecide(context.Background(), validDecideArgs, &stubDecideChatter{call: nil}, lookupOK, resolveStub, "")
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got %v", err)
	}
}

func TestExecuteDecide_UnknownActionReturned(t *testing.T) {
	chatter := &stubDecideChatter{call: &kernel.ToolCall{Action: "sys/unknown", Args: map[string]any{"query": "x"}}}
	_, err := executeDecide(context.Background(), validDecideArgs, chatter, lookupOK, resolveStub, "")
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got %v", err)
	}
}

func TestExecuteDecide_BadArgsReturned(t *testing.T) {
	chatter := &stubDecideChatter{call: &kernel.ToolCall{
		Action: "sys/lookup",
		Args:   map[string]any{"query": 123}, // should be string
	}}
	_, err := executeDecide(context.Background(), validDecideArgs, chatter, lookupOK, resolveStub, "")
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got %v", err)
	}
}

func TestExecuteDecide_Success(t *testing.T) {
	chatter := &stubDecideChatter{call: &kernel.ToolCall{
		Action: "sys/lookup",
		Args:   map[string]any{"query": "test"},
	}}
	result, err := executeDecide(context.Background(), validDecideArgs, chatter, lookupOK, resolveStub, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["action"] != "sys/lookup" {
		t.Errorf("unexpected action: %v", result["action"])
	}
	args, ok := result["args"].(map[string]any)
	if !ok || args["query"] != "test" {
		t.Errorf("unexpected args: %v", result["args"])
	}
	if _, hasMsg := result["message"]; hasMsg {
		t.Errorf("expected no message in result")
	}
}

func TestExecuteDecide_MessageIncluded(t *testing.T) {
	chatter := &stubDecideChatter{
		call: &kernel.ToolCall{Action: "sys/lookup", Args: map[string]any{"query": "x"}},
		msg:  &kernel.ChatMessage{Role: "assistant", Content: "I'll look that up"},
	}
	result, err := executeDecide(context.Background(), validDecideArgs, chatter, lookupOK, resolveStub, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := result["message"].(map[string]any)
	if !ok || m["content"] != "I'll look that up" {
		t.Errorf("expected message in result, got %v", result["message"])
	}
}

func TestExecuteDecide_ToolTurn(t *testing.T) {
	args := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "find something"},
			map[string]any{
				"role": "assistant",
				"tool": map[string]any{"action": "sys/lookup", "args": map[string]any{"query": "x"}},
			},
			map[string]any{
				"role": "tool",
				"tool": map[string]any{"action": "sys/lookup", "result": map[string]any{"results": []any{}}},
			},
		},
		"actions": []any{"sys/lookup"},
	}
	chatter := &stubDecideChatter{call: &kernel.ToolCall{
		Action: "sys/lookup",
		Args:   map[string]any{"query": "refined"},
	}}
	result, err := executeDecide(context.Background(), args, chatter, lookupOK, resolveStub, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["action"] != "sys/lookup" {
		t.Errorf("unexpected action: %v", result["action"])
	}
}
