package main

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestStatsInitializedOnActivation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@statsowner", Email: "s@e.com", Password: "p",
	})
	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         "svc",
		Kind:         kernel.KindHTTP,
		Source:       "http://x.com",
		Description:  "test action",
		InputSchema:  minSchema,
		OutputSchema: minSchema,
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

func TestLookupCommandUsesCall(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if err := env.k.FirstBoot(ctx, "sys-pass"); err != nil {
		t.Fatal(err)
	}
	if err := ensureSysLookup(ctx, env.k, "@sys"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.k.ReadUserByHandle(ctx, "@sys"); err != nil {
		t.Fatal(err)
	}
	user, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@lookup-cli", Email: "lookup-cli@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := env.k.Login(ctx, "@lookup-cli", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	// lookupCmd creates its own ephemeral process internally (no --process flag).
	_, err = runCmd(t, lookupCmd(), "--query", "weather")
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("expected lookup native action to fail without embedder, got %v", err)
	}

	// Verify the call was routed through a transaction.
	txs, err := env.k.ListTransactions(ctx, kernel.TxFilter{SubjectUserID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 1 {
		t.Fatalf("lookup command should route through Call and create one transaction, got %d", len(txs))
	}
	if txs[0].Status != kernel.TxFailure {
		t.Fatalf("lookup without embedder should create a failed transaction, got %s", txs[0].Status)
	}
	if _, err := env.k.GetReceiptByTxID(ctx, txs[0].ID); err != nil {
		t.Fatalf("lookup transaction should have a receipt: %v", err)
	}
}
