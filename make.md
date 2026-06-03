# Juice Action Make — Specification

**Version:** 0.3
**Status:** design requirement

## 1. Scope

This document specifies the synthesis pipeline that turns a natural-language task description into a registered, active WASM action. The pipeline is implemented as a single native action, `@sys/make`, callable through `Call()` and subject to the same funding, tracing, and ACL rules as every other action. Its output is an ordinary `Action` row owned by the caller.

`@sys/make` is an optional extension. It is not a required bootstrap invariant and is disabled by default; see §7.

`kernel` must not import a TinyGo toolchain or any subprocess-invoking code. The source compiler is an optional dependency injected at construction, analogous to `ScriptExecutor` and `Chatter`.

## 2. TinyGo SDK

### 2.1 Language choice

Generated WASM actions are written in TinyGo. Standard Go (`GOOS=wasip1 GOARCH=wasm`) cannot declare host function imports under arbitrary module names — it supports only the fixed WASI import namespace. The existing kernel uses a custom `juice` host module; changing that namespace would break every existing WASM action. TinyGo's `//go:wasmimport` annotation accepts any `(module, name)` pair and is therefore the only Go-family compiler that satisfies the current ABI without modifications to the kernel. Over AssemblyScript, TinyGo requires no Node.js runtime and produces a single-binary toolchain install. Over Rust, TinyGo generates code that LLMs can produce reliably from a short in-context SDK, since the generated actions are simple functions rather than systems programs.

### 2.2 SDK

The Juice WASM SDK is a single TinyGo source file stored at `script/tinygosdk/sdk.tmpl`. It is embedded into the `script` package via `//go:embed` as a `string` constant (`TinyGoSDK`) and prepended to every generated action's source before compilation. The `.tmpl` extension reflects that the SDK is input text for an external compiler, not production Go for this kernel; a `.go` file in a subdirectory would create a new package and violate the package constraint in CLAUDE.md. Its complete public surface:

```go
// Host imports — declared; implementations provided by the kernel.
//go:wasmimport juice call
func hostCall(namePtr, nameLen, argsPtr, argsLen uint32) (uint32, uint32)

//go:wasmimport juice emit
func hostEmit(eventPtr, eventLen, argsPtr, argsLen uint32)

//go:wasmimport juice log
func hostLog(lvlPtr, lvlLen, msgPtr, msgLen uint32)

// alloc must be exported so the kernel can write inputs into WASM memory.
//export alloc
func alloc(size uint32) uint32

// JuiceCall calls @owner/name with JSON args and returns JSON result bytes.
func JuiceCall(action string, args []byte) ([]byte, error)

// JuiceEmit emits a named event with JSON args through the kernel event path.
func JuiceEmit(event string, args []byte) error

// JuiceLog writes a structured log record associated with the current trace.
func JuiceLog(level, msg string)
```

Generated code must provide one additional export, not part of the SDK:

```go
//export run
func run(inputPtr, inputLen uint32) (uint32, uint32)
```

`run` receives the input JSON via linear memory and returns the output JSON via the same mechanism. The SDK handles memory layout; generated code calls `JuiceCall`, `JuiceEmit`, and `JuiceLog` and never touches memory pointers directly.

The Go file in `package script` that embeds `sdk.tmpl` must have a test verifying that the embedded SDK compiles to a valid WASM module with the correct exports and no disallowed imports.

## 3. Script package additions

The two interfaces have distinct responsibilities:

- `SourceCompiler`: TinyGo source bytes → WASM bytes. Invokes an external compiler.
- `ScriptExecutor.Compile`: WASM bytes → runtime-validated artifact + SHA-256 hash. Runs inside the process via wazero.

`kernel.SourceCompiler`:

```go
type SourceCompiler interface {
    CompileSource(ctx context.Context, source []byte) (artifact []byte, hash string, err error)
}
```

The implementation in `script` invokes `tinygo build -target wasm` in a subprocess inside a fresh temporary directory. No intentional network inputs are passed to the build; the subprocess has access only to the local filesystem and the TinyGo toolchain. The compilation timeout is `cfg.CompileTimeoutMS` (default 30 000 ms), independent of `cfg.TimeoutMS`; these are decoupled because compilation dominates execution time and must not be capped by the per-call execution limit.

`CompileSource` returns `ErrInvalidInput` on compilation failure (compiler stderr is included) and `ErrInvalidState` when the `tinygo` binary is absent from PATH.

`Kernel` gains a `compiler SourceCompiler` field set at construction. When nil, `@sys/make` returns `ErrInvalidState`.

### 3.1 Source type detection and WASM cache

