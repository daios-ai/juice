// Package native contains the built-in native action plugins for the Juice kernel.
// Each plugin is registered via its Register* function; the kernel never imports this package.
package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// RegisterChatHandler registers the @sys/llm/chat native action handler on k.
func RegisterChatHandler(k *kernel.Kernel, chatter kernel.Chatter) {
	k.RegisterNativeHandler("llm/chat", func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return executeChat(ctx, args, chatter)
	})
}

func executeChat(ctx context.Context, args map[string]any, chatter kernel.Chatter) (map[string]any, error) {
	if chatter == nil {
		return nil, kernel.ErrInvalidState.Wrap("chat service not configured")
	}

	rawMsgs, ok := args["messages"]
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("chat requires messages argument")
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
