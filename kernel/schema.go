package kernel

import (
	"encoding/json"
	"fmt"
)

// ValidateSchema checks that schema is a supported JSON Schema subset.
// Supported: type, properties, required, items, enum, nullable.
// Returns ErrSchemaViolation if the schema form is unsupported.
func ValidateSchema(schema map[string]any) error {
	return validateSchemaNode(schema, "#", 0)
}

const maxSchemaDepth = 8

func validateSchemaNode(node map[string]any, path string, depth int) error {
	if depth > maxSchemaDepth {
		return ErrSchemaViolation.Wrapf("schema at %s exceeds maximum depth %d", path, maxSchemaDepth)
	}
	if len(node) == 0 {
		return nil // empty schema = accept any
	}

	t, hasType := node["type"]
	if !hasType {
		// anyOf/oneOf are not supported
		if _, has := node["anyOf"]; has {
			return ErrSchemaViolation.Wrapf("schema at %s: anyOf is not supported; use nullable:true instead", path)
		}
		if _, has := node["oneOf"]; has {
			return ErrSchemaViolation.Wrapf("schema at %s: oneOf is not supported", path)
		}
		return nil // no type constraint — treat as Any
	}

	switch typ := t.(type) {
	case string:
		return validateTypedNode(typ, node, path, depth)
	default:
		return ErrSchemaViolation.Wrapf("schema at %s: type must be a string", path)
	}
}

func validateTypedNode(typ string, node map[string]any, path string, depth int) error {
	switch typ {
	case "object":
		props, _ := node["properties"].(map[string]any)
		for name, raw := range props {
			child, ok := raw.(map[string]any)
			if !ok {
				return ErrSchemaViolation.Wrapf("schema at %s.properties.%s: must be an object", path, name)
			}
			if err := validateSchemaNode(child, path+"."+name, depth+1); err != nil {
				return err
			}
		}
	case "array":
		items, ok := node["items"]
		if !ok {
			return ErrSchemaViolation.Wrapf("schema at %s: array must have items", path)
		}
		child, ok := items.(map[string]any)
		if !ok {
			return ErrSchemaViolation.Wrapf("schema at %s.items: must be an object", path)
		}
		if err := validateSchemaNode(child, path+".items", depth+1); err != nil {
			return err
		}
	case "string", "integer", "number", "boolean":
		// primitives — valid
	default:
		return ErrSchemaViolation.Wrapf("schema at %s: unsupported type %q", path, typ)
	}
	return nil
}

// ValidateInput checks that data conforms to schema.
// Returns ErrSchemaViolation with a descriptive message on mismatch.
func ValidateInput(schema map[string]any, data any) error {
	return validateValue(schema, data, "#")
}

func validateValue(schema map[string]any, data any, path string) error {
	if len(schema) == 0 {
		return nil // empty schema accepts anything
	}

	nullable, _ := schema["nullable"].(bool)
	if data == nil {
		if nullable {
			return nil
		}
		return ErrSchemaViolation.Wrapf("field %s: must not be null", path)
	}

	t, _ := schema["type"].(string)

	// Enum check.
	if enums, ok := schema["enum"].([]any); ok {
		for _, e := range enums {
			if fmt.Sprintf("%v", e) == fmt.Sprintf("%v", data) {
				return nil
			}
		}
		return ErrSchemaViolation.Wrapf("field %s: value not in enum", path)
	}

	switch t {
	case "object":
		obj, ok := data.(map[string]any)
		if !ok {
			return ErrSchemaViolation.Wrapf("field %s: expected object, got %T", path, data)
		}
		props, _ := schema["properties"].(map[string]any)
		var required []string
		switch v := schema["required"].(type) {
		case []string:
			required = v
		case []any:
			for _, r := range v {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}
		reqSet := make(map[string]bool, len(required))
		for _, s := range required {
			reqSet[s] = true
		}
		for name, raw := range props {
			childSchema, _ := raw.(map[string]any)
			val, present := obj[name]
			if !present {
				if reqSet[name] {
					return ErrSchemaViolation.Wrapf("field %s.%s: required field missing", path, name)
				}
				continue
			}
			if err := validateValue(childSchema, val, path+"."+name); err != nil {
				return err
			}
		}

	case "array":
		arr, ok := data.([]any)
		if !ok {
			return ErrSchemaViolation.Wrapf("field %s: expected array, got %T", path, data)
		}
		items, _ := schema["items"].(map[string]any)
		for i, elem := range arr {
			if err := validateValue(items, elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}

	case "string":
		if _, ok := data.(string); !ok {
			return ErrSchemaViolation.Wrapf("field %s: expected string, got %T", path, data)
		}

	case "integer":
		switch v := data.(type) {
		case float64:
			if v != float64(int64(v)) {
				return ErrSchemaViolation.Wrapf("field %s: expected integer, got fractional number", path)
			}
		case json.Number:
			if _, err := v.Int64(); err != nil {
				return ErrSchemaViolation.Wrapf("field %s: expected integer: %v", path, err)
			}
		default:
			return ErrSchemaViolation.Wrapf("field %s: expected integer, got %T", path, data)
		}

	case "number":
		switch data.(type) {
		case float64, json.Number:
			// ok
		default:
			return ErrSchemaViolation.Wrapf("field %s: expected number, got %T", path, data)
		}

	case "boolean":
		if _, ok := data.(bool); !ok {
			return ErrSchemaViolation.Wrapf("field %s: expected boolean, got %T", path, data)
		}
	}

	return nil
}
