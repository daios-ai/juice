package kernel

import "context"

// RegisterLookupHandler registers the @sys/lookup native action handler on k.
func RegisterLookupHandler(k *Kernel) {
	k.RegisterNativeHandler("lookup", func(ctx context.Context, args map[string]any, subjectID, _, _ string) (map[string]any, error) {
		return executeLookup(k, ctx, args, subjectID)
	})
}

func executeLookup(k *Kernel, ctx context.Context, args map[string]any, subjectID string) (map[string]any, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, ErrInvalidInput.Wrap("lookup requires query argument")
	}
	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}
	results, err := k.Lookup(ctx, LookupRequest{Query: query, Limit: limit, SubjectID: subjectID})
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
