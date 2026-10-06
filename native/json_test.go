// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// person is a schema for {name: string}, required.
func person() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []any{"name"}}
}

// Called directly, the handler refuses what the kernel's input check would refuse first on a real
// call (as ErrSchemaViolation): a missing schema or malformed messages.
func TestExecuteJSON_Refusals(t *testing.T) {
	if _, err := executeJSON(context.Background(), map[string]any{"messages": hi, "schema": person()}, nil); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("an unbound json native must answer ErrInvalidState, got %v", err)
	}
	for name, args := range map[string]map[string]any{
		"no schema":   {"messages": hi},
		"no messages": {"schema": person()},
		"no role":     {"messages": []any{map[string]any{"content": "hi"}}, "schema": person()},
	} {
		if _, err := executeJSON(context.Background(), args, &stubModel{}); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("%s: got %v, want ErrInvalidInput", name, err)
		}
	}
	if _, err := executeJSON(context.Background(), map[string]any{"messages": hi, "schema": map[string]any{"anyOf": []any{}}}, &stubModel{}); !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Errorf("a schema outside the subset: got %v, want ErrSchemaViolation", err)
	}
}

// The model is asked for the canonical schema, and its reply is returned as value only if the
// schema admits it.
func TestExecuteJSON_Value(t *testing.T) {
	m := &stubModel{value: map[string]any{"name": "Ada"}}
	result, err := executeJSON(context.Background(), map[string]any{"messages": hi, "schema": person()}, m)
	if err != nil {
		t.Fatal(err)
	}
	if v := result["value"].(map[string]any); v["name"] != "Ada" {
		t.Errorf("value = %v", result["value"])
	}
	if m.schema["additionalProperties"] != false || len(m.captured) != 2 {
		t.Errorf("the model was asked %v under %v", m.captured, m.schema)
	}
	m.value = map[string]any{"wrong": 1}
	if _, err := executeJSON(context.Background(), map[string]any{"messages": hi, "schema": person()}, m); !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("a reply the schema refuses: got %v, want ErrExecutionFailed", err)
	}
	m.err = errors.New("down")
	if _, err := executeJSON(context.Background(), map[string]any{"messages": hi, "schema": person()}, m); !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("a failed model call: got %v, want ErrExecutionFailed", err)
	}
}

// On a real call the kernel's input check refuses a request missing messages or schema before the
// handler runs, as it does for every action.
func TestJSONInputSchemaRequiresBoth(t *testing.T) {
	in := JSON("", nil).InputSchema
	if err := kernel.ValidateInput(in, map[string]any{"messages": hi, "schema": person()}); err != nil {
		t.Errorf("a complete request was refused: %v", err)
	}
	for _, args := range []map[string]any{{"messages": hi}, {"schema": person()}} {
		if err := kernel.ValidateInput(in, args); !errors.Is(err, kernel.ErrSchemaViolation) {
			t.Errorf("%v: got %v, want ErrSchemaViolation", args, err)
		}
	}
}
