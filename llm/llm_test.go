// SPDX-License-Identifier: AGPL-3.0-only

package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// TestOllamaClientTimeoutGenerous guards the HTTP timeout used for all Ollama calls:
// it must be generous enough for a large local model (e.g. the default 26B chat
// model) to generate long completions, which 120s was not.
func TestOllamaClientTimeoutGenerous(t *testing.T) {
	if ollamaClient.Timeout < 300*time.Second {
		t.Errorf("ollamaClient.Timeout = %v, want >= 300s for large local models", ollamaClient.Timeout)
	}
}

// ollamaStub serves one canned reply for every request and records the last body it received.
func ollamaStub(t *testing.T, status int, reply string) (*httptest.Server, *map[string]any) {
	t.Helper()
	got := &map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// TestOllamaEmbedRoundTrip covers the success path of the shared transport: the model and prompt
// reach the server and the decoded vector is returned.
func TestOllamaEmbedRoundTrip(t *testing.T) {
	srv, got := ollamaStub(t, http.StatusOK, `{"embedding":[0.25,0.5,0.75]}`)
	e := &OllamaEmbedder{URL: srv.URL, Model: "nomic-embed-text"}

	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.25 {
		t.Errorf("decoded vector = %v, want [0.25 0.5 0.75]", vec)
	}
	if (*got)["model"] != "nomic-embed-text" || (*got)["prompt"] != "hello" {
		t.Errorf("request body did not carry model and prompt: %v", *got)
	}
}

// TestOllamaChatRoundTrip covers the chat success path and message projection.
func TestOllamaChatRoundTrip(t *testing.T) {
	srv, got := ollamaStub(t, http.StatusOK, `{"message":{"role":"assistant","content":"pong"}}`)
	c := &OllamaChatter{URL: srv.URL, Model: "gemma4:26b"}

	msg, err := c.Chat(context.Background(), []kernel.ChatMessage{{Role: "user", Content: "ping"}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if msg.Role != "assistant" || msg.Content != "pong" {
		t.Errorf("Chat = %+v, want assistant/pong", msg)
	}
	if (*got)["stream"] != false {
		t.Errorf("chat must request a non-streaming reply, got %v", (*got)["stream"])
	}
}

// TestOllamaTransportFailures proves every stage of ollamaPost propagates rather than yielding a
// zero value: an upstream error status and a malformed body must both surface as errors, on every
// entry point. A silently-swallowed decode is how an empty embedding or reply reaches a caller.
func TestOllamaTransportFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		reply  string
	}{
		{"non-200", http.StatusInternalServerError, `{"error":"model not found"}`},
		{"malformed json", http.StatusOK, `{"embedding": [0.1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := ollamaStub(t, tc.status, tc.reply)
			ctx := context.Background()

			if _, err := (&OllamaEmbedder{URL: srv.URL, Model: "m"}).Embed(ctx, "x"); err == nil {
				t.Error("Embed: got nil error, want failure")
			}
			c := &OllamaChatter{URL: srv.URL, Model: "m"}
			if _, err := c.Chat(ctx, []kernel.ChatMessage{{Role: "user", Content: "x"}}); err == nil {
				t.Error("Chat: got nil error, want failure")
			}
			if _, err := c.ChatJSON(ctx, []kernel.ChatMessage{{Role: "user", Content: "x"}}, map[string]any{"type": "object"}); err == nil {
				t.Error("ChatJSON: got nil error, want failure")
			}
			if _, _, err := c.ChatDecide(ctx, []kernel.DecideMessage{{Role: "user", Content: "x"}}, nil); err == nil {
				t.Error("ChatDecide: got nil error, want failure")
			}
		})
	}
}

// TestOllamaUnreachableServer covers the transport leg: a closed server is an error, never a
// zero-valued success.
func TestOllamaUnreachableServer(t *testing.T) {
	srv, _ := ollamaStub(t, http.StatusOK, `{}`)
	url := srv.URL
	srv.Close()

	if _, err := (&OllamaEmbedder{URL: url, Model: "m"}).Embed(context.Background(), "x"); err == nil {
		t.Error("Embed against a closed server: got nil error, want failure")
	}
}
