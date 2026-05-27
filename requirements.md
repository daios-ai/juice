# Juice Kernel Requirements

Version: 0.1  
Status: implementation requirements  
Codename: `juice`

## 1. Purpose

Juice is a small production kernel and research platform for callable actions. It must support action registration, strict access control, budgeted execution, auditable traces, accounting, event-triggered calls, WebAssembly scripts, local language services, action discovery, platform supervision, and complete automated tests.

The kernel must preserve one central semantic object:

```text
Call(subject, process, action, args)
```

A call is valid exactly when the subject is authenticated, the action exists, the action is active, the subject has permission to call the action, the process has sufficient available funds, and the arguments validate against the action schema.

## 2. Design constraints

### 2.1 Implementation language

Juice must be implemented in Go.

Requirements:

- The implementation must build with `go build ./...`.
- The implementation must test with `go test ./...`.
- The kernel must use ordinary Go interfaces for replaceable modules.
- The kernel package must not import CLI, HTTP, SQLite, wazero, or Ollama packages directly.

Justification: the kernel is a transition system. Its correctness depends on stable interfaces and explicit state transitions, not on a particular transport or adapter.

### 2.2 Codebase size and layout

The codebase must use a small number of function-named packages.

Required package layout:

```text
cmd/juice/      command line entrypoint
kernel/         core objects and operational semantics
store/          persistence interface and SQLite implementation
script/         WebAssembly script execution
llm/            local language and embedding interface
log/            structured logging
```

Rules:

- Package names must describe function, not implementation.
- Top-level package names such as `sqlite`, `wazero`, and `ollama` are forbidden.
- Implementation-specific names may appear in file names or concrete types, but not in architectural package names.
- The initial implementation should remain below six production packages, excluding tests.
- New packages require a demonstrated dependency-cycle or cohesion reason.
- The number of source files must be kept small. New files require a cohesion reason; splitting a file for size alone is not sufficient.
- Every source file must have a corresponding `_test.go` file with independent tests for the logic in that file.

Justification: small package count lowers coupling. Function-named packages permit implementation replacement without changing the conceptual architecture. Per-file tests make coverage gaps visible and keep test files co-located with the code they exercise.

### 2.3 Machine layer and human supervision layer

Juice must maintain a strict conceptual split between machine execution and human supervision.

Requirements:

- `Call()` is the machine layer. It is the sole execution path for AI agents invoking actions. All action execution, fund locking, tracing, and settlement occur through `Call()`.
- Direct kernel operations — user and action lifecycle, process management, rating — form the human supervision layer. Humans interact with the system through these to observe, correct, and guide machine behavior.
- No human supervision operation may be routed through `Call()`. An AI agent must never be able to rate its own outputs or trigger rating propagation.
- This split is an architectural invariant, not an implementation detail.

Justification: AI agents may produce incorrect results. Human supervision provides the correction signal. Mixing the two layers would allow machines to interfere with their own feedback, undermining the integrity of the supervision signal.

## 3. Core objects

The implementation must define the following core objects in `kernel`.

### 3.1 User

A user is an authenticated subject with balances.

Required fields:

```text
id
handle
email
available
locked
suspended_at
created_at
updated_at
```

Requirements:

- User ids must be stable opaque identifiers.
- Handles must be unique.
- Balances must be non-negative integers.
- Balance units must be indivisible credits.
- A suspended user must be rejected at every authenticated request with `ErrUnauthenticated`.

### 3.2 Action

An action is a callable capability.

Required fields:

```text
id
owner_user_id
name
kind
active
price
description
input_schema
output_schema
source
artifact_hash
created_at
updated_at
```

Allowed kinds:

```text
http
wasm
native
```

Requirements:

- `id` must be globally unique.
- `native` actions are platform-owned and may only be registered by the superuser. Regular users may not create, update, or delete `native` actions.
- `(owner_user_id, name)` must be unique.
- `price` must be a non-negative integer.
- `active=false` actions must not be callable by non-owners.
- Public discovery must return only active actions unless an owner explicitly requests private state.
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
- Every nested call must create exactly one child trace.
- A child trace must inherit the parent trace’s process id.
- The trace relation must form a rooted tree for each process.
- `caused_by_trace_id` must be null for direct calls.
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
started_at
ended_at
rating
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
- Ratings must be nullable and, when present, must be in `{0,1}` in the first implementation.

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
UpdateTransaction
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
```

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
process.available >= action.price
```

