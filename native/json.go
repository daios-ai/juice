package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// JSON declares @sys/llm/json (§9): structured output validated locally against the caller's schema.
func JSON(chatter kernel.JSONChatter) Spec {
	return Spec{
		Name:        "llm/json",
		Description: "Structured JSON output from the configured language model, locally validated against a schema",
		InputSchema: obj(map[string]any{
			"messages":      arrayOf(messageSchema(), "Conversation history"),
			"system":        str("Optional system prompt"),
			"output_schema": object("JSON Schema the model output must satisfy"),
		}, "messages", "output_schema"),
		OutputSchema: obj(map[string]any{"value": object("JSON value conforming to output_schema")}),
		Handler: func(*kernel.Kernel) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeJSON(ctx, args, chatter)
			}
		},
	}
}

func executeJSON(ctx context.Context, args map[string]any, chatter kernel.JSONChatter) (map[string]any, error) {
	if chatter == nil {
		return nil, kernel.ErrInvalidState.Wrap("json chat service not configured")
	}

	messages, err := chatMessages(args, "llm/json")
	if err != nil {
		return nil, err
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
