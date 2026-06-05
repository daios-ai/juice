package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RegisterMakeHandler registers the @sys/make native action handler on k.
// sdk is the TinyGo SDK source (script.TinyGoSDK) prepended to every generated action.
// Pass "" in tests that use FakeCompiler (which ignores source content).
func RegisterMakeHandler(k *Kernel, sdk string) {
	k.RegisterNativeHandler("make", func(ctx context.Context, args map[string]any, subjectID, processID, parentTraceID string) (map[string]any, error) {
		return k.executeMake(ctx, args, subjectID, processID, parentTraceID, sdk)
	})
}

// MakeDraft is the generated action draft returned by @sys/make on success.
// Source holds TinyGo source; ArtifactHash is the SHA-256 of the compiled WASM.
type MakeDraft struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"` // always "wasm"
	Description  string `json:"description"`
	Source       string `json:"source"`
	ArtifactHash string `json:"artifact_hash"`
}

// MakeTest records the outcome of one test case run during synthesis.
type MakeTest struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "passed" | "failed"
	Reason string `json:"reason,omitempty"`
}

// MakeResult is the output schema of the @sys/make native action.
type MakeResult struct {
	Status      string     `json:"status"` // "success" | "failure"
	Draft       *MakeDraft `json:"draft,omitempty"`
	Diagnostics []string   `json:"diagnostics"`
	Tests       []MakeTest `json:"tests,omitempty"`
}

// makeInput holds the parsed input to @sys/make.
type makeInput struct {
	Description string
}

// makeTestHost implements HostFunctions for testing generated WASM during synthesis.
// All sub-calls return {} so the smoke test is hermetic.
type makeTestHost struct{}

func (h *makeTestHost) Call(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return []byte("{}"), nil
}
func (h *makeTestHost) Emit(_ context.Context, _ string, _ []byte) error { return nil }
func (h *makeTestHost) Log(_ context.Context, _, _ string) error         { return nil }

// executeMake implements the @sys/make native action.
func (k *Kernel) executeMake(ctx context.Context, args map[string]any, subjectID, processID, parentTraceID, sdk string) (map[string]any, error) {
	if k.compiler == nil {
		return nil, ErrInvalidState.Wrap("source compiler not configured")
	}
	if k.chatter == nil {
		return nil, ErrInvalidState.Wrap("LLM chat service not configured")
	}

	in, err := parseMakeInput(args)
	if err != nil {
		return nil, err
	}

	catalog := k.buildCatalog(ctx)
	diagnostics := []string{}
	tests := []MakeTest{}

	const maxSteps = 5
	for step := 0; step < maxSteps; step++ {
		runFunc, diag := k.generateSource(ctx, in, catalog, sdk, diagnostics, subjectID, processID, parentTraceID)
		if diag != "" {
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: LLM generation failed: %s", step+1, diag))
			continue
		}
		source := prepareSource(sdk, runFunc)

		wasm, hash, compileErr := k.compiler.CompileSource(ctx, []byte(source))
		if compileErr != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("step %d: compile error: %v", step+1, compileErr))
			continue
		}

		if checkDiag := k.checkWASMImports(wasm, nil); checkDiag != "" {
			return marshalMakeResult(&MakeResult{Status: "failure", Diagnostics: append(diagnostics, checkDiag)})
		}

		tests = k.runMakeTests(ctx, wasm)

		allPassed := true
		for _, t := range tests {
			if t.Status == "failed" {
				allPassed = false
				diagnostics = append(diagnostics, fmt.Sprintf("step %d: test %q failed: %s", step+1, t.Name, t.Reason))
			}
		}

		if allPassed {
			draft := &MakeDraft{
				Name:         deriveName(in.Description),
				Kind:         "wasm",
				Description:  in.Description,
				Source:       source,
				ArtifactHash: hash,
			}
			res := &MakeResult{Status: "success", Draft: draft, Diagnostics: diagnostics, Tests: tests}
			return marshalMakeResult(res)
		}
	}

	res := &MakeResult{Status: "failure", Diagnostics: diagnostics, Tests: tests}
	return marshalMakeResult(res)
}

// parseMakeInput validates @sys/make arguments. The only input is a natural language description.
func parseMakeInput(args map[string]any) (*makeInput, error) {
	desc, _ := args["description"].(string)
	if strings.TrimSpace(desc) == "" {
		return nil, ErrInvalidInput.Wrap("description is required")
	}
	return &makeInput{Description: desc}, nil
}


