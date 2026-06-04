# Juice Kernel Requirements

Version: 0.2
Status: implementation requirements
Codename: `juice`

## 1. Purpose and architecture

Juice is a small Go production kernel and research platform for callable actions. Its central semantic object is:

```text
Call(subject, process, action, args)
```

`Call()` is the sole action-execution path. A call is valid exactly when the subject is authenticated, the process exists and is open, the subject owns or has explicit authority over the process, the action exists and is active, `CanCall(subject, action)` is true, the arguments satisfy the input schema, and the process has enough available credits.

Execution and supervision are separate layers:

* **Execution:** `Call()` performs action invocation, fund locking, tracing, settlement, statistics, and receipts. Native actions, WASM `juice.call`, event consumption, and remote proxies must use it.
* **Supervision:** direct authenticated kernel operations manage users, actions, processes, ACLs, ratings, deposits, OpenAPI imports, and remote imports. They must not route through `Call()`.
* A subject must not rate its own output or trigger rating propagation from execution code.

## 2. Implementation constraints

* Use Go. `go build ./...` and `go test ./...` must pass.
* Use ordinary Go interfaces for replaceable modules.
* `kernel` must not import CLI, HTTP, SQLite, wazero, or Ollama implementations.
* Use only these function-named production packages unless a dependency-cycle or cohesion reason requires otherwise:

```text
cmd/juice/   CLI and server entrypoint
kernel/      core objects and operational semantics
store/       persistence interface and SQLite implementation
script/      WebAssembly execution
llm/         local language and embedding interface
log/         structured logging
```

* Architectural package names such as `sqlite`, `wazero`, or `ollama` are forbidden; implementation-specific names may appear in concrete types or file names.
* Keep the package and source-file counts small. Do not split files for size alone. Every production source file must have a corresponding `_test.go` file with independent tests for its logic.

## 3. Data model

All IDs are stable opaque identifiers; action IDs are globally unique. Credit balances and prices are non-negative integers; credits are indivisible.

| Object              | Required fields                                                                                                                                                                                                                             | Rules                                                                                                                                                                                                                                                                                                                   |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `User`              | `id`, `handle`, `email`, `available`, `locked`, `suspended_at`, `public_key`, `remote_base_url`, `created_at`, `updated_at`                                                                                                                 | `handle` is unique and must not contain `/`. A suspended user is rejected at every authenticated request with `ErrUnauthenticated`. `public_key`, when set, is a unique base64url Ed25519 32-byte public key. Local users have null `public_key` and `remote_base_url`; remote peers set both.                          |
| `Action`            | `id`, `owner_user_id`, `name`, `kind`, `active`, `public`, `price`, `description`, `input_schema`, `output_schema`, `source`, `artifact_hash`, `remote_action_id`, `created_at`, `updated_at`                                               | `kind ∈ {http, wasm, native, remote_proxy}`. `(owner_user_id, name)` is unique. `name` follows URL-path conventions: `/` is a hierarchy separator, not a forbidden character (e.g., `llm/chat`). The full `@owner/name` reference is structurally analogous to `host/path` in a URL — unambiguous because handles (like hostnames) cannot contain `/`. An `active=false` action is not callable by non-owners. Public discovery returns active actions only unless an owner requests private state. Authorized users may inspect script source. Compiled artifacts are content-addressed by `artifact_hash`. |
| `ACLEntry`          | `subject_user_id`, `action_id`, `permission`, `created_at`                                                                                                                                                                                  | `permission ∈ {read, call, admin}`. ACLs are direct user-to-action grants. `read` permits inspection; `call` permits execution; `admin` permits ACL and lifecycle changes. Owners implicitly have `admin`.                                                                                                              |
| `Process`           | `id`, `owner_user_id`, `available`, `locked`, `status`, `created_at`, `ended_at`                                                                                                                                                            | `status ∈ {open, closed}`. A process starts with user-provided funds and may start with zero credits (`available = 0`). Closing it returns all remaining funds to its owner. Closed processes cannot execute calls.                                                                                                     |
| `Trace`             | `id`, `process_id`, `parent_trace_id`, `caused_by_trace_id`, `cost`, `latency_ms`, `created_at`                                                                                                                                             | Every process has one root trace. Choose one root convention consistently: `parent_trace_id = id` or `parent_trace_id = null`. Every direct `Call()` creates exactly one child trace.                                                                                                                                   |
| `Transaction`       | `id`, `process_id`, `trace_id`, `parent_trace_id`, `owner_user_id`, `subject_user_id`, `target_user_id`, `action_id`, `args_json`, `reply_json`, `status`, `gross`, `net`, `fee`, `reason`, `remote_receipt_hash`, `started_at`, `ended_at` | `status ∈ {success, failure}`. Every attempted call creates one immutable transaction. `remote_receipt_hash` is null locally and stores `SHA-256(remote_receipt_json)` for cross-kernel calls.                                                                                                                          |
| `Stats`             | `uses`, `successes`, `failures`, `rating_count`, `price_mean`, `latency_mean`, `rating_mean`, `last_used_at`                                                                                                                                | Missing stats have defined defaults. `uses = successes + failures`.                                                                                                                                                                                                                                                     |
| `StatTag`           | `action_id`, `key`, `value`, `source`, `updated_at`                                                                                                                                                                                         | Optional, queryable for lookup experiments, and never required for kernel execution. Experimental tags are namespaced by source and never alter fixed-stat semantics.                                                                                                                                                   |
| `Listener`          | `id`, `owner_user_id`, `source_user_id`, `event_name`, `target_action_id`, `active`, `created_at`                                                                                                                                           | A listener subscribes its owner to an exact `(source_user_id, event_name)` pair.                                                                                                                                                                                                                                        |
| `Event`             | `id`, `listener_id`, `args_json`, `causing_trace_id`, `consumed_at`, `tx_id`, `created_at`                                                                                                                                                  | Persistent queued work item. `causing_trace_id` is nullable.                                                                                                                                                                                                                                                            |
| `Deposit`           | `id`, `operator_user_id`, `target_user_id`, `amount`, `reason`, `created_at`                                                                                                                                                                | Immutable audit record for a positive out-of-band superuser credit grant.                                                                                                                                                                                                                                               |
| `Receipt`           | `id`, `issuer_user_id`, `tx_id`, `trace_id`, `action_id`, `caller_user_id`, `process_id`, `args_hash`, `reply_hash`, `status`, `gross`, `net`, `fee`, `reason`, `started_at`, `created_at`, `signature`                                     | Immutable signed record for exactly one committed call. `caller_user_id` is the authenticated subject of the call. `started_at` is the wall-clock time the call began; `created_at` is settlement time.                                                                                                                 |
| `Rating`            | `id`, `rated_tx_id`, `rated_receipt_id`, `rater_user_id`, `rating`, `created_at`, `signature`                                                                                                                                               | Immutable signed feedback record. `rating ∈ {0, 1}`. At most one rating exists per transaction. `rated_receipt_id` may be null only for pre-receipt transactions.                                                                                                                                                       |
| `IdempotencyRecord` | `id`, `idempotency_key`, `counterparty_user_id`, `receipt_id`, `created_at`, `expires_at`                                                                                                                                                   | Used only for cross-kernel calls.                                                                                                                                                                                                                                                                                       |

