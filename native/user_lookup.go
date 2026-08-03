package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// RegisterUserLookupHandler registers the @sys/user-lookup native action handler on k (§13). It is
// the user-facing twin of @sys/lookup: it searches local principals (sys + owners of active public
// actions) and discovered users, returning each hit's stable PrincipalID plus a display reference.
func RegisterUserLookupHandler(k *kernel.Kernel) {
	k.RegisterNativeHandler("user-lookup", func(ctx context.Context, args map[string]any, _, callerID, _, _, _ string) (map[string]any, error) {
		return executeUserLookup(ctx, args, callerID, k)
	})
}

func executeUserLookup(ctx context.Context, args map[string]any, subjectID string, k *kernel.Kernel) (map[string]any, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, kernel.ErrInvalidInput.Wrap("user-lookup requires query argument")
	}
	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}
	results, err := k.LookupUsers(ctx, kernel.LookupRequest{Query: query, Limit: limit, CallerID: subjectID})
	if err != nil {
		return nil, err
	}
	items := make([]any, len(results))
	for i, r := range results {
		items[i] = map[string]any{
			"principal_id":      map[string]any{"kernel_public_key": r.KernelPublicKey, "user_id": r.UserID},
			"reference":         r.Reference,
			"handle":            r.Handle,
			"description":       r.Description,
			"kernel_public_key": r.KernelPublicKey,
			"score":             float64(r.Score),
		}
	}
	return map[string]any{"results": items}, nil
}
