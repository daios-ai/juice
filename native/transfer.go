// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Transfer declares sys/transfer: a value-bearing native carrying the "transfer" effect (D18). The
// runtime handler only VALIDATES and acknowledges — it moves no balances. The value channel is a
// deferred TransferEffect staged at admission (the reserve locked from the immediate caller C) and
// committed atomically by the kernel at settlement: a local recipient is credited in that commit, a
// recipient on another kernel is paid through the rail and credited there on arrival (P11).
// Value binds the effect id → args extractor without the kernel ever naming the action, keeping the
// effect encapsulated (natives are never hardwired in).
func Transfer() Spec {
	return Spec{
		Name:        "transfer",
		Effect:      "transfer",
		Description: "Transfers credits from the caller to a user on this kernel or another. The amount is paid from the immediate caller's own balance and delivered whole: a recipient here is credited when the call settles, a recipient on another kernel when the payment reaches that kernel.",
		InputSchema: obj(map[string]any{
			"target": str("Recipient: an address handle@kernel"),
			"amount": integer("Amount of credits to transfer (positive integer)"),
		}, "target", "amount"),
		OutputSchema: obj(map[string]any{"amount": integer("Amount transferred")}, "amount"),
		Value:        transferValue,
		Handler: func(Host) kernel.NativeFunc {
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

// maxTransfer is 2^53, the largest integer a JSON number carries exactly: a larger amount would
// arrive rounded, and the sums the kernel forms from it stay far inside int64.
const maxTransfer = 1 << 53

// transferValue extracts (amount, target) from sys/transfer args. The amount must be a positive
// integer no larger than maxTransfer (JSON numbers arrive as float64, so a fractional value is rejected).
func transferValue(args map[string]any) (int64, string, error) {
	target, _ := args["target"].(string)
	if target == "" {
		return 0, "", kernel.ErrInvalidInput.Wrap("transfer requires target")
	}
	f, ok := args["amount"].(float64)
	if !ok || f < 1 || f > maxTransfer || f != float64(int64(f)) {
		return 0, "", kernel.ErrInvalidInput.Wrap("transfer amount must be a positive integer no larger than 2^53")
	}
	return int64(f), target, nil
}
