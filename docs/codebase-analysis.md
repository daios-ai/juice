# Juice codebase reduction analysis

Date: 2026-08-07  
Scope: the existing `requirements.md` contract and every implemented capability it describes.  
Constraint: no new features. This report optimizes for fewer files, fewer public paths, less executable code, and a smaller critical kernel. No production code was changed.

## Executive answer

The first report answered the wrong question. It was mainly a correctness and hardening audit. Executing it would have grown the tree. This replacement asks a stricter question: **what can be deleted or collapsed while preserving the current product contract?** Correctness appears here only when a simpler path also removes an incorrect duplicate.

There are three different kinds of “size” in this repository, and they must not be mixed:

| Surface | Current size | Main reduction available |
|---|---:|---|
| Product contract | 1,420 lines / 29,819 words | Delete the prose test script and repeated rationale; rewrite rules once |
| Production Go | 26,081 lines | Remove duplicate narration, test scaffolding, dead public paths, and localized adapter repetition |
| Schema history | 41 files / 992 SQL lines | Replace the upgrade chain with one current baseline if the three-node deployment can cut over together |
| Test Go | 34,185 lines | Delete tests for retired migration history, fake utilities, and duplicate APIs; retain behavioral tests |

The production Go count consists of 1,862 blank lines, 3,933 line-comment lines, and 20,286 other lines. Therefore deleting comments can make the repository visibly smaller but must not be presented as executable-code reduction.

### Reduction portfolio

These are estimates to use as deletion budgets, not promises to be met by relocating code elsewhere.

| Change | Production reduction | Test/document reduction | Files | Contract effect | Recommendation |
|---|---:|---:|---:|---|---|
| Squash migrations to the current schema | about 699 SQL lines | about 450–550 migration-test lines | about 40 fewer SQL files | None after an explicit deployment cutover | Do first if old DB upgrades are no longer needed |
| Remove duplicated requirement narration from Go | about 900–1,400 comment lines | none | 0 | None | Do in touched files; preserve why-comments for concurrency, money, and signatures |
| Delete production test scaffolding and tests of fakes | about 110–130 Go lines | about 120–180 Go lines | 2 fewer Go files are plausible | None | Do; put the one actually needed fake beside its consumer test |
| Retire duplicate/dead Kernel and Store paths | about 120–180 Go lines | about 150–250 Go lines | 0 | None for in-repo behavior | Do after a final external-API check |
| Collapse Ollama request plumbing | about 60–90 Go lines | small | 0 | None | Do; one private request function is enough |
| Compact native schema construction and registration | about 80–140 Go lines | small | 0 | None | Do, but do not invent a general schema DSL |
| Inline service functions that only forward | about 55–85 Go lines | small | 0 | None | Do; retain the substantial response-building functions |
| Consolidate only genuinely identical CLI/HTTP wrappers | about 60–120 Go lines | about 40–80 Go lines | 0 | None | Do conservatively; command behavior stays explicit |
| Narrow OpenAPI/schema grammar | about 160–260 Go lines | about 100–200 Go lines | possibly one dependency removed | **Yes: accepted input narrows** | Separate product decision, not part of the unconditional cut |

The unconditional target is therefore roughly:

- 425–745 fewer executable production Go lines;
- 900–1,400 fewer production comment lines;
- about 699 fewer production SQL lines;
- roughly 760–1,060 fewer test lines;
- roughly 42 fewer files, mostly historical migrations and test-only utility pairs.

That is a meaningful reduction, but it is not a claim that thousands of executable Go lines are redundant. The current contract itself is broad: local execution, WASM, HTTP/OpenAPI, OAuth delegation, steps, receipts, ratings, federation, gossip, settlement, CLI, and control API. A much larger executable reduction requires deleting or narrowing one of those contracted capabilities. The only concentrated candidate that can be narrowed without disturbing the money kernel is the OpenAPI/schema input surface, and it is explicitly separated below.

## Corrections to the earlier analysis

The following prior recommendations or emphases should be rejected:

- **Keep `pricedStore`** (`kernel/federation.go:186-268`). It attaches derived remote pricing to the store handle used by all action reads. Removing it would require patching dozens of reads or adding a parallel resolver. The decorator is the smaller, safer expression of a cross-cutting read rule.
- **Do not build a generalized SQLite mapper.** `store/sqlite.go` already has canonical scanners for the major row types and a generic `queryList`. The remaining roughly 50 `Scan` sites are largely scalar reads or compound-transition reads with different projections. Reflection, an ORM, or a second row DSL would add machinery for little deletion.
- **Do not describe `cmd/juice` as four duplicated layers.** The CLI uses one generic `apiCall`/`apiEmit` client in `client.go`; it does not have one client method per endpoint. `service.go` is also not generally pass-through: most of it constructs safe public views and federation/control responses. Only the small forwarders listed below should disappear.
- **Do not add a resource manager, exact JCS implementation, arithmetic framework, or ten test families under a “reduction” heading.** Those may be legitimate correctness work, but they are additions. They belong in a separate backlog.
- **Do not split the requirements into an ADR corpus, conformance matrix, and several new documents.** The shortest documentation architecture is one product contract plus the existing `API.md`.

## Method and verified baseline

I read all 16 requirements sections and traced their implementations through `cmd/juice`, `kernel`, `store`, `fed`, `script`, `llm`, `native`, and `log`. I inspected the route/CLI/service/kernel/store paths, the 41 migration files, Store scanners and transactions, native bootstrap metadata, authentication variants, OpenAPI/schema compilation, and tests tied to obsolete paths.

Read-only verification:

- `go test ./...` passes.
- `go vet ./...` passes.
- 46 production Go files and 54 Go test files.
- Every production Go file has the required same-basename `_test.go` file.
- Production Go: `kernel` 10,723; `cmd/juice` 8,981; `store` 3,616; `fed` 768; `native` 763; `llm` 488; `script` 475; `log` 267.
- `kernel.Store` has 120 methods. `*store.DB` has 138 pointer methods including private/test helpers and `Close`.
- `store/sqlite.go` contains about 50 `Scan` call sites and 26 `withTx` calls.
- Applying all migrations to an empty database and dumping the current schema produces about 293 SQL lines, versus 992 lines of migration history.

Counts in this report come from the current checkout, so they intentionally differ from estimates based on an earlier revision.

## Capability-by-capability implementation analysis

### 1. Execution, supervision, and role law

Implementation: `kernel/call.go`, run/process code in `kernel/kernel.go`, `kernel/steps.go`, and the HTTP/federation entry points.

The central design is already the right small design: every paid execution converges on the same call/trace/settlement machinery, while supervision stays outside it. Do not introduce another dispatcher or execution service.

Reduction:

- Keep one internal funded-call engine, but reduce its public surface. `CallRequest` exposes orchestration-only fields such as an existing trace, supplied action, step, and idempotency record. Production callers use them in controlled ways, but one wide public request type makes several trusted modes look like a general API. Make the orchestration request private when compatibility permits and keep the narrow user-facing operations (`Run`, callback/subcall, step completion, inbound federation) as the callers. This is an API deletion and clarity improvement, not a new layer.
- Delete wrapper methods that have no production caller rather than preserving “complete-looking” APIs.
- Preserve compound settlement methods. Splitting them would create more calls and more invalid intermediate states.

Bug worth fixing only because it removes ambiguity: the funded call should not accept a caller-supplied authoritative `Action` through a public API. Sealing the mode is smaller than adding validation around every field.

### 2. Packages and implementation constraints

Implementation: all packages and paired tests.

The package graph follows the contract, and the current package count should not grow. No new package is justified by this review. In particular:

- do not create `internal/testutil`, `schema`, `money`, `transportlimits`, or `service` packages;
- do not split the large files merely to make line counts look smaller;
- keep `kernel` independent of SQLite, libp2p, HTTP servers, Ollama, and wazero.

Reduction should come from deleting code within the existing cohesive files. A file split is neutral or negative under the stated objective.

### 3. Data model

Implementation: `kernel/types.go`, `kernel/store.go`, migrations, and `store/sqlite.go`.

