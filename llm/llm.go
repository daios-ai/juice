// SPDX-License-Identifier: AGPL-3.0-only

// Package llm reaches language models. An endpoint is data — a file naming its wire protocol, its
// URL and the models this installation uses there (D17) — and a protocol is code: one adaptor for
// the OpenAI-compatible API, which nearly every provider and local runner speaks, and one for
// Anthropic's Messages API, the one major API that differs.
package llm

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// The wire protocols an endpoint may name, and the kinds of model it may hold.
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
	KindChat          = "chat"
	KindEmbed         = "embed"
)

// anthropicMaxTokens is the output cap sent when a model names none: Anthropic requires one.
const anthropicMaxTokens = 4096

//go:embed endpoints
var shipped embed.FS

// Shipped holds the endpoint files this build ships. They are written into an installation's llm/
// directory when absent and never overwritten, as worlds are, so an operator's edit outlives an
// upgrade and an endpoint of their own is a file they add.
func Shipped() fs.FS {
	sub, err := fs.Sub(shipped, "endpoints")
	if err != nil {
		panic(err) // the directory is embedded above; its absence is a build error
	}
	return sub
}

// Endpoint is one file of $JUICE_HOME/llm/: where a provider is, how it is spoken to, and which of
// its models this installation uses. A model is listed under a short name of the operator's, since
// its own id may carry characters an action name cannot (gemma4:26b, vendor/model).
type Endpoint struct {
	Schema      string           `json:"$schema,omitempty" default:"-" doc:"The JSON Schema describing this file, for editors."`
	Protocol    string           `json:"protocol" enum:"openai,anthropic" default:"-" doc:"How the provider is spoken to: openai (the OpenAI-compatible API) or anthropic (Anthropic's Messages API)."`
	URL         string           `json:"url" default:"-" doc:"The API's base address, e.g. https://api.openai.com/v1."`
	KeyRequired bool             `json:"key_required" doc:"The provider needs an API key, set in config.json under native.llm.endpoints.<this file's name>.key."`
	Models      map[string]Model `json:"models" doc:"The models used here, each under a short name of your own (lowercase, at most 32 characters)."`
}

// Model is one model of an endpoint: the provider's id for it, what it is for, and its output cap
// (0 leaves the provider's own, except on Anthropic, which requires one).
type Model struct {
	ID        string `json:"id" doc:"The provider's own name for the model, e.g. gemma4:26b or claude-opus-5-5."`
	Kind      string `json:"kind" enum:"chat,embed" doc:"chat (backs chat and decide) or embed (backs embeddings; not on anthropic)."`
	MaxTokens int    `json:"max_tokens" doc:"Most tokens a reply may hold; 0 leaves the provider's own, except anthropic, which uses 4096."`
}

// nameRe is what an endpoint or model name may be: one short segment of an action name
// (llm/<endpoint>/<model>/chat), never shaped like an id or a key.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// Load reads every endpoint file in dir, keyed by name. It is strict, as world files and config.json
// are: a key this build does not know is a setting that would silently have no effect, so the
// refusal names the file.
func Load(dir string) (map[string]Endpoint, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	eps := make(map[string]Endpoint, len(paths))
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		ep, err := readEndpoint(path)
		if err == nil {
			err = ep.validate(name)
		}
		if err != nil {
			return nil, fmt.Errorf("endpoint file %s: %w", path, err)
		}
		eps[name] = ep
	}
	return eps, nil
}

func readEndpoint(path string) (Endpoint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Endpoint{}, err
	}
	var ep Endpoint
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ep); err != nil {
		return Endpoint{}, err
	}
	if dec.More() {
		return Endpoint{}, fmt.Errorf("more than one document")
	}
	return ep, nil
}

func (e Endpoint) validate(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("the name %q must be lowercase letters, digits, '.', '_' or '-', at most 32", name)
	}
	if e.Protocol != ProtocolOpenAI && e.Protocol != ProtocolAnthropic {
		return fmt.Errorf("protocol %q is not one this build speaks (%q or %q)", e.Protocol, ProtocolOpenAI, ProtocolAnthropic)
	}
	if e.URL == "" {
		return fmt.Errorf("url is required")
	}
	for m, model := range e.Models {
		switch {
		case !nameRe.MatchString(m):
			return fmt.Errorf("model name %q must be lowercase letters, digits, '.', '_' or '-', at most 32", m)
		case model.ID == "":
			return fmt.Errorf("model %q names no id", m)
		case model.Kind != KindChat && model.Kind != KindEmbed:
			return fmt.Errorf("model %q has kind %q; a model is %q or %q", m, model.Kind, KindChat, KindEmbed)
		case model.Kind == KindEmbed && e.Protocol == ProtocolAnthropic:
			return fmt.Errorf("model %q: the anthropic protocol has no embeddings", m)
		case model.MaxTokens < 0:
			return fmt.Errorf("model %q: max_tokens must not be negative", m)
		}
	}
	return nil
}

