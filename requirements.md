# Juice Kernel Requirements

Version: 0.5
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

Use Go. `go build ./...` and `go test ./...` must pass. Replaceable modules use ordinary Go interfaces. `kernel` must not import CLI, HTTP, SQLite, wazero, Ollama, or libp2p implementations.

Production packages:

```text
cmd/juice/   CLI and server entrypoint
kernel/      core objects and operational semantics
store/       persistence interface and SQLite implementation
script/      WebAssembly execution
llm/         local language and embedding interface
fed/         federation transport interface and libp2p implementation
log/         structured logging
native/      native function implementations
```

Package names such as `sqlite`, `wazero`, `ollama`, and `libp2p` are forbidden. Implementation-specific names may appear in concrete types or filenames. Keep package and source-file counts small. Do not split files for size alone. Every production source file must have a corresponding `_test.go` file with independent tests for its logic.

## 3. Data model

All IDs are stable opaque identifiers. Action IDs are globally unique. Credit balances and prices are non-negative indivisible integers.

| Object              | Fields                                                                                                                                                                                                                                                                           | Rules                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `User`              | `id`, `handle`, `email`, `available`, `locked`, `suspended_at`, `denied_at`, `public_key`, `created_at`, `updated_at`                                                                                                                                         | `handle` is unique and contains no `/`. Suspended users are rejected at every authenticated request with `ErrUnauthenticated`. `public_key`, when set, is a unique base64url Ed25519 32-byte public key. There is no user "kind": an account authenticates by the credentials it holds — a password (session credential: log in, hold tokens) and/or a `public_key` (signature credential: authenticate by federation signature, per request, §13). `user create` makes a password account; peer acceptance makes a key account (§13); an account with neither credential cannot authenticate. An account may additionally hold `Grant` rows (§8) delegating its upstream OAuth identity to specific actions; grants are consent records, not authentication credentials. Location is never stored — the federation transport resolves a key to a live path at call time (§13). `denied_at` marks a key whose requests are rejected (§13).                                                                                                                                                                                                                                          |
| `Action`            | `id`, `owner_user_id`, `name`, `kind`, `active`, `public`, `price`, `description`, `input_schema`, `output_schema`, `source`, `auth_json`, `artifact_hash`, `remote_action_id`, `created_at`, `updated_at`                                                                       | `owner_user_id` is the action owner. `kind ∈ {http, wasm, native, remote_proxy}`. `(owner_user_id,name)` is unique. `/` is allowed in `name`; handles cannot contain `/`, so `@owner/name` is unambiguous. Inactive actions are not callable. `GET /v1/actions` unauthenticated returns active public actions; authenticated returns active public actions plus the caller's own active actions (union, deduplicated by `id`). Action owners may list all their own actions regardless of `active` or `public` via the `?owner=` filter when it resolves to themselves. Authorized users may inspect script source. `artifact_hash` content-addresses compiled artifacts. `auth_json` is the write-only upstream credential config, encrypted at rest, never returned by any read path (§8). For `remote_proxy`, `remote_action_id` is the action ID on the remote kernel and `artifact_hash` stores the signed manifest hash; the peer is identified by the proxy owner's `public_key`, which the federation transport resolves to a live path (§13), so no URL is stored in `source`. Active actions require non-empty natural-language `description`, valid schemas, and schema field descriptions sufficient for lookup and LLM function calling. |
| `Process`           | `id`, `owner_user_id`, `available`, `locked`, `status`, `created_at`, `ended_at`                                                                                                                                                                                                 | `owner_user_id` is the process owner and payer. `status ∈ {open,closed}`. A process is created by `run`, funded with exactly the root action's price, parked from the owner's `available` into the owner's `locked` (§6); the process holds it as `available`. It is bijective with its root trace and exists as a longer-lived wallet only because traces settle eagerly (§6): it absorbs refunds destined for already-settled traces and holds parked steps. Enforcement is per call, on the call's trace (§6); the process's `available + locked` is the total held across its calls' wallets and parked steps. It closes automatically when the root call has returned and no Steps of the process are outstanding; closing returns remaining funds to the process owner and releases the owner's lock. Closed processes cannot call.                                                                                                                                                                              |
| `Trace`             | `id`, `process_id`, `parent_trace_id`, `action_owner_id`, `available`, `locked`, `idempotency_key`, `dispatch_json`, `created_at`                                                                                                                                  | `action_owner_id` is the action owner of the action executing in the trace; used for trace-scoped process authority. `process_id` is denormalized (derivable by walking `parent_trace_id` to the root). Root traces have null parent. Every `Call()` creates exactly one child trace. A trace is the call's wallet: `available` starts as the action's price at entry and is the call's remaining allocation; `locked` is what the call has committed to its direct subcalls and steps. Calling something of price `q` requires `available ≥ q` and moves `q` from this trace's `available` into its `locked`, becoming the callee's `available` (§6). Settlement pays out the trace's remaining `available` (§6). A call's own latency is `transaction.ended_at − transaction.started_at`; there is no cached latency field (§11). `idempotency_key` and `dispatch_json` are null except on a remote-proxy trace, where the outbound key and request payload are recorded atomically with dispatch; while set and unsettled, the call is awaiting its receipt and restart resumes its retry (§5, §13). |
| `Transaction`       | `id`, `process_id`, `trace_id`, `parent_trace_id`, `owner_user_id`, `caller_user_id`, `target_user_id`, `action_id`, `action_name`, `args_json`, `reply_json`, `status`, `gross`, `net`, `fee`, `reason`, `remote_receipt_hash`, `remote_receipt_json`, `started_at`, `ended_at` | `status ∈ {success,failure}`. Every attempted call creates one immutable transaction. Fields obey the role law. `action_name` is captured at creation so history remains self-contained after action deletion. Local calls have null remote receipt fields. Remote-proxy commits atomically store full remote receipt JSON and `SHA-256(remote_receipt_json)`.                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `Stats`             | `uses`, `successes`, `failures`, `rating_count`, `latency_estimate`, `rating_estimate`, `last_used_at`                                                                                                                                                                           | Missing stats have defined defaults. `uses = successes + failures`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `StatTag`           | `action_id`, `key`, `value`, `source`, `updated_at`                                                                                                                                                                                                                              | Optional lookup-experiment data, namespaced by source, never execution semantics.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `Step`              | `id`, `parent_trace_id`, `required_caller_user_id`, `action_id`, `price`, `partial_args`, `status`, `tx_id`, `created_at`                                                       | `status ∈ {waiting, running, done, cancelled}`. `parent_trace_id` is the creating/funding trace and derives the process (`Trace(parent_trace_id).process_id`); it is inherited by the completion trace. `required_caller_user_id` is mandatory; open completion is not supported. `partial_args` is pre-bound input merged with the caller-supplied input at completion (`input` overwrites `partial_args` on key collision). The completer's allowed input is derived as `action.input_schema \ keys(partial_args)`, not stored (§10); this is safe because changing an action's schema deactivates it and completing against a changed or deactivated action resets the step to `waiting` (§10), so the derivation never sees a moving target. `tx_id` is recorded atomically when status transitions to `done`. `price` is `action.price` snapshotted at step creation: the amount parked in the process's `locked`, the completion call's allocation and `gross`, spent when the step completes and refunded if it is cancelled. `EndProcess` atomically cancels all `waiting` steps tied to the process in the same transaction as closure; `cancelled` is terminal and carries no `tx_id`. An outstanding (`waiting` or `running`) step keeps its process open, its allocation parked in the process's `locked` (§10). |
| `Adjustment`        | `id`, `operator_user_id`, `target_user_id`, `direction`, `amount`, `reason`, `external_key`, `created_at`                                                                                                                                                                                                     | Immutable audit record for a superuser balance change. `direction ∈ {credit, debit}`: a credit is a positive out-of-band grant; a debit is a credit redemption obliging an out-of-band payout (§12). `amount` is positive. `external_key` is an optional opaque idempotency token from the out-of-band system. When present it is globally unique; a create with an existing `external_key` returns the existing record without applying the balance change again. Juice never interprets it, keeping the kernel payment-rail agnostic.                                                                                                                                                                                                                                                                                                                                                              |
| `Grant`             | `id`, `grantor_user_id`, `action_id`, `refresh_token`, `created_at` | A user's delegated upstream OAuth credential for exactly one action (§8). Unique per `(grantor_user_id, action_id)` — a re-consent overwrites in place. `refresh_token` is AES-256-GCM encrypted at rest and write-only: never returned by any read path. Deleted on revoke, on provider `invalid_grant`, and when a deactivating update, auth replacement, or delete invalidates the action (§8). A consent record, not an authentication credential. |
| `Receipt`           | `id`, `issuer_user_id`, `tx_id`, `trace_id`, `action_id`, `caller_user_id`, `process_id`, `args_hash`, `reply_hash`, `status`, `gross`, `net`, `fee`, `charge`, `reason`, `started_at`, `created_at`, `signature`                                                                | Immutable signed record for exactly one committed call. `caller_user_id` is the call caller. `started_at` is call start; `created_at` is settlement. `charge` is the amount actually drawn from the caller's funds: `= gross` on success, `≤ gross` on failure (settled descendants stay paid, §6), `0` on rejection.                                                                                                                                                                                                                                                                  |
| `Rating`            | `id`, `rated_tx_id`, `rated_receipt_id`, `rater_user_id`, `rating`, `note`, `created_at`, `signature`                                                                                                                                                                            | Immutable signed feedback record. `rating ∈ {0,1}`. At most one rating exists per transaction. `rated_receipt_id` may be null only for pre-receipt transactions. `note` is optional, nullable, human-readable, and included in the single Ed25519 rating signature payload.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `IdempotencyRecord` | `id`, `idempotency_key`, `counterparty_user_id`, `receipt_id`, `status`, `result_json`, `created_at`, `expires_at`                                                                                                                                                               | Cross-kernel only. `status ∈ {pending,complete}`. Insert pending before execution; complete atomically with transaction and receipt. Completion stores `result_json`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `DiscoveredKernel`  | `public_key`, `handle`, `introduced_by`, `stats_json`, `first_seen`, `updated_at`                                                                                                                                                                                    | One row per (kernel, introducer); accumulated from gossip (§13). Information only — never execution semantics, callability, pricing, or settlement. Location is not stored; the transport resolves a key when needed (§13).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

