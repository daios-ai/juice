# @sys/make — WASM Action Synthesis

Version: 0.3
Status: implementation reference (matches `native/make.go`)

> This document describes the **actual** implementation of `@sys/make` as it
> stands in `native/make.go`, the reasoning behind the design, and the hard-won
> lessons about Ollama/TinyGo behaviour that shaped it. The authoritative
> behavioural contract is `requirements.md` §9; where this document adds detail
> it never contradicts it.

---

## 1. What `@sys/make` is

`@sys/make` is a native kernel action that turns a natural-language description
into a **registered, active, callable WASM action** owned by the calling user.

```
@sys/make({"description": "A calculator that evaluates expressions like (3+4)*5"})
        │
        ▼
  { "status": "success",
    "action_id": "…", "action_name": "calculator",
    "diagnostics": [...], "tests": [ {compile:passed}, {example-1:passed}, … ] }
```

It is owned by `@sys`, registered at bootstrap (`price = 20`, `active`, `public`),
and invoked through the ordinary `kernel.Call` path like any other action. It is
**fully encapsulated** — the kernel knows nothing about synthesis; everything
lives in `native/make.go` and uses only public kernel entry points (`Call`,
`CreateAction`, `SetActive`, `ReadCallableAction`).

It never hard-errors for a *synthesis* failure. A failed synthesis returns
`{"status":"failure", "diagnostics":[…]}` as a normal result. Kernel-level errors
(`ErrInvalidInput`, `ErrInvalidState`) are reserved for **preconditions**: empty
description, missing compiler, missing LLM.

---

## 2. The core idea: plan → reuse → write → prove

A naive synthesizer just asks an LLM "write me Go code for X." That fails for two
reasons:

1. **It ignores the platform.** Juice is a marketplace of priced, callable
   actions. If a calculator already exists, a "natural-language calculator"
   should *call it*, not re-derive arithmetic. The synthesizer must look at the
   catalog.
2. **It can't tell whether it succeeded.** LLM-generated code compiles and looks
   plausible but may be wrong. We need an objective gate.

So `@sys/make` is built as a **four-phase pipeline wrapped in a repair loop**:

```
        ┌──────────────────────────────────────────────────────────┐
        │  for step in 1..maxSteps (default 5):                     │
        │                                                           │
        │   PHASE 1  Plan & Research                                │
        │     derive contract { name, schemas, constraints, plan }  │
        │     for each capability in plan:                          │
        │         @sys/lookup(capability)  → candidate actions      │
        │         @sys/llm/decide(...)     → pick one (or none)     │
        │     → "composable surface"                                │
        │                                                           │
        │   PHASE 2  Code                                           │
        │     @sys/llm/chat(SDK + contract + surface + diagnostics) │
        │     → TinyGo source                                       │
        │                                                           │
        │   PHASE 3  Compile                                        │
        │     tinygo build → WASM                                   │
        │     validate imports/exports  (disallowed import = fatal) │
        │                                                           │
        │   PHASE 4  Evaluate                                       │
        │     generate 3 example inputs, run each against the WASM  │
        │     all outputs must be valid + satisfy output_schema     │
        │                                                           │
        │   all passed?  → register + activate → return SUCCESS     │
        │   else         → append diagnostics, loop                 │
        └──────────────────────────────────────────────────────────┘
        exhausted → return FAILURE(diagnostics)
```

### Why all four phases are inside the loop

This is the single most important structural decision, and it was a **mistake we
corrected**. The first cut ran Plan & Research *once*, then looped only
Code→Compile→Evaluate. That is wrong: when codegen fails, the fault is often in
the **plan or the contract**, not just the code. A schema that's awkward to
satisfy, a plan capability that sent research down the wrong path, a constraint
that wasn't extracted — re-running codegen against a frozen plan can never fix
those.

By putting **all four phases in the loop** and feeding the accumulated
`diagnostics` back into `deriveContract`, a failed attempt can revise the
contract, schemas, constraints, plan, *and* the composable surface — not just
re-roll the dice on code. The plan is a hypothesis; each loop iteration is
allowed to revise the hypothesis, not only the implementation.

