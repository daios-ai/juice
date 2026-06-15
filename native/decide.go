package native

import (
	"context"
	"strings"

	"github.com/daios-ai/juice/kernel"
)

// RegisterDecideHandler registers the @sys/llm/decide native action handler on k.
func RegisterDecideHandler(k *kernel.Kernel, chatter kernel.DecideChatter) {
	k.RegisterNativeHandler("llm/decide", func(ctx context.Context, args map[string]any, _, _, ownerUserID, _, _ string) (map[string]any, error) {
		return executeDecide(ctx, args, chatter, k.ReadCallableAction, ownerUserID)
	})
}

func executeDecide(
	ctx context.Context,
	args map[string]any,
	chatter kernel.DecideChatter,
	lookup func(ctx context.Context, ownerHandle, actionName, processOwnerID string) (*kernel.Action, error),
	processOwnerID string,
) (map[string]any, error) {
	if chatter == nil {
		return nil, kernel.ErrInvalidState.Wrap("decide chat service not configured")
	}

	rawMsgs, ok := args["messages"]
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("llm/decide requires messages argument")
	}
	msgList, ok := rawMsgs.([]any)
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("messages must be an array")
	}
	messages := make([]kernel.DecideMessage, 0, len(msgList))
	for _, item := range msgList {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, kernel.ErrInvalidInput.Wrap("each message must be an object")
		}
		role, _ := m["role"].(string)
		if role == "" {
			return nil, kernel.ErrInvalidInput.Wrap("each message must have a role")
		}
		if role == "tool" {
			action, _ := m["action"].(string)
			mArgs, _ := m["args"].(map[string]any)
			result, _ := m["result"].(map[string]any)
			messages = append(messages, kernel.DecideMessage{Role: "tool", Action: action, Args: mArgs, Result: result})
		} else {
			content, _ := m["content"].(string)
			if content == "" {
				return nil, kernel.ErrInvalidInput.Wrap("each non-tool message must have content")
			}
			messages = append(messages, kernel.DecideMessage{Role: role, Content: content})
		}
	}

	rawActions, ok := args["actions"]
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("llm/decide requires actions argument")
	}
	actionList, ok := rawActions.([]any)
	if !ok || len(actionList) == 0 {
		return nil, kernel.ErrInvalidInput.Wrap("actions must be a non-empty array")
	}

	tools := make([]kernel.ToolDefinition, 0, len(actionList))
	toolSchemas := make(map[string]map[string]any, len(actionList))
	for _, raw := range actionList {
		ref, ok := raw.(string)
		if !ok {
			return nil, kernel.ErrInvalidInput.Wrap("each action must be a string")
		}
		idx := strings.IndexByte(ref, '/')
		if idx < 2 || ref[0] != '@' {
			return nil, kernel.ErrInvalidInput.Wrapf("invalid action reference %q: expected @owner/name", ref)
		}
		ownerHandle := ref[:idx]
		actionName := ref[idx+1:]

		a, err := lookup(ctx, ownerHandle, actionName, processOwnerID)
		if err != nil {
			return nil, err
		}

		price := a.Price
		tools = append(tools, kernel.ToolDefinition{
			Action:      ref,
			Description: a.Description,
			InputSchema: a.InputSchema,
			Price:       &price,
		})
		toolSchemas[ref] = a.InputSchema
	}

	call, msg, err := chatter.ChatDecide(ctx, messages, tools)
	if err != nil {
		return nil, err
	}

	if call == nil {
		return nil, kernel.ErrExecutionFailed.Wrap("model did not select an action")
	}

	schema, found := toolSchemas[call.Action]
	if !found {
		return nil, kernel.ErrExecutionFailed.Wrapf("model returned unknown action %q", call.Action)
	}
	if err := kernel.ValidateInput(schema, call.Args); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("action %q args failed validation: %v", call.Action, err)
	}

	result := map[string]any{
		"action": call.Action,
		"args":   call.Args,
	}
	if msg != nil {
		result["message"] = map[string]any{"role": msg.Role, "content": msg.Content}
	}
	return result, nil
}
