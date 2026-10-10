// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// canonObj is a closed object schema with the given properties, the canonical shape of every object
// that declares fields.
func canonObj(props map[string]any, required ...any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func field(schema map[string]any) map[string]any { return canonObj(map[string]any{"f": schema}) }

func TestValidateTitle(t *testing.T) {
	accepted := map[string]string{
		"Send a message":             "Send a message",
		"  padded  ":                 "padded",
		strings.Repeat("é", 80):      strings.Repeat("é", 80), // characters, not bytes
		strings.Repeat("a", 80):      strings.Repeat("a", 80),
		"Wetter für morgen — Zürich": "Wetter für morgen — Zürich",
	}
	for in, want := range accepted {
		got, err := ValidateTitle(in)
		if err != nil || got != want {
			t.Errorf("ValidateTitle(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	refused := []string{"", "   ", "two\nlines", "cr\rhere", strings.Repeat("a", 81)}
	for _, in := range refused {
		if _, err := ValidateTitle(in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ValidateTitle(%q) = %v; want ErrInvalidInput", in, err)
		}
	}
}

// Every keyword the subset accepts, on the type that accepts it, normalizes without error and
// is kept as written.
func TestNormalizeSchemaAcceptsTheSubset(t *testing.T) {
	cases := map[string]map[string]any{
		"annotations":         {"type": "string", "title": "T", "description": "d", "default": "x", "examples": []any{"a"}, "deprecated": true},
		"string enum":         {"type": "string", "enum": []any{"a", "b"}},
		"string format":       {"type": "string", "format": "date-time"},
		"string bounds":       {"type": "string", "minLength": float64(1), "maxLength": float64(5), "pattern": "^[a-z]+$"},
		"integer bounds":      {"type": "integer", "minimum": float64(0), "maximum": float64(9), "multipleOf": float64(3)},
		"number exclusive":    {"type": "number", "exclusiveMinimum": float64(0), "exclusiveMaximum": float64(1)},
		"boolean":             {"type": "boolean", "default": false},
		"array bounds":        {"type": "array", "items": map[string]any{"type": "string"}, "minItems": float64(1), "maxItems": float64(3), "uniqueItems": true},
		"nullable type":       {"type": []any{"integer", "null"}},
		"nullable enum":       {"type": []any{"string", "null"}, "enum": []any{"a", nil}},
		"open object":         {"type": "object", "description": "anything"},
		"map-shaped object":   {"type": "object", "properties": map[string]any{}, "additionalProperties": map[string]any{"type": "number"}},
		"map requiring a key": {"type": "object", "properties": map[string]any{}, "additionalProperties": map[string]any{"type": "number"}, "required": []any{"k"}},
		"nullable default":    {"type": []any{"integer", "null"}, "default": nil},
		"nested default":      {"type": "array", "items": map[string]any{"type": "integer", "default": float64(3)}, "default": []any{float64(1)}},
		"nested closed":       canonObj(map[string]any{"g": map[string]any{"type": "string"}}, "g"),
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			in := field(f)
			out, notes, err := NormalizeSchema("input", in)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if len(notes) != 0 {
				t.Errorf("canonical schema produced notes: %v", notes)
			}
			if !jsonEqual(out, in) {
				t.Errorf("canonical schema changed:\n in  %v\n out %v", in, out)
			}
			if err := ValidateSchema("input", in); err != nil {
				t.Errorf("ValidateSchema refused a canonical schema: %v", err)
			}
		})
	}
}

// Every form outside the subset is refused with the path to it and the rule it breaks.
func TestNormalizeSchemaRefusesOutsideTheSubset(t *testing.T) {
	cases := map[string]struct {
		field map[string]any
		want  string
	}{
		"oneOf":                       {map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}}, "input.properties.f: oneOf is not supported"},
		"anyOf of two types":          {map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}}, "input.properties.f: anyOf is not supported"},
		"anyOf of three with null":    {map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}, map[string]any{"type": "null"}}}, "anyOf is not supported"},
		"type union":                  {map[string]any{"type": []any{"string", "integer"}}, "type may name one type"},
		"null alone":                  {map[string]any{"type": "null"}, "unsupported type null"},
		"untyped field":               {map[string]any{"description": "any value"}, "input.properties.f: type is required"},
		"empty field":                 {map[string]any{}, "input.properties.f: type is required"},
		"not":                         {map[string]any{"type": "string", "not": map[string]any{}}, `unsupported keyword "not"`},
		"if":                          {map[string]any{"type": "object", "if": map[string]any{}}, `unsupported keyword "if"`},
		"patternProperties":           {map[string]any{"type": "object", "patternProperties": map[string]any{}}, `unsupported keyword "patternProperties"`},
		"propertyNames":               {map[string]any{"type": "object", "propertyNames": map[string]any{}}, `unsupported keyword "propertyNames"`},
		"dependentRequired":           {map[string]any{"type": "object", "dependentRequired": map[string]any{}}, `unsupported keyword "dependentRequired"`},
		"prefixItems":                 {map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "prefixItems": []any{}}, `unsupported keyword "prefixItems"`},
		"contains":                    {map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "contains": map[string]any{}}, `unsupported keyword "contains"`},
		"bound on wrong type":         {map[string]any{"type": "string", "minimum": float64(0)}, `unsupported keyword "minimum"`},
		"integer enum":                {map[string]any{"type": "integer", "enum": []any{float64(1)}}, "enum is supported on strings only"},
		"mixed enum":                  {map[string]any{"type": "string", "enum": []any{"a", float64(1)}}, "enum values must be strings"},
		"empty enum":                  {map[string]any{"type": "string", "enum": []any{}}, "enum must list at least one string"},
		"const outside the enum":      {map[string]any{"type": "string", "enum": []any{"a"}, "const": "b"}, `const "b" is not in the enum`},
		"numeric const":               {map[string]any{"type": "integer", "const": float64(1)}, "const is supported on strings only"},
		"array without items":         {map[string]any{"type": "array"}, "must declare items"},
		"negative length":             {map[string]any{"type": "string", "minLength": float64(-1)}, "minLength must be a non-negative integer"},
		"fractional count":            {map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 1.5}, "maxItems must be a non-negative integer"},
		"zero multipleOf":             {map[string]any{"type": "number", "multipleOf": float64(0)}, "multipleOf must be greater than 0"},
		"bad pattern":                 {map[string]any{"type": "string", "pattern": "("}, "pattern does not compile"},
		"open beside properties":      {map[string]any{"type": "object", "properties": map[string]any{"g": map[string]any{"type": "string"}}, "additionalProperties": true}, "additionalProperties true beside declared properties"},
		"non-string description":      {map[string]any{"type": "string", "description": float64(1)}, "description must be a string"},
		"external ref":                {map[string]any{"$ref": "https://example.com/s.json"}, "is not a local reference"},
		"unresolvable ref":            {map[string]any{"$ref": "#/$defs/missing"}, "does not resolve"},
		"allOf of objects":            {map[string]any{"allOf": []any{canonObj(map[string]any{"a": map[string]any{"type": "string"}}), canonObj(map[string]any{"b": map[string]any{"type": "string"}})}}, "input.properties.f: allOf is not supported"},
		"allOf of one schema":         {map[string]any{"allOf": []any{map[string]any{"type": "string"}}, "description": "d"}, "allOf is not supported"},
		"anyOf with null":             {map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}}, `may be empty is "type": [T, "null"]`},
		"sibling weakens a reference": {map[string]any{"$ref": "#/$defs/code", "minLength": float64(2)}, `minLength beside $ref "#/$defs/code" differs`},
		"sibling retypes a reference": {map[string]any{"$ref": "#/$defs/code", "type": "integer"}, `type beside $ref "#/$defs/code" differs`},
		"default of the wrong type":   {map[string]any{"type": "integer", "default": "10"}, "field input.properties.f default: expected integer, got a string"},
		"default outside its enum":    {map[string]any{"type": "string", "enum": []any{"a"}, "default": "b"}, "default: value not in enum"},
		"null default, not nullable":  {map[string]any{"type": "string", "default": nil}, "default: must not be null"},
		"example out of bounds":       {map[string]any{"type": "integer", "maximum": float64(9), "example": float64(10)}, "input.properties.f example"},
		"nested default names a path": {map[string]any{"type": "array", "items": map[string]any{"type": "integer", "default": true}}, "input.properties.f.items default"},
		"required but undeclared":     {map[string]any{"type": "object", "properties": map[string]any{"g": map[string]any{"type": "string"}}, "required": []any{"h"}}, `input.properties.f: required names "h"`},
		"nested error names its path": {map[string]any{"type": "array", "items": canonObj(map[string]any{"g": map[string]any{"oneOf": []any{}}})}, "input.properties.f.items.properties.g: oneOf"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := field(c.field)
			in["$defs"] = map[string]any{"code": map[string]any{"type": "string", "minLength": float64(5)}}
			_, _, err := NormalizeSchema("input", in)
			if !errors.Is(err, ErrSchemaViolation) {
				t.Fatalf("got %v, want ErrSchemaViolation", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
		})
	}
}

