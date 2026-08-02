package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// RegisterTransferHandler wires sys/transfer: a value-bearing native that moves `amount` from the
// immediate caller to `target` (§13). A same-kernel target settles here as the existing atomic ledger
// transfer; a peer caller (an inbound federated transfer, target `sys@<kernel>/transfer`) has the
// delivered value settled by the receipt legs, so the handler only acknowledges. RegisterValueAction
// tells the kernel the action is value-bearing without the kernel ever naming it, keeping cross-kernel
// transfer encapsulated (native actions are never hardwired into the kernel).
func RegisterTransferHandler(k *kernel.Kernel) {
	k.RegisterValueAction("transfer", transferValue)
	k.RegisterNativeHandler("transfer", func(ctx context.Context, args map[string]any, _, callerID, _, _, _ string) (map[string]any, error) {
		amount, target, err := transferValue(args)
		if err != nil {
			return nil, err
		}
		caller, err := k.ResolveUser(ctx, callerID)
		if err != nil {
			return nil, err
		}
		if caller.IsPeer() {
			// Inbound serving leg: the delivered value is credited to the beneficiary at settlement.
			return map[string]any{"amount": amount}, nil
		}
		benef, err := k.ResolveUser(ctx, target)
		if err != nil || benef == nil {
			return nil, kernel.ErrNotFound.Wrapf("transfer target %q not found", target)
		}
		extKey, _ := args["external_key"].(string)
		e, err := k.Transfer(ctx, callerID, benef.ID, amount, "transfer", extKey)
		if err != nil {
			return nil, err
		}
		return map[string]any{"transfer_id": e.ID, "amount": amount}, nil
	})
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
