package native

import (
	"testing"
	"time"
)

func TestExecuteTime_ReturnsUnixAndISO(t *testing.T) {
	before := time.Now().UTC().Unix()
	result, err := executeTime()
	after := time.Now().UTC().Unix()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	unix, ok := result["unix"].(int64)
	if !ok {
		t.Fatalf("unix must be int64, got %T", result["unix"])
	}
	if unix < before || unix > after {
		t.Errorf("unix %d not in [%d, %d]", unix, before, after)
	}
	iso, ok := result["iso"].(string)
	if !ok {
		t.Fatalf("iso must be string, got %T", result["iso"])
	}
	if _, err := time.Parse(time.RFC3339, iso); err != nil {
		t.Errorf("iso %q does not parse as RFC 3339: %v", iso, err)
	}
}

func TestExecuteTime_ISOMatchesUnix(t *testing.T) {
	result, err := executeTime()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	unix := result["unix"].(int64)
	iso := result["iso"].(string)
	parsed, _ := time.Parse(time.RFC3339, iso)
	if parsed.Unix() != unix {
		t.Errorf("unix %d does not match iso %q (parsed unix %d)", unix, iso, parsed.Unix())
	}
}
