package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Sink declares @sys/sink (§9): the universal no-op a step parks against.
func Sink() Spec {
	return Spec{
		Name:         "sink",
		Description:  "Universal no-op sink; accepts any input and returns {}",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Handler: func(*kernel.Kernel) kernel.NativeFunc {
			return func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeSink()
			}
		},
	}
}

func executeSink() (map[string]any, error) {
	return map[string]any{}, nil
}