The kernel must return a typed error for each failed precondition.

### 5.2 Call transition

For a valid call with price `q`, the kernel must perform:

```text
create child trace
lock q credits in process
execute action
  sub-calls within execution (juice.call host function):
    create ephemeral process owned by the calling action's owner,
      funded from owner's available balance for exactly sub-action.price
    execute sub-call against ephemeral process following §5.1–§5.5
    on sub-call completion: close ephemeral process;
      return unused locked funds to owner on failure
record transaction
on success: transfer net to target and fee to fee recipient
on failure: apply refund rule
update action statistics
return reply with tx_id and trace_id
```

### 5.3 Fund locking

Before execution, the process transition must be:

```text
available := available - q
locked    := locked + q
```

This transition is valid only when:

```text
q >= 0 ∧ available >= q
```

Justification: this is the budget safety invariant. No action can execute unless the process has already reserved the required funds.

### 5.4 Failure accounting

Juice must implement the following explicit refund rule:

```text
If execution fails before the target action starts, refund the full gross amount.
If execution starts and returns failure, charge zero in the first implementation and refund the full gross amount.
If a later policy charges partial failure cost, that policy must be represented explicitly in the transaction.
```

Requirements:

- The first implementation must charge only successful calls.
- A failed call must not leak locked funds.
- A failed call must still create a transaction.
- The fund refund and the failure transaction record must be committed atomically in a single store operation. It must not be possible for funds to be refunded without a transaction record, or for a failure transaction to be recorded without the corresponding refund.
- Transaction status and reason must make the failure class observable.

Justification: this rule is conservative and testable. It separates execution failure from economic settlement.

If the action owner has insufficient balance to fund the ephemeral process for a sub-call, the sub-call fails. The failure propagates to the top-level call and the original caller is fully refunded. Sub-call costs already settled from ephemeral processes before the point of failure are not reversed. Each action owner absorbs the costs of their own sub-calls.

### 5.5 Payment transition

For a successful call:

```text
gross = action.price
fee   = Fee(gross)
net   = gross - fee
```

The settlement transition must satisfy:

```text
owner.locked decreases by gross
process.locked decreases by gross
target.available increases by net
fee_recipient.available increases by fee
```

Correctness condition:

```text
gross = net + fee
```

### 5.6 Schema validation

Requirements:

- Every action must have input and output schemas.
- The first implementation may support a strict JSON Schema subset.
- Unsupported schema forms must fail at action creation or update time.
- Calls must validate inputs before funds are locked.
- Replies must validate outputs before successful settlement.

Justification: validating inputs before locking funds avoids charging invalid calls. Validating outputs before settlement prevents payment for malformed replies.

### 5.7 Contractor execution model

When `juice.call` is invoked inside an action's execution context, the kernel implements the contractor model:

1. The kernel creates an ephemeral process owned by the calling action's owner, funded from that owner's available balance for exactly the sub-action's price.
2. The sub-call executes against the ephemeral process following the standard §5.1–§5.5 call path.
3. On sub-call completion, the ephemeral process is closed. On failure, unused locked funds are returned to the owner.
4. This applies recursively: each action in the call tree bears the cost of its own sub-calls.

The caller's process is debited only by the top-level `action.price`. Sub-call costs are isolated to the respective action owner's balance at each depth.

The process hierarchy mirrors the trace hierarchy: each trace node corresponds to an ephemeral process owned by the action's owner at that level.

Invariant:

```text
caller.process.available decreases by at most action.price per call,
regardless of sub-call depth or cost.
```

If the action owner has insufficient balance to fund a sub-call, the sub-call fails, the top-level call fails, and the original caller is fully refunded. Action owners absorb costs already incurred by their own sub-calls.

## 6. Action lifecycle

### 6.1 Creation

Requirements:

- Actions must be created inactive by default.
- Action creation must validate owner, name, kind, price, description, schemas, and source.
- For `wasm` actions, creation must compile or validate the artifact before the action can be activated.
- For `http` actions, creation must validate endpoint configuration without calling the endpoint unless explicitly requested.
- For `http` actions, the `source` URL must be validated at creation and activation time. Loopback addresses, private IP ranges (RFC 1918), link-local addresses (169.254.x.x), and non-HTTP(S) schemes must be rejected.
- Only the kernel bootstrap process may register native actions. `CreateAction` must reject `Kind=native` from all callers; bootstrap uses `RegisterNativeAction` instead.

