// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"math"

	"github.com/daios-ai/juice/kernel"
)

// Random declares @sys/random (§9): the randomness source WASM scripts have no ambient access to.
func Random() Spec {
	return Spec{
		Name:         "random",
		Description:  "Returns a cryptographically secure random float in [0, 1)",
		InputSchema:  obj(map[string]any{}),
		OutputSchema: obj(map[string]any{"value": num("Random float in [0, 1)")}),
		Handler: func(Host) kernel.NativeFunc {
			return func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeRandom()
			}
		},
	}
}

func executeRandom() (map[string]any, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, kernel.ErrInternal.Wrap("random: failed to read entropy")
	}
	u := binary.BigEndian.Uint64(buf[:])
	f := float64(u) / (math.MaxUint64 + 1.0)
	return map[string]any{"value": f}, nil
}
