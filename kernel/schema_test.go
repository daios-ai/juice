// SPDX-License-Identifier: AGPL-3.0-only

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

	t.Run("object with non-map properties rejected", func(t *testing.T) {
		s := map[string]any{"type": "object", "properties": []any{}}
		if err := ValidateSchema(s); err == nil {
			t.Error("expected error for non-map properties")
		}
	})

	t.Run("object with non-array required rejected", func(t *testing.T) {
		s := map[string]any{"type": "object", "required": "x"}
		if err := ValidateSchema(s); err == nil {
			t.Error("expected error for non-array required")
		}
	})

	t.Run("object with array required valid", func(t *testing.T) {
		s := map[string]any{
			"type":       "object",
			"required":   []any{"x"},
			"properties": map[string]any{"x": map[string]any{"type": "string", "description": "x"}},
		}
		if err := ValidateSchema(s); err != nil {
			t.Errorf("expected valid schema, got: %v", err)
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

func TestValidateSchemaDescriptions(t *testing.T) {
	t.Run("no properties passes", func(t *testing.T) {
		s := map[string]any{"type": "object"}
		if err := validateSchemaDescriptions(s, "#"); err != nil {
			t.Errorf("empty properties: unexpected error: %v", err)
		}
	})

	t.Run("property with description passes", func(t *testing.T) {
		s := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "the name"},
			},
		}
		if err := validateSchemaDescriptions(s, "#"); err != nil {
			t.Errorf("described property: unexpected error: %v", err)
		}
	})

	t.Run("property without description fails", func(t *testing.T) {
		s := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
		}
		if err := validateSchemaDescriptions(s, "#"); err == nil {
			t.Error("expected error for property missing description")
		}
	})

	t.Run("nested property without description fails", func(t *testing.T) {
		s := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"outer": map[string]any{
					"type":        "object",
					"description": "outer field",
					"properties": map[string]any{
						"inner": map[string]any{"type": "string"},
					},
				},
			},
		}
		if err := validateSchemaDescriptions(s, "#"); err == nil {
			t.Error("expected error for nested property missing description")
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

	t.Run("schema without type is unconstrained and accepts nil", func(t *testing.T) {
		schema := map[string]any{"description": "x"}
		if err := ValidateInput(schema, nil); err != nil {
			t.Errorf("schema without type should accept nil: %v", err)
		}
		if err := ValidateInput(schema, "anything"); err != nil {
			t.Errorf("schema without type should accept any value: %v", err)
		}
	})

	t.Run("undeclared key rejected when properties declared", func(t *testing.T) {
		s := map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
		}
		if err := ValidateInput(s, map[string]any{"x": "a", "y": float64(1)}); err == nil {
			t.Error("expected error for undeclared key y")
		}
		if err := ValidateInput(s, map[string]any{"x": "a"}); err != nil {
			t.Errorf("declared key should be accepted: %v", err)
		}
	})

	t.Run("no properties clause accepts any keys", func(t *testing.T) {
		s := map[string]any{"type": "object"}
		if err := ValidateInput(s, map[string]any{"anything": true, "extra": 42}); err != nil {
			t.Errorf("object without properties should accept any keys: %v", err)
		}
	})

	t.Run("required field absent from properties is still enforced", func(t *testing.T) {
		// JSON Schema: required is independent of properties — a required name not in
		// properties must still be present in the input object.
		s := map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
			"required":   []any{"x", "y"}, // "y" is not in properties
		}
		if err := ValidateInput(s, map[string]any{"x": "val"}); err == nil {
			t.Error("expected error: required field 'y' missing from object and not in properties")
		}
		if err := ValidateInput(s, map[string]any{"x": "val", "y": "present"}); err != nil {
			t.Errorf("unexpected error when all required fields present: %v", err)
		}
	})

	t.Run("enum is checked after type so wrong-typed values are rejected", func(t *testing.T) {
		s := map[string]any{"type": "string", "enum": []any{"1", "2"}}
		// Numeric 1 renders as "1" but must not satisfy a string enum.
		if err := ValidateInput(s, float64(1)); err == nil {
			t.Error("expected error: numeric 1 must not satisfy {type:string, enum:[\"1\"]}")
		}
		// Matched-type membership still passes; non-member of the right type is rejected.
		if err := ValidateInput(s, "1"); err != nil {
			t.Errorf("string \"1\" should satisfy the enum: %v", err)
		}
		if err := ValidateInput(s, "3"); err == nil {
			t.Error("expected error: \"3\" is not in the enum")
		}
		// Integer enum still works for matched numeric input.
		si := map[string]any{"type": "integer", "enum": []any{float64(1), float64(2)}}
		if err := ValidateInput(si, float64(2)); err != nil {
			t.Errorf("integer 2 should satisfy the enum: %v", err)
		}
	})
}
