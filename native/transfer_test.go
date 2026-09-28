// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

func TestTransferValue(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		wantErr bool
		amount  int64
	}{
		{"ok", map[string]any{"target": "bob@k", "amount": float64(100)}, false, 100},
		{"missing target", map[string]any{"amount": float64(100)}, true, 0},
		{"missing amount", map[string]any{"target": "bob@k"}, true, 0},
		{"zero amount", map[string]any{"target": "bob@k", "amount": float64(0)}, true, 0},
		{"negative amount", map[string]any{"target": "bob@k", "amount": float64(-5)}, true, 0},
		{"fractional amount", map[string]any{"target": "bob@k", "amount": float64(1.5)}, true, 0},
		{"non-numeric amount", map[string]any{"target": "bob@k", "amount": "100"}, true, 0},
		{"largest exact amount", map[string]any{"target": "bob@k", "amount": float64(1 << 53)}, false, 1 << 53},
		{"beyond exact range", map[string]any{"target": "bob@k", "amount": float64(1<<53) * 2}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			amount, _, err := transferValue(c.args)
			if c.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !c.wantErr && (err != nil || amount != c.amount) {
				t.Fatalf("got amount=%d err=%v, want %d nil", amount, err, c.amount)
			}
		})
	}
}

// seedNativeAction creates a kind=native action owned by ownerID with the given price and effect
// (effect "transfer" makes it value-bearing; "" for an ordinary native).
func seedNativeAction(t *testing.T, st kernel.Store, ownerID, name string, price int64, effect string) {
	t.Helper()
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: ownerID, Name: name, Kind: kernel.KindNative,
		Price: price, Effect: effect, Active: true, Visibility: kernel.VisibilityPublic, Description: "transfer credits",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatalf("seedNativeAction %s: %v", name, err)
	}
}

// TestTransferLocal exercises a same-kernel transfer through Run (run sys/transfer): the caller is
// debited and the beneficiary credited in the settling commit.
func TestTransferLocal(t *testing.T) {
	k, db := newLookupTestKernel(t)
	Register(k, []Spec{Transfer()})
	ctx := context.Background()
	sys := seedOwner(t, db, "sys")
	seedNativeAction(t, db, sys.ID, "transfer", 0, "transfer")
	alice := seedUserWithBalance(t, db, "alice", 1000)
	bob := seedUserWithBalance(t, db, "bob", 0)

	reply, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "sys@k/transfer", Args: map[string]any{"target": "bob@k", "amount": float64(100)}})
	if err != nil {
		t.Fatalf("run sys/transfer: %v", err)
	}
	if reply == nil {
		t.Fatal("nil reply")
	}
	if a, _ := db.ReadUser(ctx, alice.ID); a.Available != 900 {
		t.Errorf("alice available: got %d, want 900", a.Available)
	}
	if b, _ := db.ReadUser(ctx, bob.ID); b.Available != 100 {
		t.Errorf("bob available: got %d, want 100", b.Available)
	}

	// A beneficiary on another kernel must be resolved there before anything is charged (P11): with
	// no federation to ask, the run is refused and alice keeps her balance.
	if _, err := k.BindPetname(ctx, base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "other", true); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: alice.ID, ActionRef: "sys@k/transfer", Args: map[string]any{"target": "bob@other", "amount": float64(10)}}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unresolvable remote target: got %v, want ErrNotFound", err)
	}
	if a, _ := db.ReadUser(ctx, alice.ID); a.Available != 900 || a.Locked != 0 {
		t.Errorf("alice after a refused remote transfer: %d available, %d locked, want 900/0", a.Available, a.Locked)
	}
	// Insufficient balance is rejected atomically (bob has 100, tries to send 200).
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: bob.ID, ActionRef: "sys@k/transfer", Args: map[string]any{"target": "alice@k", "amount": float64(200)}}); err == nil {
		t.Error("expected insufficient-funds rejection")
	}
	if a, _ := db.ReadUser(ctx, alice.ID); a.Available != 900 {
		t.Errorf("alice balance changed after failed transfer: %d", a.Available)
	}
}
