package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// RegisterEmbedHandler registers the @sys/llm/embed native action handler on k.
func RegisterEmbedHandler(k *kernel.Kernel, embedder kernel.Embedder) {
	k.RegisterNativeHandler("llm/embed", func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return executeEmbed(ctx, args, embedder)
	})
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
