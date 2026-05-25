package llm

import (
	"context"
	"math"
	"testing"
)

func TestFakeEmbedder(t *testing.T) {
	f := &FakeEmbedder{Dims: 4}
	ctx := context.Background()

	vec, err := f.Embed(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 4 {
		t.Errorf("expected 4-dim vector, got %d", len(vec))
	}

	// Vector should be unit-length (L2-normalized).
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	if math.Abs(float64(norm)-1.0) > 0.01 {
		t.Errorf("vector not approximately unit-length: norm=%f", norm)
	}
}

func TestFakeEmbedderSimilarity(t *testing.T) {
	f := &FakeEmbedder{Dims: 8}
	ctx := context.Background()

	// Same text should produce identical vectors.
	v1, _ := f.Embed(ctx, "abc")
	v2, _ := f.Embed(ctx, "abc")

	var dot float32
	for i := range v1 {
		dot += v1[i] * v2[i]
	}
	if math.Abs(float64(dot)-1.0) > 1e-4 {
		t.Errorf("identical texts should produce dot product 1.0, got %f", dot)
	}

	// Different texts should produce different vectors.
	v3, _ := f.Embed(ctx, "xyz")
	var same bool
	for i := range v1 {
		if v1[i] != v3[i] {
			same = false
			break
		}
		same = true
	}
	if same {
		t.Error("different texts produced identical vectors")
	}
}

func TestFakeEmbedderDefaultDims(t *testing.T) {
	f := &FakeEmbedder{} // dims=0 → defaults to 8
	vec, err := f.Embed(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 8 {
		t.Errorf("expected 8 dims by default, got %d", len(vec))
	}
}