`action.Source` stores TinyGo source for synthesized actions. Existing actions may store compiled WASM bytes in `action.Source`; the magic-byte test (`\x00asm` at offset 0) is the only distinction between the two representations. Synthesized actions store TinyGo source because it is inspectable and re-compilable; WASM bytes in `action.Source` remain valid and are passed directly to `ScriptExecutor.Compile` as before.

All paths that currently call `k.scripts.Compile(ctx, []byte(action.Source))` — `CreateAction`, `SetActive`, and `executeWasm` — first check whether the source is already compiled WASM by testing for the WASM magic bytes (`\x00asm` at offset 0). If the source starts with the magic bytes, it is passed directly to `ScriptExecutor.Compile` as before. If not, `k.compiler.CompileSource` is called first to produce WASM bytes, which are then passed to `ScriptExecutor.Compile`.

To avoid re-invoking the TinyGo toolchain on every call, `script.Executor` gains an in-process WASM bytes cache:

```go
wasmCache map[string][]byte  // keyed by SHA-256(TinyGo source)
```

On a cache hit, the cached WASM bytes are passed directly to `ScriptExecutor.Compile` (which has its own wazero module cache keyed by artifact hash). On a cache miss — after a process restart — the TinyGo source is recompiled and the result is stored. The per-action recompilation cost is bounded by `CompileTimeoutMS` and is expected to be rare.

A free function `InspectModule` enumerates both the `(module, name)` import pairs and the exported names from a WASM binary without executing it, using wazero's module decoder:

```go
func InspectModule(artifact []byte) (imports []ImportedFunc, exports []string, err error)

type ImportedFunc struct{ Module, Name string }
```

## 4. Context document

The context document is a UTF-8 plain-text string constructed at each LLM call so it reflects the current action catalog. It contains, in order:

1. The complete text of `TinyGoSDK` (the embedded `sdk.tmpl` content).
2. A fixed prose section (one sentence each) describing: the `@owner/name` call convention; the JSON I/O contract over linear memory; what `juice.call`, `juice.emit`, and `juice.log` do semantically.
3. An explicit dynamic dispatch pattern showing how to call `@sys/lookup` at runtime and use the result to invoke a sub-action:

```go
// To call a sub-action by capability, look it up first:
rawResults, _ := JuiceCall("@sys/lookup", mustMarshal(map[string]any{
    "query": "describe the capability you need here",
    "limit": 1,
}))
var resp struct {
    Results []struct {
        OwnerHandle string `json:"owner_handle"`
        Name        string `json:"name"`
    } `json:"results"`
}
json.Unmarshal(rawResults, &resp)
if len(resp.Results) == 0 {
    // handle: no suitable action found, implement inline or return error
}
actionRef := resp.Results[0].OwnerHandle + "/" + resp.Results[0].Name
result, err := JuiceCall(actionRef, args)
```

This pattern is included verbatim because compositionality — calling existing actions rather than reimplementing their logic — is the primary mechanism by which synthesized actions stay small, correct, and maintainable.

4. The active action catalog: for each active public action, a block of the form `@owner/name | description | input_schema | output_schema | price | uses | successes | latency_mean_ms`.
5. The synthesis request: the task description; the caller-supplied `input_schema` and `output_schema` if provided.

## 5. MakeSpec

The first LLM call produces a `MakeSpec` JSON object describing the action before code exists. Validating it before code generation catches structural problems (invalid schemas, unresolvable queries) without spending compilation time.

```json
{
  "name":           "string — action name, may use / as path separator, no @",
  "description":    "string — non-empty",
  "input_schema":   { "type": "object", "...": "..." },
  "output_schema":  { "type": "object", "...": "..." },
  "dependencies":   [{"query": "string", "purpose": "string"}],
  "price":          0,
  "failure_modes":  ["string"]
}
```

`dependencies` is a list of capability queries, not hard-coded action references. Each entry describes a subtask the generated action will delegate to an existing action found via `@sys/lookup` at runtime. The LLM must set `price` to be no less than the expected sum of sub-action prices per invocation; a price below total sub-call cost makes the action unprofitable for its owner and will cause the contractor model to debit more from `owner.available` than the caller pays.

Validation rules:

- `name` is non-empty, contains no `@`, and conforms to the action name grammar (§3 of requirements.md).
- `description` is non-empty.
- `input_schema` passes `kernel.ValidateSchema`.
- `output_schema` passes `kernel.ValidateSchema`.
- For each entry in `dependencies`: `query` is non-empty. Stage 2 determines inline-vs-dynamic by running `k.Lookup(query)` and comparing the top score against `JUICE_MAKE_LOOKUP_THRESHOLD` (default 0.7); no explicit `inline` flag is needed in `MakeSpec`.
- `price` is non-negative.

Validation failure returns `ErrInvalidInput`. No retry is attempted at this stage.

