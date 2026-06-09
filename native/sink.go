package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

func RegisterSinkHandler(k *kernel.Kernel) {
	k.RegisterNativeHandler("sink", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return executeSink()
	})
}

func executeSink() (map[string]any, error) {
	return map[string]any{}, nil
}
