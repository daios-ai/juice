# Juice Action Make — Specification

**Version:** 0.1
**Status:** design requirement

## 1. Scope

This document specifies the synthesis pipeline that turns a natural-language task description into a registered, active WASM action. The pipeline is implemented as a single native action, `@sys/make`, callable through `Call()` and subject to the same funding, tracing, and ACL rules as every other action. Its output is an ordinary `Action` row owned by the caller.

`kernel` must not import a TinyGo toolchain or any subprocess-invoking code. The source compiler is an optional dependency injected at construction, analogous to `ScriptExecutor` and `Chatter`.

## 2. TinyGo SDK

### 2.1 Language choice

Generated WASM actions are written in TinyGo. Standard Go (`GOOS=wasip1 GOARCH=wasm`) cannot declare host function imports under arbitrary module names — it supports only the fixed WASI import namespace. The existing kernel uses a custom `juice` host module; changing that namespace would break every existing WASM action. TinyGo's `//go:wasmimport` annotation accepts any `(module, name)` pair and is therefore the only Go-family compiler that satisfies the current ABI without modifications to the kernel. Over AssemblyScript, TinyGo requires no Node.js runtime and produces a single-binary toolchain install. Over Rust, TinyGo generates code that LLMs can produce reliably from a short in-context SDK, since the generated actions are simple functions, not systems programs.

### 2.2 SDK

The Juice WASM SDK is a single TinyGo source file stored at `script/tinygosdk/sdk.go`. It is embedded into the `script` package as a `string` constant (`TinyGoSDK`) and prepended to every generated action's source before compilation. Its complete public surface:

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

`run` receives the input JSON via linear memory and returns the output JSON via the same mechanism. The SDK handles memory layout; the generated code calls `JuiceCall`, `JuiceEmit`, and `JuiceLog` and never touches memory pointers directly.

## 3. Script package additions

`kernel.SourceCompiler` is a new interface, separate from `ScriptExecutor`:

```go
type SourceCompiler interface {
    CompileSource(ctx context.Context, source []byte) (artifact []byte, hash string, err error)
}
```

The implementation in `script` invokes `tinygo build -target wasm` in a subprocess. The subprocess runs in a fresh temporary directory with no network access. The compilation timeout is `cfg.CompileTimeoutMS` (default 30 000 ms), independent of `cfg.TimeoutMS`. These are separate because compilation dominates execution time for generated actions and must not be capped by the per-execution limit.

