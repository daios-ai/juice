// SPDX-License-Identifier: AGPL-3.0-only

// Package llm provides language model and embedding adapters.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// ollamaClient is a shared HTTP client with a generous timeout so a hung Ollama
// server cannot block a juice call indefinitely. The timeout must accommodate large
// local models (e.g. the default 26B chat model) generating long completions —
// 120s was too tight and surfaced as
// "context deadline exceeded (Client.Timeout exceeded while awaiting headers)".
var ollamaClient = &http.Client{Timeout: 300 * time.Second}

// ollamaPost performs one Ollama JSON round trip: marshal, POST, status check, decode. Each stage
// failure names its stage, so an unreachable server, a rejection, and a bad reply stay tellable
// apart. The four entry points then differ only in payload and result shape.
func ollamaPost(ctx context.Context, url string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return kernel.ErrInternal.Wrapf("encode request: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return kernel.ErrInternal.Wrapf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ollamaClient.Do(req)
	if err != nil {
		return kernel.ErrInternal.Wrapf("HTTP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return kernel.ErrInternal.Wrapf("ollama returned status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return kernel.ErrInternal.Wrapf("decode response: %v", err)
	}
	return nil
}

// OllamaEmbedder calls the Ollama /api/embeddings endpoint.
type OllamaEmbedder struct {
	URL   string // e.g. "http://localhost:11434"
	Model string // e.g. "nomic-embed-text"
}

func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := ollamaPost(ctx, o.URL+"/api/embeddings", map[string]string{"model": o.Model, "prompt": text}, &result); err != nil {
		return nil, err
	}
	if len(result.Embedding) == 0 {
		return nil, kernel.ErrInternal.Wrap("empty embedding returned")
	}
	return result.Embedding, nil
}

// ollamaMsg is the wire shape of one chat message; ollamaMessages projects the kernel's
// messages onto it for the two plain /api/chat entry points.
type ollamaMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func ollamaMessages(messages []kernel.ChatMessage) []ollamaMsg {
	var out []ollamaMsg // nil for no messages, so the request body is unchanged
	for _, m := range messages {
		out = append(out, ollamaMsg{Role: m.Role, Content: m.Content})
	}
	return out
}

// OllamaChatter calls the Ollama /api/chat endpoint.
type OllamaChatter struct {
	URL   string
	Model string
}

func (o *OllamaChatter) Chat(ctx context.Context, messages []kernel.ChatMessage) (kernel.ChatMessage, error) {
	msgs := ollamaMessages(messages)
	var result struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := ollamaPost(ctx, o.URL+"/api/chat", map[string]any{"model": o.Model, "messages": msgs, "stream": false}, &result); err != nil {
		return kernel.ChatMessage{}, err
	}
	return kernel.ChatMessage{Role: result.Message.Role, Content: result.Message.Content}, nil
}

// ChatJSON calls /api/chat with a JSON schema format constraint.
// It returns the raw parsed JSON value; schema validation is the caller's responsibility.
func (o *OllamaChatter) ChatJSON(ctx context.Context, messages []kernel.ChatMessage, schema map[string]any) (any, error) {
	msgs := ollamaMessages(messages)
	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := ollamaPost(ctx, o.URL+"/api/chat", map[string]any{"model": o.Model, "messages": msgs, "stream": false, "format": schema}, &result); err != nil {
		return nil, err
	}
	content := extractFirstJSON(stripCodeFence(result.Message.Content))
	var value any
	// Use Decoder instead of Unmarshal so trailing garbage (backticks, text) is ignored.
	if err := json.NewDecoder(strings.NewReader(content)).Decode(&value); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("model did not return valid JSON: %v", err)
	}
	return value, nil
}

// stripCodeFence removes markdown code fences (```...```) from a string, returning
// the inner content. If no fence is present the original string is returned unchanged.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line (e.g. "```json\n").
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	// Drop the closing fence.
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// extractFirstJSON advances past any leading non-JSON text (comments, prose) to
// the first '{' character so JSON parsing succeeds even when the model preambles.
func extractFirstJSON(s string) string {
	if i := strings.IndexByte(s, '{'); i > 0 {
		return s[i:]
	}
	return s
}