### 6.2 Activation

Requirements:

- Activation must require owner or admin permission.
- Activation must initialize action statistics if absent.
- Activation must fail if the action has invalid schema, missing source, invalid artifact, or invalid runtime configuration.

### 6.3 Update

Requirements:

- Updating source, schema, kind, price, or endpoint must deactivate the action unless the update is explicitly marked safe.
- Updating a `wasm` action must recompute `artifact_hash`.
- Previous script source and artifact hash must remain available in transaction history.

### 6.4 Deletion

Requirements:

- Deletion must require owner or admin permission.
- Deletion must not remove historical transactions.
- Deletion must remove ACL entries and disable discovery.
- Deletion may mark the action deleted rather than physically removing it.

### 6.5 Native actions

Requirements:

- Native actions are registered programmatically at bootstrap, not through the normal creation flow.
- Native actions must be owned by the superuser.
- Native actions must not be creatable, updatable, or deletable by regular users.

## 7. WebAssembly scripting

### 7.1 Runtime

Juice must use wazero for WebAssembly execution.

Requirements:

- Scripts must execute in a sandboxed wazero runtime.
- Scripts must not receive ambient filesystem, network, environment, or process access.
- Scripts must receive only the host functions explicitly exported by Juice.
- Each script execution must have a timeout.
- Each script execution must have a memory limit.
- Each script execution must have deterministic cancellation through `context.Context`.
- Compiled modules must be cached by `artifact_hash`.

### 7.2 Source and compilation

Requirements:

- The user must be able to inspect script source.
- The system must store script source.
- The system must store or cache the compiled artifact.
- The activation path must precompile scripts when possible.
- Compilation errors must be surfaced as typed action lifecycle errors.

### 7.3 Host functions

The initial host function surface must be small:

```text
juice.call
juice.emit
juice.log
```

Requirements:

- `juice.call` must call another action through the kernel call path.
- `juice.call` must enforce ACL, accounting, trace creation, and schema validation.
- `juice.call` must create an ephemeral process owned by the calling action's owner and use it as the process context for the sub-call (contractor model, §5.7).
- `juice.emit` must emit an event through the kernel event path.
- `juice.emit` must store the current trace ID of the emitting action as `causing_trace_id` in each created event record. This ID is passed to the kernel call path at consume time and recorded as `caused_by_trace_id` on the listener-triggered trace (FOLLOWS_FROM).
- `juice.log` must write structured logs under the current trace id.
- Host functions must never expose raw user tokens to guest code.

Correctness condition:

```text
ScriptAuthority ⊆ KernelAuthority(trace, process, subject).
```

Justification: scripts may compose kernel operations, but cannot bypass kernel authorization.

## 8. Lookup and local language services

### 8.1 Ollama adapter

Juice must use Ollama for local language models and sentence embeddings.

Requirements:

- `llm` must expose an interface for chat and embeddings.
- The concrete implementation must call Ollama.
- The kernel must not import the Ollama adapter.
- Embedding model name must be configurable.
- Chat model name must be configurable.
- Lookup tests must use a fake embedding implementation.

Required interface shape:

```text
Embed(ctx, text) -> vector
Chat(ctx, messages) -> message
```

### 8.2 Lookup module

Requirements:

- Lookup must rank active actions for a natural-language query.
- Lookup must combine semantic similarity with action statistics.
- The ranking formula must be explicit and tested.
- The lookup table must be replaceable without changing kernel semantics.
- The first implementation may use brute-force cosine similarity over stored embeddings.
- Lookup is exposed as the system native action `/lookup` (owned by the system superuser), callable through `Call()` by any authenticated user (grant-all applied at bootstrap).

Justification: lookup is a research module. The kernel requires only a ranked list of action ids, not a specific ranking algorithm.

## 9. Action statistics

### 9.1 Fixed fields

Each action must track these fixed statistics:

```text
uses
successes
failures
price_mean
latency_mean
rating_mean
last_used_at
```

Requirements:

- `uses = successes + failures`.
- `price_mean` must be computed from successful calls.
- `latency_mean` must be computed from completed calls.
- `rating_mean` must be computed from rated calls only. The denominator for the incremental mean is the number of previous ratings, not `uses`. A separate `rating_count` field must track this.
- Missing statistics must have defined defaults.

### 9.2 Extension tags

Action statistics must support extension tags.

Required tag fields:

```text
action_id
key
value
source
updated_at
```

Requirements:

- Tags must not be required for kernel execution.
- Tags must be queryable for lookup experiments.
- Tags must be namespaced by source when generated by experimental modules.
- Tags must not alter fixed-field semantics.

### 9.3 Update rule

The first implementation must use a simple incremental mean for fixed statistics.

For a sequence of observations \(x_1,\ldots,x_n\):

```text
mean_n = (x_1 + ... + x_n) / n
```

Online update:

```text
mean_{n+1} = mean_n + (x_{n+1} - mean_n)/(n+1)
```

For `price_mean`, n is `successes`. For `latency_mean`, n is `uses`. For `rating_mean`, n is `rating_count` — the number of rated observations, which may be less than `uses`. Using `uses` as the denominator for `rating_mean` is incorrect and must not be done.

Justification: this rule is deterministic, unbiased for stationary observations, and easy to test. Risk-averse updates may be added later as an experimental tag or lookup feature.

## 10. Trace and feedback model

### 10.1 Trace tree

Requirements:

- Every transaction must reference a trace.
- Nested calls must create child traces.
- Trace lookup by process must return the execution tree.
- Trace deletion must not delete transaction history.

Two trace relationship types exist:

```text
CHILD_OF (parent_trace_id): synchronous sub-call within the same execution context.
  The parent waits for the child. process_id is inherited.

FOLLOWS_FROM (caused_by_trace_id): causal link across process or listener boundaries.
  The originating trace may be closed before the triggered trace starts.
  The referenced trace may belong to a different process.
```

Event-triggered traces use FOLLOWS_FROM. Direct sub-calls use CHILD_OF.

### 10.2 Ratings and trace metrics

**Rating**

Requirements:

- A human may rate any transaction 0 (bad) or 1 (good) via RateTransaction.
- When a transaction is rated, the rating must automatically cascade to all unrated descendant transactions in the trace tree.
- The initial rating update and the descendant cascade must be performed in a single atomic SQLite transaction using one recursive SQL operation. The two writes must not be separate operations; the cascade error must not be silently ignored. No separate application-level propagation step is required or permitted.
- Rating is a human supervision operation. It must not be callable through `Call()`.

**Trace metrics**

Requirements:

- Every process and trace must maintain cumulative cost and wall-clock latency aggregates.
- These aggregates must be updated automatically as each transaction completes.
- No separate query operation is required to compute subtree cost or latency.

Correctness condition:

```text
trace.cost    = sum(gross(t) for all transactions t in subtree)
trace.latency = max(ended_at(t)) - started_at(trace)
```

Justification: an action that calls other actions induces downstream cost and latency. Automatic aggregation makes the user-experienced burden of selecting that action continuously visible without a separate query.

## 11. Events

### 11.1 Listener

A listener binds an event to a call.

Required fields:

```text
id
owner_user_id
source_user_id
event_name
target_action_id
active
created_at
```

Requirements:

- Creating a listener requires permission to call the target action.
- A listener does not store a process or trace. The process is supplied by the caller at consume time (see §11.4).
- Inactive listeners must not fire.
- Deleting a listener must atomically deactivate it and purge all pending (unconsumed) events for that listener.

### 11.2 Emit

Requirements:

- Emitting an event must select active listeners where:

```text
listener.source_user_id = emitter_user_id
listener.event_name     = emitted_event_name
listener.active         = true
```

- For each selected listener, the kernel must create an event record in the listener’s queue, storing the event arguments and the emitter’s current trace ID as `causing_trace_id`.
- Emit must not call the target action. Execution is deferred to the listener owner.
- `EmitEvent` returns the IDs of the created event records, not transaction IDs.
- If no listeners match, `EmitEvent` returns an empty list without error.

### 11.3 Event record

An event record is a pending work item in a listener’s queue.

Required fields:

```text
id
listener_id
args_json
causing_trace_id
consumed_at
tx_id
created_at
```

Requirements:

