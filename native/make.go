package native

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/daios-ai/juice/kernel"
)

// MakeDeps holds the external dependencies injected into the @sys/make handler at registration.
type MakeDeps struct {
	Store    kernel.Store
	Scripts  kernel.ScriptExecutor
	Compiler kernel.SourceCompiler
	Chatter  kernel.Chatter
	Embedder kernel.Embedder
}

// RegisterMakeHandler registers the @sys/make native action handler on k.
// sdk is the TinyGo SDK source (script.TinyGoSDK) prepended to every generated action.
// maxSteps is the maximum number of synthesis repair iterations; 0 uses the default of 5.
func RegisterMakeHandler(k *kernel.Kernel, deps MakeDeps, sdk string, maxSteps int) {
	if maxSteps <= 0 {
		maxSteps = 5
	}
	k.RegisterNativeHandler("make", func(ctx context.Context, args map[string]any, subjectID, processID, parentTraceID string) (map[string]any, error) {
		return executeMake(ctx, args, subjectID, processID, parentTraceID, k, deps, sdk, maxSteps)
	})
}

// actionContract is produced by step 2 (contract derivation).
// Schemas are NOT validated with ValidateSchema — the LLM may produce valid JSON Schema
// keywords that our strict user-input validator rejects.
type actionContract struct {
	Name         string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Plan         string // identifies capabilities needed for implementation
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
func (h *makeTestHost) Emit(_ context.Context, _ string, _ []byte) error { return nil }
func (h *makeTestHost) Log(_ context.Context, _, _ string) error         { return nil }

// executeMake implements the full @sys/make 10-step pipeline.
func executeMake(ctx context.Context, args map[string]any, subjectID, processID, parentTraceID string, k *kernel.Kernel, deps MakeDeps, sdk string, maxSteps int) (map[string]any, error) {
	if deps.Compiler == nil {
		return nil, kernel.ErrInvalidState.Wrap("source compiler not configured")
	}
	if deps.Chatter == nil {
		return nil, kernel.ErrInvalidState.Wrap("LLM chat service not configured")
	}

	// Step 1: validate input.
	in, err := parseMakeInput(args)
	if err != nil {
		return nil, err
	}

	// Step 2: derive contract + plan.
	contract, diag := deriveContract(ctx, in.Description, subjectID, processID, parentTraceID, k)
	if diag != "" {
		return marshalMakeResult(&MakeResult{
			Status:      "failure",
			Diagnostics: []string{"contract derivation failed: " + diag},
			Tests:       []MakeTest{},
		})
	}

	// Step 3: resolve explicit @owner/name references in the description.
	refs := resolveActionRefs(ctx, in.Description, deps.Store)

	// Step 4: search catalog based on capabilities identified in the plan.
	found := searchCatalog(ctx, contract, subjectID, processID, parentTraceID, k, deps)

	// Merge steps 3+4, deduplicated by action ID.
	composable := mergeActions(refs, found)

	diagnostics := []string{}
	tests := []MakeTest{}

	for step := 0; step < maxSteps; step++ {
		// Step 5: generate TinyGo source.
		runFunc, genDiag := generateSource(ctx, in, contract, composable, sdk, diagnostics, subjectID, processID, parentTraceID, k, deps.Store)
		if genDiag != "" {
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: LLM generation failed: %s", step+1, genDiag))
			continue
		}
		source := prepareSource(sdk, runFunc)

		// Step 6: compile to WASM.
		wasm, _, compileErr := deps.Compiler.CompileSource(ctx, []byte(source))
		if compileErr != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: compile error: %v", step+1, compileErr))
			continue
		}

		// Step 7: validate WASM artifact.
		if checkDiag := checkWASMImports(wasm, deps.Scripts); checkDiag != "" {
			return marshalMakeResult(&MakeResult{
				Status:      "failure",
				Diagnostics: append(diagnostics, checkDiag),
				Tests:       tests,
			})
		}

		// Step 8: generate examples and run them.
		tests = generateAndRunExamples(ctx, contract, wasm, subjectID, processID, parentTraceID, k, deps.Scripts)

		allPassed := true
		for _, t := range tests {
			if t.Status == "failed" {
				allPassed = false
				diagnostics = append(diagnostics, fmt.Sprintf("step %d: test %q failed: %s", step+1, t.Name, t.Reason))
			}
		}

		// Step 10: register and activate the action under the caller's account.
		if allPassed {
			inSchema := sanitizeSchemaForRegistration(contract.InputSchema)
			outSchema := sanitizeSchemaForRegistration(contract.OutputSchema)
			action, createErr := k.CreateAction(ctx, subjectID, kernel.CreateActionRequest{
				OwnerUserID:  subjectID,
				Name:         contract.Name,
				Kind:         kernel.KindWasm,
				Price:        0,
				Description:  in.Description,
				InputSchema:  inSchema,
				OutputSchema: outSchema,
				Source:       string(wasm),
			})
			if createErr != nil {
				return marshalMakeResult(&MakeResult{
					Status:      "failure",
					Diagnostics: append(diagnostics, "registration failed: "+createErr.Error()),
					Tests:       tests,
				})
			}
			if activateErr := k.SetActive(ctx, subjectID, action.ID, true); activateErr != nil {
				return marshalMakeResult(&MakeResult{
					Status:      "failure",
					Diagnostics: append(diagnostics, "activation failed: "+activateErr.Error()),
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
		// Step 9: loop with diagnostics.
	}

	// Step 10: return failure.
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

// deriveContract calls @sys/llm/chat to produce a name, input schema, output schema,
// and implementation plan from the natural language description.
func deriveContract(ctx context.Context, description, subjectID, processID, parentTraceID string, k *kernel.Kernel) (*actionContract, string) {
	prompt := fmt.Sprintf(`You are designing a callable API action for the Juice platform.
Given the description, produce a JSON object with exactly these fields:
{
  "name": "short-kebab-slug (3 words max)",
  "input_schema": { "type": "object", "properties": { ... }, "required": [...] },
  "output_schema": { "type": "object", "properties": { ... }, "required": [...] },
  "plan": "concise implementation plan; list any external capabilities needed (e.g. weather data, geocoding)"
}

Respond with ONLY the JSON object, nothing else.

Description: %s`, description)

	reply, err := callLLM(ctx, prompt, subjectID, processID, parentTraceID, k)
	if err != nil {
		return nil, err.Error()
	}

	jsonStr := extractJSON(reply)
	if jsonStr == "" {
		return nil, "LLM did not return a JSON object"
	}

	var raw struct {
		Name         string         `json:"name"`
		InputSchema  map[string]any `json:"input_schema"`
		OutputSchema map[string]any `json:"output_schema"`
		Plan         string         `json:"plan"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Sprintf("invalid JSON: %v", err)
	}
	if raw.Name == "" || raw.InputSchema == nil || raw.OutputSchema == nil {
		return nil, "contract missing required fields (name, input_schema, output_schema)"
	}

	return &actionContract{
		Name:         raw.Name,
		InputSchema:  raw.InputSchema,
		OutputSchema: raw.OutputSchema,
		Plan:         raw.Plan,
	}, ""
}

var actionRefRe = regexp.MustCompile(`@[\w-]+/[\w./-]+`)

// resolveActionRefs extracts explicit @owner/name references from the description
// and looks them up in the store.
func resolveActionRefs(ctx context.Context, description string, store kernel.Store) []*kernel.Action {
	matches := actionRefRe.FindAllString(description, -1)
	seen := map[string]bool{}
	var result []*kernel.Action
	for _, ref := range matches {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		idx := strings.Index(ref[1:], "/")
		if idx < 0 {
			continue
		}
		ownerHandle := ref[:idx+1]
		actionName := ref[idx+2:]
		owner, err := store.ReadUserByHandle(ctx, ownerHandle)
		if err != nil {
			continue
		}
		a, err := store.ReadActionByOwnerName(ctx, owner.ID, actionName)
		if err != nil || a == nil || !a.Active {
			continue
		}
		result = append(result, a)
	}
	return result
}

// searchCatalog calls @sys/lookup for each capability identified in the contract plan.
// Actions with a failure rate above 50% (over at least 5 uses) are excluded.
func searchCatalog(ctx context.Context, contract *actionContract, subjectID, processID, parentTraceID string, k *kernel.Kernel, deps MakeDeps) []*kernel.Action {
	if contract.Plan == "" || deps.Embedder == nil {
		return nil
	}

	queries := extractCapabilityQueries(contract.Plan)

	seen := map[string]bool{}
	var result []*kernel.Action

	for _, q := range queries {
		if q == "" {
			continue
		}
		reply, err := k.Call(ctx, kernel.CallRequest{
			SubjectID:     subjectID,
			ProcessID:     processID,
			ParentTraceID: parentTraceID,
			TargetUserID:  "@sys",
			ActionName:    "lookup",
			Args:          map[string]any{"query": q, "limit": float64(5)},
		})
		if err != nil {
			continue
		}
		items, _ := reply.Result["results"].([]any)
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			actionID, _ := m["action_id"].(string)
			if seen[actionID] {
				continue
			}
			seen[actionID] = true
			a, err := deps.Store.ReadAction(ctx, actionID)
			if err != nil || a == nil || !a.Active {
				continue
			}
			if isUnreliableAction(deps.Store.ReadStats(ctx, a.ID)) {
				continue
			}
			result = append(result, a)
		}
	}
	return result
}

// isUnreliableAction returns true when stats show a failure rate above 50% over at least 5 uses.
func isUnreliableAction(stats *kernel.Stats, _ error) bool {
	if stats == nil {
		return false
	}
	total := stats.Successes + stats.Failures
	if total < 5 {
		return false
	}
	return stats.Failures > stats.Successes
}

// extractCapabilityQueries splits a plan string into focused search queries.
func extractCapabilityQueries(plan string) []string {
	plan = strings.ReplaceAll(plan, ";", "\n")
	plan = strings.ReplaceAll(plan, ",", "\n")
	parts := strings.Split(plan, "\n")
	var queries []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimLeft(p, "-•*123456789. ")
		if len(p) > 4 {
			queries = append(queries, p)
		}
	}
	return queries
}

// mergeActions combines two action lists, deduplicating by action ID.
func mergeActions(a, b []*kernel.Action) []*kernel.Action {
	seen := map[string]bool{}
	var result []*kernel.Action
	for _, list := range [][]*kernel.Action{a, b} {
		for _, act := range list {
			if !seen[act.ID] {
				seen[act.ID] = true
				result = append(result, act)
			}
		}
	}
	return result
}

// generateSource calls @sys/llm/chat to produce the TinyGo run function.
func generateSource(ctx context.Context, in *makeInput, contract *actionContract, composable []*kernel.Action, sdk string, prevDiagnostics []string, subjectID, processID, parentTraceID string, k *kernel.Kernel, store kernel.Store) (source, diag string) {
	var sb strings.Builder
	sb.WriteString("You are a TinyGo programmer generating a WASM action for the Juice platform.\n\n")

	sb.WriteString("The following SDK is ALREADY included — DO NOT redeclare anything from it:\n```go\n")
	if sdk != "" {
		sb.WriteString(sdk)
	}
	sb.WriteString("\n```\n\n")

	sb.WriteString("Action contract:\n")
	inJSON, _ := json.MarshalIndent(contract.InputSchema, "", "  ")
	outJSON, _ := json.MarshalIndent(contract.OutputSchema, "", "  ")
	sb.WriteString("  name: " + contract.Name + "\n")
	sb.WriteString("  input_schema: " + string(inJSON) + "\n")
	sb.WriteString("  output_schema: " + string(outJSON) + "\n\n")

	if len(composable) > 0 {
		sb.WriteString("Composable actions available via JuiceCall(\"@owner/name\", argsJSON):\n")
		for _, a := range composable {
			ownerHandle := a.OwnerUserID
			u, err := store.ReadUser(ctx, a.OwnerUserID)
			if err == nil && u != nil {
				ownerHandle = u.Handle
			}
			ref := fmt.Sprintf("@%s/%s", strings.TrimPrefix(ownerHandle, "@"), a.Name)
			sb.WriteString(fmt.Sprintf("  %s — %s\n", ref, a.Description))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Task: " + in.Description + "\n\n")

	sb.WriteString("STRICT rules:\n")
	sb.WriteString("- Generate ONLY the //export run function and private helpers\n")
	sb.WriteString("- NO package main, NO imports, NO redeclaration of SDK functions\n")
	sb.WriteString("- Forbidden names: hostCall hostEmit hostLog _ptrLen _strPtrLen alloc JuiceCall JuiceEmit JuiceLog mustMarshal main\n")
	sb.WriteString("- Entry point must return packed i64 (upper 32 bits = outputPtr, lower 32 bits = outputLen):\n")
	sb.WriteString("  //export run\n  func run(inputPtr, inputLen uint32) uint64 {\n    input := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(inputPtr))), int(inputLen))\n    ...\n    p, l := _ptrLen(mustMarshal(result)); return uint64(p)<<32 | uint64(l)\n  }\n")
	sb.WriteString("- Use named struct types for JSON input/output (preferred over map[string]any)\n")
	sb.WriteString("- For sub-calls: argsJSON := mustMarshal(reqStruct); reply, _ := JuiceCall(\"@owner/name\", argsJSON)\n")
	sb.WriteString("Wrap the code in ```go ... ```\n")

	if len(prevDiagnostics) > 0 {
		sb.WriteString("\nFix these issues from the previous attempt:\n")
		for _, d := range prevDiagnostics {
			sb.WriteString("- " + d + "\n")
		}
	}

	reply, err := callLLM(ctx, sb.String(), subjectID, processID, parentTraceID, k)
	if err != nil {
		return "", err.Error()
	}
	src := extractGoBlock(reply)
	if src == "" {
		return "", "LLM did not return a Go code block"
	}
	return src, ""
}

// generateAndRunExamples asks the LLM for test inputs, executes them against the WASM,
// and validates the output structure.
func generateAndRunExamples(ctx context.Context, contract *actionContract, wasm []byte, subjectID, processID, parentTraceID string, k *kernel.Kernel, scripts kernel.ScriptExecutor) []MakeTest {
	if scripts == nil {
		return []MakeTest{{Name: "compile", Status: "failed", Reason: "script executor not configured"}}
	}

	artifact, _, err := scripts.Compile(ctx, wasm)
	if err != nil {
		return []MakeTest{{Name: "compile", Status: "failed", Reason: fmt.Sprintf("WASM compile: %v", err)}}
	}
	results := []MakeTest{{Name: "compile", Status: "passed"}}

	inJSON, _ := json.MarshalIndent(contract.InputSchema, "", "  ")
	prompt := fmt.Sprintf(`Given this JSON input schema, generate 2 realistic example inputs.
Respond with ONLY a JSON array of objects, each matching the schema.

Schema:
%s`, string(inJSON))

	reply, err := callLLM(ctx, prompt, subjectID, processID, parentTraceID, k)
	if err != nil {
		return results
	}

	jsonStr := extractJSON(reply)
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

// callLLM calls @sys/llm/chat through kernel.Call() producing a sub-transaction and trace.
func callLLM(ctx context.Context, prompt, subjectID, processID, parentTraceID string, k *kernel.Kernel) (string, error) {
	reply, err := k.Call(ctx, kernel.CallRequest{
		SubjectID:     subjectID,
		ProcessID:     processID,
		ParentTraceID: parentTraceID,
		TargetUserID:  "@sys",
		ActionName:    "llm/chat",
		Args: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": prompt},
			},
		},
	})
	if err != nil {
		return "", err
	}
	msg, _ := reply.Result["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return content, nil
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
	allowedImports := map[string]bool{"call": true, "emit": true, "log": true}
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
	if i := strings.Index(s, "{"); i >= 0 {
		depth := 0
		for j := i; j < len(s); j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return s[i : j+1]
				}
			}
		}
	}
	if i := strings.Index(s, "["); i >= 0 {
		depth := 0
		for j := i; j < len(s); j++ {
			switch s[j] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					return s[i : j+1]
				}
			}
		}
	}
	return ""
}