## 6. The @sys/make native action

`@sys/make` is registered at bootstrap when `JUICE_MAKE_ENABLED=true`. It is public and grant-all. Its price is set via config key `make_price` (env: `JUICE_MAKE_PRICE`; default 0).

Staging the pipeline across multiple native actions would require the caller to orchestrate them and creates partial-state cleanup problems (a stage-6 failure has already consumed LLM calls). `@sys/make` runs all stages internally; the retry at stage 4 is the only recovery path needed because compilation is the only stage with a recoverable transient failure mode.

**Input schema:**

| Field | Type | Required | Description |
|---|---|---|---|
| `task` | string | yes | Natural-language description of the desired action |
| `input_schema` | object | no | Desired input JSON Schema |
| `output_schema` | object | no | Desired output JSON Schema |

**Output schema:**

| Field | Type | Description |
|---|---|---|
| `action_id` | string | ID of the newly registered action |
| `action_ref` | string | `@owner/name` form for use in subsequent calls |
| `dry_run_tx_id` | string | Transaction ID of the first successful test call |
| `spec` | object | The `MakeSpec` produced at stage 1 |

### 6.1 Subject ID threading

`executeNative` takes an explicit `subjectID` parameter:

```go
func (k *Kernel) executeNative(ctx context.Context, subjectID string, action *Action, args map[string]any) (map[string]any, error)
```

`Call()` passes `req.SubjectID` at the dispatch site. `executeMake` receives `subjectID` as a parameter and uses it directly, keeping identity explicit and testable without ambient context state.

### 6.2 Pipeline stages

**Stage 1 — Spec generation**

Build the context document. Call `k.chatter.Chat()` with a system prompt instructing the model to output a `MakeSpec` JSON block and nothing else. Parse and validate per §5. On failure, return `ErrInvalidInput` immediately.

**Stage 2 — Composition plan**

For each entry in `spec.dependencies`, call `k.Lookup()` with `entry.query`. If `score >= JUICE_MAKE_LOOKUP_THRESHOLD`, mark as "dynamic call" — the generated code will call `@sys/lookup` at runtime with that query. If below the threshold, mark as "inline" — the logic must be implemented directly. The plan is a Go-level data structure; the lookup decision is kept in Go rather than delegated to the LLM so it remains testable and deterministic.

**Stage 3 — Code generation**

Build the context document, appending the `MakeSpec` and the composition plan. Call `k.chatter.Chat()` instructing the model to produce a single TinyGo file that imports the SDK and exports `run`. For each "dynamic call" dependency, the generated code must use the `@sys/lookup` dispatch pattern from the context document with the dependency's `query` string. Extract the first Go code block from the response. Prepend `TinyGoSDK` to produce the full source.

**Stage 4 — Compilation**

Call `k.compiler.CompileSource()`. On failure, retry once: append the compiler error to the code-generation context, repeat stage 3, repeat compilation. A second pass with the compiler error message recovers the common failure class (wrong import path, missing export). A third attempt rarely succeeds without a structural change to the spec; at that point the task description is the problem. If the second attempt fails, return `ErrExecutionFailed` with the last compiler error.

**Stage 5 — Static checks**

Call `script.InspectModule()` on the compiled artifact. The check passes iff:

- `imports(artifact) ⊆ {(juice, call), (juice, emit), (juice, log)}`
- `exports(artifact) ⊇ {alloc, run}`
- For each "dynamic call" dependency: the literal string `@sys/lookup` appears in the extracted source.

Any violation returns `ErrInvalidInput`. These checks are limited to claims the implementation can enforce by static analysis of the WASM binary and source text; runtime behavior is validated by the dry run.

**Stage 6 — Registration**

The action is created at `price = 0` for the duration of the dry run. This avoids self-payment fees during testing: a price-0 call costs the test process nothing, so no fee accrues to the fee recipient from quality-control work. After a successful dry run the price is updated to `spec.price`.

Call `k.CreateAction()`:

```
OwnerUserID  = callerID
Name         = spec.name
Kind         = KindWasm
Description  = spec.description
InputSchema  = spec.input_schema
OutputSchema = spec.output_schema
Source       = tinyGoSource   (the generated TinyGo source, not WASM bytes)
Price        = 0
Active       = false
```

`CreateAction` detects that the source is TinyGo (no WASM magic bytes), calls `k.compiler.CompileSource`, passes the result to `k.scripts.Compile`, and sets `ArtifactHash` from the compiled WASM hash. The TinyGo source is stored in `action.Source` and remains inspectable.

**Stage 7 — Test generation and dry run**

