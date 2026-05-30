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

- **Execution:** `Call()` performs action invocation, fund locking, tracing, settlement, statistics, and receipts. Native actions, WASM `juice.call`, event consumption, and remote proxies must use it.
- **Supervision:** direct authenticated kernel operations manage users, actions, processes, ACLs, ratings, and deposits. They must not route through `Call()`.
- A subject must not rate its own output or trigger rating propagation from execution code.

## 2. Implementation constraints

- Use Go. `go build ./...` and `go test ./...` must pass.
- Use ordinary Go interfaces for replaceable modules.
- `kernel` must not import CLI, HTTP, SQLite, wazero, or Ollama implementations.
- Use only these function-named production packages unless a dependency-cycle or cohesion reason requires otherwise:

```text
cmd/juice/   CLI and server entrypoint
kernel/      core objects and operational semantics
store/       persistence interface and SQLite implementation
script/      WebAssembly execution
llm/         local language and embedding interface
log/         structured logging
```

- Architectural package names such as `sqlite`, `wazero`, or `ollama` are forbidden; implementation-specific names may appear in concrete types or file names.
- Keep the package and source-file counts small. Do not split files for size alone. Every production source file must have a corresponding `_test.go` file with independent tests for its logic.

## 3. Data model

All IDs are stable opaque identifiers; action IDs are globally unique. Credit balances and prices are non-negative integers; credits are indivisible.

| Object | Required fields | Rules |
|---|---|---|
| `User` | `id`, `handle`, `email`, `available`, `locked`, `suspended_at`, `public_key`, `remote_base_url`, `created_at`, `updated_at` | `handle` is unique. A suspended user is rejected at every authenticated request with `ErrUnauthenticated`. `public_key`, when set, is a unique base64url Ed25519 32-byte public key. Local users have null `public_key` and `remote_base_url`; remote peers set both. |
| `Action` | `id`, `owner_user_id`, `name`, `kind`, `active`, `public`, `price`, `description`, `input_schema`, `output_schema`, `source`, `artifact_hash`, `created_at`, `updated_at` | `kind ∈ {http, wasm, native}`. `(owner_user_id, name)` is unique. An `active=false` action is not callable by non-owners. Public discovery returns active actions only unless an owner requests private state. Authorized users may inspect script source. Compiled artifacts are content-addressed by `artifact_hash`. |
| `ACLEntry` | `subject_user_id`, `action_id`, `permission`, `created_at` | `permission ∈ {read, call, admin}`. ACLs are direct user-to-action grants. `read` permits inspection; `call` permits execution; `admin` permits ACL and lifecycle changes. Owners implicitly have `admin`. |
| `Process` | `id`, `owner_user_id`, `available`, `locked`, `status`, `created_at`, `ended_at` | `status ∈ {open, closed}`. A process starts with user-provided funds and may start with zero credits (`available = 0`). Closing it returns all remaining funds to its owner. Closed processes cannot execute calls. |
| `Trace` | `id`, `process_id`, `parent_trace_id`, `caused_by_trace_id`, `cost`, `latency_ms`, `created_at` | Every process has one root trace. Choose one root convention consistently: `parent_trace_id = id` or `parent_trace_id = null`. Every direct `Call()` creates exactly one child trace. |
| `Transaction` | `id`, `process_id`, `trace_id`, `parent_trace_id`, `owner_user_id`, `subject_user_id`, `target_user_id`, `action_id`, `args_json`, `reply_json`, `status`, `gross`, `net`, `fee`, `reason`, `remote_receipt_hash`, `started_at`, `ended_at` | `status ∈ {success, failure}`. Every attempted call creates one immutable transaction. `remote_receipt_hash` is null locally and stores `SHA-256(remote_receipt_json)` for cross-kernel calls. |
| `Stats` | `uses`, `successes`, `failures`, `rating_count`, `price_mean`, `latency_mean`, `rating_mean`, `last_used_at` | Missing stats have defined defaults. `uses = successes + failures`. |
| `StatTag` | `action_id`, `key`, `value`, `source`, `updated_at` | Optional, queryable for lookup experiments, and never required for kernel execution. Experimental tags are namespaced by source and never alter fixed-stat semantics. |
| `Listener` | `id`, `owner_user_id`, `source_user_id`, `event_name`, `target_action_id`, `active`, `created_at` | A listener subscribes its owner to an exact `(source_user_id, event_name)` pair. |
| `Event` | `id`, `listener_id`, `args_json`, `causing_trace_id`, `consumed_at`, `tx_id`, `created_at` | Persistent queued work item. `causing_trace_id` is nullable. |
| `Deposit` | `id`, `operator_user_id`, `target_user_id`, `amount`, `reason`, `created_at` | Immutable audit record for a positive out-of-band superuser credit grant. |
| `Receipt` | `id`, `issuer_user_id`, `tx_id`, `trace_id`, `action_id`, `args_hash`, `reply_hash`, `status`, `gross`, `net`, `fee`, `reason`, `created_at`, `signature` | Immutable signed record for exactly one committed call. |
| `Rating` | `id`, `rated_tx_id`, `rated_receipt_id`, `rater_user_id`, `rating`, `created_at`, `signature` | Immutable signed feedback record. `rating ∈ {0, 1}`. At most one rating exists per transaction. `rated_receipt_id` may be null only for pre-receipt transactions. |
| `IdempotencyRecord` | `id`, `idempotency_key`, `counterparty_user_id`, `receipt_id`, `created_at`, `expires_at` | Used only for cross-kernel calls. |

### 3.1 ACL rule

ACL checks must occur inside the kernel path, not only at CLI or HTTP boundaries:

```text
CanCall(u, a) := Active(a) ∧ (Owner(u, a) ∨ Public(a) ∨ ACL(u, a, call) ∨ ACL(u, a, admin))
```

`public` is stored directly on the action. Grant-all and revoke-all toggle this flag without replacing direct ACL entries; only the owner or an action admin may invoke them.

### 3.2 Trace relationships

