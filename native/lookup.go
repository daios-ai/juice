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
		if r.Discovered != nil {
			// A not-yet-resolved remote action (§13): render with its STABLE identity — the remote
			// action id and the kernel-qualified reference (raw key; a gossiped label never resolves).
			// Selecting it invokes it by reference, which resolves and verifies from the home kernel.
			d := r.Discovered
			items[i] = map[string]any{
				"action_id":     d.ActionID,
				"action":        d.Handle + "@" + d.KernelPublicKey + "/" + d.Name,
				"description":   d.Description,
				"price":         r.Price,
				"score":         float64(r.Score),
				"input_schema":  d.InputSchema,
				"output_schema": d.OutputSchema,
				// Keyed on the remote action id, so this equals the quote_hash of the local proxy
				// this hit resolves to: a hash read here binds a first cross-kernel call (§13).
				"quote_hash": kernel.QuoteHash(&kernel.Action{
					RemoteActionID: d.ActionID, Effect: d.Effect, Description: d.Description,
					InputSchema: d.InputSchema, OutputSchema: d.OutputSchema, Price: r.Price,
				}),
			}
			continue
		}
		items[i] = map[string]any{
			"action_id":     r.Action.ID,
			"action":        kernel.FormatActionRef(r.Action),
			"description":   r.Action.Description,
			"price":         r.Price,
			"score":         float64(r.Score),
			"input_schema":  r.Action.InputSchema,
			"output_schema": r.Action.OutputSchema,
			"quote_hash":    kernel.QuoteHash(r.Action),
		}
	}
	return map[string]any{"results": items}, nil
}
