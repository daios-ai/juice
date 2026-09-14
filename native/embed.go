package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Embed declares @sys/llm/embed (§9).
func Embed(embedder kernel.Embedder) Spec {
	return Spec{
		Name:         "llm/embed",
		Description:  "Returns a text embedding vector from the configured embedding model",
		InputSchema:  obj(map[string]any{"text": str("Text to embed")}, "text"),
		OutputSchema: obj(map[string]any{"embedding": arrayOf(map[string]any{"type": "number"}, "Embedding vector")}),
		Handler: func(Host) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeEmbed(ctx, args, embedder)
			}
		},
	}
}

func executeEmbed(ctx context.Context, args map[string]any, embedder kernel.Embedder) (map[string]any, error) {
	if embedder == nil {
		return nil, kernel.ErrInvalidState.Wrap("embed service not configured")
	}
	text, _ := args["text"].(string)
	if text == "" {
		return nil, kernel.ErrInvalidInput.Wrap("embed requires text argument")
	}
	vec, err := embedder.Embed(ctx, text)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("embed failed: %v", err)
	}
	out := make([]any, len(vec))
	for i, v := range vec {
		out[i] = float64(v)
	}
	return map[string]any{"embedding": out}, nil
}
