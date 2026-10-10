// SPDX-License-Identifier: AGPL-3.0-only

package llm

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// seen is one request a fake endpoint received.
type seen struct {
	path   string
	header http.Header
	body   map[string]any
}

// fakeEndpoint answers each path with its canned reply, in order, and records every request.
func fakeEndpoint(t *testing.T, replies map[string][]string) (*httptest.Server, *[]seen) {
	t.Helper()
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, seen{path: r.URL.Path, header: r.Header, body: body})
		queue := replies[r.URL.Path]
		if len(queue) == 0 {
			http.Error(w, "no reply", http.StatusNotFound)
			return
		}
		replies[r.URL.Path] = queue[1:]
		_, _ = w.Write([]byte(queue[0]))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func client(protocol, url, key string, model Model) *Client {
	return &Client{Endpoint: Endpoint{Protocol: protocol, URL: url}, Model: model, Key: key}
}

var user = []kernel.ChatMessage{{Role: "system", Content: "be brief"}, {Role: "user", Content: "ping"}}

// The timeout must let a large local model finish a long completion; 120s was not enough.
func TestClientTimeoutGenerous(t *testing.T) {
	if httpClient.Timeout < 300*time.Second {
		t.Errorf("timeout = %v, want >= 300s for large local models", httpClient.Timeout)
	}
}

// Every endpoint file this build ships loads as written, so a fresh installation boots.
func TestShippedEndpointsLoad(t *testing.T) {
	dir := t.TempDir()
	err := fs.WalkDir(Shipped(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(Shipped(), path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, path), body, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	eps, err := Load(dir)
	if err != nil {
		t.Fatalf("shipped endpoints do not load: %v", err)
	}
	ollama := eps["ollama"]
	gemma, _ := ollama.Model("gemma")
	nomic, _ := ollama.Model("nomic")
	if gemma.Kind != KindChat || nomic.Kind != KindEmbed || ollama.KeyRequired {
		t.Errorf("the default bindings' endpoint is not as configuration expects: %+v", ollama)
	}
	if !eps["anthropic"].KeyRequired || eps["anthropic"].Protocol != ProtocolAnthropic {
		t.Errorf("anthropic endpoint = %+v", eps["anthropic"])
	}
}

// An endpoint file that would silently mean something else is refused, naming the file.
func TestLoadRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":       `{"protocol":"openai","url":"u","modles":[]}`,
		"unknown protocol":  `{"protocol":"grpc","url":"u"}`,
		"no url":            `{"protocol":"openai"}`,
		"model without id":  `{"protocol":"openai","url":"u","models":[{"name":"m","kind":"chat"}]}`,
		"unknown kind":      `{"protocol":"openai","url":"u","models":[{"name":"m","id":"x","kind":"rerank"}]}`,
		"anthropic embed":   `{"protocol":"anthropic","url":"u","models":[{"name":"m","id":"x","kind":"embed"}]}`,
		"model name shape":  `{"protocol":"openai","url":"u","models":[{"name":"Big/One","id":"x","kind":"chat"}]}`,
		"negative cap":      `{"protocol":"openai","url":"u","models":[{"name":"m","id":"x","kind":"chat","max_tokens":-1}]}`,
		"two documents":     `{"protocol":"openai","url":"u"} {}`,
		"model name twice":  `{"protocol":"openai","url":"u","models":[{"name":"m","id":"x","kind":"chat"},{"name":"m","id":"y","kind":"chat"}]}`,
		"not json":          `protocol: openai`,
		"unknown model key": `{"protocol":"openai","url":"u","models":[{"name":"m","id":"x","kind":"chat","temp":1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "e.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "e.json") {
				t.Fatalf("got %v, want a refusal naming the file", err)
			}
		})
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Bad Name.json"), []byte(`{"protocol":"openai","url":"u"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("an endpoint name that cannot be an action segment was accepted")
	}
}

// The openai protocol: Chat Completions with a bearer key, system prompt in the conversation.
func TestOpenAIChat(t *testing.T) {
	srv, got := fakeEndpoint(t, map[string][]string{
		"/v1/chat/completions": {`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`}})
	c := client(ProtocolOpenAI, srv.URL+"/v1/", "sk", Model{ID: "m1", Kind: KindChat, MaxTokens: 50})
	reply, err := c.Chat(context.Background(), user)
	if err != nil || reply.Content != "pong" || reply.Role != "assistant" {
		t.Fatalf("Chat = %+v, %v", reply, err)
	}
	r := (*got)[0]
	if r.header.Get("Authorization") != "Bearer sk" || r.body["model"] != "m1" || r.body["max_tokens"] != float64(50) {
		t.Errorf("request = %v %v", r.header, r.body)
	}
	if msgs := r.body["messages"].([]any); len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("the system prompt must travel in the conversation: %v", msgs)
	}

	keyless, got := fakeEndpoint(t, map[string][]string{"/chat/completions": {`{"choices":[{"message":{"content":"x"}}]}`}})
	if _, err := client(ProtocolOpenAI, keyless.URL, "", Model{ID: "m"}).Chat(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	if h := (*got)[0].header.Get("Authorization"); h != "" {
		t.Errorf("a keyless endpoint was sent Authorization %q", h)
	}
}

// Structured output is asked for by response_format, not strict, with the schema whole.
func TestOpenAIChatJSON(t *testing.T) {
	srv, got := fakeEndpoint(t, map[string][]string{
		"/chat/completions": {"{\"choices\":[{\"message\":{\"content\":\"```json\\n{\\\"x\\\": 1}\\n```\"}}]}"}})
	schema := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "integer", "minimum": 0}}}
	v, err := client(ProtocolOpenAI, srv.URL, "", Model{ID: "m"}).ChatJSON(context.Background(), user, schema)
	if err != nil || v.(map[string]any)["x"] != float64(1) {
		t.Fatalf("ChatJSON = %v, %v", v, err)
	}
	rf := (*got)[0].body["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["strict"] != nil {
		t.Errorf("response_format = %v", rf)
	}
	if js["schema"].(map[string]any)["properties"].(map[string]any)["x"].(map[string]any)["minimum"] != float64(0) {
		t.Error("the openai protocol must receive the schema whole")
	}
}