// The top level is the arguments or the result of a call, which is always an object.
func TestNormalizeSchemaTopLevel(t *testing.T) {
	out, _, err := NormalizeSchema("output", map[string]any{})
	if err != nil || !jsonEqual(out, map[string]any{"type": "object"}) {
		t.Errorf("{} = %v, %v; want an open object", out, err)
	}
	out, _, err = NormalizeSchema("input", map[string]any{"properties": map[string]any{"a": map[string]any{"type": "string"}}})
	if err != nil || out["type"] != "object" {
		t.Errorf("properties without type = %v, %v; want an object", out, err)
	}
	for name, s := range map[string]map[string]any{
		"array":    {"type": "array", "items": map[string]any{"type": "string"}},
		"string":   {"type": "string"},
		"nullable": {"type": []any{"object", "null"}},
	} {
		if _, _, err := NormalizeSchema("output", s); err == nil || !strings.Contains(err.Error(), "output: the top level must be an object") {
			t.Errorf("%s at the top level: got %v", name, err)
		}
	}
	if _, _, err := NormalizeSchema("input", nil); !errors.Is(err, ErrSchemaViolation) {
		t.Errorf("nil schema: got %v", err)
	}
}

// Each fold yields the canonical form a strict decoder accepts, and says so in a note naming the
// path, except where only the representation changed.
func TestNormalizeSchemaFolds(t *testing.T) {
	str := map[string]any{"type": "string"}
	cases := map[string]struct {
		in, want map[string]any
		note     string // "" means the change is representation only and is not reported
	}{
		"nullable true": {
			field(map[string]any{"type": "string", "nullable": true}),
			field(map[string]any{"type": []any{"string", "null"}}),
			"input.properties.f: nullable folded",
		},
		"nullable false": {
			field(map[string]any{"type": "string", "nullable": false}),
			field(str), "",
		},
		"nullable enum keeps its values": {
			field(map[string]any{"type": "string", "enum": []any{"a"}, "nullable": true}),
			field(map[string]any{"type": []any{"string", "null"}, "enum": []any{"a"}}),
			"nullable folded",
		},
		"const inside an enum": {
			field(map[string]any{"type": "string", "enum": []any{"a", "b"}, "const": "b"}),
			field(map[string]any{"type": "string", "enum": []any{"b"}}),
			"const folded",
		},
		"type list order": {
			field(map[string]any{"type": []any{"null", "string"}}),
			field(map[string]any{"type": []any{"string", "null"}}), "",
		},
		"const": {
			field(map[string]any{"const": "only"}),
			field(map[string]any{"type": "string", "enum": []any{"only"}}),
			"input.properties.f: const folded",
		},
		"declared properties close the object": {
			field(map[string]any{"type": "object", "properties": map[string]any{"a": str}}),
			field(canonObj(map[string]any{"a": str})), "",
		},
		"empty properties leave it open": {
			field(map[string]any{"type": "object", "properties": map[string]any{}}),
			field(map[string]any{"type": "object"}), "",
		},
		"open additionalProperties": {
			field(map[string]any{"type": "object", "additionalProperties": true}),
			field(map[string]any{"type": "object"}), "",
		},
		"example becomes examples": {
			field(map[string]any{"type": "string", "example": "x"}),
			field(map[string]any{"type": "string", "examples": []any{"x"}}), "",
		},
		"metadata dropped": {
			field(map[string]any{"type": "string", "readOnly": true, "x-vendor": 1, "$comment": "c"}),
			field(str), "",
		},
		"required as strings": {
			map[string]any{"type": "object", "properties": map[string]any{"a": str}, "required": []string{"a"}},
			canonObj(map[string]any{"a": str}, "a"), "",
		},
		"empty required dropped": {
			map[string]any{"type": "object", "properties": map[string]any{"a": str}, "required": []any{}},
			canonObj(map[string]any{"a": str}), "",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out, notes, err := NormalizeSchema("input", c.in)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !jsonEqual(out, c.want) {
				t.Errorf("canonical form:\n got  %v\n want %v", out, c.want)
			}
			joined := strings.Join(notes, "\n")
			if c.note == "" && len(notes) != 0 {
				t.Errorf("unexpected notes: %v", notes)
			}
			if c.note != "" && !strings.Contains(joined, c.note) {
				t.Errorf("notes %q do not say %q", joined, c.note)
			}
			// The canonical form is a fixed point: normalizing it again changes nothing and notes
			// nothing, and it passes as canonical.
			again, notes2, err := NormalizeSchema("input", out)
			if err != nil || !jsonEqual(again, out) || len(notes2) != 0 {
				t.Errorf("not a fixed point: %v, %v, %v", again, notes2, err)
			}
			if err := ValidateSchema("input", out); err != nil {
				t.Errorf("ValidateSchema refused the canonical form: %v", err)
			}
			if c.note != "" || !jsonEqual(c.in, out) {
				if err := ValidateSchema("input", c.in); err == nil {
					t.Error("ValidateSchema accepted a schema that is not canonical")
				}
			}
		})
	}
}

