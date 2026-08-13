package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// RegisterLookupHandler registers the @sys/lookup native action handler on k.
func RegisterLookupHandler(k *kernel.Kernel) {
	k.RegisterNativeHandler("lookup", func(ctx context.Context, args map[string]any, _, callerID, _, _, _ string) (map[string]any, error) {
		return executeLookup(ctx, args, callerID, k)
	})
}

func executeLookup(ctx context.Context, args map[string]any, subjectID string, k *kernel.Kernel) (map[string]any, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, kernel.ErrInvalidInput.Wrap("lookup requires query argument")
	}
	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}
	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: query, Limit: limit, CallerID: subjectID})
	if err != nil {
		return nil, err
	}
	items := make([]any, len(results))
	for i, r := range results {
		// A discovered hit (§13) renders its STABLE identity — remote action id, kernel-qualified
		// reference by raw key — so selecting it resolves from the home kernel; a local hit its own row.
		var actionID, ref, description string
		var in, out map[string]any
		if d := r.Discovered; d != nil {
			actionID, ref, description, in, out = d.ActionID, d.Handle+"@"+d.KernelPublicKey+"/"+d.Name, d.Description, d.InputSchema, d.OutputSchema
		} else {
			actionID, ref, description, in, out = r.Action.ID, kernel.FormatActionRef(r.Action), r.Action.Description, r.Action.InputSchema, r.Action.OutputSchema
		}
		items[i] = map[string]any{
			"action_id":     actionID,
			"action":        ref,
			"description":   description,
			"price":         r.Price,
			"score":         float64(r.Score),
			"input_schema":  in,
			"output_schema": out,
			"quote_hash":    r.QuoteHash,
		}
	}
	return map[string]any{"results": items}, nil
}
