// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// An action's contract is a tool definition (D4): a title a person reads in a list, a description,
// and input and output schemas in one documented subset of JSON Schema 2020-12. This file is the
// one definition of that subset. NormalizeSchema turns any accepted spelling into the canonical
// form, and everything downstream — the validator, the stored row, the quote and manifest hashes,
// the manifest a peer receives, a model's tool definition — reads only that form.

// MaxTitleLength is the longest title an action may carry, in characters [policy].
const MaxTitleLength = 80

// ValidateTitle returns the title an action stores: trimmed, one line, 1 to MaxTitleLength
// characters. The address names an action for software; the title names it for a person reading a
// catalogue, so every action has one.
func ValidateTitle(title string) (string, error) {
	t := strings.TrimSpace(title)
	switch {
	case t == "":
		return "", ErrInvalidInput.Wrap("title is required: a short name a person reads in a list")
	case strings.ContainsAny(t, "\r\n"):
		return "", ErrInvalidInput.Wrap("title must be one line")
	case utf8.RuneCountInString(t) > MaxTitleLength:
		return "", ErrInvalidInput.Wrapf("title is longer than %d characters", MaxTitleLength)
	}
	return t, nil
}

const (
	maxExactInteger = 1<<53 - 1
	maxSchemaDepth  = 8
	maxSchemaBytes  = 64 << 10 // canonical size after $ref inlining [policy]
)

// annotationKeys may appear on any node; they describe a value and never constrain it.
var annotationKeys = map[string]bool{
	"type": true, "title": true, "description": true, "default": true, "examples": true, "deprecated": true,
}

