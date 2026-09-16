// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// Time declares @sys/time (§9).
func Time() Spec {
	return Spec{
		Name:        "time",
		Description: "Returns the current UTC time",
		InputSchema: obj(map[string]any{}),
		OutputSchema: obj(map[string]any{
			"unix": integer("Seconds since UTC epoch"),
			"iso":  str("RFC 3339 timestamp"),
		}),
		Handler: func(Host) kernel.NativeFunc {
			return func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeTime()
			}
		},
	}
}

func executeTime() (map[string]any, error) {
	now := time.Now().UTC()
	return map[string]any{
		"unix": now.Unix(),
		"iso":  now.Format(time.RFC3339),
	}, nil
}