| Field | Relation | Scope | Use |
|---|---|---|---|
| `parent_trace_id` | `CHILD_OF` | Same process only | Direct calls within a process |
| `caused_by_trace_id` | `FOLLOWS_FROM` | Cross-process | Contractor sub-calls and event-triggered calls |

Rules:

- A child inherits its parent's `process_id`; trace trees are rooted per process. Every transaction references a trace. Trace lookup by process returns the execution tree; trace deletion never deletes transaction history.
- The kernel must reject a missing or cross-process supplied `parent_trace_id` with `ErrInvalidInput`.
- Direct calls have null `caused_by_trace_id`.
- Contractor ephemeral-process roots set `caused_by_trace_id` to the calling action trace.
- Event-triggered calls set `caused_by_trace_id` to the emitter trace stored at emit time.
- `caused_by_trace_id` must never be treated as `parent_trace_id`.
- Each completed descendant transaction updates ancestor `cost` and `latency_ms` automatically. The originating trace of a `FOLLOWS_FROM` relationship may already be closed; the referenced trace belongs to a different process.

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

| Operation | Atomic writes |
|---|---|
| Start process | user debit, process creation, root trace creation |
| Fund process | user debit, process credit |
| Successful call | transaction, receipt, locked-fund settlement, target payment, platform fee, trace metrics, stats |
| Failed call | transaction, receipt, full refund, trace metrics, stats |
| End process | process closure, return of remaining owner funds |
| Deposit | user credit, deposit record |
| Rating cascade | all new rating records |

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

<<<<<<< HEAD
- `id` must be globally unique.
- `native` actions are platform-owned and may only be registered by the superuser. Regular users may not create, update, or delete `native` actions.
- `(owner_user_id, name)` must be unique.
- `price` must be a non-negative integer.
- `active=false` actions must not be callable by non-owners.
- Public discovery must return only active public actions.
- Script source must be visible to authorized users.
- Compiled artifacts must be content-addressed by `artifact_hash`.

### 3.3 ACL entry

Access control must be direct from user to action.

Required fields:

```text
subject_user_id
action_id
permission
created_at
```

Allowed permissions:

```text
read
call
admin
```

Requirements:

- `call` permits execution.
- `read` permits metadata inspection.
- `admin` permits ACL and lifecycle changes.
- Owners implicitly have `admin`.
- ACL is always direct from user to action. 
- ACL checks must be performed inside the kernel call path, not only at the HTTP or CLI boundary.

Correctness condition:

```text
CanCall(u,a) := Active(a) ∧ (Owner(u,a) ∨ Public(a) ∨ ACL(u,a,call) ∨ ACL(u,a,admin)).
```

Where `Public(a)` is true when the action's `public` flag is set (see §19.5). This flag is stored directly on the action and checked without a separate ACL query.

### 3.4 Process

A process is a budgeted execution context.

Required fields:

```text
id
owner_user_id
available
locked
status
created_at
ended_at
```

Allowed status values:

```text
open
closed
```

Requirements:

- A process starts with user-provided funds.
- A process may be created with zero initial funds. `available = 0` is a valid initial state.
- Zero-credit processes may execute actions with `price = 0`. The fund locking invariant `available >= price` is satisfied by `0 >= 0`.
- A closed process cannot execute calls.
- Ending a process returns all process funds to the owner.
- Process balances must be non-negative integers.

### 3.5 Trace

A trace records causal structure.

Required fields:

```text
id
process_id
parent_trace_id
caused_by_trace_id
cost
latency_ms
created_at
```

Requirements:

- Every process must have one root trace.
- `cost` and `latency_ms` must be updated automatically as each descendant transaction completes.
- The root trace must have `parent_trace_id = id` or `parent_trace_id = null`; this choice must be consistent across the codebase.
- Every direct `Call()` invocation creates exactly one child trace within the calling process.
- A child trace must inherit the parent trace’s process id.
- The trace relation must form a rooted tree for each process.
- `caused_by_trace_id` must be null for direct calls that are not contractor sub-calls or event-triggered.
- For contractor sub-calls via `juice.call` (§5.7), the ephemeral process root trace must set `caused_by_trace_id` to the calling action’s trace ID. This is a FOLLOWS_FROM reference. The caller’s trace tree is not extended; the link is causal, not structural. The caller’s process is not the ephemeral process’s process.
- For event-triggered calls, `caused_by_trace_id` must be set to the trace ID of the emitting action at the moment of emit. This is a FOLLOWS_FROM reference, not a parent-child link. The referenced trace may belong to a different process.
- The kernel must validate that a supplied `parent_trace_id` exists and belongs to the same process before creating a child trace. An invalid or cross-process `parent_trace_id` must be rejected with `ErrInvalidInput`.

Correctness condition:

```text
∀ child. child.process_id = parent(child).process_id.
```

This condition applies only to CHILD_OF relationships (`parent_trace_id`). `caused_by_trace_id` crosses process boundaries and is exempt from this condition.

### 3.6 Transaction

A transaction records an attempted call.

Required fields:

```text
id
process_id
trace_id
parent_trace_id
owner_user_id
subject_user_id
target_user_id
action_id
args_json
reply_json
status
gross
net
fee
reason
remote_receipt_hash
started_at
ended_at
```

Allowed status values:

```text
success
failure
```

Requirements:

- Every attempted call must create one transaction.
- Successful paid calls must have `gross = net + fee`.
- Failed calls must use the explicit refund rule in Section 5.4.
- A transaction record is immutable after its `status`, `gross`, `net`, `fee`, `reason`, and `reply_json` are set at commit time. No field may be updated after the transaction is committed.
- `remote_receipt_hash` is nullable. For cross-kernel calls, it stores the SHA-256 hash of the remote kernel's receipt (see §21.4). For local calls it is null.

## 4. Persistence

### 4.1 Database

Juice must use SQLite.

Requirements:

- The store must be file-backed SQLite.
- WAL mode must be enabled by default.
- Migrations must be deterministic and stored in the repository.
- All monetary transitions must occur inside SQLite transactions.
- Transaction creation and fund settlement must occur within a single atomic SQLite transaction. A committed transaction record must never exist without corresponding balance settlement.
- Failed-call fund refund and failure transaction record must occur within a single atomic SQLite transaction. A refunded process must never exist without a corresponding failure transaction.
- Process creation, the associated user debit, and root trace creation must occur within a single atomic SQLite transaction. A funded process must never exist without a root trace.
- Tests must use temporary SQLite databases.
- No production feature may depend on an in-memory-only store.

### 4.2 Store interface

`kernel` must depend on a store interface, not directly on SQLite.

The store interface must support:

```text
CreateUser
ReadUser
ReadUserByPublicKey
ListUsers
SuspendUser
UnsuspendUser
CreateAction
ReadAction
UpdateAction
DeleteAction
ListAllActions
GrantACL
RevokeACL
CheckACL
CreateProcess
ReadProcess
EndProcess
ListAllProcesses
CreateTrace
CreateTransaction
ListTransactions
ListAllTransactions
ReadStats
UpdateStats
CreateListener
ReadListener
ListListeners
CreateEvent
ListPendingEvents
ConsumeEvent
PurgeListenerEvents
GetConfig
SetConfig
CreateDeposit
CreateReceipt
ReadReceipt
CreateRating
ReadRating
ListRatings
CreateIdempotencyRecord
ReadIdempotencyRecord
```

Note: `UpdateTransaction` is removed. Transaction records are immutable after creation (§3.6). All fields must be set at `CreateTransaction` time.

Justification: the kernel must be testable with fake stores and replaceable persistent stores.

## 5. Kernel operational semantics

### 5.1 Call preconditions

A call must check, in order:

```text
authenticated subject
existing open process
subject is process owner or has explicit process authority
existing action
active action
ACL permits call
valid input schema
signed receipts can be issued
process.available >= action.price
```

The kernel must return a typed error for each failed precondition.

### 5.2 Call transition

For a valid call with price `q`, the kernel must perform:
=======
### 5.2 Transition

For price `q`:
>>>>>>> 73da227669bd9782246edf035fed5dc44acfd77f

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

Only successful calls are charged in v1:

```text
gross = action.price
fee   = Fee(gross)
net   = gross - fee
gross = net + fee
```

<<<<<<< HEAD
Fee policy for v1:

```text
root local call           → fee = Fee(gross, JUICE_FEE_BPS)
contractor sub-call       → fee = 0, net = gross
remote proxy call         → fee = 0
remote destination call   → destination kernel policy
```

The `call_context` is determined by the kernel. Action providers do not control it.

Justification: contractor calls are production inputs. Charging a percentage fee on every internal edge taxes implementation depth and discourages composition.

### 5.6 Schema validation
=======
Success decreases the process and owner locked balances by `gross`, credits the target by `net`, and credits the fee recipient by `fee`.
>>>>>>> 73da227669bd9782246edf035fed5dc44acfd77f

Any failure before or after target execution starts charges zero, refunds the full locked gross amount, records a failure transaction, and exposes the failure class through `status` and `reason`. A later partial-failure policy must be represented explicitly in the transaction.

### 5.4 Schemas

Every action has input and output schemas. The first implementation may support a strict JSON Schema subset, but unsupported forms must fail action creation or update. Validate input before locking funds and output before successful settlement.

### 5.5 Contractor sub-calls

<<<<<<< HEAD
When `juice.call` is invoked inside an action's execution context, the kernel implements the contractor model:

1. The kernel creates an ephemeral process owned by the calling action's owner, funded from that owner's available balance for exactly the sub-action's price.
2. The sub-call executes against the ephemeral process following the standard §5.1–§5.5 call path, with fee = 0. The contractor receives the full `action.price`.
3. On sub-call completion, the ephemeral process is closed. On failure, unused locked funds are returned to the owner.
4. This applies recursively: each action in the call tree bears the cost of its own sub-calls.

The caller's process is debited only by the top-level `action.price`. Sub-call costs are isolated to the respective action owner's balance at each depth.

Each contractor sub-call creates a new ephemeral process with its own root trace. The root trace of the ephemeral process sets `caused_by_trace_id` to the calling action's trace ID (FOLLOWS_FROM). It does not set `parent_trace_id` to the caller's trace — contractor sub-calls cross process boundaries and are not CHILD_OF the calling trace. The caller's trace tree is structurally complete at its own process boundary; the causal link is for observability only.

Invariant:
=======
When a running action invokes WASM host function `juice.call(target, args)`:
>>>>>>> 73da227669bd9782246edf035fed5dc44acfd77f

```text
owner := calling action owner
create ephemeral process owned by owner
fund it from owner.available with exactly target.price
create ephemeral root trace with caused_by_trace_id = calling trace id
invoke normal Call(owner, ephemeral process, target, args)
close ephemeral process and return unused funds
```

Rules:

- The caller's process pays only the top-level action price.
- Each recursive action owner pays for its own direct sub-calls.
- The ephemeral root's causal link is `FOLLOWS_FROM`, not `CHILD_OF`.
- Insufficient owner funds fail the sub-call and propagate failure to the top-level call; the original caller is fully refunded.
- Previously settled descendant costs are not reversed.
- Ephemeral processes are always closed after completion.

```text
caller.process.available decreases by at most action.price per call, regardless of sub-call depth or cost
```

## 6. Action lifecycle

| Operation | Rules |
|---|---|
| Create | Create inactive by default. Validate owner, name, kind, non-negative price, description, schemas, and source. WASM creation validates or compiles its artifact. HTTP creation validates endpoint configuration without calling the endpoint unless explicitly requested. Reject non-HTTP(S), loopback, private IP ranges (RFC 1918), and link-local (`169.254.x.x`) source URLs at creation and activation. Normal `CreateAction` always rejects `Kind=native`; bootstrap uses `RegisterNativeAction` instead. |
| Activate | Require owner or admin. Initialize stats if absent. Reject invalid schema, missing source, invalid artifact, invalid HTTP URL, or invalid runtime configuration. |
| Update | Require owner or admin. Updating source, schema, kind, price, or endpoint deactivates unless explicitly marked safe. Recompute WASM `artifact_hash`; retain prior source and hash in transaction history. |
| Delete | Require owner or admin. Disable discovery, remove ACL entries, and preserve historical transactions; soft deletion is permitted. |
| Native | Register programmatically during bootstrap only, owned by `@sys`. Regular users cannot create, update, or delete native actions. |

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

