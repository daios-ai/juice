# Juice Kernel Requirements

Version: 0.2
Status: implementation requirements
Codename: `juice`

## 1. Execution, supervision, and role law

Juice is a Go production kernel and research platform for callable actions. Its sole execution primitive is:

```text
Call(caller, process, action, args)
```

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

These meanings are fixed. In a transaction, `owner_user_id` is the process owner, not the call caller; `caller_user_id` is the immediate requester, not necessarily the process owner; `target_user_id` is the called action owner.

| Case                       | `P`                       | `C`                     | `A`                                 |
| -------------------------- | ------------------------- | ----------------------- | ----------------------------------- |
| Root call                  | process owner             | authenticated requester | called action owner                 |
| WASM subcall               | parent process owner      | parent action owner     | subcalled action owner              |
| Event consumption          | supplied process owner    | listener owner          | listener target action owner        |
| Remote proxy call          | local process owner       | local call caller       | local remote-peer user owning proxy |
| `@sys/make` worker subcall | requester’s process owner | `@sys`                  | worker action owner                 |

All execution paths use `Call()`: root calls, native actions, WASM `juice.call`, event consumption, OpenAPI-imported HTTP actions, and remote proxies. `Call()` dispatches by `action.kind`, not by action-owner identity.

Supervision operations never route through `Call()`; they manage users, actions, processes, ratings, deposits, OpenAPI imports, and remote imports. Execution code must not rate outputs or propagate ratings.

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
| ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `User`              | `id`, `handle`, `email`, `available`, `locked`, `suspended_at`, `public_key`, `remote_base_url`, `created_at`, `updated_at`                                                                                                                                                      | `handle` is unique and contains no `/`. Suspended users are rejected at every authenticated request with `ErrUnauthenticated`. `public_key`, when set, is a unique base64url Ed25519 32-byte public key. Local users have null `public_key` and `remote_base_url`; remote peers set both.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `Action`            | `id`, `owner_user_id`, `name`, `kind`, `active`, `public`, `price`, `description`, `input_schema`, `output_schema`, `source`, `artifact_hash`, `remote_action_id`, `created_at`, `updated_at`                                                                                    | `owner_user_id` is the action owner. `kind ∈ {http, wasm, native, remote_proxy}`. `(owner_user_id,name)` is unique. `/` is allowed in `name`; handles cannot contain `/`, so `@owner/name` is unambiguous. Inactive actions are not callable. Public discovery returns active public actions. Action owners may list all their own actions regardless of `active` or `public`. Authorized users may inspect script source. `artifact_hash` content-addresses compiled artifacts. For `remote_proxy`, `source` is the federation call URL, `remote_action_id` is the action ID on the remote kernel, and `artifact_hash` stores the signed manifest hash. Active actions require non-empty natural-language `description`, valid schemas, and schema field descriptions sufficient for lookup and LLM function calling. |
| `Process`           | `id`, `owner_user_id`, `available`, `locked`, `status`, `created_at`, `ended_at`                                                                                                                                                                                                 | `owner_user_id` is the process owner and payer. `status ∈ {open,closed}`. A process starts with user-provided funds, possibly zero. Ending returns remaining funds to the process owner. Closed processes cannot call.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `Trace`             | `id`, `process_id`, `parent_trace_id`, `action_owner_id`, `cost`, `latency_ms`, `created_at`                                                                                                                                                                                     | `action_owner_id` is the action owner of the action executing in the trace; used for trace-scoped process authority. Root traces have null parent. Every `Call()` creates exactly one child trace.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `Transaction`       | `id`, `process_id`, `trace_id`, `parent_trace_id`, `owner_user_id`, `caller_user_id`, `target_user_id`, `action_id`, `action_name`, `args_json`, `reply_json`, `status`, `gross`, `net`, `fee`, `reason`, `remote_receipt_hash`, `remote_receipt_json`, `started_at`, `ended_at` | `status ∈ {success,failure}`. Every attempted call creates one immutable transaction. Fields obey the role law. `action_name` is captured at creation so history remains self-contained after action deletion. Local calls have null remote receipt fields. Remote-proxy commits atomically store full remote receipt JSON and `SHA-256(remote_receipt_json)`.                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `Stats`             | `uses`, `successes`, `failures`, `rating_count`, `price_mean`, `latency_mean`, `rating_mean`, `last_used_at`                                                                                                                                                                     | Missing stats have defined defaults. `uses = successes + failures`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `StatTag`           | `action_id`, `key`, `value`, `source`, `updated_at`                                                                                                                                                                                                                              | Optional lookup-experiment data, namespaced by source, never execution semantics.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `Listener`          | `id`, `owner_user_id`, `source_user_id`, `event_name`, `target_action_id`, `active`, `created_at`                                                                                                                                                                                | `owner_user_id` is the listener owner. Exact subscription to `(source_user_id,event_name)`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `Event`             | `id`, `listener_id`, `args_json`, `causing_trace_id`, `consumed_at`, `tx_id`, `created_at`                                                                                                                                                                                       | Persistent queued work item. `causing_trace_id` is nullable.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `Deposit`           | `id`, `operator_user_id`, `target_user_id`, `amount`, `reason`, `created_at`                                                                                                                                                                                                     | Immutable audit record for a positive out-of-band superuser credit grant.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `Receipt`           | `id`, `issuer_user_id`, `tx_id`, `trace_id`, `action_id`, `caller_user_id`, `process_id`, `args_hash`, `reply_hash`, `status`, `gross`, `net`, `fee`, `reason`, `started_at`, `created_at`, `signature`                                                                          | Immutable signed record for exactly one committed call. `caller_user_id` is the call caller. `started_at` is call start; `created_at` is settlement.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `Rating`            | `id`, `rated_tx_id`, `rated_receipt_id`, `rater_user_id`, `rating`, `note`, `created_at`, `signature`                                                                                                                                                                            | Immutable signed feedback record. `rating ∈ {0,1}`. At most one rating exists per transaction. `rated_receipt_id` may be null only for pre-receipt transactions. `note` is optional, nullable, human-readable, and included in the single Ed25519 rating signature payload.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `IdempotencyRecord` | `id`, `idempotency_key`, `counterparty_user_id`, `receipt_id`, `status`, `result_json`, `created_at`, `expires_at`                                                                                                                                                               | Cross-kernel only. `status ∈ {pending,complete}`. Insert pending before execution; complete atomically with transaction and receipt. Completion stores `result_json`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |

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
3. C may use process:
   C = P, or
   explicit process authority, or
   supplied parent trace has action_owner_id = C
