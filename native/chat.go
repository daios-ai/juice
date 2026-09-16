// SPDX-License-Identifier: AGPL-3.0-only

// Package native contains the built-in native action plugins for the Juice kernel.
// Each plugin is registered via its Register* function; the kernel never imports this package.
package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Chat declares @sys/llm/chat (§9).
func Chat(chatter kernel.Chatter) Spec {
	return Spec{
		Name:        "llm/chat",
		Description: "Chat completion via the configured language model",
		InputSchema: obj(map[string]any{
			"messages": arrayOf(messageSchema(), "Conversation history"),
			"system":   str("Optional system prompt"),
		}, "messages"),
		OutputSchema: obj(map[string]any{
			"message": objd("Generated reply message", map[string]any{
				"role":    str("Role of the message sender (assistant)"),
				"content": str("Text content of the reply"),
			}),
		}),
		Handler: func(Host) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeChat(ctx, args, chatter)
			}
		},
	}
}

func executeChat(ctx context.Context, args map[string]any, chatter kernel.Chatter) (map[string]any, error) {
	if chatter == nil {
		return nil, kernel.ErrInvalidState.Wrap("chat service not configured")
	}

	messages, err := chatMessages(args, "chat")
	if err != nil {
		return nil, err
	}

	reply, err := chatter.Chat(ctx, messages)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("chat failed: %v", err)
	}
	return map[string]any{
		"message": map[string]any{
			"role":    reply.Role,
			"content": reply.Content,
		},
	}, nil
}

// chatMessages extracts the {role, content} list both plain LLM natives take, prepending the
// optional system message. One parser, so llm/chat and llm/json cannot diverge on what they accept.
func chatMessages(args map[string]any, who string) ([]kernel.ChatMessage, error) {
	rawMsgs, ok := args["messages"]
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrapf("%s requires messages argument", who)
	}
	msgList, ok := rawMsgs.([]any)
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("messages must be an array")
	}
	var messages []kernel.ChatMessage
	if sys, ok := args["system"].(string); ok && sys != "" {
		messages = append(messages, kernel.ChatMessage{Role: "system", Content: sys})
	}
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