`llm` exposes replaceable interfaces; concrete adapters call Ollama. The kernel must not import Ollama adapters. Model names are configurable; default chat model: `gemma4:26b`.

```text
Embed(ctx, text) -> vector
Chat(ctx, messages) -> message
```

Tests use fake embedding and chat implementations.

### 7.3 Native lookup and chat

| Native action | Rules |
|---|---|
| `@sys/lookup` | Public, grant-all, and callable only through `Call()`. Rank active actions for a natural-language query using an explicit tested formula combining semantic similarity and action statistics. Ranking storage is replaceable; brute-force cosine similarity over stored embeddings is acceptable. Input: required string `query`, optional integer `limit` defaulting to `10`. Output: `results[]` with `action_id`, `name`, `owner_handle`, `description`, and numeric `score`. Direct lookup exists only for platform diagnostics and is not exposed through user-facing APIs or WASM hosts. |
| `@sys/llm/chat` | Public, grant-all, and callable through `Call()`. Input: required `messages[]` of `{role, content}` plus optional prepended string `system`. Output: `message` object with `role` and `content`. Return `ErrInvalidState` if chat is unconfigured. |

### 7.4 Statistics

Use incremental means:

```text
mean_(n+1) = mean_n + (x_(n+1) - mean_n) / (n + 1)
```

- `price_mean`: successful calls only; denominator `successes`.
- `latency_mean`: completed calls; denominator `uses`.
- `rating_mean`: rated calls only; denominator `rating_count`, never `uses`.

## 8. Events

### 8.1 Listener and emit

Creating a listener requires authenticated owner authority, an existing source user, exact event-name match, an existing target action, and owner permission to call that target. A listener stores neither process nor trace; the consumer supplies the process at consume time. Inactive listeners never fire. Deleting a listener requires its owner, atomically deactivates it, and purges all pending events.

`EmitEvent(source, event_name, args, causing_trace_id)` creates one queued event per active exact-match listener where `listener.source_user_id = emitter_user_id` and `listener.event_name = emitted_event_name`. Each event stores the raw arguments at emit time. Emit does not call targets, change balances, require an emitter process, or create transactions. It returns created event IDs, or an empty list when no listeners match. WASM `juice.emit` passes the current action trace ID.

### 8.2 Queue states and consume

| State | Condition |
|---|---|
| Pending | `consumed_at = null` |
| In-flight | `consumed_at != null ∧ tx_id = null` |
| Consumed | `consumed_at != null ∧ tx_id != null` |

Only the listener owner may consume; the source user may not consume unless also the owner. Consumption atomically locks a pending event, then invokes the normal call path using the supplied `process_id`, stored `args_json`, and stored `causing_trace_id` as `FOLLOWS_FROM`. Success stores the resulting `tx_id`; failure resets the event to pending. Reject inactive listeners and already-consumed events with `ErrInvalidState`. Delivery is at-least-once; the lock prevents concurrent double-processing. Startup resets in-flight events to pending.

Polling returns pending events with `id`, `args_json`, `causing_trace_id`, and `created_at`. The listener owner or source user may poll.

## 9. Feedback, receipts, and signatures

### 9.1 Trace metrics and ratings

Every transaction references a trace. Trace lookup by process returns its execution tree; trace deletion must not remove transaction history. Every process and trace maintains cumulative cost and wall-clock latency aggregates automatically as transactions complete; no separate subtree-metric query is required. On descendant completion:

- `trace.cost` is the sum of descendant transaction gross amounts.
- `trace.latency_ms` is wall-clock elapsed time from trace creation until the latest descendant completion.

Any authenticated subject may call `RateTransaction(tx_id, rating)` with `rating ∈ {0, 1}`. Rating is a supervision operation and must not route through `Call()`. Verify the rating signature at submission. Ratings are immutable rows; transaction rows never change. A duplicate rating returns `ErrInvalidInput`. Rating `0` or `1` propagates recursively to unrated descendant transactions in the same trace tree without overwriting existing ratings; all inserts commit atomically. Update stats accordingly.

### 9.2 Receipts

Every committed success or failure has exactly one immutable receipt:

```text
∀ committed call. ∃ exactly one receipt r. r.tx_id = call.tx_id
```

- `issuer_user_id` is the local `@sys` user.
- `args_hash` and `reply_hash` are SHA-256 hashes of RFC 8785 JCS canonical `args_json` and `reply_json`.
- Receipt economic fields exactly match the transaction.
- `signature` is the platform Ed25519 signature over canonical receipt JSON excluding `signature`.
- Transaction and receipt creation are atomic in both `CommitCall` and `CommitFailedCall`.

### 9.3 Signed JSON

Receipts, ratings, and action manifests use RFC 8785 JSON Canonicalization Scheme (JCS) for signing and verification. The `script` package or a shared utility provides:

```text
CanonicalJSON(v any) ([]byte, error)
```

Generate and verify signatures only over `CanonicalJSON` output. A rating signature covers all fields except `signature`; ordinary raters use their private key and `@sys` uses the platform key.

## 10. Authentication and errors

Human authentication uses an OAuth/OIDC-style flow. Browser login supports authorization code with PKCE; CLI login supports device authorization or loopback login. API calls use short-lived bearer access tokens. Refresh tokens, if used, are rotatable; logout revokes them server-side. Scripts never receive access or refresh tokens; internal script calls use trace-scoped authority.

Kernel errors are typed and mapped to stable CLI exit codes and HTTP statuses:

```text
ErrUnauthenticated ErrUnauthorized ErrNotFound ErrInvalidInput ErrInvalidState
ErrInsufficientFunds ErrExecutionFailed ErrSchemaViolation ErrTimeout ErrInternal
```

Messages are concise and user-facing; logs may include diagnostics.

## 11. Superuser, bootstrap, and deposits