// Client is one model on one endpoint, holding the endpoint's key. It is every capability the llm
// natives and the kernel ask of a model; which it is used for is decided by the model's kind.
type Client struct {
	Endpoint Endpoint
	Model    Model
	Key      string
}

// httpClient's timeout accommodates a large local model generating a long completion. It follows
// no redirect: a redirect would carry the key, and on a 307 or 308 the prompt, to wherever it
// points, and an endpoint's address is the operator's to write as the final one. The redirect
// response itself is returned, and refused as any reply other than 200 is.
var httpClient = &http.Client{
	Timeout:       300 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// do performs one JSON round trip under the protocol's headers; a nil payload is a GET and a nil
// out discards the reply. Each stage failure names its stage, so an unreachable server, a refusal
// and a bad reply stay tellable apart.
func (c *Client) do(ctx context.Context, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return kernel.ErrInternal.Wrapf("encode request: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Endpoint.URL, "/")+path, body)
	if err != nil {
		return kernel.ErrInternal.Wrapf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Endpoint.Protocol == ProtocolAnthropic {
		req.Header.Set("x-api-key", c.Key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return kernel.ErrInternal.Wrapf("HTTP: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return kernel.ErrInternal.Wrapf("endpoint returned status %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	// A reply is held to the bound every reply in the kernel is (D12); one byte past it is enough
	// to know it is over.
	reply, err := io.ReadAll(io.LimitReader(resp.Body, kernel.MaxReplyBytes+1))
	if err != nil {
		return kernel.ErrInternal.Wrapf("read response: %v", err)
	}
	if len(reply) > kernel.MaxReplyBytes {
		return kernel.ErrInternal.Wrapf("reply exceeds %d bytes", kernel.MaxReplyBytes)
	}
	if err := json.Unmarshal(reply, out); err != nil {
		return kernel.ErrInternal.Wrapf("decode response: %v", err)
	}
	return nil
}

// Probe lists an endpoint's models. It is a diagnostic logged at boot: it proves the endpoint
// answers and accepts the key, not that a model works, so it creates and removes nothing.
func Probe(ctx context.Context, ep Endpoint, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return (&Client{Endpoint: ep, Key: key}).do(ctx, http.MethodGet, "/models", nil, nil)
}

// ---- requests and replies, per protocol ----

type wireMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIReply struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"` // a JSON document, as a string
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

type anthropicReply struct {
	Content []struct {
		Type  string         `json:"type"`
		Text  string         `json:"text"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	} `json:"content"`
}

// request is the body every completion starts from. Anthropic takes the system prompt beside the
// conversation rather than in it, and requires an output cap.
func (c *Client) request(messages []kernel.ChatMessage) map[string]any {
	req := map[string]any{"model": c.Model.ID}
	var system []string
	msgs := make([]wireMsg, 0, len(messages))
	for _, m := range messages {
		if c.Endpoint.Protocol == ProtocolAnthropic && m.Role == "system" {
			system = append(system, m.Content)
			continue
		}
		msgs = append(msgs, wireMsg{Role: m.Role, Content: m.Content})
	}
	req["messages"] = msgs
	if len(system) > 0 {
		req["system"] = strings.Join(system, "\n")
	}
	if c.Model.MaxTokens > 0 {
		req["max_tokens"] = c.Model.MaxTokens
	} else if c.Endpoint.Protocol == ProtocolAnthropic {
		req["max_tokens"] = anthropicMaxTokens
	}
	return req
}

// complete asks for one reply, constrained to schema when one is given, and returns its text.
func (c *Client) complete(ctx context.Context, messages []kernel.ChatMessage, schema map[string]any) (string, error) {
	req := c.request(messages)
	if c.Endpoint.Protocol == ProtocolAnthropic {
		if schema != nil {
			req["output_config"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": structuredSubset(schema)}}
		}
		var out anthropicReply
		if err := c.do(ctx, http.MethodPost, "/messages", req, &out); err != nil {
			return "", err
		}
		var text strings.Builder
		for _, b := range out.Content {
			if b.Type == "text" {
				text.WriteString(b.Text)
			}
		}
		return text.String(), nil
	}
	if schema != nil {
		// Not strict: strict mode demands every property be required, which a canonical schema
		// need not say. The reply is validated against the whole schema by the native either way.
		req["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "output", "schema": schema}}
	}
	var out openAIReply
	if err := c.do(ctx, http.MethodPost, "/chat/completions", req, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", kernel.ErrInternal.Wrap("reply carried no choice")
	}
	return out.Choices[0].Message.Content, nil
}

// selectTool offers the tools and returns the one the model called, "" when it called none. The
// choice is left to the model ("auto"): Anthropic's newest models refuse a forced tool choice, and
// a reply that calls nothing falls back to plain-text routing.
func (c *Client) selectTool(ctx context.Context, messages []kernel.ChatMessage, tools []kernel.ToolDefinition, names []string) (string, map[string]any, error) {
	req := c.request(messages)
	defs := make([]map[string]any, 0, len(tools))
	for i, t := range tools {
		// An action's input schema is a standard tool parameter schema (D4), so the model receives
		// it whole — bounds, enums, nullability and closed objects included.
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object"}
		}
		if c.Endpoint.Protocol == ProtocolAnthropic {
			defs = append(defs, map[string]any{"name": names[i], "description": t.Description, "input_schema": params})
		} else {
			defs = append(defs, map[string]any{"type": "function", "function": map[string]any{
				"name": names[i], "description": t.Description, "parameters": params}})
		}
	}
	req["tools"] = defs
	if c.Endpoint.Protocol == ProtocolAnthropic {
		req["tool_choice"] = map[string]any{"type": "auto"}
		var out anthropicReply
		if err := c.do(ctx, http.MethodPost, "/messages", req, &out); err != nil {
			return "", nil, err
		}
		for _, b := range out.Content {
			if b.Type == "tool_use" {
				return b.Name, b.Input, nil
			}
		}
		return "", nil, nil
	}
	var out openAIReply
	if err := c.do(ctx, http.MethodPost, "/chat/completions", req, &out); err != nil {
		return "", nil, err
	}
	if len(out.Choices) == 0 || len(out.Choices[0].Message.ToolCalls) == 0 {
		return "", nil, nil
	}
	fn := out.Choices[0].Message.ToolCalls[0].Function
	var args map[string]any
	if err := json.Unmarshal([]byte(fn.Arguments), &args); err != nil {
		return "", nil, nil // arguments that are not a JSON object are no call
	}
	return fn.Name, args, nil
}

// structuredOutputRefuses holds the keywords Anthropic's structured outputs refuse. They are removed
// from what the model is asked for, never from what its reply is held to: the native validates the
// reply against the whole schema.
var structuredOutputRefuses = map[string]bool{
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true,
	"minLength": true, "maxLength": true, "pattern": true, "format": true,
	"minItems": true, "maxItems": true, "uniqueItems": true,
}

// structuredSubset copies a canonical schema without those keywords. It descends only where a
// schema nests one (properties, items, additionalProperties), so a property named "format" and data
// in "default" or "examples" are kept as written.
func structuredSubset(s map[string]any) map[string]any {
	sub := func(v any) any {
		if m, ok := v.(map[string]any); ok {
			return structuredSubset(m)
		}
		return v
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		if structuredOutputRefuses[k] {
			continue
		}
		switch k {
		case "properties":
			if props, ok := v.(map[string]any); ok {
				kept := make(map[string]any, len(props))
				for name, p := range props {
					kept[name] = sub(p)
				}
				v = kept
			}
		case "items", "additionalProperties":
			v = sub(v)
		}
		out[k] = v
	}
	return out
}

// ---- the capabilities ----

// Chat returns the model's reply to a conversation.
func (c *Client) Chat(ctx context.Context, messages []kernel.ChatMessage) (kernel.ChatMessage, error) {
	text, err := c.complete(ctx, messages, nil)
	if err != nil {
		return kernel.ChatMessage{}, err
	}
	return kernel.ChatMessage{Role: "assistant", Content: text}, nil
}

// ChatJSON returns the model's reply parsed as JSON, asked for under schema. Validation against the
// schema is the caller's.
func (c *Client) ChatJSON(ctx context.Context, messages []kernel.ChatMessage, schema map[string]any) (any, error) {
	text, err := c.complete(ctx, messages, schema)
	if err != nil {
		return nil, err
	}
	var value any
	// A Decoder rather than Unmarshal, so trailing text after the document is ignored.
	if err := json.NewDecoder(strings.NewReader(extractFirstJSON(stripCodeFence(text)))).Decode(&value); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("model did not return valid JSON: %v", err)
	}
	return value, nil
}

// Embed returns the model's embedding of text. Only the openai protocol has embeddings; Load refuses
// an embed model on any other.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/embeddings", map[string]any{"model": c.Model.ID, "input": text}, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, kernel.ErrInternal.Wrap("empty embedding returned")
	}
	return out.Data[0].Embedding, nil
}

// ChatDecide asks the model to call one of tools and returns the action it chose. A model that calls
// none, or one the list does not name, is asked again in plain text.
func (c *Client) ChatDecide(ctx context.Context, messages []kernel.DecideMessage, tools []kernel.ToolDefinition) (*kernel.ToolCall, *kernel.ChatMessage, error) {
	names := toolNames(tools)
	msgs := append(decideHistory(messages), kernel.ChatMessage{Role: "user", Content: "Call the appropriate tool now."})
	name, args, err := c.selectTool(ctx, msgs, tools, names)
	if err != nil {
		return nil, nil, err
	}
	if i := slices.Index(names, name); i >= 0 {
		return &kernel.ToolCall{Action: tools[i].Action, Args: args}, nil, nil
	}
	return c.chatDecideText(ctx, messages, tools, names)
}

// decideHistory renders a decide conversation as plain turns. Earlier calls and their results are
// context for one fresh choice, not calls to replay, so both protocols receive them as text and no
// tool-call ids need be kept in step.
func decideHistory(messages []kernel.DecideMessage) []kernel.ChatMessage {
	out := make([]kernel.ChatMessage, 0, len(messages)+1)
	for _, m := range messages {
		switch {
		case m.Role == "assistant" && m.Tool != nil:
			args, _ := json.Marshal(m.Tool.Args)
			out = append(out, kernel.ChatMessage{Role: "assistant", Content: "Called " + m.Tool.Action + " with " + string(args)})
		case m.Role == "tool" && m.Tool != nil:
			result, _ := json.Marshal(m.Tool.Result)
			out = append(out, kernel.ChatMessage{Role: "user", Content: "Result of " + m.Tool.Action + ": " + string(result)})
		case m.Role == "tool":
			out = append(out, kernel.ChatMessage{Role: "user", Content: m.Content})
		default:
			out = append(out, kernel.ChatMessage{Role: m.Role, Content: m.Content})
		}
	}
	return out
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

// toolNameUnsafe is every character a provider's tool name may not hold.
var toolNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// toolNames names each candidate for the model: its address with every unsafe character made '_',
// cut to 48 characters, and suffixed _1 to _N by position. The suffix alone keeps the names
// distinct however the addresses read, and the cut keeps them within every provider's length
// limit; a chosen name is mapped back by its position, never by decoding it.
func toolNames(tools []kernel.ToolDefinition) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		base := toolNameUnsafe.ReplaceAllString(t.Action, "_")
		if len(base) > 48 {
			base = base[:48]
		}
		names[i] = base + "_" + strconv.Itoa(i+1)
	}
	return names
}

// chatDecideText implements decide via plain-text chat and minimal string parsing.
// Used when the tool-calling path produces no tool call, which models without tool calling do.
//
// The model is asked to output exactly one line:
//
//	"lookup:<search query>"  → select the lookup action
//	"<tool-name>"            → select that action (e.g. "sys_k_llm_chat_3")
func (c *Client) chatDecideText(ctx context.Context, messages []kernel.DecideMessage, tools []kernel.ToolDefinition, names []string) (*kernel.ToolCall, *kernel.ChatMessage, error) {
	var actionDescs strings.Builder
	for i, t := range tools {
		actionDescs.WriteString("- " + names[i] + ": " + t.Description + "\n")
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
		"  OR the action name      (e.g. " + names[0] + ")\n\n" +
		"Available actions:\n" + actionDescs.String()

	chatMsgs := []kernel.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent},
	}

	reply, err := c.Chat(ctx, chatMsgs)
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

	// "<name>:<query>" or "lookup:<query>" → that action, or the lookup action, with the query.
	lowered := strings.ToLower(line)
	if head, query, ok := strings.Cut(line, ":"); ok {
		head = strings.ToLower(strings.TrimSpace(head))
		for i, t := range tools {
			if head == strings.ToLower(names[i]) || head == "lookup" && strings.HasSuffix(t.Action, "/lookup") {
				return &kernel.ToolCall{Action: t.Action, Args: map[string]any{"query": strings.TrimSpace(query)}}, nil, nil
			}
		}
	}

	// A tool name standing as a whole word anywhere in the reply, so _1 never matches inside _12.
	for _, word := range strings.FieldsFunc(lowered, func(r rune) bool { return toolNameUnsafe.MatchString(string(r)) }) {
		for i, t := range tools {
			if word == strings.ToLower(names[i]) {
				return &kernel.ToolCall{Action: t.Action, Args: map[string]any{}}, nil, nil
			}
		}
	}

	return nil, nil, kernel.ErrExecutionFailed.Wrap("model did not select an action")
}