// Selection offers each action as a function under a sanitized name and maps the call back; a model
// that calls nothing is asked again in plain text.
func TestOpenAIDecide(t *testing.T) {
	tools := []kernel.ToolDefinition{{Action: "bob@k/echo", Description: "Echo", InputSchema: map[string]any{"type": "object"}}}
	srv, got := fakeEndpoint(t, map[string][]string{"/chat/completions": {
		`{"choices":[{"message":{"tool_calls":[{"function":{"name":"bob_k_echo_1","arguments":"{\"say\":\"hi\"}"}}]}}]}`,
		`{"choices":[{"message":{"content":""}}]}`,
		`{"choices":[{"message":{"content":"bob_k_echo_1"}}]}`,
	}})
	c := client(ProtocolOpenAI, srv.URL, "", Model{ID: "m"})
	history := []kernel.DecideMessage{{Role: "user", Content: "echo hi"},
		{Role: "assistant", Tool: &kernel.DecideTool{Action: "bob@k/echo", Args: map[string]any{"say": "a"}}},
		{Role: "tool", Tool: &kernel.DecideTool{Action: "bob@k/echo", Result: map[string]any{"said": "a"}}}}
	call, _, err := c.ChatDecide(context.Background(), history, tools)
	if err != nil || call.Action != "bob@k/echo" || call.Args["say"] != "hi" {
		t.Fatalf("ChatDecide = %+v, %v", call, err)
	}
	fn := (*got)[0].body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bob_k_echo_1" || fn["parameters"] == nil {
		t.Errorf("tool = %v", fn)
	}
	msgs := (*got)[0].body["messages"].([]any)
	if len(msgs) != 4 || !strings.HasPrefix(msgs[1].(map[string]any)["content"].(string), "Called bob@k/echo") ||
		msgs[2].(map[string]any)["role"] != "user" {
		t.Errorf("history must travel as plain turns: %v", msgs)
	}

	call, _, err = c.ChatDecide(context.Background(), history[:1], tools)
	if err != nil || call == nil || call.Action != "bob@k/echo" {
		t.Fatalf("plain-text fallback = %+v, %v", call, err)
	}
}