4. action exists
5. CanCall(P, action)
6. args satisfy action.input_schema
7. process.available >= action.price
8. supplied parent_trace_id, if any, exists
```

Precondition 3 controls spending authority over the process. Precondition 5 controls action access by the process owner.

Explicit process authority is a reserved kernel relation. Version 0.2 defines no durable grant object for it. Until such an object is specified, implementations must treat explicit process authority as false except where trace-scoped authority applies.

Trace relation:

| Case                  | `parent_trace_id`      | `process_id`               |
| --------------------- | ---------------------- | -------------------------- |
| Root trace            | null                   | owning process             |
| Subcall trace         | executing action trace | same as parent             |
| Event-triggered trace | emitting action trace  | supplied consuming process |

`process_id` determines payment. `parent_trace_id` records causality only. Cross-process parent references are valid for event-triggered calls. Each completed descendant transaction updates ancestor `cost` and `latency_ms`.

## 5. Persistence and atomicity

Use file-backed SQLite with WAL by default. Migrations are deterministic and stored in the repository. Tests use temporary SQLite databases. No production feature may depend on an in-memory-only store. `kernel` depends on a store interface, never SQLite.

Store interface:

```text
CreateUser ReadUser ReadUserByPublicKey ListUsers SuspendUser UnsuspendUser
CreateAction ReadAction UpdateAction DeleteAction ListAllActions
CreateProcess ReadProcess EndProcess ListAllProcesses
CreateTrace CreateTransaction ListTransactions ListAllTransactions
ReadStats UpdateStats
CreateListener ReadListener ListListeners DeleteListener
CreateEvent ListPendingEvents ConsumeEvent PurgeListenerEvents
GetConfig SetConfig CreateDeposit
CreateReceipt ReadReceipt
CreateRating ReadRating ListRatings
CreateIdempotencyRecord ReadIdempotencyRecord
```

`UpdateTransaction` is forbidden. Transactions are immutable after creation.

Atomic write sets:

| Operation       | Atomic writes                                                                                    |
| --------------- | ------------------------------------------------------------------------------------------------ |
| Start process   | user debit, process creation, root trace creation                                                |
| Fund process    | user debit, process credit                                                                       |
| Successful call | transaction, receipt, locked-fund settlement, target payment, platform fee, trace metrics, stats |
| Failed call     | transaction, receipt, full refund, trace metrics, stats                                          |
| End process     | process closure, return of remaining funds to process owner                                      |
| Deposit         | user credit, deposit record                                                                      |
| Rating          | rating record                                                                                    |

A monetary transition and its audit record must commit or fail together.

## 6. Call transition and settlement

For `q = action.price`:

```text
create child trace with action_owner_id = A
lock q credits in process
execute action by dispatching on action.kind
validate output against action.output_schema
on success: commit transaction + receipt + settlement + metrics + stats
on failure: commit transaction + receipt + full refund + metrics + stats
return result, tx_id, trace_id
```

Locking precedes execution:

```text
available := available - q
locked    := locked + q
valid iff q >= 0 ∧ available >= q
```

Zero-credit processes may execute zero-price actions.

Only successes are charged in v1:

```text
gross    = action.price
sub_cost = sum(gross paid to direct subcalls during execution)
taxable  = max(gross - sub_cost, 0)
fee      = (taxable * fee_bps + 9_999) / 10_000
net      = gross - fee
```

Default `fee_bps = 2000`. Fee recipient is fixed as `@sys`. Success decreases the process locked balance and process owner locked balance by `gross`, credits `target_user_id` by `net`, and credits `@sys` by `fee`. Negative value added creates no fee credit or kernel payout. Each kernel taxes only its own layer; remote subcalls are subject to the remote kernel’s fee policy independently.

Failures charge zero, refund the full locked gross amount, record `status=failure`, and expose the failure class in `reason`. Any later partial-failure policy must be explicit in the transaction.

Schemas exist for every action. Unsupported JSON Schema subset forms fail action creation or update. A schema node without `type` is unconstrained; this is intentional and not an error. Validate input before locking and output before successful settlement.

Subcall law:

```text
juice.call(target_action,args) from parent_action in parent_process
= Call(parent_action.owner_user_id, parent_process, target_action, args)
```

Subcall transaction:

```text
owner_user_id  = parent_process.owner_user_id
caller_user_id = parent_action.owner_user_id
target_user_id = target_action.owner_user_id
```

No ephemeral process is created. All subcalls spend from the same process. Trace-scoped process authority requires `Trace(parent_trace_id).action_owner_id = caller_user_id`. Settled subcall costs persist even if an ancestor later fails. Insufficient funds fail and refund the subcall; the parent propagates failure.

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

OpenAPI webhooks are event ingress, not actions. They must validate incoming payloads and enter through `EmitEvent`; they must not bypass the normal listener and consume flow.

### Remote

`remote import` fetches a signed manifest and creates or updates a local `kind=remote_proxy` action owned by the local remote-peer user row. It does not copy implementation.

Manifest required fields:

```text
action_id owner_handle name description input_schema output_schema price kind
artifact_hash stats updated_at signature
```

Only active public remote actions have manifests. Manifest descriptions and schemas are the canonical interface used by importing kernels for lookup and LLM function calling. `signature` is the remote platform Ed25519 signature over canonical JSON excluding `signature`, verified against the remote peer’s `public_key`. Match key:

```text
proxy.owner_user_id + proxy.remote_action_id
```

Remote contract fields:

```text
action_id artifact_hash description input_schema kind name output_schema owner_handle price
```

Manifest stats and `updated_at` do not affect contract comparison; manifest stats never overwrite local `Stats` and are not stored as `StatTag`. Invalid signatures reject import. Missing, inactive, or non-public remote actions deactivate the proxy and reset current local stats. Unimport deactivates the proxy, does not contact the remote kernel, and preserves all history.

## 9. Adapters, native actions, and stats

WASM uses wazero. Scripts receive no ambient filesystem, network, environment, process access, or raw user tokens. They receive only explicit host functions, each execution having memory limit, timeout, deterministic context cancellation, and artifact-hash compiled-module cache. Store source and artifact; authorized users may inspect source; activation should precompile; compilation failures are typed.

Host surface:

```text
juice.call   subcall under §6
juice.emit   emit under §10 with current trace id
juice.log    structured trace log
```

Script authority:

```text
ScriptAuthority ⊆ KernelAuthority(trace, process, subject)
```

`llm` exposes replaceable `Embed(ctx,text)->vector` and `Chat(ctx,messages)->message`; concrete adapters call Ollama, but `kernel` must not import them. Defaults: URL `http://localhost:11434`, chat `gemma4:26b`, embedding `nomic-embed-text`. Tests use fakes.