- `args_json` stores the raw event arguments at emit time.
- `causing_trace_id` stores the emitter’s trace ID at emit time (nullable). This is a FOLLOWS_FROM reference (§3.5).
- An event is **pending** when `consumed_at` is null.
- An event is **consumed** when `consumed_at` is set and `tx_id` is set.
- An event is **in-flight** when `consumed_at` is set and `tx_id` is null. This state exists only during an active consume call and is resolved to consumed or pending on completion. On startup, all in-flight events are reset to pending (§19.3).
- The event queue must be persistent.

### 11.4 Consume

The listener owner processes pending events by consuming them.

Requirements:

- Only the listener owner may consume events. The source user may not.
- Consuming an event must atomically lock it before calling the target action, preventing double-processing.
- On successful lock, the kernel calls the target action through the normal kernel call path, using the caller-supplied `process_id`, the stored `args_json`, and the event’s `causing_trace_id` as a FOLLOWS_FROM reference.
- On success, the event is marked consumed with the resulting `tx_id`.
- On failure, the lock is reset and the event returns to pending. The listener owner may retry.
- Consuming an event from an inactive listener must return `ErrInvalidState`.
- Consuming an already-consumed event must return `ErrInvalidState`.
- Delivery guarantee: at-least-once. An event may be retried after a failed consume. Double-processing is prevented by the atomic lock; only one consume attempt may execute the call at a time.

### 11.5 Poll

Requirements:

- Polling a listener returns its pending (unconsumed) event records.
- Reading a listener requires owner or source authority.
- The result must include event ID, `args_json`, `causing_trace_id`, and `created_at` for each pending event.

## 12. Authentication

Requirements:

- Human authentication must use a modern OAuth/OIDC-style flow.
- Browser login must support authorization code with PKCE.
- CLI login must support device authorization or loopback login.
- API calls must use short-lived bearer access tokens.
- Refresh tokens, if used, must be rotatable.
- Scripts must never receive access tokens or refresh tokens.
- Internal script calls must use trace-scoped kernel authority.

Justification: users authenticate to the kernel; scripts receive only mediated authority. This prevents scripts from exfiltrating reusable credentials.

## 13. CLI

Juice must provide a CLI named `juice`.

Required commands:

```text
juice serve
juice user create
juice auth login
juice auth logout
juice action add
juice action update
juice action enable
juice action disable
juice action acl grant
juice action acl revoke
juice action grant-all
juice action revoke-all
juice action list
juice process start
juice process show
juice process fund
juice process end
juice call
juice events listen
juice events unlisten
juice events emit
juice events poll
juice events consume
juice tx list
juice tx show
juice tx rate
juice stats show
juice lookup
juice health
juice admin user list
juice admin user show
juice admin user suspend
juice admin user unsuspend
juice admin user deposit
juice admin action list
juice admin action disable
juice admin process list
juice admin tx list
```

Requirements:

- CLI commands must call the same service layer as the server.
- CLI output must support human-readable text and JSON.
- Every CLI command must have at least one test.
- The CLI must be usable against a local SQLite database without running the HTTP server where feasible.

Justification: the CLI is both an operator tool and a test surface. Duplicated semantics would create inconsistencies.

## 14. Server interface

Requirements:

- Juice must run as a production server.
- The HTTP API is primary. For every HTTP endpoint the server exposes, there must be a corresponding CLI command.
- The server must use the same kernel service layer as the CLI.
- The server must propagate request id, subject id, process id, trace id, action id, and transaction id into logs where available.
- HTTP status codes must distinguish authentication failure, authorization failure, invalid input, insufficient funds, missing resource, and internal failure.
- Authentication and account creation endpoints must enforce per-IP rate limiting. Excessive requests must return HTTP 429.

## 15. Logging

Juice must provide rich structured logging to terminal and file.

Required fields:

```text
time
level
event
request_id
subject_user_id
process_id
trace_id
action_id
tx_id
status
duration_ms
error
```

Requirements:

- Logs must be emitted to terminal and file simultaneously.
- Log format must be configurable as text or JSON.
- File path and log level must be configurable.
- Every kernel transition must log start and end events.
- Errors must be logged with stable error codes.
- Script logs must be associated with the current trace id.

Justification: production operation and research reproducibility both require reconstructable execution records.

## 16. Testing

