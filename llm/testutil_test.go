package llm

import (
	"context"
	"math"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestFakeChatter_DefaultReply(t *testing.T) {
	f := &FakeChatter{}
	got, err := f.Chat(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Role != "assistant" || got.Content != "ok" {
		t.Errorf("want {assistant ok}, got %+v", got)
	}
}

func TestFakeChatter_ConfiguredReply(t *testing.T) {
	want := kernel.ChatMessage{Role: "assistant", Content: "hello"}
	f := &FakeChatter{Reply: want}
	got, err := f.Chat(context.Background(), []kernel.ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("want %+v, got %+v", want, got)
	}
}

func TestFakeJSONChatter_ReturnsValue(t *testing.T) {
	f := &FakeJSONChatter{Value: map[string]any{"x": float64(1)}}
	got, err := f.ChatJSON(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok || m["x"] != float64(1) {
		t.Errorf("unexpected value: %v", got)
	}
}

func TestFakeToolChatter_ReturnsCalls(t *testing.T) {
	calls := []kernel.ToolCall{{Action: "@sys/lookup", Args: map[string]any{"query": "test"}}}
	f := &FakeToolChatter{Calls: calls}
	got, msg, err := f.ChatTools(context.Background(), nil, nil, "auto", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Action != "@sys/lookup" {
		t.Errorf("unexpected calls: %v", got)
	}
	if msg != nil {
		t.Errorf("expected nil message, got %v", msg)
	}
}

func TestFakeEmbedder_Length(t *testing.T) {
	f := &FakeEmbedder{Dims: 4}
	vec, err := f.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vec) != 4 {
		t.Errorf("want len 4, got %d", len(vec))
	}
}

func TestFakeEmbedder_DefaultDims(t *testing.T) {
	f := &FakeEmbedder{}
	vec, err := f.Embed(context.Background(), "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vec) != 8 {
		t.Errorf("want len 8, got %d", len(vec))
	}
}

func TestFakeEmbedder_Normalized(t *testing.T) {
	f := &FakeEmbedder{Dims: 4}
	vec, err := f.Embed(context.Background(), "normalize me")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if math.Abs(norm-1.0) > 0.01 {
		t.Errorf("vector not normalized: L2 norm = %f", math.Sqrt(norm))
	}
}

func TestFakeEmbedder_Deterministic(t *testing.T) {
	f := &FakeEmbedder{Dims: 8}
	a, _ := f.Embed(context.Background(), "same text")
	b, _ := f.Embed(context.Background(), "same text")
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("embedding not deterministic at index %d: %f != %f", i, a[i], b[i])
		}
	}
}
