package kernel

import "context"

// RegisterChatHandler registers the @sys/llm/chat native action handler on k.
func RegisterChatHandler(k *Kernel) {
	k.RegisterNativeHandler("llm/chat", func(ctx context.Context, args map[string]any, _, _, _ string) (map[string]any, error) {
		return executeChat(k, ctx, args)
	})
}

func executeChat(k *Kernel, ctx context.Context, args map[string]any) (map[string]any, error) {
	if k.chatter == nil {
		return nil, ErrInvalidState.Wrap("chat service not configured")
	}

	rawMsgs, ok := args["messages"]
	if !ok {
		return nil, ErrInvalidInput.Wrap("chat requires messages argument")
	}
	msgList, ok := rawMsgs.([]any)
	if !ok {
		return nil, ErrInvalidInput.Wrap("messages must be an array")
	}

	var messages []ChatMessage
	if sys, ok := args["system"].(string); ok && sys != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: sys})
	}
	for _, item := range msgList {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, ErrInvalidInput.Wrap("each message must be an object")
		}
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == "" || content == "" {
			return nil, ErrInvalidInput.Wrap("each message must have role and content")
		}
		messages = append(messages, ChatMessage{Role: role, Content: content})
	}

	reply, err := k.chatter.Chat(ctx, messages)
	if err != nil {
		return nil, ErrExecutionFailed.Wrapf("chat failed: %v", err)
	}
	return map[string]any{
		"message": map[string]any{
			"role":    reply.Role,
			"content": reply.Content,
		},
	}, nil
}
