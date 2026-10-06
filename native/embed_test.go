// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

type stubEmbedder struct {
	vec []float32
	err error
}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return s.vec, s.err
}

func TestExecuteEmbed_Unbound(t *testing.T) {
	if _, err := executeEmbed(context.Background(), map[string]any{"input": "hello"}, nil); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("an unbound embed native must answer ErrInvalidState, got %v", err)
	}
}

func TestExecuteEmbed_EmptyInput(t *testing.T) {
	for _, args := range []map[string]any{{}, {"input": ""}} {
		if _, err := executeEmbed(context.Background(), args, &stubEmbedder{}); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("%v: got %v, want ErrInvalidInput", args, err)
		}
	}
}

// An embedding answers in the Embeddings shape.
func TestExecuteEmbed_Reply(t *testing.T) {
	result, err := executeEmbed(context.Background(), map[string]any{"input": "hello"}, &stubEmbedder{vec: []float32{0.1, 0.2, 0.3}})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := result["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data = %v", result["data"])
	}
	d := data[0].(map[string]any)
	emb := d["embedding"].([]any)
	if d["index"] != 0 || len(emb) != 3 || emb[0].(float64) != float64(float32(0.1)) {
		t.Errorf("data[0] = %v", d)
	}
	if _, err := executeEmbed(context.Background(), map[string]any{"input": "x"}, &stubEmbedder{err: errors.New("down")}); !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("a failed model call: got %v, want ErrExecutionFailed", err)
	}
}
