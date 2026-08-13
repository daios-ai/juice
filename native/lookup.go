package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Lookup declares @sys/lookup (§9): ranked search over callable actions, local and discovered.
func Lookup() Spec {
	return Spec{
		Name:        "lookup",
		Description: "Semantic search over active actions",
		InputSchema: searchQuerySchema(),
		OutputSchema: obj(map[string]any{
			"results": arrayOf(obj(map[string]any{
				"action_id":     str("Unique action identifier"),
				"action":        str("Action reference as owner/name"),
				"description":   str("Human-readable description of the action"),
				"price":         integer("All-in price the caller pays; indicative for a not-yet-resolved remote action"),
				"score":         num("Relevance score between 0 and 1"),
				"input_schema":  object("JSON Schema for the action's input"),
				"output_schema": object("JSON Schema for the action's output"),
				"quote_hash":    str("Fingerprint of the quoted terms; pin it on a run to be refused if they changed"),
			}), "Ranked list of matching actions"),
		}),
		Handler: func(k *kernel.Kernel) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, callerID, _, _, _ string) (map[string]any, error) {
				return executeLookup(ctx, args, callerID, k)
			}
		},
	}
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
