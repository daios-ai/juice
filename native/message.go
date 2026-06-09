package native

import (
	"context"
	"encoding/json"

	"github.com/daios-ai/juice/kernel"
)

// RegisterMessageHandler registers the @sys/message native action handler on k.
func RegisterMessageHandler(k *kernel.Kernel, n kernel.Notifier) {
	k.RegisterNativeHandler("message", func(ctx context.Context, args map[string]any, _, callerID, ownerUserID, processID, parentTraceID string) (map[string]any, error) {
		return executeMessage(ctx, args, callerID, ownerUserID, processID, parentTraceID, k, n)
	})
}

func executeMessage(ctx context.Context, args map[string]any, callerID, ownerUserID, processID, parentTraceID string, k *kernel.Kernel, n kernel.Notifier) (map[string]any, error) {
	to, _ := args["to"].(string)
	if to == "" {
		return nil, kernel.ErrInvalidInput.Wrap("message requires to argument")
	}
	msg, _ := args["message"].(string)
	if msg == "" {
		return nil, kernel.ErrInvalidInput.Wrap("message requires message argument")
	}
	nextActionRef, _ := args["next_action"].(string)
	if nextActionRef == "" {
		return nil, kernel.ErrInvalidInput.Wrap("message requires next_action argument")
	}
	subject, _ := args["subject"].(string)

	recipient, err := k.ReadUserByHandle(ctx, to)
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrapf("to %q not found", to)
	}

	ownerHandle, actionName, err := kernel.ParseActionRef(nextActionRef)
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrapf("next_action: %v", err)
	}
	action, err := k.ReadCallableAction(ctx, ownerHandle, actionName, ownerUserID)
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrapf("next_action %q: %v", nextActionRef, err)
	}

	var partialArgs json.RawMessage
	if pa := args["partial_args"]; pa != nil {
		b, err := json.Marshal(pa)
		if err != nil {
			return nil, kernel.ErrInvalidInput.Wrap("partial_args must be a JSON object")
		}
		partialArgs = b
	}

	var ptID *string
	if parentTraceID != "" {
		ptID = &parentTraceID
	}

	step, err := k.CreateStep(ctx, callerID, processID, ptID, action.ID, partialArgs, nil, recipient.ID)
	if err != nil {
		return nil, err
	}

	delivered := false
	if n != nil {
		sub := subject
		if sub == "" {
			sub = "You have a new message"
		}
		if notifyErr := n.Notify(ctx, recipient.Email, sub, msg); notifyErr == nil {
			delivered = true
		}
	}

	return map[string]any{
		"step_id":   step.ID,
		"delivered": delivered,
	}, nil
}