The model is large because the contracted product is large, not because each entity has multiple implementations. Immutable IDs, snapshots, ledger rows, receipts, grants, connections, peer identities, and pending transfers each have distinct state laws. Combining unrelated entities into generic maps would shorten declarations while making the kernel less verifiable.

Reduction:

- Rewrite the 2,416-word data-model section of `requirements.md`. It currently packs schema, transitions, security rules, display rules, and rationale into table cells. Keep fields and row invariants here; move each transition rule to its one owning operation section; delete the repeats.
- Remove unused read APIs rather than trying to genericize the data model. `ReadRootTrace` and `ListTraces` have no production callers; they are test conveniences.
- Keep typed structs and explicit SQL. They are appropriate for a critical money state machine.

Do not merge `Account` and `Kernel`, `Grant` and `Connection`, or execution and value-transfer fields merely to save declarations. Those distinctions enforce existing trust boundaries.

### 4. Authorization, call validity, and traces

Implementation: call preconditions in `kernel/call.go`, action/user methods in `kernel/kernel.go`, `kernel/capability.go`, grants, and HTTP middleware.

The `Live`/`Visible`/`CanCall` rules are a useful compact specification and should replace repeated prose elsewhere. The implementation largely centralizes these rules.

Reduction:

- Remove the single-action grant API trio when no external Go consumer requires it: `AttachBearerGrant` (`kernel/kernel.go:404`), `CreateGrant` (`:419`), and `RevokeGrant` (`:492`). Production uses selector/reconcile operations (`AttachBearerGrants`, selector revocation), and a full action selector already represents the one-action case. This deletes parallel authorization flows and their tests.
- Keep one canonical action-reference parser and one authorization check; do not duplicate the visibility law in CLI or service code.
- Seal internal call modes as described above.

Small correctness fix: `BeginSubcall` should include the existing “parent unsettled/process open” predicate in its admission SQL. This is a predicate on the owning transition, not a new abstraction. It matters particularly for a zero-price subcall, where `available >= 0` alone accepts a settled trace.

### 5. Persistence, atomicity, and recovery

Implementation: `kernel/store.go`, `store/sqlite.go`, embedded migrations, and `Kernel.Recover` in `kernel/steps.go`.

The compound SQLite operations are the strongest and most important part of the kernel. They should not be decomposed to reduce method size. The correct reduction is around them:

- squash historical migrations after deployment cutover;
- remove Store/DB methods used only as test inspection shortcuts;
- remove redundant public Kernel wrappers over Store;
- keep canonical scan helpers and `queryList`;
- keep explicit `withTx` blocks for genuinely different atomic write sets.

`kernel.Store` is broad because it is the dependency boundary for the whole kernel. Splitting it into many micro-interfaces would add declarations and wiring without deleting SQLite behavior. Replacing it with the concrete DB is impossible without reversing the package dependency. Leave the interface broad unless method deletion makes it naturally smaller.

Recovery has a real correctness question—failed repairs are logged and the broad reset continues—but a redesign would add or move code. Treat it separately from the deletion program. The only reduction-safe change is to remove the redundant `Kernel.ResetRunningSteps` wrapper (`kernel/steps.go:14`) because recovery already calls the Store operation directly.

### 6. Call transition and settlement

Implementation: `kernel/call.go` and compound call operations in `store/sqlite.go`.

This is critical architecture and should stay explicit. The trace wallet and compound success/failure commits are not boilerplate. Do not replace them with a generic state-machine framework, event sourcing layer, or hooks.

Reduction:

- keep one calculation for each monetary concept and route callers through it when that deletes variants;
- keep the transaction/receipt writes together;
- delete duplicated narrative comments that restate §6 line by line, retaining comments that explain lock ordering, crash behavior, or why an apparently redundant write is atomic.

The outbound remote-transfer reserve has a confirmed one-line defect in `kernel/federation.go:128`: it uses `k.cfg.RemoteBPS` instead of `actionRemoteBPS(a)`. Fixing it is line-neutral and removes disagreement between reserve and settlement. Broad checked-arithmetic work is valid correctness work but likely net-additive, so it is not part of the reduction order.

