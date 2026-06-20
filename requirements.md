# Juice Kernel Requirements

Version: 0.4
Status: implementation requirements
Codename: `juice`

## 1. Execution, supervision, and role law

Juice is a Go production kernel and research platform for callable actions. Execution starts with:

```text
run(action, args)        // action is @owner/name
```

which atomically creates a process funded with exactly `action.price` (the subtree bound, §6), locked from the caller's available balance, creates and funds the root trace from that process, and issues the root call. Its sole dispatch primitive is:

```text
Call(caller, trace, action, args)
```

The `trace` carries the funding wallet, process (`trace.process_id`), and causal parent — so no separate process argument is needed. Every `run` is a `Call` on a freshly created and funded root trace; the process closes automatically when the root call has returned and no Steps remain outstanding (§10).

For every call, define:

```text
P = process.owner_user_id      // process owner; payer
C = caller                     // call caller; immediate requester
A = action.owner_user_id       // action owner; payee on success
```

The transaction created by that call records:

```text
owner_user_id  = P
caller_user_id = C
target_user_id = A
```

These meanings are fixed. In a transaction, `owner_user_id` is the process owner, not the call caller; `caller_user_id` is the immediate requester, not necessarily the process owner (for a root call they coincide: the `run` requester becomes the process owner); `target_user_id` is the called action owner.

| Case                       | `P`                       | `C`                     | `A`                                 |
| -------------------------- | ------------------------- | ----------------------- | ----------------------------------- |
| Root call (`run`)          | process owner             | authenticated requester (= P) | called action owner           |
| WASM subcall               | parent process owner      | parent action owner     | subcalled action owner              |
| Step completion            | step's process owner      | required_caller_user_id | step's next action owner            |
| Remote proxy call          | local process owner       | local call caller       | local remote-peer user owning proxy |
| `@sys/make` worker subcall | requester's process owner | `@sys`                  | worker action owner                 |

All execution paths use `Call()`: root calls (via `run`), native actions, WASM `juice.call`, step completion, OpenAPI-imported HTTP actions, and remote proxies. `Call()` dispatches by `action.kind`, not by action-owner identity.

Supervision operations never route through `Call()`; they manage users, actions, processes, ratings, deposits, OpenAPI imports, and federation peering. Execution code must not rate outputs or propagate ratings.

Juice is meant to be a kernel like an OS kernel: only minimal but general and robust primitives, the rest lives on the application layer (actions).

## 2. Packages and implementation constraints

Use Go. `go build ./...` and `go test ./...` must pass. Replaceable modules use ordinary Go interfaces. `kernel` must not import CLI, HTTP, SQLite, wazero, or Ollama implementations.

Production packages:

```text
cmd/juice/   CLI and server entrypoint
kernel/      core objects and operational semantics
store/       persistence interface and SQLite implementation
script/      WebAssembly execution
llm/         local language and embedding interface
log/         structured logging
native/      native function implementations
```

Package names such as `sqlite`, `wazero`, and `ollama` are forbidden. Implementation-specific names may appear in concrete types or filenames. Keep package and source-file counts small. Do not split files for size alone. Every production source file must have a corresponding `_test.go` file with independent tests for its logic.

## 3. Data model

All IDs are stable opaque identifiers. Action IDs are globally unique. Credit balances and prices are non-negative indivisible integers.

| Object              | Fields                                                                                                                                                                                                                                                                           | Rules                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `User`              | `id`, `handle`, `email`, `available`, `locked`, `suspended_at`, `denied_at`, `public_key`, `remote_base_url`, `created_at`, `updated_at`                                                                                                                                         | `handle` is unique and contains no `/`. Suspended users are rejected at every authenticated request with `ErrUnauthenticated`. `public_key`, when set, is a unique base64url Ed25519 32-byte public key. Every user is local (null `public_key`/`remote_base_url`; authenticates by password or token) or a proxy (both set; authenticates only by federation signature, per request; §13). The kind is fixed at creation; proxy users cannot log in, hold tokens, or be created by `user create`. `denied_at` marks an unfriended peer key whose requests are rejected (§13).                                                                                                                                                                                                                                          |
| `Action`            | `id`, `owner_user_id`, `name`, `kind`, `active`, `public`, `price`, `description`, `input_schema`, `output_schema`, `source`, `auth_json`, `artifact_hash`, `remote_action_id`, `created_at`, `updated_at`                                                                       | `owner_user_id` is the action owner. `kind ∈ {http, wasm, native, remote_proxy}`. `(owner_user_id,name)` is unique. `/` is allowed in `name`; handles cannot contain `/`, so `@owner/name` is unambiguous. Inactive actions are not callable. `GET /v1/actions` unauthenticated returns active public actions; authenticated returns active public actions plus the caller's own active actions (union, deduplicated by `id`). Action owners may list all their own actions regardless of `active` or `public` via the `?owner=` filter when it resolves to themselves. Authorized users may inspect script source. `artifact_hash` content-addresses compiled artifacts. `auth_json` is the write-only upstream credential config, encrypted at rest, never returned by any read path (§8). For `remote_proxy`, `source` is the federation call URL, `remote_action_id` is the action ID on the remote kernel, and `artifact_hash` stores the signed manifest hash. Active actions require non-empty natural-language `description`, valid schemas, and schema field descriptions sufficient for lookup and LLM function calling. |
| `Process`           | `id`, `owner_user_id`, `available`, `locked`, `status`, `created_at`, `ended_at`                                                                                                                                                                                                 | `owner_user_id` is the process owner and payer. `status ∈ {open,closed}`. A process is created by `run`, funded with exactly the root action's price, parked from the owner's `available` into the owner's `locked` (§6); the process holds it as `available`. It is bijective with its root trace and exists as a longer-lived wallet only because traces settle eagerly (§6): it absorbs refunds destined for already-settled traces and holds parked steps. Enforcement is per call, on the call's trace (§6); the process's `available + locked` is the total held across its calls' wallets and parked steps. It closes automatically when the root call has returned and no Steps of the process are outstanding; closing returns remaining funds to the process owner and releases the owner's lock. Closed processes cannot call.                                                                                                                                                                              |
| `Trace`             | `id`, `process_id`, `parent_trace_id`, `action_owner_id`, `available`, `locked`, `idempotency_key`, `dispatch_json`, `created_at`                                                                                                                                  | `action_owner_id` is the action owner of the action executing in the trace; used for trace-scoped process authority. `process_id` is denormalized (derivable by walking `parent_trace_id` to the root). Root traces have null parent. Every `Call()` creates exactly one child trace. A trace is the call's wallet: `available` starts as the action's price at entry and is the call's remaining allocation; `locked` is what the call has committed to its direct subcalls and steps. Calling something of price `q` requires `available ≥ q` and moves `q` from this trace's `available` into its `locked`, becoming the callee's `available` (§6). Settlement pays out the trace's remaining `available` (§6). A call's own latency is `transaction.ended_at − transaction.started_at`; there is no cached latency field (§11). `idempotency_key` and `dispatch_json` are null except on a remote-proxy trace, where the outbound key and request payload are recorded atomically with dispatch; while set and unsettled, the call is awaiting its receipt and restart resumes its retry (§5, §13). |
| `Transaction`       | `id`, `process_id`, `trace_id`, `parent_trace_id`, `owner_user_id`, `caller_user_id`, `target_user_id`, `action_id`, `action_name`, `args_json`, `reply_json`, `status`, `gross`, `net`, `fee`, `reason`, `remote_receipt_hash`, `remote_receipt_json`, `started_at`, `ended_at` | `status ∈ {success,failure}`. Every attempted call creates one immutable transaction. Fields obey the role law. `action_name` is captured at creation so history remains self-contained after action deletion. Local calls have null remote receipt fields. Remote-proxy commits atomically store full remote receipt JSON and `SHA-256(remote_receipt_json)`.                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `Stats`             | `uses`, `successes`, `failures`, `rating_count`, `latency_estimate`, `rating_estimate`, `last_used_at`                                                                                                                                                                           | Missing stats have defined defaults. `uses = successes + failures`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `StatTag`           | `action_id`, `key`, `value`, `source`, `updated_at`                                                                                                                                                                                                                              | Optional lookup-experiment data, namespaced by source, never execution semantics.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `Step`              | `id`, `parent_trace_id`, `required_caller_user_id`, `action_id`, `price`, `partial_args`, `status`, `tx_id`, `created_at`                                                       | `status ∈ {waiting, running, done, cancelled}`. `parent_trace_id` is the creating/funding trace and derives the process (`Trace(parent_trace_id).process_id`); it is inherited by the completion trace. `required_caller_user_id` is mandatory; open completion is not supported. `partial_args` is pre-bound input merged with the caller-supplied input at completion (`input` overwrites `partial_args` on key collision). The completer's allowed input is derived as `action.input_schema \ keys(partial_args)`, not stored (§10); this is safe because changing an action's schema deactivates it and completing against a changed or deactivated action resets the step to `waiting` (§10), so the derivation never sees a moving target. `tx_id` is recorded atomically when status transitions to `done`. `price` is `action.price` snapshotted at step creation: the amount parked in the process's `locked`, the completion call's allocation and `gross`, spent when the step completes and refunded if it is cancelled. `EndProcess` atomically cancels all `waiting` steps tied to the process in the same transaction as closure; `cancelled` is terminal and carries no `tx_id`. An outstanding (`waiting` or `running`) step keeps its process open, its allocation parked in the process's `locked` (§10). |
| `Deposit`           | `id`, `operator_user_id`, `target_user_id`, `amount`, `reason`, `created_at`                                                                                                                                                                                                     | Immutable audit record for a positive out-of-band superuser credit grant.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `Withdrawal`        | `id`, `operator_user_id`, `target_user_id`, `amount`, `reason`, `created_at`                                                                                                                                                                                                     | Immutable audit record for a positive superuser credit redemption obliging an out-of-band payout (§12).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `Receipt`           | `id`, `issuer_user_id`, `tx_id`, `trace_id`, `action_id`, `caller_user_id`, `process_id`, `args_hash`, `reply_hash`, `status`, `gross`, `net`, `fee`, `charge`, `reason`, `started_at`, `created_at`, `signature`                                                                | Immutable signed record for exactly one committed call. `caller_user_id` is the call caller. `started_at` is call start; `created_at` is settlement. `charge` is the amount actually drawn from the caller's funds: `= gross` on success, `≤ gross` on failure (settled descendants stay paid, §6), `0` on rejection.                                                                                                                                                                                                                                                                  |
| `Rating`            | `id`, `rated_tx_id`, `rated_receipt_id`, `rater_user_id`, `rating`, `note`, `created_at`, `signature`                                                                                                                                                                            | Immutable signed feedback record. `rating ∈ {0,1}`. At most one rating exists per transaction. `rated_receipt_id` may be null only for pre-receipt transactions. `note` is optional, nullable, human-readable, and included in the single Ed25519 rating signature payload.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `IdempotencyRecord` | `id`, `idempotency_key`, `counterparty_user_id`, `receipt_id`, `status`, `result_json`, `created_at`, `expires_at`                                                                                                                                                               | Cross-kernel only. `status ∈ {pending,complete}`. Insert pending before execution; complete atomically with transaction and receipt. Completion stores `result_json`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `DiscoveredKernel`  | `public_key`, `handle`, `base_url`, `introduced_by`, `stats_json`, `first_seen`, `updated_at`                                                                                                                                                                                    | One row per (kernel, introducer); accumulated from gossip (§13). Information only — never execution semantics, callability, pricing, or settlement.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

