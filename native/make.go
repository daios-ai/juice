package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/daios-ai/juice/kernel"
)

// MakeDeps holds the external dependencies injected into the @sys/make handler at registration.
type MakeDeps struct {
	Scripts  kernel.ScriptExecutor
	Compiler kernel.SourceCompiler
	Chatter  kernel.Chatter
	Embedder kernel.Embedder
}

// RegisterMakeHandler registers the @sys/make native action handler on k.
// sdk is the TinyGo SDK source (script.TinyGoSDK) prepended to every generated action.
// maxSteps is the maximum number of decide-loop iterations; 0 uses the default of 10.
func RegisterMakeHandler(k *kernel.Kernel, deps MakeDeps, sdk string, maxSteps int) {
	if maxSteps <= 0 {
		maxSteps = 10
	}
	k.RegisterNativeHandler("make", func(ctx context.Context, args map[string]any, targetID, callerID, ownerUserID, processID, parentTraceID string) (map[string]any, error) {
		return executeMake(ctx, args, targetID, callerID, ownerUserID, processID, parentTraceID, k, deps, sdk, maxSteps)
	})
}

// actionContract is produced by contract derivation.
type actionContract struct {
	Name         string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
}

// MakeTest records the outcome of one test case run during synthesis.
type MakeTest struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "passed" | "failed"
	Reason string `json:"reason,omitempty"`
}

// MakeResult is the output schema of the @sys/make native action.
type MakeResult struct {
	Status      string     `json:"status"`                // "success" | "failure"
	ActionID    string     `json:"action_id,omitempty"`   // registered action ID on success
	ActionName  string     `json:"action_name,omitempty"` // registered action name on success
	Diagnostics []string   `json:"diagnostics"`
	Tests       []MakeTest `json:"tests,omitempty"`
}

// makeInput holds the parsed input to @sys/make.
type makeInput struct {
	Description string
}

// makeTestHost implements kernel.HostFunctions for testing generated WASM during synthesis.
// All sub-calls return {} — output values cannot be predicted when the action uses
// catalog actions whose real responses are unknown at synthesis time.
type makeTestHost struct{}

func (h *makeTestHost) Call(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return []byte("{}"), nil
}
func (h *makeTestHost) StepCreate(_ context.Context, _ []byte, _, _ string) (string, error) {
	return "", nil
}
func (h *makeTestHost) StepComplete(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return []byte("{}"), nil
}
func (h *makeTestHost) Log(_ context.Context, _, _ string) error { return nil }