### 7. Action lifecycle

Implementation: action CRUD and activation in `kernel/kernel.go`, action SQL in `store/sqlite.go`, service views, and routes.

There is one real lifecycle; CLI and HTTP are adapters, not competing implementations. Retain private/inactive creation and explicit activation.

Reduction:

- inline `enableAction`, `disableAction`, `deleteAction`, and `actionStats` from `cmd/juice/service.go:745-760` into their handlers. They are four- to six-line forwarders with no presentation logic.
- remove `Kernel.ResetActionStats` (`kernel/kernel.go:2691`), which has no callers.
- consolidate enable/disable at the adapter level only where it is already parameterized; do not make kernel lifecycle state a generic boolean mutation.
- if action update/delete and grant invalidation are later made one atomic Store command for correctness, use that opportunity to delete the old low-level mutation combination. Do not keep both paths.

### 8. Imported actions, OpenAPI, and delegated credentials

Implementation: `kernel/openapi.go` (664 lines), `kernel/schema.go` (275 lines), auth/grant code in `kernel/kernel.go`, and `cmd/juice/authenticator.go`.

This is the largest concentrated simplification surface, but part of it is a product decision.

Contract-preserving reductions:

- delete the single-action grant functions in favor of selector reconciliation;
- reconcile and, if compatibility permits, remove the direct password-grant fallback from `/v1/auth/token`; requirements specify only the authorization-code exchange, while `API.md` still documents the older direct path;
- reject unsupported inputs early rather than carrying permissive fallback branches deeper into compilation;
- use one schema normalization/validation pass for action creation, import, and LLM structured output.

Optional contract narrowing:

- accept OpenAPI as JSON only and delete YAML normalization. YAML is not explicitly promised by §8, but existing users may reasonably rely on it; decide explicitly.
- reject all `$ref` rather than maintaining a partial local resolver. This is a large compatibility restriction because ordinary OpenAPI documents use component references.
- define the supported JSON Schema grammar as only the constructs Juice executes: object/properties/required, array/items, and primitive types. Reject every other keyword. Removing currently supported `enum`/`nullable` would also be a compatibility change.

If all three narrowings are accepted, `openapi.go`/`schema.go` can plausibly lose 160–260 production lines and the direct YAML dependency. If they are not accepted, do not promise a major line reduction here: making the present resolver fully correct is more likely to add code.

The lean standard pattern is a **strict compiler for a declared subset**, not a home-grown permissive OpenAPI engine and not a full OpenAPI framework dependency.

### 9. Adapters, native actions, and stats

Implementation: `script`, `llm`, `native`, `cmd/juice/http_exec.go`, bootstrap native specs, and kernel lookup/stats.

WASM and native handlers are mostly small and cohesive. Keep them as mediated adapters into the single call path.

Reduction:

- `llm/llm.go` repeats JSON marshal, request construction, headers, status handling, body closure, and response decode for embed/chat/JSON/decide. One private `post(path, request, response)` helper can remove about 60–90 lines without changing an interface or adding a file.
- `llm/testutil.go` is 70 production lines of fakes. Only `FakeEmbedder` has one consumer outside tests of the fake itself. Put a minimal embedder in the existing consuming test and delete `testutil.go` plus `testutil_test.go`. Do not create a test utility package.
- `script/compile.go:141-179` embeds a fake compiler and test WASM in production. Move a compact fake beside the existing native compiler tests and delete tests whose only purpose is testing the fake. Some test duplication may remain, so count the net result, not only production LOC.
- `cmd/juice/bootstrap.go:216-493` expresses 13 native schemas in verbose nested `map[string]any`; `cmd/juice/main.go:341-358` separately lists handler registration; the native files repeat handler names. Keep the metadata in the existing bootstrap/native files, but introduce only a handful of schema constructors for the already-supported grammar and one registration list. This should be a local vocabulary (`object`, `array`, primitive field), not a general schema DSL or new package.

Stats are small derived records attached to call commits. Do not extract a metrics subsystem.

### 10. Steps

