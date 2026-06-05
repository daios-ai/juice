# @sys/make Requirement Specification

Version: 0.1
Status: implementation requirements

## 1. Purpose

`@sys/make` is a native kernel action that synthesizes a new WASM action from a natural-language description. It uses the platform LLM and the action catalog to design, generate, compile, and smoke-test a TinyGo WASM artifact, returning a draft ready for registration.

`@sys/make` executes through the normal `Call()` path. It is owned by `@sys`, registered at bootstrap, and callable by any authenticated user.

## 2. Registration

`@sys/make` is a native action registered programmatically at bootstrap via `RegisterNativeAction`. It must not be created through the normal `CreateAction` path.

| Field          | Value                                                          |
| -------------- | -------------------------------------------------------------- |
| `owner`        | `@sys`                                                         |
| `name`         | `make`                                                         |
| `kind`         | `native`                                                       |
| `price`        | 20 credits                                                     |
| `active`       | true (activated at bootstrap)                                  |
| `public`       | true (grant-all applied at bootstrap)                          |
| `description`  | `Generate a WASM action from a natural-language description.`  |

Bootstrap is idempotent: if the action already exists, it is activated and grant-all is applied without recreating it.

## 3. Input and output schemas

### 3.1 Input

```json
{
  "type": "object",
  "properties": {
    "description": {
      "type": "string",
      "description": "Natural-language description of the action to generate"
    }
  },
  "required": ["description"]
}
```

`description` must be a non-empty string. An empty or missing value returns `ErrInvalidInput`.

### 3.2 Output

```json
{
  "type": "object",
  "properties": {
    "status":      { "type": "string",  "description": "\"success\" or \"failure\"" },
    "draft":       { "type": "object",  "description": "Generated action draft; present only on success" },
    "diagnostics": { "type": "array",   "items": { "type": "string" }, "description": "Compilation and test feedback from each iteration" },
    "tests":       { "type": "array",   "description": "Test results from the final dry-run; present only on success" }
  },
  "required": ["status", "diagnostics"]
}
```

The `draft` object contains:

| Field          | Type   | Description                                  |
| -------------- | ------ | -------------------------------------------- |
| `name`         | string | Kebab-case action name (≤ 3 words)           |
| `kind`         | string | Always `"wasm"`                              |
| `description`  | string | Natural-language description from input      |
| `input_schema` | object | JSON Schema derived for the action's input   |
| `output_schema`| object | JSON Schema derived for the action's output  |
| `source`       | string | Complete TinyGo source code                  |
| `artifact_hash`| string | SHA-256 hex hash of the compiled WASM binary |

`@sys/make` returns `status: "success"` or `status: "failure"` — it does not return a kernel-level error for synthesis failures. Kernel-level errors (`ErrInvalidInput`, `ErrInvalidState`, etc.) are reserved for precondition failures (bad input, missing LLM, missing compiler).

## 4. Synthesis pipeline

`@sys/make` runs a **repair loop** of up to `maxSteps` iterations (default 5). Each iteration attempts to generate, compile, validate, and test a WASM artifact. Diagnostics from each failed iteration are fed back into the next generation attempt.

### 4.1 Step ordering

```
1. Validate input (description non-empty)
2. Derive contract and plan via LLM
3. Resolve explicit action references from description
4. Search catalog for composable actions
5. Merge and deduplicate composable action list
6. Generate TinyGo source via LLM            ← repair loop re-enters here
7. Compile source to WASM
8. Validate WASM imports and exports
9. Generate and run smoke tests
10. Return success or loop with diagnostics
```

Steps 2–5 run once. Steps 6–10 repeat up to `maxSteps` times.

### 4.2 Contract derivation (step 2)

Call `@sys/llm/chat` with a prompt that asks the LLM to produce:

- `name`: a kebab-case slug of at most 3 words
- `input_schema`: a valid JSON Schema for the action's input
- `output_schema`: a valid JSON Schema for the action's output
- `plan`: a free-text description of the capabilities needed, expressed as a sequence of capability sentences

This call routes through the normal `Call()` path using the subject's process.

### 4.3 Catalog resolution (steps 3–5)

**Explicit references** — scan `description` for `@owner/name` patterns; look each up in the store directly.

**Catalog search** — split the LLM-produced plan into individual capability sentences (delimited by `;`, `,`, newlines, and conjunctions). For each sentence call `@sys/lookup` with `limit=5`. Deduplicate results across queries by `action_id`.

**Filter** — exclude actions that are:
- inactive, or
- unreliable: failure rate > 50% with at least 5 recorded uses.

Actions with no usage history are included.

**Merge** — combine explicit references and catalog results, deduplicating by `action_id`. The merged list is passed to the LLM as the available composable surface.

### 4.4 Source generation (step 6)

Call `@sys/llm/chat` with a prompt that includes:

