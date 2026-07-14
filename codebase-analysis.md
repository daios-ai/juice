# Codebase analysis against `requirements.md`

Date: 2026-07-14

## Scope and method

I read `requirements.md` as the product contract, then inspected the implementation by package and by capability. I focused on correctness risks, accidental scope, code bloat, and opportunities to remove or route through existing paths rather than add abstractions.

Commands/checks used during this analysis:

- `cat requirements.md`
- `rg --files`
- `go test ./...`
- Targeted reads of `kernel/`, `store/`, `cmd/juice/`, `script/`, `fed/`, `native/`, `llm/`, and `log/` source files.

## Requirements topics / capabilities

The requirements break down into these major topics:

1. Execution kernel and role law: `run`, `Call`, process/trace/transaction semantics, immutable role fields.
2. Package boundaries and implementation constraints: Go package layout, replaceable interfaces, no implementation-specific package names, tests for every production file.
3. Data model and persistence: users, actions, processes, traces, transactions, stats, steps, adjustments, grants, connections, federation records.
4. Authentication and authorization: password/session accounts, federation key accounts, suspended/denied users, superuser operations.
5. Action lifecycle: create/update/delete/list, activation rules, native action ownership, source/artifact handling.
6. Funding, settlement, fees, refunds, and process closure.
7. WASM execution and host capabilities.
8. HTTP/OpenAPI actions and upstream auth, including delegated OAuth/bearer connections.
9. Async steps and restart recovery.
10. Ratings, stats, lookup, and ranking inputs.
11. Admin adjustments and reconciliation.
12. Federation: identities, friendship, manifests/imports, proxy calls, receipts, retry, gossip, peer sync, retention.
13. CLI/HTTP compiled-kernel surface and required user-flow tests.
14. Logging and operational observability.

## High-level implementation shape

### Strengths

- The core package boundary is mostly aligned with the requirements. `kernel` owns interfaces and semantics; concrete SQLite, WASM, HTTP, CLI, and transport code live outside it.
- The implementation has strong test density. Every production `.go` file in the packages inspected has a matching `_test.go` file.
- Execution is intentionally centralized around `Kernel.Call`, with `Run`, WASM subcalls, step completion, HTTP actions, OpenAPI-imported actions, and remote proxies routed toward the same transition.
- Persistence exposes compound monetary methods such as `BeginRun`, `BeginSubcall`, `CommitCall`, `CommitFailedCall`, `BeginStepCall`, and `CommitRemoteSettlement`, which is the right shape for enforcing atomic accounting.
- There are explicit restart/recovery paths for orphan traces, running steps, and pending remote dispatches.
- The federation receipt code includes canonical JSON signing, hash verification, role-aware settlement checks, and retry persistence.

### Top concerns

1. **The codebase is much larger than the kernel-minimality goal.** Several files are very large: `kernel/kernel.go` (~2931 LOC), `store/sqlite.go` (~2677 LOC), `cmd/juice/serve.go` (~1585 LOC), `kernel/federation.go` (~1229 LOC), and `cmd/juice/cmd.go` (~1042 LOC). Some size is unavoidable, but much of the complexity appears to come from overlapping service/CLI/HTTP transformations and broad feature scope.
2. **`kernel.Store` is extremely wide.** It mixes execution-critical accounting, auth/session state, OpenAPI, OAuth grants/connections, lookup indexes, peer lifecycle, gossip, and config. This keeps package count low, but it also makes the persistence contract hard to audit and makes every concrete store a large all-or-nothing implementation.
3. **`Kernel` is doing too many supervision tasks.** Requirements say supervision manages users, actions, deposits, OpenAPI imports, federation peering, etc., while execution stays minimal. The `Kernel` type currently owns both execution semantics and broad supervision/import/federation workflows. This is not automatically wrong, but it blurs the intended separation and increases risk of execution paths depending on administrative helpers.
4. **Federation is by far the highest-risk area.** It combines pricing transformations, Ed25519 identity/signing, remote receipt audit, idempotency, retry, peer lifecycle, manifest import, gossip, and display cache. The code is substantial enough that future bugs are likely to hide in state transitions rather than algorithms.
5. **There are fields and comments that appear stale or contract-drifting.** For example, `Action.Source` still comments “federation URL for remote_proxy,” while requirements say remote proxies must not store a URL in `source`. The implementation may store the remote action reference there, but the comment itself is misleading and should be corrected to prevent future wrong changes.
6. **The no-new-package constraint has led to very large files rather than compact cohesive modules.** The requirement asks to keep package and source-file counts small, not to collapse unrelated logic into single files. A small number of careful intra-package file moves could reduce audit load without changing public APIs, but no code movement was done in this session.