Implementation: `kernel/steps.go`, Store step transitions, HTTP service views, native message/transfer handlers, and federation step messages.

Steps are intrinsically cross-cutting because they park money and resume execution later. The waiting/running/done transitions and atomic claim/repark operations should remain explicit.

Reduction:

- delete `Kernel.ResetRunningSteps`, a wrapper used only by tests; tests should exercise `Recover` or the Store transition at the appropriate package boundary;
- inline `completeStep` and `endProcess` service forwarders;
- keep `enrichStep` and allowed-input derivation because they shape the public contract and prevent private action leakage;
- do not create a generic workflow engine.

The migration/history cut deletes substantial step-related compatibility setup without touching current step semantics.

### 11. Receipts, ratings, signatures, and transaction access

Implementation: `kernel/jcs.go`, receipt/rating methods, federation evidence, and atomic Store commits.

One canonical signing representation and one receipt per committed call are the right small architecture. Keep domain separation and immutable audit rows.

Reduction:

- delete `ListAllTransactionViews` (`kernel/kernel.go:1964`) and its supporting `ListAllTransactions` path if the normal `ListTransactions` superuser scope fully covers it. The former is used only by tests; the control/CLI path already uses the normal listing.
- inline the six-line `verifyReceipt` service forwarder.
- remove repeated signature-payload prose from code comments and tests; the contract should state the payload once and tests should use named fixtures/vectors.

The current custom JCS implementation has correctness risks, but exact RFC 8785 work is additive. Schedule it honestly outside this reduction effort. Do not “simplify” cryptographic canonicalization unless the replacement is demonstrably standard-compliant.

### 12. Authentication, bootstrap, and supervision

Implementation: `kernel/auth.go`, auth portions of `kernel/kernel.go`, bootstrap, HTTP auth routes, and CLI browser/loopback flow.

The production CLI uses authorization code + PKCE, refresh, and recovery. There is also a direct password-token path:

- `postTokenMulti` (`cmd/juice/serve.go:1208`) defaults to a password grant branch;
- `Kernel.Login` (`kernel/kernel.go:1220`) supports that branch;
- `LoginWithRefresh` (`kernel/auth.go:261`) has no production caller and is used as a test setup shortcut.

Requirements §12 describes browser authorization code with PKCE and the `/v1/auth/token` exchange of code plus verifier. The current CLI follows that path. `API.md`, however, still documents a direct `{handle,password}` token request, and the server still implements it. This is contract/documentation drift, not an undocumented path. Decide which document is authoritative for compatibility. If `requirements.md` is the product contract as stated, delete the direct branch and `Kernel.Login`, update `API.md`, and replace test setup calls with authorization-code/refresh primitives or package-local setup. If external clients are promised the `API.md` behavior, retain it and remove this deletion from the budget.

Also remove exported test knobs where possible: `SetBcryptCostForTesting`, `SetMinPasswordLenForTesting`, and `SetLookupHost`. Tests in an external package may require small package-local seams, but these should not be general public product APIs. Prefer existing-package tests for unexported state over production “ForTesting” methods.

Bootstrap itself contains necessary persistence and config reconciliation; rewriting it as a framework will not shrink the tree. Its long comments can be reduced after the contract is concise.

### 13. Federation

Implementation: `kernel/federation.go`, `fed/fed.go`, `fed/transport.go`, discovery/control orchestration, Store exposure/settlement/evidence methods, and migrations.

Federation is the largest requirements section (7,433 words) and a large part of the kernel because it contracts identity, discovery, signed calls, idempotency, exposure, settlement, value transfer, remote steps, and retention. There is no honest multi-thousand-line deletion here without removing one of those capabilities.

Keep:

- `pricedStore`; it is the smallest safe place for derived remote prices;
- signed manifests/receipts and idempotency records;
- explicit pending/quarantined transfer state;
- non-transitive proxy resolution;
- compound local settlement writes.

Reduce:

- replace repeated prose with a protocol state table: message, signer, idempotency key, precondition, atomic local transition, terminal states;
- delete version-history commentary and old migration compatibility after baseline cutover;
- route all local action reads through `pricedStore`, not copied price logic;
- fix the one-line remote BPS bug while the pricing code is touched.

