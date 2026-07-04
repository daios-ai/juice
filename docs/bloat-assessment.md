# Juice Codebase Bloat Assessment

## Executive summary

The codebase is bloated in real implementation terms, not primarily because of comments. The largest sources of excess code are repeated implementation patterns around CLI commands, HTTP handlers, service-layer response enrichment, SQLite CRUD and scan plumbing, broad kernel wrapper methods, large scenario tests, and native action adapters.

The product contract itself calls for a small package/file count, thin CLI and HTTP wiring, and server-side service logic. Therefore, the right simplification strategy is to route repeated behavior through fewer existing paths, not to delete required domain concepts or weaken requirements.

A realistic safe target is a reduction of roughly 3,000-6,000 lines of Go code, mostly by consolidating boilerplate and duplicated test setup while preserving behavior and coverage.

## Current size profile

A repository scan showed roughly 42,629 lines of Go code.

Largest production hotspots:

- `store/sqlite.go`: about 2,406 lines
- `kernel/kernel.go`: about 1,942 lines
- `cmd/juice/serve.go`: about 1,181 lines
- `cmd/juice/cmd.go`: about 938 lines
- `native/make.go`: about 961 lines
- `kernel/federation.go`: about 1,049 lines

Largest test hotspots:

- `store/sqlite_test.go`: about 2,673 lines
- `cmd/juice/flow_test.go`: about 2,564 lines
- `cmd/juice/serve_test.go`: about 2,343 lines
- `kernel/kernel_test.go`: about 2,350 lines
- `kernel/call_test.go`: about 1,792 lines
- `kernel/federation_test.go`: about 1,721 lines

The size alone is not automatically a defect, because the requirements include substantial behavior. However, the concentration of many top-level functions in a few files strongly suggests mechanical duplication, especially in persistence, command construction, endpoint handling, and tests.

## Product-contract constraints

Any simplification must preserve the requirements contract:

- The package set should remain small.
- The kernel must not depend on concrete CLI, HTTP, SQLite, wazero, Ollama, or libp2p implementations.
- CLI handlers and HTTP handlers should remain thin wires.
- Kernel calls, enrichment, validation, and transformation should live server-side in the service layer.
- Monetary transitions must remain compound atomic store operations.
- The required automated test surface must remain intact.

The goal is not to remove essential concepts such as users, actions, processes, traces, transactions, receipts, steps, ratings, idempotency records, or remote-proxy settlement. Those are product requirements. The goal is to remove duplicated mechanics around them.

## Major simplification opportunities

### 1. Consolidate SQLite CRUD, scan, list, and transaction plumbing

`store/sqlite.go` is the largest production file and is likely the highest-return target. The store interface is necessarily broad, but the SQLite implementation can route repeated SQL mechanics through fewer unexported helpers.

Recommended helpers inside `store/sqlite.go`:

- `withTx(ctx, fn)` for transaction lifecycle
- `queryOne(ctx, query, args, scan)` for single-row reads
- `queryMany(ctx, query, args, scan)` for list reads
- `exec(ctx, query, args...)` for simple statements
- shared scanners such as `scanUser`, `scanAction`, `scanProcess`, `scanTrace`, `scanTransaction`, `scanStep`, and `scanReceipt`
- shared pagination helper for limit/offset normalization

This should begin with read-only methods and supervision CRUD. Compound money-path methods should be left alone until later, because they encode required atomic write sets.

Expected reduction: 500-900 production lines, plus 300-600 test lines if store test fixtures are consolidated.

Risk: low for read-only methods; medium for monetary paths. Start with read-only code.

### 2. Collapse CLI boilerplate with a small endpoint-command constructor

`cmd/juice/cmd.go` repeats the same pattern across many commands:

1. define Cobra metadata
2. parse flags
3. resolve a user/action/process/transaction identifier
4. build a JSON request body
5. call `apiCall` or `apiEmit`
6. print JSON or human output

The CLI is supposed to be a thin HTTP client. It is thin semantically, but structurally verbose.

Recommended direction:

- Add one small unexported command-constructor helper for simple endpoint-backed commands.
- Keep complex commands hand-written.
- Use the helper for simple commands such as show/list/delete/enable/disable/rate/verify where appropriate.