### 3.1 ACL rule

ACL checks must occur inside the kernel path, not only at CLI or HTTP boundaries:

```text
CanCall(u, a) := Active(a) ∧ (Owner(u, a) ∨ Public(a) ∨ ACL(u, a, call) ∨ ACL(u, a, admin))
```

`public` is stored directly on the action. Grant-all and revoke-all toggle this flag without replacing direct ACL entries; only the owner or an action admin may invoke them.

For `remote_proxy` actions, `source` is the federation call URL and `remote_action_id` is the action's ID on the remote kernel. `artifact_hash` stores the manifest hash. Dispatch in `Call()` is based on `kind`, not on the owner's identity.

OpenAPI registration and federation do not create durable objects parallel to `Action`. They are supervision procedures that create or update ordinary `Action` rows. Execution always proceeds through `Call()`.

Every active action must have a non-empty natural-language `description`, a valid `input_schema`, and a valid `output_schema`. Schemas used for active actions must contain enough field descriptions to support lookup and LLM function calling.

### 3.2 Trace relationships

| Field                | Relation       | Scope             | Use                                            |
| -------------------- | -------------- | ----------------- | ---------------------------------------------- |
| `parent_trace_id`    | `CHILD_OF`     | Same process only | Direct calls within a process                  |
| `caused_by_trace_id` | `FOLLOWS_FROM` | Cross-process     | Contractor sub-calls and event-triggered calls |

Rules:

* A child inherits its parent's `process_id`; trace trees are rooted per process. Every transaction references a trace. Trace lookup by process returns the execution tree; trace deletion never deletes transaction history.
* The kernel must reject a missing or cross-process supplied `parent_trace_id` with `ErrInvalidInput`.
* Direct calls have null `caused_by_trace_id`.
* Contractor ephemeral-process roots set `caused_by_trace_id` to the calling action trace.
* Event-triggered calls set `caused_by_trace_id` to the emitter trace stored at emit time.
* `caused_by_trace_id` must never be treated as `parent_trace_id`.
* Each completed descendant transaction updates ancestor `cost` and `latency_ms` automatically. The originating trace of a `FOLLOWS_FROM` relationship may already be closed; the referenced trace belongs to a different process.

```text
∀ child. child.process_id = parent(child).process_id
```

## 4. Persistence and atomicity

Use file-backed SQLite with WAL enabled by default. Migrations must be deterministic and stored in the repository. Tests use temporary SQLite databases. No production feature may depend on an in-memory-only store.

`kernel` depends on a store interface, never directly on SQLite. The interface must support:

```text
CreateUser ReadUser ReadUserByPublicKey ListUsers SuspendUser UnsuspendUser
CreateAction ReadAction UpdateAction DeleteAction ListAllActions
GrantACL RevokeACL CheckACL
CreateProcess ReadProcess EndProcess ListAllProcesses
CreateTrace CreateTransaction ListTransactions ListAllTransactions
ReadStats UpdateStats
CreateListener ReadListener ListListeners
CreateEvent ListPendingEvents ConsumeEvent PurgeListenerEvents
GetConfig SetConfig CreateDeposit
CreateReceipt ReadReceipt
CreateRating ReadRating ListRatings
CreateIdempotencyRecord ReadIdempotencyRecord
```

`UpdateTransaction` is forbidden: transactions are immutable after creation, and all fields are set at `CreateTransaction` time.

All monetary transitions occur inside SQLite transactions. Each operation must atomically include:

| Operation       | Atomic writes                                                                                    |
| --------------- | ------------------------------------------------------------------------------------------------ |
| Start process   | user debit, process creation, root trace creation                                                |
| Fund process    | user debit, process credit                                                                       |
| Successful call | transaction, receipt, locked-fund settlement, target payment, platform fee, trace metrics, stats |
| Failed call     | transaction, receipt, full refund, trace metrics, stats                                          |
| End process     | process closure, return of remaining owner funds                                                 |
| Deposit         | user credit, deposit record                                                                      |
| Rating          | rating record insert                                                                             |

A committed monetary transition must never exist without its audit record, or vice versa.

## 5. Call state machine

### 5.1 Ordered preconditions

Check, in order:

```text
1. authenticated, non-suspended subject
2. existing open process
3. subject owns the process or has explicit process authority
4. existing action
5. active action
6. CanCall(subject, action)
7. valid input schema
8. process.available >= action.price
9. valid same-process parent trace, if supplied
```

Return the matching typed error for the first failed precondition.

### 5.2 Transition

For price `q`:

```text
create child trace
lock q credits in process
execute action
validate output schema
on success: atomically record transaction + receipt, settle payment, update trace metrics + stats
on failure: atomically record transaction + receipt, refund full q, update trace metrics + stats
return result, tx_id, trace_id
```

Locking occurs before execution:

```text
available := available - q
locked    := locked + q
valid iff q >= 0 ∧ available >= q
```

Zero-credit processes may execute actions with `price = 0` because `available >= price` is satisfied by `0 >= 0`.

### 5.3 Settlement

Only successful calls are charged in v1. Fee applies to value added only:

```text
gross       = action.price
sub_cost    = sum of gross paid to direct sub-calls during execution
taxable     = max(gross - sub_cost, 0)
fee         = (taxable * fee_bps + 9_999) / 10_000
net         = gross - fee
```

Negative value added does not create a fee credit or a kernel payout.

Success decreases the process and owner locked balances by `gross`, credits the target by `net`, and credits the fee recipient by `fee`. Each kernel taxes only its own layer; remote sub-calls are subject to the remote kernel's fee policy independently.

Any failure before or after target execution starts charges zero, refunds the full locked gross amount, records a failure transaction, and exposes the failure class through `status` and `reason`. A later partial-failure policy must be represented explicitly in the transaction.

### 5.4 Schemas

Every action has input and output schemas. The first implementation may support a strict JSON Schema subset, but unsupported forms must fail action creation or update. A schema node without a `type` key is treated as unconstrained (accepts any value); this is intentional and not an error. Validate input before locking funds and output before successful settlement.

### 5.5 Contractor sub-calls

When a running action invokes WASM host function `juice.call(target, args)`:

```text
owner := calling action owner
create ephemeral process owned by owner
fund it from owner.available with exactly target.price
create ephemeral root trace with caused_by_trace_id = calling trace id
invoke normal Call(owner, ephemeral process, target, args)
close ephemeral process and return unused funds
```

Rules:

* The caller's process pays only the top-level action price.
* Each recursive action owner pays for its own direct sub-calls.
* The ephemeral root's causal link is `FOLLOWS_FROM`, not `CHILD_OF`.
* Insufficient owner funds fail the sub-call and propagate failure to the top-level call; the original caller is fully refunded.
* Previously settled descendant costs are not reversed.
* Ephemeral processes are always closed after completion.

```text
caller.process.available decreases by at most action.price per call, regardless of sub-call depth or cost
```

## 6. Action lifecycle

| Operation | Rules                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Create    | Create inactive by default. Validate owner, name, kind, and non-negative price. `description`, non-nil schemas, and source value are required at activation, not creation. WASM creation validates or compiles its artifact only when a script executor is configured. HTTP creation validates endpoint configuration without calling the endpoint unless explicitly requested. Reject non-HTTP(S), loopback, private IP ranges (RFC 1918), and link-local (`169.254.x.x`) source URLs at creation and activation. Normal `CreateAction` always rejects `Kind=native`; bootstrap uses `RegisterNativeAction` instead. `RegisterNativeAction` does not enforce `@sys` ownership; that is the caller's responsibility. |
| Activate  | Require owner or admin. Initialize stats if absent. Reject invalid schema, missing source, invalid artifact, invalid HTTP URL, or invalid runtime configuration.                                                                                                                                                                                                                                                                                                                                               |
| Update    | Require owner or admin. Updating source, schema, kind, price, or endpoint deactivates unless explicitly marked safe. Recompute WASM `artifact_hash`; retain prior source and hash in transaction history.                                                                                                                                                                                                                                                                                                      |
| Delete    | Require owner or admin. Disable discovery, remove ACL entries, and preserve historical transactions; soft deletion is permitted.                                                                                                                                                                                                                                                                                                                                                                               |
| Native    | Register programmatically during bootstrap only, owned by `@sys`. Regular users cannot create, update, or delete native actions.                                                                                                                                                                                                                                                                                                                                                                               |

### 6.1 OpenAPI registration

OpenAPI registration is a supervision operation. It imports HTTP operations as ordinary inactive `Action` rows with `kind = http` and `source.type = openapi`.

OpenAPI import accepts any operation representable as:

```text
Call(args: JSON object) -> JSON object
```

GET, POST, PUT, PATCH, and DELETE operations may be imported. The HTTP method is stored in `Action.source`; it is not part of `Call()` semantics.

An imported OpenAPI operation must provide, or be rejected or kept inactive with validation messages:

```text
operationId or x-juice-name
description or summary
parameters and/or requestBody schema
2xx JSON response schema
x-juice-price, defaulting to 0 when absent
```

