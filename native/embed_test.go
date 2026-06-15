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

func TestExecuteEmbed_NilEmbedder(t *testing.T) {
	_, err := executeEmbed(context.Background(), map[string]any{"text": "hello"}, nil)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState with nil embedder, got %v", err)
	}
}

func TestExecuteEmbed_EmptyText(t *testing.T) {
	_, err := executeEmbed(context.Background(), map[string]any{"text": ""}, &stubEmbedder{})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty text, got %v", err)
	}
}

func TestExecuteEmbed_MissingText(t *testing.T) {
	_, err := executeEmbed(context.Background(), map[string]any{}, &stubEmbedder{})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing text, got %v", err)
	}
}

func TestExecuteEmbed_Success(t *testing.T) {
	e := &stubEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	result, err := executeEmbed(context.Background(), map[string]any{"text": "hello"}, e)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	emb, ok := result["embedding"].([]any)
	if !ok {
		t.Fatalf("expected []any embedding, got %T", result["embedding"])
	}
	if len(emb) != 3 {
		t.Errorf("expected 3 elements, got %d", len(emb))
	}
	if emb[0].(float64) != float64(float32(0.1)) {
		t.Errorf("unexpected first element: %v", emb[0])
	}
}
