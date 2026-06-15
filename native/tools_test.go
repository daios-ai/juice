package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

type stubToolChatter struct {
	calls []kernel.ToolCall
	msg   *kernel.ChatMessage
	err   error
}

func (s *stubToolChatter) ChatTools(_ context.Context, _ []kernel.ChatMessage, _ []kernel.ToolDefinition, _ string, _ int) ([]kernel.ToolCall, *kernel.ChatMessage, error) {
	return s.calls, s.msg, s.err
}

var validToolsArgs = map[string]any{
	"messages": []any{map[string]any{"role": "user", "content": "find something"}},
	"tools": []any{map[string]any{
		"action":      "@sys/lookup",
		"description": "search actions",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
	}},
}

func TestExecuteTools_NilChatter(t *testing.T) {
	_, err := executeTools(context.Background(), validToolsArgs, nil)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState, got %v", err)
	}
}

func TestExecuteTools_MissingMessages(t *testing.T) {
	_, err := executeTools(context.Background(), map[string]any{
		"tools": validToolsArgs["tools"],
	}, &stubToolChatter{})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteTools_MissingTools(t *testing.T) {
	_, err := executeTools(context.Background(), map[string]any{
		"messages": validToolsArgs["messages"],
	}, &stubToolChatter{})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteTools_UnsupportedToolSchema(t *testing.T) {
	_, err := executeTools(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"action":       "@sys/lookup",
			"description":  "search",
			"input_schema": map[string]any{"anyOf": []any{}},
		}},
	}, &stubToolChatter{})
	if !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Errorf("expected ErrSchemaViolation, got %v", err)
	}
}

func TestExecuteTools_UnknownToolReturned(t *testing.T) {
	chatter := &stubToolChatter{calls: []kernel.ToolCall{{Action: "@sys/unknown", Args: map[string]any{}}}}
	_, err := executeTools(context.Background(), validToolsArgs, chatter)
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed for unknown tool, got %v", err)
	}
}

func TestExecuteTools_BadArgsReturned(t *testing.T) {
	chatter := &stubToolChatter{calls: []kernel.ToolCall{{
		Action: "@sys/lookup",
		Args:   map[string]any{"query": 123}, // should be string
	}}}
	_, err := executeTools(context.Background(), validToolsArgs, chatter)
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed for bad args, got %v", err)
	}
}

func TestExecuteTools_RequiredNoCallsProduced(t *testing.T) {
	chatter := &stubToolChatter{calls: nil}
	args := map[string]any{
		"messages":    validToolsArgs["messages"],
		"tools":       validToolsArgs["tools"],
		"tool_choice": "required",
	}
	_, err := executeTools(context.Background(), args, chatter)
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed for required+no calls, got %v", err)
	}
}

func TestExecuteTools_Success(t *testing.T) {
	chatter := &stubToolChatter{calls: []kernel.ToolCall{{
		Action: "@sys/lookup",
		Args:   map[string]any{"query": "test"},
	}}}
	result, err := executeTools(context.Background(), validToolsArgs, chatter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tcs, ok := result["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %v", result)
	}
	tc := tcs[0].(map[string]any)
	if tc["action"] != "@sys/lookup" {
		t.Errorf("unexpected action: %v", tc["action"])
	}
}

func TestExecuteTools_MessageIncluded(t *testing.T) {
	msg := &kernel.ChatMessage{Role: "assistant", Content: "I'll look that up"}
	chatter := &stubToolChatter{
		calls: []kernel.ToolCall{{Action: "@sys/lookup", Args: map[string]any{"query": "x"}}},
		msg:   msg,
	}
	result, err := executeTools(context.Background(), validToolsArgs, chatter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := result["message"].(map[string]any)
	if !ok || m["content"] != "I'll look that up" {
		t.Errorf("expected message in result, got %v", result["message"])
	}
}
