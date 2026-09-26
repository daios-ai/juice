// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"encoding/json"
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
				"action_id":              str("Unique action identifier"),
				"action":                 str("Action address as owner@kernel/name"),
				"description":            str("Human-readable description of the action"),
				"price":                  integer("All-in price the caller pays; indicative for a not-yet-resolved remote action"),
				"score":                  num("Relevance score between 0 and 1"),
				"input_schema":           object("JSON Schema for the action's input"),
				"output_schema":          object("JSON Schema for the action's output"),
				"quote_hash":             str("Fingerprint of the quoted terms; a run carries it back as consent to them"),
				"evidence":               object("What this kernel holds about the action's conduct: its own calls, the provider's own signed report, and other kernels' accounts of trading with it — each labelled, never summed, never scored"),
				"observed_at":            str("When this kernel last verified the authority's own description of a remote action (RFC 3339); absent for local actions, which this kernel is itself the authority for"),
				"last_seen":              str("When this kernel last reached the hosting kernel (RFC 3339); absent for local actions and until a first contact"),
				"last_contact_failed_at": str("When contact with the hosting kernel last failed (RFC 3339); later than last_seen means recent attempts are failing. Absent for local actions and until a first failure"),
			}), "Ranked list of matching actions"),
		}),
		Handler: func(k Host) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, callerID, _, _, _ string) (map[string]any, error) {
				return executeLookup(ctx, args, callerID, k)
			}
		},
	}
}

// asJSONObject renders a value the way the reply will carry it. A native's result is checked
// against its own output schema before it is paid for (U12), and that check is made on JSON
// values, not on Go types, so a struct is rendered here rather than at the boundary.
func asJSONObject(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func executeLookup(ctx context.Context, args map[string]any, subjectID string, k Host) (map[string]any, error) {
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
	names := k.NewNames()
	for i, r := range results {
		// A discovered hit (§13) renders its STABLE identity — the remote action id and a
		// kernel-qualified reference — so selecting it resolves from the home kernel; a local hit
		// renders its own row. The kernel qualifier goes through the one naming rule (§13): a bound
		// petname when this kernel has met the peer, its key otherwise, both of which resolve.
		// observed_at dates what this kernel last verified against the authority: the gossip
		// observation for a discovered action, the last reconcile for a resolved proxy. A local
		// action has none — this kernel IS its authority, so its row is never an observation.
		// Each hit also carries the record a person reads on the action itself, so an agent
		// choosing between candidates weighs what a person would (U10, U46). Ranking stays
		// relevance only: a rank by evidence would be the opaque score U39 excludes.
		var actionID, ref, description, observedAt string
		var in, out map[string]any
		var record *kernel.ActionRecord
		if d := r.Discovered; d != nil {
			actionID, description, in, out = d.ActionID, d.Description, d.InputSchema, d.OutputSchema
			ref = kernel.Address{Handle: d.Handle, Kernel: k.KernelName(ctx, d.KernelPublicKey), Name: d.Name}.String()
			observedAt = d.ObservedAt.UTC().Format(time.RFC3339)
			record = k.DiscoveredRecord(ctx, d)
		} else {
			actionID, ref, description, in, out = r.Action.ID, names.Action(ctx, r.Action), r.Action.Description, r.Action.InputSchema, r.Action.OutputSchema
			if r.Action.Kind == kernel.KindRemoteProxy {
				observedAt = r.Action.UpdatedAt.UTC().Format(time.RFC3339)
			}
			record = k.ActionRecord(ctx, r.Action)
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
			"evidence":      asJSONObject(record),
		}
		if observedAt != "" {
			item["observed_at"] = observedAt
		}
		// Reachability of the hosting kernel as raw facts, never a verdict: the caller compares them
		// and decides what counts as stale. Absent until the corresponding contact has happened.
		if h := r.Host; h != nil {
			if h.LastSeen != nil {
				item["last_seen"] = h.LastSeen.UTC().Format(time.RFC3339)
			}
			if h.LastContactFailedAt != nil {
				item["last_contact_failed_at"] = h.LastContactFailedAt.UTC().Format(time.RFC3339)
			}
		}
		items[i] = item
	}
	return map[string]any{"results": items}, nil
}
