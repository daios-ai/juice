package rail_test

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/rail"
)

// On a world with no chain the operator's own record is the finalized fact, so a payment is final
// the moment it is made and there is no window in which it might still fail — and the answer does
// not depend on which process made it, so a row a crash left submitted finalizes after a restart.
func TestManualPaysAndConfirmsAtOnce(t *testing.T) {
	m := rail.NewManual()
	ctx := context.Background()

	if err := m.Ready(ctx); err != nil {
		t.Fatalf("the manual rail has nothing to verify: %v", err)
	}
	out, err := m.Pay(ctx, "row-1", "", 100)
	if err != nil || out.TxHash == "" || out.Blocked != "" {
		t.Fatalf("pay: %+v %v", out, err)
	}
	restarted := rail.NewManual()
	st, fact, err := restarted.Outcome(ctx, "row-1")
	if err != nil || st != kernel.RailConfirmed || !fact.Exec || fact.TxHash != out.TxHash {
		t.Fatalf("outcome after a restart: %s %+v %v", st, fact, err)
	}
}

// Every crossing names the payment it records. Without a name, repeating the command would mint
// money, so the reference is required rather than defaulted (U3).
func TestManualWitnessRequiresAReference(t *testing.T) {
	m := rail.NewManual()
	ctx := context.Background()

	if _, err := m.Witness(ctx, "", 10); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("an unnamed payment must be refused, got %v", err)
	}
	if _, err := m.Witness(ctx, "inv-1", 0); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("a payment of nothing must be refused, got %v", err)
	}
	d, err := m.Witness(ctx, "inv-1", 250)
	if err != nil {
		t.Fatal(err)
	}
	if d.Amount != 250 || d.Key == "" {
		t.Fatalf("witness: %+v", d)
	}
	// The same reference always names the same fact, which is what makes booking it idempotent.
	again, _ := m.Witness(ctx, "inv-1", 250)
	if again.Key != d.Key {
		t.Errorf("one reference must name one fact: %q vs %q", d.Key, again.Key)
	}
}

// This world has no payment addresses, so claiming one is claiming something nothing here could
// ever check.
func TestManualRefusesAddresses(t *testing.T) {
	m := rail.NewManual()

	if m.Address() != "" {
		t.Errorf("there is no vault here, got %q", m.Address())
	}
	if _, err := m.Verify(nil, "0xabc", "sig"); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("registering an address must be refused, got %v", err)
	}
	if _, err := m.Verify(nil, "", ""); err != nil {
		t.Errorf("having no address is the normal case here: %v", err)
	}
	if sig, err := m.Sign([]byte("anything")); err != nil || sig != "" {
		t.Errorf("there is no key to prove anything with: %q %v", sig, err)
	}
	// A withdrawal is paid to the account itself: the operator moves the money and records it.
	if dest, err := m.Destination(""); err != nil || dest != "" {
		t.Errorf("destination: %q %v", dest, err)
	}
}

// Nothing arrives on its own here, and there is no outside to audit against.
func TestManualObservesNothing(t *testing.T) {
	m := rail.NewManual()
	ctx := context.Background()

	if got, err := m.ScanDeposits(ctx, 0); err != nil || len(got) != 0 {
		t.Errorf("scan: %v %v", got, err)
	}
	if _, _, _, ok, err := m.FinalizedBalances(ctx); ok || err != nil {
		t.Errorf("there is nothing to read: ok=%v err=%v", ok, err)
	}
	if _, ok, err := m.DepositsScannedTo(); ok || err != nil {
		t.Errorf("there is no cursor: ok=%v err=%v", ok, err)
	}
	if _, _, err := m.RefillCost(ctx, "any"); err == nil {
		t.Error("nothing here burns fuel, so there is no cost to read")
	}
}

// The key a payment is listed under names that payment. Handing it back is what an operator does
// when reading it off `admin deposit`, and it must resolve to the same fact — not mint a second
// name for it, which is what recorded the money twice.
func TestManualWitnessAcceptsTheKeyItMinted(t *testing.T) {
	m := rail.NewManual()
	ctx := context.Background()

	first, err := m.Witness(ctx, "inv-7", 40)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Witness(ctx, first.Key, 40)
	if err != nil {
		t.Fatalf("the published key must name its own payment: %v", err)
	}
	if again.Key != first.Key {
		t.Fatalf("the key nested instead of resolving: %q became %q", first.Key, again.Key)
	}
	if again.TxHash != first.TxHash || again.Amount != first.Amount {
		t.Errorf("the same key named a different fact: %+v vs %+v", again, first)
	}
}
