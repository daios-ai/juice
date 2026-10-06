// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// stubModel is a chat model that answers with canned values and records what it was asked.
type stubModel struct {
	reply    kernel.ChatMessage
	value    any
	err      error
	captured []kernel.ChatMessage
	schema   map[string]any
}

func (s *stubModel) Chat(_ context.Context, msgs []kernel.ChatMessage) (kernel.ChatMessage, error) {
	s.captured = msgs
	return s.reply, s.err
}

func (s *stubModel) ChatJSON(_ context.Context, msgs []kernel.ChatMessage, schema map[string]any) (any, error) {
	s.captured, s.schema = msgs, schema
	return s.value, s.err
}

func (s *stubModel) ChatDecide(context.Context, []kernel.DecideMessage, []kernel.ToolDefinition) (*kernel.ToolCall, *kernel.ChatMessage, error) {
	return nil, nil, s.err
}

var hi = []any{map[string]any{"role": "system", "content": "be brief"}, map[string]any{"role": "user", "content": "hi"}}

func firstMessage(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	choices, ok := result["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %v", result["choices"])
	}
	c := choices[0].(map[string]any)
	if c["index"] != 0 {
		t.Errorf("index = %v", c["index"])
	}
	return c["message"].(map[string]any)
}

func TestExecuteChat_Unbound(t *testing.T) {
	if _, err := executeChat(context.Background(), map[string]any{"messages": hi}, nil); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("an unbound chat native must answer ErrInvalidState, got %v", err)
	}
}

func TestExecuteChat_BadMessages(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"missing":   {},
		"not array": {"messages": "x"},
		"no role":   {"messages": []any{map[string]any{"content": "hi"}}},
	} {
		if _, err := executeChat(context.Background(), args, &stubModel{}); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("%s: got %v, want ErrInvalidInput", name, err)
		}
	}
}

// A plain call answers in the Chat Completions shape, the system prompt travelling as a message.
func TestExecuteChat_Reply(t *testing.T) {
	m := &stubModel{reply: kernel.ChatMessage{Role: "assistant", Content: "hello"}}
	result, err := executeChat(context.Background(), map[string]any{"messages": hi}, m)
	if err != nil {
		t.Fatal(err)
	}
	if msg := firstMessage(t, result); msg["role"] != "assistant" || msg["content"] != "hello" {
		t.Errorf("message = %v", msg)
	}
	if len(m.captured) != 2 || m.captured[0].Role != "system" {
		t.Errorf("the model was asked %v", m.captured)
	}
	m.err = errors.New("boom")
	if _, err := executeChat(context.Background(), map[string]any{"messages": hi}, m); !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("a failed model call: got %v, want ErrExecutionFailed", err)
	}
}

// llm/chat answers with text and nothing else: a request for structured output is refused by its
// own contract, before the handler, since that output kind is llm/json's (D4).
func TestChatInputSchemaIsTextOnly(t *testing.T) {
	in := Chat("", nil).InputSchema
	if err := kernel.ValidateInput(in, map[string]any{"messages": hi}); err != nil {
		t.Errorf("a plain chat was refused: %v", err)
	}
	for _, extra := range []string{"response_format", "system"} {
		if err := kernel.ValidateInput(in, map[string]any{"messages": hi, extra: map[string]any{}}); err == nil {
			t.Errorf("%s was admitted", extra)
		}
	}
}
