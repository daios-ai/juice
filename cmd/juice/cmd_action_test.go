package main

import (
	"context"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestActionCreateAndToggle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@actowner", Email: "actowner@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	a, err := env.k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/cli-action",
		Kind:        kernel.KindHTTP,
		Source:      "http://example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Active {
		t.Error("new action should be inactive")
	}

	if err := env.k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	updated, _ := env.k.ReadAction(ctx, a.ID)
	if !updated.Active {
		t.Error("action should be active after enable")
	}

	if err := env.k.SetActive(ctx, owner.ID, a.ID, false); err != nil {
		t.Fatal(err)
	}
	updated2, _ := env.k.ReadAction(ctx, a.ID)
	if updated2.Active {
		t.Error("action should be inactive after disable")
	}
}

func TestActionACLGrantRevoke(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@owner2", Email: "o2@example.com", Password: "pass",
	})
	other, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@other", Email: "other@example.com", Password: "pass",
	})

	a, _ := env.k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "/acl-test",
		Kind: kernel.KindHTTP, Source: "http://example.com",
	})

	if err := env.k.GrantACL(ctx, other.ID, a.ID, kernel.PermCall, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.k.RevokeACL(ctx, other.ID, a.ID, kernel.PermCall, owner.ID); err != nil {
		t.Fatal(err)
	}
}

func TestActionPriceUpdateDeactivates(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@priceowner", Email: "price@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := env.k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/priced",
		Kind:        kernel.KindHTTP,
		Price:       10,
		Source:      "http://example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}

	price := int64(20)
	updated, err := env.k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{
		ID:    a.ID,
		Price: &price,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Active {
		t.Fatal("price update should deactivate action")
	}
	if updated.Price != price {
		t.Fatalf("price: got %d, want %d", updated.Price, price)
	}
}

func TestActionDelete(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@delowner", Email: "del@example.com", Password: "pass",
	})
	a, _ := env.k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "/to-delete",
		Kind: kernel.KindHTTP, Source: "http://example.com",
	})
	if err := env.k.DeleteAction(ctx, owner.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.k.ReadAction(ctx, a.ID); err == nil {
		t.Error("expected error reading deleted action")
	}
}

func TestActionListActive(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@listowner", Email: "list@example.com", Password: "pass",
	})
	a, _ := env.k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "/listed",
		Kind: kernel.KindHTTP, Source: "http://example.com",
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	actions, err := env.k.ListActions(ctx, true, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, act := range actions {
		if act.ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Error("active action should appear in ListActions")
	}
}
