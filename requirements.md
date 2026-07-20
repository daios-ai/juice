# Juice Kernel Requirements

Version: 0.9.0
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

These meanings are fixed: `owner_user_id` = process owner (not the caller); `caller_user_id` = immediate requester (not necessarily the process owner — for a root call they coincide, the `run` requester becoming the process owner); `target_user_id` = called action owner.

| Case                       | `P`                       | `C`                     | `A`                                 |
| -------------------------- | ------------------------- | ----------------------- | ----------------------------------- |
| Root call (`run`)          | process owner             | authenticated requester (= P) | called action owner           |
| WASM subcall               | parent process owner      | parent action owner     | subcalled action owner              |
| Step completion            | step's process owner      | required_caller_user_id | step's next action owner            |
| Remote proxy call          | local process owner       | local call caller       | local remote-peer user owning proxy |

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
| `User`              | `id`, `handle`, `description`, `available`, `locked`, `suspended_at`, `public_key`, `recovery_public_key`, `peer_last_seen`, `peer_credit`, `created_at`, `updated_at`                                                                                                                                         | `handle` unique, no `/`. `description` is a free-text "about" the account may set (§13); `@sys`'s description is the kernel's own advertised "about". A suspended account cannot act: a suspended user is rejected at every authenticated request with `ErrUnauthenticated`, and a suspended peer's inbound federation calls are refused (§13) — `suspended_at` is the single moderation axis for humans and peers alike. `public_key`, when set, is a unique base64url Ed25519 32-byte key. `recovery_public_key`, when set, is the account's own base64url Ed25519 recovery key (§12): a *recovery* credential, not an authentication credential and not a federation identity — it is distinct from `public_key` and never makes the account a peer. No user "kind": an account authenticates by the credentials it holds — a password (session: log in, hold tokens) and/or a `public_key` (federation signature, per request, §13). `user create` makes a password account and enrolls a recovery key from a client-held seed phrase (§12), a subscriber's first call or a deposit a key account (§13); neither authentication credential ⇒ cannot authenticate. An account may also hold `Grant` rows (§8): consent records delegating its upstream OAuth identity to specific actions, not auth credentials. Location is never stored — the transport resolves a key to a live path at call time (§13). `peer_last_seen`/`peer_credit` are null except on peer rows: the peer-sync display cache (§13 peer sync), never execution semantics.                                                                                                                                                                                                                                          |
| `Action`            | `id`, `owner_user_id`, `name`, `kind`, `active`, `visibility`, `price`, `description`, `input_schema`, `output_schema`, `source`, `auth_json`, `artifact_hash`, `remote_action_id`, `created_at`, `updated_at`                                                                       | `owner_user_id` is the action owner. `kind ∈ {http, wasm, native, remote_proxy}`. `visibility ∈ {private, local, public}` is the caller-scoped callability scope (§4): `private` = owner only; `local` = any local (non-peer) caller, never served to peers; `public` = anyone, and the only value served in manifests/gossip (§13). Default `private`; created private and widened only by update. `(owner_user_id,name)` unique. `/` is allowed in `name`; handles cannot contain `/`, so `@owner/name` is unambiguous. Inactive actions are not callable. Listing visibility per §14 (`GET /v1/actions`): owners may list all their own regardless of `active`/`visibility` via `?owner=` self-match. Authorized users may inspect script source. `artifact_hash` content-addresses compiled artifacts. `auth_json` is the write-only upstream credential config, encrypted at rest, never returned by any read path (§8); reads expose only its non-secret summary — `auth_scheme` and a `requires_grant` flag. For `remote_proxy`, `remote_action_id` is the action ID on the remote kernel and `artifact_hash` the signed manifest hash; the peer is the proxy owner's `public_key`, transport-resolved to a live path (§13), so no URL is stored in `source`. Active actions require non-empty natural-language `description`, valid schemas, and field descriptions sufficient for lookup and LLM function calling. |
| `Process`           | `id`, `owner_user_id`, `available`, `locked`, `status`, `created_at`, `ended_at`                                                                                                                                                                                                 | `owner_user_id` is the process owner and payer. `status ∈ {open,closed}`. Created by `run`, funded with exactly the root action's price parked from the owner's `available` into `locked` (§6); the process holds it as `available`. Bijective with its root trace; it is a longer-lived wallet only because traces settle eagerly (§6): it absorbs refunds destined for already-settled traces and holds parked steps. Enforcement is per call, on the call's trace (§6); `available + locked` is the total held across its calls' wallets and parked steps. Closes automatically when the root call has returned and no Steps are outstanding, returning remaining funds to the owner and releasing the owner's lock. Closed processes cannot call.                                                                                                                                                                              |
| `Trace`             | `id`, `process_id`, `parent_trace_id`, `action_owner_id`, `available`, `locked`, `idempotency_key`, `dispatch_json`, `created_at`                                                                                                                                  | `action_owner_id` is the owner of the action executing in the trace, used for trace-scoped process authority. `process_id` is denormalized (derivable by walking `parent_trace_id` to the root). Root traces have null parent. Every `Call()` creates exactly one child trace. A trace is the call's wallet: `available` starts as the action's price at entry (the call's remaining allocation); `locked` is what the call has committed to direct subcalls and steps. Calling price `q` requires `available ≥ q` and moves `q` from `available` into `locked`, becoming the callee's `available` (§6). Settlement pays out the trace's remaining `available` (§6). Own latency is `transaction.ended_at − transaction.started_at`; no cached latency field (§11). `idempotency_key`/`dispatch_json` are null except on a remote-proxy trace, where the outbound key and request payload are recorded atomically with dispatch; `idempotency_record_id` is the *inbound* cross-kernel record the trace serves when the call is answering a peer (§13) — set for every action kind, so whichever settlement finally resolves the trace (commit, dispatch retry, max-age bound, forced closure, or crash recovery) completes that record; while set and unsettled, the call is awaiting its receipt and restart resumes its retry (§5, §13). |
| `Transaction`       | `id`, `process_id`, `trace_id`, `parent_trace_id`, `owner_user_id`, `caller_user_id`, `target_user_id`, `action_id`, `action_name`, `args_json`, `reply_json`, `status`, `gross`, `net`, `fee`, `reason`, `remote_receipt_hash`, `remote_receipt_json`, `started_at`, `ended_at` | `status ∈ {success,failure}`. Every attempted call creates one immutable transaction. Fields obey the role law. `action_name` is captured at creation so history remains self-contained after action deletion. Local calls have null remote receipt fields. Remote-proxy commits atomically store full remote receipt JSON and `SHA-256(remote_receipt_json)`.                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `Stats`             | `uses`, `successes`, `failures`, `rating_count`, `latency_estimate`, `rating_estimate`, `last_used_at`                                                                                                                                                                           | Missing stats have defined defaults. `uses = successes + failures`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `StatTag`           | `action_id`, `key`, `value`, `source`, `updated_at`                                                                                                                                                                                                                              | Optional lookup-experiment data, namespaced by source, never execution semantics.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `Step`              | `id`, `parent_trace_id`, `required_caller_user_id`, `action_id`, `price`, `partial_args`, `status`, `tx_id`, `created_at`                                                       | `status ∈ {waiting, running, done, cancelled}`. `parent_trace_id` is the creating/funding trace, derives the process (`Trace(parent_trace_id).process_id`), and is inherited by the completion trace. `required_caller_user_id` is mandatory; open completion is unsupported. `partial_args` is pre-bound input merged with caller input at completion (`input` overwrites `partial_args` on key collision). The completer's allowed input is derived `action.input_schema \ keys(partial_args)`, not stored (§10) — safe because a schema change deactivates the action and completing against a changed/deactivated action resets the step to `waiting` (§10). `tx_id` is recorded atomically at `done`. `price` is `action.price` snapshotted at creation: the amount parked in the process's `locked`, the completion call's allocation and `gross`, spent on completion and refunded on cancellation. `EndProcess` atomically cancels all `waiting` steps of the process in the same transaction as closure; `cancelled` is terminal with no `tx_id`. An outstanding (`waiting`/`running`) step keeps its process open, allocation parked (§10). |
| `LedgerEntry`       | `id`, `operator_user_id`, `from_user_id`, `to_user_id`, `amount`, `reason`, `external_key`, `created_at`                                                                                                                                                                                                     | Immutable audit record of one direct balance movement, source and destination each nullable: a deposit credits (`from_user_id` null, `to_user_id` set), a withdrawal debits (`from_user_id` set, `to_user_id` null), a user transfer moves between two local users (both set, §12). At least one of `from_user_id`/`to_user_id` is non-null. `operator_user_id` is the authorizer — `@sys` for a deposit/withdrawal, the sender for a transfer. `amount` is positive; the debit requires the `from` user's `available ≥ amount`. `external_key` is an optional opaque idempotency token; when present it is globally unique, and a create with an existing `external_key` returns the existing record without reapplying the balance change. Juice never interprets it, keeping the kernel payment-rail agnostic.                                                                                                                                                                                                                                                                                                                                                              |
| `Grant`             | `id`, `grantor_user_id`, `action_id`, `connection_id`, `created_at` | A user's per-action delegated consent (§8): a pointer binding one action to the upstream account (`connection_id`, a `Connection`) whose credential it may wield. Holds no token. Unique per `(grantor_user_id, action_id)`; re-consent overwrites in place. Deleted on revoke, on `Connection` deletion (cascade), on provider `invalid_grant` (which deletes the `Connection` and cascades), and when a deactivating update / auth replacement / delete invalidates the action (§8) — invalidation deletes grants only, never the `Connection` (the upstream account outlives any one action's consent). A consent record, not an authentication credential. |
| `Connection`        | `id`, `user_id`, `provider_key`, `sealed_secret`, `scopes_json`, `created_at`, `updated_at` | A user's upstream account credential, stored once and shared by every `Grant` pointing at it (§8). Unique per `(user_id, provider_key)`. `provider_key` is derived from verified facts, never names: `bearer:<host>` from a `delegated_bearer` action's pinned `source` base-URL host; `oauth:<token_url>|<client_id>|<source-domain>` from an `oauth_delegated` action's auth config, `source-domain` being the registrable domain (eTLD+1) of the action's `source` host — binding the token to its resource server (§8 confused-deputy defense). `sealed_secret` (the OAuth refresh token or static bearer/API-key token) is AES-256-GCM encrypted with AAD `user_id|connection_id`, write-only, never returned by any read path. `scopes_json` is the requested-scope union consented so far (providers need not echo granted scopes, so requested scope is authoritative for coverage). Provider refresh-token rotation updates this one row, leaving dependent grants unaffected; provider `invalid_grant` deletes it and cascades its grants. A zero-grant `Connection` is kept (re-consent reuses it), surfaced `unused` in read paths (§14), never auto-expired. |
| `Receipt`           | `id`, `issuer_user_id`, `tx_id`, `trace_id`, `action_id`, `caller_user_id`, `process_id`, `args_hash`, `reply_hash`, `status`, `gross`, `net`, `fee`, `charge`, `reason`, `started_at`, `created_at`, `signature`                                                                | Immutable signed record for exactly one committed call. `caller_user_id` is the call caller. `started_at` is call start; `created_at` is settlement. `charge` is the amount actually drawn from the caller's funds: `= gross` on success, `≤ gross` on failure (settled descendants stay paid, §6), `0` on rejection.                                                                                                                                                                                                                                                                  |
| `Rating`            | `id`, `rated_tx_id`, `rated_receipt_id`, `rater_user_id`, `rating`, `note`, `created_at`, `signature`                                                                                                                                                                            | Immutable signed feedback record. `rating ∈ {0,1}`. At most one rating exists per transaction. `rated_receipt_id` may be null only for pre-receipt transactions. `note` is optional, nullable, human-readable, and included in the single Ed25519 rating signature payload.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `IdempotencyRecord` | `id`, `idempotency_key`, `counterparty_user_id`, `receipt_id`, `status`, `result_json`, `created_at`, `expires_at`                                                                                                                                                               | Cross-kernel only. `status ∈ {pending,complete}`. Insert pending before execution; complete atomically with transaction and receipt. Completion stores `result_json`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `DiscoveredKernel`  | `public_key`, `handle`, `introduced_by`, `stats_json`, `first_seen`, `updated_at`                                                                                                                                                                                    | One row per (kernel, introducer); accumulated from gossip (§13). Information only — never execution semantics, callability, pricing, or settlement. Location is not stored; the transport resolves a key when needed (§13).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

OpenAPI registration and federation create or update ordinary `Action` rows. They create no durable object parallel to `Action`.

## 4. Authorization, call validity, and traces

Every action has a `visibility ∈ {private, local, public}` (§3). `private` is callable only by its owner; `local` by any local (non-peer) caller of this kernel but never by a peer proxy user; `public` by anyone including peers, and only `public` actions are served in manifests and gossip (§13).

```text
CanCall(C, a) := active(a) ∧ ¬suspended(a.owner_user_id) ∧
                 (public(a) ∨ (local(a) ∧ ¬peer(C)) ∨ C = a.owner_user_id)
```