// A fold changes how a contract is written, never what it admits.
func TestNormalizeSchemaFoldsPreserveMeaning(t *testing.T) {
	cases := []struct {
		in       map[string]any
		accepted []any
		refused  []any
	}{
		{map[string]any{"type": "string", "nullable": true}, []any{"x", nil}, []any{float64(1)}},
		{map[string]any{"type": "string", "enum": []any{"a", "b"}, "nullable": true}, []any{"a"}, []any{"c", nil}},
		{map[string]any{"type": "string", "enum": []any{"a", nil}, "nullable": true}, []any{"a", nil}, []any{"c"}},
		{map[string]any{"type": "string", "enum": []any{"a", "b"}, "const": "b"}, []any{"b"}, []any{"a"}},
		{map[string]any{"const": "only"}, []any{"only"}, []any{"other", nil}},
		{map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
			[]any{map[string]any{"a": "x"}, map[string]any{}}, []any{map[string]any{"z": "x"}}},
	}
	for i, c := range cases {
		out, _, err := NormalizeSchema("input", field(c.in))
		if err != nil {
			t.Fatalf("case %d refused: %v", i, err)
		}
		for _, v := range c.accepted {
			if err := ValidateInput(out, map[string]any{"f": v}); err != nil {
				t.Errorf("case %d: %v refused: %v", i, v, err)
			}
		}
		for _, v := range c.refused {
			if err := ValidateInput(out, map[string]any{"f": v}); err == nil {
				t.Errorf("case %d: %v accepted", i, v)
			}
		}
	}
}