Native actions:

| Action          | Rules                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| --------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `@sys/lookup`   | Public; action owner `@sys`; callable only through `Call()`. Rank active actions by tested formula combining semantic similarity and stats. Replaceable ranking storage; brute-force cosine acceptable. Input: required `query`, optional `limit=10`. Output: `results[]` with `action_id`, `name`, `owner_handle`, `description`, `score`. Direct lookup only for diagnostics, not user-facing APIs or WASM hosts.                                                                                                                                                                                                                                                                                                                                       |
| `@sys/llm/chat` | Public; action owner `@sys`; callable through `Call()`. Input: `messages[]` of `{role,content}` plus optional `system`. Output: `message{role,content}`. `ErrInvalidState` if chat unconfigured.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `@sys/make`     | Public; action owner `@sys`; price 20; callable through `Call()`. Synthesizes a WASM action from a natural-language description using the platform LLM and TinyGo compiler. Input required `description`; empty gives `ErrInvalidInput`; missing LLM/compiler gives `ErrInvalidState`. Up to `maxSteps=5`: derive contract, search catalog, generate TinyGo, compile, validate WASM imports/exports, smoke-test with stub host. Success registers and activates action under `C` of the `@sys/make` call. Return `status="success"`, `action_id`, `action_name`, `diagnostics`, `tests`. Name collision returns `status="failure"` without alternate-name retry. Synthesis failures use output failure status, not kernel errors. Worker subcalls follow role law: `owner_user_id=same process owner`, `caller_user_id=@sys`, `target_user_id=worker action owner`. |