OpenAPI registration and federation create or update ordinary `Action` rows. They create no durable object parallel to `Action`.

## 4. Authorization, call validity, and traces

```text
CanCall(P, a) := active(a) ∧ (public(a) ∨ P = a.owner_user_id)
```

`CanCall` is about the process owner `P`, not the call caller `C`. Public actions are callable by any process owner. Private actions are callable only when the process owner is the action owner. The call caller may differ from both only if process-use authority permits it.

Check call preconditions in this exact order and return the typed error for the first failure:

```text
1. C is authenticated and not suspended
2. process exists and is open
3. supplied parent_trace_id, if any, exists and belongs to the process
4. C may use process:
   C = P, or
   supplied parent trace has action_owner_id = C, or
   for a step completion, C = step.required_caller_user_id (§10)
5. action exists
6. CanCall(P, action)
7. args satisfy action.input_schema
8. funds: the passed trace's `available` >= action.price (§6) — for a root call this is the root trace `run` funded from the process; a step completion is funded by its parked price instead (§10)
```

Precondition 4 controls spending authority over the process. Precondition 6 controls action access by the process owner.

Trace relation:

| Case                  | `parent_trace_id`      | `process_id`               |
| --------------------- | ---------------------- | -------------------------- |
| Root trace            | null                   | owning process             |
| Subcall trace         | executing action trace | same as parent             |
| Step-completion trace | step.parent_trace_id   | Trace(step.parent_trace_id).process_id |

`process_id` determines payment. `parent_trace_id` records causality only. There is no cached per-trace latency; a call's own latency is its transaction's elapsed time and subtree latency is a query over descendant transactions (§11).

## 5. Persistence and atomicity

Use file-backed SQLite with WAL by default. Migrations are deterministic and stored in the repository. Tests use temporary SQLite databases. No production feature may depend on an in-memory-only store. `kernel` depends on a store interface, never SQLite.

Store interface. Each monetary transition commits together with its audit record (transaction, receipt, stats, step/idempotency state) inside one store call, so the atomic write-sets below are atomic at the store boundary rather than coordinated above it. The money-path methods are therefore **compound atomic operations**, not fine-grained primitives; reads and supervision are ordinary methods. The listing is representative, not exhaustive:

```text
Money paths (each commits a monetary transition + its audit record atomically):
  BeginRun            user-wallet park + process creation/funding + root trace funded
  BeginSubcall        parent-trace move (available → locked) + child trace funded
  BeginStepCall       unpark step.price + completion trace funded + step waiting → running
  CommitCall          success transaction + receipt + payout + lock release + stats (+ step/idempotency)
  CommitFailedCall    failure transaction + receipt + subtree refund/cancellation + stats (+ step/idempotency)
  CommitRemoteSettlement  remote-proxy settlement on a signed receipt (charge/duty/refund) + transaction + receipt
  EndProcess          cancel waiting steps + return funds + close process
  CreateStep          step record + park step.price from the creating trace
  CreateDeposit CreateWithdrawal  out-of-band credit grant / redemption with its audit record
  CreateRatingAndUpdateStats  rating record + rating stats
Reads / supervision (no monetary mutation):
  CreateUser ReadUser ReadUserByHandle ReadUserByPublicKey ListUsers SuspendUser UnsuspendUser UpdateUser
  CreateAction ReadAction ReadActionByOwnerName UpdateAction UpdateActionAndResetStats DeleteAction ListAllActions
  ReadProcess ListProcesses ListAllProcesses
  ReadTrace ReadRootTrace ListTraces
  ReadTransaction ListTransactions ListAllTransactions
  ReadStats UpsertStats
  ReadStep ListSteps ResetStepAndRepark ResetRunningSteps
  ReadReceipt ReadReceiptByTxID
  ReadRatingByTxID ListRatings
  InsertPendingIdempotencyRecord ReadIdempotencyRecord CompleteIdempotencyRecordIfPending DeleteIdempotencyRecord
  GetConfig SetConfig InitFirstBoot
  CreateOrUpdateDiscoveredKernel ListDiscoveredKernels DenyPeerCascade CreateProxyUser
```

`UpdateTransaction` is forbidden. Transactions are immutable after creation.

Atomic write sets:

| Operation       | Atomic writes                                                                                    |
| --------------- | ------------------------------------------------------------------------------------------------ |
| run             | user-wallet park (available → locked), process creation and funding, root trace creation funded from the process; the root `Call` then dispatches on it (§6) |
| Call entry      | caller-wallet move (available → locked), child trace creation with its allocation                |
| Successful call | transaction, receipt, payout of the trace's available (target net, platform fee), caller lock release, metrics, stats |
| Failed call     | transaction, receipt, subtree rollup (refund to caller's available, cancellation of outstanding steps beneath), caller lock release, metrics, stats |
| Process closure | process closed, remaining funds returned to process owner                                        |
| Deposit         | user credit, deposit record                                                                      |
| Withdrawal      | user debit, withdrawal record                                                                    |
| Rating          | rating record                                                                                    |
| User update     | user email and/or password hash                                                                  |
| Step creation   | creator-trace move (available → locked), step record                                             |
| Step completion | step status→done, tx_id recorded, price unparked, call transaction, receipt, settlement, metrics, stats |
| Step cancellation | step status→cancelled, parked price returned                                                   |

A monetary transition and its audit record must commit or fail together.

Recovery is atomicity's crash-side guarantee. At startup, every trace without a transaction was mid-execution at shutdown and can never return: it is settled as a failure with `reason = interrupted`, deepest first, applying the normal refund rollup (§6) — settled descendants stay settled, refunds flow up the chain, and processes then close by the automatic rule (§3). Exception: a remote-proxy trace with a recorded dispatch is not interrupted — it resumes retrying with its stored `idempotency_key` until a signed receipt settles it (§13); its allocation stays locked and its process stays open. Steps with `status = running AND tx_id IS NULL` are reset to `status = waiting`, their interrupted completion call's allocation returned to the step's parking; `waiting` steps are untouched — their parked prices and open processes survive restarts. Recovery is idempotent.

## 6. Call transition and settlement

`action.price` is a **subtree bound**: the maximum total cost of the call and everything it calls, advertised worst-case by the provider. The caller pays at most `price` for the whole tree under the call.

Every wallet in the chain — user, process, trace — has `available` and `locked`, and money moves the same way at every level: `run` parks the price in the user's `locked`, the process holds it as `available`, and the root trace is funded from the process — the same caller/child move as every call; process closure releases the user's lock, returning remaining funds to `available`. Thereafter a subcall moves the price from the parent trace into the child trace. Every `Call` is funded from the trace it is handed.

For `q = action.price`, every `Call` runs:

```text
require trace.available >= q                  // the trace handed to Call (root trace pre-funded by run)
trace.available -= q;  trace.locked += q
create child trace with action_owner_id = A, available = q, locked = 0
execute action by dispatching on action.kind
validate output against action.output_schema
on success: commit transaction + receipt + settlement + metrics + stats
on failure: commit transaction + receipt + refund + metrics + stats
return result, tx_id, trace_id
```

A child's resolution always releases the caller's lock — `locked` holds only outstanding commitments:

```text
child succeeds:   pay out child.available;              caller.locked -= q
child fails:      caller.available += refunded amount;  caller.locked -= q
step created (price p):   creator.available -= p;  creator.locked += p     // parked
step resolved:            creator.locked -= p     // complete or cancelled (§10)
```

Zero-credit processes may execute zero-price actions.

**Settlement (success).** The call's remaining `available` — what it did not commit to subcalls and steps — is its value added, and is paid out:

```text
taxable = trace.available
fee     = (taxable * fee_bps + 9_999) / 10_000
net     = taxable - fee
```

`target_user_id` is credited `net`; `@sys` is credited `fee`. Unused budget is the provider's margin, not a refund: `price` is a price, not a metered estimate. Negative value added is structurally impossible (`available ≥ 0`). Default `fee_bps = 2000`. Fee recipient is fixed as `@sys`. Each kernel taxes only its own layer; remote subcalls are subject to the remote kernel's fee policy independently. Exception: `kind = remote_proxy` settles per §13's receipt rule; all other kinds settle as above.

**Refund (failure).** A failed call is rolled up entirely: its remaining `available`, plus the parked prices of all its outstanding steps — and, recursively, everything outstanding beneath them — is cancelled and returned to the caller's `available` (for a root call, to the process, and from there to the owner at closure). A refund whose destination trace has already settled goes to the process instead, and from there to the owner at closure: settlement is final, a trace never regains `available`. The failed call charges zero fee and net, records `status=failure`, and exposes the failure class in `reason`. Already-settled subcalls inside the failed call stay settled — their providers were paid from money the call had already spent.

Schemas exist for every action. Unsupported JSON Schema subset forms fail action creation or update. A schema node without `type` is unconstrained; this is intentional and not an error. Validate input before locking and output before successful settlement.

Subcall law:

```text
juice.call(target_action,args) from parent_action in parent_trace
= Call(parent_action.owner_user_id, parent_trace, target_action, args)
```

Subcall transaction:

```text
owner_user_id  = parent_trace.process.owner_user_id
caller_user_id = parent_action.owner_user_id
target_user_id = target_action.owner_user_id
```

No ephemeral process is created. All subcalls spend from their parent call's trace within the same process. Trace-scoped process authority requires `Trace(parent_trace_id).action_owner_id = caller_user_id`. Settled subcall costs persist even if an ancestor later fails. A subcall whose price exceeds the parent's `available` fails with `ErrInsufficientFunds`; the parent decides whether to propagate failure.

## 7. Action lifecycle

`CreateAction`: inactive by default. Validate action owner, name, kind, non-negative price. `description`, schemas, and source are required at activation. WASM creation validates or compiles only when an executor is configured. HTTP creation validates endpoint configuration without calling it unless requested. Reject non-HTTP(S), loopback, RFC 1918 private, and link-local `169.254.x.x` source URLs at creation and activation. Normal `CreateAction` rejects `kind=native`.

`RegisterNativeAction`: bootstrap-only; caller is responsible for `@sys` ownership.

`Activate`: action-owner authority; initialize stats if absent; reject invalid schema, missing source, invalid artifact, unsafe URL, or invalid runtime.

`Update`: action-owner authority; changing source, schema, kind, price, or endpoint deactivates unless explicitly safe; recompute WASM `artifact_hash`; reject unsafe HTTP source URLs; preserve historical transaction source/hash.

`Delete`: action-owner authority; disable discovery and preserve history; soft deletion permitted.

Native actions are bootstrap-registered, owned by `@sys`, not user-creatable/updatable/deletable, and callable only through `Call()`.

## 8. Imported actions

Imported actions are ordinary `Action` rows. Import is supervision. Execution remains through `Call()`.

Shared reconciliation: import is idempotent over its match key. Reimport compares contract fields. Unchanged contracts preserve active state and local stats. Changed contracts update, deactivate, refresh lookup data, reset current stats, and preserve `Action.id`. Removed or no-longer-callable imported operations deactivate and reset stats. Unimport deactivates. Import and unimport are scoped by import provenance and match key. They must not affect manual actions, actions imported from another server, or actions imported through another mechanism. They never delete transaction, receipt, rating, or trace history. Stat reset writes missing-stat defaults only; it does not alter transactions, receipts, ratings, or trace history.

### OpenAPI

`juice action import --openapi <spec-url>` imports representable HTTP operations as inactive `kind=http`, `source.type=openapi` actions owned by the importer:

```text
Call(args: JSON object) -> JSON object
```

Allowed methods: GET, POST, PUT, PATCH, DELETE. Method is stored in `Action.source`, not `Call()` semantics.

Required or rejected/kept inactive with validation messages: `operationId` or `x-juice-name`; `description` or `summary`; parameters and/or requestBody schema; 2xx JSON response schema; optional `x-juice-price` defaulting to 0. Path, query, and JSON body fields compile into one canonical `input_schema`; selected 2xx JSON response schema becomes `output_schema`.

Never active: non-JSON responses, streaming responses, multipart uploads, ambiguous success schemas, unsupported authentication, unsafe URLs, and invalid schemas. Invalid schemas and unsafe source URLs are rejected at import time, reported in the rejection list, and not stored inactive. Ratings and stats are never imported.

Draft import needs no API ownership proof. Public activation requires proof by well-known challenge, challenge in the OpenAPI document, or verified credential.

Authentication to upstream APIs is per-action: the importer stores an auth config in a dedicated write-only `auth_json` column — `{scheme, config, secrets}` — stored AES-256-GCM encrypted at rest; applied by a replaceable authenticator adapter at HTTP dispatch (§9). Secrets are write-only: never returned by any read path, never visible to scripts, never present in args, replies, logs, receipts, hashes, or manifests, and excluded from contract comparison. Implemented schemes: `header` (static header), `query` (query parameter), `bearer` (Authorization: Bearer), `basic` (HTTP Basic Auth). Deferred schemes: OAuth2 client-credentials with token caching, HMAC request signing — actions requiring them are imported inactive. Per-caller delegated authentication (e.g. OAuth authorization-code) is out of scope. Operations requiring it remain never-active.

OpenAPI provenance:

```json
{
  "type": "openapi",
  "spec_url": "...",
  "base_url": "...",
  "method": "...",
  "path": "...",
  "operation_key": "...",
  "operation_hash": "..."
}
```

`operation_key = x-juice-name || operationId || canonical(method,path)`. Match key:

```text
action.owner_user_id + source.type + source.spec_url + source.operation_key
```

Contract fields: description, method, path, parameter bindings, input schema, output schema, selected response, price, execution source. `operation_hash` excludes stats, ratings, timestamps, and formatting. Manual name collision rejects import. OpenAPI unimport matches `owner_user_id + source.type=openapi + source.spec_url`, and optionally `Action.name` or `source.operation_key`.

Inbound webhook payloads enter through the standard authenticated call path: external systems authenticate as registered users with bearer tokens and call `POST /v1/run` directly, or complete a pre-created step via `POST /v1/steps/{id}/complete`. No special webhook-registration endpoint exists in the kernel.

### Remote

`peer friend` fetches the remote's active public actions, verifies each signed manifest, and creates or updates local `kind=remote_proxy` actions owned by the local remote-peer user row. Imported actions are immediately enabled and public. It does not copy implementation.

Manifest required fields:

```text
action_id owner_handle name description input_schema output_schema price kind
artifact_hash stats updated_at signature
```

Only active public remote actions have manifests. Manifest descriptions and schemas are the canonical interface used by importing kernels for lookup and LLM function calling. `signature` is the remote platform Ed25519 signature over canonical JSON excluding `signature`, verified against the remote peer's `public_key`. Match key:

```text
proxy.owner_user_id + proxy.remote_action_id
```

Remote contract fields:

```text
action_id artifact_hash description input_schema kind name output_schema owner_handle price
```

Manifest stats and `updated_at` do not affect contract comparison; manifest stats never overwrite local `Stats` and are not stored as `StatTag`. Invalid signatures skip that action. A second `peer friend` re-syncs: new actions are imported, changed-contract actions are updated (re-enabled), and actions no longer active/public on the remote are deactivated and their stats reset. `peer unfriend` deactivates all proxies from that peer and preserves all history.

The proxy's local `price` is `manifest.price` plus the worst-case import duty: `price = mp + ceil(mp * import_bps / 10000)` (§13). The local caller sees one price bounding the whole remote call, duty included; settlement charges duty on the actual remote charge and refunds the difference (§13). A change of the local `import_bps` recomputes proxy prices but is not a manifest contract change and does not deactivate.

## 9. Adapters, native actions, and stats

WASM uses wazero. Scripts receive no ambient filesystem, network, environment, process access, or raw user tokens. They receive only explicit host functions, each execution having memory limit, timeout, deterministic context cancellation, and artifact-hash compiled-module cache. Store source and artifact; authorized users may inspect source; activation should precompile; compilation failures are typed.

Host surface:

```text
juice.call          subcall under §6
juice.step_create   create a waiting step under §10; returns step_id
juice.step_complete complete a waiting step under §10; returns {result, tx_id, trace_id}
juice.log           structured trace log
```

Script authority:

```text
ScriptAuthority ⊆ KernelAuthority(trace, process, subject)
```

`llm` exposes replaceable `Embed(ctx,text)->vector` and `Chat(ctx,messages)->message`; concrete adapters call Ollama, but `kernel` must not import them. Defaults (configurable in `juice.json` under `native.llm`, §14): URL `http://localhost:11434`, chat `gemma4:26b`, embedding `nomic-embed-text`. Tests use fakes.

Upstream authentication is a replaceable adapter: `Authenticator.Apply(request, auth) -> request` transforms an outbound HTTP request using the action's stored auth config (§8); schemes are implementations behind this interface, and `kernel` must not import them. Tests use fakes.

Native actions are standard actions shipped alongside the kernel as a platform stdlib. They have no special kernel privileges — any provider could have supplied equivalent actions as HTTP or WASM actions. They are registered at bootstrap under `@sys`, interact with the platform only through injected dependencies and the same `Call()` / `CreateStep()` entry points available to all actions, and never extend the kernel's internal interfaces on their own behalf.

| Action          | Rules                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `@sys/lookup`   | Public; action owner `@sys`; price 0 (configurable, `native.lookup`, §14); callable only through `Call()`. Rank active actions by tested formula combining semantic similarity and stats. Replaceable ranking storage; brute-force cosine acceptable. Input: required `query`, optional `limit=10`. Output: `results[]` with `action_id`, `action` (`@owner/name`), `description`, `score`, `input_schema`, `output_schema`. Direct lookup only for diagnostics, not user-facing APIs or WASM hosts.                                                                                                                                                                                                                                                                                                       |
| `@sys/llm/chat` | Public; action owner `@sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Input: `messages[]` of `{role,content}` plus optional `system`. Output: `message{role,content}`. `ErrInvalidState` if chat unconfigured.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `@sys/llm/embed` | Public; action owner `@sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Input: required `text` (string). Output: `embedding` (array of numbers). `ErrInvalidInput` if `text` is empty. `ErrInvalidState` if embedder unconfigured. |
| `@sys/llm/json` | Public; action owner `@sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Input: `messages[]` of `{role,content}`, optional `system`, required `output_schema`. Output: `value` (JSON value). Validates model output locally against `output_schema`. `ErrSchemaViolation` for unsupported schema. `ErrInvalidState` if structured output unavailable. `ErrExecutionFailed` if no valid JSON produced. |
| `@sys/llm/decide` | Public; action owner `@sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Given a conversation and a set of Juice actions, asks the LLM to select one and propose args — does not execute the call. Input: `messages[]` of `{role, content?, tool?}` where `role` is one of `system`, `user`, `assistant`, `tool` and `tool` is an optional object `{action?, args?, result?}`; required `actions[]` (list of `@owner/name`). Kernel fetches each action's canonical description, `input_schema`, and price from the DB. Output: `action` (`@owner/name`), `args` (validated against that action's `input_schema`), optional `message`. `ErrNotFound` if any action reference is unknown. `ErrInvalidState` if LLM tool calling unavailable. `ErrExecutionFailed` if no valid selection produced. |
| `@sys/make`     | Public; action owner `@sys`; price 20 (configurable, `native.make`, §14); callable through `Call()`. Synthesizes a WASM action from a natural-language description using the platform LLM and TinyGo compiler. Input required `description`; empty gives `ErrInvalidInput`; missing LLM/compiler gives `ErrInvalidState`. Up to `maxSteps=5` (configurable): derive contract, search catalog, generate TinyGo, compile, validate WASM imports/exports, smoke-test with stub host. Success registers and activates action under `C` of the `@sys/make` call. Return `status="success"`, `action_id`, `action_name`, `diagnostics`, `tests`. A name collision with a live action bumps a numeric suffix (`name`, `name-2`, … up to `name-100`) and registers under the first free name, mirroring proxy-handle allocation (§13); soft-deleted actions do not reserve a name. Synthesis failures use output failure status, not kernel errors. Worker subcalls follow role law: `owner_user_id=same process owner`, `caller_user_id=@sys`, `target_user_id=worker action owner`. |
| `@sys/time`     | Public; action owner `@sys`; price 0 (configurable, `native.time`, §14); callable through `Call()`. No input required. Output: `unix` (integer seconds since UTC epoch), `iso` (RFC 3339 string). |
| `@sys/sink`     | Public; action owner `@sys`; price 0 (configurable, `native.sink`, §14); callable through `Call()`. Accepts any input, returns `{}`. Universal no-op sink for steps that require an onward action but no further computation. |
| `@sys/message`  | Public; action owner `@sys`; price 0 (configurable, `native.message`, §14); callable through `Call()`. Sends a message to another platform user by creating a Step they must acknowledge. Input: required `to` (`@handle` of recipient), required `message`. Output: `step_id`. The Step sets `required_caller_user_id` to the resolved target user and `partial_args` to `{"message":"..."}` so the recipient can read it via `step list`. Uses `@sys/sink` as the step's `action`. `ErrInvalidInput` if `to` cannot be resolved. |
| `@sys/random`   | Public; action owner `@sys`; price 0 (configurable, `native.random`, §14); callable through `Call()`. No input required. Output: `value` (float in `[0, 1)`). Exists to provide randomness to WASM scripts, which have no ambient access to the OS random source. |
| `@sys/tinygo/compile` | Public; action owner `@sys`; price 5 (configurable, `native.tinygo`, §14); callable through `Call()`. Compiles author-supplied TinyGo to a WASM artifact using the platform TinyGo compiler, prepending the Juice WASM SDK so the author writes only `func Handle(in map[string]any) (map[string]any, error)` (the SDK owns `package`, imports, `alloc`, `run`, `main`). Input: required `source`. Output: `status` (`success`/`failure`), `artifact` (base64 WASM, on success), `artifact_hash` (SHA-256 hex, on success), `diagnostics` (array). Empty `source` gives `ErrInvalidInput`; an unavailable compiler toolchain gives `ErrInvalidState` (platform misconfiguration — the call fails and is not charged). Author compile errors and import/export-validation failures use output failure status, not kernel errors (so the attempt is charged), mirroring `@sys/make`. Registration is separate supervision: pass the returned artifact to `action create --kind wasm --artifact` (§14). |