OpenAPI registration and federation create or update ordinary `Action` rows. They create no durable object parallel to `Action`.

## 4. Authorization, call validity, and traces

```text
CanCall(P, a) := active(a) ∧ ¬suspended(a.owner_user_id) ∧ (public(a) ∨ P = a.owner_user_id)
```

`CanCall` is about the process owner `P`, not the call caller `C`. Public actions are callable by any process owner. Private actions are callable only when the process owner is the action owner. The call caller may differ from both only if process-use authority permits it. An action whose owner is suspended fails `CanCall` regardless of `active`/`public` state: a suspended owner's actions are not callable and are excluded from action listings (§14); unsuspending restores them, since suspension preserves data (§12).

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
  CreateAdjustment    out-of-band credit grant / redemption with its audit record
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
  CreateOrReplaceGrant ReadGrant ListGrantsByUser UpdateGrantRefreshToken DeleteGrant DeleteGrantsForAction
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
| Deposit         | user credit, adjustment record (credit)                                                          |
| Withdrawal      | user debit, adjustment record (debit)                                                            |
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

`CreateAction`: inactive by default. Validate action owner, name, kind, non-negative price. `description`, schemas, and source are required at activation. WASM creation validates or compiles only when an executor is configured. HTTP creation validates endpoint configuration without calling it unless requested; every `kind=http` action — manual or imported — stores one structured source (verb, base URL, path, parameter bindings), with an optional verb (default POST) and explicit or implicit field routing (§8). Reject non-HTTP(S), loopback, RFC 1918 private, and link-local `169.254.x.x` source URLs at creation and activation. Normal `CreateAction` rejects `kind=native`.

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

Allowed methods: GET, POST, PUT, PATCH, DELETE. Method is stored in `Action.source`, not `Call()` semantics. Imports and manual `kind=http` actions share one source representation, distinguished only by `source.type` (`openapi` vs `http`); import reconciliation (§8 match key) is scoped to `source.type=openapi` rows and never touches manual actions.

Required or rejected/kept inactive with validation messages: `operationId` or `x-juice-name`; `description` or `summary`; parameters and/or requestBody schema; 2xx JSON response schema; optional `x-juice-price` defaulting to 0. Path, query, and JSON body fields compile into one canonical `input_schema`; selected 2xx JSON response schema becomes `output_schema`.

Never active: non-JSON responses, streaming responses, multipart uploads, ambiguous success schemas, unsupported authentication, unsafe URLs, and invalid schemas. Invalid schemas and unsafe source URLs are rejected at import time, reported in the rejection list, and not stored inactive. Ratings and stats are never imported.

Draft import needs no API ownership proof. Public activation requires proof by well-known challenge, challenge in the OpenAPI document, or verified credential.

Authentication to upstream APIs is per-action: the importer stores an auth config in a dedicated write-only `auth_json` column — `{scheme, config, secrets}` — stored AES-256-GCM encrypted at rest; applied by a replaceable authenticator adapter at HTTP dispatch (§9). Secrets are write-only: never returned by any read path, never visible to scripts, never present in args, replies, logs, receipts, hashes, or manifests, and excluded from contract comparison. Implemented schemes: `header` (static header), `query` (query parameter), `bearer` (Authorization: Bearer), `basic` (HTTP Basic Auth), `oauth_client_credentials` (client id/secret exchanged at the token endpoint for a bearer), `oauth_jwt_bearer` (RFC 7523: a stored RSA private key signs a JWT assertion exchanged at the token endpoint), and `oauth_delegated` (per-caller authorization-code+PKCE or device flow). The scheme and its required config/secret keys are validated at action create/update, and dispatch fails closed on an unknown scheme — a request is never sent unauthenticated because its scheme was unrecognized. HMAC request signing is unsupported; operations requiring it stay never-active.

Owner-held schemes live entirely in `auth_json`. The `oauth_delegated` scheme stores provider config only — `auth_url`, `token_url`, optional `device_auth_url`, `client_id`, `scopes`, optional `client_secret` — while the per-caller credential is a `Grant` row (§3) created by the consent flow (§12). **Binding rule (confused-deputy defense):** at dispatch a delegated token is applied iff `grant.grantor_user_id = process.owner_user_id` (the paying human who ran the tree) and `grant.action_id` = the executing action's id (the exact code they trusted). Delegation therefore never transfers to a subcall of another action, never crosses federation (a `remote_proxy` executes the proxy, not the http action, so the token never leaves the kernel), and WASM scripts never see tokens. Consent is lazy and a precondition, not an error to interpret: a call to an `oauth_delegated` action whose process owner holds no matching grant is rejected with the typed `ErrGrantRequired` (§12), carrying the action reference as structured metadata so any client detects it by code — not by parsing a message — and can drive consent. The rejection happens before any funds are locked and before any transaction exists (so an unconsented call never charges the caller nor dents the provider's failure stats, §9). Access tokens are cached in memory only, refreshed from the stored refresh token, and dropped on an upstream 401; provider refresh-token rotation persists the new refresh token; provider `invalid_grant` deletes the Grant so the next call re-consents. A deactivating update (§7), an auth replacement, or action deletion revokes the action's grants — consent binds to the contract, not the enabled bit. Token-endpoint fetches obey the same SSRF discipline as action sources (§7).

