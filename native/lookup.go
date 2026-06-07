package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// RegisterLookupHandler registers the @sys/lookup native action handler on k.
func RegisterLookupHandler(k *kernel.Kernel) {
	k.RegisterNativeHandler("lookup", func(ctx context.Context, args map[string]any, _, _, ownerUserID, _, _ string) (map[string]any, error) {
		return executeLookup(ctx, args, ownerUserID, k)
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
		items[i] = map[string]any{
			"action_id":    r.Action.ID,
			"name":         r.Action.Name,
			"owner_handle": r.OwnerHandle,
			"description":  r.Action.Description,
			"score":        float64(r.Score),
		}
	}
	return map[string]any{"results": items}, nil
}