// Every candidate gets its own name, however alike two addresses read once their unsafe characters
// are replaced, and every name fits a provider's limit.
func TestToolNamesAreDistinctAndShort(t *testing.T) {
	long := strings.Repeat("x", 200)
	names := toolNames([]kernel.ToolDefinition{{Action: "a@b_at_c/x"}, {Action: "a_at_b@c/x"}, {Action: "bob@k/" + long}})
	if names[0] == names[1] || names[0] != "a_b_at_c_x_1" || names[1] != "a_at_b_c_x_2" {
		t.Errorf("names = %v", names)
	}
	if len(names[2]) > 64 || !strings.HasSuffix(names[2], "_3") {
		t.Errorf("a long address named %q", names[2])
	}
}

// The model's choice maps back to the candidate it named, even where two addresses collide once
// sanitized; the plain-text fallback matches a whole name, so _1 is never read inside _12.
func TestDecideMapsBackByPosition(t *testing.T) {
	tools := make([]kernel.ToolDefinition, 12)
	for i := range tools {
		tools[i] = kernel.ToolDefinition{Action: "a@b_at_c/x", Description: "d", InputSchema: map[string]any{"type": "object"}}
	}
	tools[1].Action = "a_at_b@c/x"
	tools[11].Action = "z@k/twelfth"
	srv, _ := fakeEndpoint(t, map[string][]string{"/chat/completions": {
		`{"choices":[{"message":{"tool_calls":[{"function":{"name":"a_at_b_c_x_2","arguments":"{}"}}]}}]}`,
		`{"choices":[{"message":{"content":""}}]}`,
		`{"choices":[{"message":{"content":"I pick z_k_twelfth_12."}}]}`,
		`{"choices":[{"message":{"content":""}}]}`,
		`{"choices":[{"message":{"content":"lookup: weather"}}]}`,
	}})
	c := client(ProtocolOpenAI, srv.URL, "", Model{ID: "m"})
	ask := []kernel.DecideMessage{{Role: "user", Content: "go"}}
	if call, _, err := c.ChatDecide(context.Background(), ask, tools); err != nil || call.Action != "a_at_b@c/x" {
		t.Fatalf("tool call mapped to %+v, %v; want the second candidate", call, err)
	}
	if call, _, err := c.ChatDecide(context.Background(), ask, tools); err != nil || call.Action != "z@k/twelfth" {
		t.Fatalf("text fallback chose %+v, %v; want the twelfth candidate, not the first", call, err)
	}
	tools[4].Action = "sys@k/lookup"
	if call, _, err := c.ChatDecide(context.Background(), ask, tools); err != nil || call.Action != "sys@k/lookup" || call.Args["query"] != "weather" {
		t.Fatalf("lookup shortcut chose %+v, %v", call, err)
	}
}

func TestOpenAIEmbed(t *testing.T) {
	srv, got := fakeEndpoint(t, map[string][]string{"/embeddings": {`{"data":[{"index":0,"embedding":[0.25,0.5]}]}`}, "/x": nil})
	vec, err := client(ProtocolOpenAI, srv.URL, "", Model{ID: "nomic", Kind: KindEmbed}).Embed(context.Background(), "hello")
	if err != nil || len(vec) != 2 || vec[0] != 0.25 {
		t.Fatalf("Embed = %v, %v", vec, err)
	}
	if b := (*got)[0].body; b["model"] != "nomic" || b["input"] != "hello" {
		t.Errorf("request = %v", b)
	}
	empty, _ := fakeEndpoint(t, map[string][]string{"/embeddings": {`{"data":[]}`}})
	if _, err := client(ProtocolOpenAI, empty.URL, "", Model{ID: "m"}).Embed(context.Background(), "x"); err == nil {
		t.Error("an empty embedding was accepted")
	}
}