Path parameters, query parameters, and JSON request-body fields are compiled into one canonical `input_schema`. The selected 2xx JSON response schema becomes `output_schema`. Non-JSON responses, streaming responses, multipart uploads, ambiguous success schemas, unsafe URLs, and unsupported authentication must not produce active actions.

Ratings and stats are never imported from OpenAPI.

Draft import may occur without API ownership proof. Public activation, `grant-all`, or any operation that makes the action callable by users other than the owner requires API ownership proof. Accepted proof mechanisms are:

```text
well-known challenge
challenge embedded in the OpenAPI document
verified credential supplied by the owner
```

OpenAPI provenance is stored in `Action.source`:

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

`operation_key` is `x-juice-name` when present, otherwise `operationId`, otherwise a canonical derivation from method and path.

`import` is idempotent — re-running it against a changed spec applies this policy, matched by:

```text
owner_user_id + source.type + source.spec_url + source.operation_key
```

Import must not affect manual actions or actions imported from another server.

| Case                                | Result                                                              |
| ----------------------------------- | ------------------------------------------------------------------- |
| imported action unchanged           | preserve active state and stats                                     |
| any imported contract field changed | update action, deactivate, refresh lookup data, reset current stats |
| operation removed                   | deactivate, do not delete, reset current stats                      |
| new operation                       | create inactive action                                              |
| name collision with manual action   | reject                                                              |

An imported contract field includes description, method, path, parameter bindings, input schema, output schema, selected response, price, and execution source.

`operation_hash` covers the imported contract fields. It excludes stats, ratings, timestamps, and formatting. Import preserves `Action.id` for matched actions so transaction and receipt history remain attached.

OpenAPI webhooks are not imported as actions. They describe HTTP requests sent to Juice, not callable operations. Supporting them requires event-ingress configuration that validates the incoming payload and then calls `EmitEvent`; it must not bypass the normal listener and consume flow.

Unimporting OpenAPI actions deactivates matching actions; it does not delete them. It matches:

```text
owner_user_id + source.type=openapi + source.spec_url
```

For a single operation, it also matches `Action.name` or `source.operation_key`. Unimporting may remove ACL entries for the deactivated actions, but it must not delete transaction, receipt, rating, or trace history.

## 7. Adapters

### 7.1 WebAssembly

Use wazero. Scripts receive no ambient filesystem, network, environment, or process access; they receive only explicitly exported host functions. Each execution has a memory limit, timeout, deterministic `context.Context` cancellation, and an artifact-hash compiled-module cache. Store source and artifact; authorized users may inspect source; activation should precompile where possible; lifecycle compilation failures are typed errors.

Initial host surface:

```text
juice.call   normal contractor sub-call (§5.5)
juice.emit   emit through the kernel event path with the current trace id
juice.log    structured log associated with the current trace id
```

Scripts never receive raw user tokens. Script authority is mediated by the kernel:

```text
ScriptAuthority ⊆ KernelAuthority(trace, process, subject)
```

### 7.2 Local language services

`llm` exposes replaceable interfaces; concrete adapters call Ollama. The kernel must not import Ollama adapters. Model names and URL are configurable; defaults: URL `http://localhost:11434`, chat model `gemma4:26b`, embedding model `nomic-embed-text`.

```text
Embed(ctx, text) -> vector
Chat(ctx, messages) -> message
```

Tests use fake embedding and chat implementations.

### 7.3 Native lookup and chat

| Native action   | Rules                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `@sys/lookup`   | Public, grant-all, and callable only through `Call()`. Rank active actions for a natural-language query using an explicit tested formula combining semantic similarity and action statistics. Ranking storage is replaceable; brute-force cosine similarity over stored embeddings is acceptable. Input: required string `query`, optional integer `limit` defaulting to `10`. Output: `results[]` with `action_id`, `name`, `owner_handle`, `description`, and numeric `score`. Direct lookup exists only for platform diagnostics and is not exposed through user-facing APIs or WASM hosts. |
| `@sys/llm/chat` | Public, grant-all, and callable through `Call()`. Input: required `messages[]` of `{role, content}` plus optional prepended string `system`. Output: `message` object with `role` and `content`. Return `ErrInvalidState` if chat is unconfigured.                                                                                                                                                                                                                                                                                                                                             |

### 7.4 Statistics

Use incremental means:

```text
mean_(n+1) = mean_n + (x_(n+1) - mean_n) / (n + 1)
```

* `price_mean`: successful calls only; denominator `successes`.
* `latency_mean`: completed calls; denominator `uses`.
* `rating_mean`: rated calls only; denominator `rating_count`, never `uses`.

Resetting current stats means writing the defined missing-stat defaults for that action. It does not alter transactions, receipts, ratings, or trace history.

## 8. Events

### 8.1 Listener and emit

Creating a listener requires authenticated owner authority, an existing source user, exact event-name match, an existing target action, and owner permission to call that target. A listener stores neither process nor trace; the consumer supplies the process at consume time. Inactive listeners never fire. Deleting a listener requires its owner, atomically deactivates it, and purges all pending events.

`EmitEvent(source, event_name, args, causing_trace_id)` creates one queued event per active exact-match listener where `listener.source_user_id = emitter_user_id` and `listener.event_name = emitted_event_name`. Each event stores the raw arguments at emit time. Emit does not call targets, change balances, require an emitter process, or create transactions. It returns created event IDs, or an empty list when no listeners match. WASM `juice.emit` passes the current action trace ID.

### 8.2 Queue states and consume