- The full TinyGo SDK (host function bindings, `JuiceCall`, `JuiceEmit`, `JuiceLog`, `alloc`)
- The derived contract (name, input schema, output schema as pretty-printed JSON)
- The composable actions list in `@owner/name — description` form
- The original `description`
- Any diagnostics accumulated from previous iterations
- Strict generation rules: forbidden identifier names, required export signature, JSON I/O marshaling conventions

Extract the first fenced Go code block from the LLM response. If no block is found, record a diagnostic and loop.

### 4.5 Compilation (step 7)

Compile the generated source to WASM using `tinygo build -target wasip1`. The compiler:

- Writes source to a temporary file
- Invokes the TinyGo CLI
- Returns the artifact bytes and `SHA-256(artifact)` as the `artifact_hash`
- Caches compiled artifacts by source hash

A compilation failure is a typed error (`ErrInvalidInput`); the diagnostic message is recorded and the loop continues.

### 4.6 Import and export validation (step 8)

Inspect the compiled WASM module. The validation rules are:

**Allowed imports:**

| Module                    | Function    | Reason                          |
| ------------------------- | ----------- | ------------------------------- |
| `juice`                   | `call`      | Contractor sub-calls            |
| `juice`                   | `emit`      | Event emission                  |
| `juice`                   | `log`       | Structured logging              |
| `wasi_snapshot_preview1`  | (any)       | TinyGo WASI runtime requirement |

Any import outside this set is a fatal validation error; the synthesis fails immediately without looping.

**Required exports:** `alloc` and `run`. Missing either is a fatal error.

### 4.7 Smoke testing (step 9)

Call `@sys/llm/chat` to generate two realistic example inputs that conform to `input_schema`. For each example:

1. Execute the compiled WASM with a stub host that returns `{}` for all `juice.call` invocations.
2. Verify the output is valid JSON.
3. Verify the output satisfies `output_schema`.

Record each test as passed or failed. If any test fails, append the failure diagnostic and loop to step 6. If all tests pass, proceed to step 10.

### 4.8 Termination

On success, return:

```json
{ "status": "success", "draft": { ... }, "diagnostics": [...], "tests": [...] }
```

After `maxSteps` failed iterations, return:

```json
{ "status": "failure", "diagnostics": [...] }
```

## 5. TinyGo SDK

The SDK is embedded in the `script` package and passed to the make handler at registration time. It is prepended verbatim to every generated source file. The SDK provides:

- `JuiceCall(action string, args []byte) ([]byte, error)` — contractor sub-call
- `JuiceEmit(event string, args []byte) error` — event emission
- `JuiceLog(msg string)` — structured log
- `alloc(size uint32) uint32` — memory allocator export (required by executor)
- Low-level host import bindings (`hostCall`, `hostEmit`, `hostLog`)

Generated source must not redeclare any SDK identifier. The SDK is not part of the action schema or the generated action's public interface.

## 6. Sub-call authority

When a WASM action generated by `@sys/make` calls `juice.call(action, args)` at runtime, the normal contractor sub-call rules apply (§5.5 of the kernel requirements):

- An ephemeral process is created, owned by the calling action's owner.
- The ephemeral process is funded from the action owner's `available` balance.
- The ephemeral root trace carries `caused_by_trace_id = calling trace id` (`FOLLOWS_FROM`).
- The ephemeral process is always closed after the sub-call completes.
- Insufficient owner funds fail the sub-call and propagate failure to the caller.

The caller's process is debited only by `@sys/make`'s price (20 credits). Sub-call costs are borne by the action owner, not the make caller.

## 7. Configuration

| Parameter    | Source                              | Default | Description                                |
| ------------ | ----------------------------------- | ------- | ------------------------------------------ |
| `maxSteps`   | Passed at `RegisterMakeHandler` time | 5      | Maximum repair loop iterations             |
| LLM endpoint | `llm` package configuration         | —       | Shared with `@sys/llm/chat`                |
| TinyGo SDK   | Embedded in `script` package        | —       | Passed at registration; not runtime-config |

`@sys/make` returns `ErrInvalidState` if the LLM (`@sys/llm/chat`) is unconfigured or the compiler is unavailable.

## 8. Required tests

```
make: rejects empty description with ErrInvalidInput
make: derives contract name, input schema, output schema, and plan from description
make: explicit @owner/name references in description are resolved and included
make: catalog search excludes unreliable actions (>50% failure rate, ≥5 uses)
make: catalog search includes actions with no usage history
make: generated source is compiled to WASM
make: import validation rejects forbidden host imports
make: import validation rejects missing alloc or run exports
make: smoke tests execute with stub host returning {}
make: failed smoke test appends diagnostic and retries
make: returns status=success with draft after passing smoke tests
make: returns status=failure after exhausting maxSteps without passing tests
make: bootstrap is idempotent (second call does not recreate the action)
make: @sys/make is callable through Call() with normal preconditions enforced
make: LLM unavailable returns ErrInvalidState
make: compiler unavailable returns ErrInvalidState
```