This is OAuth's own trust model mapped onto Juice's principals: the grant is consent to an identified client — the granted action — exactly as deployed OAuth names one token holder while the services composed above it consume its output unseen. Juice keeps that boundary but governs what OAuth leaves ungoverned: a grant, like every resource of the process owner, is exercisable by the call trees the grantor funds, and each use is price-bounded, ledger-attributed (the role law records whose code requested it), and ratings-disciplined. The confinement line is therefore *data at the run boundary, tokens absolutely*: what a granted action returns flows to its caller like any result, while the token itself never leaves dispatch.

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

`admin friend` fetches the remote's active public actions, verifies each signed manifest, and creates or updates local `kind=remote_proxy` actions owned by the local remote-peer user row. Imported actions are immediately enabled and public. It does not copy implementation.

Manifest required fields:

```text
action_id owner_handle name description input_schema output_schema price kind
artifact_hash stats updated_at signature
```

Only active public remote actions have manifests; an action using the `oauth_delegated` auth scheme (§8) is never served as a manifest and never gossiped, because a remote peer's proxy user can never complete a browser consent. A kernel serves manifests and gossips (as its own exposed actions) only actions it owns — `kind ∈ {http, wasm, native}`; an imported `kind=remote_proxy` action is never re-served, so friendship stays non-transitive: reaching a peer's imported action requires friending its true owner directly. Manifest descriptions and schemas are the canonical interface used by importing kernels for lookup and LLM function calling. `signature` is the remote platform Ed25519 signature over canonical JSON excluding `signature`, verified against the remote peer's `public_key`. Match key:

```text
proxy.owner_user_id + proxy.remote_action_id
```

Remote contract fields:

```text
action_id artifact_hash description input_schema kind name output_schema owner_handle price
```

Manifest stats and `updated_at` do not affect contract comparison; manifest stats never overwrite local `Stats` and are not stored as `StatTag`. Invalid signatures skip that action. A second `admin friend` re-syncs: new actions are imported, changed-contract actions are updated (re-enabled), and actions no longer active/public on the remote are deactivated and their stats reset. `admin unfriend` deactivates all proxies from that peer and preserves all history.

The proxy's local `price` is `manifest.price` plus the worst-case import duty: `price = mp + ceil(mp * import_bps / 10000)` (§13). The local caller sees one price bounding the whole remote call, duty included; settlement charges duty on the actual remote charge and refunds the difference (§13). A change of the local `import_bps` recomputes proxy prices but is not a manifest contract change and does not deactivate.

## 9. Adapters, native actions, and stats

