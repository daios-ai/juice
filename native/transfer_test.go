package native

import (
	"context"
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
		{"ok", map[string]any{"target": "bob", "amount": float64(100)}, false, 100},
		{"missing target", map[string]any{"amount": float64(100)}, true, 0},
		{"missing amount", map[string]any{"target": "bob"}, true, 0},
		{"zero amount", map[string]any{"target": "bob", "amount": float64(0)}, true, 0},
		{"negative amount", map[string]any{"target": "bob", "amount": float64(-5)}, true, 0},
		{"fractional amount", map[string]any{"target": "bob", "amount": float64(1.5)}, true, 0},
		{"non-numeric amount", map[string]any{"target": "bob", "amount": "100"}, true, 0},
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

// seedNativeAction creates a kind=native action owned by ownerID with the given price.
func seedNativeAction(t *testing.T, st kernel.Store, ownerID, name string, price int64) {
	t.Helper()
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: ownerID, Name: name, Kind: kernel.KindNative,
		Price: price, Active: true, Visibility: kernel.VisibilityPublic, Description: "transfer credits",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatalf("seedNativeAction %s: %v", name, err)
	}
}

// TestTransferLocal exercises a same-kernel transfer through Call() (run sys/transfer): the caller is
// debited, the beneficiary credited, and a ledger entry recorded — no fee, no value machinery.
func TestTransferLocal(t *testing.T) {
	k, db := newLookupTestKernel(t)
	RegisterTransferHandler(k)
	ctx := context.Background()
	sys := seedOwner(t, db, "sys")
	seedNativeAction(t, db, sys.ID, "transfer", 0)
	alice := seedUserWithBalance(t, db, "alice", 1000)
	bob := seedUserWithBalance(t, db, "bob", 0)

	reply, err := k.Run(ctx, alice.ID, "sys/transfer", map[string]any{"target": "bob", "amount": float64(100)})
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

	// A kernel-qualified target on the local action is rejected — cross-kernel transfers are addressed
	// as sys@<kernel>/transfer, not sys/transfer with a remote target.
	if _, err := k.Run(ctx, alice.ID, "sys/transfer", map[string]any{"target": "bob@other", "amount": float64(10)}); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("kernel-qualified target: got %v, want ErrInvalidInput", err)
	}
	// Insufficient balance is rejected atomically (bob has 100, tries to send 200).
	if _, err := k.Run(ctx, bob.ID, "sys/transfer", map[string]any{"target": "alice", "amount": float64(200)}); err == nil {
		t.Error("expected insufficient-funds rejection")
	}
	if a, _ := db.ReadUser(ctx, alice.ID); a.Available != 900 {
		t.Errorf("alice balance changed after failed transfer: %d", a.Available)
	}
}