The fixed platform superuser handle is `@sys`; it is not configurable. The kernel itself has no privileged subject concept and enforces normal ACL rules. CLI admin authority compares the authenticated handle to `config.superuser_handle`.

First boot prompts only for a password and atomically creates:

```text
@sys user
config.superuser_handle = @sys
config.signing_public_key  = base64url Ed25519 public key
config.signing_private_key = base64url Ed25519 private key
```

The private key is sensitive: never log or return it. Partial first boot must remain safely rerunnable.

<<<<<<< HEAD
**Rating**

A rating is a platform-signed immutable record submitted by an authenticated subject for a transaction.

Required fields:
=======
Every server startup reads `config.superuser_handle` to confirm first boot and identify `@sys`. Before accepting requests:
>>>>>>> 73da227669bd9782246edf035fed5dc44acfd77f

```text
verify both signing keys exist; abort if either is missing
register and enable @sys/lookup and @sys/llm/chat if absent
apply grant-all to both native actions
reset in-flight events to pending (`consumed_at = NULL` where `consumed_at IS NOT NULL AND tx_id IS NULL`)
```

Bootstrap is idempotent. Native actions are owned by `@sys`, registered programmatically, and execute through `Call()`. Supervision operations must not be registered as native actions.

<<<<<<< HEAD
- Only the direct buyer may rate a transaction via `RateTransaction(subject, tx_id, rating)`.

```text
direct_buyer(tx) := owner_user_id of the process that paid for tx

CanRate(subject, tx) := subject.id = direct_buyer(tx)
```

For contractor sub-calls, the direct buyer is the owner of the ephemeral process. For event-triggered calls, the direct buyer is the owner of the consuming process.

- Each rating is stored as a new record in the `ratings` table. The transaction row is not modified (§3.6 immutability).
- A transaction may have at most one rating record. Submitting a second rating for the same transaction must be rejected with `ErrInvalidInput`.
- `rated_receipt_id` references the receipt issued for that transaction (see §20). It is null for transactions predating the receipt requirement.
- `signature` is the platform Ed25519 signature of the canonical rating record.
- Ratings do not cascade. A rating applies only to the rated transaction. Derived propagated scores may be computed as experimental statistics or tags, but they are not rating records.
- Rating is a supervision operation. It must not be callable through `Call()`.
=======
The superuser may suspend or unsuspend users. Suspension preserves data and causes every authenticated request to return `ErrUnauthenticated`.
>>>>>>> 73da227669bd9782246edf035fed5dc44acfd77f

`Kernel.Deposit(operator_user_id, target_user_id, amount, reason)` is a supervision operation available only through the admin CLI. It verifies that the operator is the configured superuser, requires a positive amount, and atomically credits `user.available` while creating a retrievable audit record. `reason` is optional but stored when provided. No HTTP endpoint exists.

## 12. Federation

### 12.1 Remote peers and discovery

A remote kernel is an ordinary user with `public_key` and `remote_base_url` set. Its public key is the remote platform signing key; its URL is the remote HTTP API base. Remote users cannot authenticate with passwords or receive tokens. Convention: unique handle `@<hostname>`; for example, `remote_base_url = https://remote.example.com`.

Discovery is manual only:

```text
juice remote add <url>                       fetch and validate <url>/.well-known/juice-kernel.json
juice remote list                            list registered peers
juice remote import <remote-handle> <action-name>   fetch signed manifest and create local http proxy action (kind = http)
```

No gossip or crawling exists in v1. Imported actions are local `http` actions owned by the remote-user record.

### 12.2 Action manifests

Only active public actions have signed manifests. Required fields:

```text
owner_handle name description input_schema output_schema price kind
artifact_hash stats updated_at signature
```

`artifact_hash` matches `action.artifact_hash`; `stats` is a fixed-stat snapshot; `signature` is the platform Ed25519 signature over canonical JSON excluding `signature`.

Expose:

```text
GET /.well-known/juice-kernel.json   -> public_key, handle (@sys), base_url
GET /v1/actions/{id}/manifest        -> signed public-action manifest
```

### 12.3 Cross-kernel calls and idempotency

A local remote-proxy action follows the normal local call path. Its HTTP handler sends a UUID v4 `idempotency_key`. On remote success, store `SHA-256(receipt_json)` (the remote receipt JSON) in `transaction.remote_receipt_hash`; v1 defers remote receipt-signature verification to later audit.

Idempotency applies only to cross-kernel calls. A repeated unexpired `(idempotency_key, counterparty_user_id)` returns the original receipt without re-execution. `expires_at = created_at + 24 hours`; expired records may be purged.

## 13. CLI and HTTP server

Provide CLI `juice`. CLI commands use the same service layer as the server, support human-readable and JSON output, work directly against local SQLite where feasible, and each have at least one test.

Required commands:

```text
juice serve
juice user create                         juice user me
juice auth login                          juice auth logout
juice action add                          juice action update
juice action delete                       juice action enable
juice action disable                      juice action list
juice action acl grant                    juice action acl revoke
juice action grant-all                    juice action revoke-all
juice process start                       juice process list
juice process show                        juice process fund
juice process end                         juice call
juice events listen                       juice events list
juice events unlisten                     juice events emit
juice events poll                         juice events consume
juice tx list                             juice tx show
juice tx rate                             juice stats show
juice lookup                              juice health
juice admin user list                     juice admin user show
juice admin user suspend                  juice admin user unsuspend
juice admin user deposit                  juice admin action list
juice admin action disable                juice admin process list
juice admin tx list
juice remote add                          juice remote list
juice remote import
```

The HTTP API is primary. Every exposed endpoint has a corresponding CLI command. The server uses the shared kernel layer, propagates request, subject, process, trace, action, and transaction IDs into logs where available, maps authentication failure, authorization failure, invalid input, insufficient funds, missing resource, and internal failure to distinct HTTP statuses, and rate-limits authentication and account-creation endpoints per IP with HTTP `429` on excess.

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

