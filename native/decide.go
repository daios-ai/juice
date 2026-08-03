package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// RegisterDecideHandler registers the @sys/llm/decide native action handler on k.
func RegisterDecideHandler(k *kernel.Kernel, chatter kernel.DecideChatter) {
	k.RegisterNativeHandler("llm/decide", func(ctx context.Context, args map[string]any, _, callerID, _, _, _ string) (map[string]any, error) {
		return executeDecide(ctx, args, chatter, k.ReadCallableAction, k.ResolveAction, callerID)
	})
}

func executeDecide(
	ctx context.Context,
	args map[string]any,
	chatter kernel.DecideChatter,
	lookup func(ctx context.Context, ownerHandle, actionName, callerID string) (*kernel.Action, error),
	resolve func(ctx context.Context, ref string) (*kernel.Action, error),
	callerID string,
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
		content, _ := m["content"].(string)
		var tool *kernel.DecideTool
		if t, ok := m["tool"].(map[string]any); ok {
			action, _ := t["action"].(string)
			args, _ := t["args"].(map[string]any)
			result, _ := t["result"].(map[string]any)
			tool = &kernel.DecideTool{Action: action, Args: args, Result: result}
		}
		messages = append(messages, kernel.DecideMessage{Role: role, Content: content, Tool: tool})
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
		r, err := kernel.ParseActionRef(ref)
		if err != nil {
			return nil, kernel.ErrInvalidInput.Wrapf("invalid action reference %q: expected owner/name", ref)
		}

		var a *kernel.Action
		if r.Kernel != "" {
			// A discovered kernel-qualified reference (lookup → decide → run): resolve it through the
			// same resolve-and-cache path as run. A stale/unresolvable candidate is DISCARDED so one
			// dead peer never blocks selection among the valid candidates (§13).
			a, err = resolve(ctx, ref)
			if err != nil {
				continue
			}
		} else {
			// A bare local reference keeps strict behavior: an unknown one aborts.
			a, err = lookup(ctx, r.Owner, r.Name, callerID)
			if err != nil {
				return nil, err
			}
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
	if len(tools) == 0 {
		return nil, kernel.ErrNotFound.Wrap("no action reference could be resolved")
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
