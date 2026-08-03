package native

import (
	"context"
	"encoding/json"

	"github.com/daios-ai/juice/kernel"
)

func RegisterMessageHandler(k *kernel.Kernel) {
	k.RegisterNativeHandler("message", func(ctx context.Context, args map[string]any, _, callerID, _, _, parentTraceID string) (map[string]any, error) {
		return executeMessage(ctx, args, callerID, parentTraceID, k)
	})
}

func executeMessage(ctx context.Context, args map[string]any, callerID, parentTraceID string, k *kernel.Kernel) (map[string]any, error) {
	to, _ := args["to"].(string)
	if to == "" {
		return nil, kernel.ErrInvalidInput.Wrap("message requires to")
	}
	msg, _ := args["message"].(string)
	if msg == "" {
		return nil, kernel.ErrInvalidInput.Wrap("message requires message")
	}

	// `to` may be a local handle or a remote user@kernel (§13): resolve to the routing account id
	// plus the completer's stable remote id (empty for a local recipient).
	recipientID, remoteID, err := k.ResolveRequiredCaller(ctx, to)
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrapf("to %q not found", to)
	}

	sink, err := k.ReadCallableAction(ctx, kernel.SuperuserHandle, "sink", callerID)
	if err != nil {
		return nil, kernel.ErrInvalidState.Wrap("sys/sink not available")
	}

	partialArgs, _ := json.Marshal(map[string]any{"message": msg})

	step, err := k.CreateStep(ctx, parentTraceID, sink.ID, json.RawMessage(partialArgs), recipientID, remoteID)
	if err != nil {
		return nil, err
	}

	return map[string]any{"step_id": step.ID}, nil
}
