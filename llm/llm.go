// Package llm provides language model and embedding adapters.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/daios-ai/juice/kernel"
)

// OllamaEmbedder calls the Ollama /api/embeddings endpoint.
type OllamaEmbedder struct {
	URL   string // e.g. "http://localhost:11434"
	Model string // e.g. "nomic-embed-text"
}

func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]string{"model": o.Model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, kernel.ErrInternal.Wrapf("embed request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, kernel.ErrInternal.Wrapf("embed HTTP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, kernel.ErrInternal.Wrapf("ollama returned status %d", resp.StatusCode)
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, kernel.ErrInternal.Wrapf("decode embed response: %v", err)
	}
	if len(result.Embedding) == 0 {
		return nil, kernel.ErrInternal.Wrap("empty embedding returned")
	}
	return result.Embedding, nil
}

// OllamaChatter calls the Ollama /api/chat endpoint.
type OllamaChatter struct {
	URL   string
	Model string
}

func (o *OllamaChatter) Chat(ctx context.Context, messages []kernel.ChatMessage) (kernel.ChatMessage, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var msgs []msg
	for _, m := range messages {
		msgs = append(msgs, msg{Role: m.Role, Content: m.Content})
	}
	body, _ := json.Marshal(map[string]any{"model": o.Model, "messages": msgs, "stream": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return kernel.ChatMessage{}, kernel.ErrInternal.Wrapf("chat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return kernel.ChatMessage{}, kernel.ErrInternal.Wrapf("chat HTTP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return kernel.ChatMessage{}, kernel.ErrInternal.Wrapf("ollama chat returned status %d", resp.StatusCode)
	}

	var result struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return kernel.ChatMessage{}, kernel.ErrInternal.Wrapf("decode chat response: %v", err)
	}
	return kernel.ChatMessage{Role: result.Message.Role, Content: result.Message.Content}, nil
}

// ChatJSON calls /api/chat with a JSON schema format constraint.
// It returns the raw parsed JSON value; schema validation is the caller's responsibility.
func (o *OllamaChatter) ChatJSON(ctx context.Context, messages []kernel.ChatMessage, schema map[string]any) (any, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var msgs []msg
	for _, m := range messages {
		msgs = append(msgs, msg{Role: m.Role, Content: m.Content})
	}
	body, _ := json.Marshal(map[string]any{"model": o.Model, "messages": msgs, "stream": false, "format": schema})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, kernel.ErrInternal.Wrapf("chat json request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, kernel.ErrInternal.Wrapf("chat json HTTP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, kernel.ErrInternal.Wrapf("ollama chat json returned status %d", resp.StatusCode)
	}

	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, kernel.ErrInternal.Wrapf("decode chat json response: %v", err)
	}
	var value any
	if err := json.Unmarshal([]byte(result.Message.Content), &value); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("model did not return valid JSON: %v", err)
	}
	return value, nil
}

// ChatTools calls /api/chat with tool definitions and returns proposed tool calls.
func (o *OllamaChatter) ChatTools(ctx context.Context, messages []kernel.ChatMessage, tools []kernel.ToolDefinition, toolChoice string, maxToolCalls int) ([]kernel.ToolCall, *kernel.ChatMessage, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var msgs []msg
	for _, m := range messages {
		msgs = append(msgs, msg{Role: m.Role, Content: m.Content})
	}

	type fnParams struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties,omitempty"`
		Required   []string       `json:"required,omitempty"`
	}
	type fn struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Parameters  fnParams `json:"parameters"`
	}
	type tool struct {
		Type     string `json:"type"`
		Function fn     `json:"function"`
	}

	var ollamaTools []tool
	for _, t := range tools {
		params := fnParams{Type: "object"}
		if props, ok := t.InputSchema["properties"].(map[string]any); ok {
			params.Properties = props
		}
		if req, ok := t.InputSchema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					params.Required = append(params.Required, s)
				}
			}
		}
		ollamaTools = append(ollamaTools, tool{
			Type: "function",
			Function: fn{Name: t.Action, Description: t.Description, Parameters: params},
		})
	}

	payload := map[string]any{
		"model":    o.Model,
		"messages": msgs,
		"tools":    ollamaTools,
		"stream":   false,
	}
	if toolChoice != "" && toolChoice != "auto" {
		payload["tool_choice"] = toolChoice
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, nil, kernel.ErrInternal.Wrapf("chat tools request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, kernel.ErrInternal.Wrapf("chat tools HTTP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, kernel.ErrInternal.Wrapf("ollama chat tools returned status %d", resp.StatusCode)
	}

	var result struct {
		Message struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil, kernel.ErrInternal.Wrapf("decode chat tools response: %v", err)
	}

	var calls []kernel.ToolCall
	for _, tc := range result.Message.ToolCalls {
		if maxToolCalls > 0 && len(calls) >= maxToolCalls {
			break
		}
		calls = append(calls, kernel.ToolCall{Action: tc.Function.Name, Args: tc.Function.Arguments})
	}

	var replyMsg *kernel.ChatMessage
	if result.Message.Content != "" {
		replyMsg = &kernel.ChatMessage{Role: result.Message.Role, Content: result.Message.Content}
	}
	return calls, replyMsg, nil
}