| State     | Condition                             |
| --------- | ------------------------------------- |
| Pending   | `consumed_at = null`                  |
| In-flight | `consumed_at != null ∧ tx_id = null`  |
| Consumed  | `consumed_at != null ∧ tx_id != null` |

Only the listener owner may consume; the source user may not consume unless also the owner. Consumption atomically locks a pending event, then invokes the normal call path using the supplied `process_id`, stored `args_json`, and stored `causing_trace_id` as `FOLLOWS_FROM`. Success stores the resulting `tx_id`; failure resets the event to pending. Reject inactive listeners and already-consumed events with `ErrInvalidState`. Delivery is at-least-once; the lock prevents concurrent double-processing. Startup resets in-flight events to pending.

Polling returns pending events with `id`, `args_json`, `causing_trace_id`, and `created_at`. The listener owner or source user may poll.

## 9. Feedback, receipts, and signatures

### 9.1 Trace metrics and ratings

Every transaction references a trace. Trace lookup by process returns its execution tree; trace deletion must not remove transaction history. Every process and trace maintains cumulative cost and wall-clock latency aggregates automatically as transactions complete; no separate subtree-metric query is required. On descendant completion:

* `trace.cost` is the sum of descendant transaction gross amounts.
* `trace.latency_ms` is wall-clock elapsed time from trace creation until the latest descendant completion.