Example shape:

```go
type endpointCmd struct {
    use    string
    short  string
    args   cobra.PositionalArgs
    method string
    path   func(args []string) (string, error)
    body   func(cmd *cobra.Command, args []string) (any, error)
    after  func(args []string, raw json.RawMessage) error
}
```

This should not become a framework. It should only remove obvious repeated endpoint-call wrappers.

Expected reduction: 250-500 production lines.

Risk: medium-low.

### 3. Consolidate HTTP handler decode, caller, error, and response boilerplate

`cmd/juice/serve.go` contains many endpoint handlers with likely repeated structure:

- read caller from context
- parse path/query parameters
- decode a JSON body
- validate missing body or required fields
- call service functions
- map errors to HTTP responses
- write JSON

Recommended direction:

- Add shared local helpers for decoding request bodies, writing responses, parsing pagination, parsing optional filters, and getting the authenticated caller.
- Convert low-risk JSON endpoints first.
- Do not touch auth middleware, rate limiting, federation startup, or graceful shutdown in the first pass.

Possible helper shape:

```go
func respond(w http.ResponseWriter, v any, err error)
func decodeBody(r *http.Request, dst any) error
func caller(r *http.Request) string
func pagination(r *http.Request) (limit, offset int)
```

Expected reduction: 250-450 production lines.

Risk: medium.

### 4. Centralize action and user reference resolution

The service layer already contains user and action reference resolution helpers, while the CLI also resolves action references before calling HTTP endpoints. That duplicates natural-key handling across layers.

Recommended options:

1. Preferred, if acceptable: allow server action endpoints to accept either raw action IDs or action refs in the `{id}` position, then make CLI commands pass the user's reference directly.
2. Lower-scope alternative: keep HTTP semantics unchanged, but use one CLI helper for action-ID resolution and endpoint invocation.

Expected reduction: 100-250 production lines.

Risk: medium if route semantics change; low if only CLI helper consolidation is done.

### 5. Prefer existing typed request structs over ad hoc CLI maps

Several CLI commands manually build `map[string]any` request bodies that correspond to existing kernel request structs. This is verbose and stringly typed.

Recommended direction:

- Where JSON shapes already align, build existing request structs instead of maps.
- Keep maps only where the HTTP shape genuinely differs from kernel input.
- Avoid adding new public types just for CLI convenience.

Expected reduction: 80-180 production lines.

Risk: high safety if JSON tags already align; otherwise skip.

### 6. Consolidate service-layer enrichment patterns

The service layer adds response-only fields such as computed action refs, HTTP source views, process awaiting-receipt state, and step waiting-on-peer state. This placement is correct, but the list/detail enrichment paths can be made more uniform.

Recommended direction:

- Keep existing response types.
- Add or consolidate helpers such as:
  - `enrichActions(actions []*kernel.Action) []actionResp`
  - `enrichSteps(ctx, k, steps) []*stepWithAction`
  - `enrichProcesses(ctx, k, processes) []*processView`
- Make list and detail handlers use the same helpers.

Expected reduction: 100-200 production lines.

Risk: low.

### 7. Reduce duplicate kernel wrapper/read/list/update variants

`kernel/kernel.go` is large and likely contains public methods that differ only by authorization scope or are thin pass-throughs to the store.

Recommended direction:

- Look for `ReadX`, `ReadXForSubject`, `ListX`, `ListAllX`, and `ListOwnedX` variants that can share one internal implementation.
- Remove or merge public methods that have a single production caller and no independent semantic value.
- Keep authorization behavior explicit and thoroughly tested.

Expected reduction: 200-400 production lines.

Risk: medium. Do this after lower-risk store/CLI/server simplifications.

### 8. Extract repeated call settlement construction without weakening atomicity

The call path must preserve compound atomic success/failure settlement, receipts, stats, and role law. But repeated construction of transactions, receipts, stats, timestamps, logging fields, gross/net/fee/reason values, and role fields can likely be centralized.

Recommended direction:

- Add small unexported constructors for transaction, receipt, and stats objects.
- Keep success and failure settlement semantics visibly distinct.
- Do not merge payout and refund rules into an opaque abstraction.

Expected reduction: 100-250 production lines.

Risk: medium.

### 9. Standardize native action adapter helpers

Many native actions likely repeat the same shape:

1. decode arguments
2. validate required fields
3. call a dependency
4. return a map response

Recommended direction:

- Add small unexported helpers for common argument extraction and validation.
- Keep action files cohesive.
- Do not merge all native actions into one giant file.

Possible helpers:

```go
func stringArg(args map[string]any, key string) (string, error)
func objectArg(args map[string]any, key string) (map[string]any, error)
func intArg(args map[string]any, key string, def int) (int, error)
```

Expected reduction: 100-250 production lines.

Risk: high safety for simple native actions; lower for `make.go`, which deserves separate targeted treatment.

### 10. Consolidate test setup and assertions without deleting coverage

The largest total-line savings are in tests. The right approach is not to delete scenarios, but to remove repeated setup and assertion mechanics.

Recommended direction:

- Build transparent test fixture helpers.
- Convert repeated kernel/store/server/CLI setup into fluent but simple builders.
- Keep scenario names and invariant assertions explicit.

Possible shapes:

```go
env := newTestKernel(t)
alice := env.user("@alice").deposit(100)
echo := env.action(alice, "echo").price(10).native(...)
tx := env.run(alice, echo, args).mustSucceed()
```

For HTTP tests:

```go
resp := env.POST(t, token, "/v1/run", body).OK()
resp.JSON(&out)
```

For store tests:

```go
assertUserBalance(t, db, userID, available, locked)
assertTraceWallet(t, db, traceID, available, locked)
```

Expected reduction: 1,500-3,000 test lines.

Risk: low-medium. Helpers must stay simple and should not hide the invariant being tested.

## What not to simplify

Do not remove or collapse required product concepts unless `requirements.md` changes. In particular, keep:

- `Process`
- `Trace`
- `Transaction`
- `Receipt`
- `Step`
- `Rating`
- `IdempotencyRecord`
- imported actions as ordinary `Action` rows
- remote-proxy receipt settlement
- compound atomic money-path store methods

Do not split the codebase into additional packages without explicit approval. The product contract and repository instructions prefer a small package count.

Do not weaken tests. The required behavior surface is large, so simplification should reduce duplicated mechanics, not remove coverage.

## Suggested staged roadmap

### Phase 1: low-risk mechanical consolidation

Target reduction: 800-1,400 lines.

- Add SQLite query/scan/transaction helpers.
- Convert read-only SQLite methods first.
- Add a tiny CLI helper for simple endpoint commands.
- Add shared HTTP decode/respond/pagination helpers.
- Run focused tests, then `go test ./...`.

### Phase 2: presentation and resolution cleanup

Target reduction: 500-900 lines.

- Consolidate action, step, and process enrichment helpers.
- Collapse repeated CLI action-ID resolution.
- Consider server-side action-ref acceptance if acceptable.
- Replace selected map request bodies with typed structs.

### Phase 3: kernel-internal simplification

Target reduction: 400-800 lines.

- Merge duplicate read/list authorization variants.
- Extract transaction, receipt, and stats constructors in call paths.
- Audit single-caller public methods.

### Phase 4: test fixture consolidation

Target reduction: 1,500-3,000 lines.

- Add minimal test fixture builders.
- Convert repeated setup in the largest test files.
- Preserve all scenarios and explicit invariants.

## Recommended first implementation slice

The best first concrete refactor is SQLite read/list helper consolidation.

Reasons:

- It targets the largest production file.
- It can be done without changing public APIs.
- It has limited semantic risk if started with read-only methods.
- It aligns with the repository instruction to route through existing paths instead of adding broad new abstractions.

Scope for the first slice:

1. Add unexported helpers inside `store/sqlite.go`.
2. Convert user read/list methods.
3. Convert action read/list methods.
4. Convert stats read methods.
5. Leave compound monetary writes untouched.
6. Run `go test ./store ./kernel ./cmd/juice` and then `go test ./...`.

Expected first-slice reduction: 150-300 lines with low risk.
