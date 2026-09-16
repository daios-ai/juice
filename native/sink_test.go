// SPDX-License-Identifier: AGPL-3.0-only

package native

import "testing"

func TestExecuteSink_ReturnsEmpty(t *testing.T) {
	result, err := executeSink()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}