## Capability-by-capability analysis

### 1. Execution, `run`, `Call`, role law

Implementation read:

- `kernel/call.go`
- `kernel/kernel.go`
- `kernel/types.go`
- `kernel/store.go`
- `store/sqlite.go`
- `kernel/call_test.go`
- `kernel/kernel_test.go`

Findings:

- `CallRequest` models the central dispatch modes: existing pre-funded trace for root/step calls, or parent trace for subcalls. This maps well to the contract.
- `Kernel.Call` enforces authentication, derives process from trace, checks process open, validates process-use authority for subcalls, resolves/validates action, checks funds for subcalls, creates/adopts trace, executes by `action.Kind`, validates output, and commits success/failure.
- The transaction role fields are set explicitly from the process owner, caller, and target owner. This is the right local representation of the role law.
- The action snapshot path for root/step calls is a useful simplification: wrappers pre-resolve and fund, then `Call` validates the same snapshot, reducing TOCTOU windows.

Risks / bugs to inspect further:

- `Call` is long and mixes precondition checking, trace creation, execution dispatch, remote proxy settlement, output validation, fee computation, receipt building, logging, and error settlement. This increases risk that a future change bypasses one branch. The simplification target should be fewer branches inside `Call`, not new abstractions.
- `Call` dispatches remote proxy inline rather than routing through a tiny helper with the same settlement contract. This makes local and remote failure behavior hard to compare.
- The current `canCall` function includes owner suspension from an `OwnerSuspended` join-derived field. Any path that supplies an `Action` without that field populated could accidentally miss the suspended-owner check. The code often reads actions through store methods that appear to populate it, but this is a fragile implicit contract.
- `ResolveAction` accepts a raw action ID if no slash is present, while requirements emphasize `@owner/name` for `run(action,args)`. Raw-ID support may be useful internally, but it broadens the API surface and should be verified as intentionally allowed only where needed.

Lean-code opportunities:

- Keep `Call` as the single primitive, but reduce local branching by extracting only private, side-effect-named helpers for: resolved target/action loading, trace funding/adoption, local execution settlement, and remote execution settlement. Do not add public API.
- Remove or constrain raw action-ID resolution if no user flow requires it. One action reference path is simpler and safer.
- Make action owner suspension a store-independent check by reading the target user in `checkCallPreconditions` or by requiring a small action+owner loaded type. This may cost a read but removes reliance on join-populated fields.

### 2. Package boundaries and required tests

Implementation read:

- Package roots: `kernel`, `store`, `cmd/juice`, `script`, `llm`, `fed`, `log`, `native`
- `go.mod`
- production/test file inventory

Findings:

- Package names follow the allowed production package list. There are no forbidden package names such as `sqlite`, `wazero`, `ollama`, or `libp2p` as package directories.
- All inspected production `.go` files have corresponding `_test.go` files.
- `kernel` defines interfaces for concrete adapters, satisfying the “kernel must not import concrete implementations” rule.

Risks / bloat:

- Package boundaries are okay, but file sizes are not aligned with “code size and simplicity are hard requirements.” The biggest files should be treated as risk hotspots.
- `cmd/juice` is both CLI, HTTP service, client, bootstrap, auth, and integration glue. This is permitted by package list but hard to review.

Lean-code opportunities:

- Prefer deleting duplicate CLI/HTTP mapping logic over further splitting. If splitting becomes necessary, keep it inside existing packages and only by cohesive feature, not by size alone.
- Add a simple repository check that verifies every non-test `.go` file has a corresponding test file. The codebase currently satisfies it, but an automated guard would prevent drift.

### 3. Data model and SQLite persistence

Implementation read:

- `kernel/types.go`
- `kernel/store.go`
- `store/sqlite.go`
- `store/migrations/*.sql`
- `store/sqlite_test.go`

Findings:

- The model contains the required fields plus some compatibility or derived-display fields such as `PasswordHash`, `DeletedAt`, `CompletionTraceID`, and join-populated owner fields.
- The store interface exposes atomic methods for monetary transitions, which is correct for invariants like no negative balances and trace/process/user wallet conservation.
- Migrations appear to track the specification evolution: WASM artifacts, steps cancellation, trace uniqueness, refunds, HTTP source structuring, grants/connections, P2P federation, lookup FTS, peer sync, email non-unique.

Risks / bugs to inspect further:

- `store/sqlite.go` is a single very large persistence implementation. Accounting, auth, grants, lookup, federation, and peer purge logic living together raises audit burden.
- The wide `Store` interface makes it easy for unrelated features to gain access to execution-critical write methods. This is not a runtime bug, but it weakens the semantic boundary.
- The requirements say transaction rows are immutable after commit. This should be protected in SQLite with schema/trigger or code discipline. The analysis found interface-level comments but did not confirm a DB trigger enforcing immutability.
- Some fields differ from the strict model (`DeletedAt`, `PasswordHash`, `CompletionTraceID`). They may be necessary implementation details, but each extra field is a state surface that needs invariant tests.

Lean-code opportunities:

- Keep one `Store` interface if package count must stay small, but consider smaller private interfaces at call sites (`executionStore`, `grantStore`, `lookupStore`) without changing the concrete store. This documents what each kernel method actually needs.
- Audit and delete unused store methods, if any, with `rg "MethodName"` before changing. The goal should be fewer persistence entry points.
- Add explicit transaction-immutability protection if absent. If already enforced by tests only, consider a minimal DB trigger.

### 4. Users, auth, suspended/denied accounts

Implementation read:

- `kernel/auth.go`
- `cmd/juice/authenticator.go`
- `cmd/juice/cmd_auth.go`
- `cmd/juice/serve.go`
- auth tests

Findings:

- The account model supports password/session and public-key federation identities with no user kind, matching requirements.
- Suspended users are checked by `requireActiveUser` in `Call` and other authenticated paths.
- Denied peer users are represented and used in federation peer lifecycle.

Risks / bloat:

- `authenticator.go` is large for a JWT/session component and likely contains both parsing and policy checks. That is security-sensitive and should stay lean.
- Multiple entrypoints (CLI token flows, HTTP middleware, federation signature auth) increase chances of inconsistent suspended/denied behavior.

Lean-code opportunities:

- Route every HTTP/CLI authenticated action through one subject-resolution function and keep federation signature auth separate but minimal.
- Review whether token claims/audience/issuer handling duplicates logic between client, service, and authenticator.

### 5. Action lifecycle and activation rules

Implementation read:

- `kernel/kernel.go`
- `kernel/schema.go`
- `kernel/openapi.go`
- `native/*.go`
- `cmd/juice/service.go`
- `cmd/juice/serve.go`

Findings:

- Actions support `http`, `wasm`, `native`, and `remote_proxy` kinds.
- Activation validates non-empty descriptions and schemas in kernel paths.
- Source and auth handling is implemented with write-only `AuthJSON` and token-free read shapes.
- Native actions are registered at bootstrap and orphaned native rows can be pruned.

Risks / bugs to inspect further:

- `Action.Source` comment is stale for `remote_proxy`; requirements explicitly say remote proxies do not store a URL in source. The code should make the intended meaning unambiguous.
- Action lifecycle logic in `kernel/kernel.go` is very large and likely overlaps OpenAPI import, WASM compilation, schema validation, lookup text, grant invalidation, and visibility logic.
- The requirement says active actions need field descriptions sufficient for lookup and LLM function calling. The schema validator should be checked for nested property description enforcement; shallow validation would be insufficient.

Lean-code opportunities:

- Delete stale compatibility fields or comments where possible; stale comments are dangerous in a contract-heavy kernel.
- Consolidate activation validation into one private function used by create, update, OpenAPI import activation, and remote import activation.
- Prefer deactivating and invalidating grants through one path for all contract-changing updates.

### 6. Funding, settlement, fees, refunds, closure

Implementation read:

- `kernel/call.go`
- `kernel/federation.go`
- `kernel/steps.go`
- `store/sqlite.go`
- settlement-related tests

Findings:

- The implementation uses exact root funding, trace-level child funding, eager settlement, and process closure when quiescent.
- `traceStripes` serialize fund spends and settlement around a trace. This is a compact solution to capability-composition races without per-trace lock lifecycle management.
- Failed calls use `CommitFailedCall`, which cancels outstanding steps in the subtree and refunds remaining trace allocation plus parked prices.
- Remote settlement has a distinct `CommitRemoteSettlement` path because charge/duty/refund differs from local net/fee settlement.

Risks / bugs to inspect further:

- Local success builds receipts with charge equal to gross, while net/fee are based on taxable remaining trace allocation. This appears deliberate for “caller paid advertised price,” but receipt semantics should be cross-checked against all reconciliation tests.
- Remote proxy price decomposition uses `floor(q * 10000 / (10000 + ImportBPS))`. If proxy prices are rounded up at import time, reverse derivation can be lossy. Tests should assert exact behavior across small prices, especially prices 0 and 1.
- There are multiple refund destinations: process, parent trace, step semantics, remote pending retry, process closure. These are correct concepts but high risk.

Lean-code opportunities:

- Add or keep table-driven tests around settlement arithmetic rather than more comments.
- Consider one small internal struct for settlement inputs to reduce long method signatures to store commit functions. This is a readability refactor only if it reduces parameters and mistakes; avoid broad abstraction.

### 7. WASM and host capabilities

Implementation read:

- `script/compile.go`
- `script/wasm.go`
- `script/sdk.tmpl`
- `kernel/capability.go`
- WASM/native tests

Findings:

- WASM execution is behind `ScriptExecutor`, and the kernel does not import the concrete WASM implementation.
- Host callbacks include call, step create, step complete, and log, matching the composition model.
- Compile/execute concerns are isolated in `script`.

Risks / bloat:

- Host capability security depends on trace-scoped process authority and settled-trace invalidation. This area should remain small and heavily tested.
- `tinygo.go` in `native` plus `script/compile.go` may duplicate compilation-facing concepts. Verify whether both are required.

Lean-code opportunities:

- Keep the SDK template minimal. Avoid adding convenience host functions that duplicate kernel primitives.
- If native TinyGo helper action is only an operational wrapper around `script`, ensure it does not become a second compilation path with different validation.

### 8. HTTP actions, OpenAPI import, upstream auth, delegated grants/connections

Implementation read:

- `kernel/openapi.go`
- `cmd/juice/http_exec.go`
- `kernel/kernel.go` auth/grant helpers
- `cmd/juice/connect.go`
- `docs/oauth.md`
- OpenAPI/auth tests

Findings:

- OpenAPI parsing supports JSON/YAML, local `$ref` resolution, operation import, schema compilation, provenance hashing, and contract-change deactivation.
- Auth schemes include owner-held and delegated OAuth/bearer schemes.
- `Connection`/`Grant` model follows the requirement: connection holds the encrypted upstream credential; grants point at connections; read views do not expose secrets.
- Pre-lock `grant_required` enforcement is implemented in `checkGrantRequired` for delegated HTTP actions.

Risks / bugs to inspect further:

- OpenAPI import is necessarily complex, but `kernel/openapi.go` includes parser, schema compiler, matching, provenance, ownership proof assumptions, and operation rules in one file. It is a hotspot.
- Local `$ref` recursion is depth-limited, but circular schemas may silently remain partially unresolved. That may be acceptable, but activation should fail closed if unresolved schemas become invalid.
- OAuth provider-key derivation is security-sensitive. Any mismatch between action source host parsing in kernel and HTTP executor can create confused-deputy risk.
- The requirements mention startup backfill for legacy grant tokens. Ensure this path is idempotent and not reachable during normal execution.

Lean-code opportunities:

- Reduce OpenAPI import scope to only contract-required features; reject ambiguous constructs rather than support more of OpenAPI.
- Keep one canonical provider-key derivation helper; do not duplicate in CLI/service/executor.
- Remove legacy grant-token fields and backfill path once migrations and production compatibility allow.

### 9. Async steps and recovery

Implementation read:

- `kernel/steps.go`
- `store/sqlite.go`
- step tests

Findings:

- Step creation parks the action price from the parent trace and stores `required_caller_user_id`, action, partial args, and price snapshot.
- Step completion derives process from `parent_trace_id`, enforces required caller, derives allowed input schema from current action schema minus partial args, and calls through `Call` with a pre-funded completion trace.
- Recovery re-parks empty crashed step completions and settles non-empty orphan completion traces as failed.

Risks / bugs to inspect further:

- `CompleteStep` has subtle schema-collision logic. It protects all-bound declared properties, but this should have tests for undeclared extra keys, partial overwrite, and schema changes after step creation.
- Step completion against a changed/deactivated action is supposed to reset the step to waiting. Confirm all changed-schema paths do this rather than returning an error with the step stuck running.
- External step creation precondition authority is delegated to service layer per comment. That boundary should have HTTP tests; otherwise kernel-level direct callers can create unauthorized external steps.

Lean-code opportunities:

- Keep step state transitions in as few store methods as possible. Avoid exposing primitive updates for `status`, `tx_id`, or `CompletionTraceID`.
- Prefer deleting any stored derived completion schema; requirements correctly derive it live.

