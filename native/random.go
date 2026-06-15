package native

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"math"

	"github.com/daios-ai/juice/kernel"
)

// RegisterRandomHandler registers the @sys/random native action handler on k.
func RegisterRandomHandler(k *kernel.Kernel) {
	k.RegisterNativeHandler("random", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return executeRandom()
	})
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
