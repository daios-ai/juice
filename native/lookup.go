package native

import (
	"context"
	"time"

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
				"quote_hash":    str("Fingerprint of the quoted terms; a run carries it back as consent to them"),
				"observed_at":   str("When this kernel last verified the authority's own description of a remote action (RFC 3339); absent for local actions, which this kernel is itself the authority for"),
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
		// A discovered hit (§13) renders its STABLE identity — the remote action id and a
		// kernel-qualified reference — so selecting it resolves from the home kernel; a local hit
		// renders its own row. The kernel qualifier goes through the one naming rule (§13): a bound
		// petname when this kernel has met the peer, its key otherwise, both of which resolve.
		// observed_at dates what this kernel last verified against the authority: the gossip
		// observation for a discovered action, the last reconcile for a resolved proxy. A local
		// action has none — this kernel IS its authority, so its row is never an observation.
		var actionID, ref, description, observedAt string
		var in, out map[string]any
		if d := r.Discovered; d != nil {
			actionID, description, in, out = d.ActionID, d.Description, d.InputSchema, d.OutputSchema
			ref = d.Handle + "@" + k.KernelName(ctx, d.KernelPublicKey) + "/" + d.Name
			observedAt = d.ObservedAt.UTC().Format(time.RFC3339)
		} else {
			actionID, ref, description, in, out = r.Action.ID, kernel.FormatActionRef(r.Action), r.Action.Description, r.Action.InputSchema, r.Action.OutputSchema
			if r.Action.Kind == kernel.KindRemoteProxy {
				observedAt = r.Action.UpdatedAt.UTC().Format(time.RFC3339)
			}
		}
		item := map[string]any{
			"action_id":     actionID,
			"action":        ref,
			"description":   description,
			"price":         r.Price,
			"score":         float64(r.Score),
			"input_schema":  in,
			"output_schema": out,
			"quote_hash":    r.QuoteHash,
		}
		if observedAt != "" {
			item["observed_at"] = observedAt
		}
		items[i] = item
	}
	return map[string]any{"results": items}, nil
}