Stats use:

```text
mean_(n+1) = mean_n + (x_(n+1)-mean_n)/(n+1)
```

`price_mean` uses successful calls only, with denominator `successes`.
`latency_mean` uses completed calls, with denominator `uses`.
`rating_mean` uses rated calls only, with denominator `rating_count`, never `uses`.

## 10. Events

Listener creation requires listener-owner authority, existing source user, exact event-name match, existing target action, and `CanCall(listener.owner_user_id,target_action)`. A listener stores no process or trace. Inactive listeners never fire. Deleting a listener requires listener-owner authority and atomically deactivates it and purges pending events.

`EmitEvent(source_user_id,event_name,args,causing_trace_id)` creates one queued event per active exact-match listener. It stores raw args, performs no call, changes no balance, needs no emitter process, and creates no transaction. It returns created event IDs, or an empty list when no listeners match. Authenticated HTTP/CLI emission sets `source_user_id` to request user and rejects supplied `source_user_id`. WASM `juice.emit` sets `source_user_id` to executing action owner and `causing_trace_id` to current trace.

Event states:

```text
Pending   consumed_at = null
In-flight consumed_at != null ∧ tx_id = null
Consumed  consumed_at != null ∧ tx_id != null
```

Only the listener owner may consume, unless the source user is also the listener owner. Consumption atomically locks a pending event, then performs:

```text
Call(listener.owner_user_id, supplied_process, target_action, event.args_json)
with parent_trace_id = event.causing_trace_id
```

Consumption transaction:

```text
owner_user_id  = supplied_process.owner_user_id
caller_user_id = listener.owner_user_id
target_user_id = target_action.owner_user_id
```

Normal call preconditions apply. The listener owner must be allowed to use the supplied process; `CanCall(supplied_process.owner_user_id,target_action)` must hold. `causing_trace_id` records causality, not payment. Success stores `tx_id`; failure resets pending. Inactive listeners and already-consumed events return `ErrInvalidState`. Delivery is at-least-once; the lock prevents concurrent double-processing. Startup resets in-flight events. Polling pending events is allowed to listener owner or source user and returns `id`, `args_json`, `causing_trace_id`, and `created_at`.

## 11. Receipts, ratings, signatures, transaction access

Every transaction references a trace. Trace lookup by process returns the execution tree. Trace deletion must not remove transaction history. On descendant completion:

```text
trace.cost       = sum(descendant transaction gross)
trace.latency_ms = elapsed from trace creation to latest descendant completion
```

Only `tx.owner_user_id` may rate the transaction. Rating is supervision, immutable, non-cascading, duplicate-rejected with `ErrInvalidInput`, and never routed through `Call()`.

Every committed success or failure has exactly one receipt. `issuer_user_id=@sys`. `args_hash` and `reply_hash` are SHA-256 over RFC 8785 JCS canonical `args_json` and `reply_json`. Receipt economics match the transaction. Signature is Ed25519 over canonical receipt JSON excluding `signature`. Transaction and receipt creation are atomic.

Receipts, ratings, and manifests use RFC 8785 JCS:

```text
CanonicalJSON(v any) ([]byte,error)
```

Sign and verify only canonical JSON. Receipt signatures and rating signatures are Ed25519 signatures made with the platform key. Rating signatures cover all fields except `signature`. ASCII property names make UTF-8 ordering equivalent to RFC 8785 UTF-16 ordering.

Transaction access uses historical transaction fields:

```text
CanReadTransaction(u,t) :=
  u = t.owner_user_id ∨ u = t.target_user_id ∨ IsSuperuser(u)
```

Buyer is `t.owner_user_id`. Seller is `t.target_user_id`. This remains valid after action deletion because the seller was captured at transaction creation. Receipts remain internal settlement/federation artifacts; there is no provider-receipt endpoint. Every credit to an action owner must be reconstructible from transactions readable by that action owner.

## 12. Authentication, bootstrap, deposits

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

Every startup reads `config.superuser_handle` to confirm first boot and identify `@sys`; it verifies signing keys and aborts if either is absent. It then registers, enables, and makes public `@sys/lookup`, `@sys/llm/chat`, and `@sys/make` if absent. It resets in-flight events by setting `consumed_at = NULL` where `consumed_at IS NOT NULL AND tx_id IS NULL`. It resets in-flight calls by restoring locked process funds for all open processes with `locked > 0`:

```text
available += locked
locked = 0
```

Bootstrap is idempotent. Supervision operations are not native actions.

The superuser may suspend or unsuspend users. Suspension preserves data and makes every authenticated request return `ErrUnauthenticated`.

`Kernel.Deposit(operator_user_id,target_user_id,amount,reason)` is admin-CLI-only supervision. It requires configured superuser and positive amount, then atomically credits `user.available` with a deposit record. `reason` is optional and stored when provided. No HTTP endpoint exists.

## 13. Federation

A remote kernel is a local `User` row with `public_key` and `remote_base_url`. The public key is stable identity; URL is mutable location. Remote peers cannot authenticate with passwords or receive tokens. Convention: `@<hostname>`. Remote peers are ordinary users and appear in ordinary user listings; `remote list` is a convenience view over users with `remote_base_url` set.

