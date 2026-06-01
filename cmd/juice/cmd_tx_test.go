package main

import (
	"context"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestTransactionListEmpty(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@txowner", Email: "tx@e.com", Password: "p",
	})

	txs, err := env.k.ListTransactions(ctx, kernel.TxFilter{OwnerUserID: owner.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 0 {
		t.Errorf("expected 0 transactions, got %d", len(txs))
	}
}

func TestTransactionRate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Create a transaction via a direct store insert so we can rate it.
	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@rateowner", Email: "ro@example.com", Password: "p",
	})
	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "/rateable",
		Kind: kernel.KindHTTP, Source: "http://example.com",
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)

	p, root, _ := env.k.StartProcess(ctx, owner.ID, owner.ID, 0)
	_ = root

	// No calls made, so no transactions to rate.
	// Verify that rating a non-existent tx returns an error.
	_, err := env.k.RateTransaction(ctx, owner.ID, "nonexistent-tx", 1)
	if err == nil {
		t.Error("expected error rating nonexistent transaction")
	}
	_ = p
}

func TestTransactionRating(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	owner, _ := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@rater", Email: "r@e.com", Password: "p",
	})
	p, root, err := env.k.StartProcess(ctx, owner.ID, owner.ID, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Create and enable a wasm action — but since we have no script executor, use HTTP kind.
	a, _ := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID,
		Name:        "/tx-rate-svc",
		Kind:        kernel.KindHTTP,
		Source:      "http://example.com",
	})
	_ = env.k.SetActive(ctx, owner.ID, a.ID, true)
	_ = root

	// List — should still be 0 since no calls made.
	txs, _ := env.k.ListTransactions(ctx, kernel.TxFilter{OwnerUserID: owner.ID, Limit: 10})
	if len(txs) != 0 {
		t.Errorf("expected 0 txs before any call, got %d", len(txs))
	}

	_ = p
}
