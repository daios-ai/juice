// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

type stubJSONChatter struct {
	value  any
	err    error
	schema map[string]any // what the model was asked for
}

func (s *stubJSONChatter) ChatJSON(_ context.Context, _ []kernel.ChatMessage, schema map[string]any) (any, error) {
	s.schema = schema
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

// The caller's schema is read in the canonical form: the model is asked for that form, and the
// answer is checked against what the caller's spelling means.
func TestExecuteJSON_ReadsTheCanonicalForm(t *testing.T) {
	chatter := &stubJSONChatter{value: map[string]any{"name": nil}}
	result, err := executeJSON(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"output_schema": map[string]any{
			"properties": map[string]any{"name": map[string]any{"type": "string", "nullable": true}},
			"required":   []any{"name"},
		},
	}, chatter)
	if err != nil {
		t.Fatalf("a nullable field must admit null: %v", err)
	}
	if v, _ := result["value"].(map[string]any); v == nil {
		t.Errorf("unexpected result: %v", result)
	}
	name, _ := chatter.schema["properties"].(map[string]any)["name"].(map[string]any)
	if chatter.schema["additionalProperties"] != false || len(name["type"].([]any)) != 2 {
		t.Errorf("the model must be asked for the canonical form, got %v", chatter.schema)
	}
}