// executeMake implements the @sys/make decide-driven synthesis loop.
// targetID is make's action owner (@sys); callerID is the call caller who will own the synthesized action;
// ownerUserID is the process owner (payer).
func executeMake(ctx context.Context, args map[string]any, targetID, callerID, ownerUserID, processID, parentTraceID string, k *kernel.Kernel, deps MakeDeps, sdk string, maxSteps int) (map[string]any, error) {
	if deps.Compiler == nil {
		return nil, kernel.ErrInvalidState.Wrap("source compiler not configured")
	}

	in, err := parseMakeInput(args)
	if err != nil {
		return nil, err
	}

	// Phase 1: derive the action contract once via @sys/llm/chat.
	contract, diag := deriveContract(ctx, in.Description, targetID, processID, parentTraceID, k)
	if diag != "" {
		return marshalMakeResult(&MakeResult{
			Status:      "failure",
			Diagnostics: []string{"contract derivation failed: " + diag},
			Tests:       []MakeTest{},
		})
	}

	// Seed the decide loop message history with the goal and derived contract.
	contractJSON, _ := json.Marshal(map[string]any{
		"name":          contract.Name,
		"input_schema":  contract.InputSchema,
		"output_schema": contract.OutputSchema,
	})
	messages := []any{
		map[string]any{"role": "system", "content": buildSystemPrompt(sdk, contract)},
		map[string]any{"role": "user", "content": "Build this action: " + in.Description},
		map[string]any{"role": "tool", "tool": map[string]any{
			"action": "@sys/llm/chat",
			"result": map[string]any{"contract": string(contractJSON)},
		}},
	}

	loopActions := []string{"@sys/lookup", "@sys/llm/chat"}
	diagnostics := []string{}
	tests := []MakeTest{}

	for step := 0; step < maxSteps; step++ {
		// Ask the LLM what to do next given the accumulated history.
		selected, selectedArgs, _, decideErr := callDecide(ctx, messages, loopActions, targetID, processID, parentTraceID, k)
		if decideErr != nil {
			// If the model chose @sys/llm/chat but proposed args that fail schema validation,
			// recover: treat this as a decision to generate code. The args are always overridden
			// below via buildCodeGenArgs, so the model's arg proposal doesn't matter.
			if errors.Is(decideErr, kernel.ErrExecutionFailed) &&
				strings.Contains(decideErr.Error(), "@sys/llm/chat") &&
				strings.Contains(decideErr.Error(), "args failed validation") {
				selected = "@sys/llm/chat"
				selectedArgs = map[string]any{}
			} else {
				diagnostics = append(diagnostics, fmt.Sprintf("step %d: decide: %v", step+1, decideErr))
				continue
			}
		}

		// Record the assistant's proposal in the conversation.
		messages = append(messages, map[string]any{
			"role": "assistant",
			"tool": map[string]any{"action": selected, "args": selectedArgs},
		})

		// For @sys/llm/chat: the decide model only chose the action; construct the actual
		// code-generation args from the full accumulated history so the LLM has SDK context.
		if selected == "@sys/llm/chat" {
			selectedArgs = buildCodeGenArgs(messages, diagnostics)
		}

		// Execute the selected action.
		result, execErr := callAction(ctx, selected, selectedArgs, targetID, processID, parentTraceID, k)
		if execErr != nil {
			messages = append(messages, map[string]any{
				"role": "tool",
				"tool": map[string]any{"action": selected, "args": selectedArgs, "result": map[string]any{"error": execErr.Error()}},
			})
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: %s: %v", step+1, selected, execErr))
			continue
		}
		messages = append(messages, map[string]any{
			"role": "tool",
			"tool": map[string]any{"action": selected, "args": selectedArgs, "result": result},
		})

		if selected != "@sys/llm/chat" {
			// Informational action (e.g. @sys/lookup) — result is now in history.
			continue
		}

		// @sys/llm/chat was selected: attempt to extract, compile, and test code.
		msg, _ := result["message"].(map[string]any)
		content, _ := msg["content"].(string)

		runFunc := extractGoBlock(content)
		if runFunc == "" {
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: no Go code block in response", step+1))
			continue
		}
		source := prepareSource(sdk, runFunc)

		wasm, _, compileErr := deps.Compiler.CompileSource(ctx, []byte(source))
		if compileErr != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: compile: %v", step+1, compileErr))
			continue
		}

		// Terminal failure: disallowed WASM imports are a structural violation, not retried.
		if checkDiag := checkWASMImports(wasm, deps.Scripts); checkDiag != "" {
			return marshalMakeResult(&MakeResult{
				Status:      "failure",
				Diagnostics: append(diagnostics, checkDiag),
				Tests:       tests,
			})
		}

		price := computePrice(ctx, source, ownerUserID, k)
		tests = generateAndRunExamples(ctx, contract, wasm, targetID, processID, parentTraceID, k, deps.Scripts)

		allPassed := true
		for _, tc := range tests {
			if tc.Status == "failed" {
				allPassed = false
				diagnostics = append(diagnostics, fmt.Sprintf("step %d: test %q: %s", step+1, tc.Name, tc.Reason))
			}
		}

		if allPassed {
			inSchema := sanitizeSchemaForRegistration(contract.InputSchema)
			outSchema := sanitizeSchemaForRegistration(contract.OutputSchema)
			var action *kernel.Action
			var createErr error
			for attempt := 0; attempt <= 9; attempt++ {
				candidateName := contract.Name
				if attempt > 0 {
					candidateName = fmt.Sprintf("%s-%d", contract.Name, attempt+1)
				}
				action, createErr = k.CreateAction(ctx, callerID, kernel.CreateActionRequest{
					OwnerUserID:  callerID,
					Name:         candidateName,
					Kind:         kernel.KindWasm,
					Price:        price,
					Description:  contract.Description,
					InputSchema:  inSchema,
					OutputSchema: outSchema,
					Source:       source,
					WasmArtifact: base64.StdEncoding.EncodeToString(wasm),
				})
				if createErr == nil || !isNameCollision(createErr) {
					break
				}
			}
			if createErr != nil {
				return marshalMakeResult(&MakeResult{
					Status:      "failure",
					Diagnostics: append(diagnostics, "registration: "+createErr.Error()),
					Tests:       tests,
				})
			}
			if activateErr := k.SetActive(ctx, callerID, action.ID, true); activateErr != nil {
				return marshalMakeResult(&MakeResult{
					Status:      "failure",
					Diagnostics: append(diagnostics, "activation: "+activateErr.Error()),
					Tests:       tests,
				})
			}
			return marshalMakeResult(&MakeResult{
				Status:      "success",
				ActionID:    action.ID,
				ActionName:  action.Name,
				Diagnostics: diagnostics,
				Tests:       tests,
			})
		}
	}

	return marshalMakeResult(&MakeResult{Status: "failure", Diagnostics: diagnostics, Tests: tests})
}