Commands:

```text
juice remote add <url>
juice remote list
juice remote import <remote-handle> <action-name>
juice remote unimport <remote-handle> <action-name>
```

`remote add` validates `<url>/.well-known/juice-kernel.json`. Re-adding an existing public key updates URL and owned proxy source URLs. Same handle or URL with different key fails. Key rotation is unsupported. No gossip or crawling.

Expose:

```text
GET /.well-known/juice-kernel.json -> public_key, handle (@sys), base_url
GET /v1/actions/{id}/manifest      -> signed public-action manifest
```

A local remote-proxy call follows normal role law:

```text
owner_user_id  = local process owner
caller_user_id = local call caller
target_user_id = local remote-peer user id
```

Proxy handler sends UUID v4 `idempotency_key`. On remote success, local transaction atomically stores remote receipt JSON and its SHA-256 hash.

Inbound federation signs:

```text
JCS({action,counterparty,idempotency_key,timestamp,args_hash})
```

`counterparty` is the caller’s base64url public key. `args_hash = SHA-256(raw request body bytes)`. Receiver verifies signature, raw hash, registered caller, and timestamp age ≤ 5 minutes. Unregistered callers are rejected.

Idempotency is cross-kernel only. Insert pending before execution; unique `(idempotency_key,counterparty_user_id)` blocks concurrent duplicates. Complete replay returns stored result and receipt; pending replay returns 409. Expiration is 24 hours.

`VerifyRemoteReceipt(caller_id,tx_id)` requires `CanReadTransaction` and verifies entirely from local data. It checks signature, receipt hash, remote `action_id == proxy.remote_action_id`, and matching `status`, `gross`, `net`, `fee`, `args_hash`, `reply_hash`. It returns per-check results and top-level `valid`; non-proxy transactions give `ErrInvalidState`.

## 14. CLI, HTTP, logging, config

HTTP API is primary. Every exposed endpoint has a CLI command. CLI uses the same service layer, supports human-readable and JSON output, works directly against local SQLite where feasible, and each command has at least one test. Admin is CLI-only.

Required commands:

```text
juice serve
juice user create                         juice user me
juice auth login                          juice auth logout
juice action create                       juice action update
juice action delete                       juice action enable
juice action disable                      juice action list
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
juice tx rate                             juice tx verify-receipt
juice health
juice admin user list                     juice admin user show
juice admin user suspend                  juice admin user unsuspend
juice admin user deposit                  juice admin action list
juice admin action disable                juice admin process list
juice admin tx list
juice remote add                          juice remote list
juice remote import                       juice remote unimport
```

OpenAPI commands:

```text
juice action import --openapi <spec-url>
juice action unimport --openapi <spec-url>
juice action unimport --openapi <spec-url> --name <action-name>
```

`juice serve` handles `SIGTERM`/`SIGINT`, stops accepting new requests, drains in-flight calls, exits cleanly. No `juice stop`.

Server logs request, caller, process, trace, action, and transaction IDs where available; maps distinct auth, authorization, invalid input, insufficient funds, missing resource, and internal failures to distinct statuses; rate-limits auth and account creation per IP with 429. Action read/list responses include computed `action=@owner/name`.

Endpoint rules:

| Endpoint                                         | Rule                                                                                                            |
| ------------------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| `GET /health`                                    | unauthenticated                                                                                                 |
| `GET /v1/me`                                     | authenticated `id`, `handle`, `email`, `available`, `locked`; suspended rejected before handler                 |
| `PUT /v1/actions/{id}`                           | action-owner update; `public` updatable; deactivation rules apply                                               |
| `DELETE /v1/actions/{id}`                        | action-owner delete preserving history                                                                          |
| `POST /v1/actions/import`                        | authenticated OpenAPI supervision import                                                                        |
| `POST /v1/actions/unimport`                      | action-owner import-provenance deactivation                                                                     |
| `GET /v1/processes`                              | process owner’s processes, descending `created_at`                                                              |
| `GET /v1/listeners`                              | listener owner’s listeners                                                                                      |
| `GET /v1/listeners/{id}/events`                  | listener owner or source user; returns pending `id`, `args_json`, `causing_trace_id`, `created_at`              |
| `POST /v1/call`                                  | requires `args`; `{}` valid; absent gives `ErrInvalidInput`; action is `@owner/name`                            |
| `POST /v1/events/emit`                           | source user is request user; rejects `source_user_id`; requires `args`; returns created event IDs or empty list |
| `POST /v1/auth/logout`                           | refresh token body; missing/revoked gives `ErrUnauthenticated`                                                  |
| `GET /v1/transactions`                           | buyer/seller transactions under `CanReadTransaction`                                                            |
| `GET /v1/transactions/{id}`                      | full detail to parties; `ErrNotFound` to non-parties                                                            |
| `GET /v1/transactions/{id}/receipt-verification` | parties; remote verification; local tx gives `ErrInvalidState`                                                  |

Transaction list/detail include `rating: {"value":0|1,"note":string|null}` or `null`, visible to buyer and seller.

Admin commands require configured `@sys`, reject non-superusers with `ErrUnauthorized`, stay outside `Call()`, and register no `/v1/admin/*` routes.

Logs go to stderr and optionally file; stdout is resource payloads only. Configurable format, level, file. Every kernel transition logs start/end; errors include stable codes; script logs include trace ID.

Required log fields:

```text
time level event request_id caller_user_id process_id trace_id action_id tx_id
status duration_ms error
```

Config sources: env plus optional file. Safe local defaults; no committed production secrets; invalid startup config rejected.

```text
JUICE_DB_PATH JUICE_LOG_LEVEL JUICE_LOG_FILE JUICE_FEE_BPS
JUICE_AUTH_ISSUER JUICE_AUTH_AUDIENCE JUICE_TOKEN_TTL JUICE_SECRET_KEY
JUICE_OLLAMA_URL JUICE_OLLAMA_CHAT_MODEL JUICE_OLLAMA_EMBED_MODEL
JUICE_SCRIPT_TIMEOUT_MS JUICE_SCRIPT_MEMORY_BYTES
```

## 15. Required automated tests

`go test ./...` must pass without external network access. Tests use temporary SQLite databases, fake Ollama and fake script adapters unless explicitly integration tests, no global state, and no order dependence.

Required suites:

```text
user creation
authentication token validation
action create/update/delete
action activation/deactivation
public/private access control
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
subcall VAT: taxed on value added only
trace cost and latency updated on transaction completion
native action callable through Call()
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
subcall spends from same process
failed subcall refunds same process
successful subcall remains settled if parent later fails
subcall trace has same process_id and parent_trace_id pointing to caller trace
event-triggered trace has parent_trace_id referencing emitting action trace in another process
event consumption transaction has owner_user_id = supplied process owner
event consumption transaction has caller_user_id = listener owner
event consumption transaction has target_user_id = listener target action owner
root trace has null parent_trace_id
pending events absent from poll after successful consume
emit does not alter emitter balance or process balance
EmitEvent returns created event IDs or an empty list when no listeners match
polling returns pending event id, args_json, causing_trace_id, created_at
second ConsumeEvent on same event returns ErrInvalidState
ConsumeEvent against inactive listener returns ErrInvalidState
DeleteListener purges all pending events for that listener
ConsumeEvent fails and resets event to pending when process has insufficient funds
bootstrap resets in-flight events (consumed_at set, tx_id null) to pending
bootstrap resets in-flight calls: restores locked process funds to available for all open processes with locked > 0
startup reads config.superuser_handle to confirm first boot and identify @sys
second rating on same transaction rejected with ErrInvalidInput
rating record created in ratings table, transaction row unchanged
rating note is included in the single platform-key Ed25519 rating signature payload
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
remote manifest signature is Ed25519 over canonical JSON excluding signature
remote proxy transaction has target_user_id = local remote peer user id
remote add updates remote_proxy source URLs when base URL changes for an existing public key
remote add rejects same handle or base URL paired with a different public key
inbound federation call rejected when args_hash does not match request body
full remote receipt JSON stored atomically with transaction on remote-proxy call
receipt verification returns valid for a well-formed stored remote receipt
receipt verification detects signature tampering
receipt verification detects field mismatch (action_id, status, gross)
receipt verification returns ErrInvalidState for a non-remote-proxy transaction
```