| Endpoint | Rule |
|---|---|
| `GET /health` | Unauthenticated server status; CLI: `juice health`. |
| `GET /v1/me` | Authenticated subject profile: `id`, `handle`, `email`, `available`, `locked`; reject suspended users before handler. |
| `PUT /v1/actions/{id}` | Owner or action admin; apply update/deactivation rules. |
| `DELETE /v1/actions/{id}` | Owner or action admin; preserve transaction history. |
| `POST /v1/actions/{id}/grant-all` | Owner or action admin; CLI: `juice action grant-all --id`. |
| `POST /v1/actions/{id}/revoke-all` | Owner or action admin; CLI: `juice action revoke-all --id`. |
| `GET /v1/processes` | Authenticated owner's processes ordered by descending `created_at`. |
| `GET /v1/listeners` | Authenticated owner's listeners. |
| `GET /v1/listeners/{id}/events` | Listener owner or source; return pending event fields. |
| `POST /v1/auth/logout` | Accept refresh token in body, revoke it, and return `ErrUnauthenticated` for missing or already-revoked tokens. |

## 14. Logging and configuration

Log structured records to terminal and file simultaneously. Format (`text` or `JSON`), file path, and level are configurable. Every kernel transition logs start and end; errors include stable codes; script logs include trace ID.

Required fields:

```text
time level event request_id subject_user_id process_id trace_id action_id tx_id
status duration_ms error
```

Load configuration from environment variables and an optional config file. Use safe local defaults where possible; commit no production secrets; reject invalid startup configuration clearly.

```text
JUICE_DB_PATH JUICE_LOG_LEVEL JUICE_LOG_FILE JUICE_FEE_BPS JUICE_FEE_RECIPIENT
JUICE_AUTH_ISSUER JUICE_AUTH_AUDIENCE JUICE_TOKEN_TTL
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
root local call charges fee
contractor sub-call has fee = 0, contractor receives full price
direct buyer can rate transaction
non-buyer cannot rate transaction
ratings do not cascade
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
Ed25519 signing keypair present after first boot
zero-credit process satisfies fund locking for zero-price actions
Kernel.Deposit rejected with ErrUnauthorized for non-superuser caller
receipt created atomically with successful transaction commit
receipt created atomically with failed transaction commit
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
contractor sub-calls do not incur platform fee
direct buyer identified as process owner
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
rating record references valid tx_id
```

<<<<<<< HEAD
## 17. Configuration

Juice must load configuration from environment variables and optional config file.

Required configuration keys:

```text
JUICE_DB_PATH
JUICE_LOG_LEVEL
JUICE_LOG_FILE
JUICE_FEE_BPS
JUICE_FEE_RECIPIENT
JUICE_AUTH_ISSUER
JUICE_AUTH_AUDIENCE
JUICE_TOKEN_TTL
JUICE_OLLAMA_URL
JUICE_OLLAMA_CHAT_MODEL
JUICE_OLLAMA_EMBED_MODEL
JUICE_SCRIPT_TIMEOUT_MS
JUICE_SCRIPT_MEMORY_BYTES
```

Requirements:

- Configuration must have safe local defaults where possible.
- `JUICE_FEE_BPS` must be between 0 and 10000; if nonzero, `JUICE_FEE_RECIPIENT` must resolve to a user before startup succeeds.
- Production secrets must not be committed.
- Invalid configuration must fail at startup with a clear error.

## 18. Error model

The kernel must use typed errors.

Required error classes:

```text
ErrUnauthenticated
ErrUnauthorized
ErrNotFound
ErrInvalidInput
ErrInvalidState
ErrInsufficientFunds
ErrExecutionFailed
ErrSchemaViolation
ErrTimeout
ErrInternal
```

Requirements:

- CLI and server interfaces must map typed errors to stable exit codes or HTTP status codes.
- Error messages must be concise and user-facing.
- Logs may contain additional diagnostic context.

## 19. Superuser and platform operations

### 19.1 Superuser account

One designated platform operator exists per installation.

Requirements:

- The superuser handle is always `@sys`. It is a platform constant, not configurable.
- On first boot, the operator is prompted only for a password. The handle `@sys` is set automatically.
- The `config` table records the sentinel key `superuser_handle = @sys` to indicate first boot has completed.
- On first boot, an Ed25519 signing keypair must be generated atomically with the superuser account and config entry, in the same database transaction. `config.signing_public_key` stores the public key (base64url). `config.signing_private_key` stores the private key (base64url, sensitive — never logged or returned by any API). It is an invalid state for `@sys` to exist without a signing keypair; a partial first-boot must leave the system re-runnable.
- On every startup, the server reads `config.superuser_handle` to confirm first boot and identify the superuser.
- Admin authority is enforced in the CLI by comparing the authenticated subject handle to `@sys`.
- Admin and superuser operations must not be exposed through HTTP endpoints.
- The kernel has no concept of superuser; it enforces normal ACL rules for all users.

Justification: a fixed handle makes system actions stably addressable across every deployment. An agent or script can always call `@sys/lookup` without out-of-band knowledge of the installation's superuser handle. The signing keypair allows this installation to issue verifiable receipts and action manifests.

### 19.2 User suspension

Requirements:

- The superuser may suspend or unsuspend any user.
- A suspended user is rejected at every authenticated request with `ErrUnauthenticated`.
- Suspension does not delete the user or their data.

### 19.3 Bootstrap

On every `juice serve` startup, after first-boot setup, before accepting requests:

1. Verify that `config.signing_public_key` and `config.signing_private_key` are present. Abort startup if either is missing.
2. Register and enable `/lookup` (KindNative, owned by the system superuser) if absent.
3. Apply grant-all on `/lookup`.
4. Reset all in-flight event consumptions: set `consumed_at = NULL` for every event where `consumed_at IS NOT NULL AND tx_id IS NULL`. These represent consume calls interrupted by a prior crash; resetting them to pending makes them retryable.

Bootstrap must be idempotent.

First-boot (superuser creation) must be atomic: the superuser account and its config entry must be created in a single database transaction. A partial first-boot (e.g., crash after user creation but before config write) must leave the system in a state where re-running bootstrap succeeds cleanly.

### 19.4 System native actions