WASM uses wazero. Scripts receive no ambient filesystem, network, environment, process access, or raw user tokens. They receive only explicit host functions, each execution having memory limit, timeout, deterministic context cancellation, and artifact-hash compiled-module cache. The network is reachable only mediated, never ambient: a script holds no sockets and reaches the web solely by calling the read-only, SSRF-restricted `@sys/web` action (or a provider's pinned `kind=http` action) through `Call()`, charged like any other call. Store source and artifact; authorized users may inspect source; activation should precompile; compilation failures are typed.

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

Upstream authentication is a replaceable adapter: `Authenticator.Apply(request, auth) -> request` transforms an outbound HTTP request using the action's stored auth config (§8); schemes are implementations behind this interface, including the OAuth token exchange and in-memory token cache (§8), and `kernel` must not import them. Tests use fakes.

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
| `@sys/web`      | Public; action owner `@sys`; price 0 (configurable, `native.web`, §14); callable through `Call()`. Read-only fetch of a public web page. Input: required `url` (string); a scheme-less `url` defaults to `https` (HTTPS-first, like a browser), and an explicit `http`/`https` scheme is respected and never silently downgraded. Output: `status` (HTTP status integer), `body` (response body string), `content_type` (response `Content-Type` string), `final_url` (the URL actually fetched, after scheme defaulting and redirects). GET only; no caller-supplied headers or auth, so nothing sensitive enters args/receipts/logs. A fixed, configurable descriptive `User-Agent` is set by the action itself. Same SSRF discipline as `kind=http` (§7): loopback, RFC 1918 private, and link-local `169.254.x.x` hosts are rejected with `ErrInvalidInput` unless `allow_local_sources` is set. Non-2xx statuses are returned in `status`, not raised as errors, so crawlers can react to them; 10 MiB response cap. `ErrInvalidInput` for empty `url`; `ErrInvalidState` if the fetcher is unconfigured; `ErrExecutionFailed` on transport failure. The mediated path by which WASM scripts read the network: scripts still receive no ambient sockets — they reach the web only by calling this action through `Call()`, charged and SSRF-restricted to public hosts (§9). |
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

The same PKCE/device machinery serves upstream OAuth consent for `oauth_delegated` actions (§8): the client hosts the redirect target — a local client (CLI/desktop) catches it on a loopback listener reachable by the operator's own browser even behind NAT; a hosted client uses its own provider-registered callback URL — and drives `POST /v1/grants/start` / `POST /v1/grants/complete` authenticated as the grantor; the kernel holds only short-lived in-memory PKCE/device state (~10 min TTL) and performs the code→token exchange itself, so no unauthenticated callback route exists on the kernel and the refresh token never passes through the client. The kernel never dials `redirect_uri`; it only embeds it in the authorize URL, and the OAuth provider's registered-redirect allowlist (exact match) is what binds an issued code to a legitimate client, so any http(s) redirect is accepted and the consent-phishing vector is closed at the provider. In-memory consent state is single-use, TTL-bound, and completable only by the grantor who started it.

Errors:

```text
ErrUnauthenticated ErrUnauthorized ErrNotFound ErrInvalidInput ErrInvalidState
ErrInsufficientFunds ErrExecutionFailed ErrSchemaViolation ErrTimeout ErrInternal
ErrGrantRequired
```

`ErrGrantRequired` is a precondition failure, the twin of `ErrInsufficientFunds` — the process owner must delegate an upstream OAuth grant before the action can run (§8). It carries the action reference as structured metadata so clients act on a code, not a message.

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

Private signing key and JWT secret are never logged or returned. Partial first boot is rerunnable. `JUICE_SECRET_KEY` overrides stored JWT secret at runtime only. First boot also **requires a `kernel_handle`** — the name this kernel presents to the network (§13): from config, else `JUICE_BOOTSTRAP_KERNEL_HANDLE`, else an interactive prompt that repeats until a non-empty name is given; a headless first boot with none set fails rather than name the kernel silently.

The kernel's federation network identity is derived deterministically from this same Ed25519 signing key; there is no second identity or network key. The `public_key` is simultaneously the kernel's Juice identity (§13) and its address on the federation transport. The signature domains of the transport handshake and of Juice payloads (receipts, ratings, manifests, federation requests) must be disjoint: no byte string signed in one domain may verify as a valid message in the other. This disjointness is verified by test (§15).

Every startup reads `config.superuser_handle` to confirm first boot and identify `@sys`; it verifies signing keys and aborts if either is absent. It then registers, enables, and makes public `@sys/lookup`, `@sys/llm/chat`, `@sys/llm/embed`, `@sys/llm/json`, `@sys/llm/decide`, `@sys/make`, `@sys/time`, `@sys/sink`, `@sys/message`, `@sys/random`, `@sys/web`, and `@sys/tinygo/compile` if absent, and reconciles their configurable fields (price and action-specific settings) from config on every startup. It then runs recovery (§5).

Bootstrap is idempotent. Supervision operations are not native actions.

The superuser may suspend or unsuspend users. Suspension preserves data and makes every authenticated request return `ErrUnauthenticated`. It also excludes the suspended user's actions from action listings and makes them uncallable (`CanCall` fails on a suspended owner, §4); their data survives and unsuspending restores listing and callability.

`Kernel.Deposit(operator_user_id,target_user_id,amount,reason)` is admin-CLI-only supervision. It requires configured superuser and positive amount, then atomically credits `user.available` with a credit adjustment record. `reason` is optional and stored when provided. The operation is idempotent over `external_key` when supplied. It is served on the public TCP API (§14) as a superuser-gated route: authority is the `@sys` bearer token, not filesystem access.

`Kernel.Withdraw(operator_user_id,target_user_id,amount,reason)` is admin-CLI-only supervision: the mirror of `Deposit`. It requires configured superuser, positive amount, and `target.available ≥ amount`, then atomically debits `user.available` with an immutable debit adjustment record — the user's credits are redeemed and the operator owes the out-of-band payout. The operation is idempotent over `external_key` when supplied: a replay returns the existing adjustment record before the `target.available ≥ amount` check runs, so a replayed withdrawal never fails on a balance that has since dropped. It is served on the public TCP API (§14) as a superuser-gated route: authority is the `@sys` bearer token, not filesystem access.

`Kernel.UpdateUser(callerUserID, email, currentPassword, newPassword)` is user self-service: only the authenticated, non-suspended local user may update their own account. `email` and `newPassword` are both optional; at least one must be provided. When `newPassword` is non-empty, `currentPassword` must match the stored hash; mismatch returns `ErrUnauthenticated`. An account with no password credential (a key-only account) cannot use this operation (`ErrInvalidState`). `handle` is immutable. The update is atomic.

## 13. Federation

### Transport

Federation has exactly one carrier: a peer-to-peer transport (libp2p) behind the replaceable `fed` interface (§2), which `kernel` never imports. The kernel addresses a peer only by its `public_key`; the transport resolves that key to a live connection — direct when the peer is publicly reachable, hole-punched through NAT when possible, relayed through a public helper node as a last resort. Streams are mutually authenticated by peer key, so a connection is itself proof of the counterparty's network identity; the per-request Juice signatures below are nonetheless retained deliberately, because receipts, rejections, and dispatch records must be storable and verifiable offline (§11) — channel authentication cannot replace a signed artifact. The transport-handshake and Juice-payload signature domains are disjoint (§12).

On startup the kernel announces its key to the discovery network (DHT / rendezvous), bootstrapped from `bootstrap_peers` in `juice.json` (§14). A bootstrap peer is just a publicly-reachable kernel (every kernel runs the DHT and a relay, §13 above); the shipped default points at the project's public node, so a fresh `juice serve` joins out of the box. An empty list means the kernel neither announces nor discovers. Beyond joining, `bootstrap_peers` is also the **seed of the known network** (§13 Gossip): on a timer (`discovery_interval_seconds`, §14) the kernel advertises itself as a provider under a fixed discovery key, enumerates that key to learn other online kernels, and pulls gossip from the bootstrap seeds plus enumerated providers into `DiscoveredKernel` — so a fresh box has a directory to friend from without already knowing anyone. This directory pull is separate from gossip's reputation role and from friending: learning a kernel this way grants nothing (calling still requires a friendship and a deposit, §13). Because a libp2p peer ID inlines its Ed25519 key, a bootstrap peer supplied only as a multiaddr is addressable and inspectable by the same base64url key every federation command takes. Publicly-addressed and NAT-bound kernels are indistinguishable in how they federate; a home kernel behind a router federates identically to one on a public host, with no advertised address, port-forwarding, or `.well-known` document of any kind (the local `server_url` in §14 is only the loopback URL the CLI dials to drive your own kernel, never a federation address).

Federation protocols are versioned libp2p streams: `/juice/fed/call/1` (inbound proxy call), `/juice/fed/friend/1` (friend handshake), `/juice/fed/manifest/1` (manifest serving, chunked per action so a large-catalog sync survives bandwidth-capped relayed connections), `/juice/fed/gossip/1`, and `/juice/fed/inspect/1`. Payloads and verification are exactly the settlement rules below; only the carrier is libp2p.

Inbound federation traffic is resource-limited at the transport: per-source-address limits where an address is visible, per-peer stream and byte budgets, a global inbound cap, and stricter budgets for relayed (address-less) traffic. Peer identities are self-issued and free to mint, so per-key limits alone are never sufficient against Sybil flooding. These transport limits replace §14's per-IP peer-request rate limit.

### Peers and proxy users

There is no user "kind" — every user is one account model distinguished only by the credentials it holds. A **password account** (from `user create`) logs in and holds tokens; a **key account** (`public_key` set, created only by peer acceptance) is a peer kernel's account here and authenticates by federation signature, per request. An account can't get a token without a password, and can't be a federation counterparty without a key. Because a peer is just a user, federation adds no new money model: a key account holds credits, pays, and is paid like anyone else.

A `@handle` is a name in one kernel's namespace; a `public_key` is the name in the global (inter-kernel) namespace. Both name accounts; the `@` prefix vs a base64url key tells them apart, so any command naming an account accepts either. Friending a peer mounts its namespace under a local alias: peer B's action originally owned by `alice` is imported owner-qualified and addressed `@B/alice/greet` — the owner becomes part of the action name (`@B` names the mount, `alice/greet` the action), so two owners on B with the same action name never collide. The relation is not transitive, so the graph flattens into each observer's root rather than nesting. Key rotation is unsupported, so a lost key is a lost identity and lost reachability at once; a peer's self-reported handle is only a default alias — each importer binds its own, auto-suffixed on collision. Settlement rolls up to the mount: every one of B's owners settles into the single proxy user for B (the bilateral account), so `@B/alice/…` and `@B/bob/…` name and attribute but do not fragment the wallet.

### Friending — the ACT relation

Two kernels transact only as **friends**: a reciprocal relation with the proxy-user pair (`@B` on A, `@A` on B).

```text
juice admin friend <key>      register peer by public key + bulk-import all their active public actions
juice admin unfriend <user>   end the relation; deny future requests; deactivate all proxies (user is @handle or key)
juice admin peers             friended peers and balances (--all also shows denied/unfriended)
juice admin inspect <key|user>  view remote identity, public actions, transacted friends, and reachability (key, or @handle if already friended; no DB write)
```

Federation trust is superuser supervision, so these live under `admin`, served on the public TCP API as superuser-gated routes (§14). The inbound friend handshake is the `/juice/fed/friend/1` protocol, authenticated by federation signature — not a local API. `admin inspect <key>` is the operator's window into a remote kernel (there is no browser-reachable federation endpoint): it reports the peer's identity, public actions, and transacted friends, plus reachability diagnostics (direct / hole-punched / relayed, latency, protocol versions).

Federation commands are defined for an offline peer and bounded so they fail promptly: `admin friend` fails as unreachable, `admin inspect` degrades to the last-known local data with reachability `unreachable`, and `admin unfriend`/`peers`/`identity` are local and always work.

`admin identity` prints this kernel's own federation identity — its public key (the value peers friend it by, since there is no `.well-known`), handle, and libp2p listen addresses. Every kernel runs a circuit-relay service and joins the discovery DHT, so a **publicly-reachable `juice serve` automatically acts as the network's bootstrap + relay** — the meeting point NAT-bound kernels announce to and are reached through; there is no separate seed process. A public node binds the standard federation port `31313` for a stable address (a NAT-bound node uses an OS-assigned port and is found by key). `friend` opens an authenticated stream to `<key>` and sends a signed request; the peer's self-reported handle arrives over the protocol. By default kernels **auto-accept** (`peer_auto_accept = true`): the proxy user is created with balance 0 and a reciprocal request completes the pair. With manual mode, requests sit pending until the operator friends back. Friend requests are subject to the §13 transport resource limits.

Friendship by itself grants nothing: a zero-balance friend's calls are all rejected. The trust decision is the **deposit** — an operator credits a friend's proxy user only after real money moved out of band (§12). Friendship exchanges keys; funding expresses trust.

`unfriend` ends the relation and puts the peer's key on the deny list (`denied_at`): future requests from that key are rejected, not auto-accepted. Their imported proxies here deactivate; manifests stop being served to them; new inbound calls get signed rejections; waiting steps with the peer as `required_caller` are cancelled and their parked prices refunded (§10). The balance and all history survive — the operator settles the net by `Withdraw` (§12). In-flight outbound calls still settle on their receipts: the receipt path stays open for pending idempotency keys. Notification is best-effort; a peer that missed it learns from its next rejection. Your own `friend` on a denied key clears the denial and restarts the handshake — only you can unblock, by choosing to re-friend.

The relation is **not transitive** and is the only path to calling: invoking `@B`'s actions requires direct friendship with B. Exposure is each side's lever: a kernel may stop serving manifests to a friend whose balance cannot cover its cheapest exposed action; funding restores them.

### Gossip — discovery and reputation

Gossip is information, never authority. The `/juice/fed/gossip/1` protocol (open, read-only) returns the kernel's identity, its own exposed actions with manifests and local stats, and its **transacted friends** — friends it has settled calls with — each with key, handle, and the kernel's earned local stats for that friend's actions. Gossiped actions are named owner-qualified (`@owner/name`) in the originating kernel's namespace, never bare. No URLs appear: a friend is identified by key, resolved through the transport. Friends without settled traffic are not gossiped: endorsement is earned by trade, never granted by friending.

Two engines feed the known network, matched to its growth goal. The **directory** — *which* kernels exist — grows fast and broad from DHT provider-record enumeration (the announce/discover pass above): every kernel provides a fixed discovery key and enumerates it, so the roster fills at the rate kernels come online. **Gossip** is the reputation overlay on top: it is deliberately trade-gated (transacted friends only), so it can never be the directory — it supplies earned stats for entries the directory surfaces. A kernel accumulates both into `DiscoveredKernel`, one row per (kernel, introducer); the introducer is the kernel itself for a first-party self-report (directory/self gossip) and the telling friend for hearsay. Supervision surfaces this as a grouped roster (kernel → introducer → action), with the operator's own earned stats shown distinctly from gossiped opinion, since own stats dominate the priors as they accumulate (§9).

Gossip results accumulate in the local discovery table, one row per (kernel, introducer):

```text
DiscoveredKernel { public_key, handle, introduced_by, stats_json, first_seen, updated_at }
```

Third-party stats are stored namespaced by source (`StatTag`, `source = <introducer>`) and affect **ranking only** — priors weighted by introducer count and local experience with the introducer, dominated by own `Stats` as they accumulate. They never affect callability, pricing, or settlement. Knowing a kernel through gossip permits nothing: calling requires your own friendship, and manifests are always fetched and verified from the owner, never trusted from an introducer.

Importing an action initializes its local `Stats` to the defaults (§3): ground truth starts empty and is earned by settled calls. Priors are consulted at ranking time from the stored manifest (the owner's claim) and `StatTag` (introducers' earned stats), never copied into `Stats`. How priors combine is the ranking layer's tested, replaceable formula (§9); the kernel mandates only the signals and the discipline above.

### Money — prepaid credits

Federation is prepaid. For A's users to call B's actions, A must hold credits on B: A's operator pays B's operator out of band; B's superuser `Deposit`s the `@A` proxy user (§12). Redemption is the mirror, by `Withdraw` (§12). Credits cross the bank boundary only by supervision; execution never mints or burns.

The proxy user's balance is the bilateral account: `@B`'s balance on A rises when A's users import from B (settlement pays it as `target_user_id`, §6) and falls when B's users import from A (inbound calls spend it). Operators settle only the net, out of band, at their own cadence. Netting needs no kernel machinery — it falls out of the peer being a user. The depositor bears counterparty risk, bounded by the deposit: keep deposits small and settle often.

### Calls, receipts, settlement

Outbound: a remote-proxy call follows normal role law (`owner` = local process owner, `caller` = local call caller, `target` = the proxy user) and normal wallet mechanics, funded with the proxy's local price `mp + maxduty` (§8). The handler records the UUID v4 `idempotency_key` and the outbound request payload (`dispatch_json`) on the proxy trace atomically with dispatch (§3, §5) — the stored payload is what makes retry after restart possible. On the remote kernel it arrives over the `/juice/fed/call/1` protocol, authenticated by the per-request federation signature (not a bearer token), and is then an ordinary inbound call by this kernel's proxy user there, paid from the prepaid balance, executed wholly under that kernel's §6.

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

The no-receipt state is the expected steady state of a network of intermittently-online home kernels, not an error: a caller of an offline peer holds its allocation locked and its process open until the peer returns and a signed receipt settles the call, or the process owner forces closure. Process listings must surface this awaiting-receipt state — with the time it has been awaiting (the earliest parked call's start) — so an operator can see funds parked on an unreachable peer and for how long. Step listings likewise flag a waiting step whose required caller is a peer, so work parked on a possibly-offline peer is visible; both are factual (age, peer-ness), never a liveness claim.

Inbound: calls sign `JCS({action, counterparty, idempotency_key, timestamp, args_hash})`; `counterparty` is the caller's base64url public key; `args_hash = SHA-256(raw body)`. Receiver verifies signature, raw hash, friendship, timestamp age ≤ 5 minutes; non-friends and denied keys are rejected. A valid signature authenticates the proxy user for that request; the call is `run` as that user, paying from its balance. Any inbound call the receiver can determine will not execute — insufficient balance, or a known-but-non-executable action (inactive, non-public, suspended owner) — returns a **signed rejection receipt** (`status = failure`, zero charge) carrying the action's id, so the caller always has something to settle on rather than pinning funds until the pending bound. Only a genuinely absent or unverifiable action stays a plain error (no receipt whose `action_id` could match the caller's stored `remote_action_id`), leaving the caller pending. Idempotency: insert pending before execution, unique `(idempotency_key, counterparty_user_id)`; completed replay returns the stored receipt, pending replay 409, expiry 24h.

`VerifyRemoteReceipt(caller_id, tx_id)` requires `CanReadTransaction` and verifies entirely locally, in two parts. **Receipt integrity:** signature against the peer's `public_key`, stored JSON against its stored SHA-256, `receipt.action_id == proxy.remote_action_id`. **Settlement consistency:** the local transaction's outcome matches `receipt.status`; the amount paid to the proxy user equals `receipt.charge`; the local refund equals `(mp + maxduty) − receipt.charge − duty` with duty per this section (zero on failure); `args_hash` and `reply_hash` match the local record. The receipt's own `gross/net/fee` are the remote kernel's economics and are reported, not compared. Returns per-check results and top-level `valid`; non-proxy transactions give `ErrInvalidState`.

### Retention

A peer is kept only while it holds value or has been used recently. Its activity is the most recent settled call (either direction), gossip mention, or deposit/withdrawal; a nonzero balance or funds locked in flight always count as live. A peer idle past `peer_retention_days` (§14) — reachable only at zero balance with nothing locked — has its accumulated data purged: its proxy actions, their stats, its `StatTag` rows, and its `DiscoveredKernel` rows, and its identity is forgotten (its `public_key` is cleared, so re-friending starts fresh). The immutable transaction and receipt ledger is preserved — its party ids carry no foreign key, so a now-dangling peer id is harmless and every local counterparty's credits stay reconstructible (§11); the anonymized user row remains as a legible ledger anchor. Purge is reachable only through zero value, so it never deletes funds or a still-reconstructible credit.

## 14. CLI, HTTP, logging, config

HTTP API is primary. Every exposed endpoint has a CLI command. CLI uses the same service layer, supports human-readable and JSON output, and each command has at least one test. All commands — user-facing and admin/peer alike — are HTTP clients of the server (base URL from `--server`/`JUICE_SERVER`/`server_url`); admin and peer commands hit the same public TCP API, on routes gated by an `IsSuperuser` check, authenticated by the `@sys` bearer token (no separate socket or filesystem authority — keep the bearer secret and run `serve` behind TLS or on loopback). `juice serve` is the sole process that opens SQLite; the CLI never touches the database directly. A command's primary identifier is a positional argument by its natural key — a user is `@handle` (never an id), an action is `@owner/name` (an id is also accepted), and processes, steps, and transactions are ids; a second mandatory value (amount, rating) is the second positional. CLI human-readable output exposes the same fields as the corresponding HTTP response; `--json` selects the canonical JSON form (the HTTP shape).

CLI handlers and HTTP handlers are thin wires: parse input, call the public TCP HTTP API (admin/peer commands hit superuser-gated routes on that same API), and format output. All kernel calls, enrichment, validation, and transformation live server-side in the service layer. No kernel calls outside the service layer.

Required commands:

```text
juice serve
juice user create <user> <email>          juice user me
juice user update                          juice user connect <action>
juice user disconnect <action>
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
juice admin friend <key>                  juice admin unfriend <user>
juice admin peers                         juice admin inspect <key>
juice admin identity
```

`admin` holds only the operator verbs no ordinary user performs — money, access, federation trust, and the global roster (`users`/`show`). Supervision over everything else is **scope on the normal commands**: a superuser sees all owners' rows on `action list`, `process list`, `tx list`, and `step list`, and may `action disable`/`enable` any action, all over the public TCP API. As for everyone, `action list` is active-only by default; inactive/private rows appear only with `--all` (`?all=1`), so a deactivated action — e.g. an unfriended peer's proxies — drops out of the default list. There is no `admin actions/disable/processes/txs/steps` — those were duplicates of the base commands with wider reach.

OpenAPI commands (the OpenAPI spec URL is the positional argument):

```text
juice action import <spec-url>
juice action unimport <spec-url>
juice action unimport <spec-url> --name <action-name>
```

`juice serve` handles `SIGTERM`/`SIGINT`, stops accepting new requests, drains in-flight calls, exits cleanly. No `juice stop`.

Server logs request, caller, process, trace, action, and transaction IDs where available; maps distinct auth, authorization, invalid input, insufficient funds, missing resource, and internal failures to distinct statuses; rate-limits auth and account creation per IP with 429 (inbound federation traffic is limited at the transport instead, §13). Action read/list responses include computed `action=@owner/name`. Outputs render user identities as `@handle`, never as raw user ids: a returned id must be a consumable input to some command, and no command accepts a user id (users are addressed by `@handle` or public key). So transaction responses carry `owner_handle`/`caller_handle`/`target_handle` (not the stored `*_user_id`), step responses `required_caller_handle`, process responses `owner_handle`, deposit/withdraw responses `operator_handle`/`target_handle`, and the peer list carries no internal id — while the underlying immutable records keep their captured `*_user_id` fields (§3). A handle unresolvable at read time (a purged party, §13) falls back to the raw id. `GET /v1/me` is the sole exception: it returns the caller's own `id`.

Endpoint rules (notable rules only; the complete HTTP endpoint list is in `API.md`):

| Endpoint                                         | Rule                                                                                                            |
| ------------------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| `GET /health`                                    | unauthenticated → `{status, handle, public_key}`: liveness plus the kernel's advertised federation identity (§13), so a client sees which kernel it's on before logging in |
| `GET /v1/actions[?owner=&name=&all=]`             | unauthenticated → active public actions; authenticated → active public actions plus caller's own active actions (union, deduplicated); superuser → all owners' active actions; a suspended owner's actions are excluded (§12); `?all=1` includes inactive/private rows in the caller's scope; `?owner=` further filters by that owner's handle; `?name=` filters by name |
| `GET /v1/me`                                     | authenticated `id`, `handle`, `email`, `available`, `locked`, and `grants` (each: action `@owner/name`, requested scopes, `created_at`; no token material); suspended rejected before handler |
| `POST /v1/grants/start`                          | authenticated; `{action, [redirect_uri], [flow]}` begins delegated-OAuth consent (§8); `redirect_uri` is any http(s) target the client will receive the code at (loopback for local clients, a registered callback for hosted ones); code flow returns `{state, authorize_url}`, device flow `{state, verification_uri, user_code, interval, expires_in}` |
| `POST /v1/grants/complete`                       | authenticated as the grantor; `{state, [code]}`; exchanges the code (or reports device-poll `pending`) and stores the grant; foreign/expired state rejected |
| `DELETE /v1/grants?action=@owner/name`           | authenticated; revokes the caller's grant for that action                                                       |
| `PUT /v1/me`                                     | authenticated password account only; `{[email], [current_password, password]}`; `password` requires `current_password`; at least one field required; returns updated `id`, `handle`, `email`, `available`, `locked`; a key-only account (no password) returns `ErrInvalidState` |
| `PUT /v1/actions/{id}`                           | action-owner update; `public` updatable; deactivation rules apply                                               |
| `DELETE /v1/actions/{id}`                        | action-owner delete preserving history                                                                          |
| `POST /v1/actions/import`                        | authenticated OpenAPI supervision import                                                                        |
| `POST /v1/actions/unimport`                      | action-owner import-provenance deactivation                                                                     |
| `GET /v1/processes`                              | process owner's processes, descending `created_at`; each carries `awaiting_receipt` and, when set, `awaiting_receipt_since` (§13) |
| `GET /v1/steps`                                  | authenticated; returns steps visible to caller per `CanListStep`; optional `?process_id=` and `?status=` filters; each step carries `created_by` (the creating action `@owner/name`, from its parent trace) beside `action` (the completion target); a waiting step also carries `allowed_input` (the derived completion schema `action.input_schema \ keys(partial_args)`, §10) so its required caller can complete it without separately reading a private target action; a waiting step whose required caller is a peer carries `waiting_on_peer` (§13) |
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

Admin commands require configured `@sys`, reject non-superusers with `ErrUnauthorized`, and stay outside `Call()`. The operator verbs (money, access, federation trust, roster reads) are served on the public TCP API as dedicated routes gated by an `IsSuperuser` check — not a separate surface. Superuser *scope* on the normal read/toggle endpoints (seeing all rows, disabling any action) is enforced the same way, mirroring the existing transaction/step widening. Admin authority is therefore the `@sys` bearer token alone; operators keep it secret and expose `serve` only behind TLS or on loopback.

Logs go to stderr and optionally file; stdout is resource payloads only. Configurable format, level, file. Every kernel transition logs start/end; errors include stable codes; script logs include trace ID.

Required log fields:

```text
time level event request_id caller_user_id process_id trace_id action_id tx_id
status duration_ms error
```

Config lives in `juice.json` (path from `JUICE_CONFIG`, default `./juice.json`). Top-level kernel keys: `db_path`, `fee_bps`, `import_bps`, `peer_auto_accept`, `server_url` (the local server base URL the CLI dials for user-facing commands — a loopback address for driving your own kernel, not a federation identity), auth issuer/audience/token TTL, log file/format/level, script timeout and memory limits, plus the federation identity and discovery keys this kernel needs to satisfy §8 and §13: `kernel_handle` (the handle this kernel presents to the network in friend handshakes and gossip), `bootstrap_peers` (the peer multiaddrs the transport dials to join the discovery network; defaults to the project's public node so `juice serve` works out of the box, empty means the kernel neither announces nor discovers), `credentials_key` (the §8 base64url AES-256-GCM key for `auth_json` and sealed OAuth grant refresh tokens, auto-generated at first boot; each action's OAuth provider config lives in its `auth_json`, so there are no OAuth keys in `juice.json`), `allow_local_sources` (dev-only escape hatch over §7's loopback/private/link-local URL rejection for action source URLs, default `false`), and `remote_retry_interval_seconds` (seconds between passes of the running server's retry loop that re-drives pending remote-proxy calls so a returning peer settles parked calls — and the §13 max-age refund fires — without a restart; default 60, non-positive falls back to the default), `discovery_interval_seconds` (seconds between passes of the known-network discovery loop that advertises this kernel, enumerates providers, and pulls gossip from bootstrap + discovered peers into `DiscoveredKernel`, §13; default 300, non-positive falls back to the default; an empty `bootstrap_peers` disables discovery entirely), and `peer_retention_days` (days a peer may stay idle at zero balance before it and everything derived from it are purged, §13; default 90, non-positive disables purging). All native-action configuration lives under `native.<action>`; no deeper nesting:

```json
{
  "db_path": "./juice.db",
  "fee_bps": 2000,
  "import_bps": 500,
  "peer_auto_accept": true,
  "server_url": "",
  "kernel_handle": "",
  "bootstrap_peers": ["/dns4/daios.ai/tcp/31313/p2p/12D3KooWJ5ZwPSAV17q2hv6ttZ8J3hHsTMNaSC61kbVbvxvtArjK"],
  "credentials_key": "",
  "allow_local_sources": false,
  "remote_retry_interval_seconds": 60,
  "discovery_interval_seconds": 300,
  "peer_retention_days": 90,
  "native": {
    "llm":     { "url": "http://localhost:11434", "chat_model": "gemma4:26b", "embed_model": "nomic-embed-text", "price": 0 },
    "make":    { "compiler": "tinygo", "max_steps": 5, "price": 20 },
    "lookup":  { "default_limit": 10, "price": 0 },
    "time":    { "price": 0 },
    "sink":    { "price": 0 },
    "message": { "price": 0 },
    "random":  { "price": 0 },
    "web":     { "price": 0, "user_agent": "juice-kernel/0.4 (+https://github.com/daios-ai/juice)" },
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
JUICE_BOOTSTRAP_KERNEL_HANDLE  kernel name, required for an unattended first boot (§12, §13)
JUICE_ALLOW_LOCAL_SOURCES  dev-only: permit loopback/private/link-local action source URLs (§7)
```

All other settings are configured through `juice.json` only; there are no further environment overrides.

## 15. Required automated tests

`go test ./...` must pass without external network access. Tests use temporary SQLite databases, fake Ollama, fake script, and fake `fed`-transport adapters unless explicitly integration tests, no global state, and no order dependence.

Federation is tested in three tiers. **Unit** (`go test ./...`, offline): kernel federation logic runs against a fake `fed` transport, exercising every §13 settlement rule without a real network. **Flows** (offline, real transport on loopback): the multi-kernel flow suite runs the actual libp2p transport over `127.0.0.1`, with one kernel serving as the bootstrap + relay for the others (every kernel runs the DHT and relay, so no separate seed process) — discovery-by-key, relayed carriage, and restart-retry are exercised on one machine with no internet. **Real-network check** (release gate for any federation-touching change, not part of `go test ./...`): a scripted flow run from a machine behind a real NAT against one remote peer, asserting hole-punch and relay-fallback paths that loopback cannot reproduce.

Required suites:

```text
user creation
authentication token validation
user update email; change reflected in GET /v1/me
user update password with correct current_password; old password rejected after change
user update password with wrong current_password returns ErrUnauthenticated
user update with neither email nor password returns ErrInvalidInput
key-only account (no password) UpdateUser returns ErrInvalidState
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
lookup demotes a persistently-failing action below an untested one (Laplace-smoothed quality)
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
@sys/web returns status, body, and content_type for a public URL (fake fetcher)
@sys/web with missing or empty url returns ErrInvalidInput
@sys/web with unconfigured fetcher returns ErrInvalidState
@sys/web rejects loopback/private/link-local URLs unless allow_local_sources
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
suspended owner's active public action is excluded from GET /v1/actions and uncallable (ErrInvalidState); unsuspend restores both
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
unknown upstream auth scheme rejected at create/update and fails closed at dispatch
oauth client-credentials exchanges at the token endpoint and applies the bearer upstream (fake provider); token cached, dropped and refreshed once on a 401
oauth jwt-bearer signs an RFC 7523 RS256 assertion the provider verifies
delegated token applied iff grantor = process owner and grant action = executing action (both mismatch edges); refresh-token rotation persists; invalid_grant deletes the grant
call on an oauth_delegated action with no grant rejects before locking funds with structured ErrGrantRequired (code + action metadata): no transaction, no process, balances unchanged
grants/start accepts any http(s) redirect_uri (loopback or hosted), rejects a bad scheme
one grant per (grantor, action); re-consent overwrites; deactivating update / auth replacement / delete revokes the action's grants
grant tokens never appear in args, replies, receipts, logs, or any read path; /v1/me lists grants without tokens
grants/start and grants/complete require authentication; complete rejects another user's or an expired state
delegated-OAuth action is excluded from manifests and gossip; a remote-proxy call carries no local delegated token
OpenAPI import/unimport flow for API-owned actions
OpenAPI import compiles parameters and JSON body into one input schema
OpenAPI activation rejects incomplete schemas or missing descriptions
OpenAPI public activation requires ownership proof
OpenAPI import affects only matching OpenAPI-provenance actions
OpenAPI import preserves Action.id, deactivates on contract change, and resets current stats
remote import/unimport flow for signed manifests
remote import preserves Action.id, deactivates on manifest contract change, and does not overwrite local Stats
remote import initializes local Stats to defaults
remote import names actions owner-qualified (addressed @peer/owner/name); two owners on a peer with the same action name do not collide
admin deposit/withdraw/unfriend resolve a peer by key (global name) as well as by @handle
fresh boot with no configured handle derives a distinct @k-<key> kernel handle, never @sys
remote manifest signature is Ed25519 over canonical JSON excluding signature
remote proxy transaction has target_user_id = proxy user id
friend auto-accept creates zero-balance proxy pair; manual mode holds pending
zero-balance friend's inbound call gets signed rejection receipt
inbound call to a known-but-non-executable action (inactive, non-public, suspended owner) gets a signed zero-charge rejection receipt the caller settles on immediately
unfriend sets denied_at, deactivates proxies, cancels steps addressed to peer; balance survives
denied key's friend request rejected; own friend clears denial
gossip lists only transacted friends with stats, keyed by public key with no URLs; non-transacted friends absent
friending a peer does not import that peer's own imports (no transitive re-export); manifests and gossip exclude remote_proxy actions
offline peer: inspect degrades to local data + unreachable, friend fails as unreachable, unfriend/peers/identity work locally
gossip results stored per (kernel, introducer); StatTag namespaced by source
proxy call locks mp+maxduty; success settles charge+duty on actual charge and refunds difference
remote failure with charge refunds (mp+maxduty)−charge; duty is zero; settled remote work stays paid
failure counts against proxy stats regardless of charge
duty credited to @sys; charge credited to proxy user
timeout does not settle; retry with same idempotency key recovers receipt
restart with a dispatched proxy call resumes retry; receipt obtained after restart settles it; no interrupted refund
running server's retry loop settles a pending remote call when the peer returns, without a restart; max-age expiry fires from the running server too
process awaiting a remote receipt is reported with awaiting_receipt and its age; a waiting step addressed to a peer is flagged waiting_on_peer
inbound federation call rejected when args_hash does not match request body
full remote receipt JSON stored atomically with transaction on remote-proxy call
receipt verification returns valid for a well-formed stored remote receipt
receipt verification detects signature tampering
receipt verification detects mismatch (action_id, status, charge, settlement arithmetic)
receipt verification returns ErrInvalidState for a non-remote-proxy transaction
network identity derives deterministically from the platform Ed25519 signing key
transport and Juice payload signature domains are disjoint (a signature valid in one is rejected in the other)
fed transport is behind an interface with a fake implementation; all kernel federation logic is testable without libp2p
friend by key over the fake transport creates the zero-balance proxy pair (auto-accept and manual)
outbound call to an unresolvable peer key does not settle; allocation stays locked, process stays open; retry resumes on reconnect
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
an account with no password cannot obtain a token; an account authenticates by federation signature only if it has a public_key
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

— Delegated OAuth —
a user runs an oauth_delegated action and is rejected pre-lock with the structured grant_required
  outcome naming the action; the client drives consent from it (grants start/complete) against a fake
  provider; re-runs successfully with the provider seeing the bearer; sees the grant in `user me`;
  revokes it; the next run rejects again

— Ratings and reconciliation —
caller executes a paid action multiple times; the action owner lists transactions for their action
  and the sum of transaction net amounts equals the total credits received by the owner
caller rates a transaction with a note; the note and rating value appear in transaction detail and
  list responses for all parties; an unrated transaction returns null for the rating field

— Federation (real transport over loopback; one kernel is the bootstrap+relay) —
two kernels start on 127.0.0.1; the first serves as bootstrap+relay, the second dials it, and they
  friend each other by key alone (no URL); keys resolve through the DHT — no dialable address is configured
two kernels friend each other (auto-accept); operator A deposits B's proxy (and vice versa);
  B imports A's action; B's user runs it; charge lands in A's proxy balance on B, duty to B's @sys,
  difference refunded; both sides' tx verify passes all checks
call settles over a forced-relay path: the two kernels are denied a direct dial, the call and its
  signed receipt travel through the relay, and settlement is byte-identical to the direct case
B imports a large catalog from A: manifest sync is chunked per action and completes over a
  bandwidth-capped stream
kernel gossips a transacted peer; third kernel reads the gossip over the transport, sees earned stats,
  friends the subject directly by key, imports, runs — its own Stats start at defaults and accumulate
A unfriends B: B's proxies deactivate, B's next inbound call gets a signed rejection receipt,
  steps addressed to B cancelled with refunds, balance intact; A re-friends and traffic resumes
inbound call from an underfunded friend yields a signed rejection receipt the caller settles on
caller runs a NAT-bound peer's action, the peer goes offline mid-call; the caller's allocation stays
  locked and the process stays open until the peer returns and a signed receipt settles it (no timeout settle)

— Federation (real-network release gate; excluded from `go test ./...`) —
from a machine behind a real NAT, friend a remote peer by key, call it both directions with the path
  hole-punched (asserted via `admin inspect`), then force a relay fallback and a restart-mid-call recovery
```

## 16. Design rationale

Stable kernel interfaces and explicit transitions prevent correctness from depending on transports or adapters. Small function-named packages and per-file tests reduce coupling, keep replaceable implementations visible, and expose coverage gaps. Execution and supervision are separated because execution may err, while supervision supplies correction signals execution must not manipulate.

Credit locking of the full subtree price before execution prevents unfunded work anywhere in the tree. Failure refunds of the remaining allocation keep accounting conservative, observable, and testable. Input-before-lock and output-before-settlement prevent charging invalid requests or paying malformed replies. Mediated WASM authority permits composition without credential leakage or authorization bypass.

Subtree pricing makes a price a price: the caller pays one advertised number, composition risk lives with the provider who composed, and the fee taxes each layer's margin — value added — rather than gross flows. `run` removes process bookkeeping from the user: funding is exact, closure is automatic, and a process is simply the lifetime of a computation and its continuations. Steps are funded continuations: money reserved at suspension is what makes asynchronous composition safe, restartable, and honest about who pays.

Federation incentives are aligned in both directions. A kernel federates because its users gain a larger action space and its providers gain outside demand; its operator's reward is the import duty. The domestic fee (default 20%) deliberately exceeds the import duty (default 5%), so a kernel always earns more on local supply than on imports — federation complements local providers rather than undercutting them. Discipline is self-enforcing without enforcement machinery: a kernel whose counterparty account runs dry stops serving it manifests, because executing unpaid work loses money twice — once in service, once in the failure stats that sink its rank abroad; funding restores exposure. And because gossip carries only trade-backed opinions, reliable behavior compounds into discoverability: reputation is the long-run asset a kernel earns by settling honestly.

A single peer-to-peer carrier for federation is the coherent expression of a system whose identity was always a key and never an address. One transport means one code path, one failure mode, and one thing to verify against §13 — and it lets a kernel behind a home router federate identically to one on a public host, which is the install-and-run experience the design promises. HTTP federation was the lone layer dragging advertised addresses, `.well-known` documents, and peer-URL SSRF rules back into a key-native system; removing it deletes a whole class of configuration rather than maintaining a parallel path. The honest cost, stated plainly: a small class of public helper nodes (bootstrap, rendezvous, relay) is load-bearing infrastructure for all federation — they pass encrypted bytes and hold no Juice data, but someone must run them, and they are unpaid and out-of-protocol for now. Settlement never knew the carrier, so moving it onto the transport changes how bytes arrive and nothing about who pays or what settles.

Replaceable lookup ranking permits research changes without changing kernel semantics. Fixed stats plus optional tags preserve deterministic baseline metrics while isolating experiments. Latency is derived from transaction timestamps rather than cached on traces, so buyer-experienced wall time is always current — a subtree query reflects late descendants without a retroactive write — at the cost of that query at read time.

Fixed `@sys` and signing keys give stable system action names and verifiable receipts/manifests. Structured logs make production operation and research reproduction reconstructable.

OpenAPI import as supervision keeps registration low-friction while preserving uniform execution. Federation imports signed action contracts without leaking implementation. Contract-change deactivation prevents silent interface drift for callers and LLMs. Unimport deactivates rather than deletes so history remains auditable. Friendship is deliberately worthless (auto-accepted, zero balance) so that trust lives in deposits and reputational gossip carries only earned, trade-backed opinions; a Bayesian shrinkage of ranking priors toward trust-weighted introducer means is the natural baseline formula, but the formula is the ranking layer's experiment, not kernel semantics.