func TestNormalizeSchemaReferences(t *testing.T) {
	t.Run("local definitions are inlined and dropped", func(t *testing.T) {
		in := map[string]any{
			"type":       "object",
			"properties": map[string]any{"when": map[string]any{"$ref": "#/$defs/day", "description": "the day"}},
			"$defs":      map[string]any{"day": map[string]any{"type": "string", "format": "date", "description": "a day"}},
		}
		out, _, err := NormalizeSchema("input", in)
		if err != nil {
			t.Fatal(err)
		}
		want := canonObj(map[string]any{"when": map[string]any{"type": "string", "format": "date", "description": "the day"}})
		if !jsonEqual(out, want) {
			t.Errorf("got %v, want %v (sibling keywords override the target)", out, want)
		}
	})
	t.Run("a constraint beside a reference is added to the target's", func(t *testing.T) {
		in := map[string]any{
			"type":       "object",
			"properties": map[string]any{"code": map[string]any{"$ref": "#/$defs/code", "maxLength": float64(8), "minLength": float64(5)}},
			"$defs":      map[string]any{"code": map[string]any{"type": "string", "minLength": float64(5)}},
		}
		out, _, err := NormalizeSchema("input", in)
		if err != nil {
			t.Fatal(err)
		}
		want := canonObj(map[string]any{"code": map[string]any{"type": "string", "minLength": float64(5), "maxLength": float64(8)}})
		if !jsonEqual(out, want) {
			t.Errorf("got %v, want both constraints", out)
		}
	})
	t.Run("a definition used twice is inlined twice", func(t *testing.T) {
		in := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"$ref": "#/definitions/n"},
				"b": map[string]any{"$ref": "#/definitions/n"},
			},
			"definitions": map[string]any{"n": map[string]any{"type": "number"}},
		}
		if _, _, err := NormalizeSchema("input", in); err != nil {
			t.Errorf("a shared definition is not recursion: %v", err)
		}
	})
	t.Run("recursion is refused", func(t *testing.T) {
		in := map[string]any{
			"type":       "object",
			"properties": map[string]any{"tree": map[string]any{"$ref": "#/$defs/node"}},
			"$defs": map[string]any{"node": map[string]any{
				"type": "object", "properties": map[string]any{"child": map[string]any{"$ref": "#/$defs/node"}},
			}},
		}
		if _, _, err := NormalizeSchema("input", in); err == nil || !strings.Contains(err.Error(), "recursive") {
			t.Errorf("got %v, want a recursion refusal", err)
		}
	})
	t.Run("references resolve against the document a schema came from", func(t *testing.T) {
		doc := map[string]any{"components": map[string]any{"schemas": map[string]any{"Pet": map[string]any{
			"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}},
		}}}}
		out, _, err := normalizeIn("output", map[string]any{"$ref": "#/components/schemas/Pet"}, doc)
		if err != nil || !jsonEqual(out, canonObj(map[string]any{"name": map[string]any{"type": "string"}})) {
			t.Errorf("got %v, %v", out, err)
		}
	})
}

