// SPDX-License-Identifier: AGPL-3.0-only

// Package native contains the built-in native action plugins for the Juice kernel.
// Each plugin is registered via its Register* function; the kernel never imports this package.
package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Chat declares a chat native (§9): llm/chat, which configuration binds to a model, or the native
// of one generated model (D17). Either is the Chat Completions subset and answers with text; JSON
// a schema describes is llm/json's, since an action has one output kind (D4).
func Chat(model string, m kernel.Chatter) Spec {
	return Spec{
		Name:        llmName(model, "chat"),
		Title:       "Chat with " + llmTitle(model),
		Description: "Chat completion by " + llmBy(model) + ", in the Chat Completions shape; the reply is text",
		InputSchema: obj(map[string]any{
			"messages": arrayOf(messageSchema(), "Conversation history; a system prompt is a message with role system"),
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

func executeChat(ctx context.Context, args map[string]any, m kernel.Chatter) (map[string]any, error) {
	if m == nil {
		return nil, kernel.ErrInvalidState.Wrap("no chat model is bound")
	}
	messages, err := chatMessages(args)
	if err != nil {
		return nil, err
	}
	reply, err := m.Chat(ctx, messages)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("chat failed: %v", err)
	}
	return map[string]any{"choices": []any{map[string]any{
		"index":   0,
		"message": map[string]any{"role": "assistant", "content": reply.Content},
	}}}, nil
}

// chatMessages extracts the {role, content} turns llm/chat and llm/json take.
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