// parseMakeInput validates @sys/make arguments.
func parseMakeInput(args map[string]any) (*makeInput, error) {
	desc, _ := args["description"].(string)
	if strings.TrimSpace(desc) == "" {
		return nil, kernel.ErrInvalidInput.Wrap("description is required")
	}
	return &makeInput{Description: desc}, nil
}

// buildSystemPrompt constructs the system message that seeds the decide loop.
func buildSystemPrompt(sdk string, contract *actionContract) string {
	inJSON, _ := json.MarshalIndent(contract.InputSchema, "", "  ")
	outJSON, _ := json.MarshalIndent(contract.OutputSchema, "", "  ")
	var sb strings.Builder
	sb.WriteString("You are synthesizing a Juice WASM action. ")
	sb.WriteString("Prefer composing existing platform actions via JuiceCall over writing new code from scratch.\n\n")
	sb.WriteString("Contract:\n")
	sb.WriteString("  name: " + contract.Name + "\n")
	sb.WriteString("  input_schema: " + string(inJSON) + "\n")
	sb.WriteString("  output_schema: " + string(outJSON) + "\n\n")
	sb.WriteString("Toolkit:\n")
	sb.WriteString("  @sys/lookup   — find existing actions by description\n")
	sb.WriteString("  @sys/llm/chat — generate TinyGo code once you have sufficient context\n\n")
	if sdk != "" {
		sb.WriteString("TinyGo SDK (already included — DO NOT redeclare):\n```go\n")
		sb.WriteString(sdk)
		sb.WriteString("\n```\n\n")
	}
	sb.WriteString("Code rules (STRICT — violations cause compile errors):\n")
	sb.WriteString("- Output ONLY the body: //export run function plus any private helpers\n")
	sb.WriteString("- NO package declaration, NO import statements — SDK already imports encoding/json and unsafe\n")
	sb.WriteString("- NO redeclaration of any SDK symbol (mustMarshal, JuiceCall, _ptrLen, etc.)\n")
	sb.WriteString("- ONLY encoding/json and unsafe are in scope — NO fmt, NO strings, NO other packages\n")
	sb.WriteString("- NO fail(), success(), ptrToBytes() — those helpers do NOT exist in the SDK\n")
	sb.WriteString("- Every variable declared with := MUST be used immediately; declare only what you need\n")
	sb.WriteString("- Use float64 for numbers, string for text — rawNumber, json.Number, int are NOT available\n")
	sb.WriteString("- argsJSON declared for JuiceCall MUST appear as the second argument of JuiceCall on the next line\n\n")
	sb.WriteString("Required patterns:\n")
	sb.WriteString("  Read input:    inBytes := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(inputPtr))), int(inputLen))\n")
	sb.WriteString("                 json.Unmarshal(inBytes, &input)\n")
	sb.WriteString("  Return result: out := mustMarshal(result); p, l := _ptrLen(out); return uint64(p)<<32 | uint64(l)\n")
	sb.WriteString("  Sub-call (COMPLETE pattern — both lines required):\n")
	sb.WriteString("    argsJSON := mustMarshal(map[string]any{\"key\": value})\n")
	sb.WriteString("    replyBytes, _ := JuiceCall(\"@owner/name\", argsJSON)\n")
	sb.WriteString("    var reply struct{ Result float64 `json:\"result\"` }\n")
	sb.WriteString("    json.Unmarshal(replyBytes, &reply)\n\n")
	sb.WriteString("@sys/llm/chat call and response (Juice format, NOT OpenAI):\n")
	sb.WriteString("  args = mustMarshal(map[string]any{\"messages\": []any{map[string]any{\"role\": \"user\", \"content\": prompt}}})\n")
	sb.WriteString("  replyBytes, _ = JuiceCall(\"@sys/llm/chat\", args)\n")
	sb.WriteString("  var chatReply struct{ Message struct{ Content string `json:\"content\"` } `json:\"message\"` }\n")
	sb.WriteString("  json.Unmarshal(replyBytes, &chatReply)  // chatReply.Message.Content is the text\n")
	sb.WriteString("  // NOTE: response uses {\"message\":{\"content\":\"...\"}}, NOT {\"choices\":[...]}\n\n")
	sb.WriteString("Minimal working example (echo action):\n")
	sb.WriteString("```go\n")
	sb.WriteString("//export run\n")
	sb.WriteString("func run(inputPtr, inputLen uint32) uint64 {\n")
	sb.WriteString("\tvar input struct{ Text string `json:\"text\"` }\n")
	sb.WriteString("\tinBytes := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(inputPtr))), int(inputLen))\n")
	sb.WriteString("\tjson.Unmarshal(inBytes, &input)\n")
	sb.WriteString("\tout := mustMarshal(map[string]any{\"result\": input.Text})\n")
	sb.WriteString("\tp, l := _ptrLen(out)\n")
	sb.WriteString("\treturn uint64(p)<<32 | uint64(l)\n")
	sb.WriteString("}\n")
	sb.WriteString("```\n\n")
	sb.WriteString("Wrap generated code in ```go\\n...\\n```\n")
	return sb.String()
}