### 16.1 Test command

The entire system must be testable by:

```text
go test ./...
```

This command must pass without external network access.

### 16.2 Test isolation

Requirements:

- Tests must use temporary SQLite databases.
- Tests must use fake Ollama and fake script adapters unless the test explicitly targets integration.
- Tests must not depend on global state.
- Tests must not depend on test order.

### 16.3 Required test suites

The implementation must include tests for:

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
rating cascade to unrated descendant transactions
trace cost and latency updated on transaction completion
native action callable through Call()
non-superuser rejected from admin endpoints
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
```

### 16.4 Invariant tests

The following invariants must be tested directly:

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
rating cascade does not overwrite already-rated transactions
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
```

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

- Handle and password are set interactively on first boot if no superuser exists (prompt for both, like a standard database setup).
- The handle is fixed once set; it cannot be changed without direct database access.
- The superuser handle is stored in a `config` table in SQLite (key: `superuser_handle`).
- On every startup, the server reads `config.superuser_handle` to identify the superuser.
- Admin authority is enforced at the HTTP and CLI layers by comparing the authenticated subject handle to the stored superuser handle.
- The kernel has no concept of superuser; it enforces normal ACL rules for all users.

### 19.2 User suspension

Requirements:

- The superuser may suspend or unsuspend any user.
- A suspended user is rejected at every authenticated request with `ErrUnauthenticated`.
- Suspension does not delete the user or their data.

### 19.3 Bootstrap

On every `juice serve` startup, after first-boot setup, before accepting requests:

1. Register and enable `/lookup` (KindNative, owned by the system superuser) if absent.
2. Apply grant-all on `/lookup`.
3. Reset all in-flight event consumptions: set `consumed_at = NULL` for every event where `consumed_at IS NOT NULL AND tx_id IS NULL`. These represent consume calls interrupted by a prior crash; resetting them to pending makes them retryable.

Bootstrap must be idempotent.

First-boot (superuser creation) must be atomic: the superuser account and its config entry must be created in a single database transaction. A partial first-boot (e.g., crash after user creation but before config write) must leave the system in a state where re-running bootstrap succeeds cleanly.

### 19.4 System native actions

Requirements:

- System actions are KindNative, owned by the superuser, registered at bootstrap.
- System actions execute through the normal kernel call path (`Call()`).
- The initial system action is `/lookup` (owned by the system superuser, public, grant-all at bootstrap). The label `@sys/lookup` used in some contexts is a conceptual shorthand for "the `/lookup` action owned by `@sys`", not the action's Name field.
- Human supervision operations must not be registered as native actions (see §2.3).

### 19.5 Public access control

Requirements:

- An action may be made callable by all authenticated users via grant-all.
- grant-all and revoke-all are operations on an action, not a change to the ACL model.
- Only the action owner or a user with admin permission on the action may call grant-all or revoke-all.
- grant-all does not replace or remove existing per-user ACL entries.

### 19.6 Admin operations

The following operations are restricted to the superuser. Each has a corresponding HTTP endpoint and CLI command.

User management:

```text
GET  /v1/admin/users                    juice admin user list
GET  /v1/admin/users/{id}               juice admin user show --id
POST /v1/admin/users/{id}/suspend       juice admin user suspend --id
POST /v1/admin/users/{id}/unsuspend     juice admin user unsuspend --id
POST /v1/admin/users/{id}/deposit       juice admin user deposit --handle / --id
```

Action management:

```text
GET  /v1/admin/actions                  juice admin action list
POST /v1/admin/actions/{id}/disable     juice admin action disable --id
```

Process management:

```text
GET  /v1/admin/processes                juice admin process list
```

Transaction management:

```text
GET  /v1/admin/transactions             juice admin tx list
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

### 19.9 Deposits

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
- Amount must be a positive integer.
- A deposit must atomically increase `user.available` by the specified amount inside a single SQLite transaction.
- Each deposit must be persisted as an audit record.
- `reason` is optional but stored when provided.
- Deposits must not route through `Call()`. They are a human supervision operation (§2.3).

Required tests:

- Deposit increases target user's available balance by the exact amount.
- Non-superuser deposit attempt is rejected with `ErrUnauthorized`.
- Zero or negative amount is rejected with `ErrInvalidInput`.
- Deposit record is retrievable after creation.
