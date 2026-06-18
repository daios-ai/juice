package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
// maxSteps is the maximum number of repair-loop iterations (default 5 per §9).
func RegisterMakeHandler(k *kernel.Kernel, deps MakeDeps, sdk string, maxSteps int) {
	if maxSteps <= 0 {
		maxSteps = 5
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
	Constraints  []string
	Plan         []string
}

// composableAction is a platform action surfaced during research for use in codegen.
type composableAction struct {
	Ref            string
	Description    string
	Price          int64
	RequiredInputs []string // required input field names from the action's input_schema
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

// makeTestHost implements kernel.HostFunctions for smoke-testing generated WASM.
// Sub-calls return {} — real responses are unknown at synthesis time.
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

// executeMake runs the @sys/make four-phase pipeline, all phases inside one repair loop:
//
//	Phase 1 — Plan & research: derive contract, constraints, plan; for each plan
//	          capability run lookup→decide to surface composable actions.
//	Phase 2 — Code: generate TinyGo from contract + constraints + surface + prior diagnostics.
//	Phase 3 — Compile: TinyGo → WASM; validate imports/exports.
//	Phase 4 — Evaluate: smoke-test ≥3 inputs, all must pass.
//
// All four phases repeat up to maxSteps times on failure; accumulated diagnostics feed back
// into planning so a failed attempt can revise the contract, plan, and composable surface.
// targetID is make's action owner (@sys); callerID is the call caller who will own the
// synthesized action; ownerUserID is the payer.
func executeMake(ctx context.Context, args map[string]any, targetID, callerID, ownerUserID, processID, parentTraceID string, k *kernel.Kernel, deps MakeDeps, sdk string, maxSteps int) (map[string]any, error) {
	if deps.Compiler == nil {
		return nil, kernel.ErrInvalidState.Wrap("source compiler not configured")
	}
	if deps.Chatter == nil {
		return nil, kernel.ErrInvalidState.Wrap("LLM not configured")
	}

	in, err := parseMakeInput(args)
	if err != nil {
		return nil, err
	}

	// Repair loop: every attempt re-plans, re-researches, codes, compiles, and
	// evaluates. Accumulated diagnostics feed back into planning so a failed
	// attempt can revise the contract, plan, and composable surface — not just
	// re-generate code against a frozen plan.
	diagnostics := []string{}
	tests := []MakeTest{}

	for step := 0; step < maxSteps; step++ {
		// Phase 1: Plan & research.
		contract, surface, failDiag := planAndResearch(ctx, in.Description, diagnostics, targetID, processID, parentTraceID, k)
		if failDiag != "" {
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: plan & research: %s", step+1, failDiag))
			continue
		}

		// Phase 2: Generate code.
		source, codeDiag := generateCode(ctx, contract, surface, diagnostics, targetID, processID, parentTraceID, k, sdk)
		if codeDiag != "" {
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: %s", step+1, codeDiag))
			continue
		}

		// Phase 3: Compile.
		wasm, _, compileErr := deps.Compiler.CompileSource(ctx, []byte(source))
		if compileErr != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: compile: %v", step+1, compileErr))
			continue
		}

		// Validate WASM imports/exports — terminal failure, not retried.
		if checkDiag := checkWASMImports(wasm, deps.Scripts); checkDiag != "" {
			return marshalMakeResult(&MakeResult{
				Status:      "failure",
				Diagnostics: append(diagnostics, checkDiag),
				Tests:       tests,
			})
		}

		// Phase 4: Evaluate.
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
			action, createErr := k.CreateAction(ctx, callerID, kernel.CreateActionRequest{
				OwnerUserID:  callerID,
				Name:         contract.Name,
				Kind:         kernel.KindWasm,
				Price:        price,
				Description:  contract.Description,
				InputSchema:  inSchema,
				OutputSchema: outSchema,
				Source:       source,
				WasmArtifact: base64.StdEncoding.EncodeToString(wasm),
			})
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

// planAndResearch runs Phase 1: derives the contract (with constraints and plan),
// then for each plan capability runs lookup→decide to surface composable actions.
// diagnostics from prior failed attempts are fed into contract derivation so the plan
// can be revised. Returns the contract, a formatted surface string, and an error diagnostic.
func planAndResearch(ctx context.Context, description string, diagnostics []string, targetID, processID, parentTraceID string, k *kernel.Kernel) (*actionContract, string, string) {
	contract, diag := deriveContract(ctx, description, diagnostics, targetID, processID, parentTraceID, k)
	if diag != "" {
		return nil, "", diag
	}

	seen := map[string]bool{}
	var surface []composableAction

	for _, capability := range contract.Plan {
		candidates, err := researchCapability(ctx, capability, contract.Constraints, targetID, processID, parentTraceID, k)
		if err != nil || len(candidates) == 0 {
			continue // non-fatal: this capability will be implemented from scratch
		}
		for _, c := range candidates {
			if !seen[c.Ref] {
				seen[c.Ref] = true
				surface = append(surface, c)
			}
		}
	}

	return contract, buildSurface(surface), ""
}

// researchCapability runs lookup then decide for one plan capability.
// Returns the selected action or nil; errors are treated as from-scratch by the caller.
func researchCapability(ctx context.Context, capability string, constraints []string, targetID, processID, parentTraceID string, k *kernel.Kernel) ([]composableAction, error) {
	result, err := callAction(ctx, "@sys/lookup", map[string]any{"query": capability}, targetID, processID, parentTraceID, k)
	if err != nil {
		return nil, err
	}

	results, _ := result["results"].([]any)
	if len(results) == 0 {
		return nil, nil
	}

	var candidates []composableAction
	for _, r := range results {
		rm, _ := r.(map[string]any)
		ownerHandle, _ := rm["owner_handle"].(string)
		name, _ := rm["name"].(string)
		desc, _ := rm["description"].(string)
		price, _ := rm["price"].(float64)
		if ownerHandle == "" || name == "" {
			continue
		}
		candidates = append(candidates, composableAction{
			Ref:         ownerHandle + "/" + name,
			Description: desc,
			Price:       int64(price),
		})
	}

	candidates = filterCandidates(candidates, constraints)
	if len(candidates) == 0 {
		return nil, nil
	}

	actionRefs := make([]string, len(candidates))
	for i, c := range candidates {
		actionRefs[i] = c.Ref
	}

	messages := []any{
		map[string]any{"role": "user", "content": "Select the best existing action for this capability: " + capability},
	}
	selected, _, _, err := callDecide(ctx, messages, actionRefs, targetID, processID, parentTraceID, k)
	if err != nil {
		return nil, err
	}

	for _, c := range candidates {
		if c.Ref == selected {
			// Fetch required input fields so codegen knows the correct argument names.
			ownerHandle, actionName, parseErr := kernel.ParseActionRef(c.Ref)
			if parseErr == nil {
				if a, readErr := k.ReadCallableAction(ctx, ownerHandle, actionName, targetID); readErr == nil {
					c.RequiredInputs = topLevelKeys(a.InputSchema)
				}
			}
			return []composableAction{c}, nil
		}
	}
	return nil, nil
}

// filterCandidates removes actions that violate the given constraints.
func filterCandidates(candidates []composableAction, constraints []string) []composableAction {
	if !hasLLMConstraint(constraints) {
		return candidates
	}
	var out []composableAction
	for _, c := range candidates {
		if !strings.Contains(strings.ToLower(c.Ref), "llm") {
			out = append(out, c)
		}
	}
	return out
}

// hasLLMConstraint returns true if any constraint prohibits LLM use.
func hasLLMConstraint(constraints []string) bool {
	for _, c := range constraints {
		l := strings.ToLower(c)
		if strings.Contains(l, "llm") || strings.Contains(l, "no ai") || strings.Contains(l, "without ai") {
			return true
		}
	}
	return false
}

// buildSurface formats the composable surface as a string for the codegen prompt.
func buildSurface(surface []composableAction) string {
	if len(surface) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, a := range surface {
		line := fmt.Sprintf("  %s — %s (price %d)", a.Ref, a.Description, a.Price)
		if len(a.RequiredInputs) > 0 {
			line += " | required inputs: " + strings.Join(a.RequiredInputs, ", ")
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

// deriveContract calls @sys/llm/chat to produce name, description, schemas,
// constraints, and plan from the user's description. diagnostics from prior failed
// attempts are appended so the LLM can revise the contract and plan.
//
// This uses plain @sys/llm/chat, NOT schema-constrained @sys/llm/json: the contract's
// input_schema/output_schema are themselves free-form JSON Schema, and constraining their
// generation with Ollama's `format` makes the model stall/truncate on the nested structure
// (the §8.1 large-context JSON-mode failure). We parse defensively with extractJSON instead.
// Schema-constrained decoding is reserved for example generation, where the target shape is
// a concrete, already-derived input_schema (see generateAndRunExamples).
func deriveContract(ctx context.Context, userIntent string, diagnostics []string, targetID, processID, parentTraceID string, k *kernel.Kernel) (*actionContract, string) {
	prompt := fmt.Sprintf(`You are designing a callable API action for the Juice platform.
Given the user's request, produce a JSON object with exactly these fields:
{
  "name": "short-kebab-slug (3 words max, captures the core function)",
  "description": "Clean one-line description of what this action does",
  "input_schema": { "type": "object", "properties": { ... }, "required": [...] },
  "output_schema": { "type": "object", "properties": { ... }, "required": [...] },
  "constraints": ["extract any restrictions from the user's phrasing, e.g. 'no LLM', 'only +-*/'"],
  "plan": ["distinct capability needed to implement this action", ...]
}
Every schema property must have a "type" of object, array, string, integer, number, or boolean.
Respond with ONLY the JSON object, nothing else.
User request: %s`, userIntent)

	if len(diagnostics) > 0 {
		prompt += "\n\nPrior attempts failed with these errors — revise the plan, schemas, or constraints to avoid them:\n"
		for _, d := range diagnostics {
			prompt += "- " + d + "\n"
		}
	}

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
		Constraints  []string       `json:"constraints"`
		Plan         []string       `json:"plan"`
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
		Constraints:  raw.Constraints,
		Plan:         raw.Plan,
	}, ""
}

// generateCode runs one code generation attempt via @sys/llm/chat.
// Returns the prepared source and empty string, or "" and a diagnostic on failure.
func generateCode(ctx context.Context, contract *actionContract, surface string, diagnostics []string, targetID, processID, parentTraceID string, k *kernel.Kernel, sdk string) (string, string) {
	messages := buildCodeGenMessages(sdk, contract, surface, diagnostics)
	result, err := callAction(ctx, "@sys/llm/chat", map[string]any{"messages": messages}, targetID, processID, parentTraceID, k)
	if err != nil {
		return "", "codegen LLM: " + err.Error()
	}
	msg, _ := result["message"].(map[string]any)
	content, _ := msg["content"].(string)
	body := extractGoBlock(content)
	if body == "" {
		return "", "no Go code block in LLM response"
	}
	return prepareSource(sdk, body), ""
}

// buildCodeGenMessages constructs the messages array for the code generation chat call.
func buildCodeGenMessages(sdk string, contract *actionContract, surface string, diagnostics []string) []any {
	inJSON, _ := json.MarshalIndent(contract.InputSchema, "", "  ")
	outJSON, _ := json.MarshalIndent(contract.OutputSchema, "", "  ")

	var sys strings.Builder
	sys.WriteString("You are synthesizing a Juice WASM action in TinyGo.\n\n")
	sys.WriteString("Contract:\n")
	sys.WriteString("  name: " + contract.Name + "\n")
	sys.WriteString("  input_schema: " + string(inJSON) + "\n")
	sys.WriteString("  output_schema: " + string(outJSON) + "\n\n")

	if len(contract.Constraints) > 0 {
		sys.WriteString("Constraints (MUST honor):\n")
		for _, c := range contract.Constraints {
			sys.WriteString("  - " + c + "\n")
		}
		sys.WriteString("\n")
	}

	if surface != "" {
		sys.WriteString("Available platform actions to compose via JuiceCall:\n")
		sys.WriteString(surface)
		sys.WriteString("\n")
	}

	if sdk != "" {
		sys.WriteString("TinyGo SDK (already included — DO NOT redeclare):\n```go\n")
		sys.WriteString(sdk)
		sys.WriteString("\n```\n\n")
	}

	sys.WriteString("Code rules (STRICT — violations cause compile errors):\n")
	sys.WriteString("- Output ONLY one function plus any private helpers:\n")
	sys.WriteString("    func Handle(in map[string]any) (map[string]any, error)\n")
	sys.WriteString("  The SDK owns the entry point (run, main, alloc) and calls Handle for you.\n")
	sys.WriteString("- NO package declaration, NO import statements — the SDK already imports what you need.\n")
	sys.WriteString("- NO func run, NO func main, NO //export, NO unsafe — the SDK provides those; redeclaring them fails.\n")
	sys.WriteString("- NO redeclaration of any SDK symbol (Handle's body may USE mustMarshal, JuiceCall, JuiceLog,\n")
	sys.WriteString("  JuiceStepCreate, JuiceStepComplete, but must NOT redefine them).\n")
	sys.WriteString("- Available packages (already imported): encoding/json, strings, strconv, math, sort. NO fmt.\n")
	sys.WriteString("- Every variable declared with := MUST be used immediately; declare only what you need.\n")
	sys.WriteString("- Read input from the `in` parameter; JSON numbers arrive as float64, text as string.\n")
	sys.WriteString("- Return (result, nil) on success; return (nil, errors.New(\"reason\")) to fail the call (errors is imported).\n")
	sys.WriteString("- JuiceCall's first argument MUST be a full \"@owner/name\" reference (use one from the\n")
	sys.WriteString("  actions list above, e.g. \"@alice/calculator\") — NEVER a bare name, and NEVER this\n")
	sys.WriteString("  action's own name (no self-calls; that recurses and fails).\n")
	sys.WriteString("- Wrap generated code in ```go\\n...\\n```\n\n")
	sys.WriteString("Required patterns:\n")
	sys.WriteString("  Read input:    x, _ := in[\"x\"].(float64)   // numbers are float64\n")
	sys.WriteString("                 name, _ := in[\"name\"].(string)\n")
	sys.WriteString("  Return result: return map[string]any{\"result\": x * 2}, nil\n")
	sys.WriteString("  Sub-call (COMPLETE pattern — both lines required):\n")
	sys.WriteString("    argsJSON := mustMarshal(map[string]any{\"key\": value})\n")
	sys.WriteString("    replyBytes, _ := JuiceCall(\"@owner/name\", argsJSON)\n")
	sys.WriteString("    var reply struct{ Result float64 `json:\"result\"` }\n")
	sys.WriteString("    json.Unmarshal(replyBytes, &reply)\n\n")
	sys.WriteString("Calling an LLM — choose the right action:\n")
	sys.WriteString("- Need STRUCTURED data (a number, a record, a list, an enum)? Use @sys/llm/json — NEVER\n")
	sys.WriteString("  hand-parse free-form text. A chat model replies 'The answer is 35', not '35'; parsing\n")
	sys.WriteString("  that yourself silently fails. @sys/llm/json constrains the model to a schema you supply.\n")
	sys.WriteString("- Need free-form natural-language TEXT (a translation, a summary)? Use @sys/llm/chat.\n\n")
	sys.WriteString("@sys/llm/json call (STRUCTURED output — the reply's \"value\" is GUARANTEED to match output_schema):\n")
	sys.WriteString("  jsonArgs := mustMarshal(map[string]any{\n")
	sys.WriteString("    \"messages\": []any{map[string]any{\"role\": \"user\", \"content\": prompt}},\n")
	sys.WriteString("    \"output_schema\": map[string]any{\n")
	sys.WriteString("      \"type\": \"object\",\n")
	sys.WriteString("      \"properties\": map[string]any{\"result\": map[string]any{\"type\": \"number\"}},\n")
	sys.WriteString("      \"required\": []any{\"result\"},\n")
	sys.WriteString("    },\n")
	sys.WriteString("  })\n")
	sys.WriteString("  jb, _ := JuiceCall(\"@sys/llm/json\", jsonArgs)\n")
	sys.WriteString("  var jr struct{ Value struct{ Result float64 `json:\"result\"` } `json:\"value\"` }\n")
	sys.WriteString("  json.Unmarshal(jb, &jr)  // jr.Value.Result is present and numeric, guaranteed\n")
	sys.WriteString("  // output_schema MUST be type \"object\" (wrap scalars/lists in a field, e.g.\n")
	sys.WriteString("  // {result: number} or {items: array}); the reply's \"value\" is always an object.\n\n")
	sys.WriteString("@sys/llm/chat call and response (free-form TEXT only; Juice format, NOT OpenAI):\n")
	sys.WriteString("  args := mustMarshal(map[string]any{\"messages\": []any{map[string]any{\"role\": \"user\", \"content\": prompt}}})\n")
	sys.WriteString("  replyBytes, _ := JuiceCall(\"@sys/llm/chat\", args)\n")
	sys.WriteString("  var chatReply struct{ Message struct{ Content string `json:\"content\"` } `json:\"message\"` }\n")
	sys.WriteString("  json.Unmarshal(replyBytes, &chatReply)  // chatReply.Message.Content is the text\n")
	sys.WriteString("  // NOTE: response uses {\"message\":{\"content\":\"...\"}}, NOT {\"choices\":[...]}\n\n")
	sys.WriteString("@sys/llm/decide call (choose the best action from a list):\n")
	sys.WriteString("  // REQUIRED: both 'messages' and 'actions' fields must be present\n")
	sys.WriteString("  decideArgs := mustMarshal(map[string]any{\n")
	sys.WriteString("    \"messages\": []any{map[string]any{\"role\": \"user\", \"content\": task}},\n")
	sys.WriteString("    \"actions\": []any{\"@alice/calculator\", \"@alice/other-action\"},\n")
	sys.WriteString("  })\n")
	sys.WriteString("  decideBytes, _ := JuiceCall(\"@sys/llm/decide\", decideArgs)\n")
	sys.WriteString("  var decideReply struct{ Action string `json:\"action\"`; Args map[string]any `json:\"args\"` }\n")
	sys.WriteString("  json.Unmarshal(decideBytes, &decideReply)  // decideReply.Action is \"@alice/calculator\"\n\n")
	sys.WriteString("Minimal working example (echo action):\n")
	sys.WriteString("```go\n")
	sys.WriteString("func Handle(in map[string]any) (map[string]any, error) {\n")
	sys.WriteString("\ttext, _ := in[\"text\"].(string)\n")
	sys.WriteString("\treturn map[string]any{\"result\": text}, nil\n")
	sys.WriteString("}\n")
	sys.WriteString("```\n")

	var userMsg strings.Builder
	userMsg.WriteString("Implement the action described above.")
	if len(diagnostics) > 0 {
		userMsg.WriteString("\n\nFix these errors from the previous attempt:\n")
		for _, d := range diagnostics {
			userMsg.WriteString("- " + d + "\n")
		}
	}

	return []any{
		map[string]any{"role": "system", "content": sys.String()},
		map[string]any{"role": "user", "content": userMsg.String()},
	}
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

// callJSON calls @sys/llm/json with schema-constrained decoding and returns the validated
// value. The reply is guaranteed to satisfy outputSchema — Ollama constrains generation to
// the schema (format=schema, temperature 0) and @sys/llm/json validates locally — so callers
// parse it directly without the prose-stripping heuristics plain @sys/llm/chat requires.
// Returns the value, or a non-empty diagnostic string on failure.
func callJSON(ctx context.Context, messages []any, outputSchema map[string]any, targetID, processID, parentTraceID string, k *kernel.Kernel) (any, string) {
	result, err := callAction(ctx, "@sys/llm/json", map[string]any{
		"messages":      messages,
		"output_schema": outputSchema,
	}, targetID, processID, parentTraceID, k)
	if err != nil {
		return nil, err.Error()
	}
	return result["value"], ""
}

// callDecide calls @sys/llm/decide and returns the selected action reference and its args.
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

// generateAndRunExamples asks the LLM for 3 example inputs, executes them against the WASM,
// and validates the output structure. Failures to produce or run examples are recorded as
// failed tests — never silently treated as passing.
func generateAndRunExamples(ctx context.Context, contract *actionContract, wasm []byte, targetID, processID, parentTraceID string, k *kernel.Kernel, scripts kernel.ScriptExecutor) []MakeTest {
	if scripts == nil {
		return []MakeTest{{Name: "compile", Status: "failed", Reason: "script executor not configured"}}
	}
	artifact, _, err := scripts.Compile(ctx, wasm)
	if err != nil {
		return []MakeTest{{Name: "compile", Status: "failed", Reason: fmt.Sprintf("WASM compile: %v", err)}}
	}
	results := []MakeTest{{Name: "compile", Status: "passed"}}

	// Schema-constrained example generation: ask @sys/llm/json for an object holding an
	// "examples" array whose items conform to the action's (sanitized) input schema. Ollama
	// constrains generation to the schema, so every example is a valid input — no prose
	// stripping, no parse guessing. The object envelope is required because @sys/llm/json's
	// own output_schema constrains its "value" to an object (a bare array would be rejected).
	itemSchema := sanitizeSchemaForRegistration(contract.InputSchema)
	envelope := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"examples": map[string]any{"type": "array", "items": itemSchema},
		},
		"required": []any{"examples"},
	}
	inJSON, _ := json.MarshalIndent(contract.InputSchema, "", "  ")
	prompt := fmt.Sprintf(`Generate 3 realistic, distinct example inputs for an action with this input schema.
Return an object with an "examples" array of exactly 3 objects, each conforming to the schema.

Schema:
%s`, string(inJSON))

	value, diag := callJSON(ctx, []any{map[string]any{"role": "user", "content": prompt}}, envelope, targetID, processID, parentTraceID, k)
	if diag != "" {
		results = append(results, MakeTest{Name: "examples", Status: "failed", Reason: "could not generate example inputs: " + diag})
		return results
	}
	obj, ok := value.(map[string]any)
	if !ok {
		results = append(results, MakeTest{Name: "examples", Status: "failed", Reason: "example output was not a JSON object"})
		return results
	}
	rawArr, ok := obj["examples"].([]any)
	if !ok {
		results = append(results, MakeTest{Name: "examples", Status: "failed", Reason: "example output missing \"examples\" array"})
		return results
	}
	var examples []map[string]any
	for _, e := range rawArr {
		if m, ok := e.(map[string]any); ok {
			examples = append(examples, m)
		}
	}
	if len(examples) < 3 {
		results = append(results, MakeTest{Name: "examples", Status: "failed",
			Reason: fmt.Sprintf("need 3 example inputs, got %d", len(examples))})
		return results
	}

	outKeys := topLevelKeys(contract.OutputSchema)
	for i, exArgs := range examples[:3] {
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
		var missing []string
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

// supportedSchemaTypes are the JSON Schema "type" values the kernel's ValidateSchema accepts.
var supportedSchemaTypes = map[string]bool{
	"object": true, "array": true, "string": true,
	"integer": true, "number": true, "boolean": true,
}

// sanitizeSchemaForRegistration strips unsupported JSON Schema keywords, drops malformed
// "type" values (a local model may emit garbage like "::_string" or a non-string type), and
// ensures every property has a description. The result is guaranteed to pass ValidateSchema —
// both at activation and when used as the items schema for @sys/llm/json example generation.
// A node whose type is dropped becomes unconstrained (accept-any), which is safe.
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
	if t, ok := result["type"]; ok {
		if s, isStr := t.(string); !isStr || !supportedSchemaTypes[s] {
			delete(result, "type")
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

// extractJSON returns the first balanced {...} or [...] JSON span in s, skipping any prose
// the model prepends. Used for @sys/llm/chat replies (contract derivation); @sys/llm/json
// replies are already schema-constrained and need no extraction.
func extractJSON(s string) string {
	obj := strings.Index(s, "{")
	arr := strings.Index(s, "[")
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