Stats use:

```text
mean_(n+1) = mean_n + (x_(n+1)-mean_n)/(n+1)
```

`latency_estimate` is the arithmetic mean over completed calls, with denominator `uses`; each sample is the call's own elapsed time, `transaction.ended_at − transaction.started_at`. Buyer-experienced subtree latency (including step dormancy) is not a stat — it is a query over descendant transactions (§11).
`rating_estimate` is the arithmetic mean over rated calls only, with denominator `rating_count`.
There is no cost estimate: an action's all-in cost is its advertised `price` (§6), known in advance; lookup ranks on it directly.

## 10. Steps

A Step is a partially applied future Call: a suspended computation boundary that records enough context to resume when a caller later supplies the remaining input.

```text
Step {
  id
  parent_trace_id          // creating/funding trace; derives process; inherited by completion trace
  required_caller_user_id
  action_id
  price
  partial_args
  status           // waiting | running | done | cancelled
  tx_id
  created_at
}
```

Core invariant:

```text
CompleteStep(caller, id, input) = Call(caller, step.parent_trace_id, action_id, partial_args ⊕ input)
```

A step is funded at creation: `action.price` is snapshotted as `step.price` and moved from the creating trace's `available` into its `locked` (§6). The parked price is the completion call's allocation — completion never checks funds, because the money is already reserved. Cancellation returns the parked price (§6: with the creator's failure rollup, or at process closure to the process owner).

`partial_args ⊕ input` is a shallow object merge. Keys in `input` overwrite keys in `partial_args`. The completer's allowed input is `action.input_schema \ keys(partial_args)` — the action's input keys not already bound — derived live rather than stored; only those keys may appear in `input`. Final arguments are validated against `action.input_schema` by the underlying `Call`. Live derivation is safe because an action's schema cannot change under an active step: a schema change deactivates the action (§7), and a completion against a deactivated or contract-changed action resets the step to `waiting` (below).

`required_caller_user_id` is mandatory. Open completion is not supported.

Status states:

| State     | Meaning                                                                                            |
| --------- | -------------------------------------------------------------------------------------------------- |
| `waiting`   | Completable if its process is open. The only state from which `CompleteStep` may begin. Holds its parked price; keeps the process open.            |
| `running`   | Claimed; the resumed `Call` is executing. Prevents concurrent double-execution.                    |
| `done`      | The resumed `Call` finished. Success, failure, receipts, and settlement belong to the transaction. |
| `cancelled` | The process closed or the creating call failed before completion. Terminal; no `tx_id`; parked price returned. Not completable.           |

A step does not duplicate transaction state. Completion timing, result, and failure reason are obtained from the transaction referenced by `tx_id`.

A process closes automatically when its root call has returned and no steps are outstanding (§3). Forced closure (`EndProcess`) atomically cancels all `waiting` steps in the same transaction as closure, returning their parked prices to the process owner; a call's failure cancels its outstanding subtree (§6).

Startup recovery: `ResetRunningSteps` sets all steps with `status=running` and `tx_id=null` back to `status=waiting`. `cancelled` steps are never reset.

### Operations

`CreateStep(required_caller, trace, action, partial_args) -> step_id`: the creating authority is the trace's action owner (`Trace(trace).action_owner_id`), so in-execution creation is implicit. For external creation (`POST /v1/steps`) the service layer requires that the authenticated, non-suspended user be permitted to use `trace` (precondition 4 of §4) before invoking the primitive. The process is `Trace(trace).process_id` and must be open; `CanCall(process.owner_user_id, action)` must hold. Requires `Trace(trace).available ≥ action.price`; creation parks that amount from `trace` (§6), so the parked price is always the recorded payer's money. The completer's allowed input is derived (above), not supplied. Returns a `waiting` step.

`ReadStep(caller, id)`: requires `CanReadStep`.

`ListSteps(caller)`: returns steps visible to the caller per `CanListStep`, ordered by `created_at` descending. Optional filters: `process_id`, `status`.

`CompleteStep(caller, id, input)`:

```text
1. step exists
2. status = waiting
3. process exists and is open
4. caller = required_caller_user_id  (no superuser exception; IsSuperuser does not permit completing another user's step)
5. input satisfies the step's allowed input (`action.input_schema \ keys(partial_args)`)  (a violation rejects the completion and leaves the step waiting; it is never recorded as a failure of the action)
```

`ClaimStep` atomically transitions `waiting→running`. Then executes:

```text
Call(caller, step.parent_trace_id, action_id, partial_args ⊕ input)
```

The resumed `Call` takes the step's parked price as its allocation — `creator.locked -= p`, new trace `available = p` — in place of §6's trace-entry move; no availability check occurs. The completion call's allocation and transaction `gross` are `step.price`, not the action's current price. On `Call` completion (success or failure), atomically records `status=done` and `tx_id`. If `Call` rejects before creating a transaction (action deactivated, `CanCall` lost, malformed input), the step is reset to `waiting` — its parked price stays parked. Funds exhaustion cannot occur.

Completion transaction role law:

```text
owner_user_id  = step's process owner
caller_user_id = required_caller_user_id
target_user_id = action.owner_user_id
```

### Access rules

```text
CanListStep(u, k) :=
  u = Process(Trace(k.parent_trace_id).process_id).owner_user_id
  ∨ u = k.required_caller_user_id
  ∨ IsSuperuser(u)

CanReadStep(u, k) := CanListStep(u, k)
```

### WASM host functions

```text
juice.step_create(partial_args, required_caller_user_id, action_id) -> step_id
```

Creates a waiting step bound to the current process and current trace (as `parent_trace_id`). Arguments are JSON-encoded strings.

```text
juice.step_complete(step_id, input) -> {result, tx_id, trace_id}
```

Completes a waiting step. The executing action owner is used as the caller, matching `juice.call` subcall semantics. `input` is a JSON-encoded object.

### Webhook integration

External systems deliver inbound payloads through the standard authenticated call path. They register as users, obtain bearer tokens, and either:

- Call `POST /v1/run` directly with the webhook payload as `args`.
- Complete a pre-created step via `POST /v1/steps/{id}/complete` with the payload as `input`.

No special webhook-registration endpoint exists in the kernel.

## 11. Receipts, ratings, signatures, transaction access

Every transaction references a trace. Trace lookup by process returns the execution tree. Trace deletion must not remove transaction history. Latency is derived from transaction timestamps, not cached on the trace:

```text
own-call latency    = transaction.ended_at − transaction.started_at
buyer-experienced   = max(descendant.ended_at) − root.started_at   // a query over the subtree
```

Buyer-experienced (subtree) latency is wall time: it includes step dormancy (e.g. human approvals) and is always current — a query reflects late descendants the moment they complete, with no retroactive write. Ranking must treat it accordingly (§9); it is not a measure of compute time.

Only `tx.owner_user_id` may rate the transaction. Rating is supervision, immutable, non-cascading, duplicate-rejected with `ErrInvalidInput`, and never routed through `Call()`.

Every committed success or failure has exactly one receipt. `issuer_user_id=@sys`. `args_hash` and `reply_hash` are SHA-256 over RFC 8785 JCS canonical `args_json` and `reply_json`. Receipt economics match the transaction. `gross` is the call's price — its full allocation (§6). `fee` and `net` are the settlement amounts: `fee + net = taxable`, the allocation remaining at settlement; `gross − fee − net` is what the call committed downstream. On failure `fee = net = 0`, and the refunded amount equals `gross` minus the `fee + net` totals of the call's settled descendants. Signature is Ed25519 over canonical receipt JSON excluding `signature`. Transaction and receipt creation are atomic.

Receipts, ratings, and manifests use RFC 8785 JCS:

```text
CanonicalJSON(v any) ([]byte,error)
```

Sign and verify only canonical JSON. Receipt signatures and rating signatures are Ed25519 signatures made with the platform key. Rating signatures cover all fields except `signature`. ASCII property names make UTF-8 ordering equivalent to RFC 8785 UTF-16 ordering.

Transaction access uses historical transaction fields:

```text
CanReadTransaction(u,t) :=
  u = t.owner_user_id ∨ u = t.caller_user_id ∨ u = t.target_user_id ∨ IsSuperuser(u)
```

All three transaction parties are identified by captured fields: `owner_user_id` (payer), `caller_user_id` (call caller), `target_user_id` (payee). These remain valid after action deletion because all parties were captured at transaction creation. Receipts remain internal settlement/federation artifacts; there is no provider-receipt endpoint. Every credit to an action owner must be reconstructible from transactions readable by that action owner.

## 12. Authentication, bootstrap, and supervision

Authentication uses OAuth/OIDC-style browser authorization code with PKCE, CLI device authorization or loopback login, short-lived bearer access tokens, rotatable refresh tokens if used, and server-side logout revocation. Scripts never receive access or refresh tokens.

Errors:

```text
ErrUnauthenticated ErrUnauthorized ErrNotFound ErrInvalidInput ErrInvalidState
ErrInsufficientFunds ErrExecutionFailed ErrSchemaViolation ErrTimeout ErrInternal
```

Errors map to stable CLI exit codes and HTTP statuses. Messages are concise; logs may include diagnostics.

The fixed superuser handle is `@sys`. CLI admin authority compares authenticated handle to `config.superuser_handle`.

First boot prompts only for a password and atomically creates:

```text
@sys user
config.superuser_handle = @sys
config.signing_public_key  = base64url Ed25519 public key
config.signing_private_key = base64url Ed25519 private key
config.jwt_secret          = 32 random bytes, hex
```

Private signing key and JWT secret are never logged or returned. Partial first boot is rerunnable. `JUICE_SECRET_KEY` overrides stored JWT secret at runtime only.

Every startup reads `config.superuser_handle` to confirm first boot and identify `@sys`; it verifies signing keys and aborts if either is absent. It then registers, enables, and makes public `@sys/lookup`, `@sys/llm/chat`, `@sys/llm/embed`, `@sys/llm/json`, `@sys/llm/decide`, `@sys/make`, `@sys/time`, `@sys/sink`, `@sys/message`, `@sys/random`, and `@sys/tinygo/compile` if absent, and reconciles their configurable fields (price and action-specific settings) from config on every startup. It then runs recovery (§5).

Bootstrap is idempotent. Supervision operations are not native actions.

The superuser may suspend or unsuspend users. Suspension preserves data and makes every authenticated request return `ErrUnauthenticated`.

`Kernel.Deposit(operator_user_id,target_user_id,amount,reason)` is admin-CLI-only supervision. It requires configured superuser and positive amount, then atomically credits `user.available` with a deposit record. `reason` is optional and stored when provided. No HTTP endpoint exists.

`Kernel.Withdraw(operator_user_id,target_user_id,amount,reason)` is admin-CLI-only supervision: the mirror of `Deposit`. It requires configured superuser, positive amount, and `target.available ≥ amount`, then atomically debits `user.available` with an immutable withdrawal record — the user's credits are redeemed and the operator owes the out-of-band payout. No HTTP endpoint exists.

`Kernel.UpdateUser(callerUserID, email, currentPassword, newPassword)` is user self-service: only the authenticated, non-suspended local user may update their own account. `email` and `newPassword` are both optional; at least one must be provided. When `newPassword` is non-empty, `currentPassword` must match the stored hash; mismatch returns `ErrUnauthenticated`. Proxy users have no stored password and cannot use this operation (`ErrInvalidState`). `handle` is immutable. The update is atomic.

## 13. Federation

### Peers and proxy users

Every user is **local** (null `public_key`/`remote_base_url`; authenticates by password or token) or a **proxy** (both set; a peer kernel's account here, authenticating only by federation signature, per request). The kind is fixed at creation. Proxy users cannot log in, hold tokens, or be created by `user create` — only by peer acceptance. Because a peer is just a user, federation adds no new money model: a proxy user holds credits, pays, and is paid like anyone else.

A proxy user's handle is a **local alias** chosen at acceptance (default: the peer's self-reported handle, if free). Addressing is uniform for local and proxy users alike — `@B/translate` looks exactly like `@jane/translate`; the real-vs-proxy distinction is internal and never surfaced in the address. Identity is always `public_key`, location always `remote_base_url`; handles never appear in the federation protocol. Re-adding a known key updates the URL; a known handle or URL with a different key is rejected; key rotation is unsupported.

### Friending — the ACT relation

Two kernels transact only as **friends**: a reciprocal relation with the proxy-user pair (`@B` on A, `@A` on B).

```text
juice peer friend <url>      register peer + bulk-import all their active public actions
juice peer unfriend <user>   end the relation; deny future requests; deactivate all proxies (user is @handle)
juice peer list              known peers and balances
juice peer inspect <url>     view remote identity, public actions, and transacted friends (no DB write)
```

All `peer` commands are superuser supervision, CLI-only (§14). `POST /v1/peers` is the inbound protocol endpoint, authenticated by federation signature — not a local API.

`friend` verifies `<url>/.well-known/juice-kernel.json` and sends a signed request. By default kernels **auto-accept** (`peer_auto_accept = true`): the proxy user is created with balance 0 and a reciprocal request completes the pair. With manual mode, requests sit pending until the operator friends back. Friend requests are rate-limited per IP like account creation (§14).

Friendship by itself grants nothing: a zero-balance friend's calls are all rejected. The trust decision is the **deposit** — an operator credits a friend's proxy user only after real money moved out of band (§12). Friendship exchanges keys; funding expresses trust.

`unfriend` ends the relation and puts the peer's key on the deny list (`denied_at`): future requests from that key are rejected, not auto-accepted. Their imported proxies here deactivate; manifests stop being served to them; new inbound calls get signed rejections; waiting steps with the peer as `required_caller` are cancelled and their parked prices refunded (§10). The balance and all history survive — the operator settles the net by `Withdraw` (§12). In-flight outbound calls still settle on their receipts: the receipt path stays open for pending idempotency keys. Notification is best-effort; a peer that missed it learns from its next rejection. Your own `friend` on a denied key clears the denial and restarts the handshake — only you can unblock, by choosing to re-friend.

The relation is **not transitive** and is the only path to calling: invoking `@B`'s actions requires direct friendship with B. Exposure is each side's lever: a kernel may stop serving manifests to a friend whose balance cannot cover its cheapest exposed action; funding restores them.

### Gossip — discovery and reputation

Gossip is information, never authority. `GET /v1/gossip` (public, read-only) returns the kernel's identity, its own exposed actions with manifests and local stats, and its **transacted friends** — friends it has settled calls with — each with key, URL, and the kernel's earned local stats for that friend's actions. Friends without settled traffic are not gossiped: endorsement is earned by trade, never granted by friending.

Gossip results accumulate in the local discovery table, one row per (kernel, introducer):

```text
DiscoveredKernel { public_key, handle, base_url, introduced_by, stats_json, first_seen, updated_at }
```

Third-party stats are stored namespaced by source (`StatTag`, `source = <introducer>`) and affect **ranking only** — priors weighted by introducer count and local experience with the introducer, dominated by own `Stats` as they accumulate. They never affect callability, pricing, or settlement. Knowing a kernel through gossip permits nothing: calling requires your own friendship, and manifests are always fetched and verified from the owner, never trusted from an introducer.

Importing an action initializes its local `Stats` to the defaults (§3): ground truth starts empty and is earned by settled calls. Priors are consulted at ranking time from the stored manifest (the owner's claim) and `StatTag` (introducers' earned stats), never copied into `Stats`. How priors combine is the ranking layer's tested, replaceable formula (§9); the kernel mandates only the signals and the discipline above.

### Money — prepaid credits

Federation is prepaid. For A's users to call B's actions, A must hold credits on B: A's operator pays B's operator out of band; B's superuser `Deposit`s the `@A` proxy user (§12). Redemption is the mirror, by `Withdraw` (§12). Credits cross the bank boundary only by supervision; execution never mints or burns.

The proxy user's balance is the bilateral account: `@B`'s balance on A rises when A's users import from B (settlement pays it as `target_user_id`, §6) and falls when B's users import from A (inbound calls spend it). Operators settle only the net, out of band, at their own cadence. Netting needs no kernel machinery — it falls out of the peer being a user. The depositor bears counterparty risk, bounded by the deposit: keep deposits small and settle often.

### Calls, receipts, settlement

Outbound: a remote-proxy call follows normal role law (`owner` = local process owner, `caller` = local call caller, `target` = the proxy user) and normal wallet mechanics, funded with the proxy's local price `mp + maxduty` (§8). The handler records the UUID v4 `idempotency_key` and the outbound request payload (`dispatch_json`) on the proxy trace atomically with dispatch (§3, §5) — the stored payload is what makes retry after restart possible. On the remote kernel it arrives at `POST /v1/federation/call`, authenticated by the per-request federation signature (not a bearer token), and is then an ordinary inbound call by this kernel's proxy user there, paid from the prepaid balance, executed wholly under that kernel's §6.

A proxy call settles **only on a signed remote receipt** — never on a network timeout:

```text
remote success:   charge = receipt.charge (= receipt.gross = mp)
                  duty   = ceil(charge * import_bps / 10000)
                  pay charge to the proxy user; duty to @sys
                  refund (mp + maxduty) − charge − duty to the caller's trace
                  local transaction: success; receipt JSON + SHA-256 stored atomically
remote failure (charge = receipt.charge ≤ mp; the remote draw — settled remote descendants stay paid, §6):
                  pay charge to the proxy user; duty = 0 (fee is zero on failure, so is duty)
                  refund (mp + maxduty) − charge to the caller's trace
                  local transaction: failure
signed rejection (charge = 0):
                  full refund; local transaction: failure
no receipt (timeout):
                  no settlement — the call stays running, the process stays open;
                  retry with the same idempotency_key until a signed receipt arrives
```

This is the local failure rule (§6) applied across the wire: a failed call refunds its remaining allocation, and the receipt's `charge` is how the local kernel learns what remained. A failure counts against the proxy's stats regardless of charge — a provider gains nothing by failing-with-charge over succeeding, and farming failures collapses its rank (§9, §16).

Duty taxes the actual import, to the local `@sys`; the remote kernel never sees it. Default `import_bps = 500`, per peer. Forced closure (`EndProcess`) fails an in-flight proxy call like any running call (§6) and refunds the caller; if the remote side did commit, its charge stands there and is absorbed by the bilateral account, surfacing at reconciliation.

Inbound: calls sign `JCS({action, counterparty, idempotency_key, timestamp, args_hash})`; `counterparty` is the caller's base64url public key; `args_hash = SHA-256(raw body)`. Receiver verifies signature, raw hash, friendship, timestamp age ≤ 5 minutes; non-friends and denied keys are rejected. A valid signature authenticates the proxy user for that request; the call is `run` as that user, paying from its balance. Insufficient balance returns a **signed rejection receipt** (`status = failure`, zero charge) so the caller always has something to settle on. Idempotency: insert pending before execution, unique `(idempotency_key, counterparty_user_id)`; completed replay returns the stored receipt, pending replay 409, expiry 24h.

`VerifyRemoteReceipt(caller_id, tx_id)` requires `CanReadTransaction` and verifies entirely locally, in two parts. **Receipt integrity:** signature against the peer's `public_key`, stored JSON against its stored SHA-256, `receipt.action_id == proxy.remote_action_id`. **Settlement consistency:** the local transaction's outcome matches `receipt.status`; the amount paid to the proxy user equals `receipt.charge`; the local refund equals `(mp + maxduty) − receipt.charge − duty` with duty per this section (zero on failure); `args_hash` and `reply_hash` match the local record. The receipt's own `gross/net/fee` are the remote kernel's economics and are reported, not compared. Returns per-check results and top-level `valid`; non-proxy transactions give `ErrInvalidState`.

## 14. CLI, HTTP, logging, config

HTTP API is primary. Every exposed endpoint has a CLI command. CLI uses the same service layer, supports human-readable and JSON output, works directly against local SQLite where feasible, and each command has at least one test. Admin is CLI-only. A command's primary identifier is a positional argument by its natural key — a user is `@handle` (never an id), an action is `@owner/name` (an id is also accepted), and processes, steps, and transactions are ids; a second mandatory value (amount, rating) is the second positional. CLI human-readable output exposes the same fields as the corresponding HTTP response; `--json` selects the canonical JSON form (the HTTP shape).

CLI handlers and HTTP handlers are thin wires: parse input, call the service layer, format output. All kernel calls, enrichment, validation, and transformation live in the service layer. No kernel calls outside the service layer.

Required commands:

```text
juice serve
juice user create <user> <email>          juice user me
juice user update
juice auth login <user>                    juice auth logout
juice action create <name>                 juice action update <action>
juice action delete <action>              juice action enable <action>
juice action disable <action>             juice action list
juice action import <spec-url>            juice action unimport <spec-url>
juice action stats <action>
juice process list                        juice process show <id>
juice process end <id>                    juice run <action> [json]
juice step create <action>                juice step list
juice step show <id>                      juice step complete <id> [json]
juice tx list                             juice tx show <id>
juice tx rate <id> <0|1>                  juice tx verify <id>
juice health
juice admin users                         juice admin show <user>
juice admin suspend <user>                juice admin unsuspend <user>
juice admin deposit <user> <amount>       juice admin withdraw <user> <amount>
juice admin actions
juice admin disable <action>              juice admin processes
juice admin txs                           juice admin steps
juice peer friend <url>                   juice peer unfriend <user>
juice peer list                           juice peer inspect <url>
```

OpenAPI commands (the OpenAPI spec URL is the positional argument):

```text
juice action import <spec-url>
juice action unimport <spec-url>
juice action unimport <spec-url> --name <action-name>
```

`juice serve` handles `SIGTERM`/`SIGINT`, stops accepting new requests, drains in-flight calls, exits cleanly. No `juice stop`.

Server logs request, caller, process, trace, action, and transaction IDs where available; maps distinct auth, authorization, invalid input, insufficient funds, missing resource, and internal failures to distinct statuses; rate-limits auth, account creation, and peer requests per IP with 429. Action read/list responses include computed `action=@owner/name`.

Endpoint rules (notable rules only; the complete HTTP endpoint list is in `API.md`):

| Endpoint                                         | Rule                                                                                                            |
| ------------------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| `GET /health`                                    | unauthenticated                                                                                                 |
| `GET /.well-known/juice-kernel.json`             | unauthenticated; `public_key`, handle (`@sys`), `base_url` (§13)                                                |
| `GET /v1/actions[?owner=&name=]`                 | unauthenticated → active public actions; authenticated → active public actions plus caller's own active actions (union, deduplicated); `?owner=` further filters by that owner's handle; `?name=` filters by name |
| `GET /v1/actions/{id}/manifest`                  | signed public-action manifest; served to friends in good standing per the exposure lever (§13)                  |
| `GET /v1/gossip`                                 | unauthenticated; identity, own actions with manifests and stats, transacted friends with stats (§13)            |
| `POST /v1/peers`                                 | signed friend request (§13); rate-limited per IP                                                                |
| `POST /v1/federation/call`                       | inbound proxy call (§13); `?action=@owner/name`, `?counterparty=` peer key; `X-Signature`/`X-Timestamp`/`X-Idempotency-Key` headers; per-request federation signature over the request, `args_hash` must match the body — not bearer-authenticated |
| `GET /v1/me`                                     | authenticated `id`, `handle`, `email`, `available`, `locked`; suspended rejected before handler                 |
| `PUT /v1/me`                                     | authenticated local user only; `{[email], [current_password, password]}`; `password` requires `current_password`; at least one field required; returns updated `id`, `handle`, `email`, `available`, `locked`; proxy user returns `ErrInvalidState` |
| `PUT /v1/actions/{id}`                           | action-owner update; `public` updatable; deactivation rules apply                                               |
| `DELETE /v1/actions/{id}`                        | action-owner delete preserving history                                                                          |
| `POST /v1/actions/import`                        | authenticated OpenAPI supervision import                                                                        |
| `POST /v1/actions/unimport`                      | action-owner import-provenance deactivation                                                                     |
| `GET /v1/processes`                              | process owner's processes, descending `created_at`                                                              |
| `GET /v1/steps`                                  | authenticated; returns steps visible to caller per `CanListStep`; optional `?process_id=` and `?status=` filters |
| `POST /v1/steps`                                 | authenticated; creates a waiting step; requires `trace_id` (funding trace), `action_id`, `required_caller`, `partial_args`; the authenticated user must be authorized to use `trace_id` (§4 precondition 4) |
| `GET /v1/steps/{id}`                             | `CanReadStep`; returns step fields                                                                              |
| `POST /v1/steps/{id}/complete`                   | `CanReadStep`; `args` required (`{}` valid); absent gives `ErrInvalidInput`; returns `result`, `tx_id`, `trace_id`, `step_id` |
| `POST /v1/run`                                   | requires `args`; `{}` valid; absent gives `ErrInvalidInput`; action is `@owner/name`                            |
| `POST /v1/auth/authorize`                        | unauthenticated; `{handle, password, code_challenge, [redirect_uri]}`; if `redirect_uri` provided → `302` redirect; if omitted → `200 {"redirect": "?code=CODE"}` for programmatic clients |
| `POST /v1/auth/token`                            | unauthenticated; exchanges auth code + `code_verifier` for `access_token` and `refresh_token`                   |
| `POST /v1/auth/logout`                           | refresh token body; missing/revoked gives `ErrUnauthenticated`                                                  |
| `GET /v1/transactions`                           | transactions visible to the authenticated user under `CanReadTransaction`                                       |
| `GET /v1/transactions/{id}`                      | full detail to parties; `ErrNotFound` to non-parties                                                            |
| `GET /v1/transactions/{id}/receipt-verification` | parties; remote verification; local tx gives `ErrInvalidState`                                                  |

Transaction list/detail include `rating: {"value":0|1,"note":string|null}` or `null`, visible to all transaction parties.

Admin commands require configured `@sys`, reject non-superusers with `ErrUnauthorized`, stay outside `Call()`, and register no `/v1/admin/*` routes.

Logs go to stderr and optionally file; stdout is resource payloads only. Configurable format, level, file. Every kernel transition logs start/end; errors include stable codes; script logs include trace ID.

Required log fields:

```text
time level event request_id caller_user_id process_id trace_id action_id tx_id
status duration_ms error
```

Config lives in `juice.json` (path from `JUICE_CONFIG`, default `./juice.json`). Top-level kernel keys: `db_path`, `fee_bps`, `import_bps`, `peer_auto_accept`, auth issuer/audience/token TTL, log file/format/level, script timeout and memory limits, plus the federation identity and credential keys this kernel needs to satisfy §8 and §13: `server_url` and `peer_handle` (this kernel's advertised base URL and handle for the §13 `.well-known` document and reciprocal friend requests), `credentials_key` (the §8 base64url AES-256-GCM key for `auth_json`, auto-generated at first boot), and `allow_local_sources`/`allow_local_peer_urls` (dev-only escape hatches over §7's loopback/private/link-local URL rejection, default `false`). All native-action configuration lives under `native.<action>`; no deeper nesting:

```json
{
  "db_path": "./juice.db",
  "fee_bps": 2000,
  "import_bps": 500,
  "peer_auto_accept": true,
  "server_url": "",
  "peer_handle": "",
  "credentials_key": "",
  "allow_local_sources": false,
  "allow_local_peer_urls": false,
  "native": {
    "llm":     { "url": "http://localhost:11434", "chat_model": "gemma4:26b", "embed_model": "nomic-embed-text", "price": 0 },
    "make":    { "compiler": "tinygo", "max_steps": 5, "price": 20 },
    "lookup":  { "default_limit": 10, "price": 0 },
    "time":    { "price": 0 },
    "sink":    { "price": 0 },
    "message": { "price": 0 },
    "random":  { "price": 0 },
    "tinygo":  { "price": 5 }
  }
}
```

Safe local defaults apply when the file or a key is absent; invalid startup config is rejected; no committed production secrets.

Environment variables are bootstrap and overrides only:

```text
JUICE_CONFIG               path to juice.json
JUICE_DB_PATH              database path override
JUICE_SECRET_KEY           JWT secret override, runtime only (§12)
JUICE_LOG_LEVEL            log level override
JUICE_CREDENTIALS_KEY      AES credentials key override, runtime only (§8)
JUICE_BOOTSTRAP_PASSWORD   superuser password for unattended first boot (§12)
JUICE_BOOTSTRAP_PEER_HANDLE  peer handle for unattended first boot (§13)
JUICE_ALLOW_LOCAL_SOURCES  dev-only: permit loopback/private/link-local source and peer URLs (§7)
```

All other settings are configured through `juice.json` only; there are no further environment overrides.

## 15. Required automated tests

`go test ./...` must pass without external network access. Tests use temporary SQLite databases, fake Ollama and fake script adapters unless explicitly integration tests, no global state, and no order dependence.

Required suites:

```text
user creation
authentication token validation
user update email; change reflected in GET /v1/me
user update password with correct current_password; old password rejected after change
user update password with wrong current_password returns ErrUnauthenticated
user update with neither email nor password returns ErrInvalidInput
proxy user UpdateUser returns ErrInvalidState
action create/update/delete
action activation/deactivation
public/private access control
run creates, funds, and closes processes
successful paid call
failed call refunds remaining allocation to caller's trace
run with insufficient user balance rejected
subcall exceeding parent trace available fails with ErrInsufficientFunds
input schema rejection
output schema rejection
trace root and child creation
nested call trace tree
transaction creation
payment split
run locks exactly the root price from the user's wallet
call entry moves q from caller wallet available to locked; child trace starts available=q
child success releases caller lock; child failure refunds remainder and releases lock
settlement pays fee+net = trace.available (taxable); unused budget is provider margin, never refunded on success
failure rollup cancels outstanding steps recursively and refunds up the chain
settled descendants survive ancestor failure; refund equals gross minus settled descendants' fee+net
process closes automatically when root returned and no steps outstanding
outstanding step keeps process open with price parked
step completion spends parked price; reset-to-waiting keeps it parked
recovery: orphan traces fail as interrupted with rollup; claimed steps re-park; waiting steps survive restart
step create/read/list/complete
wasm script execution
wasm host function call (juice.call, juice.step_create, juice.step_complete)
script timeout
script memory limit
lookup ranking with fake embeddings
lookup results include action (@owner/name), input_schema, and output_schema
stats update
CLI commands
CLI primary identifiers are positional natural keys (user=@handle, action=@owner/name)
CLI human-readable output exposes the same fields as the corresponding HTTP response
logging smoke test
superuser first-boot prompt and config storage
suspended user rejected at authentication
direct buyer can rate transaction
non-buyer cannot rate transaction
native action callable through Call()
@sys/random returns value in [0, 1)
wasm script can call @sys/random to obtain a random value
@sys/tinygo/compile returns base64 artifact and hash for valid source; status=failure with diagnostics on compile or import-validation error; empty source returns ErrInvalidInput
action create --artifact registers a wasm action from a pre-compiled base64 artifact
@sys/llm/embed returns embedding array for valid text
@sys/llm/embed with empty text returns ErrInvalidInput
@sys/llm/embed with unconfigured embedder returns ErrInvalidState
@sys/llm/json returns value matching output_schema for valid input
@sys/llm/json with unsupported output_schema returns ErrSchemaViolation
@sys/llm/json with unconfigured structured output returns ErrInvalidState
@sys/llm/json rejects model output that fails schema validation
@sys/llm/decide returns selected action and validated args
@sys/llm/decide fetches action contract from DB by @owner/name
@sys/llm/decide rejects unknown action reference with ErrNotFound
@sys/llm/decide rejects returned args that fail action input_schema
@sys/llm/decide with unconfigured tool calling returns ErrInvalidState
@sys/llm/decide with no valid selection returns ErrExecutionFailed
non-superuser rejected from admin CLI commands
active public action callable by any caller
active private action callable only by owner
inactive action not callable
bootstrap is idempotent
subcall uses parent process, not an ephemeral process
subcall caller is parent action owner and owner is original process owner
subcall transaction has owner_user_id = parent process owner
subcall transaction has caller_user_id = parent action owner
subcall transaction has target_user_id = target action owner
subcall authorizes process use by caller_user_id
subcall authorizes action access by process owner
subcall spends from parent trace within same process
failed subcall refunds parent trace
successful subcall remains settled if parent later fails
subcall trace has same process_id and parent_trace_id pointing to caller trace
root trace has null parent_trace_id
step create returns waiting step with correct fields and parked price
step complete merges partial_args with caller input (input keys overwrite partial_args keys)
step complete input validated against derived allowed input (action.input_schema minus partial_args keys) before merge
step complete final args validated against action.input_schema by Call
step complete with wrong caller returns ErrUnauthorized
step complete against running or done step returns ErrInvalidState
step complete resets to waiting when Call rejects before creating a transaction
step complete with deactivated action resets step to waiting
step complete with private action (CanCall lost) resets step to waiting
step complete disallowed-key (derived-schema) violation rejects, leaves step waiting, no action failure recorded
step tx_id recorded atomically with status=done
step-completion trace has parent_trace_id equal to step.parent_trace_id
step-completion trace process_id derived from step.parent_trace_id
step-completion transaction has owner_user_id = step process owner
step-completion transaction has caller_user_id = required_caller_user_id
step-completion transaction has target_user_id = action owner
step completion gross equals step.price snapshot
step allowed input is action.input_schema minus partial_args keys (derived, not stored)
CanListStep: process owner sees own step
CanListStep: required_caller_user_id user sees step
CanListStep: unrelated user denied
CanReadStep: same rules as CanListStep
wasm juice.step_create returns step_id bound to current process and trace
wasm juice.step_complete executes the step's action and returns result, tx_id, trace_id
webhook caller authenticates as registered user and calls POST /v1/run directly
webhook caller completes a pre-created step via POST /v1/steps/{id}/complete
startup reads config.superuser_handle to confirm first boot and identify @sys
second rating on same transaction rejected with ErrInvalidInput
rating record created in ratings table, transaction row unchanged
rating note is included in the single platform-key Ed25519 rating signature payload
ratings do not cascade
Ed25519 signing keypair present after first boot
zero-credit process satisfies fund locking for zero-price actions
Kernel.Deposit rejected with ErrUnauthorized for non-superuser caller
withdraw debits available with withdrawal record; rejected for non-superuser
receipt created atomically with successful transaction commit
receipt created atomically with failed transaction commit
action owner reads transactions for calls to their action
non-party denied access to a transaction
upstream auth secret never appears in args, replies, logs, receipts, or read paths
OpenAPI import/unimport flow for API-owned actions
OpenAPI import compiles parameters and JSON body into one input schema
OpenAPI activation rejects incomplete schemas or missing descriptions
OpenAPI public activation requires ownership proof
OpenAPI import affects only matching OpenAPI-provenance actions
OpenAPI import preserves Action.id, deactivates on contract change, and resets current stats
remote import/unimport flow for signed manifests
remote import preserves Action.id, deactivates on manifest contract change, and does not overwrite local Stats
remote import initializes local Stats to defaults
remote manifest signature is Ed25519 over canonical JSON excluding signature
remote proxy transaction has target_user_id = proxy user id
friend auto-accept creates zero-balance proxy pair; manual mode holds pending
zero-balance friend's inbound call gets signed rejection receipt
unfriend sets denied_at, deactivates proxies, cancels steps addressed to peer; balance survives
denied key's friend request rejected; own friend clears denial
gossip lists only transacted friends with stats; non-transacted friends absent
gossip results stored per (kernel, introducer); StatTag namespaced by source
proxy call locks mp+maxduty; success settles charge+duty on actual charge and refunds difference
remote failure with charge refunds (mp+maxduty)−charge; duty is zero; settled remote work stays paid
failure counts against proxy stats regardless of charge
duty credited to @sys; charge credited to proxy user
timeout does not settle; retry with same idempotency key recovers receipt
restart with a dispatched proxy call resumes retry; receipt obtained after restart settles it; no interrupted refund
inbound federation call rejected when args_hash does not match request body
full remote receipt JSON stored atomically with transaction on remote-proxy call
receipt verification returns valid for a well-formed stored remote receipt
receipt verification detects signature tampering
receipt verification detects mismatch (action_id, status, charge, settlement arithmetic)
receipt verification returns ErrInvalidState for a non-remote-proxy transaction
```

Direct invariant tests:

```text
balances are never negative
successful settlement satisfies taxable = net + fee, with taxable = trace.available at settlement
wallet totals (user, process, trace) change only by run, call entry, settlement, refund, step park/unpark, deposit, withdrawal, closure
closed processes cannot call actions
inactive actions are not callable
every call creates exactly one transaction
every nested call creates exactly one child trace
suspended users cannot authenticate
native actions are always owned by the superuser
ratings do not cascade; each rating applies only to the rated transaction
all subcalls spend from their parent trace within the original funded process
successful subcall settlements persist if ancestor call later fails
failed call's refund equals gross minus fee+net totals of its settled descendants
root traces have parent_trace_id = null
subcall traces share their parent's process_id
step-completion traces have parent_trace_id equal to step.parent_trace_id
step-completion traces have process_id derived from step.parent_trace_id
a step without tx_id is never done; tx_id is set atomically with status=done
waiting steps are cancelled with parked prices refunded when their process closes or their creating call fails; cancelled steps carry no tx_id
an outstanding step keeps its process open
a settled trace never regains available; refunds destined for it route to the process
user.locked equals the sum of funds in the user's open processes
transaction row is immutable after commit
rating records reference valid tx_id and receipt_id
every transaction obeys owner_user_id = process owner, caller_user_id = call caller, target_user_id = action owner
every credit to an action owner is reconstructible from transactions readable by that action owner
proxy users cannot log in or hold tokens; local users cannot authenticate by federation signature
imported action reimport or unimport never deletes transaction or receipt history
imported action current stats reset never mutates transaction, receipt, or rating rows
OpenAPI and remote imports create ordinary Actions, not separate action types
all imported actions execute only through Call()
```

Required user-flow tests (against the compiled kernel surface — CLI and HTTP only):

```text
— Local execution —
user signs up, deposits arrive (admin), runs a public action by @owner/name, gets result;
  process auto-created, auto-closed, exact price debited, provider's net and @sys fee observable in tx list
provider creates a WASM action that subcalls two cheaper actions, activates it, a caller runs it;
  caller pays one advertised price, subproviders paid from the provider's budget, provider keeps margin
caller runs an action that fails mid-tree; settled subcall stays paid, remainder refunded,
  process closes, transactions show success and failure with reasons

— Async / steps —
action parks an approval step addressed to a human and returns; process stays open with price parked;
  the human sees it in step list, completes it; fulfillment runs on parked funds; process closes
external system (webhook) registers as a user, a purchase flow pre-creates a step addressed to it,
  the system POSTs the payload to /v1/steps/{id}/complete; transaction obeys role law
owner force-ends a process with waiting steps; steps cancelled, parked prices refunded, balances reconcile
kernel restarts mid-flight: interrupted calls fail as interrupted with refunds; waiting steps survive
  and remain completable after restart

— @sys/make —
user runs @sys/make from a description; worker subcalls obey role law; resulting action is owned
  by the requester, activated, and immediately runnable by another user

— OpenAPI —
API owner imports an OpenAPI document with a stored API key, activates an action, makes it public,
  and a caller executes it through Call(); the key never surfaces
API owner re-runs import against a changed OpenAPI document; the matched action is deactivated,
  stats reset, and historical transactions remain attached
API owner unimports an OpenAPI document; matching actions are deactivated and history remains attached

— Ratings and reconciliation —
caller executes a paid action multiple times; the action owner lists transactions for their action
  and the sum of transaction net amounts equals the total credits received by the owner
caller rates a transaction with a note; the note and rating value appear in transaction detail and
  list responses for all parties; an unrated transaction returns null for the rating field

— Federation —
two kernels friend each other (auto-accept); operator A deposits B's proxy (and vice versa);
  B imports A's action; B's user runs it; charge lands in A's proxy balance on B, duty to B's @sys,
  difference refunded; both sides' tx verify passes all checks
kernel gossips a transacted peer; third kernel reads the gossip, sees earned stats, friends the
  subject directly, imports, runs — its own Stats start at defaults and accumulate
A unfriends B: B's proxies deactivate, B's next inbound call gets a signed rejection receipt,
  steps addressed to B cancelled with refunds, balance intact; A re-friends and traffic resumes
inbound call from an underfunded friend yields a signed rejection receipt the caller settles on
```

## 16. Design rationale

Stable kernel interfaces and explicit transitions prevent correctness from depending on transports or adapters. Small function-named packages and per-file tests reduce coupling, keep replaceable implementations visible, and expose coverage gaps. Execution and supervision are separated because execution may err, while supervision supplies correction signals execution must not manipulate.

Credit locking of the full subtree price before execution prevents unfunded work anywhere in the tree. Failure refunds of the remaining allocation keep accounting conservative, observable, and testable. Input-before-lock and output-before-settlement prevent charging invalid requests or paying malformed replies. Mediated WASM authority permits composition without credential leakage or authorization bypass.

Subtree pricing makes a price a price: the caller pays one advertised number, composition risk lives with the provider who composed, and the fee taxes each layer's margin — value added — rather than gross flows. `run` removes process bookkeeping from the user: funding is exact, closure is automatic, and a process is simply the lifetime of a computation and its continuations. Steps are funded continuations: money reserved at suspension is what makes asynchronous composition safe, restartable, and honest about who pays.

Federation incentives are aligned in both directions. A kernel federates because its users gain a larger action space and its providers gain outside demand; its operator's reward is the import duty. The domestic fee (default 20%) deliberately exceeds the import duty (default 5%), so a kernel always earns more on local supply than on imports — federation complements local providers rather than undercutting them. Discipline is self-enforcing without enforcement machinery: a kernel whose counterparty account runs dry stops serving it manifests, because executing unpaid work loses money twice — once in service, once in the failure stats that sink its rank abroad; funding restores exposure. And because gossip carries only trade-backed opinions, reliable behavior compounds into discoverability: reputation is the long-run asset a kernel earns by settling honestly.

Replaceable lookup ranking permits research changes without changing kernel semantics. Fixed stats plus optional tags preserve deterministic baseline metrics while isolating experiments. Latency is derived from transaction timestamps rather than cached on traces, so buyer-experienced wall time is always current — a subtree query reflects late descendants without a retroactive write — at the cost of that query at read time.

Fixed `@sys` and signing keys give stable system action names and verifiable receipts/manifests. Structured logs make production operation and research reproduction reconstructable.

OpenAPI import as supervision keeps registration low-friction while preserving uniform execution. Federation imports signed action contracts without leaking implementation. Contract-change deactivation prevents silent interface drift for callers and LLMs. Unimport deactivates rather than deletes so history remains auditable. Friendship is deliberately worthless (auto-accepted, zero balance) so that trust lives in deposits and reputational gossip carries only earned, trade-backed opinions; a Bayesian shrinkage of ranking priors toward trust-weighted introducer means is the natural baseline formula, but the formula is the ranking layer's experiment, not kernel semantics.