// deriveContract calls @sys/llm/chat to produce name, description, input_schema, and output_schema.
// The description field is a clean one-line summary derived from the user's intent, not the raw input.
func deriveContract(ctx context.Context, userIntent, targetID, processID, parentTraceID string, k *kernel.Kernel) (*actionContract, string) {
	prompt := fmt.Sprintf(`You are designing a callable API action for the Juice platform.
Given the user's request, produce a JSON object with exactly these fields:
{
  "name": "short-kebab-slug (3 words max, captures the core function)",
  "description": "Clean one-line description of what this action does (ignore preamble, tips, or conversational phrasing)",
  "input_schema": { "type": "object", "properties": { ... }, "required": [...] },
  "output_schema": { "type": "object", "properties": { ... }, "required": [...] }
}
Respond with ONLY the JSON object, nothing else.
User request: %s`, userIntent)

	result, err := callAction(ctx, "@sys/llm/chat", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	}, targetID, processID, parentTraceID, k)
	if err != nil {
		return nil, err.Error()
	}
	msg, _ := result["message"].(map[string]any)
	content, _ := msg["content"].(string)

	jsonStr := extractJSON(content)
	if jsonStr == "" {
		return nil, "LLM did not return a JSON object"
	}
	var raw struct {
		Name         string         `json:"name"`
		Description  string         `json:"description"`
		InputSchema  map[string]any `json:"input_schema"`
		OutputSchema map[string]any `json:"output_schema"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Sprintf("invalid JSON: %v", err)
	}
	if raw.Name == "" || raw.InputSchema == nil || raw.OutputSchema == nil {
		return nil, "contract missing required fields (name, input_schema, output_schema)"
	}
	if raw.Description == "" {
		raw.Description = raw.Name
	}
	return &actionContract{
		Name:         raw.Name,
		Description:  raw.Description,
		InputSchema:  raw.InputSchema,
		OutputSchema: raw.OutputSchema,
	}, ""
}

// callAction calls any Juice action by @owner/name reference through kernel.Call,
// routing the sub-call through the same process and trace as the parent make call.
func callAction(ctx context.Context, actionRef string, args map[string]any, targetID, processID, parentTraceID string, k *kernel.Kernel) (map[string]any, error) {
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID:      targetID,
		ParentTraceID: parentTraceID,
		ActionRef:     actionRef,
		Args:          args,
	})
	if err != nil {
		return nil, err
	}
	return reply.Result, nil
}

// buildCodeGenArgs constructs the @sys/llm/chat args for code generation from the accumulated
// message history. The decide model only selects the action name; this function provides the
// full context (SDK system prompt + task description + contract + any prior errors) so the
// code-generation LLM has everything it needs without relying on the routing model to re-state it.
func buildCodeGenArgs(messages []any, priorErrors []string) map[string]any {
	var sysContent, taskDesc, contractStr string
	var lookupSnippets []string

	for _, m := range messages {
		msg, _ := m.(map[string]any)
		role, _ := msg["role"].(string)
		switch role {
		case "system":
			if c, _ := msg["content"].(string); c != "" {
				sysContent = c
			}
		case "user":
			if c, _ := msg["content"].(string); c != "" && taskDesc == "" {
				taskDesc = c
			}
		case "tool":
			tool, _ := msg["tool"].(map[string]any)
			action, _ := tool["action"].(string)
			result, _ := tool["result"].(map[string]any)
			switch action {
			case "@sys/llm/chat":
				if c, _ := result["contract"].(string); c != "" && contractStr == "" {
					contractStr = c
				}
			case "@sys/lookup":
				if b, err := json.Marshal(result); err == nil {
					lookupSnippets = append(lookupSnippets, string(b))
				}
			}
		}
	}

	var userMsg strings.Builder
	if taskDesc != "" {
		userMsg.WriteString(taskDesc + "\n\n")
	}
	if contractStr != "" {
		userMsg.WriteString("Contract:\n" + contractStr + "\n\n")
	}
	for _, s := range lookupSnippets {
		userMsg.WriteString("Found existing actions:\n" + s + "\n\n")
	}
	if len(priorErrors) > 0 {
		userMsg.WriteString("Fix these errors from the previous attempt:\n")
		for _, e := range priorErrors {
			userMsg.WriteString("- " + e + "\n")
		}
		userMsg.WriteString("\n")
	}
	userMsg.WriteString("Write the TinyGo code now.")

	var chatMsgs []any
	if sysContent != "" {
		chatMsgs = append(chatMsgs, map[string]any{"role": "system", "content": sysContent})
	}
	chatMsgs = append(chatMsgs, map[string]any{"role": "user", "content": strings.TrimSpace(userMsg.String())})
	return map[string]any{"messages": chatMsgs}
}

// callDecide calls @sys/llm/decide and returns the selected action reference, its args,
// and an optional message. Returns an error if decide fails or produces no selection.
func callDecide(ctx context.Context, messages []any, actions []string, targetID, processID, parentTraceID string, k *kernel.Kernel) (string, map[string]any, string, error) {
	actionsAny := make([]any, len(actions))
	for i, a := range actions {
		actionsAny[i] = a
	}
	result, err := callAction(ctx, "@sys/llm/decide", map[string]any{
		"messages": messages,
		"actions":  actionsAny,
	}, targetID, processID, parentTraceID, k)
	if err != nil {
		return "", nil, "", err
	}
	action, _ := result["action"].(string)
	args, _ := result["args"].(map[string]any)
	message := ""
	if msg, ok := result["message"].(map[string]any); ok {
		message, _ = msg["content"].(string)
	}
	return action, args, message, nil
}

var juiceCallRe = regexp.MustCompile(`JuiceCall\("(@[^"]+)"`)

