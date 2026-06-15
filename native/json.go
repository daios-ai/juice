package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// RegisterJSONHandler registers the @sys/llm/json native action handler on k.
func RegisterJSONHandler(k *kernel.Kernel, chatter kernel.JSONChatter) {
	k.RegisterNativeHandler("llm/json", func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return executeJSON(ctx, args, chatter)
	})
}

func executeJSON(ctx context.Context, args map[string]any, chatter kernel.JSONChatter) (map[string]any, error) {
	if chatter == nil {
		return nil, kernel.ErrInvalidState.Wrap("json chat service not configured")
	}

	rawMsgs, ok := args["messages"]
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("llm/json requires messages argument")
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

	rawSchema, ok := args["output_schema"]
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("llm/json requires output_schema")
	}
	outputSchema, ok := rawSchema.(map[string]any)
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("output_schema must be an object")
	}
	if err := kernel.ValidateSchema(outputSchema); err != nil {
		return nil, err
	}

	value, err := chatter.ChatJSON(ctx, messages, outputSchema)
	if err != nil {
		return nil, err
	}

	if err := kernel.ValidateInput(outputSchema, value); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("model output failed schema validation: %v", err)
	}

	return map[string]any{"value": value}, nil
}