// generateSource calls @sys/llm/chat via kernel.Call() to produce the run function.
func (k *Kernel) generateSource(ctx context.Context, in *makeInput, catalog string, sdk string, prevDiagnostics []string, subjectID, processID, parentTraceID string) (source, diag string) {
	var sb strings.Builder
	sb.WriteString("You are a TinyGo programmer generating a WASM action for the Juice platform.\n\n")
	sb.WriteString("The following SDK code is ALREADY included in the final file — DO NOT redeclare anything from it:\n\n")
	if sdk != "" {
		sb.WriteString("```go\n")
		sb.WriteString(sdk)
		sb.WriteString("\n```\n\n")
	}
	sb.WriteString("Generate ONLY the exported `run` function and any PRIVATE helper functions it needs.\n")
	sb.WriteString("STRICT rules — violating any will cause a compile error:\n")
	sb.WriteString("- NO `package main` line\n")
	sb.WriteString("- NO import statements\n")
	sb.WriteString("- NO redeclaration of: hostCall, hostEmit, hostLog, _nextAlloc, _ptrLen, _strPtrLen, alloc, JuiceCall, JuiceEmit, JuiceLog, mustMarshal, main\n")
	sb.WriteString("- The entry point must be:\n  //export run\n  func run(inputPtr, inputLen uint32) (uint32, uint32)\n")
	sb.WriteString("- Read input: input := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(inputPtr))), int(inputLen))\n")
	sb.WriteString("- Parse: json.Unmarshal(input, &req)\n")
	sb.WriteString("- Return: p, l := _ptrLen(mustMarshal(result)); return p, l\n")
	sb.WriteString("Wrap ONLY the new functions in a ```go ... ``` block.\n\n")

	sb.WriteString("You may call any action from the catalog above using JuiceCall.\n\n")

	sb.WriteString("Available actions in the catalog:\n")
	sb.WriteString(catalog)
	sb.WriteString("\nTask: " + in.Description + "\n")

	if len(prevDiagnostics) > 0 {
		sb.WriteString("\n\nFix these issues from the previous attempt:\n")
		for _, d := range prevDiagnostics {
			sb.WriteString("- " + d + "\n")
		}
	}

	reply, err := k.callLLM(ctx, sb.String(), subjectID, processID, parentTraceID)
	if err != nil {
		return "", err.Error()
	}
	src := extractGoBlock(reply)
	if src == "" {
		return "", "LLM did not return a Go code block"
	}
	return src, ""
}

// callLLM calls @sys/llm/chat through kernel.Call() to produce a sub-transaction and trace.
func (k *Kernel) callLLM(ctx context.Context, prompt, subjectID, processID, parentTraceID string) (string, error) {
	reply, err := k.Call(ctx, CallRequest{
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

// buildCatalog returns a concise text summary of active public actions for the LLM context.
func (k *Kernel) buildCatalog(ctx context.Context) string {
	actions, err := k.store.ListPublicActions(ctx, 50, 0)
	if err != nil || len(actions) == 0 {
		return "(no actions in catalog)\n"
	}
	var sb strings.Builder
	for _, a := range actions {
		if !a.Active {
			continue
		}
		u, _ := k.store.ReadUser(ctx, a.OwnerUserID)
		handle := a.OwnerUserID
		if u != nil {
			handle = u.Handle
		}
		fmt.Fprintf(&sb, "@%s/%s — %s\n", strings.TrimPrefix(handle, "@"), a.Name, a.Description)
	}
	return sb.String()
}

// checkWASMImports validates that the artifact only imports allowed host functions.
// Uses WASMInspector if the script executor implements it; otherwise skips the check.
// Returns a diagnostic string on failure, or "" on success.
func (k *Kernel) checkWASMImports(wasm []byte, _ []string) string {
	insp, ok := k.scripts.(WASMInspector)
	if !ok {
		return ""
	}
	imports, exports, err := insp.InspectWASM(wasm)
	if err != nil {
		return fmt.Sprintf("WASM inspection failed: %v", err)
	}
	allowedImports := map[string]bool{"call": true, "emit": true, "log": true}
	for _, imp := range imports {
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

// runMakeTests runs a compile smoke test against the WASM artifact.
func (k *Kernel) runMakeTests(ctx context.Context, wasm []byte) []MakeTest {
	if k.scripts == nil {
		return []MakeTest{{Name: "compile", Status: "failed", Reason: "script executor not configured"}}
	}
	_, _, err := k.scripts.Compile(ctx, wasm)
	if err != nil {
		return []MakeTest{{Name: "compile", Status: "failed", Reason: fmt.Sprintf("WASM compile: %v", err)}}
	}
	return []MakeTest{{Name: "compile", Status: "passed"}}
}


// prepareSource combines the SDK with the LLM-generated run function.
// It strips any `package main` line from generated to avoid duplicate declarations.
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

// marshalMakeResult converts a MakeResult to the map[string]any expected by Call().
func marshalMakeResult(r *MakeResult) (map[string]any, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, ErrInternal.Wrapf("marshal make result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, ErrInternal.Wrapf("unmarshal make result: %v", err)
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
	// Skip the newline after the fence.
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	}
	end2 := strings.Index(rest, end)
	if end2 < 0 {
		return rest
	}
	return rest[:end2]
}

// extractJSON extracts the first {...} JSON object from a string.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// deriveName creates a slug action name from a description.
func deriveName(desc string) string {
	words := strings.Fields(strings.ToLower(desc))
	if len(words) == 0 {
		return "generated"
	}
	if len(words) > 3 {
		words = words[:3]
	}
	name := strings.Join(words, "-")
	var clean strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			clean.WriteRune(r)
		}
	}
	result := strings.Trim(clean.String(), "-")
	if result == "" {
		return "generated"
	}
	return result
}