Only the direct buyer (`tx.owner_user_id`'s process owner) may call `RateTransaction(tx_id, rating)` with `rating ∈ {0, 1}`; any other subject returns `ErrUnauthorized`. Rating is a supervision operation and must not route through `Call()`. Ratings are immutable rows; transaction rows never change. A duplicate rating returns `ErrInvalidInput`. Ratings do not cascade; each rating applies only to the rated transaction. Update stats accordingly.

### 9.2 Receipts

Every committed success or failure has exactly one immutable receipt:

```text
∀ committed call. ∃ exactly one receipt r. r.tx_id = call.tx_id
```

* `issuer_user_id` is the local `@sys` user.
* `args_hash` and `reply_hash` are SHA-256 hashes of RFC 8785 JCS canonical `args_json` and `reply_json`.
* Receipt economic fields exactly match the transaction.
* `signature` is the platform Ed25519 signature over canonical receipt JSON excluding `signature`.
* Transaction and receipt creation are atomic in both `CommitCall` and `CommitFailedCall`.

### 9.3 Signed JSON

Receipts, ratings, and action manifests use RFC 8785 JSON Canonicalization Scheme (JCS) for signing and verification. The `script` package or a shared utility provides:

```text
CanonicalJSON(v any) ([]byte, error)
```

Generate and verify signatures only over `CanonicalJSON` output. A rating signature covers all fields except `signature` and is signed with the platform key. Property ordering uses UTF-8 byte order; this matches RFC 8785 UTF-16 ordering for all-ASCII property names, which is all this implementation uses.

### 9.4 Transaction access

Both parties to a transaction may read it in full — the buyer (`owner_user_id`) and the seller (the called action's owner) — so an owner can debug and audit calls to their action.

```text
CanReadTransaction(u, t) := u = t.owner_user_id ∨ u = Action(t.action_id).owner_user_id ∨ IsSuperuser(u)
```

Receipts stay an internal settlement artifact for federation and verification (§9.2, §12); there is no provider-receipt endpoint.

Invariant: every credit to an action owner is reconstructible from the transactions readable by that owner.

## 10. Authentication and errors

Human authentication uses an OAuth/OIDC-style flow. Browser login supports authorization code with PKCE; CLI login supports device authorization or loopback login. API calls use short-lived bearer access tokens. Refresh tokens, if used, are rotatable; logout revokes them server-side. Scripts never receive access or refresh tokens; internal script calls use trace-scoped authority.

Kernel errors are typed and mapped to stable CLI exit codes and HTTP statuses:

```text
ErrUnauthenticated ErrUnauthorized ErrNotFound ErrInvalidInput ErrInvalidState
ErrInsufficientFunds ErrExecutionFailed ErrSchemaViolation ErrTimeout ErrInternal
```

Messages are concise and user-facing; logs may include diagnostics.

## 11. Superuser, bootstrap, and deposits

The fixed platform superuser handle is `@sys`; it is not configurable. `@sys` has implicit admin authority on all actions. CLI admin authority compares the authenticated handle to `config.superuser_handle`.

First boot prompts only for a password and atomically creates:

```text
@sys user
config.superuser_handle = @sys
config.signing_public_key  = base64url Ed25519 public key
config.signing_private_key = base64url Ed25519 private key
config.jwt_secret          = 32 random bytes, hex-encoded
```

`config.signing_private_key` and `config.jwt_secret` are sensitive: never log or return them. Partial first boot must remain safely rerunnable. `JUICE_SECRET_KEY`, when set, overrides `config.jwt_secret` at runtime without altering the stored value.

Every server startup reads `config.superuser_handle` to confirm first boot and identify `@sys`. Before accepting requests:

```text
verify both signing keys exist; abort if either is missing
register and enable @sys/lookup and @sys/llm/chat if absent
apply grant-all to both native actions
reset in-flight events to pending (`consumed_at = NULL` where `consumed_at IS NOT NULL AND tx_id IS NULL`)
```

Bootstrap is idempotent. Native actions are owned by `@sys`, registered programmatically, and execute through `Call()`. Supervision operations must not be registered as native actions.

The superuser may suspend or unsuspend users. Suspension preserves data and causes every authenticated request to return `ErrUnauthenticated`.

`Kernel.Deposit(operator_user_id, target_user_id, amount, reason)` is a supervision operation available only through the admin CLI. It verifies that the operator is the configured superuser, requires a positive amount, and atomically credits `user.available` while creating a retrievable audit record. `reason` is optional but stored when provided. No HTTP endpoint exists.

## 12. Federation

### 12.1 Remote peers and discovery

A remote kernel is an ordinary user with `public_key` and `remote_base_url` set. Its public key is the remote platform signing key; its URL is the remote HTTP API base. Remote users cannot authenticate with passwords or receive tokens. Convention: unique handle `@<hostname>`; for example, `remote_base_url = https://remote.example.com`.

Discovery is manual only:

```text
juice remote add <url>                       fetch and validate <url>/.well-known/juice-kernel.json
juice remote list                            list registered peers
juice remote import <remote-handle> <action-name>   fetch signed manifest and create or update local http proxy action (idempotent)
juice remote unimport <remote-handle> <action-name> deactivate local proxy action
```

No gossip or crawling exists in v1. Imported actions are local `http` actions owned by the remote-user record.

Remote imports create `Action` rows with `kind = remote_proxy`. `source` is the federation call URL; `remote_action_id` is the remote action's ID; `artifact_hash` is the manifest hash. They do not expose or copy the remote action's internal implementation.

### 12.2 Action manifests

Only active public actions have signed manifests. Required fields:

```text
action_id owner_handle name description input_schema output_schema price kind
artifact_hash stats updated_at signature
```

`artifact_hash` matches `action.artifact_hash`; `stats` is a fixed-stat snapshot; `signature` is the platform Ed25519 signature over canonical JSON excluding `signature`.

Manifest descriptions and schemas are the canonical interface used by importing kernels for lookup and LLM function calling.

`remote import` is idempotent — re-running it re-fetches the manifest and applies this policy, matched by `owner_user_id + remote_action_id`:

| Case                                      | Result                                                                          |
| ----------------------------------------- | ------------------------------------------------------------------------------- |
| manifest unchanged                        | preserve active state and local stats                                           |
| any manifest contract field changed       | update proxy action, deactivate, refresh lookup data, reset current local stats |
| signature invalid                         | reject                                                                          |
| remote action disappeared                 | deactivate, do not delete, reset current local stats                            |
| remote action lost active or public state | deactivate, reset current local stats                                           |

Manifest contract fields include description, input schema, output schema, price, kind, artifact hash, and execution identity. Manifest stats do not overwrite local `Stats`. They may be stored as `StatTag` entries with source `remote_manifest` and used by lookup experiments.

Unimporting a remote action deactivates the local proxy action. It does not contact the remote kernel, delete history, or affect the remote action.

Expose:

```text
GET /.well-known/juice-kernel.json   -> public_key, handle (@sys), base_url
GET /v1/actions/{id}/manifest        -> signed public-action manifest
```

### 12.3 Cross-kernel calls and idempotency

A local remote-proxy action follows the normal local call path. Its HTTP handler sends a UUID v4 `idempotency_key`. On remote success, store `SHA-256(receipt_json)` (the remote receipt JSON) in `transaction.remote_receipt_hash`; v1 defers remote receipt-signature verification to later audit.

Inbound federation calls are authenticated: the calling kernel signs `{idempotency_key, action, timestamp}` with its Ed25519 private key; the local kernel verifies against the stored `public_key` and rejects timestamps older than 5 minutes. Unregistered callers are rejected.

Idempotency applies only to cross-kernel calls. A `pending` record is inserted before execution; a unique constraint on `(idempotency_key, counterparty_user_id)` prevents concurrent duplicates. On completion the record becomes `complete` and stores `result_json`. A `complete` replay returns the stored result; a `pending` replay returns 409. `IdempotencyRecord` gains `status` and `result_json` fields. `expires_at = created_at + 24 hours`; expired records may be purged.

## 13. CLI and HTTP server

Provide CLI `juice`. CLI commands use the same service layer as the server, support human-readable and JSON output, work directly against local SQLite where feasible, and each have at least one test.

Required commands:

```text
juice serve
juice user create                         juice user me
juice auth login                          juice auth logout
juice action create                       juice action update
juice action delete                       juice action enable
juice action disable                      juice action list
juice action acl grant                    juice action acl revoke
juice action grant-all                    juice action revoke-all
juice action import                       juice action unimport
juice action stats
juice process start                       juice process list
juice process show                        juice process fund
juice process end                         juice call
juice listener create                     juice listener list
juice listener show                       juice listener delete
juice event emit                          juice event list
juice event consume
juice tx list                             juice tx show
juice tx rate
juice health
juice admin user list                     juice admin user show
juice admin user suspend                  juice admin user unsuspend
juice admin user deposit                  juice admin action list
juice admin action disable                juice admin process list
juice admin tx list
juice remote add                          juice remote list
juice remote import                       juice remote unimport
```

OpenAPI action import uses:

```text
juice action import --openapi <spec-url>
juice action unimport --openapi <spec-url>
juice action unimport --openapi <spec-url> --name <action-name>
```

`juice serve` catches `SIGTERM` and `SIGINT`, stops accepting new requests, drains in-flight calls to completion, and exits cleanly. No `juice stop` command is provided; process lifecycle is managed by the OS or a process manager.

The HTTP API is primary. Every exposed endpoint has a corresponding CLI command. The server uses the shared kernel layer, propagates request, subject, process, trace, action, and transaction IDs into logs where available, maps authentication failure, authorization failure, invalid input, insufficient funds, missing resource, and internal failure to distinct HTTP statuses, and rate-limits authentication and account-creation endpoints per IP with HTTP `429` on excess.

Action read and list responses include a computed `action` field of the form `@owner/name` alongside the resource `id`, so that lookup results and list output can be used directly in call requests without a separate resolution step.

Admin operations are CLI-only: do not register `/v1/admin/*` routes. They authenticate the caller, reject a non-superuser with `ErrUnauthorized`, require `@sys`, stay outside `Call()`, and include user list/show/suspend/unsuspend/deposit, action list/disable, process list, and transaction list.

```text
juice admin user list
juice admin user show --id
juice admin user suspend --id
juice admin user unsuspend --id
juice admin user deposit --handle / --id
juice admin action list
juice admin action disable --id
juice admin process list
juice admin tx list
```

Required endpoint behavior:

| Endpoint                           | Rule                                                                                                                                        |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /health`                      | Unauthenticated server status; CLI: `juice health`.                                                                                         |
| `GET /v1/me`                       | Authenticated subject profile: `id`, `handle`, `email`, `available`, `locked`; reject suspended users before handler.                       |
| `PUT /v1/actions/{id}`             | Owner or action admin; apply update/deactivation rules.                                                                                     |
| `DELETE /v1/actions/{id}`          | Owner or action admin; preserve transaction history.                                                                                        |
| `POST /v1/actions/{id}/grant-all`  | Owner or action admin; CLI: `juice action grant-all --id`.                                                                                  |
| `POST /v1/actions/{id}/revoke-all` | Owner or action admin; CLI: `juice action revoke-all --id`.                                                                                 |
| `POST /v1/actions/import`          | Authenticated supervision operation; idempotent — imports and reconciles OpenAPI operations as inactive `http` actions. |
| `POST /v1/actions/unimport`        | Owner or action admin; deactivates actions with matching import provenance without deleting history.                     |
| `GET /v1/processes`                | Authenticated owner's processes ordered by descending `created_at`.                                                                         |
| `GET /v1/listeners`                | Authenticated owner's listeners.                                                                                                            |
| `GET /v1/listeners/{id}/events`    | Listener owner or source; return pending event fields.                                                                                      |
| `POST /v1/call`                    | Requires `args` field; rejected with `ErrInvalidInput` when absent. `{}` is valid for unconstrained inputs. `action` is `@owner/name`.     |
| `POST /v1/events/emit`             | Does not accept `source_user_id`; the event source is always the authenticated subject. Requires `args` field.                              |
| `POST /v1/auth/logout`             | Accept refresh token in body, revoke it, and return `ErrUnauthenticated` for missing or already-revoked tokens.                             |
| `GET /v1/transactions`             | Subject's transactions as buyer or seller per `CanReadTransaction`; `GET /v1/transactions/{id}` returns `ErrNotFound` to non-parties.       |

## 14. Logging and configuration

Log structured records to stderr and optionally to a file simultaneously. Diagnostic output (logs, progress, errors) must never go to stdout; stdout is reserved for resource payloads only. Format (`text` or `JSON`), file path, and level are configurable. Every kernel transition logs start and end; errors include stable codes; script logs include trace ID.

Required fields:

```text
time level event request_id subject_user_id process_id trace_id action_id tx_id
status duration_ms error
```

Load configuration from environment variables and an optional config file. Use safe local defaults where possible; commit no production secrets; reject invalid startup configuration clearly.

```text
JUICE_DB_PATH JUICE_LOG_LEVEL JUICE_LOG_FILE JUICE_FEE_BPS JUICE_FEE_RECIPIENT
JUICE_AUTH_ISSUER JUICE_AUTH_AUDIENCE JUICE_TOKEN_TTL JUICE_SECRET_KEY
JUICE_OLLAMA_URL JUICE_OLLAMA_CHAT_MODEL JUICE_OLLAMA_EMBED_MODEL
JUICE_SCRIPT_TIMEOUT_MS JUICE_SCRIPT_MEMORY_BYTES
```

## 15. Required automated tests

`go test ./...` must pass without external network access. Tests use temporary SQLite databases, fake Ollama and script adapters unless explicitly integration tests, no global state, and no order dependence.

Required suites:

```text
user creation
authentication token validation
action create/update/delete
action activation/deactivation
ACL grant/revoke/check
process create/fund/end
successful paid call
failed call with refund
insufficient funds
input schema rejection
output schema rejection
trace root and child creation
nested call trace tree
transaction creation
payment split
event listen/emit/poll/consume/unlisten
wasm script execution
wasm host function call
script timeout
script memory limit
lookup ranking with fake embeddings
stats update
CLI commands
logging smoke test
superuser first-boot prompt and config storage
suspended user rejected at authentication
direct buyer can rate transaction
non-buyer cannot rate transaction
contractor sub-call taxed on value added only (VAT model)
trace cost and latency updated on transaction completion
native action callable through Call()
non-superuser rejected from admin CLI commands
grant-all allows any authenticated user to call action
revoke-all removes open grant
bootstrap is idempotent
sub-calls charged to action owner's ephemeral process, not caller's process
caller process balance debited only by action.price
ephemeral process closed after sub-call completes
caller fully refunded when action owner has insufficient balance for sub-call
recursive sub-calls: each level charged to correct owner
event-triggered trace carries caused_by_trace_id of emitting action
direct call trace has null caused_by_trace_id
caused_by_trace_id references a trace in a different process
pending events absent from poll after successful consume
emit does not alter emitter balance or process balance
second ConsumeEvent on same event returns ErrInvalidState
ConsumeEvent against inactive listener returns ErrInvalidState
DeleteListener purges all pending events for that listener
ConsumeEvent fails and resets event to pending when process has insufficient funds
bootstrap resets in-flight events (consumed_at set, tx_id null) to pending
second rating on same transaction rejected with ErrInvalidInput
rating record created in ratings table, transaction row unchanged
ratings do not cascade
Ed25519 signing keypair present after first boot
zero-credit process satisfies fund locking for zero-price actions
Kernel.Deposit rejected with ErrUnauthorized for non-superuser caller
receipt created atomically with successful transaction commit
receipt created atomically with failed transaction commit
action owner reads transactions for calls to their action
non-party denied access to a transaction
OpenAPI import/unimport flow for API-owned actions
OpenAPI import compiles parameters and JSON body into one input schema
OpenAPI activation rejects incomplete schemas or missing descriptions
OpenAPI public activation requires ownership proof
OpenAPI import affects only matching OpenAPI-provenance actions
OpenAPI import preserves Action.id, deactivates on contract change, and resets current stats
OpenAPI webhooks enter through event ingress, not actions
remote import/unimport flow for signed manifests
remote import preserves Action.id, deactivates on manifest contract change, and does not overwrite local Stats
```

Direct invariant tests:

```text
balances are never negative
successful payment satisfies gross = net + fee
process.available + process.locked changes only by funding, settlement, or end
closed processes cannot call actions
inactive actions cannot be called by non-owners
call permission is required for execution
every call creates exactly one transaction
every nested call creates exactly one child trace
script calls cannot bypass ACL
suspended users cannot authenticate
native actions are always owned by the superuser
ratings do not cascade; each rating applies only to the rated transaction
trace.cost equals sum of descendant transaction gross amounts
caller.process.available decreases by at most action.price per call
ephemeral processes are always closed after sub-call completion
sub-call cost reversal does not occur on top-level failure
direct calls always have caused_by_trace_id = null
event-triggered calls always have caused_by_trace_id set
caused_by_trace_id never equals parent_trace_id (FOLLOWS_FROM ≠ CHILD_OF)
emitter balance is unchanged by EmitEvent regardless of how many listeners match
consumed events never appear in ListPendingEvents
pending events are absent after listener deletion
transaction row is immutable after commit (no field updated post-creation)
rating records reference valid tx_id and receipt_id
every credit to an action owner is reconstructible from transactions readable by that owner
imported action reimport or unimport never deletes transaction or receipt history
imported action current stats reset never mutates transaction, receipt, or rating rows
OpenAPI and remote imports create ordinary Actions, not separate action types
all imported actions execute only through Call()
```

Required user-flow tests:

```text
API owner imports an OpenAPI document, activates an action, grants public call access, and a caller executes it through Call()
API owner re-runs import against a changed OpenAPI document; the matched action is deactivated, stats reset, and historical transactions remain attached
API owner unimports an OpenAPI document; matching actions are deactivated and history remains attached
remote kernel is added, a signed manifest is imported, a caller executes the proxy through Call(), and local stats remain separate from manifest stats
remote proxy is unimported; the local proxy is deactivated and the remote kernel is unaffected
caller executes a paid action multiple times; the action owner lists transactions for their action and the sum of transaction net amounts equals the total credits received by the owner
```

## 16. Design rationale

These constraints preserve the original design intent:

| Decision                                                      | Rationale                                                                                                                                               |
| ------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Stable kernel interfaces and explicit transitions             | Kernel correctness must not depend on a transport or adapter.                                                                                           |
| Small function-named package and file set with per-file tests | Lower coupling, replaceable implementations, and visible coverage gaps.                                                                                 |
| Separate execution and supervision layers                     | Execution can be wrong; supervision supplies a correction signal that execution must not manipulate.                                                    |
| Lock credits before execution                                 | No action runs without reserving its budget.                                                                                                            |
| Full refund on v1 failures                                    | Conservative, observable, and testable accounting separates execution failure from settlement.                                                          |
| Validate input before lock and output before settlement       | Invalid requests are not charged and malformed replies are not paid.                                                                                    |
| Mediated WASM authority                                       | Scripts compose kernel operations without bypassing authorization or exfiltrating reusable credentials.                                                 |
| Replaceable lookup ranking                                    | Research experiments can change ranking without changing kernel semantics.                                                                              |
| Incremental fixed statistics plus optional tags               | Deterministic baseline metrics remain stable while experiments stay isolated; later risk-averse updates belong in experimental tags or lookup features. |
| Automatic trace aggregates                                    | Downstream cost and latency remain continuously visible without separate subtree queries.                                                               |
| Fixed `@sys` handle and signing keypair                       | System actions are stably addressable and the installation can issue verifiable receipts and manifests.                                                 |
| Structured terminal and file logs                             | Production operation and research reproduction require reconstructable execution records.                                                               |
| OpenAPI import as action supervision                          | API registration remains low-friction while execution remains uniform.                                                                                  |
| Federation as signed action import                            | Remote kernels expose action contracts without leaking implementation details.                                                                          |
| Deactivation on imported contract changes                     | Callers and LLMs never silently use a changed callable contract.                                                                                        |
| Unimport as deactivation                                      | Historical transactions, receipts, ratings, and traces remain auditable.                                                                                |