// sanitizeToolName converts an action reference like "sys/llm/chat" (or a kernel-qualified
// "bob@acme/foo") to a valid Ollama/OpenAI function name by replacing "@" with "_at_" and "/"
// with "__" — e.g. "sys__llm__chat", "bob_at_acme__foo". The nameToRef map restores the original
// reference on decode, so the transform only needs to be injective across a single tool list.
func sanitizeToolName(ref string) string {
	s := strings.ReplaceAll(ref, "@", "_at_")
	return strings.ReplaceAll(s, "/", "__")
}

// ChatDecide calls /api/chat with tool definitions and returns the LLM's single chosen action.
func (o *OllamaChatter) ChatDecide(ctx context.Context, messages []kernel.DecideMessage, tools []kernel.ToolDefinition) (*kernel.ToolCall, *kernel.ChatMessage, error) {
	// Map sanitized function name → original action reference for response decoding.
	nameToRef := make(map[string]string, len(tools))
	for _, t := range tools {
		nameToRef[sanitizeToolName(t.Action)] = t.Action
	}

	type ollamaToolCall struct {
		Function struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"function"`
	}
	type ollamaMsgWithTools struct {
		Role      string           `json:"role"`
		Content   string           `json:"content"`
		ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
		ToolName  string           `json:"tool_name,omitempty"`
	}

	var msgs []ollamaMsgWithTools
	lastWasAssistantToolCall := false
	for _, m := range messages {
		switch {
		case m.Role == "assistant" && m.Tool != nil:
			var tc ollamaToolCall
			tc.Function.Name = sanitizeToolName(m.Tool.Action)
			tc.Function.Arguments = m.Tool.Args
			msgs = append(msgs, ollamaMsgWithTools{Role: "assistant", ToolCalls: []ollamaToolCall{tc}})
			lastWasAssistantToolCall = true
		case m.Role == "tool" && m.Tool != nil:
			content, _ := json.Marshal(m.Tool.Result)
			if lastWasAssistantToolCall {
				msgs = append(msgs, ollamaMsgWithTools{
					Role:     "tool",
					ToolName: sanitizeToolName(m.Tool.Action),
					Content:  string(content),
				})
			} else {
				msgs = append(msgs, ollamaMsgWithTools{Role: "user", Content: "Result: " + string(content)})
			}
			lastWasAssistantToolCall = false
		default:
			msgs = append(msgs, ollamaMsgWithTools{Role: m.Role, Content: m.Content})
			lastWasAssistantToolCall = false
		}
	}
	msgs = append(msgs, ollamaMsgWithTools{Role: "user", Content: "Call the appropriate tool now."})

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
	type ollamaTool struct {
		Type     string `json:"type"`
		Function fn     `json:"function"`
	}

	var ollamaTools []ollamaTool
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
		ollamaTools = append(ollamaTools, ollamaTool{
			Type:     "function",
			Function: fn{Name: sanitizeToolName(t.Action), Description: t.Description, Parameters: params},
		})
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
	payload := map[string]any{"model": o.Model, "messages": msgs, "tools": ollamaTools, "stream": false}
	if err := ollamaPost(ctx, o.URL+"/api/chat", payload, &result); err != nil {
		return nil, nil, err
	}

	if len(result.Message.ToolCalls) > 0 {
		tc := result.Message.ToolCalls[0]
		ref, ok := nameToRef[tc.Function.Name]
		if !ok {
			// Model may partially sanitize the name (e.g. "sys_llm/chat" instead of "sys_llm_chat").
			// Sanitize and try again before falling back to plain-text routing.
			ref, ok = nameToRef[sanitizeToolName(tc.Function.Name)]
		}
		if ok {
			return &kernel.ToolCall{Action: ref, Args: tc.Function.Arguments}, nil, nil
		}
		// Unrecognized name — treat as no tool call and use plain-text routing.
	}

	// Tool calling produced no result — fall back to plain-text routing.
	return o.chatDecideText(ctx, messages, tools)
}

// chatDecideText implements decide via plain-text chat and minimal string parsing.
// Used when the tool-calling path produces no tool_calls. Plain-text avoids the
// grammar-constraint stalls that JSON-mode causes on Ollama with accumulated context.
//
// The model is asked to output exactly one line:
//
//	"lookup:<search query>"  → select @sys/lookup
//	"<sanitized-name>"       → select that action (e.g. "sys_llm_chat")
func (o *OllamaChatter) chatDecideText(ctx context.Context, messages []kernel.DecideMessage, tools []kernel.ToolDefinition) (*kernel.ToolCall, *kernel.ChatMessage, error) {
	// Build sanitized name map and action descriptions.
	sanitizedToRef := make(map[string]string, len(tools))
	var actionDescs strings.Builder
	for _, t := range tools {
		san := sanitizeToolName(t.Action)
		sanitizedToRef[san] = t.Action
		actionDescs.WriteString("- " + san + ": " + t.Description + "\n")
	}

	// Build a concise user message: task + brief history. Skip system messages (too long).
	var taskDesc string
	var historyParts []string
	for _, m := range messages {
		switch {
		case m.Role == "system":
			// Skip — TinyGo SDK context is enormous; irrelevant for routing.
		case m.Role == "user" && m.Tool == nil && taskDesc == "":
			taskDesc = m.Content
		case m.Role == "assistant" && m.Tool != nil:
			historyParts = append(historyParts, "Called: "+m.Tool.Action)
		case m.Role == "tool" && m.Tool != nil:
			r, _ := json.Marshal(m.Tool.Result)
			snippet := string(r)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			historyParts = append(historyParts, "Result of "+m.Tool.Action+": "+snippet)
		}
	}

	userContent := taskDesc
	if len(historyParts) > 0 {
		userContent += "\n\nHistory:\n" + strings.Join(historyParts, "\n")
	}
	userContent += "\n\nWhat is the next action?"

	systemPrompt := "Output ONLY one line — no explanation:\n" +
		"  lookup:<search query>   (to search for existing actions)\n" +
		"  OR the action name      (e.g. sys_llm_chat to generate code)\n\n" +
		"Available actions:\n" + actionDescs.String()

	chatMsgs := []kernel.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent},
	}

	reply, err := o.Chat(ctx, chatMsgs)
	if err != nil {
		return nil, nil, kernel.ErrInternal.Wrapf("chat decide text: %v", err)
	}

	line := strings.TrimSpace(reply.Content)
	// Strip markdown fences or extra lines — take only the first non-empty line.
	for _, l := range strings.Split(line, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "```") {
			line = l
			break
		}
	}

	// "lookup:<query>" → @sys/lookup
	lowered := strings.ToLower(line)
	for san, ref := range sanitizedToRef {
		if strings.HasPrefix(lowered, strings.ToLower(san)+":") {
			query := strings.TrimSpace(line[len(san)+1:])
			return &kernel.ToolCall{Action: ref, Args: map[string]any{"query": query}}, nil, nil
		}
	}
	if strings.HasPrefix(lowered, "lookup:") {
		query := strings.TrimSpace(line[7:])
		for san, ref := range sanitizedToRef {
			if strings.Contains(san, "lookup") {
				return &kernel.ToolCall{Action: ref, Args: map[string]any{"query": query}}, nil, nil
			}
		}
	}

	// Plain action name mentioned anywhere in the response.
	for san, ref := range sanitizedToRef {
		if strings.Contains(lowered, strings.ToLower(san)) {
			return &kernel.ToolCall{Action: ref, Args: map[string]any{}}, nil, nil
		}
	}

	return nil, nil, kernel.ErrExecutionFailed.Wrap("model did not select an action")
}