// computePrice scans the TinyGo source for JuiceCall("@owner/name") patterns and returns
// the sum of the referenced actions' prices as the minimum subtree bound.
func computePrice(ctx context.Context, source, processOwnerID string, k *kernel.Kernel) int64 {
	matches := juiceCallRe.FindAllStringSubmatch(source, -1)
	seen := map[string]bool{}
	var total int64
	for _, m := range matches {
		ref := m[1]
		if seen[ref] {
			continue
		}
		seen[ref] = true
		ownerHandle, actionName, err := kernel.ParseActionRef(ref)
		if err != nil {
			continue
		}
		a, err := k.ReadCallableAction(ctx, ownerHandle, actionName, processOwnerID)
		if err != nil {
			continue
		}
		total += a.Price
	}
	return total
}

// generateAndRunExamples asks the LLM for test inputs, executes them against the WASM,
// and validates the output structure.
func generateAndRunExamples(ctx context.Context, contract *actionContract, wasm []byte, targetID, processID, parentTraceID string, k *kernel.Kernel, scripts kernel.ScriptExecutor) []MakeTest {
	if scripts == nil {
		return []MakeTest{{Name: "compile", Status: "failed", Reason: "script executor not configured"}}
	}
	artifact, _, err := scripts.Compile(ctx, wasm)
	if err != nil {
		return []MakeTest{{Name: "compile", Status: "failed", Reason: fmt.Sprintf("WASM compile: %v", err)}}
	}
	results := []MakeTest{{Name: "compile", Status: "passed"}}

	inJSON, _ := json.MarshalIndent(contract.InputSchema, "", "  ")
	prompt := fmt.Sprintf(`Given this JSON input schema, generate 3 realistic example inputs.
Respond with ONLY a JSON array of objects, each matching the schema.

Schema:
%s`, string(inJSON))

	result, err := callAction(ctx, "@sys/llm/chat", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	}, targetID, processID, parentTraceID, k)
	if err != nil {
		return results
	}
	msg, _ := result["message"].(map[string]any)
	content, _ := msg["content"].(string)

	jsonStr := extractJSON(content)
	if jsonStr == "" {
		return results
	}
	var examples []map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &examples); err != nil {
		var single map[string]any
		if err2 := json.Unmarshal([]byte(jsonStr), &single); err2 == nil {
			examples = []map[string]any{single}
		} else {
			return results
		}
	}
	if len(examples) < 3 {
		results = append(results, MakeTest{Name: "examples", Status: "failed", Reason: fmt.Sprintf("need 3 test inputs, got %d", len(examples))})
		return results
	}

	outKeys := topLevelKeys(contract.OutputSchema)
	for i, exArgs := range examples {
		name := fmt.Sprintf("example-%d", i+1)
		inputJSON, _ := json.Marshal(exArgs)
		outputJSON, execErr := scripts.Execute(ctx, artifact, inputJSON, &makeTestHost{})
		if execErr != nil {
			results = append(results, MakeTest{Name: name, Status: "failed", Reason: "execution: " + execErr.Error()})
			continue
		}
		var output map[string]any
		if err := json.Unmarshal(outputJSON, &output); err != nil {
			results = append(results, MakeTest{Name: name, Status: "failed", Reason: "output not valid JSON"})
			continue
		}
		missing := []string{}
		for _, key := range outKeys {
			if _, ok := output[key]; !ok {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			results = append(results, MakeTest{Name: name, Status: "failed",
				Reason: fmt.Sprintf("output missing required fields: %s", strings.Join(missing, ", "))})
		} else {
			results = append(results, MakeTest{Name: name, Status: "passed"})
		}
	}
	return results
}

