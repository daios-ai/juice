package rail

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Manual is the rail of a world with no chain. The operator's own records are the finalized facts:
// they paid somebody, or somebody paid them, and they say so. That makes every money rule in the
// kernel run unchanged in a world where no crypto exists — which is what lets somebody try Juice
// with a handle and a password and nothing else (D23).
//
// It is stateless. What must not be recorded twice is recorded by the kernel's own row, keyed by
// the reference the operator gave; and a payment here is final the moment it is made, so there is
// nothing to remember about it either — after a restart the answer is the same as before.
type Manual struct{}

// NewManual builds the manual rail.
func NewManual() *Manual { return &Manual{} }

// Ready is always true: there is nothing outside to verify.
func (m *Manual) Ready(context.Context) error { return nil }

// Address is empty: money here has no address, only the operator's word.
func (m *Manual) Address() string { return "" }

// Destination is the account itself. The operator pays outside the system and records that they did.
func (m *Manual) Destination(string) (string, error) { return "", nil }

// Pay confirms at once. There is no window between authorizing a payment and it being final,
// because the operator's act of recording it is what makes it final.
func (m *Manual) Pay(_ context.Context, id, _ string, _ int64) (kernel.RailOutcome, error) {
	return kernel.RailOutcome{TxHash: "manual:" + id}, nil
}

// Outcome reports every payment as confirmed. The kernel asks only about a row it wrote after Pay
// returned, and Pay here never fails, so the answer holds across a restart as much as within one —
// which is what lets a row left submitted by a crash finalize on the next pass.
func (m *Manual) Outcome(_ context.Context, id string) (kernel.RailStatus, kernel.RailFact, error) {
	return kernel.RailConfirmed, kernel.RailFact{TxHash: "manual:" + id, Exec: true}, nil
}

// Refill never applies: nothing here burns fuel, so nothing is ever short of it.
func (m *Manual) Refill(context.Context, int64) (kernel.RailRefill, error) {
	return kernel.RailRefill{}, kernel.ErrInvalidState.Wrap("this world has no fuel to buy")
}

// FindRefill finds nothing, for the same reason.
func (m *Manual) FindRefill(context.Context, func(string) bool) (kernel.RailRefill, bool, error) {
	return kernel.RailRefill{}, false, nil
}

// RefillCost never applies: nothing here burns fuel.
func (m *Manual) RefillCost(context.Context, string) (int64, kernel.RailStatus, error) {
	return 0, kernel.RailUnknown, kernel.ErrNotFound.Wrap("this world has no fuel to buy")
}

// ScanDeposits finds nothing on its own: money arrives here only when the operator says it has.
func (m *Manual) ScanDeposits(context.Context, uint64) ([]kernel.RailDeposit, error) { return nil, nil }

// Witness turns the operator's reference into the fact it names, which is what a world with no chain
// has instead of a finalized transaction. The reference is required, and it is what makes the record
// idempotent: without one, a repeated command would mint money. It is also the payment's only name
// here, so it is reported as the transaction that carried it.
func (m *Manual) Witness(_ context.Context, ref string, amount int64) (kernel.RailDeposit, error) {
	if ref == "" {
		return kernel.RailDeposit{}, kernel.ErrInvalidInput.Wrap("name the payment being recorded")
	}
	if amount <= 0 {
		return kernel.RailDeposit{}, kernel.ErrInvalidInput.Wrap("amount must be positive")
	}
	return kernel.RailDeposit{Key: "rail:ref:" + ref, TxHash: ref, Amount: amount}, nil
}

// FinalizedBalances reports nothing to compare against: there is no outside to read.
func (m *Manual) FinalizedBalances(context.Context) (int64, string, uint64, bool, error) {
	return 0, "", 0, false, nil
}

// DepositsScannedTo has no cursor: nothing is scanned.
func (m *Manual) DepositsScannedTo() (uint64, bool, error) { return 0, false, nil }

// Sign produces nothing: with no address there is nothing to prove control of.
func (m *Manual) Sign([]byte) (string, error) { return "", nil }

// Verify accepts only the absence of an address. A kernel or a user claiming one on a world that has
// none is claiming something this rail could never check.
func (m *Manual) Verify(_ []byte, address, sig string) (string, error) {
	if address != "" || sig != "" {
		return "", kernel.ErrInvalidState.Wrap("this world has no payment addresses")
	}
	return "", nil
}
