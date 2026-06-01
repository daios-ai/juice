package kernel

import "testing"

func TestValidateSchema(t *testing.T) {
	t.Run("nil schema is rejected", func(t *testing.T) {
		if err := ValidateSchema(nil); err == nil {
			t.Error("expected error for nil schema")
		}
	})

	t.Run("object schema valid", func(t *testing.T) {
		s := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []any{"name"},
		}
		if err := ValidateSchema(s); err != nil {
			t.Error(err)
		}
	})

	t.Run("anyOf rejected", func(t *testing.T) {
		s := map[string]any{"anyOf": []any{}}
		if err := ValidateSchema(s); err == nil {
			t.Error("expected error for anyOf")
		}
	})
}

func TestValidateInput(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":  map[string]any{"type": "string"},
			"count": map[string]any{"type": "number"},
		},
		"required": []any{"name"},
	}

	t.Run("valid input", func(t *testing.T) {
		data := map[string]any{"name": "hello", "count": float64(3)}
		if err := ValidateInput(schema, data); err != nil {
			t.Error(err)
		}
	})

	t.Run("missing required field", func(t *testing.T) {
		data := map[string]any{"count": float64(3)}
		if err := ValidateInput(schema, data); err == nil {
			t.Error("expected error for missing required field")
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		data := map[string]any{"name": 42}
		if err := ValidateInput(schema, data); err == nil {
			t.Error("expected error for wrong type")
		}
	})

	t.Run("nil schema accepts anything", func(t *testing.T) {
		if err := ValidateInput(nil, map[string]any{"anything": true}); err != nil {
			t.Error(err)
		}
	})
}