Direct invariant tests:

```text
balances are never negative
successful payment satisfies gross = net + fee
process.available + process.locked changes only by funding, settlement, or end
closed processes cannot call actions
inactive actions are not callable
every call creates exactly one transaction
every nested call creates exactly one child trace
suspended users cannot authenticate
native actions are always owned by the superuser
ratings do not cascade; each rating applies only to the rated transaction
trace.cost equals sum of descendant transaction gross amounts
all subcalls spend from the original funded process
successful subcall settlements persist if ancestor call later fails
root traces have parent_trace_id = null
subcall traces share their parent's process_id
event-triggered traces have parent_trace_id referencing a trace in another process
emitter balance is unchanged by EmitEvent regardless of how many listeners match
consumed events never appear in ListPendingEvents
pending events are absent after listener deletion
transaction row is immutable after commit
rating records reference valid tx_id and receipt_id
every transaction obeys owner_user_id = process owner, caller_user_id = call caller, target_user_id = action owner
every credit to an action owner is reconstructible from transactions readable by that action owner
imported action reimport or unimport never deletes transaction or receipt history
imported action current stats reset never mutates transaction, receipt, or rating rows
OpenAPI and remote imports create ordinary Actions, not separate action types
all imported actions execute only through Call()
```

Required user-flow tests:

```text
API owner imports an OpenAPI document, activates an action, makes it public, and a caller executes it through Call()
API owner re-runs import against a changed OpenAPI document; the matched action is deactivated, stats reset, and historical transactions remain attached
API owner unimports an OpenAPI document; matching actions are deactivated and history remains attached
remote kernel is added, a signed manifest is imported, a caller executes the proxy through Call(), and local stats remain separate from manifest stats
remote proxy is unimported; the local proxy is deactivated and the remote kernel is unaffected
caller executes a paid action multiple times; the action owner lists transactions for their action and the sum of transaction net amounts equals the total credits received by the owner
caller rates a transaction with a note; the note and rating value appear in the transaction detail and list responses for both buyer and seller; an unrated transaction returns null for the rating field
caller executes a remote proxy action; buyer and seller both call verify-receipt; all checks pass and valid is true
caller executes @sys/make; worker subcalls record owner_user_id = requester process owner, caller_user_id = @sys, and target_user_id = worker action owner; registered action is owned by the caller of the @sys/make call
```

## 16. Design rationale

Stable kernel interfaces and explicit transitions prevent correctness from depending on transports or adapters. Small function-named packages and per-file tests reduce coupling, keep replaceable implementations visible, and expose coverage gaps. Execution and supervision are separated because execution may err, while supervision supplies correction signals execution must not manipulate.

Credit locking before execution prevents unfunded work. Full failure refunds make v1 accounting conservative, observable, and testable. Input-before-lock and output-before-settlement prevent charging invalid requests or paying malformed replies. Mediated WASM authority permits composition without credential leakage or authorization bypass.

Replaceable lookup ranking permits research changes without changing kernel semantics. Fixed stats plus optional tags preserve deterministic baseline metrics while isolating experiments. Automatic trace aggregates keep downstream cost and latency visible without subtree queries.

Fixed `@sys` and signing keys give stable system action names and verifiable receipts/manifests. Structured logs make production operation and research reproduction reconstructable.

OpenAPI import as supervision keeps registration low-friction while preserving uniform execution. Federation imports signed action contracts without leaking implementation. Contract-change deactivation prevents silent interface drift for callers and LLMs. Unimport deactivates rather than deletes so history remains auditable.
