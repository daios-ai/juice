// SPDX-License-Identifier: AGPL-3.0-only

// Package native contains the built-in native action plugins for the Juice kernel.
// Each plugin is registered via its Register* function; the kernel never imports this package.
package native

import (
	"context"
	"encoding/json"

	"github.com/daios-ai/juice/kernel"
)

// Chat declares a chat native (§9): llm/chat, which configuration binds to a model, or the native
// of one generated model (D17). Either is the Chat Completions subset, structured output included:
// a json_schema response_format is answered as content validated against the schema.
func Chat(model string, m ChatModel) Spec {
	return Spec{
		Name:        llmName(model, "chat"),
		Title:       "Chat with " + llmTitle(model),
		Description: "Chat completion by " + llmBy(model) + ", in the Chat Completions shape; a json_schema response_format is answered as JSON content the schema admits, validated locally",
		InputSchema: obj(map[string]any{
			"messages": arrayOf(messageSchema(), "Conversation history; a system prompt is a message with role system"),
			"response_format": objd("Asks for structured output: the reply's content is JSON the schema admits", map[string]any{
				"type": map[string]any{"type": "string", "enum": []any{"json_schema"}, "description": "The kind of structured output; json_schema"},
				"json_schema": objd("The schema the reply must satisfy", map[string]any{
					"name":   str("A name for the schema"),
					"schema": object("JSON Schema the reply must satisfy"),
				}, "schema"),
			}, "type", "json_schema"),
		}, "messages"),
		OutputSchema: obj(map[string]any{
			"choices": arrayOf(obj(map[string]any{
				"index": integer("Position of this choice"),
				"message": objd("The reply", map[string]any{
					"role":    str("Role of the message sender (assistant)"),
					"content": str("Text content of the reply"),
				}),
			}), "The model's reply, as one choice"),
		}),
		Handler: func(Host) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeChat(ctx, args, m)
			}
		},
	}
}

func executeChat(ctx context.Context, args map[string]any, m ChatModel) (map[string]any, error) {
	if m == nil {
		return nil, kernel.ErrInvalidState.Wrap("no chat model is bound")
	}
	messages, err := chatMessages(args)
	if err != nil {
		return nil, err
	}
	var content string
	if rf, ok := args["response_format"].(map[string]any); ok {
		js, _ := rf["json_schema"].(map[string]any)
		raw, _ := js["schema"].(map[string]any)
		// The schema is read in the canonical form, so the model is asked for, and the reply held
		// to, exactly what any action's schema means (D4).
		schema, _, err := kernel.NormalizeSchema("response_format.json_schema.schema", raw)
		if err != nil {
			return nil, err
		}
		value, err := m.ChatJSON(ctx, messages, schema)
		if err != nil {
			return nil, kernel.ErrExecutionFailed.Wrapf("chat failed: %v", err)
		}
		if err := kernel.ValidateInput(schema, value); err != nil {
			return nil, kernel.ErrExecutionFailed.Wrapf("model output failed schema validation: %v", err)
		}
		b, err := json.Marshal(value)
		if err != nil {
			return nil, kernel.ErrInternal.Wrapf("encode reply: %v", err)
		}
		content = string(b)
	} else {
		reply, err := m.Chat(ctx, messages)
		if err != nil {
			return nil, kernel.ErrExecutionFailed.Wrapf("chat failed: %v", err)
		}
		content = reply.Content
	}
	return map[string]any{"choices": []any{map[string]any{
		"index":   0,
		"message": map[string]any{"role": "assistant", "content": content},
	}}}, nil
}

// chatMessages extracts the {role, content} turns llm/chat takes.
func chatMessages(args map[string]any) ([]kernel.ChatMessage, error) {
	msgList, ok := args["messages"].([]any)
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("chat requires a messages array")
	}
	messages := make([]kernel.ChatMessage, 0, len(msgList))
	for _, item := range msgList {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, kernel.ErrInvalidInput.Wrap("each message must be an object")
		}
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == "" || content == "" {
			return nil, kernel.ErrInvalidInput.Wrap("each message must have role and content")
		}
		messages = append(messages, kernel.ChatMessage{Role: role, Content: content})
	}
	return messages, nil
}
