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

	t.Run("allOf in typed node rejected", func(t *testing.T) {
		s := map[string]any{"type": "object", "allOf": []any{}}
		if err := ValidateSchema(s); err == nil {
			t.Error("expected error for allOf in typed node")
		}
	})

	t.Run("$ref in typed node rejected", func(t *testing.T) {
		s := map[string]any{"type": "string", "$ref": "#/definitions/foo"}
		if err := ValidateSchema(s); err == nil {
			t.Error("expected error for $ref in typed node")
		}
	})

	t.Run("patternProperties in typed node rejected", func(t *testing.T) {
		s := map[string]any{"type": "object", "patternProperties": map[string]any{}}
		if err := ValidateSchema(s); err == nil {
			t.Error("expected error for patternProperties in typed node")
		}
	})

	t.Run("unknown keyword in primitive rejected", func(t *testing.T) {
		s := map[string]any{"type": "integer", "minimum": float64(0)}
		if err := ValidateSchema(s); err == nil {
			t.Error("expected error for minimum keyword on integer")
		}
	})

	t.Run("description allowed on all types", func(t *testing.T) {
		schemas := []map[string]any{
			{"type": "string", "description": "a string field"},
			{"type": "integer", "description": "an integer field"},
			{"type": "object", "description": "an object", "properties": map[string]any{
				"x": map[string]any{"type": "string", "description": "nested"},
			}},
		}
		for _, s := range schemas {
			if err := ValidateSchema(s); err != nil {
				t.Errorf("schema with description should be valid, got: %v", err)
			}
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
