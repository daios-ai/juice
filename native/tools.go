package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// RegisterToolsHandler registers the @sys/llm/tools native action handler on k.
func RegisterToolsHandler(k *kernel.Kernel, chatter kernel.ToolChatter) {
	k.RegisterNativeHandler("llm/tools", func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return executeTools(ctx, args, chatter)
	})
}

func executeTools(ctx context.Context, args map[string]any, chatter kernel.ToolChatter) (map[string]any, error) {
	if chatter == nil {
		return nil, kernel.ErrInvalidState.Wrap("tool chat service not configured")
	}

	rawMsgs, ok := args["messages"]
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("llm/tools requires messages argument")
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

	rawTools, ok := args["tools"]
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("llm/tools requires tools argument")
	}
	toolList, ok := rawTools.([]any)
	if !ok || len(toolList) == 0 {
		return nil, kernel.ErrInvalidInput.Wrap("tools must be a non-empty array")
	}

	tools := make([]kernel.ToolDefinition, 0, len(toolList))
	toolMap := make(map[string]kernel.ToolDefinition, len(toolList))
	for _, raw := range toolList {
		t, ok := raw.(map[string]any)
		if !ok {
			return nil, kernel.ErrInvalidInput.Wrap("each tool must be an object")
		}
		action, _ := t["action"].(string)
		desc, _ := t["description"].(string)
		if action == "" || desc == "" {
			return nil, kernel.ErrInvalidInput.Wrap("each tool must have action and description")
		}
		inputSchema, _ := t["input_schema"].(map[string]any)
		if inputSchema == nil {
			inputSchema = map[string]any{"type": "object"}
		}
		if err := kernel.ValidateSchema(inputSchema); err != nil {
			return nil, err
		}
		var outputSchema map[string]any
		if os, ok := t["output_schema"].(map[string]any); ok {
			if err := kernel.ValidateSchema(os); err != nil {
				return nil, err
			}
			outputSchema = os
		}
		var price *int64
		if p, ok := t["price"].(float64); ok {
			pv := int64(p)
			price = &pv
		}
		td := kernel.ToolDefinition{
			Action:       action,
			Description:  desc,
			InputSchema:  inputSchema,
			OutputSchema: outputSchema,
			Price:        price,
		}
		tools = append(tools, td)
		toolMap[action] = td
	}

	toolChoice, _ := args["tool_choice"].(string)
	if toolChoice == "" {
		toolChoice = "auto"
	}
	var maxToolCalls int
	if m, ok := args["max_tool_calls"].(float64); ok {
		maxToolCalls = int(m)
	}

	calls, msg, err := chatter.ChatTools(ctx, messages, tools, toolChoice, maxToolCalls)
	if err != nil {
		return nil, err
	}

	validCalls := make([]any, 0, len(calls))
	for _, tc := range calls {
		td, found := toolMap[tc.Action]
		if !found {
			return nil, kernel.ErrExecutionFailed.Wrapf("model returned unknown tool %q", tc.Action)
		}
		if err := kernel.ValidateInput(td.InputSchema, tc.Args); err != nil {
			return nil, kernel.ErrExecutionFailed.Wrapf("tool %q args failed validation: %v", tc.Action, err)
		}
		validCalls = append(validCalls, map[string]any{
			"action": tc.Action,
			"args":   tc.Args,
		})
	}

	if toolChoice == "required" && len(validCalls) == 0 {
		return nil, kernel.ErrExecutionFailed.Wrap("tool_choice=required but no valid tool calls produced")
	}

	result := map[string]any{"tool_calls": validCalls}
	if msg != nil {
		result["message"] = map[string]any{"role": msg.Role, "content": msg.Content}
	}
	return result, nil
}
