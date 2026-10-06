// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// JSON declares a structured-output native (§9): llm/json, which configuration binds to a model,
// or the native of one generated model (D17). It answers with the object the caller's schema
// admits, never text: an action has one output kind (D4), and text is llm/chat's.
func JSON(model string, m kernel.JSONChatter) Spec {
	return Spec{
		Name:        llmName(model, "json"),
		Title:       "Get JSON from " + llmTitle(model),
		Description: "Structured output from " + llmBy(model) + ": the object the schema describes, checked against it before it is returned",
		InputSchema: obj(map[string]any{
			"messages": arrayOf(messageSchema(), "Conversation history; a system prompt is a message with role system"),
			"schema":   object("JSON Schema of the object to return; its top level is an object"),
		}, "messages", "schema"),
		OutputSchema: obj(map[string]any{"value": object("The object the schema admits")}),
		Handler: func(Host) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
				return executeJSON(ctx, args, m)
			}
		},
	}
}

func executeJSON(ctx context.Context, args map[string]any, m kernel.JSONChatter) (map[string]any, error) {
	if m == nil {
		return nil, kernel.ErrInvalidState.Wrap("no structured-output model is bound")
	}
	messages, err := chatMessages(args)
	if err != nil {
		return nil, err
	}
	raw, ok := args["schema"].(map[string]any)
	if !ok {
		return nil, kernel.ErrInvalidInput.Wrap("llm/json requires a schema object")
	}
	// The schema is read in the canonical form, so the model is asked for, and the reply held to,
	// exactly what any action's schema means (D4).
	schema, _, err := kernel.NormalizeSchema("schema", raw)
	if err != nil {
		return nil, err
	}
	value, err := m.ChatJSON(ctx, messages, schema)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("structured output failed: %v", err)
	}
	if err := kernel.ValidateInput(schema, value); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("model output failed schema validation: %v", err)
	}
	return map[string]any{"value": value}, nil
}
