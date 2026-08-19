package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Transfer declares @sys/transfer: a value-bearing native carrying the "transfer" effect (§13). The
// runtime handler only VALIDATES and acknowledges — it moves no balances. The value channel is a
// deferred TransferEffect staged at admission (the reserve locked from the immediate caller C) and
// committed atomically by the kernel at settlement, so no non-atomic ledger write happens in-handler.
// Value binds the effect id → args extractor without the kernel ever naming the action, keeping the
// effect encapsulated (natives are never hardwired in).
func Transfer() Spec {
	return Spec{
		Name:        "transfer",
		Effect:      "transfer",
		Description: "Transfers credits from the caller to another user of this kernel. The value is funded from the immediate caller's own balance and delivered by a deferred, receipt-backed transfer effect. Value never crosses a kernel boundary: the target is always a local recipient.",
		InputSchema: obj(map[string]any{
			"target": str("Recipient: a handle or account id on this kernel"),
			"amount": integer("Amount of credits to transfer (positive integer)"),
		}, "target", "amount"),
		OutputSchema: obj(map[string]any{"amount": integer("Amount transferred")}, "amount"),
		Value:        transferValue,
		Handler: func(*kernel.Kernel) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				amount, _, err := transferValue(args)
				if err != nil {
					return nil, err
				}
				return map[string]any{"amount": amount}, nil
			}
		},
	}
}

// transferValue extracts (amount, target) from sys/transfer args. The amount must be a positive
// integer (JSON numbers arrive as float64, so a fractional value is rejected).
func transferValue(args map[string]any) (int64, string, error) {
	target, _ := args["target"].(string)
	if target == "" {
		return 0, "", kernel.ErrInvalidInput.Wrap("transfer requires target")
	}
	f, ok := args["amount"].(float64)
	if !ok || f < 1 || f != float64(int64(f)) {
		return 0, "", kernel.ErrInvalidInput.Wrap("transfer amount must be a positive integer")
	}
	return int64(f), target, nil
}