// topLevelKeys returns the required keys from a JSON Schema's properties.
func topLevelKeys(schema map[string]any) []string {
	if schema == nil {
		return nil
	}
	required, _ := schema["required"].([]any)
	var keys []string
	for _, r := range required {
		if s, ok := r.(string); ok {
			keys = append(keys, s)
		}
	}
	if len(keys) == 0 {
		props, _ := schema["properties"].(map[string]any)
		for k := range props {
			keys = append(keys, k)
		}
	}
	return keys
}

// checkWASMImports validates that the artifact only imports allowed host functions.
func checkWASMImports(wasm []byte, scripts kernel.ScriptExecutor) string {
	insp, ok := scripts.(kernel.WASMInspector)
	if !ok {
		return ""
	}
	imports, exports, err := insp.InspectWASM(wasm)
	if err != nil {
		return fmt.Sprintf("WASM inspection failed: %v", err)
	}
	allowedImports := map[string]bool{"call": true, "step_create": true, "step_complete": true, "log": true}
	for _, imp := range imports {
		if imp.Module == "wasi_snapshot_preview1" {
			continue
		}
		if imp.Module != "juice" || !allowedImports[imp.Name] {
			return fmt.Sprintf("WASM imports disallowed function %q from module %q", imp.Name, imp.Module)
		}
	}
	hasAlloc, hasRun := false, false
	for _, e := range exports {
		if e == "alloc" {
			hasAlloc = true
		}
		if e == "run" {
			hasRun = true
		}
	}
	if !hasAlloc {
		return "WASM does not export required function alloc"
	}
	if !hasRun {
		return "WASM does not export required function run"
	}
	return ""
}