// typeKeys are the constraints each type accepts. Anything else is refused by name.
var typeKeys = map[string]map[string]bool{
	"string":  {"enum": true, "format": true, "minLength": true, "maxLength": true, "pattern": true},
	"integer": {"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true},
	"number":  {"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true},
	"boolean": {},
	"array":   {"items": true, "minItems": true, "maxItems": true},
	"object":  {"properties": true, "required": true, "additionalProperties": true},
}

// describes are the keys that describe a value without constraining it, so a use may replace them.
var describes = map[string]bool{"title": true, "description": true, "default": true, "examples": true, "deprecated": true}

// droppedKeys carry no meaning for a value: document metadata and OpenAPI presentation hints.
// They are removed rather than refused, so a generated schema imports as written.
var droppedKeys = map[string]bool{
	"$schema": true, "$id": true, "$comment": true, "readOnly": true, "writeOnly": true, "xml": true, "externalDocs": true,
}

// NormalizeSchema returns the canonical form of schema and one note per fold that changed what the
// author wrote, or an error naming the path and the rule broken. label names the root ("input",
// "output") in every path. The input is never modified.
//
// Folds, each admitting exactly the values the original spelling did: OpenAPI's `nullable: true`
// becomes `type: [T, "null"]`; `const` on a string becomes a one-value enum; local $ref is inlined;
// `example` becomes `examples`. `allOf`, `anyOf` and `oneOf` are refused. Representation only, never noted: a top-level `{}` is an
// open object, an object with declared properties states `additionalProperties: false` (it was
// already closed), `type: ["null", T]` is written `[T, "null"]`.
func NormalizeSchema(label string, schema map[string]any) (map[string]any, []string, error) {
	return normalizeIn(label, schema, nil)
}

// normalizeIn normalizes schema with its local $refs resolved against doc, the document it was
// taken from (an OpenAPI file's components), or against the schema itself when doc is nil.
func normalizeIn(label string, schema, doc map[string]any) (map[string]any, []string, error) {
	if schema == nil {
		return nil, nil, ErrSchemaViolation.Wrapf("%s: schema is required", label)
	}
	// Work on a JSON copy: a schema written as a Go literal and one decoded from a request then
	// hold the same types, and the caller's map is never touched.
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, nil, ErrSchemaViolation.Wrapf("%s: schema is not JSON: %v", label, err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, ErrSchemaViolation.Wrapf("%s: schema is not JSON: %v", label, err)
	}
	if doc == nil {
		doc = root
	}
	n := &normalizer{root: doc, active: map[string]bool{}}
	out, err := n.node(root, label, 0, true)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAnnotationValues(out, label); err != nil {
		return nil, nil, err
	}
	if t, ok := out["type"].(string); !ok || t != "object" {
		return nil, nil, ErrSchemaViolation.Wrapf("%s: the top level must be an object, not %s", label, typeName(out["type"]))
	}
	if b, err := CanonicalJSON(out); err != nil || len(b) > maxSchemaBytes {
		return nil, nil, ErrSchemaViolation.Wrapf("%s: schema is larger than %d KiB", label, maxSchemaBytes>>10)
	}
	return out, n.notes, nil
}

// ValidateSchema reports whether schema is already canonical. Only what crosses a boundary already
// normalized is held to it — a stored row, a native's declaration, a peer's manifest — since a
// schema that would change under normalization is not the one its hash or signature was taken over.
func ValidateSchema(label string, schema map[string]any) error {
	out, _, err := NormalizeSchema(label, schema)
	if err != nil {
		return err
	}
	if !jsonEqual(out, schema) {
		return ErrSchemaViolation.Wrapf("%s: schema is not in canonical form", label)
	}
	return nil
}

type normalizer struct {
	root   map[string]any
	active map[string]bool // $refs being inlined on the current path: one seen again is a cycle
	notes  []string
}

func (n *normalizer) note(path, format string, args ...any) {
	n.notes = append(n.notes, path+": "+fmt.Sprintf(format, args...))
}

// node normalizes one schema node. The structural folds each rewrite the node into a simpler one
// and recurse, so the typed checks at the end see one plain typed node.
func (n *normalizer) node(in map[string]any, path string, depth int, top bool) (map[string]any, error) {
	if depth > maxSchemaDepth {
		return nil, ErrSchemaViolation.Wrapf("%s: nested deeper than %d levels", path, maxSchemaDepth)
	}
	s := make(map[string]any, len(in))
	for k, v := range in {
		if droppedKeys[k] || strings.HasPrefix(k, "x-") {
			continue
		}
		if top && (k == "$defs" || k == "definitions") {
			continue // referenced definitions are inlined below
		}
		s[k] = v
	}
	if ex, ok := s["example"]; ok {
		delete(s, "example")
		if _, has := s["examples"]; !has {
			s["examples"] = []any{ex}
		}
	}

	if ref, ok := s["$ref"]; ok {
		return n.ref(s, ref, path, depth, top)
	}
	// Combinators are refused, all three: a field has one type, a choice among strings is an enum,
	// and a field that may be empty says so in its type.
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		if _, ok := s[k]; ok {
			return nil, ErrSchemaViolation.Wrapf("%s: %s is not supported: a field has one type, a choice among strings is an enum, and a field that may be empty is \"type\": [T, \"null\"]", path, k)
		}
	}
	if v, ok := s["nullable"]; ok {
		delete(s, "nullable")
		nullable, isBool := v.(bool)
		if !isBool {
			return nil, ErrSchemaViolation.Wrapf("%s: nullable must be true or false", path)
		}
		if !nullable {
			return n.node(s, path, depth, top)
		}
		n.note(path, "nullable folded into type [T, \"null\"]")
		return n.nullable(s, path, depth, top)
	}
	if list, ok := s["type"].([]any); ok {
		t, nullable, err := typeList(list, path)
		if err != nil {
			return nil, err
		}
		s["type"] = t
		if nullable {
			return n.nullable(s, path, depth, top)
		}
	}
	return n.typed(s, path, depth, top)
}

// nullable normalizes s and admits null beside its type.
func (n *normalizer) nullable(s map[string]any, path string, depth int, top bool) (map[string]any, error) {
	out, err := n.node(s, path, depth, top)
	if err != nil {
		return nil, err
	}
	if top {
		return nil, ErrSchemaViolation.Wrapf("%s: the top level must be an object, not null", path)
	}
	t, isName := out["type"].(string)
	if !isName {
		return out, nil // already [T, "null"]
	}
	out["type"] = []any{t, "null"}
	return out, nil
}

// typeList reads `type: [T, "null"]` in either order. A type list naming two real types is a union.
func typeList(list []any, path string) (string, bool, error) {
	var t string
	nullable := false
	for _, e := range list {
		s, _ := e.(string)
		switch {
		case s == "null":
			nullable = true
		case t == "" && s != "":
			t = s
		default:
			return "", false, ErrSchemaViolation.Wrapf("%s: type may name one type, optionally with \"null\"", path)
		}
	}
	if t == "" {
		return "", false, ErrSchemaViolation.Wrapf("%s: type must name a type other than null", path)
	}
	return t, nullable, nil
}

// ref inlines a local reference.
func (n *normalizer) ref(s map[string]any, rawRef any, path string, depth int, top bool) (map[string]any, error) {
	ref, _ := rawRef.(string)
	ptr, local := strings.CutPrefix(ref, "#/")
	if !local {
		return nil, ErrSchemaViolation.Wrapf("%s: $ref %q is not a local reference", path, ref)
	}
	if n.active[ref] {
		return nil, ErrSchemaViolation.Wrapf("%s: $ref %q is recursive", path, ref)
	}
	target, ok := resolveJSONPointer(n.root, ptr).(map[string]any)
	if !ok {
		return nil, ErrSchemaViolation.Wrapf("%s: $ref %q does not resolve", path, ref)
	}
	// Keywords beside a reference apply together with the target's (JSON Schema 2020-12). A
	// description or title beside it is the author's word for this use and replaces the target's;
	// a constraint the target lacks is added; one that differs from the target's would need both to
	// hold, which one stored schema cannot say, so it is refused.
	merged := make(map[string]any, len(target)+len(s))
	for k, v := range target {
		merged[k] = v
	}
	for k, v := range s {
		if k == "$ref" {
			continue
		}
		if old, both := target[k]; both && !describes[k] && !jsonEqual(old, v) {
			return nil, ErrSchemaViolation.Wrapf("%s: %s beside $ref %q differs from the referenced schema's", path, k, ref)
		}
		merged[k] = v
	}
	n.active[ref] = true
	defer delete(n.active, ref)
	return n.node(merged, path, depth, top)
}

// typed checks one plain node of one type and normalizes its children.
func (n *normalizer) typed(s map[string]any, path string, depth int, top bool) (map[string]any, error) {
	if c, ok := s["const"]; ok {
		cs, isString := c.(string)
		if !isString {
			return nil, ErrSchemaViolation.Wrapf("%s: const is supported on strings only", path)
		}
		delete(s, "const")
		if enum, has := s["enum"].([]any); has {
			listed := false
			for _, e := range enum {
				listed = listed || e == cs
			}
			if !listed {
				return nil, ErrSchemaViolation.Wrapf("%s: const %q is not in the enum, so no value is valid", path, cs)
			}
		}
		s["enum"] = []any{cs}
		if _, has := s["type"]; !has {
			s["type"] = "string"
		}
		n.note(path, "const folded into a one-value enum")
	}
	raw, has := s["type"]
	if _, hasProps := s["properties"]; !has && !top && !hasProps {
		// No type and nothing but annotations is `{}`, a leaf for any JSON value (D4); a limit
		// without a type would hold for some types and not others.
		for k := range s {
			if !annotationKeys[k] {
				return nil, ErrSchemaViolation.Wrapf("%s: type is required beside %q", path, k)
			}
		}
		return s, checkAnnotations(s, path)
	}
	if !has {
		s["type"] = "object"
		raw = "object"
	}
	t, _ := raw.(string)
	allowed, known := typeKeys[t]
	if !known {
		return nil, ErrSchemaViolation.Wrapf("%s: unsupported type %s", path, typeName(raw))
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the first offending keyword is the same one on every run
	for _, k := range keys {
		if annotationKeys[k] || allowed[k] {
			continue
		}
		if k == "enum" {
			return nil, ErrSchemaViolation.Wrapf("%s: enum is supported on strings only", path)
		}
		return nil, ErrSchemaViolation.Wrapf("%s: unsupported keyword %q", path, k)
	}
	if err := checkAnnotations(s, path); err != nil {
		return nil, err
	}
	switch t {
	case "string":
		if err := checkString(s, path); err != nil {
			return nil, err
		}
		// A value listed twice would be chosen two ways; listing it once admits the same values.
		if enum, ok := s["enum"].([]any); ok {
			var once []any
			for _, e := range enum {
				if !slices.Contains(once, e) {
					once = append(once, e)
				}
			}
			if len(once) < len(enum) {
				s["enum"] = once
				n.note(path, "repeated enum values listed once")
			}
		}
		return s, nil
	case "integer", "number":
		return s, checkNumber(s, path)
	case "array":
		return n.array(s, path, depth)
	case "object":
		return n.object(s, path, depth)
	}
	return s, nil
}

func checkAnnotations(s map[string]any, path string) error {
	for _, k := range []string{"title", "description", "format", "pattern"} {
		if v, ok := s[k]; ok {
			if _, isString := v.(string); !isString {
				return ErrSchemaViolation.Wrapf("%s: %s must be a string", path, k)
			}
		}
	}
	if v, ok := s["deprecated"]; ok {
		if _, isBool := v.(bool); !isBool {
			return ErrSchemaViolation.Wrapf("%s: deprecated must be true or false", path)
		}
	}
	if v, ok := s["examples"]; ok {
		if _, isList := v.([]any); !isList {
			return ErrSchemaViolation.Wrapf("%s: examples must be a list", path)
		}
	}
	return nil
}

func checkString(s map[string]any, path string) error {
	if v, ok := s["enum"]; ok {
		// null may be listed, and is then admitted where the type is nullable (as in OpenAPI 3.0.3).
		list, isList := v.([]any)
		strs := 0
		for _, e := range list {
			switch e.(type) {
			case string:
				strs++
			case nil:
			default:
				return ErrSchemaViolation.Wrapf("%s: enum values must be strings", path)
			}
		}
		if !isList || strs == 0 {
			return ErrSchemaViolation.Wrapf("%s: enum must list at least one string", path)
		}
	}
	if err := checkCount(s, path, "minLength", "maxLength"); err != nil {
		return err
	}
	if p, ok := s["pattern"].(string); ok {
		if _, err := regexp.Compile(p); err != nil {
			return ErrSchemaViolation.Wrapf("%s: pattern does not compile: %v", path, err)
		}
	}
	return nil
}

func checkNumber(s map[string]any, path string) error {
	for _, k := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
		if v, ok := s[k]; ok {
			f, isNum := numberOf(v)
			if !isNum {
				return ErrSchemaViolation.Wrapf("%s: %s must be a number", path, k)
			}
			if k == "multipleOf" && f <= 0 {
				return ErrSchemaViolation.Wrapf("%s: multipleOf must be greater than 0", path)
			}
		}
	}
	return nil
}

// checkCount checks that each named keyword, when present, is a non-negative whole number.
func checkCount(s map[string]any, path string, keys ...string) error {
	for _, k := range keys {
		if v, ok := s[k]; ok {
			f, isNum := numberOf(v)
			if !isNum || f < 0 || f != math.Trunc(f) {
				return ErrSchemaViolation.Wrapf("%s: %s must be a non-negative integer", path, k)
			}
		}
	}
	return nil
}

func (n *normalizer) array(s map[string]any, path string, depth int) (map[string]any, error) {
	items, ok := s["items"].(map[string]any)
	if !ok {
		return nil, ErrSchemaViolation.Wrapf("%s: an array must declare items as a schema", path)
	}
	child, err := n.node(items, path+".items", depth+1, false)
	if err != nil {
		return nil, err
	}
	s["items"] = child
	return s, checkCount(s, path, "minItems", "maxItems")
}

func (n *normalizer) object(s map[string]any, path string, depth int) (map[string]any, error) {
	props := map[string]any{}
	if raw, ok := s["properties"]; ok {
		p, isMap := raw.(map[string]any)
		if !isMap {
			return nil, ErrSchemaViolation.Wrapf("%s: properties must be an object", path)
		}
		names := make([]string, 0, len(p))
		for name := range p {
			names = append(names, name)
		}
		sort.Strings(names) // notes and the first error come out in one order on every run
		for _, name := range names {
			child, isMap := p[name].(map[string]any)
			if !isMap {
				return nil, ErrSchemaViolation.Wrapf("%s.properties.%s: must be a schema", path, name)
			}
			out, err := n.node(child, path+".properties."+name, depth+1, false)
			if err != nil {
				return nil, err
			}
			props[name] = out
		}
	}
	if raw, ok := s["required"]; ok {
		names, isList := raw.([]any)
		if !isList {
			return nil, ErrSchemaViolation.Wrapf("%s: required must be a list", path)
		}
		for _, e := range names {
			if _, isString := e.(string); !isString {
				return nil, ErrSchemaViolation.Wrapf("%s: required must list property names", path)
			}
		}
		if len(names) == 0 {
			delete(s, "required")
		} else {
			s["required"] = names
		}
	}
	switch ap := s["additionalProperties"].(type) {
	case nil:
		if _, present := s["additionalProperties"]; present {
			return nil, ErrSchemaViolation.Wrapf("%s: additionalProperties must be false", path)
		}
		// Declared properties close an object, as they always have here; saying so is what strict
		// decoders require. No declaration is an open object.
		if len(props) > 0 {
			s["additionalProperties"] = false
		}
	case bool:
		if ap {
			if len(props) > 0 {
				return nil, ErrSchemaViolation.Wrapf("%s: additionalProperties true beside declared properties is not supported", path)
			}
			delete(s, "additionalProperties")
		}
	default:
		// A schema here is a map, whose entries the writer names: no form lists its fields and
		// strict tool calling refuses it. Named entries are a list of objects carrying the name.
		return nil, ErrSchemaViolation.Wrapf("%s: additionalProperties must be false: named entries are a list of objects, each carrying its name", path)
	}
	// Declared properties close an object (above), so an open one declares none.
	if _, closed := s["additionalProperties"]; !closed {
		delete(s, "properties")
		return s, nil
	}
	// A closed object requiring a name it does not declare admits no value at all.
	names, _ := s["required"].([]any)
	for _, name := range names {
		if _, declared := props[name.(string)]; !declared {
			return nil, ErrSchemaViolation.Wrapf("%s: required names %q, which is not a declared property", path, name)
		}
	}
	s["properties"] = props
	return s, nil
}

// typeName names a type keyword's value for a message.
func typeName(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "untyped"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// numberOf reads a JSON number, however it was decoded.
func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// validateSchemaDescriptions returns an error if any named property in the schema
// (recursively) is missing a non-empty description. Called at activation to ensure
// schemas are usable for lookup and LLM function calling (§3.1).
func validateSchemaDescriptions(schema map[string]any, path string) error {
	props, _ := schema["properties"].(map[string]any)
	for name, raw := range props {
		child, _ := raw.(map[string]any)
		desc, _ := child["description"].(string)
		if strings.TrimSpace(desc) == "" {
			return ErrSchemaViolation.Wrapf("property %s.%s: description is required for activation", path, name)
		}
		if err := validateSchemaDescriptions(child, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

// checkAnnotationValues refuses a default or an example its own node refuses: a form starts a field
// from its default and a model copies an example, so either one must be a value the action accepts.
// It runs on the canonical form, where a nullable node already admits null.
func checkAnnotationValues(s map[string]any, path string) error {
	if d, ok := s["default"]; ok {
		if err := validateValue(s, d, path+" default"); err != nil {
			return err
		}
	}
	examples, _ := s["examples"].([]any)
	for _, e := range examples {
		if err := validateValue(s, e, path+" example"); err != nil {
			return err
		}
	}
	props, _ := s["properties"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(props)) {
		if err := checkAnnotationValues(props[name].(map[string]any), path+".properties."+name); err != nil {
			return err
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		return checkAnnotationValues(items, path+".items")
	}
	return nil
}

// ValidateInput checks that data conforms to a canonical schema.
// Returns ErrSchemaViolation with a descriptive message on mismatch.
func ValidateInput(schema map[string]any, data any) error {
	return validateValue(schema, data, "")
}

// fieldPath names a field inside a value: the top level has no name of its own, so its fields are
// named as the caller wrote them rather than under a symbol for the document.
func fieldPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// jsonKind names a value the way the person who wrote it would: JSON has seven kinds and Go's
// type names (float64, map[string]interface {}) are not among them (§14).
func jsonKind(data any) string {
	switch data.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64, int, int64, json.Number:
		return "a number"
	case map[string]any:
		return "an object"
	case []any:
		return "a list"
	default:
		return "something else"
	}
}

// schemaType reads a canonical type keyword: a type name, or [T, "null"].
func schemaType(raw any) (string, bool) {
	switch t := raw.(type) {
	case string:
		return t, false
	case []any:
		name, nullable := "", false
		for _, e := range t {
			if s, _ := e.(string); s == "null" {
				nullable = true
			} else {
				name = s
			}
		}
		return name, nullable
	}
	return "", false
}

func validateValue(schema map[string]any, data any, path string) error {
	raw, hasType := schema["type"]
	if !hasType {
		return nil // an untyped schema (the empty one included) accepts any value
	}
	t, nullable := schemaType(raw)
	if data == nil && !nullable {
		return ErrSchemaViolation.Wrapf("field %s: must not be null", path)
	}

	// null passes a nullable type; an enum, if any, must still list it.
	if data != nil {
		if err := validateTyped(schema, t, data, path); err != nil {
			return err
		}
	}

	// Enum membership runs after type validation so a value of the wrong JSON type is rejected
	// first (numeric 1 must not satisfy {type:string, enum:["1"]}).
	if enums, ok := schema["enum"].([]any); ok {
		for _, e := range enums {
			if e == data {
				return nil
			}
		}
		return ErrSchemaViolation.Wrapf("field %s: value not in enum", path)
	}
	return nil
}

// validateTyped checks a non-null value against its type and that type's constraints.
func validateTyped(schema map[string]any, t string, data any, path string) error {
	switch t {
	case "object":
		obj, ok := data.(map[string]any)
		if !ok {
			return ErrSchemaViolation.Wrapf("field %s: expected object, got %s", path, jsonKind(data))
		}
		if err := validateObject(schema, obj, path); err != nil {
			return err
		}

	case "array":
		arr, ok := data.([]any)
		if !ok {
			return ErrSchemaViolation.Wrapf("field %s: expected array, got %s", path, jsonKind(data))
		}
		if err := validateArray(schema, arr, path); err != nil {
			return err
		}

	case "string":
		s, ok := data.(string)
		if !ok {
			return ErrSchemaViolation.Wrapf("field %s: expected string, got %s", path, jsonKind(data))
		}
		if err := validateString(schema, s, path); err != nil {
			return err
		}

	case "integer", "number":
		f, ok := numberOf(data)
		if !ok {
			return ErrSchemaViolation.Wrapf("field %s: expected %s, got %s", path, t, jsonKind(data))
		}
		if t == "integer" && f != math.Trunc(f) {
			return ErrSchemaViolation.Wrapf("field %s: expected integer, got fractional number", path)
		}
		// I-JSON (RFC 7493 §2.2): an integer beyond ±(2⁵³−1) is not carried exactly by JSON
		// implementations, so it may already have been rounded; it is refused rather than checked.
		if t == "integer" && math.Abs(f) > maxExactInteger {
			return ErrSchemaViolation.Wrapf("field %s: integer beyond ±%d, the range JSON carries exactly", path, int64(maxExactInteger))
		}
		if err := validateNumber(schema, f, path); err != nil {
			return err
		}

	case "boolean":
		if _, ok := data.(bool); !ok {
			return ErrSchemaViolation.Wrapf("field %s: expected boolean, got %s", path, jsonKind(data))
		}
	}
	return nil
}

func validateObject(schema, obj map[string]any, path string) error {
	props, _ := schema["properties"].(map[string]any)
	reqSet := map[string]bool{}
	switch v := schema["required"].(type) {
	case []string:
		for _, s := range v {
			reqSet[s] = true
		}
	case []any:
		for _, r := range v {
			if s, ok := r.(string); ok {
				reqSet[s] = true
			}
		}
	}
	names := make([]string, 0, len(reqSet))
	for name := range reqSet {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, present := obj[name]; !present {
			return ErrSchemaViolation.Wrapf("field %s: required field missing", fieldPath(path, name))
		}
	}
	for name, raw := range props {
		if val, present := obj[name]; present {
			child, _ := raw.(map[string]any)
			if err := validateValue(child, val, fieldPath(path, name)); err != nil {
				return err
			}
		}
	}
	// A key nothing declares: declared properties or additionalProperties false close the object,
	// and an object declaring neither is open.
	_, closed := schema["additionalProperties"]
	closed = closed || len(props) > 0
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, declared := props[k]; declared || reqSet[k] {
			continue
		}
		if closed {
			return ErrSchemaViolation.Wrapf("field %s: undeclared key %q", path, k)
		}
	}
	return nil
}

func validateArray(schema map[string]any, arr []any, path string) error {
	if min, ok := numberOf(schema["minItems"]); ok && float64(len(arr)) < min {
		return ErrSchemaViolation.Wrapf("field %s: needs at least %v items", path, min)
	}
	if max, ok := numberOf(schema["maxItems"]); ok && float64(len(arr)) > max {
		return ErrSchemaViolation.Wrapf("field %s: allows at most %v items", path, max)
	}
	items, _ := schema["items"].(map[string]any)
	for i, elem := range arr {
		if err := validateValue(items, elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func validateString(schema map[string]any, s, path string) error {
	n := float64(utf8.RuneCountInString(s))
	if min, ok := numberOf(schema["minLength"]); ok && n < min {
		return ErrSchemaViolation.Wrapf("field %s: needs at least %v characters", path, min)
	}
	if max, ok := numberOf(schema["maxLength"]); ok && n > max {
		return ErrSchemaViolation.Wrapf("field %s: allows at most %v characters", path, max)
	}
	if p, ok := schema["pattern"].(string); ok {
		re, err := regexp.Compile(p)
		if err != nil {
			return ErrSchemaViolation.Wrapf("field %s: pattern does not compile", path)
		}
		if !re.MatchString(s) {
			return ErrSchemaViolation.Wrapf("field %s: does not match pattern %s", path, p)
		}
	}
	return nil
}

func validateNumber(schema map[string]any, f float64, path string) error {
	if v, ok := numberOf(schema["minimum"]); ok && f < v {
		return ErrSchemaViolation.Wrapf("field %s: must be at least %v", path, v)
	}
	if v, ok := numberOf(schema["maximum"]); ok && f > v {
		return ErrSchemaViolation.Wrapf("field %s: must be at most %v", path, v)
	}
	if v, ok := numberOf(schema["exclusiveMinimum"]); ok && f <= v {
		return ErrSchemaViolation.Wrapf("field %s: must be greater than %v", path, v)
	}
	if v, ok := numberOf(schema["exclusiveMaximum"]); ok && f >= v {
		return ErrSchemaViolation.Wrapf("field %s: must be less than %v", path, v)
	}
	if v, ok := numberOf(schema["multipleOf"]); ok && v > 0 && !isMultiple(f, v) {
		return ErrSchemaViolation.Wrapf("field %s: must be a multiple of %v", path, v)
	}
	return nil
}

// isMultiple reports whether f is an exact multiple of m, reading each as the decimal it was written
// as: 0.3 is a multiple of 0.1, 0.30000000001 is not. Binary floating point would get the first
// wrong, and a tolerance would admit the second.
func isMultiple(f, m float64) bool {
	x, okx := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	y, oky := new(big.Rat).SetString(strconv.FormatFloat(m, 'g', -1, 64))
	if !okx || !oky || y.Sign() == 0 {
		return false
	}
	return new(big.Rat).Quo(x, y).IsInt()
}