// The anthropic protocol: Messages with its own headers, the system prompt lifted beside the
// conversation, and an output cap always sent.
func TestAnthropicChat(t *testing.T) {
	srv, got := fakeEndpoint(t, map[string][]string{
		"/v1/messages": {`{"content":[{"type":"text","text":"po"},{"type":"text","text":"ng"}]}`}})
	reply, err := client(ProtocolAnthropic, srv.URL+"/v1", "ak", Model{ID: "claude"}).Chat(context.Background(), user)
	if err != nil || reply.Content != "pong" {
		t.Fatalf("Chat = %+v, %v", reply, err)
	}
	r := (*got)[0]
	if r.header.Get("x-api-key") != "ak" || r.header.Get("anthropic-version") == "" || r.header.Get("Authorization") != "" {
		t.Errorf("headers = %v", r.header)
	}
	if r.body["system"] != "be brief" || len(r.body["messages"].([]any)) != 1 || r.body["max_tokens"] != float64(anthropicMaxTokens) {
		t.Errorf("body = %v", r.body)
	}
}

// Structured output is asked for by output_config.format, with the keywords it refuses removed only
// where a schema nests — a property named "format" survives — and the reply parsed.
func TestAnthropicChatJSON(t *testing.T) {
	srv, got := fakeEndpoint(t, map[string][]string{"/messages": {`{"content":[{"type":"text","text":"{\"format\":\"a\"}"}]}`}})
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"format": map[string]any{"type": "string", "maxLength": 3, "examples": []any{map[string]any{"minimum": 1}}},
		"list":   map[string]any{"type": "array", "minItems": 2, "items": map[string]any{"type": "integer", "minimum": 0}},
	}}
	v, err := client(ProtocolAnthropic, srv.URL, "ak", Model{ID: "claude"}).ChatJSON(context.Background(), user, schema)
	if err != nil || v.(map[string]any)["format"] != "a" {
		t.Fatalf("ChatJSON = %v, %v", v, err)
	}
	format := (*got)[0].body["output_config"].(map[string]any)["format"].(map[string]any)
	sent := format["schema"].(map[string]any)
	props := sent["properties"].(map[string]any)
	f, list := props["format"].(map[string]any), props["list"].(map[string]any)
	if format["type"] != "json_schema" || f == nil || f["maxLength"] != nil || list["minItems"] != nil ||
		list["items"].(map[string]any)["minimum"] != nil || sent["additionalProperties"] != false {
		t.Errorf("schema sent = %v", sent)
	}
	if f["examples"].([]any)[0].(map[string]any)["minimum"] != float64(1) {
		t.Error("data inside examples was rewritten")
	}
	if schema["properties"].(map[string]any)["format"].(map[string]any)["maxLength"] != 3 {
		t.Error("the caller's schema was modified; the reply must be held to it whole")
	}
}

// Selection leaves the choice to the model (auto), since the newest models refuse a forced one.
func TestAnthropicDecide(t *testing.T) {
	srv, got := fakeEndpoint(t, map[string][]string{"/messages": {
		`{"content":[{"type":"text","text":"ok"},{"type":"tool_use","name":"bob_k_echo_1","input":{"say":"hi"}}]}`}})
	tools := []kernel.ToolDefinition{{Action: "bob@k/echo", Description: "Echo", InputSchema: map[string]any{"type": "object"}}}
	call, _, err := client(ProtocolAnthropic, srv.URL, "ak", Model{ID: "claude"}).ChatDecide(context.Background(),
		[]kernel.DecideMessage{{Role: "user", Content: "echo hi"}}, tools)
	if err != nil || call.Action != "bob@k/echo" || call.Args["say"] != "hi" {
		t.Fatalf("ChatDecide = %+v, %v", call, err)
	}
	b := (*got)[0].body
	tool := b["tools"].([]any)[0].(map[string]any)
	if b["tool_choice"].(map[string]any)["type"] != "auto" || tool["name"] != "bob_k_echo_1" || tool["input_schema"] == nil {
		t.Errorf("body = %v", b)
	}
}