The missing transport-resource budgets from §13 are a genuine existing requirement, but implementing them is purely additive. It must not be presented as code reduction. Either schedule it as correctness work or simplify/remove the requirement in an explicit contract decision; do not obscure the tradeoff.

### 14. CLI, HTTP, logging, and config

Implementation: `cmd/juice` (8,981 production lines) and `log` (267 lines).

The CLI-to-HTTP path is:

```text
Cobra command -> generic apiCall/apiEmit -> HTTP handler ->
optional response-building function -> Kernel -> Store
```

There is no per-endpoint client layer to remove. The duplication that does exist is narrower:

- service forwarders with no view construction;
- repeated simple “show one resource” commands and handlers;
- repeated pagination flag/query setup;
- repeated no-body mutation commands;
- verbose comments restating CLI requirements.

Delete the following service forwarders first: `planGrants`, `enableAction`, `disableAction`, `deleteAction`, `actionStats`, `endProcess`, `completeStep`, `verifyReceipt`, and `run`. `startRecovery`/`completeRecovery` can also be inlined if their response maps move directly into the handler. Expected production deletion is about 55–85 lines.

Retain the rest of `service.go` where it performs real presentation work: account-reference resolution, redaction, action HTTP decomposition, process/step enrichment, transaction party names, peer state, discovery views, and settlement views. Moving those lines into handlers would only relocate code and make handlers harder to audit.

For Cobra and HTTP, add at most two or three private helpers for shapes already repeated several times: show-by-ID, no-body mutation, and paginated query construction. Do not create a route/command code generator or declarative endpoint framework. The realistic net is 60–120 production lines, not the deletion of `service.go`.

`log` is already small. Keep it.

### 15. Automated tests

Implementation: 34,185 lines across paired unit/integration tests and flows.

The test suite is larger than production Go, but high test volume is not itself bloat in a critical money kernel. Delete tests only when the behavior or compatibility history they prove is deleted.

High-confidence deletion:

- migration-specific tests for migrations 011, 021, 037, 038, 039, and 040 after a baseline cutover;
- tests of `FakeChatter`, `FakeCompiler`, and other fake behavior that is not product behavior;
- tests for retired single-action grants, direct password login, test-only trace listings, and duplicate transaction listings;
- repetitive tests that assert identical HTTP plumbing after handlers share the same tested helper.

Keep behavioral tests for current schema constraints, compound money transitions, recovery, signatures, action visibility, delegated-credential isolation, federation idempotency, and current HTTP/CLI behavior. A smaller test suite achieved by deleting invariant coverage would make the kernel smaller only cosmetically.

Requirements §15 should not enumerate hundreds of test cases in prose. Tests are the executable conformance inventory; the contract needs only the required test layers and the rule that every normative behavior is covered.

### 16. Design rationale

Implementation: §16 plus thousands of comments and test explanations.

Rationale belongs next to a surprising decision, once. Current rationale is repeated in §3 tables, operation sections, §15 test prose, §16, source comments, and test comments. This is already producing terminology drift.

Replace §16 with a short principles section near the beginning:

1. one mediated call path;
2. money transitions and audit rows are atomic;
3. authority follows immediate caller and funded trace;
4. external effects occur only through adapters;
5. federation distrusts peer state and verifies signed evidence;
6. derived data is recomputable and never an authority source.

Those principles are enough. Detailed “why” remains only where it prevents a tempting but incorrect simplification.

## The large deletions in detail

### A. Replace 41 migrations with one baseline

The current migration directory contains 992 SQL lines. The resulting fresh schema dumps to about 293 lines with 33 tables and 23 explicit indexes. The mechanical opportunity is therefore about 699 production SQL lines and 40 files.

The current migration tests spend roughly 659 lines proving particular historical rewrites. Some current-schema constraint assertions should survive, but approximately 450–550 lines can disappear with the old chain.

Safe cutover condition:

- all deployed nodes can be rebuilt from the baseline, or all three databases are converted during one controlled lockstep release;
- no supported installation may start from an older schema and upgrade through repository history.

