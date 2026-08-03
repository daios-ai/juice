package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestExecuteUserLookup_MissingQuery(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeUserLookup(context.Background(), map[string]any{}, "", k)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing query, got %v", err)
	}
}

// A public-action owner is a searchable local principal; the result carries the stable PrincipalID
// (kernel key + user id) and a display reference, never a bare raw id alone.
func TestExecuteUserLookup_FindsPublicActionOwner(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	owner := seedOwner(t, st, "weatherco")
	owner.Description = "provides weather forecasting services"
	if err := st.UpdateUser(ctx, owner); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	seedAction(t, st, owner.ID, "forecast", "weather forecast temperature rain")

	result, err := executeUserLookup(ctx, map[string]any{"query": "weatherco weather"}, owner.ID, k)
	if err != nil {
		t.Fatalf("executeUserLookup: %v", err)
	}
	items, ok := result["results"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected non-empty results, got %v", result)
	}
	var found map[string]any
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["handle"] == "weatherco" {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("expected weatherco among results, got %v", items)
	}
	pid, ok := found["principal_id"].(map[string]any)
	if !ok {
		t.Fatalf("result missing principal_id object, got %v", found)
	}
	if pid["user_id"] != owner.ID {
		t.Errorf("principal_id.user_id = %v, want %s", pid["user_id"], owner.ID)
	}
	// A local user's home kernel key is empty; the reference is the bare handle.
	if pid["kernel_public_key"] != "" {
		t.Errorf("local principal kernel_public_key should be empty, got %v", pid["kernel_public_key"])
	}
	if found["reference"] != "weatherco" {
		t.Errorf("reference = %v, want bare handle for a local user", found["reference"])
	}
}

// A non-owner local user (no active public action) is not surfaced by user-lookup.
func TestExecuteUserLookup_ExcludesNonOwner(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	lurker := seedOwner(t, st, "lurker")

	result, err := executeUserLookup(ctx, map[string]any{"query": "lurker"}, lurker.ID, k)
	if err != nil {
		t.Fatalf("executeUserLookup: %v", err)
	}
	items, _ := result["results"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["handle"] == "lurker" {
			t.Errorf("a user owning no active public action must not appear in user-lookup")
		}
	}
}