// Probe proves an endpoint answers and accepts the key, under the protocol's headers.
func TestProbe(t *testing.T) {
	srv, got := fakeEndpoint(t, map[string][]string{"/v1/models": {`{"data":[]}`}})
	if err := Probe(context.Background(), Endpoint{Protocol: ProtocolAnthropic, URL: srv.URL + "/v1"}, "ak"); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if (*got)[0].header.Get("x-api-key") != "ak" {
		t.Error("the probe did not carry the key")
	}
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer refused.Close()
	if err := Probe(context.Background(), Endpoint{Protocol: ProtocolOpenAI, URL: refused.URL}, "bad"); err == nil {
		t.Error("a refused key probed as reachable")
	}
}

// A redirect is never followed: the host it names receives neither the key nor the prompt, and the
// call fails as any reply other than 200 does.
func TestRedirectIsNotFollowed(t *testing.T) {
	var reached bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer elsewhere.Close()
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, code)
		}))
		_, err := client(ProtocolAnthropic, moved.URL, "ak", Model{ID: "m"}).Chat(context.Background(), user)
		moved.Close()
		if err == nil || !strings.Contains(err.Error(), "status") {
			t.Errorf("redirect %d: got %v, want a refusal", code, err)
		}
	}
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	defer moved.Close()
	if err := Probe(context.Background(), Endpoint{Protocol: ProtocolAnthropic, URL: moved.URL}, "ak"); err == nil || reached {
		t.Errorf("the probe followed a redirect: err %v, reached %v", err, reached)
	}
}

// A reply over the kernel's bound is refused rather than read whole.
func TestOversizedReplyIsRefused(t *testing.T) {
	big := `{"choices":[{"message":{"content":"` + strings.Repeat("x", kernel.MaxReplyBytes) + `"}}]}`
	srv, _ := fakeEndpoint(t, map[string][]string{"/chat/completions": {big}})
	if _, err := client(ProtocolOpenAI, srv.URL, "", Model{ID: "m"}).Chat(context.Background(), user); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Errorf("got %v, want the reply refused for its size", err)
	}
}

// Each transport failure is an error naming its stage, never a reply.
func TestTransportFailures(t *testing.T) {
	refusing, _ := fakeEndpoint(t, map[string][]string{})
	if _, err := client(ProtocolOpenAI, refusing.URL, "", Model{ID: "m"}).Chat(context.Background(), user); err == nil ||
		!strings.Contains(err.Error(), "status 404") {
		t.Errorf("non-200: %v", err)
	}
	garbled, _ := fakeEndpoint(t, map[string][]string{"/chat/completions": {`not json`}})
	if _, err := client(ProtocolOpenAI, garbled.URL, "", Model{ID: "m"}).Chat(context.Background(), user); err == nil ||
		!strings.Contains(err.Error(), "decode") {
		t.Errorf("bad body: %v", err)
	}
	empty, _ := fakeEndpoint(t, map[string][]string{"/chat/completions": {`{"choices":[]}`}})
	if _, err := client(ProtocolOpenAI, empty.URL, "", Model{ID: "m"}).Chat(context.Background(), user); err == nil {
		t.Error("a reply with no choice was accepted")
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, err := client(ProtocolOpenAI, gone.URL, "", Model{ID: "m"}).Chat(context.Background(), user); err == nil ||
		!strings.Contains(err.Error(), "HTTP") {
		t.Errorf("unreachable: %v", err)
	}
	prose, _ := fakeEndpoint(t, map[string][]string{"/chat/completions": {`{"choices":[{"message":{"content":"no json here"}}]}`}})
	if _, err := client(ProtocolOpenAI, prose.URL, "", Model{ID: "m"}).ChatJSON(context.Background(), user, map[string]any{"type": "object"}); err == nil {
		t.Error("prose was accepted as structured output")
	}
}
