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

// structured asks for a reply the schema {name: string, required} admits.
func structured() map[string]any {
	return map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "person", "schema": map[string]any{
		"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []any{"name"}}}}
}

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

// A json_schema response_format is asked for in canonical form and answered as content the schema
// admits; a reply it refuses, or a schema outside the subset, is no answer.
func TestExecuteChat_StructuredOutput(t *testing.T) {
	m := &stubModel{value: map[string]any{"name": "Ada"}}
	result, err := executeChat(context.Background(), map[string]any{"messages": hi, "response_format": structured()}, m)
	if err != nil {
		t.Fatal(err)
	}
	if msg := firstMessage(t, result); msg["content"] != `{"name":"Ada"}` {
		t.Errorf("content = %v", msg["content"])
	}
	if m.schema["additionalProperties"] != false {
		t.Errorf("the model was not asked for the canonical schema: %v", m.schema)
	}

	m.value = map[string]any{"wrong": 1}
	if _, err := executeChat(context.Background(), map[string]any{"messages": hi, "response_format": structured()}, m); !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("a reply the schema refuses: got %v, want ErrExecutionFailed", err)
	}
	rf := structured()
	rf["json_schema"].(map[string]any)["schema"] = map[string]any{"anyOf": []any{}}
	if _, err := executeChat(context.Background(), map[string]any{"messages": hi, "response_format": rf}, m); !errors.Is(err, kernel.ErrSchemaViolation) {
		t.Errorf("a schema outside the subset: got %v, want ErrSchemaViolation", err)
	}
}

// The response_format the input schema declares admits the standard shape and nothing else.
func TestChatInputSchemaAdmitsTheStandardShape(t *testing.T) {
	in := Chat("", nil).InputSchema
	if err := kernel.ValidateInput(in, map[string]any{"messages": hi, "response_format": structured()}); err != nil {
		t.Errorf("the standard shape was refused: %v", err)
	}
	other := structured()
	other["type"] = "json_object"
	if err := kernel.ValidateInput(in, map[string]any{"messages": hi, "response_format": other}); err == nil {
		t.Error("a response_format other than json_schema was admitted")
	}
	if err := kernel.ValidateInput(in, map[string]any{"messages": hi, "system": "x"}); err == nil {
		t.Error("the non-standard system field was admitted")
	}
}
