package native

import (
	"context"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// RegisterTimeHandler registers the @sys/time native action handler on k.
func RegisterTimeHandler(k *kernel.Kernel) {
	k.RegisterNativeHandler("time", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return executeTime()
	})
}

func executeTime() (map[string]any, error) {
	now := time.Now().UTC()
	return map[string]any{
		"unix": now.Unix(),
		"iso":  now.Format(time.RFC3339),
	}, nil
}