`CompileSource` returns `ErrInvalidInput` on compilation errors (the compiler's stderr is included in the error message) and `ErrInvalidState` when the `tinygo` binary is absent from PATH.

`Kernel` gains a `compiler SourceCompiler` field set at construction. When nil, `@sys/make` returns `ErrInvalidState`.

A free function `ListImports` enumerates the `(module, name)` import pairs from a WASM binary without executing it, using wazero's module decoder:

```go
func ListImports(artifact []byte) ([]ImportedFunc, error)

type ImportedFunc struct{ Module, Name string }
```

## 4. Context document

The context document is a UTF-8 plain-text string constructed at each LLM call rather than cached, so it reflects the action catalog at synthesis time. It contains, in order:

1. The complete text of `sdk.go`.
2. A fixed prose section describing the `@owner/name` call convention, the JSON I/O contract over linear memory, and what `juice.call`, `juice.emit`, and `juice.log` do semantically (one sentence each).
3. The active action catalog: for each active public action, a block of the form `@owner/name | description | input_schema | output_schema | price | uses | successes | latency_mean_ms`.
4. The synthesis request: the task description; the caller-supplied `input_schema` and `output_schema` if provided; the `sync` preference.

## 5. MakeSpec

The first LLM call produces a `MakeSpec`, a structured JSON object that describes the action before code exists. Validating the spec before code generation is a deliberate fail-fast: structural problems (invalid schemas, missing dependencies) are caught without spending compilation time, and the caller receives a precise error immediately.

The pipeline extracts the first JSON code block from the LLM response and attempts to unmarshal it:

```json
{
  "name":           "string — action name, may use / as path separator, no @",
  "description":    "string — non-empty",
  "input_schema":   { "type": "object", "...": "..." },
  "output_schema":  { "type": "object", "...": "..." },
  "sync":           true,
  "dependencies":   ["@owner/name"],
  "price":          0,
  "failure_modes":  ["string"]
}
```

Validation rules:

- `name` is non-empty and contains no `@`.
- `description` is non-empty.
- `input_schema` passes `kernel.ValidateSchema`.
- `output_schema` passes `kernel.ValidateSchema`.
- Each entry in `dependencies` resolves to an existing active action via `store.ReadActionByOwnerName`.
- `price` is non-negative.

Validation failure returns `ErrInvalidInput`. No retry is attempted at this stage.

## 6. The @sys/make native action

`@sys/make` is registered at bootstrap alongside `@sys/lookup` and `@sys/llm/chat`. It is public and grant-all. Its price is set via config key `make_price` (env: `JUICE_MAKE_PRICE`; default 0).

Staging the pipeline across multiple native actions would require the caller to orchestrate them and would create partial-state cleanup problems (a stage-6 failure has already consumed LLM calls). `@sys/make` runs all stages internally; the retry at stage 4 is the only recovery path needed because compilation is the only stage with a recoverable transient failure mode.

**Input schema:**

| Field | Type | Required | Description |
|---|---|---|---|
| `task` | string | yes | Natural-language description of the desired action |
| `input_schema` | object | no | Desired input JSON Schema |
| `output_schema` | object | no | Desired output JSON Schema |
| `sync` | boolean | no | True for synchronous execution; default true |

**Output schema:**

| Field | Type | Description |
|---|---|---|
| `action_id` | string | ID of the newly registered action |
| `action_ref` | string | `@owner/name` form for use in subsequent calls |
| `dry_run_tx_id` | string | Transaction ID of the first successful test call |
| `spec` | object | The `MakeSpec` produced at stage 1 |

### 6.1 Subject ID threading

`Call()` sets the subject ID in the context before dispatching to `executeNative`, using an unexported context key:

```go
ctx = withMakeSubject(ctx, req.SubjectID)
```

`executeMake` retrieves it with `makeSubjectFromCtx(ctx)`. This avoids changing the signature of `executeNative` or the other dispatch cases.

### 6.2 Pipeline stages

**Stage 1 — Spec generation**

Build the context document. Call `k.chatter.Chat()` with a system prompt instructing the model to output a `MakeSpec` JSON block and nothing else. Parse and validate per §5. On failure, return `ErrInvalidInput` immediately.

**Stage 2 — Composition plan**

For each action in `spec.dependencies`, call `k.Lookup()` with the action's description as a query. If the best result has `score < JUICE_MAKE_LOOKUP_THRESHOLD` (default 0.7), mark the dependency as "inline". The plan — a list of actions to call via `juice.call` and subtasks to implement directly — is a Go-level data structure, not produced by the LLM. Delegating this decision to the LLM would risk the model inventing dependencies or ignoring existing actions; keeping it in Go makes the selection rule explicit and testable.

**Stage 3 — Code generation**

Build the context document, appending the `MakeSpec` and the composition plan. Call `k.chatter.Chat()` instructing the model to produce a single TinyGo file that imports the SDK and exports `run`. Extract the first Go code block from the response. Prepend `sdk.go` to produce the full source.

**Stage 4 — Compilation**

Call `k.compiler.CompileSource()`. On failure, retry once: append the compiler error to the code-generation context, repeat stage 3, repeat compilation. A second pass with the compiler's error message recovers the common failure (wrong import path, missing export). A third attempt rarely succeeds without a structural change to the spec; at that point the task description is the problem, not the code generator. If the second attempt also fails, return `ErrExecutionFailed` with the last compiler error.

**Stage 5 — Static checks**

Call `script.ListImports()`. The check passes iff:

- `imports(artifact) ⊆ {(juice, call), (juice, emit), (juice, log)}`
- `exports(artifact) ⊇ {alloc, run}`
- Each `@owner/name` in `spec.dependencies` not flagged "inline" appears as a literal string in the extracted source.

Any violation returns `ErrInvalidInput`.

If `spec.sync = true` and the artifact imports `juice.emit`, log a warning but do not reject.

**Stage 6 — Registration**

Call `k.CreateAction()`:

```
OwnerUserID  = callerID
Name         = spec.name
Kind         = KindWasm
Description  = spec.description
InputSchema  = spec.input_schema
OutputSchema = spec.output_schema
Source       = string(compiledWASMBytes)
Price        = spec.price
Active       = false
```

`ArtifactHash` is set by `CreateAction` via `k.scripts.Compile()` on the WASM bytes. The TinyGo source is stored as `StatTag{ActionID: actionID, Key: "make_source", Value: tinyGoSource, Source: "make"}`. Storing the source in `StatTag` avoids adding a column to the `actions` table; the tradeoff is that `StatTag` is semantically for statistics. A future migration to a dedicated column is straightforward when the feature stabilises.

**Stage 7 — Test generation and dry run**

Call `k.chatter.Chat()` with the spec and a system prompt requesting a JSON array of test cases, each `{args: object, expect_success: bool}`. The minimum acceptable set is two cases: one schema-valid expected-success case and one schema-invalid expected-failure case. If the LLM returns fewer than two cases, the pipeline constructs the missing ones: an empty `{}` args object serves as the schema-invalid case when the input schema requires fields.

Precondition: verify `caller.available >= spec.price × len(test_cases)`. The test process is funded from `caller.available` rather than from a `@sys` balance because the caller initiates the work and bears the cost; this avoids requiring an operator deposit to `@sys` before synthesis can run. If the precondition fails, return `ErrInsufficientFunds` before creating any process.

Activate the action: `k.SetActive(ctx, sysUserID, actionID, true)`.

Start a test process: `k.StartProcess(ctx, callerID, callerID, spec.price × len(test_cases))`. For each test case, call `k.Call()` using `callerID` as subject, the test process, and the synthesized action. For expected-failure schema-invalid cases, verify the error is `ErrSchemaViolation`. For expected-success cases, verify the call succeeds and the reply satisfies `spec.output_schema`.

Close the test process with `k.EndProcess()` regardless of outcome; unused credits return to the caller.

If any test case produces an unexpected outcome: `k.SetActive(ctx, sysUserID, actionID, false)`, return `ErrExecutionFailed` with the failing case.

If all tests pass: the action remains active. Return `action_id`, `action_ref`, the transaction ID of the first successful call, and the `MakeSpec`.

## 7. Bootstrap changes

Bootstrap registers `@sys/make` only when `JUICE_MAKE_ENABLED=true` (default false). The default is false because `tinygo` is an optional runtime dependency; a deployment without it must not fail bootstrap. If enabled but `tinygo` is absent, bootstrap logs a warning and skips registration without aborting. Registration is idempotent.

The `Kernel` constructor gains an optional `compiler SourceCompiler` parameter. When nil and `@sys/make` is called, it returns `ErrInvalidState`.

## 8. Required automated tests

```
SDK compiles to WASM with correct exports {alloc, run} and no disallowed imports
CompileSource returns ErrInvalidInput on TinyGo syntax error
CompileSource returns ErrInvalidState when tinygo binary is absent
ListImports returns correct (module, name) pairs for a known artifact
Static check rejects artifact with imports outside {juice.call, juice.emit, juice.log}
Static check rejects artifact missing run export
Static check rejects artifact missing alloc export
MakeSpec validation rejects empty name
MakeSpec validation rejects invalid input_schema
MakeSpec validation rejects unknown dependency
MakeSpec validation rejects negative price
Stage 1 produces valid MakeSpec from FakeChatter returning a known JSON block
Stage 3 produces compilable source from FakeChatter returning a known SDK-conforming template
Stage 4 retries once on compilation failure, succeeds on second attempt
Stage 4 returns ErrExecutionFailed after two consecutive compilation failures
Stage 5 rejects artifact that imports env.malloc
Stage 6 creates an inactive Action with correct fields and stores TinyGo source as StatTag
Stage 7 precondition fails with ErrInsufficientFunds when caller lacks test budget
Stage 7 dry run activates action, runs tests, leaves action active on success
Stage 7 deactivates action and returns ErrExecutionFailed when a test case fails
@sys/make is callable through Call() with a funded process (fake chatter + fake compiler)
@sys/make returns ErrInvalidState when compiler is nil
Bootstrap registers @sys/make when enabled; idempotent on re-run
Synthesized action callable by its owner immediately after successful synthesis
```