If data must be preserved, use one temporary release/tooling step to convert it, verify the current schema, then remove the bridge and old files. Do not carry 41 permanent migrations merely to remember a deployment path that no supported node will use.

The final state should still use a file-backed baseline and `schema_migrations` (or a single schema version) so future changes remain deterministic. This is a squash, not a switch to ad hoc Go DDL.

### B. Cut `requirements.md` by deletion, not by adding documents

Current section sizes show where the bloat is:

| Section | Words | Problem |
|---|---:|---|
| §3 data model | 2,416 | transitions, security, presentation, and rationale embedded in row descriptions |
| §8 imported actions | 1,980 | OAuth model repeated across model, import, call, and CLI rules |
| §9 adapters/native | 2,069 | schemas and operational details repeat bootstrap descriptors |
| §12 auth/bootstrap | 1,558 | endpoint scripts repeat API behavior |
| §13 federation | 7,433 | protocol, state machine, rationale, and operational UI mixed together |
| §14 CLI/HTTP | 3,105 | endpoint inventory overlaps `API.md` and handler behavior |
| §15 required tests | 6,090 | restates requirements as test names |
| §16 rationale | 992 | restates prior sections again |

Recommended single-document form:

1. principles and vocabulary;
2. entities: fields plus row invariants only;
3. authorization equations;
4. local state transitions and atomic write sets;
5. adapter contracts and the exact supported schema grammar;
6. federation messages and state transitions;
7. externally observable CLI/HTTP requirements not already specified by `API.md`;
8. short verification policy.

Immediate deletions:

- Replace §15's 414-line prose test inventory with about 10–15 lines requiring unit, store, HTTP, real-network-gated, and end-to-end coverage traceable to the normative sections.
- Delete §16 after moving only the six principles above to the introduction.
- In §3, keep one compact field table and row checks. Delete operation behavior from the cells.
- In §13, use message/state tables instead of repeating the same signature, identity, retry, and settlement laws in prose.
- In §14, refer to `API.md` for endpoint shapes; retain only kernel-specific authorization, output, and operational rules.
- Refer directly to RFC 8785 JCS, Ed25519, OAuth authorization-code + PKCE, JSON Schema, OpenAPI, and libp2p behavior where the standard is the whole rule. State only Juice's profile, deviations, and additional invariants.

Target: 15,000–18,000 words in the same file, a 40–50% reduction. This does not require stable rule IDs, ADRs, another conformance document, or generated prose.

### C. Delete duplicate and test-shaped APIs

High-confidence candidates, subject only to confirming that the Go package has no external compatibility promise:

| API | Evidence | Replacement |
|---|---|---|
| `AttachBearerGrant`, `CreateGrant`, `RevokeGrant` | no production callers; tests only | selector/reconcile grant operations; full selector handles one action |
| `ListAllTransactionViews` | tests only | normal `ListTransactions` with superuser scope |
| supporting `ListAllTransactions` | only needed by the duplicate view path if final caller check confirms | filtered normal listing |
| `ResetActionStats` | no callers | existing lifecycle/compound operation |
| `Kernel.ResetRunningSteps` | tests only; recovery calls Store directly | `Recover` or Store test at its owning boundary |
| Store/DB `ReadRootTrace` | tests only | normal trace/process outcome or package-local SQL in Store tests |
| Store/DB `ListTraces` | tests only | public list/outcome assertion or package-local SQL in Store tests |
| DB `ExecForTest` | production method used by one test | package-local test access/helper |
| `LoginWithRefresh` | tests only | exercise authorization-code/refresh primitives or local setup |
| direct password `Login` | supports a branch absent from requirements but still listed in `API.md`; CLI does not use it | PKCE authorization-code exchange, after compatibility decision |

The purpose is not merely LOC. Each exported method is another apparent supported kernel path. Removing ten to fifteen such methods makes the allowed state machine easier to enumerate.

### D. Remove contract narration from source without deleting useful reasoning

Production contains 3,933 line-comment lines; 2,114 are in `kernel` and 1,048 in `cmd/juice`. The goal is not comment-free code.