// prepareSource combines the SDK with the LLM-generated run function.
func prepareSource(sdk, generated string) string {
	var kept []string
	for _, line := range strings.Split(generated, "\n") {
		if strings.TrimSpace(line) == "package main" {
			continue
		}
		kept = append(kept, line)
	}
	body := strings.TrimSpace(strings.Join(kept, "\n"))
	if sdk == "" {
		return "package main\n\n" + body
	}
	return sdk + "\n" + body
}

// sanitizeSchemaForRegistration strips unsupported JSON Schema keywords and ensures
// every property has a description so the result passes ValidateSchema and
// validateSchemaDescriptions at activation time.
func sanitizeSchemaForRegistration(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{}
	}
	allowed := []string{"type", "nullable", "enum", "description", "properties", "required", "items"}
	result := make(map[string]any)
	for _, key := range allowed {
		if v, ok := schema[key]; ok {
			result[key] = v
		}
	}
	if props, ok := result["properties"].(map[string]any); ok {
		sanitized := make(map[string]any, len(props))
		for name, raw := range props {
			child, _ := raw.(map[string]any)
			s := sanitizeSchemaForRegistration(child)
			if _, hasDesc := s["description"]; !hasDesc {
				s["description"] = name
			}
			sanitized[name] = s
		}
		result["properties"] = sanitized
	}
	if items, ok := result["items"].(map[string]any); ok {
		result["items"] = sanitizeSchemaForRegistration(items)
	}
	return result
}

// marshalMakeResult converts a MakeResult to the map[string]any expected by Call().
func marshalMakeResult(r *MakeResult) (map[string]any, error) {
	if r.Diagnostics == nil {
		r.Diagnostics = []string{}
	}
	if r.Tests == nil {
		r.Tests = []MakeTest{}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, kernel.ErrInternal.Wrapf("marshal make result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, kernel.ErrInternal.Wrapf("unmarshal make result: %v", err)
	}
	return m, nil
}

// isNameCollision reports whether err is a SQLite UNIQUE constraint violation on the
// action name, which happens when the LLM proposes a name already registered by this owner.
func isNameCollision(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

// extractGoBlock extracts the first ```go ... ``` block from an LLM response.
func extractGoBlock(s string) string {
	const start = "```go"
	const end = "```"
	idx := strings.Index(s, start)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(start):]
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	}
	end2 := strings.Index(rest, end)
	if end2 < 0 {
		return rest
	}
	return rest[:end2]
}

// extractJSON extracts the first {...} or [...] JSON value from a string.
func extractJSON(s string) string {
	obj := strings.Index(s, "{")
	arr := strings.Index(s, "[")
	// Extract whichever valid JSON structure (array or object) appears first.
	tryExtract := func(start int, open, close byte) string {
		depth := 0
		for j := start; j < len(s); j++ {
			switch s[j] {
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return s[start : j+1]
				}
			}
		}
		return ""
	}
	if arr >= 0 && (obj < 0 || arr < obj) {
		if r := tryExtract(arr, '[', ']'); r != "" {
			return r
		}
	}
	if obj >= 0 {
		if r := tryExtract(obj, '{', '}'); r != "" {
			return r
		}
	}
	if arr >= 0 {
		return tryExtract(arr, '[', ']')
	}
	return ""
}