`CanCall` is scoped to the immediate call caller `C`, not the process owner `P` — visibility is a property of the code that directly invokes the action, exactly as lexical visibility governs a function call in a programming language. This makes a provider's `public` action able to subcall the provider's own `private` helpers in anyone's process, while foreign code a process owner funds cannot reach that owner's `private` actions (no confused deputy). For a root call `C = P`, so user-facing behavior is unchanged. `peer(C)` holds when the caller is a peer proxy user (a set `public_key`, §13); an inbound federation call is a root call by that peer, so it reaches `public` actions only — never `local` ones, which is what keeps an imported proxy (held `local`, §8) unreachable across a second hop. An action whose owner is suspended fails `CanCall` regardless of visibility: a suspended owner's actions are not callable and are excluded from action listings (§14); unsuspending restores them, since suspension preserves data (§12). Encapsulation controls the direct dependency surface, not reachability of effects: a local caller may always wrap a `local` (or `private`) action inside a `public` one and export the result, taking on the wrapper's margin and rating risk — the same freedom a programming language grants a public function over a private one.

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
6. CanCall(C, action)
7. args satisfy action.input_schema
8. funds: the passed trace's `available` >= action.price (§6) — for a root call this is the root trace `run` funded from the process; a step completion is funded by its parked price instead (§10)
```

Precondition 4 controls spending authority over the process (the process owner `P`). Precondition 6 controls action access by the immediate caller `C`. These are distinct questions: `P` decides whose funds may be spent, `C` decides whose code may reach the action.

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
  (a settled failure always reports its committed transaction to the caller, even when the
   post-settlement bookkeeping then fails — the caller was charged and must be able to find it)
  CommitFailedCall    failure transaction + receipt + subtree refund/cancellation + stats (+ step/idempotency)
  CommitRemoteSettlement  remote-proxy settlement on a signed receipt (charge/duty/refund) + transaction + receipt (+ step/idempotency)
  EndProcess          cancel waiting steps + return funds + close process
  CreateStep          step record + park step.price from the creating trace
  CreateLedgerEntry   direct balance move (deposit/withdrawal/transfer): debit from + credit to + ledger record
  CreateRatingAndUpdateStats  rating record + rating stats
Reads / supervision (no monetary mutation):
  CreateUser ReadUser ReadUserByHandle ReadUserByPublicKey ListUsers SuspendUser UnsuspendUser UpdateUser RenameUser
  CreateAction ReadAction ReadActionByOwnerName UpdateAction UpdateActionAndResetStats DeleteAction ListAllActions
  ReadProcess ListProcesses ListAllProcesses
  ReadTrace ReadRootTrace ListTraces
  ReadTransaction ListTransactions ListAllTransactions
  ReadStats UpsertStats
  ReadStep ListSteps ResetStepAndRepark ResetRunningSteps
  ReadReceipt ReadReceiptByTxID
  ReadRatingByTxID ListRatings
  ListLedgerByUser
  InsertPendingIdempotencyRecord ReadIdempotencyRecord CompleteIdempotencyRecordIfPending DeleteIdempotencyRecord
  GetConfig SetConfig InitFirstBoot
  CreateOrUpdateDiscoveredKernel ListDiscoveredKernels DeactivateActionsOwnedBy CreateProxyUser
  CreateOrReplaceGrant ReadGrant ListGrantsByUser DeleteGrant DeleteGrantsForAction
  CreateOrUpdateConnection ReadConnection ReadConnectionByUserProvider ListConnectionsByUser UpdateConnectionSecret DeleteConnectionCascade
  ListLegacyTokenGrants LinkGrantConnection  (backfill-only; retired with the legacy column)
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
| Deposit         | user credit, ledger entry (from null → to user)                                                  |
| Withdrawal      | user debit, ledger entry (from user → to null)                                                   |
| Transfer        | sender debit + recipient credit, ledger entry (from sender → to recipient) — one commit (§12)    |
| Rating          | rating record                                                                                    |
| User update     | user description and/or password hash                                                            |
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

`CreateAction`: inactive by default. Validate action owner, name, kind, non-negative price. `description`, schemas, and source are required at activation. WASM creation validates or compiles only when an executor is configured. HTTP creation validates endpoint configuration without calling it unless requested; every `kind=http` action — manual or imported — stores one structured source (verb, base URL, path, parameter bindings), with an optional verb (default POST) and explicit or implicit field routing (§8). Reject non-HTTP(S), RFC 1918 private, link-local `169.254.x.x`, unspecified, and CGNAT source URLs at creation and activation. Loopback (`127.0.0.1`/`::1`/`localhost`) is permitted by default — a service on the same host (a local model, the §9 co-located callback); `allow_local_sources` additionally permits the private/LAN/reserved classes. A loopback source that redirects to a private or link-local address is still blocked (only loopback itself is exempt). Normal `CreateAction` rejects `kind=native`.

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

Authentication to upstream APIs is per-action: the importer stores an auth config in a dedicated write-only `auth_json` column — `{scheme, config, secrets}` — stored AES-256-GCM encrypted at rest; applied by a replaceable authenticator adapter at HTTP dispatch (§9). Secrets are write-only: never returned by any read path, never visible to scripts, never present in args, replies, logs, receipts, hashes, or manifests, and excluded from contract comparison. Implemented schemes: `header` (static header), `query` (query parameter), `bearer` (Authorization: Bearer), `basic` (HTTP Basic Auth), `oauth_client_credentials` (client id/secret exchanged at the token endpoint for a bearer), `oauth_jwt_bearer` (RFC 7523: a stored RSA private key signs a JWT assertion exchanged at the token endpoint), `oauth_delegated` (per-caller authorization-code+PKCE or device flow), and `delegated_bearer` (per-caller static token: a personal access token / per-user API key the caller supplies once, applied into a configured header — no token exchange). The scheme and its required config/secret keys are validated at action create/update, and dispatch fails closed on an unknown scheme — a request is never sent unauthenticated because its scheme was unrecognized. HMAC request signing is unsupported; operations requiring it stay never-active.

Owner-held schemes live entirely in `auth_json`. The two **delegated** schemes instead keep the per-caller credential on a `Connection` row (§3) — one per `(user, upstream account)` — while the `Grant` binds only *consent* `(grantor, action, connection_id)` and holds no secret; no per-caller secret lives in `auth_json`. `oauth_delegated` stores provider config only — `auth_url`, `token_url`, optional `device_auth_url`, `client_id`, `scopes`, optional `client_secret` — and the caller's `Connection` holds an OAuth refresh token from the browser consent flow (§12). `delegated_bearer` stores no provider config and no owner-side secret — only an optional `{header, template}` naming where the token goes (default `Authorization: Bearer {token}`, so GitHub `token`, GitLab `Private-Token`, and `X-Api-Key` styles are expressible) — and the caller's `Connection` holds a static token (personal access token / per-user API key) supplied once via `POST /v1/grants` (§12); no token exchange, refresh, or `invalid_grant` handling, so a rejected token surfaces as an ordinary execution failure. **Binding rule (confused-deputy defense):** at dispatch a delegated token is applied iff `grant.grantor_user_id = process.owner_user_id` (the paying human) and `grant.action_id` = the executing action's id (the exact code trusted); the token is then fetched through `grant.connection_id`, so moving the credential onto a shared `Connection` leaves the check unchanged. Delegation therefore never transfers to another action's subcall, never crosses federation (a `remote_proxy` executes the proxy, not the http action, so the token never leaves the kernel), and WASM scripts never see tokens. Consent is a lazy precondition: a call to a delegated action whose process owner holds no matching grant is rejected with the typed `ErrGrantRequired` (§12), carrying the action reference as structured metadata so clients detect it by code, before any funds are locked and before any transaction exists — so an unconsented call never charges the caller nor dents the provider's failure stats (§9). Access tokens are cached in memory only (keyed by connection, so actions sharing an account share the cache), refreshed from the connection's stored refresh token, dropped on an upstream 401; refresh-token rotation persists the new token onto the one `Connection`, leaving sharing grants unaffected; `invalid_grant` deletes the `Connection` and cascades its grants so the next call re-consents. A deactivating update (§7), auth replacement, or deletion revokes the action's grants — consent binds to the contract, not the enabled bit — but never deletes the `Connection`, which outlives any one action. Token-endpoint fetches obey the same SSRF discipline as action sources (§7).

**Connections, selectors, and reconcile.** `provider_key` is derived from the action's pinned, SSRF-validated facts (§3, §7), never from action names or descriptions, so no action can name its way into another's credential. The `oauth_delegated` key includes the resource-server domain because an OAuth `client_id` is public: without it a hostile action reusing a legitimate `token_url|client_id` under an attacker-controlled `source` could ride a victim's existing connection and have the minted access token delivered to the attacker (confused deputy); a different resource-server domain is therefore a distinct connection requiring its own consent, and the consent plan surfaces each group's destination host(s) (§14) so the recipient of a delegated credential is never hidden. Consent is planned per upstream account. A **selector** — `@owner` or `@owner/path`, matched by path segment (`@tom/brief` matches an action named `brief` or `brief/…`, never `briefing`; a full `@owner/name` is the degenerate one-action selector; a trailing `/*` is stripped) — expands to the delegated actions the caller may call (`CanCall`, §4), grouped by `provider_key`. One `delegated_bearer` token paste or one browser consent requesting the **union** of a group's `scopes` covers the whole group at once, minting one `Grant` per action against the single `Connection`; connecting an action whose provider `Connection` already covers the scopes grants instantly, no browser round-trip. Re-consent for a wider group stores the union of the stored and newly requested scopes, so earlier grants never lose coverage. Every connect is thus a **reconcile**: it compares the selection to existing coverage and acts only on the delta (the client must display that delta — the actions to be connected — as the consent act, §14). The first-run migration re-homes each legacy per-grant token onto its derived `Connection` (on a `(user, provider_key)` collision the latest `created_at` wins the secret) and reseals it under the new AAD; an unrecoverable legacy token deletes its grant.

This maps OAuth's own trust model onto Juice's principals: the grant is consent to an identified client — the granted action — while services composed above consume its output unseen. A grant, like every resource of the process owner, is exercisable by the call trees the grantor funds, and each use is price-bounded, ledger-attributed (the role law records whose code requested it), and ratings-disciplined. The confinement line is *data at the run boundary, tokens absolutely*: a granted action's return flows to its caller like any result; the token itself never leaves dispatch.

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

`admin subscribe` fetches the remote's active public actions, verifies each signed manifest, and creates or updates local `kind=remote_proxy` actions owned by the local remote-peer user row. Imported actions are immediately enabled and given `visibility = local`: callable by this kernel's own users but never re-served to a further peer, so subscription stays non-transitive at the call layer as well as the manifest layer — a peer that names one of your imported proxies is denied by `CanCall` (a peer caller fails the `local` branch, §4), not merely absent from your manifests. It does not copy implementation.

Manifest required fields:

```text
action_id owner_handle name description input_schema output_schema price kind
artifact_hash stats updated_at signature
```

Only active public remote actions have manifests; an action using a delegated auth scheme (`oauth_delegated` or `delegated_bearer`, §8) is never served as a manifest and never gossiped, because a remote peer's single proxy user can never complete a browser consent nor hold a per-caller token. A kernel serves manifests and gossips (as its own exposed actions) only actions it owns — `kind ∈ {http, wasm, native}`; an imported `kind=remote_proxy` action is never re-served, so subscription stays non-transitive: reaching a peer's imported action requires subscribing to its true owner directly. Manifest descriptions and schemas are the canonical interface used by importing kernels for lookup and LLM function calling. `signature` is the remote platform Ed25519 signature over canonical JSON excluding `signature`, verified against the remote peer's `public_key`. Match key:

```text
proxy.owner_user_id + proxy.remote_action_id
```

Remote contract fields:

```text
action_id artifact_hash description input_schema kind name output_schema owner_handle price
```

Manifest stats and `updated_at` do not affect contract comparison; manifest stats never overwrite local `Stats` and are not stored as `StatTag`. Invalid signatures skip that action. A second `admin subscribe` re-syncs: new actions are imported, changed-contract actions are updated (re-enabled), and actions no longer active/public on the remote are deactivated and their stats reset. `admin unsubscribe` deactivates all proxies from that peer and preserves all history.

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

**Out-of-kernel composition.** Composition — subcalls and steps, funded by the caller's subtree price (§6), attributed by the role law (§1/§4) — is available in-process to WASM and native actions through the host surface above. A `kind=http` action executes outside the kernel and would otherwise be a leaf: callable but unable to subcall or suspend into a step, because composition needs a live trace and an HTTP endpoint has none. To let a service reachable *as* a `kind=http` action compose with the same guarantees, the kernel hands every dispatched HTTP call a **trace-scoped capability**: the call's `trace_id` signed with the platform key over a disjoint JCS object `{"cap": trace_id}` (§12). It is delivered as an HTTP header alongside a callback base-URL header, and never enters the payload, `args_json`, `reply_json`, receipts, receipt hashes, or logs (R9). Presented back as a bearer header on a callback, it authorizes composition **as the executing action's owner within its trace** — the WASM subcall law (§6): the callback runs with `caller_user_id = action.owner_user_id`, funded from the trace, `CanCall` evaluated against that caller — the executing action's owner (§4). It grants nothing else — no user wallet, no other trace or process, no supervision — so its blast radius equals what the owner's WASM code could do in the same trace.

Callbacks reuse the execution API, mirroring the host surface: `juice.call` ≡ `POST /v1/call` (a subcall on the trace — the HTTP twin of `Call`, capability-only, with no `run`/wallet path); `juice.step_create` ≡ `POST /v1/steps` (the capability *is* the trace, so no `trace_id` is sent); `juice.step_complete` ≡ `POST /v1/steps/{id}/complete` (completes iff `required_caller = action owner`, §10). Each reuses `BeginSubcall`/`CreateStep` — no new money path; a callback exceeding the trace's remaining `available` fails `ErrInsufficientFunds`, so the advertised `price` bounds the whole subtree. The capability is valid only while its trace is **unsettled** (no `transactions` row, the settled-once invariant §11); settlement or process closure invalidates it by committing that row, and a callback presented afterward is rejected. A spend under a capability and its trace's settlement are mutually exclusive, so concurrent callbacks neither exceed the subtree bound nor race the payout. An in-flight capability trace is swept to `interrupted` by ordinary recovery (§5); a dispatch retry never mints a fresh trace or capability. The callback base URL is the kernel's own reachable HTTP address (`http_callback_url`, §14, else derived from the listen address); a loopback callback (the co-located case, permitted by default, §7) is plain HTTP, a public one requires TLS, and the header is stripped across a host-changing redirect. Issuance is ambient — a leaf endpoint simply ignores the headers. The capability is **local to the executing kernel and never crosses federation**: a composing HTTP action is served and gossiped as an ordinary `kind=http` action (§13), its composition invisible to importers and bounded by the manifest price it advertises.

`llm` exposes replaceable `Embed(ctx,text)->vector` and `Chat(ctx,messages)->message`; concrete adapters call Ollama, but `kernel` must not import them. Defaults (configurable in `config.json` under `native.llm`, §14): URL `http://localhost:11434`, chat `gemma4:26b`, embedding `nomic-embed-text`. Tests use fakes.

Upstream authentication is a replaceable adapter: `Authenticator.Apply(request, auth) -> request` transforms an outbound HTTP request using the action's stored auth config (§8); schemes are implementations behind this interface, including the OAuth token exchange and in-memory token cache (§8), and `kernel` must not import them. Tests use fakes.

Native actions are standard actions shipped alongside the kernel as a platform stdlib. They have no special kernel privileges — any provider could have supplied equivalent actions as HTTP or WASM actions. They are registered at bootstrap under `@sys`, interact with the platform only through injected dependencies and the same `Call()` / `CreateStep()` / `CompleteStep()` entry points available to all actions, and never extend the kernel's internal interfaces on their own behalf.

| Action          | Rules                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `@sys/lookup`   | Public; action owner `@sys`; price 0 (configurable, `native.lookup`, §14); callable only through `Call()`. Rank active actions by a tested formula combining lexical (BM25) and semantic (cosine) relevance — fused by reciprocal-rank fusion. **Stats-based quality weighting is UNDER REVISION and temporarily removed** (the multiplier could bury an exact match beneath weakly-relevant ones); ranking is by fused relevance alone until the redesign lands (see `ranking.md`). The embedder is optional: with none configured, ranking degrades to the lexical leg alone, so lookup still works on a kernel with no LLM. Replaceable ranking storage; brute-force cosine acceptable. Input: required `query`, optional `limit=10`. Output: `results[]` with `action_id`, `action` (`@owner/name`), `description`, `score`, `input_schema`, `output_schema`. Direct lookup only for diagnostics, not user-facing APIs or WASM hosts.                                                                                                                                                                                                                                                                                                       |
| `@sys/llm/chat` | Public; action owner `@sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Input: `messages[]` of `{role,content}` plus optional `system`. Output: `message{role,content}`. `ErrInvalidState` if chat unconfigured.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `@sys/llm/embed` | Public; action owner `@sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Input: required `text` (string). Output: `embedding` (array of numbers). `ErrInvalidInput` if `text` is empty. `ErrInvalidState` if embedder unconfigured. |
| `@sys/llm/json` | Public; action owner `@sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Input: `messages[]` of `{role,content}`, optional `system`, required `output_schema`. Output: `value` (JSON value). Validates model output locally against `output_schema`. `ErrSchemaViolation` for unsupported schema. `ErrInvalidState` if structured output unavailable. `ErrExecutionFailed` if no valid JSON produced. |
| `@sys/llm/decide` | Public; action owner `@sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Given a conversation and a set of Juice actions, asks the LLM to select one and propose args — does not execute the call. Input: `messages[]` of `{role, content?, tool?}` where `role` is one of `system`, `user`, `assistant`, `tool` and `tool` is an optional object `{action?, args?, result?}`; required `actions[]` (list of `@owner/name`). Kernel fetches each action's canonical description, `input_schema`, and price from the DB. Output: `action` (`@owner/name`), `args` (validated against that action's `input_schema`), optional `message`. `ErrNotFound` if any action reference is unknown. `ErrInvalidState` if LLM tool calling unavailable. `ErrExecutionFailed` if no valid selection produced. |
| `@sys/time`     | Public; action owner `@sys`; price 0 (configurable, `native.time`, §14); callable through `Call()`. No input required. Output: `unix` (integer seconds since UTC epoch), `iso` (RFC 3339 string). |
| `@sys/sink`     | Public; action owner `@sys`; price 0 (configurable, `native.sink`, §14); callable through `Call()`. Accepts any input, returns `{}`. Universal no-op sink for steps that require an onward action but no further computation. |
| `@sys/message`  | Public; action owner `@sys`; price 0 (configurable, `native.message`, §14); callable through `Call()`. Sends a message to another platform user by creating a Step they must acknowledge. Input: required `to` (`@handle` of recipient), required `message`. Output: `step_id`. The Step sets `required_caller_user_id` to the resolved target user and `partial_args` to `{"message":"..."}` so the recipient can read it via `step list`. Uses `@sys/sink` as the step's `action`. `ErrInvalidInput` if `to` cannot be resolved. |
| `@sys/random`   | Public; action owner `@sys`; price 0 (configurable, `native.random`, §14); callable through `Call()`. No input required. Output: `value` (float in `[0, 1)`). Exists to provide randomness to WASM scripts, which have no ambient access to the OS random source. |
| `@sys/web`      | Public; action owner `@sys`; price 0 (configurable, `native.web`, §14); callable through `Call()`. Read-only fetch of a public web page. Input: required `url` (string); a scheme-less `url` defaults to `https` (HTTPS-first, like a browser), and an explicit `http`/`https` scheme is respected and never silently downgraded. Output: `status` (HTTP status integer), `body` (response body string), `content_type` (response `Content-Type` string), `final_url` (the URL actually fetched, after scheme defaulting and redirects). GET only; no caller-supplied headers or auth, so nothing sensitive enters args/receipts/logs. A fixed, configurable descriptive `User-Agent` is set by the action itself. Same SSRF discipline as `kind=http` (§7): RFC 1918 private, link-local `169.254.x.x`, and reserved hosts are rejected with `ErrInvalidInput` unless `allow_local_sources` is set; loopback is permitted by default like any other fetch. Non-2xx statuses are returned in `status`, not raised as errors, so crawlers can react to them; 10 MiB response cap. `ErrInvalidInput` for empty `url`; `ErrInvalidState` if the fetcher is unconfigured; `ErrExecutionFailed` on transport failure. The mediated path by which WASM scripts read the network: scripts still receive no ambient sockets — they reach the web only by calling this action through `Call()`, charged and SSRF-restricted to public hosts (§9). |
| `@sys/step/race` | Public; action owner `@sys`; price 0 (configurable, `native.step`, §14); callable through `Call()`. Completes an *onward* step on behalf of whichever contributor arrives first — the "wait for any of N" combinator over §10's one-shot continuations. Input: required `step_id` (the onward step), optional `input` (object) passed to its completion. Output: `fired` (boolean), plus `tx_id`/`trace_id`/`status` when fired and `error` on a failed onward call. A contributor that resumed the step reports `fired:true` with `status = failure` rather than a bare success: the continuation ran, so this contributor did its job, but reporting only `fired:true` would read as success and the workflow would proceed as though the continuation had worked. Stateless: the store's atomic `waiting→running` claim (§5 `BeginStepCall`) *is* the test-and-set, so exactly one contributor can win. A contributor that resumed the step reports `fired:true` even if the onward action then failed — the continuation ran, and its own transaction records the failure, exactly as a settled failed subcall does. Losing to another contributor is `fired:false`, not an error: that is the normal outcome for all but one. Anything else is raised, including a resumption still awaiting a peer's receipt, which must never be mistaken for a lost race. The classification reads `CompleteStep`'s outcome (§10), never the step's status, which is racy and cannot see an in-flight dispatch. **Confinement:** the onward step must have been created by *the same trace that invoked this gate*, else `ErrUnauthorized`. Process scope is not enough: subcalls share a process (§6), so a process-wide rule would let anyone executing there — including the process owner — resume a continuation another provider's action parked, with input of their choosing, spending funds that provider reserved. That is the confused deputy caller-scoped `CanCall` exists to exclude (§4). Binding to the creating trace ties both steps to one workflow, and implies same-process. A consequence: a top-level `run` of a gate can never fire anything, since a root trace has no parent — gates are reachable only from inside the workflow that created the onward step. The onward step names `@sys` as its required caller, and its price is parked once, not once per contributor. |
| `@sys/step/join` | Public; action owner `@sys`; price 0 (configurable, `native.step`, §14); callable through `Call()`. Counts contributions and completes the onward step once `need` of them arrive — the "wait for all of N" combinator, and the one place a barrier's inherently shared state lives. Input: required `step_id`, required `need` (positive integer, fixed by the first contribution; a later contribution naming a different threshold is `ErrInvalidInput`), optional `value` recorded with the contribution. Output: `fired`, `have`, `need`, plus `tx_id`/`trace_id`/`status` when fired and `error` on a failed onward call (same reporting rule as `@sys/step/race`). Fires at `have ≥ need` (not `==`, so a crash between counting and firing is healed by the next contribution) and completes the onward step with `{}` — its arguments were bound in `partial_args` at creation, and an author-chosen schema cannot be assumed to accept a contributions blob. Same creating-trace confinement as `@sys/step/race`. Counter state is native-owned, held in a `step_gates` row keyed by the onward step and reached through an injected dependency, never through the kernel's store interface; the row is deleted **only when this contribution actually resumed the step**, and cascades if the step is deleted. Every other outcome keeps it, because every other outcome may still need the accumulated count: a failed fire so a later contribution can retry, and a not-claimable step because that state also covers this barrier's own in-flight dispatch, which may yet leave the step completable. The cost of keeping it is one bounded-garbage row for an already-resolved barrier; the cost of deleting it wrongly is every contribution lost. With no contributor left after a permanently failing onward action, later contributions error honestly — silence would hide a broken workflow — and the parked continuation is recovered by process closure like any failed one (§6). |
| `@sys/tinygo/compile` | Public; action owner `@sys`; price 5 (configurable, `native.tinygo`, §14); callable through `Call()`. Compiles author-supplied TinyGo to a WASM artifact using the platform TinyGo compiler, prepending the Juice WASM SDK so the author writes only `func Handle(in map[string]any) (map[string]any, error)` (the SDK owns `package`, imports, `alloc`, `run`, `main`). Input: required `source`. Output: `status` (`success`/`failure`), `artifact` (base64 WASM, on success), `artifact_hash` (SHA-256 hex, on success), `diagnostics` (array). Empty `source` gives `ErrInvalidInput`; an unavailable compiler toolchain gives `ErrInvalidState` (platform misconfiguration — the call fails and is not charged). Author compile errors and import/export-validation failures use output failure status, not kernel errors (so the attempt is charged). Registration is separate supervision: pass the returned artifact to `action create --kind wasm --artifact` (§14). |

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

`required_caller_user_id` is mandatory. Open completion is not supported. It may name a peer's proxy user, in which case the step is completed over `/juice/fed/step/1` (§13) — a key account holds no session token, so the federation protocol is its only completion path.

Waiting on several things at once is composed from this one primitive rather than added to it: `@sys/step/race` and `@sys/step/join` (§9) are ordinary actions parked into as contributor steps, each resuming a shared onward step — any-of via the atomic claim, all-of via a counter. The kernel supplies funded, attributed, one-shot resumption; the coordination policy lives in actions above it.

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

`CreateStep(required_caller, trace, action, partial_args) -> step_id`: the creating authority is the trace's action owner (`Trace(trace).action_owner_id`), so in-execution creation is implicit. For external creation (`POST /v1/steps`) the service layer requires that the authenticated, non-suspended user be permitted to use `trace` (precondition 4 of §4) before invoking the primitive. The process is `Trace(trace).process_id` and must be open; `CanCall(required_caller, action)` must hold — the completion call's caller is the required caller (§4), so a step may only be parked for a caller who could actually complete it, and a private action can be parked only for its owner. Requires `Trace(trace).available ≥ action.price`; creation parks that amount from `trace` (§6), so the parked price is always the recorded payer's money. The completer's allowed input is derived (above), not supplied. Returns a `waiting` step.

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

Completion reports its own outcome, so no caller has to infer one by re-reading the step's status — a read that is racy and cannot distinguish a lost claim from this completer's own dispatch still being in flight. A completion that **never took the step** (already claimed or already resolved) is distinguishable as such; one that took it and then failed **after** committing a transaction returns that transaction alongside the error, exactly as `Call` does; one awaiting a peer's receipt is distinguishable again, and leaves the step `running` for retry (§13). Only a completion rejected before anything settled resets the step to `waiting`.

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

**Seed-phrase recovery.** A password stays the daily credential; a lost password is recovered by proving possession of a per-account recovery key, not by email (there is no email-based reset, and no `email` field). At `user create` (and first boot for `@sys`) the client generates a 12-word BIP-39 mnemonic, derives an Ed25519 keypair from it, sends only the public half — stored as `recovery_public_key` (§3) — and shows the mnemonic once; the mnemonic is the master secret and never reaches the server. Recovery is two unauthenticated calls under the auth rate limiter: `POST /v1/auth/recover/start {handle}` issues a single-use, TTL-bound nonce, and `POST /v1/auth/recover/complete {handle, nonce, signature, password}` verifies the client's Ed25519 signature over the disjoint challenge payload `{recovery_challenge: nonce}` against the account's `recovery_public_key`, consumes the nonce (single-use ⇒ no replay), and sets the new password — bypassing the current-password check that a locked-out user cannot satisfy. The recovery key is a recovery credential only: it is distinct from `public_key`, never authenticates a session, and never makes the account a peer (§3, §4).

The same PKCE/device machinery serves upstream OAuth consent for `oauth_delegated` actions (§8): the client hosts the redirect target — a local client (CLI/desktop) catches it on a loopback listener reachable by the operator's own browser even behind NAT; a hosted client uses its own provider-registered callback URL — and drives `POST /v1/grants/start` / `POST /v1/grants/complete` authenticated as the grantor. Consent is planned per upstream account (§8): a `grants/start` names a `(selector, provider)` group, its authorize request carries the union of the group's `scopes`, and `grants/complete` stores one `Connection` and mints one `Grant` per action in the group. The kernel holds only short-lived in-memory PKCE/device state (~10 min TTL) and performs the code→token exchange itself, so no unauthenticated callback route exists on the kernel and the refresh token never passes through the client. The kernel never dials `redirect_uri`; it only embeds it in the authorize URL, and the OAuth provider's registered-redirect allowlist (exact match) is what binds an issued code to a legitimate client, so any http(s) redirect is accepted and the consent-phishing vector is closed at the provider. In-memory consent state is single-use, TTL-bound, and completable only by the grantor who started it.

Errors:

```text
ErrUnauthenticated ErrUnauthorized ErrNotFound ErrInvalidInput ErrInvalidState
ErrInsufficientFunds ErrExecutionFailed ErrSchemaViolation ErrTimeout ErrInternal
ErrGrantRequired ErrPeerUnreachable ErrPeerUnfunded
```

`ErrGrantRequired` is a precondition failure, the twin of `ErrInsufficientFunds` — the process owner must delegate an upstream OAuth grant before the action can run (§8). It carries the action reference as structured metadata so clients act on a code, not a message.

`ErrPeerUnreachable` and `ErrPeerUnfunded` attribute a remote-proxy failure to the federation relationship, not to the caller. `ErrPeerUnreachable` means the first dispatch provably never reached the peer (§13, never-dispatched settlement); the call is settled locally as an ordinary failure with a full refund. `ErrPeerUnfunded` means the remote peer signed a zero-charge rejection because *this kernel's* prepaid credit there is exhausted — an operator condition remedied by an out-of-band payment and `admin deposit` on the peer, never the caller's own balance. Both carry the peer handle as structured metadata (`Meta["peer"]`), like `ErrGrantRequired` carries its action, so clients act on a code and fields, not on message text.

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

A chosen password must be at least 8 characters, enforced server-side at user creation, first boot, and password update (no composition rules); a shorter one returns `ErrInvalidInput`. First boot also enrolls `@sys`'s own recovery key and prints its one-time recovery phrase (seed-phrase recovery above), so the operator can reset the superuser password. Private signing key and JWT secret are never logged or returned. Partial first boot is rerunnable. `JUICE_SECRET_KEY` overrides stored JWT secret at runtime only. First boot also **requires a `kernel_handle`** — the name this kernel presents to the network (§13): from config, else `JUICE_BOOTSTRAP_KERNEL_HANDLE`, else an interactive prompt that repeats until a non-empty name is given; a headless first boot with none set fails rather than name the kernel silently.

The kernel's federation network identity is derived deterministically from this same Ed25519 signing key; there is no second identity or network key. The `public_key` is simultaneously the kernel's Juice identity (§13) and its address on the federation transport. The signature domains of the transport handshake and of Juice payloads (receipts, ratings, manifests, federation requests) must be disjoint: no byte string signed in one domain may verify as a valid message in the other. This disjointness is verified by test (§15). *Open item:* disjointness is today emergent (distinct JCS key-sets + libp2p's handshake prefix), not constructive; adding explicit per-domain signing prefixes changes every signed payload, so it is deferred to a federation-protocol version bump.

Every startup reads `config.superuser_handle` to confirm first boot and identify `@sys`; it verifies signing keys and aborts if either is absent. It then registers, enables, and makes public `@sys/lookup`, `@sys/llm/chat`, `@sys/llm/embed`, `@sys/llm/json`, `@sys/llm/decide`, `@sys/time`, `@sys/sink`, `@sys/message`, `@sys/random`, `@sys/web`, `@sys/step/race`, `@sys/step/join`, and `@sys/tinygo/compile` if absent, and reconciles their configurable fields (price and action-specific settings) from config on every startup. It also **soft-deletes any `kind=native` action whose handler is not registered in the running build** (disabling discovery, preserving history, §7): a native action removed from the platform stdlib stops being listed and callable on an existing database, self-healingly and for any native. It then runs recovery (§5).

Bootstrap is idempotent. Supervision operations are not native actions.

The superuser may suspend or unsuspend users. Suspension preserves data and makes every authenticated request return `ErrUnauthenticated`. It also excludes the suspended user's actions from action listings and makes them uncallable (`CanCall` fails on a suspended owner, §4); their data survives and unsuspending restores listing and callability.

`Kernel.Deposit(operator_user_id,target_user_id,amount,reason)` is admin-CLI-only supervision. It requires configured superuser and positive amount, then atomically credits `user.available` with a ledger entry (`from` null → `to` target). `reason` is optional and stored when provided. The operation is idempotent over `external_key` when supplied. It is served on the public TCP API (§14) as a superuser-gated route: authority is the `@sys` bearer token, not filesystem access.

`Kernel.Withdraw(operator_user_id,target_user_id,amount,reason)` is admin-CLI-only supervision: the mirror of `Deposit`. It requires configured superuser, positive amount, and `target.available ≥ amount`, then atomically debits `user.available` with a ledger entry (`from` target → `to` null) — the user's credits are redeemed and the operator owes the out-of-band payout. The operation is idempotent over `external_key` when supplied: a replay returns the existing ledger entry before the `target.available ≥ amount` check runs, so a replayed withdrawal never fails on a balance that has since dropped. Served on the public TCP API as a superuser-gated route (§14), like `Deposit`.

`Kernel.Transfer(caller_user_id,recipient_user_id,amount,reason)` is user self-service — the self-authorized sibling of `Deposit`/`Withdraw`, **not** superuser supervision. Only the authenticated, non-suspended local caller may move their own credits: it atomically debits the caller's `available` and credits the recipient's, recording one ledger entry (`from` caller → `to` recipient). It requires positive amount and sufficient caller balance (enforced atomically at the debit, so a concurrent spend cannot overdraw), and rejects a self-transfer, a missing or suspended recipient, and a **peer/proxy recipient** (`public_key` set) — crediting a proxy user would corrupt the bilateral federation account (§13). Because it is not an action and never routes through `Call()`, it has no composition surface: no action a caller runs can move the caller's funds. The operation is idempotent over `external_key` when supplied. Served on the public TCP API (§14) as a plain-authenticated route (no superuser gate). Local-only: never federated, gossiped, or exposed as an action.

`Kernel.UpdateUser(callerUserID, description, currentPassword, newPassword)` is user self-service: only the authenticated, non-suspended local user may update their own account. `description` and `newPassword` are both optional; at least one must be provided (`description` may be `""` to clear it). When `newPassword` is non-empty, `currentPassword` must match the stored hash; mismatch returns `ErrUnauthenticated`. An account with no password credential (a key-only account) cannot use this operation (`ErrInvalidState`). `handle` is immutable except by superuser rename. The update is atomic.

`Kernel.RenameUser(operatorID, targetID, newHandle)` is superuser-only supervision, the sole path that changes a `handle`: it validates `newHandle` (unique, no `/`) and writes it atomically, vacating the old handle for reuse. The superuser's own account cannot be renamed (`ErrInvalidInput`), as its handle is bound to `config.superuser_handle`. Served on the public TCP API (§14) as a superuser-gated route.

## 13. Federation

### Transport

Federation has exactly one carrier: a peer-to-peer transport (libp2p) behind the replaceable `fed` interface (§2), which `kernel` never imports. The kernel addresses a peer only by its `public_key`; the transport resolves that key to a live connection — direct when the peer is publicly reachable, hole-punched through NAT when possible, relayed through a public helper node as a last resort. Streams are mutually authenticated by peer key, so a connection is itself proof of the counterparty's network identity; the per-request Juice signatures below are nonetheless retained deliberately, because receipts, rejections, and dispatch records must be storable and verifiable offline (§11) — channel authentication cannot replace a signed artifact. The transport-handshake and Juice-payload signature domains are disjoint (§12).

On startup the kernel announces its key to the discovery network (DHT / rendezvous), bootstrapped from `bootstrap_peers` in `config.json` (§14). A bootstrap peer is just a publicly-reachable kernel (every kernel runs the DHT and a relay); the shipped default points at the project's public node, so a fresh `juice serve` joins out of the box. An empty list disables the DHT directory engine (the kernel neither announces nor discovers), but peer sync still runs (§13 peer sync), so a kernel with imported proxies keeps its peers' cached state fresh. `bootstrap_peers` is also the **seed of the known network** (§13 Gossip): on a timer (`discovery_interval_seconds`, §14) the kernel advertises itself as a provider under a fixed discovery key, enumerates that key to learn other online kernels, and pulls gossip from the seeds plus enumerated providers into `DiscoveredKernel` — so a fresh box has a directory to subscribe from. This directory pull is separate from gossip's reputation role and from subscribing: learning a kernel this way grants nothing (calling still requires a subscription and a deposit, §13). Because a libp2p peer ID inlines its Ed25519 key, a bootstrap peer supplied only as a multiaddr is addressable and inspectable by the same base64url key every federation command takes. Publicly-addressed and NAT-bound kernels federate identically — a home kernel behind a router needs no advertised address, port-forwarding, or `.well-known` document of any kind (the local `server_url` in §14 is only the loopback URL the CLI dials to drive your own kernel, never a federation address).

Federation protocols are versioned libp2p streams: `/juice/fed/call/1` (inbound proxy call), `/juice/fed/step/1` (step listing and completion, below), `/juice/fed/manifest/1` (manifest serving, chunked per action so a large-catalog sync survives bandwidth-capped relayed connections), `/juice/fed/gossip/1`, and `/juice/fed/inspect/1`. The first two carry the calculus's two eliminators — `Call` enters a computation, `CompleteStep` resumes one (§10) — and a wire carrying only the first would strand every continuation addressed across a boundary. There is no subscribe handshake — subscription is a local manifest import (below). Payloads and verification are exactly the settlement rules below; only the carrier is libp2p.

Inbound federation traffic is resource-limited at the transport: per-source-address limits where an address is visible, per-peer stream and byte budgets, a global inbound cap, and stricter budgets for relayed (address-less) traffic. Peer identities are self-issued and free to mint, so per-key limits alone are never sufficient against Sybil flooding. These transport limits replace §14's per-IP peer-request rate limit.

### Peers and proxy users

There is no user "kind" — every user is one account model distinguished only by the credentials it holds. A **password account** (from `user create`) logs in and holds tokens; a **key account** (`public_key` set, created only by peer acceptance) is a peer kernel's account here and authenticates by federation signature, per request. An account can't get a token without a password, and can't be a federation counterparty without a key. Because a peer is just a user, federation adds no new money model: a key account holds credits, pays, and is paid like anyone else.

A `@handle` names an account in one kernel's namespace; a `public_key` names it in the global (inter-kernel) namespace. The `@` prefix vs a base64url key tells them apart, so any command naming an account accepts either. Subscribing to a peer mounts its namespace under a local handle: peer B's action owned by `alice` is imported owner-qualified as `@B/alice/greet` (`@B` names the mount, `alice/greet` the action), so two owners on B with the same action name never collide. The relation is not transitive, so the graph flattens into each observer's root rather than nesting. Key rotation is unsupported — a lost key is a lost identity and reachability at once; a peer's self-reported handle is only a default mount, each subscriber binding its own: `subscribe` mounts under the self-reported handle (auto-suffixed on collision) and `admin rename <key> <handle>` changes it. Settlement rolls up to the mount: every one of B's owners settles into the single proxy user for B (the bilateral account), so `@B/alice/…` and `@B/bob/…` name and attribute but do not fragment the wallet.

### Subscribing — the ACT relation

Consuming another kernel's actions is a **directional subscription**, not a mutual friendship. A **subscribes to** B by importing B's active public actions as owner-qualified proxies (`@B/owner/name`), then calls and pays for them. `subscribe` is a purely local import — it pulls B's signed manifests over the open, read-only manifest protocol and writes nothing to B; B imports nothing and makes no decision.

```text
juice admin subscribe <key>             import a peer's active public actions
juice admin unsubscribe <user>          stop consuming: deactivate that peer's imported proxies here (user is @handle or key)
juice admin suspend <user>              freeze any account — local or peer — so it cannot act
juice admin unsuspend <user>            lift a suspension
juice admin rename <user> <new-handle>  rename an account's local handle, including a peer's local mount
juice admin peers                       known peers and balances (--all also shows suspended)
juice admin inspect <key|user>          view remote identity, public actions, transacted peers, and reachability (no DB write)
juice admin steps <user>                list waiting steps a peer holds for this kernel
juice admin complete <user> <step-id> [json]  complete a step a peer holds for this kernel
```

Federation trust is superuser supervision, so these live under `admin`, served on the public TCP API as superuser-gated routes (§14). `admin inspect <key>` is the operator's window into a remote kernel (there is no browser-reachable federation endpoint): it reports the peer's identity, public actions, and transacted peers, plus reachability diagnostics (direct / hole-punched / relayed, latency, protocol versions).

Federation commands are defined for an offline peer and bounded so they fail promptly: `admin subscribe` fails as unreachable, `admin inspect` degrades to the last-known local data with reachability `unreachable`, and `admin unsubscribe`/`suspend`/`peers`/`rename`/`identity` are local and always work.

`admin identity` prints this kernel's own federation identity — its public key (the value peers subscribe to it by, since there is no `.well-known`), handle, and libp2p listen addresses. Because every kernel runs a circuit-relay service and joins the discovery DHT, a **publicly-reachable `juice serve` automatically is the network's bootstrap + relay** — the meeting point NAT-bound kernels announce to and are reached through; there is no separate seed process. A public node binds the standard federation port `31313` for a stable address (a NAT-bound node uses an OS-assigned port and is found by key).

Subscription is **permissionless**: a kernel's public actions are by definition callable, so `subscribe` needs no approval, pending, or handshake. It grants nothing alone — a call is rejected until B holds credit for A. The trust decision is the **deposit** (§12): after settling A's payment out of band, B's operator `deposit`s A's key — the single act that both **creates** A's billing account (`@A`, a key account, §3) and funds it; a caller with no account is auto-provisioned a zero-balance one on its first signed call, so a price-0 action is callable at once while a priced one waits for funding. The local mount handle is addressing only, never signed or sent: `subscribe` mounts under the peer's self-reported handle (auto-suffixed on collision), and `admin rename <key> <handle>` changes it.

Moderation is **two orthogonal verbs**, one per edge. `unsubscribe` is the consumer's lever: it deactivates the proxies A imported from B and nothing else — A's balance, history, and B's account survive, and `subscribe` re-imports. `suspend` is the account lever, unified across humans and peers (§12): it freezes an account's ability to act — a suspended human cannot log in; a suspended peer's inbound calls are refused with a signed rejection — reversible with `unsuspend`. There is no separate "unfriend" and no `denied_at`: a suspended peer is the former deny, governed by the one `suspended_at` column (§3). Suspend is a pure freeze with no cascade.

The relation is **not transitive** and is the only path to calling: invoking `@B`'s actions requires subscribing to B directly. Exposure is each side's lever: a kernel may stop serving manifests to a subscriber whose balance cannot cover its cheapest exposed action; funding restores them.

### Gossip — discovery and reputation

Gossip is information, never authority. The `/juice/fed/gossip/1` protocol (open, read-only) returns the kernel's identity (key, handle, and its **about** — `@sys`'s description, §3 — a free-text self-description an operator sets with the ordinary user-update path, no config key), its own exposed actions with manifests and local stats, and its **transacted peers** — peers it has settled calls with — each with key, handle, and the kernel's earned local stats for that peer's actions. Gossiped actions are named owner-qualified (`@owner/name`) in the originating kernel's namespace, never bare. No URLs appear: a peer is identified by key, resolved through the transport. Peers without settled traffic are not gossiped: endorsement is earned by trade, never granted by subscribing.

Two engines feed the known network, matched to its growth goal. The **directory** — *which* kernels exist — grows fast and broad from DHT provider-record enumeration (the announce/discover pass above): every kernel provides a fixed discovery key and enumerates it, so the roster fills at the rate kernels come online. **Gossip** is the reputation overlay on top: it is deliberately trade-gated (transacted peers only), so it can never be the directory — it supplies earned stats for entries the directory surfaces. A kernel accumulates both into `DiscoveredKernel`, one row per (kernel, introducer); the introducer is the kernel itself for a first-party self-report (directory/self gossip) and the telling peer for hearsay. Supervision surfaces this as a grouped roster (kernel → introducer → action), with the operator's own earned stats shown distinctly from gossiped opinion, since own stats dominate the priors as they accumulate (§9).

Gossip results accumulate in the local discovery table, one row per (kernel, introducer):

```text
DiscoveredKernel { public_key, handle, introduced_by, stats_json, first_seen, updated_at }
```

Third-party stats are stored namespaced by source (`StatTag`, `source = <introducer>`) and affect **ranking only** — priors weighted by introducer count and local experience with the introducer, dominated by own `Stats` as they accumulate (§9). They never affect callability, pricing, or settlement. Knowing a kernel through gossip permits nothing: calling requires your own subscription, and manifests are always fetched and verified from the owner, never trusted from an introducer.

Importing an action initializes its local `Stats` to the defaults (§3): ground truth starts empty and is earned by settled calls. Priors are consulted at ranking time from the stored manifest (the owner's claim) and `StatTag` (introducers' earned stats), never copied into `Stats`. How priors combine is the ranking layer's tested, replaceable formula (§9); the kernel mandates only the signals and the discipline above.

The gossip response additionally carries `counterparty_balance`, populated only when the requesting peer authenticates as a known, non-suspended key: the requester's proxy-user `available` on the serving kernel — "your credit here". It is absent for strangers, suspended keys, and anonymous pulls. Like all gossip it is information, never authority: the money path is driven solely by authoritative signals (signed receipts and rejections, connection outcomes), never by this cached figure.

**Peer sync.** The same discovery timer (`discovery_interval_seconds`, §14) also pulls gossip directly from every known, non-suspended peer, independent of the DHT directory: peer sync is maintenance addressed by key, not discovery, so it runs even when `bootstrap_peers` is empty (the empty-list rule — neither announce nor discover — governs only the DHT directory engine). A successful pull persists `peer_last_seen = now` and `peer_credit = counterparty_balance` on the peer's user row (§3). Both are a display-only cache (§14): they never gate a call, price, or settlement, and they do not count as peer activity for retention (§13 Retention) — answering gossip is free liveness and would otherwise immortalize a zombie peer; retention stays trade- and value-based.

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
                  a rejection whose transport status is 402 (the remote's ErrInsufficientFunds:
                  this kernel's credit there is exhausted) settles as ErrPeerUnfunded — an
                  operator condition, never rendered as the caller's own insufficient funds
never dispatched (first dispatch only):
                  the transport provably never established a connection (resolve/connect
                  failed before any request byte was written); settle immediately as an
                  ordinary local failure (ErrPeerUnreachable), full refund — the normal
                  failed-call path, not receipt settlement. The background retry of an
                  already-parked trace NEVER uses this rule: once a dispatch may have reached
                  the peer, only a signed receipt or the max-pending-age bound settles it.
no receipt (timeout, but a dispatch may have reached the peer):
                  no settlement — the call stays running, the process stays open;
                  retry with the same idempotency_key until a signed receipt arrives
```

This is the §6 local failure rule across the wire: a failed call refunds its remaining allocation, and the receipt's `charge` is how the local kernel learns what remained. A failure counts against the proxy's stats regardless of charge — a provider gains nothing by failing-with-charge over succeeding, and farming failures collapses its rank (§9, §16).

Duty taxes the actual import, to the local `@sys`; the remote kernel never sees it. Default `import_bps = 500`, per peer. Forced closure (`EndProcess`) fails an in-flight proxy call like any running call (§6) and refunds the caller; if the remote side did commit, its charge stands there and is absorbed by the bilateral account, surfacing at reconciliation.

The no-receipt state is the expected steady state of a network of intermittently-online home kernels, not an error: a dispatched call holds its allocation locked and its process open until a signed receipt settles it or the owner forces closure, while a peer unreachable at the very first dispatch instead fails fast with a full refund (the never-dispatched rule above) — so only a call that may already be executing remotely is parked. Process listings must surface this awaiting-receipt state with its age (the earliest parked call's start), so an operator sees funds parked on an unreachable peer and for how long; step listings likewise flag a waiting step whose required caller is a peer. Both are factual (age, peer-ness), never a liveness claim.

Inbound: calls sign `JCS({action, counterparty, idempotency_key, timestamp, args_hash})`; `counterparty` is the caller's base64url public key; `args_hash = SHA-256(raw body)`. Receiver verifies signature, raw hash, timestamp age ≤ 5 minutes. A valid signature from an unknown key lazily provisions a zero-balance proxy account for it (handshake-free subscription, §13); a suspended key is rejected. The call is then `run` as that proxy user, paying from its balance. Any inbound call the receiver can determine will not execute — insufficient balance, or a known-but-non-executable action (inactive, not `public` so a peer caller fails `CanCall` per §4, suspended owner) — returns a **signed rejection receipt** (`status = failure`, zero charge) carrying the action's id, so the caller always has something to settle on rather than pinning funds until the pending bound. Only a genuinely absent or unverifiable action stays a plain error (no receipt whose `action_id` could match the caller's stored `remote_action_id`), leaving the caller pending. Idempotency: insert pending before execution, unique `(idempotency_key, counterparty_user_id)`; completed replay returns the stored receipt, pending replay 409, expiry 24h.

`VerifyRemoteReceipt(caller_id, tx_id)` requires `CanReadTransaction` and verifies entirely locally, in two parts. **Receipt integrity:** signature against the peer's `public_key`, stored JSON against its stored SHA-256, `receipt.action_id == proxy.remote_action_id`. **Settlement consistency:** the local transaction's outcome matches `receipt.status`; the amount paid to the proxy user equals `receipt.charge`; the local refund equals `(mp + maxduty) − receipt.charge − duty` with duty per this section (zero on failure); `args_hash` and `reply_hash` match the local record. The receipt's own `gross/net/fee` are the remote kernel's economics and are reported, not compared. Returns per-check results and top-level `valid`; non-proxy transactions give `ErrInvalidState`.

### Steps across the wire

A Step's `required_caller_user_id` may be a peer's proxy user: `CreateStep` resolves it like any other account and `CanCall(required_caller, action)` is checked at creation (§10), so a peer may be parked only for a `public` action. `/juice/fed/step/1` is what makes such a step completable — a key account holds no session token, so it can never reach the HTTP route (§12). Without it the step waits forever with its price parked and no actor able to free it, since the process owner is that same keyless account and `EndProcess` is process-owner-only (§10).

The protocol has two request kinds, each signed under its own canonical payload, both key-sets disjoint from every other signed payload (§12):

```text
list:      JCS({counterparty, recipient, scope: "step_list", timestamp})
           → the serving kernel's waiting steps whose required_caller is the requesting peer,
             oldest first, each with its id, partial_args, derived allowed_input (§10), price,
             and creation time; bounded per page, and a full page sets `truncated` with
             `next_cursor`, which the requester follows until exhausted
complete:  JCS({counterparty, idempotency_key, input_hash, recipient, step_id, timestamp})
           input_hash = SHA-256(input bytes); the request carries exactly those bytes
           → CompleteStep as the peer's proxy user; returns {result, tx_id, trace_id, receipt}
```

`recipient` is the **serving** kernel's public key. Every other signed payload names only its
sender, so a captured request is replayable to any kernel that would accept it — for a step list,
one kernel could replay another's request to a third and enumerate the steps parked there. Binding
the recipient closes that. The call payload has the same weakness and is deliberately left alone:
adding a field there breaks the wire for every existing peer, which §12 assigns to a
federation-protocol version bump. The step list is scoped to the required caller **in the query**,
never by filtering a capped page afterwards: the peer's own inbound-call processes are visible to
it under `CanListStep` and would otherwise crowd out precisely the steps it can complete.

Both verify the signature and a timestamp age ≤ 5 minutes, and the connection's authenticated key must match `counterparty`, exactly as an inbound call does. A suspended peer is refused on both (§12 — suspension is the one moderation axis for peers too). `list` is a pure read: an unknown key gets an empty list and is **not** lazily provisioned, since provisioning belongs to a call, which is what opens a billing relationship. `complete` refuses an unknown key outright — a stranger can hold no step here, because `CreateStep` resolves its required caller to an existing account.

A peer is served **the request, not the requester**. The reply carries only what the completer needs to act: `partial_args` (the payload the creator bound for it — §14 has `@sys/message` put its body there) and `allowed_input` (§14's substitute for reading a target action that may be private). The creating action's reference, the process owner's handle, the target action's `@owner/name`, and every local trace/action/transaction id are withheld: a user identity crossing a kernel boundary is what the encapsulation forbids, and none of it is needed to complete a step. The peer-facing shape is therefore a distinct representation, not the local step view narrowed.

`complete` reuses the cross-kernel idempotency record (`(idempotency_key, counterparty_user_id)`, §3): pending before execution, completed with the stored result, a completed replay returning that result and a pending replay 409. The key is **derived from the request** — `SHA-256(protocol | peer key | step_id | input_hash)` — not minted per attempt, so a retry after a network failure presents the same key and recovers the stored outcome; a step completion has no local trace to persist a key on, unlike a remote-proxy call (§13 outbound). The record id is threaded **into the kernel**, exactly as an inbound call threads its own: whichever commit finally settles the completion writes the record's completion atomically with the transaction (§5). That includes a settlement that happens much later — the remote-dispatch retry loop, the pending max-age bound, or a forced closure — because the parked dispatch persists the record id (§3). The service layer therefore disposes of the record only where **no commit will ever occur**: a completion rejected before anything settled deletes it, so a corrected retry is not locked out. A completion **awaiting a peer's receipt** leaves it *pending*, which is now honest rather than terminal: a replay reports a duplicate in flight, and the eventual settlement completes it.

A replayed record is rebuilt from its two stored halves — the result and the signed receipt — and discriminates success on the **receipt's** status, not by probing the result for an `error` key, which a legitimate result carrying its own `error` field would trip. A stored failure never replays as success, and the in-flight reply carries an error code so the requester re-raises a typed error instead of a generic failure.

A paged list is followed to exhaustion by the requester. A page that fails **after** earlier pages were collected returns those pages with `truncated` and a warning rather than discarding them — partial visibility of parked funds beats none — while a failure on the very first page stays an error, since an empty success would read as "nothing is parked for you".

An outbound completion distinguishes *never dispatched* from *no reply* exactly as an outbound call does (§13): only a provably-unsent request is `ErrPeerUnreachable`; any other transport failure may already have executed remotely and is reported as `ErrTimeout`, recoverable by retrying under the derived key.

Unlike a proxy call, a step completion moves **no money on the requesting kernel**: the step's price was parked on the serving kernel at creation, and completion never checks funds (§10). The requester therefore parks nothing, creates no local trace or transaction, and needs no settlement — so failures are ordinary typed errors, not signed rejection receipts, and a timeout pins nothing. The completion settles wholly on the serving kernel under §6, with the role law giving `caller_user_id` = the peer's proxy user. That settled call is ordinary peer activity for retention (below).

Driving the protocol is superuser supervision, like every other federation verb: `admin steps <peer>` lists what a peer holds for this kernel and `admin complete <peer> <step-id> [json]` resumes one (§14).

### Retention

A peer is kept only while it holds value or has been used recently. Its activity is the most recent settled call (either direction), gossip mention, or deposit/withdrawal; a nonzero balance or funds locked in flight always count as live. A peer idle past `peer_retention_days` (§14) — reachable only at zero balance with nothing locked — has its accumulated data purged: its proxy actions, their stats, its `StatTag` rows, and its `DiscoveredKernel` rows, and its identity is forgotten (its `public_key` is cleared, so re-subscribing starts fresh). The immutable transaction and receipt ledger is preserved — its party ids carry no foreign key, so a now-dangling peer id is harmless and every local counterparty's credits stay reconstructible (§11); the anonymized user row remains as a legible ledger anchor. Purge is reachable only through zero value, so it never deletes funds or a still-reconstructible credit.

## 14. CLI, HTTP, logging, config

HTTP API is primary. Every exposed endpoint has a CLI command. CLI uses the same service layer, supports human-readable and JSON output, and each command has at least one test. All commands — user-facing and admin/peer alike — are HTTP clients of the server (base URL from `--server`/`JUICE_SERVER`/`server_url`); admin and peer commands hit the same public TCP API, on routes gated by an `IsSuperuser` check, authenticated by the `@sys` bearer token (no separate socket or filesystem authority — keep the bearer secret and run `serve` behind TLS or on loopback). `juice serve` is the sole process that opens SQLite; the CLI never touches the database directly. A command's primary identifier is a positional argument by its natural key — a user is `@handle` (a public key or raw id is also accepted), an action is `@owner/name` (an id is also accepted), and processes, steps, and transactions are ids; a second mandatory value (amount, rating) is the second positional. CLI human-readable output exposes the same fields as the corresponding HTTP response; `--json` selects the canonical JSON form (the HTTP shape).

CLI handlers and HTTP handlers are thin wires: parse input, call the public TCP HTTP API (admin/peer commands hit superuser-gated routes on that same API), and format output. All kernel calls, enrichment, validation, and transformation live server-side in the service layer. No kernel calls outside the service layer.

Required commands:

```text
juice serve
juice user create <user>                   juice user me
juice user update                          juice user connect <selector>
juice user disconnect <selector>           juice user transfer <recipient> <amount>
juice user ledger
juice auth login <user>                    juice auth logout
juice auth recover <user>
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
juice admin rename <user> <new-handle>
juice admin deposit <user> <amount>       juice admin withdraw <user> <amount>
juice admin subscribe <key>               juice admin unsubscribe <user>
juice admin peers                         juice admin inspect <key>
juice admin steps <user>                  juice admin complete <user> <step-id> [json]
juice admin identity
```

`admin` holds only the operator verbs no ordinary user performs — money, access, federation trust, and the global roster (`users`/`show`). Supervision over everything else is **scope on the normal commands**: a superuser sees all owners' rows on `action list`, `process list`, `tx list`, and `step list`, and may `action disable`/`enable` any action, all over the public TCP API. As for everyone, `action list` is active-only by default; inactive/private rows appear only with `--all` (`?all=1`), so a deactivated action — e.g. an unsubscribed peer's proxies — drops out of the default list. There is no `admin actions/disable/processes/txs/steps` — those were duplicates of the base commands with wider reach.

OpenAPI commands (the OpenAPI spec URL is the positional argument):

```text
juice action import <spec-url>
juice action unimport <spec-url>
juice action unimport <spec-url> --name <action-name>
```

`juice serve` handles `SIGTERM`/`SIGINT`, stops accepting new requests, drains in-flight calls, exits cleanly. No `juice stop`.

Server logs request, caller, process, trace, action, and transaction IDs where available; maps distinct auth, authorization, invalid input, insufficient funds, missing resource, and internal failures to distinct statuses; rate-limits auth and account creation per client with 429 (inbound federation traffic is limited at the transport instead, §13). A genuine-loopback client — the operator's own CLI, direct with no `X-Forwarded-For` — is exempt, since it is already inside the trust boundary the limiter defends; when a loopback peer does carry `X-Forwarded-For` (a co-located reverse proxy), the real client (the last forwarded hop the trusted proxy appended) is the rate-limit key, so external callers are limited per-client rather than sharing one bucket. Action read/list responses include computed `action=@owner/name`, plus the non-secret auth summary `requires_grant` (always) and `auth_scheme` (when the action has upstream auth) — the scheme name and flag only, never `auth_json`'s config or secrets (§8). A `kind=remote_proxy` action additionally carries `peer_state ∈ {offline, unfunded}` from the peer-sync cache (§13): `offline` when the owner peer's `peer_last_seen` is null or older than 3× `discovery_interval_seconds`, `unfunded` when cached `peer_credit` is below the action's remote manifest price; omitted when healthy. It is display-only — lookup, callability, and settlement are untouched — and derives its staleness threshold from `discovery_interval_seconds`, adding no config key. `admin peers` likewise surfaces the cache: our credit on each peer (`peer_credit`) and `last_seen` beside the bilateral balance held here. Outputs render user identities as `@handle`, never as raw user ids: a returned id must be a consumable input to some command. A command accepts a user by `@handle`, public key, or raw user id — the three shapes are disjoint and self-identifying, so a single resolver disambiguates (symmetric with the action argument, which accepts `@owner/name` or an id); outputs still never render a raw user id. So transaction responses carry `owner_handle`/`caller_handle`/`target_handle` (not the stored `*_user_id`), step responses `required_caller_handle`, process responses `owner_handle`, ledger responses (deposit/withdraw/transfer) `operator_handle` plus `from_handle`/`to_handle` (each present only when that side is set — `from_handle` absent on a deposit, `to_handle` on a withdrawal), and the peer list carries no internal id — while the underlying immutable records keep their captured `*_user_id` fields (§3). A handle unresolvable at read time (a purged party, §13) falls back to the raw id. `GET /v1/me` is the sole exception: it returns the caller's own `id`.

Endpoint rules (notable rules only; the complete HTTP endpoint list is in `API.md`):

| Endpoint                                         | Rule                                                                                                            |
| ------------------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| `GET /health`                                    | unauthenticated → `{status, handle, public_key}`: liveness plus the kernel's advertised federation identity (§13), so a client sees which kernel it's on before logging in |
| `GET /v1/actions[?owner=&name=&all=]`             | unauthenticated → active public actions; authenticated → active public and local actions plus caller's own active actions (union, deduplicated) — a session caller is a local user, so `local` actions are visible to them but not to anonymous callers; superuser → all owners' active actions; a suspended owner's actions are excluded (§12); `?all=1` includes inactive/private rows in the caller's scope; `?owner=` further filters by that owner's handle; `?name=` filters by name |
| `GET /v1/me`                                     | authenticated `id`, `handle`, `description`, `available`, `locked`, `connectors`, and `connections`; suspended rejected before handler. `connectors` is the directory-grouped consent tree (§8): one node per folder (`directory` = the granted actions' shared folder, the ref up to its last `/`, e.g. `@chat` or `@chat/inbox`), each with the `connections` (backing upstream account(s), usually one) and the token-free `actions` under it (each: action `@owner/name`, requested scopes, `provider_key` of its backing connection, `created_at`). `connections` is the full account inventory (each: `provider`, action count, `unused` flag, `provider_key`, `created_at`), so a zero-grant connection still surfaces as `unused`. The `directory` is a display grouping only — it never gates a credential (the token binding stays per-action and fact-derived, §8 confused-deputy defense); `provider_key` is the opaque account key that also addresses `DELETE /v1/grants?account=`, and is omitted on an unbackfilled legacy grant with no connection |
| `GET /v1/grants/plan?selector=`                  | authenticated; expands the selector (§8) over delegated actions the caller may call and returns the consent plan `user connect` walks: groups by `provider_key` — `provider`, scheme, requested scope union, destination host(s), connected/covered status, and per-action `granted` flag — plus counts of skipped actions (loginless or not-callable) |
| `POST /v1/grants/start`                          | authenticated; `{selector, provider, [redirect_uri], [flow]}` begins delegated-OAuth consent for one provider group (§8); when the caller's existing `Connection` already covers the group's scope union, returns `{status:"granted", actions}` immediately with no browser; else `redirect_uri` is any http(s) target the client receives the code at, and code flow returns `{state, authorize_url}`, device flow `{state, verification_uri, user_code, interval, expires_in}` |
| `POST /v1/grants/complete`                       | authenticated as the grantor; `{state, [code]}`; exchanges the code (or reports device-poll `pending`), stores one `Connection`, and mints the group's grants; foreign/expired state rejected |
| `POST /v1/grants`                                | authenticated; `{selector, [provider], token}` stores a static token for a `delegated_bearer` group (§8) — the non-OAuth direct twin of start/complete — minting one grant per action against one `Connection`; `provider` optional when the selector resolves to exactly one bearer group; rejects a non-`delegated_bearer` group and actions the caller may not call |
| `DELETE /v1/grants?selector=` / `?account=`      | authenticated; `selector` revokes the caller's grants matching the selector (grants only; a bare `@owner/name` is the degenerate single-action case, and legacy `?action=` is accepted as its alias); `account=<provider_key>` deletes the caller's `Connection` for that provider and cascades its grants |
| `PUT /v1/me`                                     | authenticated password account only; `{[description], [current_password, password]}`; `password` requires `current_password`; at least one field required (`description` may be `""` to clear); returns updated `id`, `handle`, `description`, `available`, `locked`; a key-only account (no password) returns `ErrInvalidState` |
| `POST /v1/transfers`                             | authenticated; `{recipient, amount, [reason], [external_key]}`; moves the caller's own credits to a local recipient (§12); resolves `recipient` by `@handle`/key/id; rejects a non-positive amount, self-transfer, missing/suspended recipient, and a peer/proxy recipient; not superuser-gated; returns the ledger entry with `operator_handle`/`from_handle`/`to_handle` |
| `GET /v1/ledger[?limit=&offset=]`                | authenticated; returns the caller's own ledger entries (deposits, withdrawals, transfers where they are `from` or `to`), most recent first, paginated (default 50, cap 200), each with `operator_handle`/`from_handle`/`to_handle` |
| `PUT /v1/actions/{id}`                           | action-owner update; `visibility` (`private`/`local`/`public`) updatable and does not deactivate; widening beyond `private` on an OpenAPI import requires verified ownership (§8); deactivation rules apply                                               |
| `DELETE /v1/actions/{id}`                        | action-owner delete preserving history                                                                          |
| `POST /v1/actions/import`                        | authenticated OpenAPI supervision import                                                                        |
| `POST /v1/actions/unimport`                      | action-owner import-provenance deactivation                                                                     |
| `GET /v1/processes`                              | process owner's processes, descending `created_at`; each carries `awaiting_receipt` and, when set, `awaiting_receipt_since` (§13) |
| `GET /v1/steps`                                  | authenticated; returns steps visible to caller per `CanListStep`; optional `?process_id=` and `?status=` filters; each step carries `created_by` (the creating action `@owner/name`, from its parent trace) beside `action` (the completion target) and `owner_handle` (the process owner / payer, mirroring a transaction's `owner_handle` — the step is the continuation that settles into that owner's transaction, §10); a waiting step also carries `allowed_input` (the derived completion schema `action.input_schema \ keys(partial_args)`, §10) so its required caller can complete it without separately reading a private target action; a waiting step whose required caller is a peer carries `waiting_on_peer` (§13) |
| `POST /v1/steps`                                 | JWT: creates a waiting step; requires `trace_id` (funding trace), `action_id`, `required_caller`, `partial_args`; the authenticated user must be authorized to use `trace_id` (§4 precondition 4). Capability (§9): `trace_id` comes from the capability and is rejected in the body; the creating authority is the trace's action owner |
| `GET /v1/steps/{id}`                             | `CanReadStep`; returns step fields                                                                              |
| `POST /v1/steps/{id}/complete`                   | JWT or capability (§9); `CanReadStep`; `args` required (`{}` valid); absent gives `ErrInvalidInput`; under a capability the caller is the action owner; returns `result`, `tx_id`, `trace_id`, `step_id` |
| `POST /v1/call`                                  | capability only (§9); the HTTP twin of `juice.call`: a subcall on the capability's trace; body `{action, args}`; no `run`/wallet path; returns `result`, `tx_id`, `trace_id`. Not a user-invocable command (no CLI), like inbound federation |
| `POST /v1/run`                                   | requires `args`; `{}` valid; absent gives `ErrInvalidInput`; action is `@owner/name`; never accepts a capability |
| `POST /v1/auth/authorize`                        | unauthenticated; `{handle, password, code_challenge, [redirect_uri]}`; if `redirect_uri` provided → `302` redirect; if omitted → `200 {"redirect": "?code=CODE"}` for programmatic clients |
| `POST /v1/auth/token`                            | unauthenticated; exchanges auth code + `code_verifier` for `access_token` and `refresh_token`                   |
| `POST /v1/auth/logout`                           | refresh token body; missing/revoked gives `ErrUnauthenticated`                                                  |
| `POST /v1/auth/recover/start`                    | unauthenticated; `{handle}`; issues a single-use, TTL-bound recovery nonce (§12); `ErrInvalidState` if the account has no recovery key enrolled |
| `POST /v1/auth/recover/complete`                 | unauthenticated; `{handle, nonce, signature, password}`; verifies the Ed25519 signature over `{recovery_challenge: nonce}` against `recovery_public_key`, consumes the nonce, and resets the password (§12); invalid/expired nonce or bad signature gives `ErrUnauthorized` |
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

Config lives in `config.json` under the kernel's home directory. Juice-family binaries share one root, `$JUICE_HOME` (default `~/.juice`), with one subdirectory per component; the kernel uses `$JUICE_HOME/kernel/`, so the config defaults to `$JUICE_HOME/kernel/config.json` and the database to `$JUICE_HOME/kernel/juice.db` (override either with `--config` / `--db`). This root is fixed and absolute — never cwd-relative — so the kernel attaches to the same identity and signing key wherever it is launched. `$JUICE_HOME/kernel/cache/` is reserved for regenerable data and is safe to delete. Top-level kernel keys: `db_path`, `fee_bps`, `import_bps`, `server_url` (the local base URL the CLI dials for user-facing commands — loopback for driving your own kernel, not a federation identity), `http_callback_url` (the base URL the kernel advertises to dispatched `kind=http` endpoints for capability callbacks, §9; default empty ⇒ derived from the listen address, `http://127.0.0.1:<port>`, which suffices for the co-located case), auth issuer/audience/token TTL, log file/format/level, script timeout and memory limits, plus the federation identity and discovery keys this kernel needs to satisfy §8 and §13: `kernel_handle` (the handle this kernel presents to the network in gossip), `bootstrap_peers` (the peer multiaddrs the transport dials to join the discovery network; defaults to the project's public node so `juice serve` works out of the box, empty means the kernel neither announces nor discovers), `credentials_key` (the §8 base64url AES-256-GCM key for `auth_json` and sealed OAuth grant refresh tokens, auto-generated at first boot; each action's OAuth provider config lives in its `auth_json`, so there are no OAuth keys in `config.json`), `allow_local_sources` (escape hatch over §7's private/LAN/link-local/reserved URL rejection for action source URLs, default `false`; loopback is permitted by default and needs no flag), and `remote_retry_interval_seconds` (seconds between passes of the running server's retry loop that re-drives pending remote-proxy calls so a returning peer settles parked calls — and the §13 max-age refund fires — without a restart; default 60, non-positive falls back to the default), `discovery_interval_seconds` (seconds between passes of the known-network discovery loop that advertises this kernel, enumerates providers, and pulls gossip from bootstrap + discovered peers into `DiscoveredKernel`, §13; default 300, non-positive falls back to the default; an empty `bootstrap_peers` disables the directory engine but not peer sync, §13), and `peer_retention_days` (days a peer may stay idle at zero balance before it and everything derived from it are purged, §13; default 90, non-positive disables purging). All native-action configuration lives under `native.<action>`; no deeper nesting:

```json
{
  "db_path": "./juice.db",
  "fee_bps": 2000,
  "import_bps": 500,
  "server_url": "",
  "http_callback_url": "",
  "kernel_handle": "",
  "bootstrap_peers": ["/dns4/daios.ai/tcp/31313/p2p/12D3KooWJ5ZwPSAV17q2hv6ttZ8J3hHsTMNaSC61kbVbvxvtArjK"],
  "credentials_key": "",
  "allow_local_sources": false,
  "remote_retry_interval_seconds": 60,
  "discovery_interval_seconds": 300,
  "peer_retention_days": 90,
  "native": {
    "llm":     { "url": "http://localhost:11434", "chat_model": "gemma4:26b", "embed_model": "nomic-embed-text", "price": 0 },
    "lookup":  { "default_limit": 10, "price": 0 },
    "time":    { "price": 0 },
    "sink":    { "price": 0 },
    "message": { "price": 0 },
    "random":  { "price": 0 },
    "web":     { "price": 0, "user_agent": "juice-kernel/0.4 (+https://github.com/daios-ai/juice)" },
    "step":    { "price": 0 },
    "tinygo":  { "price": 5 }
  }
}
```

Safe local defaults apply when the file or a key is absent; invalid startup config is rejected; no committed production secrets.

Environment variables are bootstrap and overrides only:

```text
JUICE_HOME                 root for all juice state (default ~/.juice); kernel uses $JUICE_HOME/kernel/
JUICE_SECRET_KEY           JWT secret override, runtime only (§12)
JUICE_LOG_LEVEL            log level override
JUICE_CREDENTIALS_KEY      AES credentials key override, runtime only (§8)
JUICE_BOOTSTRAP_PASSWORD   superuser password for unattended first boot (§12)
JUICE_BOOTSTRAP_KERNEL_HANDLE  kernel name, required for an unattended first boot (§12, §13)
JUICE_ALLOW_LOCAL_SOURCES  permit private/LAN/link-local/reserved action source URLs (§7); loopback is allowed by default
```

All other settings are configured through `config.json` only; there are no further environment overrides.

## 15. Required automated tests

`go test ./...` must pass without external network access. Tests use temporary SQLite databases, fake Ollama, fake script, and fake `fed`-transport adapters unless explicitly integration tests, no global state, and no order dependence.

Federation is tested in three tiers. **Unit** (`go test ./...`, offline): kernel federation logic runs against a fake `fed` transport, exercising every §13 settlement rule without a real network. **Flows** (offline, real transport on loopback): the multi-kernel flow suite runs the actual libp2p transport over `127.0.0.1`, with one kernel serving as the bootstrap + relay for the others (every kernel runs the DHT and relay, so no separate seed process) — discovery-by-key, relayed carriage, and restart-retry are exercised on one machine with no internet. **Real-network check** (release gate for any federation-touching change, not part of `go test ./...`): a scripted flow run from a machine behind a real NAT against one remote peer, asserting hole-punch and relay-fallback paths that loopback cannot reproduce.

Required suites:

```text
user creation
authentication token validation
user update description; change reflected in GET /v1/me
user update password with correct current_password; old password rejected after change
user update password with wrong current_password returns ErrUnauthenticated
user update with neither description nor password returns ErrInvalidInput
key-only account (no password) UpdateUser returns ErrInvalidState
password below the minimum length rejected at user creation, first boot, and password update
seed-phrase recovery: an enrolled recovery key signs a server nonce to reset a lost password; old password rejected, new works; the nonce is single-use (replay rejected); a wrong-key signature is rejected; StartRecovery without an enrolled key returns ErrInvalidState
CLI recovery key derivation is deterministic and its signed challenge verifies against the enrolled key under the kernel's payload
users table rebuild (migration 021) drops email, defaults description to "", and adds a nullable recovery_public_key while preserving rows and FKs
action create/update/delete
action activation/deactivation
public/local/private access control (private: owner only; local: any local caller, peer denied; public: anyone)
caller-scoped CanCall: a provider's public composite reaches its own private helper in a customer's process; foreign code a process owner funds cannot reach that owner's private actions (no confused deputy)
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
lookup ranks by fused relevance only while stats-based quality weighting is UNDER REVISION (temporarily removed): two identical-relevance actions score equally regardless of success history
lookup results include action (@owner/name), input_schema, and output_schema
lookup degrades to lexical (BM25) ranking with no embedder configured; a keyword query still finds actions
lookup skips an embedding vector whose dimension differs from the query's (no panic, no cross-space score)
lookup applies CanCall before truncating, so uncallable matches do not starve callable results
lookup query is sanitized: FTS5 operators/quotes in the query are treated as literal terms, not syntax
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
@sys/web rejects private/link-local/reserved URLs unless allow_local_sources; loopback permitted by default
loopback action source (127.0.0.1/::1/localhost) permitted by default; private/link-local still rejected without allow_local_sources; a loopback source redirecting to a private/link-local address is still blocked
genuine-loopback client (no X-Forwarded-For) is exempt from the auth/account rate limiter; a loopback peer with X-Forwarded-For is limited by the forwarded client
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
active local action callable by any local caller but not by a peer (inbound federation call denied with a signed zero-charge rejection)
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
subcall authorizes action access by the immediate caller (the parent action owner), not the process owner
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
capability (§9): an http action subcalls via POST /v1/call; the subcall obeys the role law (caller = the http action's owner) and spends from its trace, matching a WASM juice.call subcall exactly (parity)
capability step_create sets parent_trace_id to the action's trace and parks from it; step_complete succeeds iff required_caller = action owner
capability is a signed trace_id valid only while the trace is unsettled; a tampered token and a token presented after settlement are both rejected; it never appears in args_json, reply_json, receipts, receipt hashes, or logs
a capability presented to POST /v1/run is rejected (no wallet path); concurrent capability callbacks cannot exceed the subtree bound and the process locked releases exactly price
capability + callback headers are injected on http dispatch and stripped across a host-changing redirect; absent callback URL sends none (leaf); an in-flight capability trace recovers as interrupted
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
deposit/withdraw/transfer all record one ledger entry: deposit from-null→to-user, withdraw from-user→to-null, transfer from-sender→to-recipient
transfer debits caller available and credits recipient in one commit; ledger entry recorded; both balances reconcile
transfer with amount exceeding caller available rejected with ErrInsufficientFunds; balances unchanged
transfer to self rejected with ErrInvalidInput
transfer to a peer/proxy user (public_key set) rejected with ErrInvalidInput
transfer by a suspended caller and to a suspended recipient both rejected
transfer is idempotent over external_key: a replay returns the existing entry and moves no funds twice
user ledger lists the caller's deposits, withdrawals, and transfers (from or to), most recent first, with from_handle/to_handle
transfer resolves the recipient by key (global name) as well as by @handle
receipt created atomically with successful transaction commit
receipt created atomically with failed transaction commit
action owner reads transactions for calls to their action
non-party denied access to a transaction
upstream auth secret never appears in args, replies, logs, receipts, or read paths
action read/list responses expose auth_scheme and requires_grant, never the auth config or secrets
unknown upstream auth scheme rejected at create/update and fails closed at dispatch
oauth client-credentials exchanges at the token endpoint and applies the bearer upstream (fake provider); token cached, dropped and refreshed once on a 401
oauth jwt-bearer signs an RFC 7523 RS256 assertion the provider verifies
delegated token applied iff grantor = process owner and grant action = executing action (both mismatch edges); refresh-token rotation persists onto the one connection leaving a sibling grant unaffected; invalid_grant deletes the connection and cascades its grants
call on an oauth_delegated action with no grant rejects before locking funds with structured ErrGrantRequired (code + action metadata): no transaction, no process, balances unchanged
grants/start accepts any http(s) redirect_uri (loopback or hosted), rejects a bad scheme
one grant per (grantor, action); re-consent overwrites; deactivating update / auth replacement / delete revokes the action's grants
grant tokens never appear in args, replies, receipts, logs, or any read path; /v1/me lists grants without tokens
grants/start and grants/complete require authentication; complete rejects another user's or an expired state
delegated-OAuth action is excluded from manifests and gossip; a remote-proxy call carries no local delegated token
delegated_bearer action create rejects an owner-side secret and a template missing {token}; accepts header/template and zero-config
delegated_bearer token applied into the configured header (default Authorization: Bearer, plus token/Private-Token/X-Api-Key) iff grantor = process owner and grant action = executing action
call on a delegated_bearer action with no grant rejects before locking funds with structured ErrGrantRequired: no transaction, no process, balances unchanged
delegated_bearer token supplied via POST /v1/grants; stored sealed; never in read paths; /v1/me lists it token-free; disconnect and deactivating update revoke it
delegated_bearer action is excluded from manifests and gossip
connection provider_key derived from the pinned base-URL host (bearer) and token_url|client_id + source registrable domain (oauth), never from action names; a colliding token_url|client_id with a different source domain does not ride an existing connection, and the consent plan surfaces the destination host
grant selector matches by path segment (@tom/brief matches brief and brief/x, never briefing; trailing /* stripped; a full @owner/name is the degenerate one-action selector)
consent plan groups the caller's delegated actions by provider_key, filters by CanCall, and marks connected/covered per group
connecting an action whose provider connection already covers the scope union grants instantly with no browser round-trip
one consent covers a multi-action provider group with the union of its scopes; a second action reuses the one connection
re-consent for a wider group widens the stored scope union so earlier grants keep coverage
disconnect by selector revokes only the matching grants; disconnect by account deletes the connection and cascades its grants
a connection with zero grants is listed unused in /v1/me and is never auto-expired
startup backfill re-homes each legacy grant token onto its derived connection (idempotent; latest created_at wins on collision; reseals under new AAD; an unrecoverable or non-delegated token deletes its grant; a nil credentials key leaves legacy rows untouched)
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
admin rename mounts a subscribed peer under an operator-chosen local handle (actions addressable and callable under it); a colliding handle auto-suffixes
kernel about: @sys's description surfaces as gossip `about` and in admin identity; admin inspect renders action descriptions carried in gossip/manifests
admin deposit/withdraw/unsubscribe/suspend resolve a peer by key (global name) as well as by @handle
fresh boot with no configured handle derives a distinct @k-<key> kernel handle, never @sys
remote manifest signature is Ed25519 over canonical JSON excluding signature
remote proxy transaction has target_user_id = proxy user id
subscribe opens a zero-balance billing account; a deposit by key both provisions and funds a not-yet-known peer's account
a signature-valid inbound call from an unknown key is lazily provisioned a zero-balance account; a price-0 call then succeeds, a priced one gets an insufficient-funds rejection
inbound call to a known-but-non-executable action (inactive, local/private so a peer fails CanCall, suspended owner) gets a signed zero-charge rejection receipt the caller settles on immediately
unsubscribe deactivates the peer's imported proxies; balance and history survive; re-subscribe reactivates
suspend freezes a peer's inbound calls (signed rejection receipt) and is reversible with unsuspend; there is no denied_at
gossip lists only transacted peers with stats, keyed by public key with no URLs; non-transacted peers absent
subscribing to a peer does not import that peer's own imports (no transitive re-export); manifests and gossip exclude remote_proxy actions
subscribe-imported proxies get visibility=local; manifests and gossip serve only visibility=public actions (a local own action is excluded from both)
action visibility migration maps legacy public=1 → public, public=0 → private; the visibility column rejects any value outside {private, local, public}
offline peer: inspect degrades to local data + unreachable, subscribe fails as unreachable, unsubscribe/peers/identity work locally
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
subscribe by key over the fake transport imports the peer's catalog and creates a zero-balance proxy account
outbound call whose first dispatch provably never connects settles immediately as ErrPeerUnreachable with a full refund; a dispatch that may have reached the peer never fail-fasts — allocation stays locked, process stays open, retry resumes on reconnect; the retry loop never settles a parked trace on a connection failure
signed zero-charge 402 rejection settles as ErrPeerUnfunded with the peer handle in meta, never as the caller's own insufficient_funds
gossip response carries counterparty_balance only for an authenticated known non-suspended peer; absent for strangers, suspended keys, and anonymous pulls
successful peer gossip pull persists peer_last_seen and peer_credit; peer sync runs with empty bootstrap_peers; admin peers surfaces both
action listings annotate remote proxies with peer_state offline/unfunded from the sync cache; the annotation never gates a call
peer step list returns only steps whose required caller is the requesting peer; another peer sees none; an unknown key gets an empty list and is NOT provisioned an account
peer step complete resumes the step as the peer's proxy user (role law: caller_user_id = proxy user), settles on the serving kernel, and is idempotent over (idempotency_key, counterparty): a replay returns the stored result and re-executes nothing
peer step complete rejects a bad signature, a stale timestamp, an input body that does not match input_hash, a non-required-caller peer, an unknown key, and a suspended peer
step-payload signature domains are disjoint: a call, step-list, and step-complete signature each verify only in their own domain
a step payload signed for another kernel's recipient does not verify here (cross-kernel replay)
the peer step list is scoped in the query: 60 steps in processes the peer owns do not crowd out the one step addressed to it, and results are oldest first
a store failure on the peer step list propagates rather than reading as an empty list
the outbound completion normalizes its input to the bytes the transport sends, so a pretty-printed body's input_hash still verifies at the peer
the outbound completion's idempotency key is derived: a retry reuses it (returning the stored result), while different input or a different step derives a different key
a mid-stream outbound failure is ErrTimeout (may have executed), not ErrPeerUnreachable; only a never-dispatched request is unreachable
a parked remote dispatch completes its inbound idempotency record when the retry loop settles it, and likewise when a forced process closure settles it; a peer replaying the same key then gets the outcome instead of a duplicate-in-flight answer
a remote settlement that FAILED stores an error body, so a replay returns the failure status rather than 200 with a null result
a step-list page cursor neither skips nor duplicates when a step settles between pages, tiebreaks on id when two steps share a created_at instant, and rejects a malformed cursor
admin steps returns the pages already collected, with a warning, when a later page fails; a first-page failure stays an error; an empty page claiming more reports truncated
a completion that failed after committing carries the settled tx/trace/receipt ids in the error's structured metadata, across the federation hop and to the CLI
a park-invariant violation from BeginStepCall is NOT reported as a lost claim (only the two genuine claim races carry that marker)
a settled failure returns its committed transaction to the caller even when post-settlement bookkeeping fails (the caller was charged); a WASM timeout completion is reported as settled, not as a parked dispatch
a crashed federated call to a LOCAL action completes its inbound idempotency record on recovery, not only a remote-proxy one
a settled-failure replay carries the receipt and the settled transaction ids, and a success whose result contains an "error" field still replays as success (the receipt decides, not the body)
a join gate row is dropped once its onward step is terminally resolved, so a late contribution cannot leak a row that nothing removes
admin steps exits non-zero when the listing is incomplete, and returns a next_cursor that --after resumes from
the peer step list carries partial_args and allowed_input and withholds owner_handle, created_by, the target action ref, and every local trace/action id (§5 boundary)
a replayed idempotency record returns the status its stored outcome implies: a settled failure never replays as 200, and the duplicate-in-flight reply carries an error code
admin steps follows the peer's pages until exhausted: 250 parked steps are all returned
admin steps/complete resolve a peer by @handle or key, reject a local (non-peer) account, sign the exact input bytes, fail an offline peer as ErrPeerUnreachable, and propagate the peer's typed error code
@sys/step/race fires exactly once under concurrent contributors (the waiting→running claim is the test-and-set); losers report fired:false; a step in another process is refused
@sys/step/join fires at have >= need with {} and deletes its gate row; a changed need is rejected; an already-resolved onward step reports fired:false and also clears the gate, while a failed fire or an in-flight onward step KEEPS it so a later contribution can retry
a gate refuses an onward step created by a different trace in the SAME process (confused deputy: foreign code funded by the process cannot fire another provider's parked continuation), leaving that step waiting
a gate reports fired:true when it resumed a step whose onward action then failed (the continuation ran; its transaction records the failure), fired:false only when another contributor claimed it, and propagates anything else — classified from CompleteStep's outcome, never from the step's status
a join whose fire fails keeps its gate row at have >= need so a later contribution re-attempts the barrier
CompleteStep reports its outcome: a claim failure carries ErrStepNotClaimed while still presenting code invalid_state and HTTP 409; a rejection before anything settles does not; a failure after a committed transaction returns that transaction alongside the error
step_gates rows hang off their step with ON DELETE CASCADE; deleting an absent gate is a no-op
```

Direct invariant tests:

```text
balances are never negative
successful settlement satisfies taxable = net + fee, with taxable = trace.available at settlement
wallet totals (user, process, trace) change only by run, call entry, settlement, refund, step park/unpark, deposit, withdrawal, transfer, closure
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
two users sign up and one is funded; the funded user transfers credits to the other by @handle;
  balances move by exactly the amount, both see the entry in `user ledger`, and a transfer
  exceeding the sender's balance is rejected with insufficient funds

— Async / steps —
action parks an approval step addressed to a human and returns; process stays open with price parked;
  the human sees it in step list, completes it; fulfillment runs on parked funds; process closes
external system (webhook) registers as a user, a purchase flow pre-creates a step addressed to it,
  the system POSTs the payload to /v1/steps/{id}/complete; transaction obeys role law
owner force-ends a process with waiting steps; steps cancelled, parked prices refunded, balances reconcile
workflow parks one onward step plus several contributor steps into @sys/step/race and @sys/step/join;
  exactly one racer fires the onward step and the rest report fired:false, the join fires only on its
  Nth contribution, and a gate naming a step in another process is refused
kernel restarts mid-flight: interrupted calls fail as interrupted with refunds; waiting steps survive
  and remain completable after restart

— OpenAPI —
API owner imports an OpenAPI document with a stored API key, activates an action, makes it public,
  and a caller executes it through Call(); the key never surfaces
API owner re-runs import against a changed OpenAPI document; the matched action is deactivated,
  stats reset, and historical transactions remain attached
API owner unimports an OpenAPI document; matching actions are deactivated and history remains attached

— Delegated OAuth —
an owner exposes two oauth_delegated actions under one directory sharing one provider app; a user runs
  one and is rejected pre-lock with the structured grant_required outcome naming the action; the client
  drives consent from the directory selector (grants plan/start/complete) against a fake provider with a
  single browser step requesting the union of both actions' scopes; both actions then run, the provider
  seeing the bearer; `user me` shows one connection covering two actions (no token); refresh-token
  rotation on one action's call leaves the other working; provider invalid_grant deletes the connection
  so both next runs re-reject
a user attaches one per-user API key to a delegated_bearer directory via `user connect @owner/path
  --token`, running two actions that share it; the fake upstream sees the token in the configured header
  on both; `user me` shows one connection with two actions and no token; `user disconnect --account`
  revokes the connection so both next runs reject pre-lock with grant_required, the failure suggesting
  the directory selector

— Ratings and reconciliation —
caller executes a paid action multiple times; the action owner lists transactions for their action
  and the sum of transaction net amounts equals the total credits received by the owner
caller rates a transaction with a note; the note and rating value appear in transaction detail and
  list responses for all parties; an unrated transaction returns null for the rating field

— Federation (real transport over loopback; one kernel is the bootstrap+relay) —
two kernels start on 127.0.0.1; the first serves as bootstrap+relay, the second dials it, and they
  reach each other by key alone (no URL); keys resolve through the DHT — no dialable address is configured
A subscribes to B by key; B's operator deposits A's proxy by key (provisioning + funding it);
  A imports B's action; A's user runs it; charge lands in A's proxy balance on B, duty to B's @sys,
  difference refunded; both sides' tx verify passes all checks
call settles over a forced-relay path: the two kernels are denied a direct dial, the call and its
  signed receipt travel through the relay, and settlement is byte-identical to the direct case
B imports a large catalog from A: manifest sync is chunked per action and completes over a
  bandwidth-capped stream
kernel gossips a transacted peer; third kernel reads the gossip over the transport, sees earned stats,
  subscribes to the subject directly by key, imports, runs — its own Stats start at defaults and accumulate
A unsubscribes from B: B's proxies deactivate on A; A re-subscribes and traffic resumes. Separately, B
  suspends A: A's next inbound call to B gets a signed rejection receipt; unsuspend restores it
inbound call from an underfunded peer yields a signed rejection receipt the caller settles on
caller runs a NAT-bound peer's action, the peer goes offline mid-call; the caller's allocation stays
  locked and the process stays open until the peer returns and a signed receipt settles it (no timeout settle)
A parks a step addressed to B (via @sys/message to B's key); B lists it with `admin steps`, sees its
  derived allowed_input, and completes it with `admin complete`; the step settles on A, a second
  completion is refused, and a suspended B is refused until unsuspended

— Federation (real-network release gate; excluded from `go test ./...`) —
from a machine behind a real NAT, subscribe to a remote peer by key, call it both directions with the path
  hole-punched (asserted via `admin inspect`), then force a relay fallback and a restart-mid-call recovery
```

## 16. Design rationale

Stable kernel interfaces and explicit transitions keep correctness independent of transports and adapters. Small function-named packages and per-file tests reduce coupling, keep replaceable implementations visible, and expose coverage gaps. Execution and supervision are separated because execution may err while supervision supplies correction signals execution must not manipulate.

Locking the full subtree price before execution prevents unfunded work anywhere in the tree; failure refunds of the remaining allocation keep accounting conservative, observable, and testable. Input-before-lock and output-before-settlement prevent charging invalid requests or paying malformed replies. Mediated WASM authority permits composition without credential leakage or authorization bypass.

Visibility is caller-scoped, not process-owner-scoped, because access and spend are different questions. The process owner's interests are already protected by the mechanisms that own them — spending authority (§4 precondition 4), the subtree price bound, and the grant binding (§8, which correctly stays with the paying human) — so scoping *access* to the owner too would only misplace it: it would break provider encapsulation (a sold composite could not use its author's private internals) and open a confused deputy (foreign code the owner funds could reach the owner's private actions). Scoping access to the immediate caller mirrors lexical visibility in a programming language and makes both failures impossible with a single correctly-placed variable. The three levels — `private`, `local`, `public` — separate the two consents that a boolean conflated: `local` publishes to a kernel's own users (the operator has already consented by running them together), while `public` additionally exports across federation (the owner's explicit consent), so an imported proxy held `local` is unreachable by a further peer at the call layer, not merely absent from manifests, and subscription is non-transitive by construction.

Subtree pricing makes a price a price: the caller pays one advertised number, composition risk lives with the provider who composed, and the fee taxes each layer's margin (value added), not gross flows. `run` removes process bookkeeping from the user — funding is exact, closure automatic, a process simply the lifetime of a computation and its continuations. Steps are funded continuations: money reserved at suspension makes asynchronous composition safe, restartable, and honest about who pays.

Federation incentives align in both directions: a kernel federates because its users gain a larger action space and its providers gain outside demand, its operator earning the import duty. The domestic fee (default 20%) deliberately exceeds the import duty (default 5%), so a kernel always earns more on local supply than on imports — federation complements local providers rather than undercutting them. Discipline is self-enforcing: a kernel whose counterparty account runs dry stops serving it manifests, since executing unpaid work loses money twice — in service and in the failure stats that sink its rank abroad; funding restores exposure. Because gossip carries only trade-backed opinions, reliable behavior compounds into discoverability: reputation is the long-run asset earned by settling honestly.

A single peer-to-peer carrier expresses a system whose identity was always a key, never an address: one transport means one code path, one failure mode, and one thing to verify against §13, and it lets a home-router kernel federate identically to one on a public host. HTTP federation was the lone layer dragging advertised addresses, `.well-known` documents, and peer-URL SSRF rules into a key-native system; removing it deletes a whole class of configuration rather than maintaining a parallel path. The honest cost: a small class of public helper nodes (bootstrap, rendezvous, relay) is load-bearing infrastructure for all federation — they pass encrypted bytes and hold no Juice data, but someone must run them, unpaid and out-of-protocol for now. Settlement never knew the carrier, so moving it onto the transport changes how bytes arrive and nothing about who pays or what settles.

Replaceable lookup ranking permits research changes without changing kernel semantics; fixed stats plus optional tags preserve deterministic baseline metrics while isolating experiments. Latency is derived from transaction timestamps, not cached on traces, so buyer-experienced wall time is always current — a subtree query reflects late descendants without a retroactive write — at the cost of that query at read time.

Fixed `@sys` and signing keys give stable system action names and verifiable receipts/manifests. Structured logs make production operation and research reproduction reconstructable.

OpenAPI import as supervision keeps registration low-friction while preserving uniform execution. Federation imports signed action contracts without leaking implementation. Contract-change deactivation prevents silent interface drift for callers and LLMs. Unimport deactivates rather than deletes so history remains auditable. Subscription is deliberately worthless (permissionless, zero balance) so that trust lives in deposits and reputational gossip carries only earned, trade-backed opinions; a Bayesian shrinkage of ranking priors toward trust-weighted introducer means is the natural baseline formula, but the formula is the ranking layer's experiment, not kernel semantics.