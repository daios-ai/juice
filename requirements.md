# Juice Kernel Requirements

Version: 0.1  
Status: implementation requirements  
Codename: `juice`

## 1. Purpose

Juice is a small production kernel and research platform for callable actions. It must support action registration, strict access control, budgeted execution, auditable traces, accounting, event-triggered calls, WebAssembly scripts, local language services, action discovery, and complete automated tests.

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
created_at
updated_at
```

Requirements:

- User ids must be stable opaque identifiers.
- Handles must be unique.
- Balances must be non-negative integers.
- Balance units must be indivisible credits.

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
CanCall(u,a) := Active(a) ∧ (Owner(u,a) ∨ ACL(u,a,call) ∨ ACL(u,a,admin)).
```

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
created_at
```

Requirements:

- Every process must have one root trace.
- The root trace must have `parent_trace_id = id` or `parent_trace_id = null`; this choice must be consistent across the codebase.
- Every nested call must create exactly one child trace.
- A child trace must inherit the parent trace’s process id.
- The trace relation must form a rooted tree for each process.

Correctness condition:

```text
∀ child. child.process_id = parent(child).process_id.
```

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
- Tests must use temporary SQLite databases.
- No production feature may depend on an in-memory-only store.

### 4.2 Store interface

`kernel` must depend on a store interface, not directly on SQLite.

The store interface must support:

```text
CreateUser
ReadUser
CreateAction
ReadAction
UpdateAction
DeleteAction
GrantACL
RevokeACL
CheckACL
CreateProcess
ReadProcess
EndProcess
CreateTrace
CreateTransaction
UpdateTransaction
ListTransactions
ReadStats
UpdateStats
CreateListener
ReadListener
ListListeners
AppendEvent
ReadEvents
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
- Transaction status and reason must make the failure class observable.

Justification: this rule is conservative and testable. It separates execution failure from economic settlement.

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

## 6. Action lifecycle

### 6.1 Creation

Requirements:

- Actions must be created inactive by default.
- Action creation must validate owner, name, kind, price, description, schemas, and source.
- For `wasm` actions, creation must compile or validate the artifact before the action can be activated.
- For `http` actions, creation must validate endpoint configuration without calling the endpoint unless explicitly requested.

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
juice.get
juice.put
```

Requirements:

- `juice.call` must call another action through the kernel call path.
- `juice.call` must enforce ACL, accounting, trace creation, and schema validation.
- `juice.emit` must emit an event through the kernel event path.
- `juice.log` must write structured logs under the current trace id.
- `juice.get` and `juice.put` must access only process-scoped or action-scoped state according to ACL.
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
- `rating_mean` must be computed from rated calls.
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

Justification: this rule is deterministic, unbiased for stationary observations, and easy to test. Risk-averse updates may be added later as an experimental tag or lookup feature.

## 10. Trace and feedback model

### 10.1 Trace tree

Requirements:

- Every transaction must reference a trace.
- Nested calls must create child traces.
- Trace lookup by process must return the execution tree.
- Trace deletion must not delete transaction history.

### 10.2 Recursive feedback

Juice must compute recursive cost and recursive latency over the trace tree.

For a node \(x\) with subtree \(T_x\):

```text
recursive_cost(x) = sum(gross(y) for y in T_x)
recursive_latency(x) = max(ended_at(y) for y in T_x) - started_at(x)
```

Requirements:

- Recursive cost must include descendants.
- Recursive latency must measure wall-clock latency of the subtree.
- Rating propagation must be explicit: an unrated child inherits the nearest rated ancestor only when the feedback job is configured to propagate ratings.
- Feedback computation must be deterministic for a fixed transaction set.

Justification: an action that calls other actions induces downstream cost and latency. Subtree aggregation measures the user-experienced burden of selecting that action.

## 11. Events

### 11.1 Listener

A listener binds an event to a call.

Required fields:

```text
id
owner_user_id
source_user_id
event_name
process_id
trace_id
target_action_id
active
created_at
```

Requirements:

- Creating a listener requires authority over the process and permission to call the target action.
- A listener stores the process and trace under which future event calls run.
- Inactive listeners must not fire.

### 11.2 Emit

Requirements:

- Emitting an event must select active listeners where:

```text
listener.source_user_id = emitter_user_id
listener.event_name = emitted_event_name
listener.active = true
```

- Each selected listener must execute through the normal kernel call path.
- Each resulting transaction id must be appended to that listener’s event queue.
- Event execution may be concurrent, but each call must preserve accounting and trace correctness.

### 11.3 Poll

Requirements:

- Users may poll listener state and queued transaction ids.
- Reading a listener requires owner or source authority.
- The event queue must be persistent.

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
juice action list
juice process start
juice process fund
juice process end
juice call
juice events listen
juice events unlisten
juice events emit
juice events poll
juice tx list
juice tx show
juice stats show
juice lookup
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
event listen/emit/poll/unlisten
wasm script execution
wasm host function call
script timeout
script memory limit
lookup ranking with fake embeddings
stats update
CLI commands
logging smoke test
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



