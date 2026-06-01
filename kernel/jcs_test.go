package kernel

import (
	"encoding/json"
	"testing"
)

func TestCanonicalJSONSortsKeys(t *testing.T) {
	input := map[string]any{"z": 1, "a": 2, "m": 3}
	out, err := CanonicalJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	// Verify the raw bytes have keys in sorted order.
	expected := `{"a":2,"m":3,"z":1}`
	if string(out) != expected {
		t.Errorf("got %s, want %s", out, expected)
	}
}

func TestCanonicalJSONNestedMaps(t *testing.T) {
	input := map[string]any{
		"outer": map[string]any{"z": 1, "a": 2},
		"key":   "value",
	}
	out, err := CanonicalJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"key":"value","outer":{"a":2,"z":1}}`
	if string(out) != expected {
		t.Errorf("got %s, want %s", out, expected)
	}
}

func TestCanonicalJSONArray(t *testing.T) {
	input := []any{3, 1, 2}
	out, err := CanonicalJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	// Arrays preserve order.
	expected := `[3,1,2]`
	if string(out) != expected {
		t.Errorf("got %s, want %s", out, expected)
	}
}

func TestJCSHashStrEmpty(t *testing.T) {
	h, err := jcsHashStr("")
	if err != nil {
		t.Fatal(err)
	}
	if h == "" {
		t.Error("expected non-empty hash")
	}
}

func TestCanonicalJSONIntegerFloats(t *testing.T) {
	// Integer-valued floats must serialize without decimal point (RFC 8785).
	input := map[string]any{"n": float64(42), "big": float64(1e14)}
	out, err := CanonicalJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"big":100000000000000,"n":42}`
	if string(out) != expected {
		t.Errorf("got %s, want %s", out, expected)
	}
}

func TestJCSHashStrDeterministic(t *testing.T) {
	// Same JSON with different key order must produce the same hash.
	a := `{"z":1,"a":2}`
	b := `{"a":2,"z":1}`
	ha, err := jcsHashStr(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := jcsHashStr(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("hash should be key-order-independent: %s != %s", ha, hb)
	}
}