func TestNormalizeSchemaLimits(t *testing.T) {
	deep := map[string]any{"type": "string"}
	for i := 0; i < maxSchemaDepth+1; i++ {
		deep = canonObj(map[string]any{"n": deep})
	}
	if _, _, err := NormalizeSchema("input", deep); err == nil || !strings.Contains(err.Error(), "nested deeper") {
		t.Errorf("depth: got %v", err)
	}
	props := map[string]any{}
	for i := 0; i < 2000; i++ {
		props[fmt.Sprintf("field%04d", i)] = map[string]any{"type": "string", "description": strings.Repeat("d", 40)}
	}
	if _, _, err := NormalizeSchema("input", map[string]any{"type": "object", "properties": props}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("size: got %v", err)
	}
}

func TestNormalizeSchemaLeavesItsInputAlone(t *testing.T) {
	in := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string", "nullable": true}}}
	before, _ := CanonicalJSON(in)
	if _, _, err := NormalizeSchema("input", in); err != nil {
		t.Fatal(err)
	}
	after, _ := CanonicalJSON(in)
	if string(before) != string(after) {
		t.Errorf("input modified:\n before %s\n after  %s", before, after)
	}
}

// Notes and the first error come out in one order, so one document always reports the same way.
func TestNormalizeSchemaIsDeterministic(t *testing.T) {
	props := map[string]any{}
	for _, k := range []string{"e", "b", "d", "a", "c"} {
		props[k] = map[string]any{"type": "string", "nullable": true}
	}
	in := map[string]any{"type": "object", "properties": props}
	_, first, _ := NormalizeSchema("input", in)
	for i := 0; i < 20; i++ {
		_, notes, _ := NormalizeSchema("input", in)
		if strings.Join(notes, "|") != strings.Join(first, "|") {
			t.Fatalf("notes reordered: %v vs %v", notes, first)
		}
	}
	if !strings.HasPrefix(first[0], "input.properties.a:") {
		t.Errorf("notes not in property order: %v", first)
	}
}

func TestValidateSchemaDescriptions(t *testing.T) {
	ok := canonObj(map[string]any{"outer": map[string]any{
		"type": "object", "description": "outer", "properties": map[string]any{"inner": map[string]any{"type": "string", "description": "inner"}},
	}})
	if err := validateSchemaDescriptions(ok, "input"); err != nil {
		t.Errorf("described properties: %v", err)
	}
	if err := validateSchemaDescriptions(map[string]any{"type": "object"}, "input"); err != nil {
		t.Errorf("no properties: %v", err)
	}
	missing := canonObj(map[string]any{"outer": map[string]any{
		"type": "object", "description": "outer", "properties": map[string]any{"inner": map[string]any{"type": "string"}},
	}})
	if err := validateSchemaDescriptions(missing, "input"); err == nil || !strings.Contains(err.Error(), "input.outer.inner") {
		t.Errorf("nested missing description: got %v", err)
	}
}

