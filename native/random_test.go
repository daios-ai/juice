package native

import "testing"

func TestExecuteRandom(t *testing.T) {
	result, err := executeRandom()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, ok := result["value"].(float64)
	if !ok {
		t.Fatalf("expected float64 value, got %T", result["value"])
	}
	if v < 0 || v >= 1 {
		t.Errorf("value %v out of [0, 1)", v)
	}
}

func TestExecuteRandomDistinct(t *testing.T) {
	a, _ := executeRandom()
	b, _ := executeRandom()
	if a["value"] == b["value"] {
		t.Error("two consecutive calls returned the same value")
	}
}
