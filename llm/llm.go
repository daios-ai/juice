// Package llm provides language model and embedding adapters.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/daios-ai/juice/kernel"
)

// OllamaEmbedder calls the Ollama /api/embeddings endpoint.
type OllamaEmbedder struct {
	URL   string // e.g. "http://localhost:11434"
	Model string // e.g. "nomic-embed-text"
}

func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]string{"model": o.Model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, kernel.ErrInternal.Wrapf("embed request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, kernel.ErrInternal.Wrapf("embed HTTP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, kernel.ErrInternal.Wrapf("ollama returned status %d", resp.StatusCode)
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, kernel.ErrInternal.Wrapf("decode embed response: %v", err)
	}
	if len(result.Embedding) == 0 {
		return nil, kernel.ErrInternal.Wrap("empty embedding returned")
	}
	return result.Embedding, nil
}

// OllamaChatter calls the Ollama /api/chat endpoint.
type OllamaChatter struct {
	URL   string
	Model string
}

func (o *OllamaChatter) Chat(ctx context.Context, messages []kernel.ChatMessage) (kernel.ChatMessage, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var msgs []msg
	for _, m := range messages {
		msgs = append(msgs, msg{Role: m.Role, Content: m.Content})
	}
	body, _ := json.Marshal(map[string]any{"model": o.Model, "messages": msgs, "stream": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return kernel.ChatMessage{}, kernel.ErrInternal.Wrapf("chat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return kernel.ChatMessage{}, kernel.ErrInternal.Wrapf("chat HTTP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return kernel.ChatMessage{}, kernel.ErrInternal.Wrapf("ollama chat returned status %d", resp.StatusCode)
	}

	var result struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return kernel.ChatMessage{}, kernel.ErrInternal.Wrapf("decode chat response: %v", err)
	}
	return kernel.ChatMessage{Role: result.Message.Role, Content: result.Message.Content}, nil
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