The precondition is: `caller.available >= spec.price × len(test_cases)`. Sub-actions called by the synthesized action are funded from `owner.available` via the contractor model (§5.5 of requirements.md), not from the test process. `spec.price` must cover expected sub-call costs per invocation (§5 constraint), so this check is a sufficient proxy. If the precondition fails, return `ErrInsufficientFunds` before creating any process.

The `@sys` activation postcondition: `@sys` activates a caller-owned action only after `Owner(a) = caller ∧ StaticOK(a) ∧ DryRunOK(a)`. This limits the scope of `@sys`'s implicit admin authority to a well-defined synthesis postcondition.

Call `k.chatter.Chat()` with the spec, requesting a JSON array of test cases, each `{args: object, expect_success: bool}`. The minimum set is two cases: one schema-valid expected-success case and one schema-invalid expected-failure case. If the LLM returns fewer than two, the pipeline constructs the missing schema-invalid case as an empty `{}` object when the input schema requires fields.

Activate the action: `k.SetActive(ctx, sysUserID, actionID, true)`.

Start a test process: `k.StartProcess(ctx, callerID, callerID, 0)`. For each test case, call `k.Call()` using `callerID` as subject, the test process, and the synthesized action. For schema-invalid cases, verify the error is `ErrSchemaViolation`. For expected-success cases, verify success and that the reply satisfies `spec.output_schema`.

Close the test process with `k.EndProcess()` regardless of outcome; unused credits return to the caller.

If any test case fails: `k.SetActive(ctx, sysUserID, actionID, false)`, return `ErrExecutionFailed` with the failing case. By the contractor model (§5.5 of requirements.md), sub-call costs settled during the dry run are not reversed on failure; those funds have already been committed to the sub-action owners.

If all tests pass: call `k.UpdateAction(ctx, sysUserID, {ID: actionID, Price: &spec.price})`, which sets the final price and deactivates the action per existing kernel behavior. Then call `k.SetActive(ctx, sysUserID, actionID, true)` to reactivate. Return `action_id`, `action_ref`, the transaction ID of the first successful test call, and the `MakeSpec`.

## 7. Bootstrap changes

`@sys/make` is an optional extension outside the required native-action set (`@sys/lookup` and `@sys/llm/chat`). Bootstrap registers it only when `JUICE_MAKE_ENABLED=true` (default false). If enabled but `tinygo` is absent from PATH, bootstrap logs a warning and skips registration without aborting. Registration is idempotent.

The `Kernel` constructor gains an optional `compiler SourceCompiler` parameter. When nil and `@sys/make` is called, it returns `ErrInvalidState`.

## 8. Required automated tests

```
SDK compiles to WASM with correct exports {alloc, run} and no disallowed imports
CompileSource returns ErrInvalidInput on TinyGo syntax error
CompileSource returns ErrInvalidState when tinygo binary is absent
InspectModule returns correct imports and exports for a known artifact
Static check rejects artifact with imports outside {juice.call, juice.emit, juice.log}
Static check rejects artifact missing run export
Static check rejects artifact missing alloc export
Magic-byte detection routes WASM source through ScriptExecutor.Compile directly
Magic-byte detection routes TinyGo source through SourceCompiler then ScriptExecutor.Compile
WASM cache hit avoids invoking SourceCompiler a second time for the same TinyGo source
MakeSpec validation rejects empty name
MakeSpec validation rejects invalid input_schema
MakeSpec validation rejects dependency with empty query
MakeSpec validation rejects negative price
Stage 1 produces valid MakeSpec from FakeChatter returning a known JSON block
Stage 2 marks dependency as inline when lookup score is below threshold
Stage 2 marks dependency as dynamic call when lookup score is at or above threshold
Stage 3 generates source containing @sys/lookup call for each dynamic call dependency
Stage 4 retries once on compilation failure, succeeds on second attempt
Stage 4 returns ErrExecutionFailed after two consecutive compilation failures
Stage 5 rejects artifact with disallowed import
Stage 5 rejects artifact missing @sys/lookup reference for a dynamic call dependency
Stage 6 stores TinyGo source in action.Source and sets ArtifactHash from compiled WASM
Stage 6 creates action at price 0
Stage 7 precondition fails with ErrInsufficientFunds when caller.available < spec.price × cases
Stage 7 dry run activates action, runs tests, updates price, reactivates on success
Stage 7 deactivates action and returns ErrExecutionFailed when a test case fails
Stage 7 sub-call costs are funded from caller.available, not the test process
@sys/make is callable through Call() with a funded process (fake chatter + fake compiler)
@sys/make returns ErrInvalidState when compiler is nil
Bootstrap registers @sys/make when enabled; skips without error when tinygo is absent
Bootstrap does not register @sys/make when JUICE_MAKE_ENABLED is false
Synthesized action callable by its owner immediately after successful synthesis
```
