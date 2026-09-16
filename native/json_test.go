// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

type stubJSONChatter struct {
	value any
	err   error
}

func (s *stubJSONChatter) ChatJSON(_ context.Context, _ []kernel.ChatMessage, _ map[string]any) (any, error) {
	return s.value, s.err
}

var validJSONArgs = map[string]any{
	"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	"output_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	},
}

func TestExecuteJSON_NilChatter(t *testing.T) {
	_, err := executeJSON(context.Background(), validJSONArgs, nil)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState, got %v", err)
	}
}

func TestExecuteJSON_MissingMessages(t *testing.T) {
	_, err := executeJSON(context.Background(), map[string]any{
		"output_schema": map[string]any{"type": "object"},
	}, &stubJSONChatter{})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteJSON_MissingOutputSchema(t *testing.T) {
	_, err := executeJSON(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, &stubJSONChatter{})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteJSON_UnsupportedSchema(t *testing.T) {
	_, err := executeJSON(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"output_schema": map[string]any{
			"anyOf": []any{},
		},
	}, &stubJSONChatter{})
	if !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Errorf("expected ErrSchemaViolation, got %v", err)
	}
}

func TestExecuteJSON_ModelOutputFailsValidation(t *testing.T) {
	chatter := &stubJSONChatter{value: map[string]any{"wrong_key": "value"}}
	_, err := executeJSON(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"output_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []any{"name"},
		},
	}, chatter)
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got %v", err)
	}
}

func TestExecuteJSON_Success(t *testing.T) {
	chatter := &stubJSONChatter{value: map[string]any{"name": "alice"}}
	result, err := executeJSON(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"output_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []any{"name"},
		},
	}, chatter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, ok := result["value"].(map[string]any)
	if !ok || v["name"] != "alice" {
		t.Errorf("unexpected result: %v", result)
	}
}