### 10. Ratings, stats, lookup, and ranking inputs

Implementation read:

- `kernel/lookup_test.go`
- `kernel/kernel.go` stats/rating/lookup sections
- `native/lookup.go`
- `ranking.md`
- store lookup methods

Findings:

- Stats are updated on call settlement and ratings through store methods.
- Lookup uses embeddings and lexical search with optional `StatTag` data for experiments.
- Execution code does not appear to rate outputs, matching requirements.

Risks / bloat:

- Ranking/lookup can easily grow into kernel semantics. The requirements explicitly keep optional tags out of execution semantics.
- `Stats` reset behavior differs for OpenAPI vs remote imports per requirements. This is easy to regress.

Lean-code opportunities:

- Keep lookup as read-only over action/stat/tag data. Avoid letting lookup write anything other than isolated embedding/index/tag rows.
- Delete or quarantine experimental ranking code if it starts affecting execution.

### 11. Admin adjustments and reconciliation

Implementation read:

- `kernel/kernel.go`
- `cmd/juice/cmd_superuser.go`
- `store/sqlite.go`
- admin tests/flows

Findings:

- Adjustments are modeled as immutable audit records with idempotent `external_key` support.
- Admin deposit/withdraw is separate from execution settlement, as required.

Risks / bugs to inspect further:

- Withdraw/debit must not consider locked funds available. Store method comments indicate it checks `available`, which is correct.
- Superuser checks should be centralized. Multiple CLI/HTTP admin paths increase risk of inconsistent enforcement.

Lean-code opportunities:

- Keep adjustments as the only out-of-band balance mutation path. Delete any direct “set balance” helpers if present.

### 12. Federation, remote proxies, receipts, gossip, peer lifecycle

Implementation read:

- `kernel/federation.go`
- `fed/fed.go`
- `fed/transport.go`
- `cmd/juice/connect.go`
- `cmd/juice/service.go`
- federation tests and flows

Findings:

- Federation identity uses Ed25519 keys and canonical JSON signatures.
- Remote manifests, imports, proxy actions, remote receipts, retry idempotency, gossip tags, peer sync cache, and peer retention are implemented.
- Remote proxy calls store idempotency/dispatch data on traces and retry pending traces after restart or from the running server.
- Receipt verification checks stored hash, signature, action ID, status, charge, settlement arithmetic, refund conservation, args hash, and reply hash.

Risks / bugs to inspect further:

- `kernel/federation.go` is too large for such security-critical logic. It includes identity, manifest signing, import reconciliation, friend lifecycle, gossip, proxy execution settlement, receipt verification, and retry. That is too much to audit as one unit.
- Requirement says remote proxy `source` should not store a URL. Confirm all imports store only an action reference and never a peer URL; fix stale comments and tests accordingly.
- Pending remote dispatch max-age behavior should be compared to the requirements: the requirements say timeout does not settle and retry recovers receipt, but also mention max-age expiry fires from running server. The implementation has `RemotePendingMaxAge`; make sure the exact max-age semantics are documented in requirements-consistent terms.
- Friend/unfriend peer lifecycle mutates users, proxies, steps, and denied state. This should be entirely atomic where required.
- Gossip is display/ranking metadata only. Ensure peer sync cache never gates calls; comments say it does not, but action listing annotations must remain advisory.

Lean-code opportunities:

- Separate federation logic internally by private sections or small files inside `kernel`: receipt verification, manifest import, peer lifecycle, remote call settlement. This is not adding a package and can reduce audit risk, but only if it does not create new abstractions.
- Delete HTTP-era federation remnants if any remain. The requirements removed advertised URLs and `.well-known` style discovery.
- Keep fake transport tests as the primary kernel federation tests; real transport should remain release-gate/integration only.

### 13. CLI and HTTP user-flow surface

Implementation read:

- `cmd/juice/*.go`
- `flows/*.sh`
- CLI/HTTP tests

Findings:

- The CLI/server package implements authentication, command parsing, service methods, HTTP handlers, client helpers, bootstrap, and control operations.
- Flow tests exist for foundations, admin, calls, WASM, federation, remote, network, and full integration.

Risks / bloat:

- `cmd/juice/serve.go`, `service.go`, and `cmd.go` are large and likely duplicate validation/serialization logic.
- Service-layer authorization comments are important: any kernel method that relies on service enforcement must be covered by HTTP/CLI tests.
- There is a risk of features being available through HTTP but not CLI, or vice versa, unless flow tests cover both required surfaces.

Lean-code opportunities:

- Prefer one service method per kernel operation and have CLI/HTTP call it rather than reimplement validation in both.
- Delete client-side shaping that duplicates server behavior, except for display formatting.

### 14. Native actions and LLM adapters

Implementation read:

- `native/*.go`
- `llm/*.go`
- native/llm tests

Findings:

- Native actions are small and individually tested.
- LLM interfaces are behind `kernel` abstractions, with concrete utility in `llm`.

Risks / bloat:

- Native actions can become an application layer inside the kernel if allowed to grow. Requirements say the kernel should remain minimal and the rest should live at the application layer as actions.
- LLM lookup/decide helpers should not become execution semantics.

Lean-code opportunities:

- Keep native actions limited to platform primitives needed by tests/product contract.
- Remove or externalize native actions that are merely demo/application features if not required by `requirements.md`.

### 15. Logging and observability

Implementation read:

- `log/log.go`
- log tests
- logging calls in kernel

Findings:

- Structured logging includes contextual process, trace, action, transaction, caller, and handle fields.
- Logging is passed into kernel rather than global-only.

Risks / bloat:

- Logging must never include secrets. HTTP auth and delegated credential paths should be periodically checked for accidental auth JSON/token logging.

Lean-code opportunities:

- Keep logging wrappers small; do not add feature-specific logging abstractions.

## Priority bug/refactor backlog

### P0 / correctness audit before adding features

1. **Confirm all action reads used for callability include owner suspension state.** If not guaranteed, move suspended-owner enforcement to an explicit target user read inside the kernel precondition path.
2. **Verify remote proxy source semantics.** Requirements say no URL in `source` for remote proxies. Fix stale comments and add a test that imported proxies store remote action references, not URLs.
3. **Audit transaction immutability at the DB level.** If SQLite does not enforce immutability after insert, add the smallest possible guard or a direct invariant test.
4. **Table-test small-price remote duty arithmetic.** Include proxy prices and manifest prices around 0, 1, and values not divisible by the duty denominator.
5. **Confirm step completion schema-change behavior.** Requirements say completing against changed/deactivated action resets the step to `waiting`; verify both code and tests.

### P1 / lean-code reductions

1. **Shrink `Kernel.Call` without changing semantics.** Extract private helpers only where they remove branches or long parameter sequences.
2. **Reduce `kernel/federation.go` audit load.** Move receipt verification, manifest import, peer lifecycle, and retry into cohesive files within `kernel` if needed; do not add packages.
3. **Narrow store usage at call sites.** Introduce private interface views only if they delete accidental dependencies and make execution-critical code easier to inspect.
4. **Deduplicate CLI/HTTP/service validation.** Route through a single service method wherever possible.
5. **Retire legacy grant-token backfill after compatibility window.** Remove `Grant.RefreshToken` and legacy migration code when safe.

### P2 / documentation and guardrails

1. **Add a test or script enforcing production-file/test-file pairing.** The repo currently satisfies the rule; automate it.
2. **Refresh comments that conflict with requirements.** Stale comments in a kernel are future bugs.
3. **Document which native actions are product-contract primitives.** Everything else should be application-layer or removable.

## Code bloat hotspots

Approximate production file sizes observed:

- `kernel/kernel.go`: ~2931 lines
- `store/sqlite.go`: ~2677 lines
- `cmd/juice/serve.go`: ~1585 lines
- `kernel/federation.go`: ~1229 lines
- `cmd/juice/cmd.go`: ~1042 lines
- `cmd/juice/service.go`: ~956 lines
- `cmd/juice/authenticator.go`: ~779 lines
- `kernel/call.go`: ~773 lines
- `kernel/openapi.go`: ~663 lines
- `fed/transport.go`: ~615 lines

The size itself is not the only problem; the issue is that several of these files combine multiple product capabilities and state transitions. The safest reduction strategy is not broad restructuring. It is repeated, small deletion/consolidation passes:

1. Find duplicated validation or formatting.
2. Route through existing kernel/service paths.
3. Delete the duplicate.
4. Add one focused regression test if a path was previously uncovered.

## Overall recommendation

Do not add new capabilities until the P0 audit items are resolved. The implementation appears thoughtful and heavily tested, but it is already near the complexity limit for a “minimal kernel.” The highest-value work is simplification and invariant hardening: fewer resolution paths, fewer persistence entry points used by execution, smaller federation audit surfaces, and stale-comment removal. Future changes should be accepted only if they delete code, route through an existing kernel primitive, or directly enforce a requirement invariant.
