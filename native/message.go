package native

import (
	"context"
	"encoding/json"

	"github.com/daios-ai/juice/kernel"
)

// Message declares @sys/message (§9): delivery by parking a Step the recipient must acknowledge.
func Message() Spec {
	return Spec{
		Name:        "message",
		Description: "Sends a message to another platform user and creates a Step they must acknowledge",
		InputSchema: obj(map[string]any{
			"to":      str("Recipient handle"),
			"message": str("Message body"),
		}, "to", "message"),
		OutputSchema: obj(map[string]any{"step_id": str("ID of the created step")}),
		Handler: func(k Host) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, callerID, _, _, parentTraceID string) (map[string]any, error) {
				return executeMessage(ctx, args, callerID, parentTraceID, k)
			}
		},
	}
}

func executeMessage(ctx context.Context, args map[string]any, callerID, parentTraceID string, k Host) (map[string]any, error) {
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

	sink, err := k.ReadCallableAction(ctx, kernel.SuperuserHandle+"/sink", callerID)
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