Requirements:

- System actions are KindNative, owned by the superuser, registered at bootstrap.
- System actions execute through the normal kernel call path (`Call()`).
- The initial system actions are `/lookup` and `/llm/chat`, both owned by `@sys`, public, grant-all at bootstrap.
  - `@sys/lookup` (target handle `@sys`, action name `/lookup`): semantic action search.
  - `@sys/llm/chat` (target handle `@sys`, action name `/llm/chat`): chat completion via the configured language model.
- Supervision operations must not be registered as native actions (see §2.3).

### 19.5 Public access control

Requirements:

- An action may be made callable by all authenticated users via grant-all.
- grant-all and revoke-all are operations on an action, not a change to the ACL model.
- Only the action owner or a user with admin permission on the action may call grant-all or revoke-all.
- grant-all does not replace or remove existing per-user ACL entries.

### 19.6 Admin operations

The following operations are restricted to the superuser and must exist only as CLI commands.

Requirements:

- Admin operations must not have HTTP endpoints.
- The HTTP server must not register `/v1/admin/*` routes.
- CLI admin commands must authenticate the caller and compare the authenticated subject's handle to `config.superuser_handle`.
- A non-superuser attempting any admin CLI command must be rejected with `ErrUnauthorized`.
- Admin operations must not route through `Call()`.

User management:

```text
juice admin user list
juice admin user show --id
juice admin user suspend --id
juice admin user unsuspend --id
juice admin user deposit --handle / --id
```

Action management:

```text
juice admin action list
juice admin action disable --id
```

Process management:

```text
juice admin process list
```

Transaction management:

```text
juice admin tx list
```

### 19.7 Access control endpoints

Require action owner or action-admin permission:

```text
POST /v1/actions/{id}/grant-all         juice action grant-all --id
POST /v1/actions/{id}/revoke-all        juice action revoke-all --id
```

### 19.8 Health endpoint

Unauthenticated. Returns server status.

```text
GET /health                             juice health
```

### 19.9 User self-view endpoint

Returns the authenticated user's own profile.

```text
GET /v1/me                              juice user me
```

Requirements:

- Requires authentication (Bearer token).
- Returns the authenticated user's id, handle, email, available balance, and locked balance.
- A suspended user must be rejected with `ErrUnauthenticated` before reaching this handler.

### 19.10 Deposits

The superuser may add credits directly to any user's available balance as an out-of-band platform operation.

Required deposit fields:

```text
id
operator_user_id
target_user_id
amount
reason
created_at
```

Requirements:

- Only the superuser may issue a deposit.
- Deposit authorization must be enforced inside `Kernel.Deposit`, not only at the CLI boundary. `Kernel.Deposit` must verify that `operator_user_id` matches the configured superuser and return `ErrUnauthorized` for any other caller.
- Amount must be a positive integer.
- A deposit must atomically increase `user.available` by the specified amount inside a single SQLite transaction.
- Each deposit must be persisted as an audit record.
- `reason` is optional but stored when provided.
- Deposits must not route through `Call()`. They are a supervision operation (§2.3).
- Deposits must only be available through the admin CLI; they must not have an HTTP endpoint.

Required tests:

- Deposit increases target user's available balance by the exact amount.
- Non-superuser deposit attempt is rejected with `ErrUnauthorized`.
- Zero or negative amount is rejected with `ErrInvalidInput`.
- Deposit record is retrievable after creation.

### 19.11 Action management endpoints

Require action owner or action-admin permission:

```text
PUT /v1/actions/{id}                    juice action update --id
DELETE /v1/actions/{id}                 juice action delete --id
```

Requirements:

- `PUT /v1/actions/{id}` applies §6.3 update semantics: updating source, schema, kind, price, or endpoint deactivates the action unless explicitly marked safe.
- `DELETE /v1/actions/{id}` applies §6.4 deletion semantics: historical transactions are preserved.

### 19.12 Owner list endpoints

Return resources owned by the authenticated user:

```text
GET /v1/processes                       juice process list
GET /v1/listeners                       juice events list
```

Requirements:

- `GET /v1/processes` returns all processes owned by the authenticated subject, ordered by `created_at` descending.
- `GET /v1/listeners` returns all listeners owned by the authenticated subject.

### 19.13 Events poll endpoint

Returns pending events for a listener:

```text
GET /v1/listeners/{id}/events           juice events poll --id
```

Requirements:

- Requires the authenticated subject to be the listener owner or the source user (§11.5).
- Returns pending (unconsumed) events with `id`, `args_json`, `causing_trace_id`, and `created_at`.

### 19.14 Auth logout endpoint

Revokes the caller's refresh token:

```text
POST /v1/auth/logout                    juice auth logout
```

Requirements:

- Accepts the refresh token in the request body.
- Marks the token revoked; subsequent refresh attempts with that token must return `ErrUnauthenticated`.
- A missing or already-revoked token must return `ErrUnauthenticated`.

## 20. Receipts

### 20.1 Receipt object

A receipt is an immutable, cryptographically signed record of a committed call. Every call — successful or failed — produces exactly one receipt.

Required fields:

```text
id
issuer_user_id
tx_id
trace_id
action_id
args_hash
reply_hash
status
gross
net
fee
reason
created_at
signature
```

Requirements:

- `issuer_user_id` is the `@sys` user of the kernel that executed the call.
- `args_hash` and `reply_hash` are SHA-256 hashes of the canonical (RFC 8785 JCS) JSON serialisation of `args_json` and `reply_json` respectively.
- `status`, `gross`, `net`, `fee`, and `reason` must match the corresponding transaction fields exactly.
- `signature` is the Ed25519 signature of the canonical JSON serialisation of all other receipt fields (excluding `signature` itself), signed with `config.signing_private_key`.
- A receipt is immutable. No field may change after creation.
- Receipt creation must be atomic with transaction commit: `CommitCall` and `CommitFailedCall` must create the transaction record and the receipt record in the same SQLite transaction. A committed transaction without a receipt is an invalid state.

### 20.2 Receipt invariant

