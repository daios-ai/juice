// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Embed declares an embedding native (§9): llm/embed, which configuration binds to a model, or the
// native of one generated model (D17). Either is the Embeddings subset; input is one string, since
// the standard's string-or-array is a oneOf the schema subset refuses (D4).
func Embed(model string, embedder kernel.Embedder) Spec {
	title := "Embed text"
	if model != "" {
		title += " with " + model
	}
	return Spec{
		Name:        llmName(model, "embed"),
		Title:       title,
		Description: "Text embedding by " + llmBy(model) + ", in the Embeddings shape",
		InputSchema: obj(map[string]any{"input": str("Text to embed")}, "input"),
		OutputSchema: obj(map[string]any{
			"data": arrayOf(obj(map[string]any{
				"index":     integer("Position of the input"),
				"embedding": arrayOf(map[string]any{"type": "number"}, "Embedding vector"),
			}), "One embedding per input"),
		}),
		Handler: func(Host) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeEmbed(ctx, args, embedder)
			}
		},
	}
}

func executeEmbed(ctx context.Context, args map[string]any, embedder kernel.Embedder) (map[string]any, error) {
	if embedder == nil {
		return nil, kernel.ErrInvalidState.Wrap("no embedding model is bound")
	}
	text, _ := args["input"].(string)
	if text == "" {
		return nil, kernel.ErrInvalidInput.Wrap("embed requires input")
	}
	vec, err := embedder.Embed(ctx, text)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("embed failed: %v", err)
	}
	out := make([]any, len(vec))
	for i, v := range vec {
		out[i] = float64(v)
	}
	return map[string]any{"data": []any{map[string]any{"index": 0, "embedding": out}}}, nil
}
