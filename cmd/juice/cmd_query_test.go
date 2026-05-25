package main

import (
	"context"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestStatsInitializedOnActivation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@statsowner", Email: "s@e.com", Password: "p",
	})
	a, _ := env.k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/svc",
		Kind:        kernel.KindHTTP,
		Source:      "http://x.com",
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	stats, err := env.k.ReadStats(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats == nil {
		t.Error("expected stats after activation")
	}
	if stats.Uses != 0 {
		t.Errorf("expected 0 uses for fresh action, got %d", stats.Uses)
	}
}

func TestLookupRequiresEmbedder(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.k.Lookup(ctx, kernel.LookupRequest{Query: "test", Limit: 5})
	if err == nil {
		t.Error("expected error when no embedder configured")
	}
}