Keep comments that answer one of these questions:

- why is this transaction atomic?
- what lock/order prevents a race?
- why is a field snapshotted rather than derived?
- exactly what bytes are signed?
- why is an apparently reachable state impossible?
- why does a compatibility exception remain?

Delete comments that merely:

- restate the function body;
- repeat a requirement section and its rationale;
- narrate version history after migrations are squashed;
- explain every response field already named by its type/tag;
- cite §13 without adding implementation-specific reasoning;
- describe test intent already clear from the test name and assertion.

A budget of 900–1,400 deleted production comment lines is realistic. Review it separately from executable LOC so a comment-only patch cannot masquerade as kernel simplification.

## Target kernel shape

No new architecture is needed. The desired shape is the current good spine with accidental alternatives removed:

```text
CLI -> one generic HTTP client -> thin HTTP adapter
                               -> response projection where required
                               -> narrow Kernel operation
                               -> one compound Store transition

WASM/native/HTTP/federation execution -> one private funded-call engine
                                      -> one settlement path
                                      -> one receipt/signature path
```

Rules for the reduced result:

- one operation owns each invariant;
- one public path per capability;
- no production test helpers;
- no history retained after its deployment compatibility obligation ends;
- no generic framework unless it deletes at least three existing, truly identical implementations;
- no “helper” that merely moves an existing block to another file;
- every refactor reports net repository lines and net files, including tests;
- any change that broadens accepted input or runtime behavior is rejected as feature work.

## Work order

This order is deliberately deletion-first and keeps optional scope decisions isolated.

1. **Migration baseline:** confirm the three-node cutover condition; squash 41 migrations; retain current-schema tests and delete historical migration tests. Expected largest file-count reduction.
2. **Dead/test-shaped surface:** remove unused Kernel/Store methods, production fakes, and tests of fakes; remove the direct password-token path only after reconciling the requirements/API compatibility conflict.
3. **Localized adapter consolidation:** collapse Ollama request plumbing, compact native schema literals/registration, inline pure service forwarders, and consolidate only repeated simple command/handler shapes.
4. **Narrative deletion:** reduce duplicated source comments after the executable paths settle. Count comment deletion separately.
5. **Requirements rewrite:** delete §15/§16 repetition, compress §3/§13/§14, and keep one contract plus `API.md`.
6. **Optional OpenAPI decision:** only if a larger executable cut is still required, decide JSON-only/no-`$ref`/minimal-schema compatibility explicitly, then delete the rejected forms and their tests.
7. **Separate correctness commit(s):** the one-line remote-BPS fix and one SQL admission predicate can accompany touched code. JCS conformance, exhaustive checked arithmetic, recovery redesign, and transport resource budgets remain visible but outside the reduction claim because they add code.

## Acceptance criteria for the reduction

A proposed implementation of this report is successful only if:

- no new user-visible capability, package, or execution path is added;
- production Go + SQL is net smaller after formatting;
- total Go + SQL, including tests, is net smaller rather than shifted from production into tests;
- file count falls materially through migration/test-utility deletion;
- the current `go test ./...` and `go vet ./...` baseline still passes after obsolete tests are removed;
- every remaining production Go file retains its corresponding test file;
- current money, authorization, execution, step, receipt, and federation behavior remains covered;
- any OpenAPI/schema narrowing is approved as a contract change before code is deleted;
- `pricedStore`, compound monetary Store operations, and the single call/settlement spine remain intact.

## Final judgment

The kernel does not need more architecture. It needs less surface around the architecture it already has.

The high-value reduction is not a wholesale rewrite of `kernel` or `store`: it is the removal of obsolete schema history, duplicate contract prose, production test utilities, dead public methods, a second login path, single-action grant variants, repeated Ollama transport code, verbose native schema literals, and trivial service wrappers. Those cuts make the supported state machine smaller without replacing explicit critical code with generic machinery.

After those changes, the remaining size is mostly the cost of the current product contract. If substantially more executable reduction is required, the decision must be made at the contract level—most plausibly by narrowing the OpenAPI/schema profile—not disguised as a refactor.