func TestValidateInput(t *testing.T) {
	cases := []struct {
		name     string
		schema   map[string]any
		accepted []any
		refused  []any
	}{
		{"required and types", canonObj(map[string]any{"name": map[string]any{"type": "string"}, "count": map[string]any{"type": "number"}}, "name"),
			[]any{map[string]any{"name": "x", "count": float64(3)}, map[string]any{"name": "x"}},
			[]any{map[string]any{"count": float64(3)}, map[string]any{"name": float64(1)}, map[string]any{"name": "x", "extra": true}, "not an object"}},
		{"open object", map[string]any{"type": "object"},
			[]any{map[string]any{"anything": true}}, []any{[]any{}}},
		{"map-shaped object", map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": map[string]any{"type": "integer"}},
			[]any{map[string]any{"a": float64(1), "b": float64(2)}}, []any{map[string]any{"a": "1"}}},
		{"declared and extra", map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "additionalProperties": map[string]any{"type": "boolean"}},
			[]any{map[string]any{"id": "x", "flag": true}}, []any{map[string]any{"id": "x", "flag": "yes"}, map[string]any{"id": float64(1)}}},
		{"legacy properties without additionalProperties stay closed", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
			[]any{map[string]any{"x": "a"}}, []any{map[string]any{"x": "a", "y": float64(1)}}},
		{"required name outside properties", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}, "required": []any{"x", "y"}},
			[]any{map[string]any{"x": "a", "y": "b"}}, []any{map[string]any{"x": "a"}}},
		{"nullable", map[string]any{"type": []any{"string", "null"}},
			[]any{"a", nil}, []any{float64(1)}},
		{"not nullable", map[string]any{"type": "string"},
			[]any{"a"}, []any{nil}},
		{"string bounds", map[string]any{"type": "string", "minLength": float64(2), "maxLength": float64(3), "pattern": "^[a-z]+$"},
			[]any{"ab", "abc"}, []any{"a", "abcd", "AB"}},
		{"length counts characters", map[string]any{"type": "string", "maxLength": float64(3)},
			[]any{"été"}, []any{"étés"}},
		{"integer", map[string]any{"type": "integer", "minimum": float64(1), "maximum": float64(10), "multipleOf": float64(2)},
			[]any{float64(2), float64(10)}, []any{float64(0), float64(12), float64(3), 2.5, "2"}},
		{"exclusive bounds", map[string]any{"type": "number", "exclusiveMinimum": float64(0), "exclusiveMaximum": float64(1)},
			[]any{0.5}, []any{float64(0), float64(1)}},
		{"fractional multipleOf is exact", map[string]any{"type": "number", "multipleOf": 0.1},
			[]any{0.3, float64(2), 0.7, -1.1}, []any{0.35, 0.30000000001, 0.1 + 1e-12}},
		{"large multipleOf", map[string]any{"type": "integer", "multipleOf": float64(1000)},
			[]any{float64(3000), float64(-2000)}, []any{float64(3001)}},
		{"array", map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": float64(1), "maxItems": float64(2), "uniqueItems": true},
			[]any{[]any{"a"}, []any{"a", "b"}}, []any{[]any{}, []any{"a", "b", "c"}, []any{"a", "a"}, []any{float64(1)}}},
		{"string enum is checked after type", map[string]any{"type": "string", "enum": []any{"1", "2"}},
			[]any{"1"}, []any{float64(1), "3"}},
		{"nullable enum listing null", map[string]any{"type": []any{"string", "null"}, "enum": []any{"a", nil}},
			[]any{"a", nil}, []any{"b"}},
		{"nullable enum not listing null", map[string]any{"type": []any{"string", "null"}, "enum": []any{"a"}},
			[]any{"a"}, []any{nil, "b"}},
		{"integers JSON carries exactly", map[string]any{"type": "integer", "multipleOf": float64(2)},
			[]any{float64(1<<53 - 2), float64(-(1<<53 - 2))}, []any{float64(1 << 53), float64(9007199254740993), float64(-(1 << 53))}},
		{"untyped accepts anything", map[string]any{"description": "x"},
			[]any{nil, "s", float64(1)}, nil},
		{"nil schema accepts anything", nil,
			[]any{map[string]any{"anything": true}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, v := range c.accepted {
				if err := ValidateInput(c.schema, v); err != nil {
					t.Errorf("%v refused: %v", v, err)
				}
			}
			for _, v := range c.refused {
				err := ValidateInput(c.schema, v)
				if !errors.Is(err, ErrSchemaViolation) {
					t.Errorf("%v: got %v, want ErrSchemaViolation", v, err)
				}
			}
		})
	}
}

// A violation names the field the caller wrote, so they can fix it.
func TestValidateInputNamesTheField(t *testing.T) {
	s := canonObj(map[string]any{"items": map[string]any{"type": "array", "items": canonObj(map[string]any{"qty": map[string]any{"type": "integer", "minimum": float64(1)}})}})
	err := ValidateInput(s, map[string]any{"items": []any{map[string]any{"qty": float64(1)}, map[string]any{"qty": float64(0)}}})
	if err == nil || !strings.Contains(err.Error(), "items[1].qty") {
		t.Errorf("got %v, want the path items[1].qty", err)
	}
}