```text
∀ committed call. ∃ exactly one receipt r. r.tx_id = call.tx_id.
```

The kernel must enforce this invariant. It is not acceptable to create a transaction without a receipt or a receipt without a transaction.

### 20.3 Ratings (separate table)

A rating record references both a transaction and its receipt.

Required fields:

```text
id
rated_tx_id
rated_receipt_id
rater_user_id
rating
created_at
signature
```

Requirements:

- `rating` must be in `{0, 1}`.
- `rated_receipt_id` is nullable for transactions that predate the receipt requirement.
- `signature` is the platform Ed25519 signature of the canonical JSON serialisation of all other rating fields (excluding `signature`).
- A transaction may have at most one rating record. A duplicate must be rejected with `ErrInvalidInput`.
- Ratings do not cascade (§10.2). Only the rated transaction receives a rating record.

### 20.4 Canonical serialisation

All signed objects (receipts, ratings, action manifests — see §21.3) must use RFC 8785 JSON Canonicalization Scheme (JCS) as the serialisation standard for signature generation and verification.

Requirements:

- The `script` package or a shared utility must provide a `CanonicalJSON(v any) ([]byte, error)` function.
- Signature generation must call `CanonicalJSON` before signing.
- Signature verification must call `CanonicalJSON` before verifying.

## 21. Federation

### 21.1 Remote kernels as ordinary users

A remote kernel is represented as an ordinary user record with `public_key` and `remote_base_url` set (§3.1). No new kernel object type is introduced.

Requirements:

- `public_key` is the Ed25519 public key of the remote kernel (its `config.signing_public_key`).
- `remote_base_url` is the base URL of the remote kernel's HTTP API (e.g. `https://remote.example.com`).
- A remote kernel user may own actions. Those actions have `kind = http` and are created locally by the `juice remote import` command.
- A remote kernel user may not authenticate with a password. It has no refresh token and no access token.
- The remote kernel user's `handle` must be unique and must not collide with local user handles. Convention: `@<hostname>`.

### 21.2 Peer discovery

In v1, peer discovery is manual only.

Requirements:

- `juice remote add <url>` fetches the well-known metadata endpoint at `<url>/.well-known/juice-kernel.json`, validates the response, and creates or updates the local user record for that remote kernel.
- `juice remote list` lists all remote kernel users registered locally.
- `juice remote import <remote-handle> <action-name>` fetches the remote kernel's action manifest for the named action and creates a local `http` action that proxies calls to the remote endpoint.
- There is no automatic peer discovery in v1. Kernels do not gossip or crawl for peers.

### 21.3 Action manifests

An action manifest is a signed, exportable description of a public active action.

Required fields:

```text
owner_handle
name
description
input_schema
output_schema
price
kind
artifact_hash
stats
updated_at
signature
```

Requirements:

- Only active public actions may be included in a manifest.
- `artifact_hash` is the content hash of the compiled artifact (same as `action.artifact_hash`).
- `stats` is a snapshot of the action's fixed statistics fields (§9.1) at manifest generation time.
- `signature` is the Ed25519 signature of the canonical JSON of all other manifest fields, signed with `config.signing_private_key`.
- The kernel must expose a manifest endpoint: `GET /v1/actions/{id}/manifest`.
- The well-known metadata endpoint `GET /.well-known/juice-kernel.json` must return at minimum: `public_key` (base64url), `handle` (`@sys`), and `base_url`.

### 21.4 Cross-kernel calls

When a local action with `kind = http` represents a remote kernel action, calls to it follow the normal §5.1–§5.5 call path locally. The remote execution is initiated by the HTTP action handler.

Requirements:

- The HTTP action handler for a remote action must include the local kernel's `idempotency_key` in the remote call request.
- On a successful remote call, the remote kernel returns its receipt. The local kernel stores `SHA-256(receipt_json)` as `transaction.remote_receipt_hash`.
- The local kernel does not re-verify the remote receipt signature in v1. Verification is deferred to a future audit step.

### 21.5 Idempotency

Cross-kernel calls must be idempotent. A repeated request with the same idempotency key from the same counterparty must return the original receipt without re-executing.

Required fields:

```text
id
idempotency_key
counterparty_user_id
receipt_id
created_at
expires_at
```

Requirements:

- `idempotency_key` is a UUID v4 generated by the local kernel at call time.
- `counterparty_user_id` is the remote kernel's local user id.
- `expires_at` is `created_at + 24 hours`.
- If a request arrives with an `idempotency_key` that matches an unexpired record for the same `counterparty_user_id`, the kernel must return the original receipt without executing again.
- Expired idempotency records may be purged.
- Idempotency applies to cross-kernel calls only. Local calls do not use idempotency keys.
=======
## 16. Design rationale

These constraints preserve the original design intent:

| Decision | Rationale |
|---|---|
| Stable kernel interfaces and explicit transitions | Kernel correctness must not depend on a transport or adapter. |
| Small function-named package and file set with per-file tests | Lower coupling, replaceable implementations, and visible coverage gaps. |
| Separate execution and supervision layers | Execution can be wrong; supervision supplies a correction signal that execution must not manipulate. |
| Lock credits before execution | No action runs without reserving its budget. |
| Full refund on v1 failures | Conservative, observable, and testable accounting separates execution failure from settlement. |
| Validate input before lock and output before settlement | Invalid requests are not charged and malformed replies are not paid. |
| Mediated WASM authority | Scripts compose kernel operations without bypassing authorization or exfiltrating reusable credentials. |
| Replaceable lookup ranking | Research experiments can change ranking without changing kernel semantics. |
| Incremental fixed statistics plus optional tags | Deterministic baseline metrics remain stable while experiments stay isolated; later risk-averse updates belong in experimental tags or lookup features. |
| Automatic trace aggregates | Downstream cost and latency remain continuously visible without separate subtree queries. |
| Fixed `@sys` handle and signing keypair | System actions are stably addressable and the installation can issue verifiable receipts and manifests. |
| Structured terminal and file logs | Production operation and research reproduction require reconstructable execution records. |
>>>>>>> 73da227669bd9782246edf035fed5dc44acfd77f