---

## 3. Phase 1 — Plan & Research (`planAndResearch`)

### 3.1 Contract derivation (`deriveContract`)

One **`@sys/llm/chat`** call asks the model to return a JSON object:

```json
{
  "name": "short-kebab-slug (≤3 words)",
  "description": "Clean one-line description",
  "input_schema":  { "type": "object", "properties": {…}, "required": [...] },
  "output_schema": { "type": "object", "properties": {…}, "required": [...] },
  "constraints": ["no LLM", "only +-*/"],
  "plan": ["parse the arithmetic expression", "evaluate it", "return the number"]
}
```

Two fields here are the heart of the design:

- **`constraints[]`** — restrictions extracted from the *user's phrasing*. This is
  the fix for a real bug: a request "Small calculator… **No LLM please!**" used to
  produce a calculator that delegated arithmetic to `@sys/llm/chat`. The
  constraint was being thrown away. Now it is a first-class field that survives
  into research (filtering) and codegen (an explicit "MUST honor" block).

- **`plan[]`** — a free-form list of capability sentences. Deliberately **not** a
  rigid step1→step2→step3 template. It's "what pieces does this need?", and each
  piece is independently a candidate for reuse-or-write.

On a repair iteration, prior diagnostics are appended to the derivation prompt
("Prior attempts failed with these errors — revise the plan/schemas/constraints
to avoid them"), so the contract itself can change between attempts.

This is the one make-internal call that does **not** use schema-constrained
`@sys/llm/json`, and that's deliberate. The contract's `input_schema`/`output_schema`
are themselves free-form JSON Schema; constraining their generation with Ollama's
`format` makes the model stall and truncate on the nested structure (the §8.1
large-context JSON-mode failure — we hit it on the translator and reverted). So here we
parse defensively: `extractJSON` pulls the first balanced `{…}` span and we `Unmarshal`.
If the reply has no JSON, or is missing `name`/`input_schema`/`output_schema`, that's a
phase-1 diagnostic and the loop continues. Schema-constrained decoding is reserved for
example generation (§6.3), where the target shape is a concrete, already-derived schema.

The model-produced schemas can be malformed (a local model emits `"type":"::_string"`
or a non-string type). `sanitizeSchemaForRegistration` is hardened to drop any
unsupported `type` — making that node unconstrained — so a bad schema can't crash
registration or the schema-constrained example step downstream.

### 3.2 Per-capability research (`researchCapability`)

For each capability in `plan[]`:

1. **`@sys/lookup({query: capability})`** → ranked existing actions
   (`owner_handle`, `name`, `description`, `price`).
2. **Filter** the candidates against constraints (`filterCandidates`): if any
   constraint is an LLM prohibition (`hasLLMConstraint` matches "llm", "no ai",
   "without ai"), drop every candidate whose ref contains "llm". This is what
   makes "No LLM please!" actually exclude `@sys/llm/*` from the menu.
3. If candidates remain, **`@sys/llm/decide`** picks the single best-matching
   action for that capability.
4. For the chosen action, fetch its **required input field names** via
   `ReadCallableAction` and attach them to the surface entry.

**Reuse vs. from-scratch is decided here, per capability** — exactly the user's
model: "Plan returns step1→step2→step3, but these steps might already exist as
actions, so we use lookup and decide (potentially deciding to implement from
scratch if the function is simple)."

Everything in research is **non-fatal**. Lookup error, empty results, decide
error, or a decide pick that doesn't match any candidate → that capability simply
contributes nothing to the surface and will be implemented inline by codegen. A
missing embedder (so lookup returns nothing) is the normal "no platform actions
available" case, and synthesis proceeds to write everything from scratch.

### 3.3 The composable surface (`buildSurface`)

The selected actions become a text block handed to codegen:

```
Available platform actions to compose via JuiceCall:
  @alice/calculator — Evaluates arithmetic expressions (price 3) | required inputs: expression
```

The **`| required inputs:`** suffix matters: without it the generated composer
guesses field names (`{"expr": …}` vs `{"expression": …}`), the sub-call gets an
empty/zeroed reply, and the composer silently returns `0`. Surfacing the real
required keys is what lets a composer call an existing action correctly.

---

## 4. Phase 2 — Code (`generateCode` / `buildCodeGenMessages`)

One `@sys/llm/chat` call with a **system** message and a **user** message.

The system message is large and deliberately prescriptive. It contains, in order:

1. The contract (name + pretty-printed input/output schemas).
2. A **"Constraints (MUST honor)"** block, if any.
3. The **composable surface**, if any.
4. The **full TinyGo SDK source**, fenced, with "DO NOT redeclare."
5. **Strict code rules** (see below).
6. **Required patterns** — exact code for reading input, returning a packed
   pointer, and making sub-calls.
7. **Calling conventions** for `@sys/llm/json`, `@sys/llm/chat`, and
   `@sys/llm/decide` (Juice's response shapes, *not* OpenAI's). The guidance is
   explicit: for **structured** results (a number, record, list, enum) call
   `@sys/llm/json` with an `output_schema` and read the guaranteed-conformant
   `value`; use `@sys/llm/chat` **only** for free-form text. This replaced an
   earlier calculator-specific "ask for a bare number" hack — see §8.3.
8. A **minimal working echo action** as a worked example.

The user message is just "Implement the action described above," plus any prior
diagnostics ("Fix these errors from the previous attempt: …").

We extract the **first ```` ```go ```` fenced block** from the reply
(`extractGoBlock`), strip any stray `package main`, and prepend the SDK
(`prepareSource`). No fenced block → phase-2 diagnostic, loop.

### 4.1 Why the system prompt is so heavy

Every rule and pattern in that prompt is scar tissue from a specific failure
class observed with the local Ollama model. They are documented in §8. The prompt
is the cheapest place to prevent a compile error: a single sentence ("ONLY
encoding/json and unsafe are in scope") eliminates an entire category of retries.

---

## 5. Phase 3 — Compile (`CompileSource` + `checkWASMImports`)

The TinyGo compiler (`tinygo build -target wasip1`, behind
`kernel.SourceCompiler`) turns the source into a WASM artifact and returns its
hash. Compile error → diagnostic, loop.

Then `checkWASMImports` inspects the module (when the executor implements
`kernel.WASMInspector`):

- **Allowed imports:** `juice.call`, `juice.step_create`, `juice.step_complete`,
  `juice.log`, and anything from `wasi_snapshot_preview1` (TinyGo's runtime).
- **Required exports:** `alloc` and `run`.

A disallowed import or a missing export is a **terminal failure** — we do *not*
loop, because it means the model reached for a host capability that doesn't exist
and retrying won't conjure it. Everything else in the loop is retryable; this is
the one in-loop hard stop besides registration.

---

## 6. Phase 4 — Evaluate (`generateAndRunExamples`)

This is the objective gate. Compilation success is necessary but **not
sufficient** — plenty of compiling code is wrong.

1. Compile the WASM into an executable artifact via the script executor.
2. Ask **`@sys/llm/json`** (schema-constrained, §6.3) for an object holding an
   `examples` array whose items conform to the action's sanitized `input_schema`.
   The schema does the work: every example is a structurally valid input, with no
   prose-stripping or "did it return 3?" guesswork.
3. For each example, `Execute` the WASM with a **stub host** (`makeTestHost`,
   which returns `{}` for every sub-call) and check the output is valid JSON whose
   top-level keys include every required field of `output_schema`.

**All three must pass** for the action to register.

### 6.1 The gate must fail loudly — a bug we fixed

The original `generateAndRunExamples` returned `[{compile: passed}]` and bailed
*silently* whenever example generation errored or returned junk. The caller saw
"no failed tests" and registered the action having proven **nothing**. Now every
failure path — can't generate, empty reply, unparseable JSON, fewer than 3
examples, execution error, invalid output, missing field — appends an explicit
**failed** `MakeTest`. No silent pass is possible.

### 6.2 What the stub host can and cannot catch

Because sub-calls return `{}` during smoke testing, the gate validates **shape,
not semantics of composition**. A composer that calls `@alice/calculator` will,
under the stub, get `{}` back and produce a structurally-valid-but-numerically-
zero output — which *passes* the schema check. So the gate guarantees:

- the WASM runs without trapping,
- it produces schema-valid output for 3 distinct inputs,

but it **cannot** guarantee that a multi-hop LLM/sub-call chain yields the right
*value* at real runtime. That residual non-determinism is a known limitation
(§8.5), not a gap we can close with a stub host.

### 6.3 Schema-constrained decoding via `@sys/llm/json`

Example generation goes through **`@sys/llm/json`**, not plain `@sys/llm/chat`.
(Contract derivation does *not* — see §3.1 for why constraining free-form schemas
backfires.) The distinction is the rule of thumb: **constrain when the target shape
is concrete and known; don't when the content is itself an arbitrary schema.** For
examples the shape is the action's already-derived `input_schema`, so `@sys/llm/json`
gives us schema-constrained decoding end to end:

```
@sys/llm/json(messages, output_schema)
   → JSONChatter.ChatJSON(messages, schema)
   → Ollama POST /api/chat  with  "format": <schema>
   → the model is constrained AT DECODE TIME to emit conforming JSON
   → local ValidateInput(schema, value)        ← backstop if it still drifts
   → { "value": <guaranteed-schema-conformant JSON> }
```

This is strictly better than asking for JSON in a prompt and then hunting for the
first `{…}` in prose (which is what we used to do, and what the "bare number" hack
in §8.3 was patching around). Two constraints shape how we use it:

- **`value` is always an object.** `@sys/llm/json`'s own output schema constrains
  its `value` to `type: object`, so a top-level array is rejected. Example
  generation therefore wraps the list in an envelope — `{ "examples": [ … ] }` —
  and the codegen guidance tells generated actions to make their `output_schema`
  an object too (wrap scalars, e.g. `{ "result": <number> }`).
- **Keep the call small.** Constrained decoding stalls on very large contexts
  (§8.1). These calls are tiny and focused, so it's safe here; the big
  accumulated-context call (`@sys/llm/decide`) deliberately avoids JSON mode.

The same mechanism is offered to **generated actions**: the codegen prompt teaches
them to call `@sys/llm/json` with an `output_schema` whenever they need structured
data at runtime, and to reserve `@sys/llm/chat` for free-form text.

---

## 7. Termination, pricing, registration

- **Success:** `CreateAction` under `callerID` (the user who called make), then
  `SetActive`. Price is computed by `computePrice`, which regex-scans the source
  for `JuiceCall("@owner/name")` and sums the referenced actions' prices — the
  subtree-cost lower bound. Returns `status:"success"` with `action_id`,
  `action_name`, diagnostics, and the passing tests.
- **Name collision:** `CreateAction` fails because the caller already owns that
  name → **`status:"failure"`, single attempt, no retry.** We never mangle the
  name to `calculator-2`. (This was an earlier spec violation; it is now correct.)
- **Import/export violation:** terminal failure (see §5).
- **Exhaustion:** after `maxSteps` attempts without an all-pass, return
  `status:"failure"` with the accumulated diagnostics.

`maxSteps` defaults to **5** (both the in-code fallback and the config default).

### Sub-call authority

Every internal call (`@sys/llm/json`, `@sys/llm/chat`, `@sys/lookup`,
`@sys/llm/decide`) goes
through `callAction`, which routes via `kernel.Call` on make's own process/trace
(`CallerID = targetID = @sys`, `ParentTraceID = parentTraceID`). The caller's
process is debited only make's price (20); the sub-call LLM/lookup costs are borne
by the action owners per normal contractor rules. At **runtime**, when a
synthesized action does `juice.call`, ordinary contractor sub-call rules apply —
an ephemeral process owned and funded by the action owner.

---

## 8. Learnings — Ollama, TinyGo, and the local model

These are the behaviours that actually drove the implementation. They are written
down so the next person doesn't re-discover them the hard way.

### 8.1 Ollama tool-calling is unreliable; we route on plain structure instead

`@sys/llm/decide` (in `llm/llm.go`) tries native tool-calling first but **falls
back to plain-text routing** because the local model frequently returns *no*
`tool_calls` at all, or returns a function name that's been mangled. We learned:

- **Function names must be sanitized.** OpenAI/Ollama function-name grammars
  reject `@` and `/`. `sanitizeToolName` maps `@sys/llm/chat` → `sys__llm__chat`
  (drop `@`, `/`→`__`). We use **double** underscore precisely so an action name
  containing a single `_` (e.g. `@user/llm_chat` → `user__llm_chat`) can't
  collide with a `/`-derived separator.
- **The model partially re-sanitizes names.** It will answer `sys_llm/chat` or
  `sys-llm-chat`. The decoder re-sanitizes and retries the lookup before giving
  up, then falls through to substring matching on a single output line.
- **JSON-mode (`format`) stalls** on Ollama once the context is large (the SDK
  alone is multi-KB). The decide fallback therefore uses *plain* chat with a
  "output exactly one line" instruction, which is far more robust than asking for
  constrained JSON over a big prompt.

Net design consequence: **don't lean on Ollama's tool-calling for control flow.**
Keep the decision surface tiny (a handful of action refs) and parse defensively.
Note the asymmetry with §8.2: *schema-constrained data extraction* over a small
prompt is reliable; *tool-call routing* over a large one is not.

### 8.2 Prefer schema-constrained decoding over parsing prose

Plain chat models rarely return *only* the JSON you asked for — they prepend
"Here is the schema:", wrap things in ```` ```json ````, or append an explanation.
The robust fix is not better string-scraping; it's to **not ask for free-form
JSON in the first place**. Pass a strict JSON schema in Ollama's `format` field so
generation is constrained at decode time, and validate locally as a backstop.

In Juice this is exactly what `@sys/llm/json` does, and make routes example
generation through it instead of `@sys/llm/chat` + span-extraction (§6.3).
The one place we still parse prose is contract
derivation (`extractJSON`), because constraining free-form schema generation
stalls Ollama (§3.1, §8.1) — so the lesson is *use constraint where the shape is
concrete, parse defensively where it can't be*.

> Lesson, stated generally: when you need structured data from a local model,
> constrain the *grammar*, don't post-process the *prose*. Reserve free-form chat
> for genuinely free-form text (translations, summaries).

### 8.3 Sampling is a model/deployment concern — the adapter sets none

The adapter (`llm/llm.go`) deliberately sends **no** `options` (no temperature, no
seed). Sampling is model-specific: pinning `temperature: 0` platform-wide bakes a
one-model assumption into shared infra, and Juice swaps models freely. Whatever
sampling a deployment wants belongs with the model/server config, not the adapter.

A consequence to keep in mind: with default sampling, repeated identical calls are
**not** reproducible, so the make value-correctness flows (does the synthesized
calculator return 35?) are inherently flaky — which is exactly why they're isolated
behind `JUICE_MAKE_FLOWS=1` and kept out of the default suite. Reliability for those
comes from the *structure* (schema-constrained decoding, the repair loop, the
all-examples-pass gate), not from freezing the sampler.

This is also why the old **"bare number" hack** is gone. Asking a chat model "what is
(3+4)*5?" yields "The answer is 35.", not `35`; a generated calculator that did
`json.Unmarshal([]byte(content), &result)` silently got `0`. The previous doc
"fixed" this by telling the generated action to beg for a bare number in its own
prompt — a patch tailored to one output type of one example. The general fix is
§8.2 + §8.3: have the generated action call `@sys/llm/json` with an `output_schema`
(e.g. `{result: number}`) and read the guaranteed-conformant `value`. Numbers,
records, lists, enums — one mechanism, no per-shape prompt hacks.

### 8.4 The Juice response shape ≠ OpenAI

`@sys/llm/chat` returns `{"message": {"content": "..."}}`, **not**
`{"choices": [...]}`. Generated code trained on OpenAI muscle memory reaches for
`choices[0].message.content`, gets nothing, and emits empty output. The codegen
prompt now spells out the exact Juice shapes for both `@sys/llm/chat` and
`@sys/llm/decide` (including that `decide` **requires** both `messages` and
`actions` fields — omitting `actions` makes the runtime sub-call trap). Restoring
these two convention blocks is what turned the translator and NL-calculator flows
green.

### 8.5 The stub-host ceiling on multi-hop reliability

Smoke tests use a host that returns `{}` for sub-calls (§6.2). A single-hop action
(e.g. a from-scratch calculator) is fully exercised. A **multi-hop composer**
("parse English → call calculator") cannot be value-checked at synthesis time,
because the calculator returns `{}` under the stub. Such actions can pass
synthesis and still return a wrong value at real runtime depending on the model's
output that particular run. This is **inherent** to synthesizing against a live,
non-deterministic LLM — we surface it honestly rather than pretending the gate is
stronger than it is. Mitigations live in the codegen prompt (correct conventions,
bare-number rule, required input names), which raise the success rate but do not
make it 1.0.

### 8.6 TinyGo / WASM ABI constraints baked into the SDK and rules

- The SDK exports `alloc` (using TinyGo's heap allocator, never address 0) so the
  kernel can write input bytes into WASM memory; `run` returns a **packed i64**
  (`ptr<<32 | len`). Generated code must follow this exactly — hence the
  "Required patterns" block.
- Only `encoding/json` and `unsafe` are in scope. The model loves to reach for
  `fmt`, `strings`, `strconv`, or invent helpers like `fail()`/`success()`. Each
  such temptation gets an explicit "does NOT exist / NOT in scope" line, because
  every one is a guaranteed compile error and a wasted loop iteration.
- TinyGo's compiler is strict about unused variables; the prompt warns "every `:=`
  must be used," which is a common model slip.

### 8.7 Prompt-as-config

The cheapest, fastest place to fix a recurring synthesis failure is the **system
prompt**, not the Go harness. Almost every fix in §8 is a few lines of prompt
text. The harness stays small and stable; the prompt absorbs the model's quirks.
This keeps `native/make.go` reviewable and the failure modes documented in one
place.

---

## 9. Failure taxonomy (quick reference)

| Situation                                   | Result                              | Retried? |
| ------------------------------------------- | ----------------------------------- | -------- |
| Empty/missing `description`                 | `ErrInvalidInput`                   | —        |
| No compiler configured                      | `ErrInvalidState`                   | —        |
| No LLM (`Chatter`) configured               | `ErrInvalidState`                   | —        |
| Contract derivation returns junk            | phase-1 diagnostic                  | yes      |
| Lookup/decide error for a capability        | capability → from-scratch           | n/a (non-fatal) |
| No ```` ```go ```` block from codegen       | phase-2 diagnostic                  | yes      |
| TinyGo compile error                        | phase-3 diagnostic                  | yes      |
| Disallowed WASM import / missing export     | `status:"failure"`                  | **no (terminal)** |
| <3 examples / exec error / bad output       | failed `MakeTest`(s)                | yes      |
| Name collision at registration             | `status:"failure"`                  | **no (terminal)** |
| `maxSteps` reached without all-pass         | `status:"failure"` + diagnostics    | —        |

---

## 10. Where the code lives

Everything is in **`native/make.go`** (handler + pipeline) with its test
counterpart **`native/make_test.go`**. Supporting pieces:

- `native/lookup.go` — `@sys/lookup` (catalog search; returns `owner_handle`,
  `name`, `description`, `price`, `score`).
- `native/json.go` — `@sys/llm/json` (schema-constrained structured output;
  `messages` + `output_schema` → validated `value`).
- `llm/llm.go` — Ollama adapters: `Chat`, `ChatJSON`, `ChatDecide`
  (tool-calling + plain-text fallback), `sanitizeToolName`. Sends no sampling
  options — temperature/seed are left to the model/server.
- `script/sdk.tmpl` (embedded as `script.TinyGoSDK`) — the SDK prepended to every
  generated source.
- `script/compile.go` — `SourceCompiler` (real TinyGo) and `FakeCompiler`
  (returns a valid echo WASM for tests without invoking TinyGo).
- `cmd/juice/config.go` — `NativeMakeConfig{ Compiler:"tinygo", MaxSteps:5,
  Price:20 }`.

The authoritative behaviour contract remains **`requirements.md` §9**.
