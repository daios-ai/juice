package llm

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// FakeChatter returns a fixed response for tests.
type FakeChatter struct {
	Reply kernel.ChatMessage
}

func (f *FakeChatter) Chat(_ context.Context, _ []kernel.ChatMessage) (kernel.ChatMessage, error) {
	if f.Reply.Role == "" {
		return kernel.ChatMessage{Role: "assistant", Content: "ok"}, nil
	}
	return f.Reply, nil
}

// FakeEmbedder returns a deterministic fixed-length vector for tests.
// The vector is derived from the byte sum of the text so similarity is meaningful.
type FakeEmbedder struct {
	Dims int // vector dimensionality; defaults to 8
}

func (f *FakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	dims := f.Dims
	if dims == 0 {
		dims = 8
	}
	vec := make([]float32, dims)
	for i, b := range []byte(text) {
		vec[i%dims] += float32(b)
	}
	// L2-normalize.
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	if norm > 0 {
		x := norm
		for i := 0; i < 10; i++ {
			x = (x + norm/x) / 2
		}
		for i := range vec {
			vec[i] /= x
		}
	}
	return vec, nil
}
