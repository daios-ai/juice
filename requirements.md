# Juice Kernel Requirements

Version: 0.12
Status: implementation requirements
Codename: `juice`

INSTRUCTIONS: THIS FILE CONSTAINS THE JUICE REQUIREMENTS.
BEFORE ADDING ANYTHING, ALWAYS CHECK WHETHER THE EXISTING TEXT CAN BE REWRITTEN.
TONE MUST BE TERSE AND ALWAYS MINIMAL.

## 1. Execution, supervision, and role law

Juice is a Go production kernel and research platform for callable actions. Execution starts with:

```text
run(action, args)        // action is owner/name
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

All IDs are stable opaque identifiers. Action IDs are globally unique. Prices are non-negative indivisible integers. Balances are indivisible integers; an **ordinary** account's `available` is non-negative, while a **peer** account's `available` may be negative — the bilateral position, negative when the peer owes this kernel (§13). Peer debt is bounded not per-peer but by the kernel's **global exposure cap** `X`, enforced at admission (§13): the sum of all peers' debts stays ≤ `X`. In the database, `CHECK (public_key IS NOT NULL OR available >= 0)` keeps ordinary accounts non-negative and leaves peer rows row-unbounded (the global bound lives in the admission op). `locked` is always non-negative.

| Object              | Fields                                                                                                                                                                                                                                                                           | Rules                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Account`           | `id`, `handle`, `description`, `available`, `locked`, `suspended_at`, `kernel_public_key`, `recovery_public_key`, `created_at`, `updated_at`                                                                                         | The local financial, authentication, and moderation principal: every transaction party is captured under an account id. A **user** account holds a `handle` (unique, bare — no sigil, no `@` or `/`, never matching the id or public-key form, §14) plus password and/or `recovery_public_key` credentials. A **kernel** account instead holds `kernel_public_key`, a foreign key to `Kernel` (§13) — the sole account↔kernel relationship and the peer discriminator: `peer(C) := C.kernel_public_key ≠ null`. The two are disjoint by CHECK: a kernel account has no handle, password, or recovery key, so a peer can never hold a session credential; it is named by its kernel's petname (§13). `description` is a free-text "about" (`sys`'s is the kernel's own advertised about). A suspended account cannot act: rejected at every authenticated request with `ErrUnauthenticated`, and a suspended kernel's inbound federation calls are refused (§13) — `suspended_at` is the single moderation axis for humans and kernels alike. An ordinary account's `available` is non-negative (`CHECK (kernel_public_key IS NOT NULL OR available >= 0)`); a kernel account's may go negative — the bilateral position, bounded not per-peer but by the kernel-global exposure cap `X` at admission (§13). `locked` is always non-negative. `recovery_public_key` is a *recovery* credential (§12), never an authentication credential and never a federation identity. `user create` makes a password account and enrolls a recovery key from a client-held seed phrase (§12); a peer's first call or a deposit makes a kernel account (§13). An account may also hold `Grant` rows (§8). A kernel account is the **financial counterparty only**, never the semantic owner/caller of a remote action — that is `PrincipalID` (§13). |
| `Action`            | `id`, `owner_user_id`, `name`, `kind`, `active`, `visibility`, `price`, `effect`, `description`, `input_schema`, `output_schema`, `source`, `auth_json`, `artifact_hash`, `remote_action_id`, `remote_owner_id`, `remote_bps`, `base_price`, `created_at`, `updated_at`                                                                       | `owner_user_id` is the action owner. `effect`, when set (`"transfer"`), names a privileged execution effect the action commits deferredly at settlement (§13 value transfer): a signed manifest contract field so value-bearing is decided by contract, never the action name; empty on every ordinary action. `kind ∈ {http, wasm, native, remote_proxy}`. `visibility ∈ {private, local, public}` is the caller-scoped callability scope (§4): `private` = owner only; `local` = any local (non-peer) caller, never served to peers; `public` = anyone, and the only value served in manifests/gossip (§13). Default `private`; created private and widened only by update. `(owner_user_id,name)` unique. `/` is allowed in `name`; handles cannot contain `/`, so `owner/name` is unambiguous. Inactive actions are not callable. Listing visibility per §14 (`GET /v1/actions`): owners may list all their own regardless of `active`/`visibility` via `?owner=` self-match. Authorized users may inspect script source. `artifact_hash` content-addresses compiled artifacts. `auth_json` is the write-only upstream credential config, encrypted at rest, never returned by any read path (§8); reads expose only its non-secret summary — `auth_scheme` and a `requires_grant` flag. For `remote_proxy` (a **transitional cache row**, §8): `remote_action_id` is the action ID on the remote kernel and the reference dispatched on the wire (§13), `remote_owner_id` the owner's **stable** id there (with the peer's `public_key` this is the semantic `PrincipalID`, §13 — the kernel account is only the accounting counterparty, never the semantic owner), `base_price` the seller's manifest price, which the local price re-derives from at read so an `import_bps` change reprices with no re-resolve (nullable; an older row heals by re-resolving), `remote_bps` the provider premium snapshot from the signed manifest (nullable; refreshed on next resolve), and `artifact_hash` the signed manifest hash; the peer is the proxy owner's `public_key`, transport-resolved to a live path (§13), so no URL is stored in `source`. Active actions require non-empty natural-language `description`, valid schemas, and field descriptions sufficient for lookup and LLM function calling. |
| `Process`           | `id`, `owner_user_id`, `available`, `locked`, `status`, `created_at`, `ended_at`                                                                                                                                                                                                 | `owner_user_id` is the process owner and payer. `status ∈ {open,closed}`. Created by `run`, funded with exactly the root action's price parked from the owner's `available` into `locked` (§6); the process holds it as `available`. Bijective with its root trace; it is a longer-lived wallet only because traces settle eagerly (§6): it absorbs refunds destined for already-settled traces and holds parked steps. Enforcement is per call, on the call's trace (§6); `available + locked` is the total held across its calls' wallets and parked steps. Closes automatically when the root call has returned and no Steps are outstanding, returning remaining funds to the owner and releasing the owner's lock. Closed processes cannot call.                                                                                                                                                                              |
| `Trace`             | `id`, `process_id`, `parent_trace_id`, `action_owner_id`, `available`, `locked`, `idempotency_key`, `dispatch_json`, `premium_bps`, `premium_parked`, `value`, `value_to`, `value_reserve`, `created_at`                                                                                                                                  | `action_owner_id` is the owner of the action executing in the trace, used for trace-scoped process authority. `premium_bps`/`premium_parked` snapshot the serving markup admitted for an inbound federated root call (§13): the rate the receipt levies on the actual charge, and the execution reserve parked in the owner's `locked` at admission. `value`/`value_to`/`value_reserve` snapshot a value transfer's `TransferEffect` (§13): the delivered amount, the resolved local beneficiary (empty for an outbound/remote destination), and the total reserve locked from the immediate caller `C`'s own `available` at admission (value + value fees) — released to the beneficiary/peer + `sys` + refund at settlement, or refunded on failure. Sourced from `C`, not the trace budget, so the value is delivered untaxed and separate from the execution channel. They ride on the trace so **every** settlement path — commit, failure, crash recovery, forced closure, max-age — releases the reserve without the in-memory request and pins the rate against a mid-call config change; all are 0 on local calls and subcalls. `process_id` is denormalized (derivable by walking `parent_trace_id` to the root). Root traces have null parent. Every `Call()` creates exactly one child trace. A trace is the call's wallet: `available` starts as the action's price at entry (the call's remaining allocation); `locked` is what the call has committed to direct subcalls and steps. Calling price `q` requires `available ≥ q` and moves `q` from `available` into `locked`, becoming the callee's `available` (§6). Settlement pays out the trace's remaining `available` (§6). Own latency is `transaction.ended_at − transaction.started_at`; no cached latency field (§11). `idempotency_key`/`dispatch_json` are null except on a remote-proxy trace, where the outbound key and request payload are recorded atomically with dispatch; `idempotency_record_id` is the *inbound* cross-kernel record the trace serves when the call is answering a peer (§13) — set for every action kind, so whichever settlement finally resolves the trace (commit, dispatch retry, max-age bound, forced closure, or crash recovery) completes that record; while set and unsettled, the call is awaiting its receipt and restart resumes its retry (§5, §13). |
| `Transaction`       | `id`, `process_id`, `trace_id`, `parent_trace_id`, `owner_user_id`, `caller_user_id`, `target_user_id`, `action_id`, `action_name`, `args_json`, `reply_json`, `status`, `gross`, `net`, `fee`, `reason`, `remote_receipt_hash`, `remote_receipt_json`, `started_at`, `ended_at` | `status ∈ {success,failure}`. Every attempted call creates one immutable transaction. Fields obey the role law. `action_name` is captured at creation so history remains self-contained after action deletion. Local calls have null remote receipt fields. Remote-proxy commits atomically store full remote receipt JSON and `SHA-256(remote_receipt_json)`.                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `Stats`             | `uses`, `successes`, `failures`, `rating_count`, `latency_estimate`, `rating_estimate`, `last_used_at`                                                                                                                                                                           | Missing stats have defined defaults. `uses = successes + failures`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `DiscoveryDoc`      | `kernel_public_key`, `kind`, `user_id`, `handle`, `description`, `action_id`, `name`, `input_schema`, `output_schema`, `serving_price`, `embed_vec`, `observed_at`                                                                                                                                 | A regenerable, searchable summary of a first-party user (`kind=user`) or action (`kind=action`) learned from gossip (§13), indexed by the lookup machinery. Carries no execution semantics and is truncatable with zero effect (a selection still resolves and verifies from the home kernel). `serving_price` is the verified manifest's `mp + ceil(mp·remote_bps/10000)`; the local all-in price adds `import_bps` at read time, so it is indicative and resolve re-quotes it. Purged with the peer. |
| `EvidenceRow`       | `issuer_public_key`, `receipt_hash`, `subject_kernel_public_key`, `subject_action_id`, `counterparty_kernel_public_key`, `evidence_receipt_json`, `rating_json`, `remote_receipt_hash`, `receipt_created_at`, `effective_at`, `observed_at`, `equivocated`                          | A verified signed evidence record about a subject action (§13), keyed by `(issuer_public_key, receipt_hash)`. `issuer` is the gossiping kernel; `subject` is the executed action's stable identity. `equivocated` marks two different valid ratings under one key (both then excluded from derived metrics). Regenerable, truncatable, purged with the peer. |
| `Step`              | `id`, `parent_trace_id`, `required_caller_user_id`, `required_caller_remote_id`, `action_id`, `price`, `import_bps`, `partial_args`, `status`, `tx_id`, `created_at`                                                       | `status ∈ {waiting, running, done, cancelled}`. `parent_trace_id` is the creating/funding trace, derives the process (`Trace(parent_trace_id).process_id`), and is inherited by the completion trace. `required_caller_user_id` is mandatory; open completion is unsupported. `required_caller_remote_id` is nullable: when set, the required caller is a **remote principal** — `required_caller_user_id` is the peer's account (the local accounting/routing account) and `required_caller_remote_id` is the completer's **stable** `user_id` on that peer kernel; completion then demands a home-kernel `step_auth` attestation naming that id (§10, §13), so a remote handle rename never mis-addresses a parked step. `partial_args` is pre-bound input merged with caller input at completion (`input` overwrites `partial_args` on key collision). The completer's allowed input is derived `action.input_schema \ keys(partial_args)`, not stored (§10) — safe because a schema change deactivates the action and completing against a changed/deactivated action resets the step to `waiting` (§10). `tx_id` is recorded atomically at `done`. `import_bps` freezes the origin fee a remote-proxy step was funded under, which may settle long after policy changes (nullable). `price` is `action.price` snapshotted at creation: the amount parked in the process's `locked`, the completion call's allocation and `gross`, spent on completion and refunded on cancellation. `EndProcess` atomically cancels all `waiting` steps of the process in the same transaction as closure; `cancelled` is terminal with no `tx_id`. An outstanding (`waiting`/`running`) step keeps its process open, allocation parked (§10). |
| `LedgerEntry`       | `id`, `operator_user_id`, `from_user_id`, `to_user_id`, `amount`, `reason`, `external_key`, `created_at`                                                                                                                                                                                                     | Immutable audit record of one direct balance movement, source and destination each nullable: a deposit credits (`from_user_id` null, `to_user_id` set), a withdrawal debits (`from_user_id` set, `to_user_id` null), a user transfer moves between two local users (both set, §12). At least one of `from_user_id`/`to_user_id` is non-null. `operator_user_id` is the authorizer — `sys` for a deposit/withdrawal, the sender for a transfer. `amount` is positive; the debit requires the `from` user's `available ≥ amount`. `external_key` is an optional opaque idempotency token; when present it is globally unique, and a create with an existing `external_key` returns the existing record without reapplying the balance change. Juice never interprets it, keeping the kernel payment-rail agnostic.                                                                                                                                                                                                                                                                                                                                                              |
| `Grant`             | `id`, `grantor_user_id`, `action_id`, `connection_id`, `created_at` | A user's per-action delegated consent (§8): a pointer binding one action to the upstream account (`connection_id`, a `Connection`) whose credential it may wield. Holds no token. Unique per `(grantor_user_id, action_id)`; re-consent overwrites in place. Deleted on revoke, on `Connection` deletion (cascade), on provider `invalid_grant` (which deletes the `Connection` and cascades), and when a deactivating update / auth replacement / delete invalidates the action (§8) — invalidation deletes grants only, never the `Connection` (the upstream account outlives any one action's consent). A consent record, not an authentication credential. |
| `Connection`        | `id`, `user_id`, `provider_key`, `sealed_secret`, `scopes_json`, `created_at`, `updated_at` | A user's upstream account credential, stored once and shared by every `Grant` pointing at it (§8). Unique per `(user_id, provider_key)`. `provider_key` is derived from verified facts, never names: `bearer:<host>` from a `delegated_bearer` action's pinned `source` base-URL host; `oauth:<token_url>|<client_id>|<source-domain>` from an `oauth_delegated` action's auth config, `source-domain` being the registrable domain (eTLD+1) of the action's `source` host — binding the token to its resource server (§8 confused-deputy defense). `sealed_secret` (the OAuth refresh token or static bearer/API-key token) is AES-256-GCM encrypted with AAD `user_id|connection_id`, write-only, never returned by any read path. `scopes_json` is the requested-scope union consented so far (providers need not echo granted scopes, so requested scope is authoritative for coverage). Provider refresh-token rotation updates this one row, leaving dependent grants unaffected; provider `invalid_grant` deletes it and cascades its grants. A zero-grant `Connection` is kept (re-consent reuses it), surfaced `unused` in read paths (§14), never auto-expired. |
| `Receipt`           | `id`, `issuer_user_id`, `tx_id`, `trace_id`, `action_id`, `caller_user_id`, `process_id`, `args_hash`, `reply_hash`, `status`, `gross`, `net`, `fee`, `charge`, `premium`, `value`, `value_premium`, `value_to`, `reason`, `started_at`, `created_at`, `signature`                                                                | Immutable signed record for exactly one committed call. `caller_user_id` is the call caller. `started_at` is call start; `created_at` is settlement. `charge` is the amount actually drawn from the caller's funds: `= gross` on success, `≤ gross` on failure (settled descendants stay paid, §6), `0` on rejection. `premium` is the execution serving markup `ceil(charge·remote_bps/10000)` (§13). The value-transfer channel is kept distinct from the execution channel (§13): `value` is the delivered amount (all-or-nothing, `∈ {0, amount}`), `value_premium = ceil(value·remote_bps/10000)` its serving markup, and `value_to` the resolved beneficiary the origin binds; all 0/empty on a non-transfer receipt (JCS `omitempty` keeps pre-transfer receipts verifying). |
| `Rating`            | `id`, `rated_tx_id`, `rated_receipt_id`, `rated_receipt_hash`, `rater_user_id`, `rating`, `note`, `created_at`, `signature`                                                                                                                                                       | Immutable signed feedback record. `rating ∈ {0,1}`. At most one rating exists per transaction. `rated_receipt_id` may be null only for pre-receipt transactions. `rated_receipt_hash` is `SHA-256(CanonicalJSON(rated receipt))` — the portable link an evidence bundle carries so a receiver joins the rating to its receipt (§13); a rating with no linkable receipt has an empty `rated_receipt_hash`, feeds local `Stats`, and is never gossip-eligible. `note` (≤ 1024 bytes) is optional, nullable, human-readable, and included in the single Ed25519 rating signature payload.                                                                                                                                                                                                                                                                     |
| `IdempotencyRecord` | `id`, `idempotency_key`, `counterparty_user_id`, `receipt_id`, `status`, `result_json`, `created_at`, `expires_at`                                                                                                                                                               | Cross-kernel only. `status ∈ {pending,complete}`. Insert pending before execution; complete atomically with transaction and receipt. Completion stores `result_json`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `Kernel`            | `public_key`, `petname`, `nickname`, `about`, `gossip_cursor`, `last_seen`, `peer_credit`, `first_seen`, `updated_at`                                                                                          | One row per known remote kernel, whether or not it holds an account here. It owns the naming state — `public_key`, `nickname`, `petname` (§13) — plus `about`, the kernel's self-description. `gossip_cursor` is the evidence high-watermark, advanced only after a page is verified and committed; `last_seen`/`peer_credit` are the peer-sync display cache, written only after a successful authenticated sync. Neither is execution semantics, callability, pricing, or settlement. Location is never stored. Discovery creates no account, so peering stays worthless. |

OpenAPI registration and federation create or update ordinary `Action` rows. They create no durable object parallel to `Action`.

## 4. Authorization, call validity, and traces

Every action has a `visibility ∈ {private, local, public}` (§3). `private` is callable only by its owner; `local` by any local (non-peer) caller of this kernel but never by a kernel account; `public` by anyone including peers, and only `public` actions are served in manifests and gossip (§13).

```text
Live(a)       := active(a) ∧ ¬suspended(a.owner_user_id)
Visible(C, a) := public(a) ∨ (local(a) ∧ ¬peer(C)) ∨ C = a.owner_user_id
CanCall(C, a) := Live(a) ∧ Visible(C, a)
```

Visibility is checked where an action reference is **bound**: a call names its target (checked against the immediate caller `C`), and a step's creator names its target at creation (checked against the creating trace's action owner, §10). Liveness is checked at every dispatch. A step completion binds no name — it resumes an already-bound reference — so it re-checks only `Live(a)`: deactivation or a suspended owner still resets it to waiting (§10), but a later visibility change never bricks a parked step. `CanCall` is scoped to the immediate call caller `C`, not the process owner `P` — visibility is a property of the code that directly invokes the action, exactly as lexical visibility governs a function call in a programming language, and a step is a closure that captures its target in the creator's scope. This makes a provider's `public` action able to subcall the provider's own `private` helpers in anyone's process, while foreign code a process owner funds cannot reach that owner's `private` actions (no confused deputy). For a root call `C = P`, so user-facing behavior is unchanged. `peer(C)` holds when the caller is a kernel account (a set `kernel_public_key`, §13); an inbound federation call is a root call by that peer, so it reaches `public` actions only — never `local` ones, which is what keeps an imported proxy (held `local`, §8) unreachable across a second hop. An action whose owner is suspended fails `CanCall` regardless of visibility: a suspended owner's actions are not callable and are excluded from action listings (§14); unsuspending restores them, since suspension preserves data (§12). Encapsulation controls the direct dependency surface, not reachability of effects: a local caller may always wrap a `local` (or `private`) action inside a `public` one and export the result, taking on the wrapper's margin and rating risk — the same freedom a programming language grants a public function over a private one.

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
7. quote pin, if supplied, matches the action's current `quote_hash`
8. args satisfy action.input_schema
9. funds: the passed trace's `available` >= action.price (§6) — for a root call this is the root trace `run` funded from the process; a step completion is funded by its parked price instead (§10)
```

Precondition 4 controls spending authority over the process (the process owner `P`). Precondition 6 controls action access by the immediate caller `C`. These are distinct questions: `P` decides whose funds may be spent, `C` decides whose code may reach the action.

`quote_hash` = SHA-256(JCS(`{action_id, effect, description, input_schema, output_schema, price}`)) over the **stable** id — a proxy's remote id, so a discovered hit and the proxy it resolves to hash identically (§13) and a hash read from lookup binds a first cross-kernel call. Action reads, listings, and `sys/lookup` carry it; a root run may pin it, and a mismatch is refused with `ErrTermsChanged` carrying the current hash and price in `meta` — its own code, since the action is callable and only its terms moved. `effect` is included because it alone decides whether a call engages the value channel (§13). Its position — after visibility, before schema validation — is why a mismatch never discloses a private action's terms and why terms that invalidate the args still report as changed terms. It binds the execution quote, not the implementation, and not a transfer total whose value fees are read live at funding.

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
  CommitRemoteSettlement  remote-proxy settlement on a signed receipt (charge/premium/refund) + transaction + receipt (+ step/idempotency)
  EndProcess          cancel waiting steps + return funds + close process
  CreateStep          step record + park step.price from the creating trace
  CreateLedgerEntry   direct balance move (deposit/withdrawal/transfer): debit from + credit to + ledger record
  CreateRatingAndUpdateStats  rating record + rating stats
Reads / supervision (no monetary mutation):
  CreateUser ReadUser ReadUserByHandle ReadAccountByKernelKey ListUsers SuspendUser UnsuspendUser UpdateUser RenameUser
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
  UpsertKernel BindPetname ListKernels ReadKernel ReadKernelByPetname SuspendKernelAccount
  DeactivateActionsOwnedBy
  CreateOrReplaceGrant ReadGrant ListGrantsByUser DeleteGrant DeleteGrantsForAction
  CreateOrUpdateConnection ReadConnection ReadConnectionByUserProvider ListConnectionsByUser UpdateConnectionSecret DeleteConnectionCascade
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

`target_user_id` is credited `net`; `sys` is credited `fee`. Unused budget is the provider's margin, not a refund: `price` is a price, not a metered estimate. Negative value added is structurally impossible (`available ≥ 0`). Default `fee_bps = 2000`. Fee recipient is fixed as `sys`. Each kernel taxes only its own layer; remote subcalls are subject to the remote kernel's fee policy independently. Exception: `kind = remote_proxy` settles per §13's receipt rule; all other kinds settle as above.

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

`RegisterNativeAction`: bootstrap-only; caller is responsible for `sys` ownership.

`Activate`: action-owner authority; initialize stats if absent; reject invalid schema, missing source, invalid artifact, unsafe URL, or invalid runtime.

`Update`: action-owner authority; changing source, schema, kind, price, or endpoint deactivates unless explicitly safe; recompute WASM `artifact_hash`; reject unsafe HTTP source URLs; preserve historical transaction source/hash.

`Delete`: action-owner authority; disable discovery and preserve history; soft deletion permitted.

Native actions are bootstrap-registered, owned by `sys`, not user-creatable/updatable/deletable, and callable only through `Call()`.

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

**Connections, selectors, and reconcile.** `provider_key` is derived from the action's pinned, SSRF-validated facts (§3, §7), never from action names or descriptions, so no action can name its way into another's credential. The `oauth_delegated` key includes the resource-server domain because an OAuth `client_id` is public: without it a hostile action reusing a legitimate `token_url|client_id` under an attacker-controlled `source` could ride a victim's existing connection and have the minted access token delivered to the attacker (confused deputy); a different resource-server domain is therefore a distinct connection requiring its own consent, and the consent plan surfaces each group's destination host(s) (§14) so the recipient of a delegated credential is never hidden. Consent is planned per upstream account. A **selector** — `owner` or `owner/path`, matched by path segment (`tom/brief` matches an action named `brief` or `brief/…`, never `briefing`; a full `owner/name` is the degenerate one-action selector; a trailing `/*` is stripped) — expands to the delegated actions the caller may call (`CanCall`, §4), grouped by `provider_key`. One `delegated_bearer` token paste or one browser consent requesting the **union** of a group's `scopes` covers the whole group at once, minting one `Grant` per action against the single `Connection`; connecting an action whose provider `Connection` already covers the scopes grants instantly, no browser round-trip. Re-consent for a wider group stores the union of the stored and newly requested scopes, so earlier grants never lose coverage. Every connect is thus a **reconcile**: it compares the selection to existing coverage and acts only on the delta (the client must display that delta — the actions to be connected — as the consent act, §14).

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

**Calling resolves on use; there is no subscription.** A call to a kernel-qualified action (`alice@B/foo`, §13 grammar) whose cached proxy row is absent *or inactive* resolves that one action's signed manifest over `/juice/fed/resolve/1`, verifies it, and creates or refreshes a `kind=remote_proxy` **cache row** owned by the local peer user — the sole cache-fill path. Reconciliation is by match key, so `Action.id` is preserved and the row re-enabled; an inactive proxy is never permanently dead. The row is `visibility = local` — callable by this kernel's own users, never re-served to a further peer (a peer naming it fails `CanCall`'s `local` branch, §4) — so federation stays non-transitive. The proxy is a **cache** reconstructible from `(peer_key, owner_id, remote_action_id, contract)`; ownership, attribution, and reputation key on `PrincipalID = (peer_key, owner_id)`, never the kernel account. Its `active` bit is kernel-managed cache state: set by resolve, cleared by a `refresh_proxy` rejection or receipt quarantine (§13), removed only by peer retention; the whole row is kernel-managed, so manual `enable`/`disable`, `update`, and `delete` are all rejected (`ErrInvalidState`), the durable peer lever being `suspend` alone.

Manifest required fields:

```text
action_id owner_id owner_handle name description input_schema output_schema price remote_bps kind
artifact_hash stats updated_at signature
```

`owner_id` is the action owner's **stable** id on the serving kernel (the identity half of `PrincipalID`); `owner_handle` is that owner's current *display* handle there (mutable metadata, never identity). Only active public remote actions have manifests; an action using a delegated auth scheme (`oauth_delegated` or `delegated_bearer`, §8) is never served as a manifest and never gossiped, because a remote peer's single kernel account can never complete a browser consent nor hold a per-caller token. A kernel serves manifests and gossips (as its own exposed actions) only actions it owns — `kind ∈ {http, wasm, native}`; an imported `kind=remote_proxy` action is never re-served, so federation stays non-transitive: reaching a peer's imported action requires resolving it from its true owner directly. Manifest descriptions and schemas are the canonical interface used by importing kernels for lookup and LLM function calling. `signature` is the remote platform Ed25519 signature over canonical JSON excluding `signature`, verified against the remote peer's `public_key`. Match key (stable-identity based, so a remote handle rename never re-keys the cache):

```text
proxy.owner_user_id + proxy.remote_owner_id + proxy.remote_action_id
```

Remote contract fields:

```text
action_id artifact_hash description effect input_schema kind name output_schema owner_id price remote_bps
```

`effect` is the signed privileged-execution-effect field (`"transfer"`, or empty): the origin decides a remote action is value-bearing from this signed contract field, never a name coincidence (§13), so a proxy imported without `effect = "transfer"` is never treated as a transfer and tampering `effect` breaks the manifest hash. `owner_handle` is **not** a contract field — a rename on the remote updates display only and never deactivates the proxy. Manifest stats and `updated_at` do not affect contract comparison; manifest stats never overwrite local `Stats`. An invalid signature, a negative `price`, or a `remote_bps` outside `[0,10000]` skips that action. Re-resolve reconciles by match key: an unchanged contract preserves the row (id, stats), a changed one updates it in place (id preserved, current stats reset), and every successful resolve (re-)enables it. A changed or withdrawn proxy is healed per-call by the `refresh_proxy` invalidation above, not by bulk re-sync.

The proxy's local `price` combines two markups on the base price `mp` (§13 pricing): the **serving kernel's** advertised remote markup (`remote_bps`, a signed manifest field) and the **origin kernel's** import fee (`import_bps`, local policy). `sr = mp + ceil(mp·remote_bps/10000)` is what the origin owes the serving kernel; `price = sr + ceil(sr·import_bps/10000)` is what the local user pays — one authenticated, deterministic price bounding the whole call. A `remote_bps` change re-syncs on the next resolve; changing local `import_bps` re-prices proxies **without** a manifest change. Settlement charges each markup on the actual amount and refunds the difference (§13).

## 9. Adapters, native actions, and stats

WASM uses wazero. Scripts receive no ambient filesystem, network, environment, process access, or raw user tokens. They receive only explicit host functions, each execution having memory limit, timeout, deterministic context cancellation, and artifact-hash compiled-module cache. The network is reachable only mediated, never ambient: a script holds no sockets and reaches the web solely by calling the read-only, SSRF-restricted `sys/web` action (or a provider's pinned `kind=http` action) through `Call()`, charged like any other call. Store source and artifact; authorized users may inspect source; activation should precompile; compilation failures are typed.

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

Native actions are standard actions shipped alongside the kernel as a platform stdlib. They have no special kernel privileges — any provider could have supplied equivalent actions as HTTP or WASM actions. They are registered at bootstrap under `sys`, interact with the platform only through injected dependencies and the same `Call()` / `CreateStep()` / `CompleteStep()` entry points available to all actions, and never extend the kernel's internal interfaces on their own behalf.

The stdlib is `local` (§4): every native is callable by this kernel's own users and by no peer, so it appears in no manifest or gossip catalog (§13). It is identical on every kernel, so serving it abroad would duplicate the whole stdlib in every peer's discovery cache while offering a buyer nothing its own kernel lacks; and it is backed by scarce local resources — the configured model, egress bandwidth, the compiler toolchain, a write into a local user's step list — which at the default price 0 no admission rule bounds, a free call adding no exposure (§13). A kernel with capacity to sell exposes it as its own `public` action, priced and rated under its own name.

| Action          | Rules                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sys/lookup`   | Local; action owner `sys`; price 0 (configurable, `native.lookup`, §14); callable only through `Call()`. Rank active actions by a tested formula combining lexical (BM25) and semantic (cosine) relevance — fused by reciprocal-rank fusion. **Stats-based quality weighting is UNDER REVISION and temporarily removed** (the multiplier could bury an exact match beneath weakly-relevant ones); ranking is by fused relevance alone until the redesign lands (see `ranking.md`). The embedder is optional: with none configured, ranking degrades to the lexical leg alone, so lookup still works on a kernel with no LLM. Replaceable ranking storage; brute-force cosine acceptable. For an authenticated **local** caller (a session user, not a peer/anonymous), ranking also fuses in **discovery docs** (§13) as a third RRF leg, so `sys/lookup` surfaces discovered-but-unresolved remote actions; a discovered hit renders `action_id` = the remote action id and `action` = `owner@<raw-kernel-key>/name` (the stable identity, unchanged across resolution; a gossiped label never resolves), and a locally-imported proxy for the same remote action shadows its discovery row. Input: required `query`, optional `limit=10`. Output: `results[]` with `action_id`, `action` (`owner/name` or `owner@kernel/name`), `description`, `price`, `score`, `input_schema`, `output_schema`, `quote_hash` (§4). `price` is the all-in local price: `action.price`, or a discovered hit's `serving_price` marked up by current `import_bps` (§13), indicative until resolve re-quotes it. Direct lookup only for diagnostics, not user-facing APIs or WASM hosts.                                                                                                                                                                                                                                                                                                       |
| `sys/user-lookup` | Local; action owner `sys`; price 0 (configurable, `native.user-lookup`, §14); callable through `Call()`. The user-facing twin of `sys/lookup`: it searches local principals (`sys` plus the owners of active public actions) and, for an authenticated local caller, discovered users (§13), by the same lexical + optional-semantic RRF. Input: required `query`, optional `limit=10`. Output: `results[]` with `principal_id` (`{kernel_public_key, user_id}` — the stable identity, §13), `reference` (`handle@<kernel-key>` for a discovered user, a bare handle for a local one), `handle`, `description`, `kernel_public_key`, `score`. Keeping actions and users on separate query surfaces preserves typed results and lets `sys/llm/decide` consume action references only. |
| `sys/llm/chat` | Local; action owner `sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Input: `messages[]` of `{role,content}` plus optional `system`. Output: `message{role,content}`. `ErrInvalidState` if chat unconfigured.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `sys/llm/embed` | Local; action owner `sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Input: required `text` (string). Output: `embedding` (array of numbers). `ErrInvalidInput` if `text` is empty. `ErrInvalidState` if embedder unconfigured. |
| `sys/llm/json` | Local; action owner `sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Input: `messages[]` of `{role,content}`, optional `system`, required `output_schema`. Output: `value` (JSON value). Validates model output locally against `output_schema`. `ErrSchemaViolation` for unsupported schema. `ErrInvalidState` if structured output unavailable. `ErrExecutionFailed` if no valid JSON produced. |
| `sys/llm/decide` | Local; action owner `sys`; price 0 (configurable, `native.llm`, §14); callable through `Call()`. Given a conversation and a set of Juice actions, asks the LLM to select one and propose args — does not execute the call. Input: `messages[]` of `{role, content?, tool?}` where `role` is one of `system`, `user`, `assistant`, `tool` and `tool` is an optional object `{action?, args?, result?}`; required `actions[]` (list of `owner/name`). Kernel fetches each action's canonical description, `input_schema`, and price from the DB. Output: `action` (`owner/name` or `owner@kernel/name`), `args` (validated against that action's `input_schema`), optional `message`. A **kernel-qualified** candidate (a discovered remote action, §13) is resolved through the same resolve-and-cache path as `run`, so the workflow is `lookup → decide → run`; a stale/unresolvable qualified candidate is **discarded** so one dead peer never blocks selection among the valid ones, and `ErrNotFound` is returned only when *no* candidate resolves. A bare local reference keeps strict behavior (an unknown one is `ErrNotFound`). `ErrInvalidState` if LLM tool calling unavailable. `ErrExecutionFailed` if no valid selection produced. |
| `sys/time`     | Local; action owner `sys`; price 0 (configurable, `native.time`, §14); callable through `Call()`. No input required. Output: `unix` (integer seconds since UTC epoch), `iso` (RFC 3339 string). |
| `sys/sink`     | Local; action owner `sys`; price 0 (configurable, `native.sink`, §14); callable through `Call()`. Accepts any input, returns `{}`. Universal no-op sink for steps that require an onward action but no further computation. |
| `sys/message`  | Local; action owner `sys`; price 0 (configurable, `native.message`, §14); callable through `Call()`. Sends a message to another platform user by creating a Step they must acknowledge. Input: required `to` (`handle` of recipient), required `message`. Output: `step_id`. The Step sets `required_caller_user_id` to the resolved target user and `partial_args` to `{"message":"..."}` so the recipient can read it via `step list`. Uses `sys/sink` as the step's `action`. `ErrInvalidInput` if `to` cannot be resolved. |
| `sys/random`   | Local; action owner `sys`; price 0 (configurable, `native.random`, §14); callable through `Call()`. No input required. Output: `value` (float in `[0, 1)`). Exists to provide randomness to WASM scripts, which have no ambient access to the OS random source. |
| `sys/transfer` | Local; action owner `sys`; `effect = "transfer"`; price 0 (configurable, `native.transfer`, §14); callable through `Call()`. An ordinary priced action whose successful execution commits a deferred, receipt-backed transfer effect delivering `amount` credits from the immediate caller to `target` (§13 value transfer). The value is funded from the caller's own balance (not the process budget) and delivered untaxed; the execution price is charged and taxed normally, on a separate wallet. Input: required `target` (a local `handle`), required `amount` (positive integer). Output: `amount`. The beneficiary is always local to this kernel; a peer reaches the action only by completing a payment step parked for it (§13), which is how value crosses a boundary inbound. `ErrInvalidInput` for a non-positive/fractional amount, a kernel-qualified `target`, or a peer/remote beneficiary the caller may not reach. |
| `sys/web`      | Local; action owner `sys`; price 0 (configurable, `native.web`, §14); callable through `Call()`. Read-only fetch of a public web page. Input: required `url` (string); a scheme-less `url` defaults to `https` (HTTPS-first, like a browser), and an explicit `http`/`https` scheme is respected and never silently downgraded. Output: `status` (HTTP status integer), `body` (response body string), `content_type` (response `Content-Type` string), `final_url` (the URL actually fetched, after scheme defaulting and redirects). GET only; no caller-supplied headers or auth, so nothing sensitive enters args/receipts/logs. A fixed, configurable descriptive `User-Agent` is set by the action itself. Same SSRF discipline as `kind=http` (§7): RFC 1918 private, link-local `169.254.x.x`, and reserved hosts are rejected with `ErrInvalidInput` unless `allow_local_sources` is set; loopback is permitted by default like any other fetch. Non-2xx statuses are returned in `status`, not raised as errors, so crawlers can react to them; 10 MiB response cap. `ErrInvalidInput` for empty `url`; `ErrInvalidState` if the fetcher is unconfigured; `ErrExecutionFailed` on transport failure. The mediated path by which WASM scripts read the network: scripts still receive no ambient sockets — they reach the web only by calling this action through `Call()`, charged and SSRF-restricted to public hosts (§9). |
| `sys/tinygo/compile` | Local; action owner `sys`; price 5 (configurable, `native.tinygo`, §14); callable through `Call()`. Compiles author-supplied TinyGo to a WASM artifact using the platform TinyGo compiler, prepending the Juice WASM SDK so the author writes only `func Handle(in map[string]any) (map[string]any, error)` (the SDK owns `package`, imports, `alloc`, `run`, `main`). Input: required `source`. Output: `status` (`success`/`failure`), `artifact` (base64 WASM, on success), `artifact_hash` (SHA-256 hex, on success), `diagnostics` (array). Empty `source` gives `ErrInvalidInput`; an unavailable compiler toolchain gives `ErrInvalidState` (platform misconfiguration — the call fails and is not charged). Author compile errors and import/export-validation failures use output failure status, not kernel errors (so the attempt is charged). Registration is separate supervision: pass the returned artifact to `action create --kind wasm --artifact` (§14). |

Stats use:

```text
mean_(n+1) = mean_n + (x_(n+1)-mean_n)/(n+1)
```

`latency_estimate` is the arithmetic mean over completed calls, with denominator `uses`; each sample is the call's own elapsed time, `transaction.ended_at − transaction.started_at`. Buyer-experienced subtree latency (including step dormancy) is not a stat — it is a query over descendant transactions (§11).
`rating_estimate` is the arithmetic mean over rated calls only, with denominator `rating_count`.
There is no cost estimate: an action's all-in cost is its advertised `price` (§6), known in advance and returned by lookup.

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

`required_caller_user_id` is mandatory. Open completion is not supported. It may name a peer's kernel account, in which case the step is completed over `/juice/fed/step/1` (§13) — a kernel account holds no session token, so the federation protocol is its only completion path.

Waiting on several things at once is **not** a kernel concern. A Step gives funded, attributed, one-shot resumption by one nominated caller; anything richer — first-of-N, all-of-N, quorum, deadline — is composed above it by ordinary actions, in user land, with no platform stdlib entry and no kernel-adjacent state. The platform ships no coordination natives.

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

`CreateStep(required_caller, trace, action, partial_args) -> step_id`: the creating authority is the trace's action owner (`Trace(trace).action_owner_id`), so in-execution creation is implicit. For external creation (`POST /v1/steps`) the service layer requires that the authenticated, non-suspended user be permitted to use `trace` (precondition 4 of §4) before invoking the primitive. The process is `Trace(trace).process_id` and must be open; `CanCall(creator, action)` must hold, the creator being the trace's action owner — a step binds its target's visibility at creation (§4 binding rule), so a provider may park its own `private`/`local` action to be completed by a caller who could never call it directly, exactly as a closure captures a private function. The `required_caller` must resolve to an account so the step is completable. Requires `Trace(trace).available ≥ action.price`; creation parks that amount from `trace` (§6), so the parked price is always the recorded payer's money. The completer's allowed input is derived (above), not supplied. Returns a `waiting` step.

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

The resumed `Call` takes the step's parked price as its allocation — `creator.locked -= p`, new trace `available = p` — in place of §6's trace-entry move; no availability check occurs. The completion call's allocation and transaction `gross` are `step.price`, not the action's current price. On `Call` completion (success or failure), atomically records `status=done` and `tx_id`. If `Call` rejects before creating a transaction (action deactivated, owner suspended, malformed input — but never a visibility change, which the binding rule ignores at completion, §4), the step is reset to `waiting` — its parked price stays parked. Funds exhaustion cannot occur.

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

Every committed success or failure has exactly one receipt. `issuer_user_id=sys`. `args_hash` and `reply_hash` are SHA-256 over RFC 8785 JCS canonical `args_json` and `reply_json`. Receipt economics match the transaction. `gross` is the call's price — its full allocation (§6). `fee` and `net` are the settlement amounts: `fee + net = taxable`, the allocation remaining at settlement; `gross − fee − net` is what the call committed downstream. On failure `fee = net = 0`, and the refunded amount equals `gross` minus the `fee + net` totals of the call's settled descendants. Signature is Ed25519 over canonical receipt JSON excluding `signature`. Transaction and receipt creation are atomic.

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

**Seed-phrase recovery.** A password stays the daily credential; a lost password is recovered by proving possession of a per-account recovery key, not by email (there is no email-based reset, and no `email` field). At `user create` (and first boot for `sys`) the client generates a 12-word BIP-39 mnemonic, derives an Ed25519 keypair from it, sends only the public half — stored as `recovery_public_key` (§3) — and shows the mnemonic once; the mnemonic is the master secret and never reaches the server. Recovery is two unauthenticated calls under the auth rate limiter: `POST /v1/auth/recover/start {handle}` issues a single-use, TTL-bound nonce, and `POST /v1/auth/recover/complete {handle, nonce, signature, password}` verifies the client's Ed25519 signature over the disjoint challenge payload `{recovery_challenge: nonce}` against the account's `recovery_public_key`, consumes the nonce (single-use ⇒ no replay), and sets the new password — bypassing the current-password check that a locked-out user cannot satisfy. The recovery key is a recovery credential only: it is distinct from `public_key`, never authenticates a session, and never makes the account a peer (§3, §4).

The same PKCE/device machinery serves upstream OAuth consent for `oauth_delegated` actions (§8): the client hosts the redirect target — a local client (CLI/desktop) catches it on a loopback listener reachable by the operator's own browser even behind NAT; a hosted client uses its own provider-registered callback URL — and drives `POST /v1/grants/start` / `POST /v1/grants/complete` authenticated as the grantor. Consent is planned per upstream account (§8): a `grants/start` names a `(selector, provider)` group, its authorize request carries the union of the group's `scopes`, and `grants/complete` stores one `Connection` and mints one `Grant` per action in the group. The kernel holds only short-lived in-memory PKCE/device state (~10 min TTL) and performs the code→token exchange itself, so no unauthenticated callback route exists on the kernel and the refresh token never passes through the client. The kernel never dials `redirect_uri`; it only embeds it in the authorize URL, and the OAuth provider's registered-redirect allowlist (exact match) is what binds an issued code to a legitimate client, so any http(s) redirect is accepted and the consent-phishing vector is closed at the provider. In-memory consent state is single-use, TTL-bound, and completable only by the grantor who started it.

Errors:

```text
ErrUnauthenticated ErrUnauthorized ErrNotFound ErrInvalidInput ErrInvalidState
ErrInsufficientFunds ErrExecutionFailed ErrSchemaViolation ErrTimeout ErrInternal
ErrGrantRequired ErrPeerUnreachable ErrPeerUnfunded ErrTermsChanged
```

`ErrGrantRequired` is a precondition failure, the twin of `ErrInsufficientFunds` — the process owner must delegate an upstream OAuth grant before the action can run (§8). It carries the action reference as structured metadata so clients act on a code, not a message.

`ErrPeerUnreachable` and `ErrPeerUnfunded` attribute a remote-proxy failure to the federation relationship, not to the caller. `ErrPeerUnreachable` means the first dispatch provably never reached the peer (§13, never-dispatched settlement); the call is settled locally as an ordinary failure with a full refund. `ErrPeerUnfunded` means the remote peer signed a zero-charge rejection because *this kernel's* prepaid credit there is exhausted — an operator condition remedied by an out-of-band payment and `admin deposit` on the peer, never the caller's own balance. Both carry the peer handle as structured metadata (`Meta["peer"]`), like `ErrGrantRequired` carries its action, so clients act on a code and fields, not on message text.

Errors map to stable CLI exit codes and HTTP statuses. Messages are concise; logs may include diagnostics.

The fixed superuser handle is `sys` (bare — handles carry no sigil, §3). CLI admin authority compares authenticated handle to `config.superuser_handle`. `sys` is the single privileged system account: signing-key owner, native-action owner, fee recipient, and gossip "about" source.

First boot prompts only for a password and atomically creates:

```text
sys user
config.superuser_handle = sys
config.signing_public_key  = base64url Ed25519 public key
config.signing_private_key = base64url Ed25519 private key
config.jwt_secret          = 32 random bytes, hex
```

A chosen password must be at least 8 characters, enforced server-side at user creation, first boot, and password update (no composition rules); a shorter one returns `ErrInvalidInput`. First boot also enrolls `sys`'s own recovery key and prints its one-time recovery phrase (seed-phrase recovery above), so the operator can reset the superuser password. Private signing key and JWT secret are never logged or returned. Partial first boot is rerunnable. `JUICE_SECRET_KEY` overrides stored JWT secret at runtime only. First boot also **requires a `kernel_handle`** — the name this kernel presents to the network (§13): from config, else `JUICE_BOOTSTRAP_KERNEL_HANDLE`, else an interactive prompt that repeats until a non-empty name is given; a headless first boot with none set fails rather than name the kernel silently.

The kernel's federation network identity is derived deterministically from this same Ed25519 signing key; there is no second identity or network key. The `public_key` is simultaneously the kernel's Juice identity (§13) and its address on the federation transport. The signature domains of the transport handshake and of Juice payloads (receipts, ratings, manifests, federation requests) must be disjoint: no byte string signed in one domain may verify as a valid message in the other. This disjointness is constructive: every signed Juice payload is signed and verified over `"juice/v1/" + domain + "\n" ‖ CanonicalJSON(v)`, one fixed `domain` per payload kind (`receipt`, `rating`, `manifest`, `evidence_receipt`, `fed_call`, `step_complete`, `step_list`, `step_auth`, `settle_open`, `settle_finish`, `settle_reconcile`, `settlement_record`, `capability`, `recovery`). A signature made under one domain never verifies under another. The transport-handshake domain is libp2p's own, disjoint from all of these. This is verified by test (§15).

Every startup reads `config.superuser_handle` to confirm first boot and identify `sys`; it verifies signing keys and aborts if either is absent. It then registers, enables, and makes `local` (§9) `sys/lookup`, `sys/user-lookup`, `sys/llm/chat`, `sys/llm/embed`, `sys/llm/json`, `sys/llm/decide`, `sys/time`, `sys/sink`, `sys/message`, `sys/random`, `sys/web`, `sys/transfer`, and `sys/tinygo/compile` if absent, and reconciles their configurable fields (price and action-specific settings) from config on every startup. It also **soft-deletes any `kind=native` action whose handler is not registered in the running build** (disabling discovery, preserving history, §7): a native action removed from the platform stdlib stops being listed and callable on an existing database, self-healingly and for any native. It then runs recovery (§5).

Bootstrap is idempotent. Supervision operations are not native actions.

The superuser may suspend or unsuspend users. Suspension preserves data and makes every authenticated request return `ErrUnauthenticated`. It also excludes the suspended user's actions from action listings and makes them uncallable (`CanCall` fails on a suspended owner, §4); their data survives and unsuspending restores listing and callability.

`Kernel.Deposit(operator_user_id,target_user_id,amount,reason)` is admin-CLI-only supervision. It requires configured superuser and positive amount, then atomically credits `user.available` with a ledger entry (`from` null → `to` target). `reason` is optional and stored when provided. The operation is idempotent over `external_key` when supplied. It is served on the public TCP API (§14) as a superuser-gated route: authority is the `sys` bearer token, not filesystem access.

`Kernel.Withdraw(operator_user_id,target_user_id,amount,reason)` is admin-CLI-only supervision: the mirror of `Deposit`. It requires configured superuser, positive amount, and `target.available ≥ amount`, then atomically debits `user.available` with a ledger entry (`from` target → `to` null) — the user's credits are redeemed and the operator owes the out-of-band payout. The operation is idempotent over `external_key` when supplied: a replay returns the existing ledger entry before the `target.available ≥ amount` check runs, so a replayed withdrawal never fails on a balance that has since dropped. Served on the public TCP API as a superuser-gated route (§14), like `Deposit`.

`Kernel.Transfer(caller_user_id,recipient_user_id,amount,reason)` is user self-service — the self-authorized sibling of `Deposit`/`Withdraw`, **not** superuser supervision. Only the authenticated, non-suspended local caller may move their own credits: it atomically debits the caller's `available` and credits the recipient's, recording one ledger entry (`from` caller → `to` recipient). It requires positive amount and sufficient caller balance (enforced atomically at the debit, so a concurrent spend cannot overdraw), and rejects a self-transfer, a missing or suspended recipient, and a **peer/proxy recipient** (`public_key` set) — crediting a kernel account would corrupt the bilateral federation account (§13). Because it is not an action and never routes through `Call()`, it has no composition surface: no action a caller runs can move the caller's funds. The operation is idempotent over `external_key` when supplied. Served on the public TCP API (§14) as a plain-authenticated route (no superuser gate). Local-only: never federated, gossiped, or exposed as an action.

`Kernel.UpdateUser(callerUserID, description, currentPassword, newPassword)` is user self-service: only the authenticated, non-suspended local user may update their own account. `description` and `newPassword` are both optional; at least one must be provided (`description` may be `""` to clear it). When `newPassword` is non-empty, `currentPassword` must match the stored hash; mismatch returns `ErrUnauthenticated`. An account with no password credential (a key-only account) cannot use this operation (`ErrInvalidState`). `handle` is immutable except by superuser rename. The update is atomic.

`Kernel.RenameUser(operatorID, targetID, newHandle)` is superuser-only supervision, the sole path that changes a `handle`: it validates `newHandle` (unique, no `/`) and writes it atomically, vacating the old handle for reuse. The superuser's own account cannot be renamed (`ErrInvalidInput`), as its handle is bound to `config.superuser_handle`; a kernel account is refused too, since its name is its kernel's petname in the other namespace — rename it by public key or petname instead. `Kernel.RenameKernel(operatorID, publicKey, petname)` is its sibling over the kernel namespace: the explicit, exact bind of §13, and the only way to name a kernel this node has merely discovered. Both are served on the public TCP API (§14) as superuser-gated routes.

## 13. Federation

### Transport

Federation has exactly one carrier: a peer-to-peer transport (libp2p) behind the replaceable `fed` interface (§2), which `kernel` never imports. The kernel addresses a peer only by its `public_key`; the transport resolves that key to a live connection — direct when the peer is publicly reachable, hole-punched through NAT when possible, relayed through a public helper node as a last resort. Streams are mutually authenticated by peer key, so a connection is itself proof of the counterparty's network identity; the per-request Juice signatures below are nonetheless retained deliberately, because receipts, rejections, and dispatch records must be storable and verifiable offline (§11) — channel authentication cannot replace a signed artifact. The transport-handshake and Juice-payload signature domains are disjoint (§12).

On startup the kernel joins the peer-to-peer transport, bootstrapped from `bootstrap_peers` in `config.json` (§14). A bootstrap peer is just a publicly-reachable kernel (every kernel runs the DHT — for key→address resolution — and a relay); the shipped default points at the project's public node, so a fresh `juice serve` joins out of the box. **Discovery is libp2p routing discovery over a fixed namespace**, not gossip-carried membership: every kernel advertises the namespace to the DHT — its provider record supplies its transport addresses, which the receiver refreshes into its libp2p peerstore as ephemeral reachability data, never persisted as Juice identity nor trusted as authority — and on a timer (`discovery_interval_seconds`, §14) enumerates the namespace's providers and pulls gossip from each, together with its `bootstrap_peers` seeds and its counterparties, into `Kernel` rows, so the known network fills in progressively out of the box. An empty `bootstrap_peers` means no seeds and no directory discovery: with no way into the DHT the kernel skips advertising and provider enumeration and only syncs its existing counterparties, which still resolve and settle normally. Learning a kernel this way grants no execution authority or credit — a call still requires authoritative resolution over `/juice/fed/resolve/1` and admission under the balance/exposure rules (§13). Because a libp2p peer ID inlines its Ed25519 key, a bootstrap peer supplied only as a multiaddr is addressable and inspectable by the same base64url key every federation command takes. Publicly-addressed and NAT-bound kernels federate identically — a home kernel behind a router needs no advertised address, port-forwarding, or `.well-known` document of any kind (the local `server_url` in §14 is only the loopback URL the CLI dials to drive your own kernel, never a federation address).

Federation protocols are versioned libp2p streams: `/juice/fed/call/1` (inbound proxy call), `/juice/fed/step/1` (step listing and completion, below), `/juice/fed/resolve/1` (open read-only single-action manifest and principal resolution — the execution primitive: resolve one action or one user by handle→`PrincipalID`, the sole cache-fill path), `/juice/fed/gossip/1` (cursored: catalog snapshot + one evidence page), and `/juice/fed/settle/1` (two-party commit/reveal for sub-quantum residual debts, §13). There is no separate inspect protocol — `admin inspect` is served by gossip. `Call` enters a computation and `CompleteStep` resumes one (§10) — a wire carrying only the first would strand every continuation addressed across a boundary. A payload change (e.g. the call payload's `recipient`/`expected_contract_hash`, below) keeps its stream id and surfaces as a per-payload signature failure; the network upgrades in lockstep (§12). Payloads and verification are exactly the settlement rules below; only the carrier is libp2p.

Inbound federation traffic is resource-limited at the transport: per-source-address limits where an address is visible, per-peer stream and byte budgets, a global inbound cap, and stricter budgets for relayed (address-less) traffic. Peer identities are self-issued and free to mint, so per-key limits alone are never sufficient against Sybil flooding. These transport limits replace §14's per-IP peer-request rate limit.

### Peers and kernel accounts

**Terminology.** A **peer** is any remote kernel this kernel knows — through discovery or through an account. A **counterparty** is a peer with a bilateral financial account here (a `Kernel` row with a referencing `Account`, §3); the terms *peer account*, *peer sync*, `last_seen`, and `peer(C)` all refer to that account-holding sense. **Discovery** finds peer identities and transport addresses; **gossip** exchanges first-party catalogs and evidence; **evidence** is a signed statement by one kernel about a subject kernel. Discovery is global and permissionless and creates no account; a counterparty is created only by a call or a deposit (below).

A **user** and a **kernel** are different entities that each hold an `Account` (§3). A user account logs in with a password and holds tokens; a kernel account holds no credential at all and authenticates by federation signature, per request. The account is the shared half, so federation adds no new money model: a kernel account holds credits, pays, and is paid like anyone else. Only a live *user* account has a handle — a purged peer leaves a handleless, keyless row that anchors the ledger (§13 Retention) but never enters user operations.

**Naming is a petname system** — Stiegler's structure (*An Introduction to Petname Systems*, 2005), the standard resolution of Zooko's triangle: a name cannot be secure, decentralized, and human-meaningful at once without a registry or a consensus ledger, and Juice has neither.

| Name | Trust | Resolves? |
| ---- | ----- | --------- |
| **key** | self-certifying, globally unique, unmemorable | always |
| **nickname** | self-asserted by the remote, not unique, no authority | **never** |
| **petname** | assigned here, unique here, memorable | yes, kernel position |
| **handle** | assigned here, unique here | yes, user position |

A stable principal is `PrincipalID = (kernel_public_key, user_id)`; neither half changes when a name changes. References are `owner/name` locally and `owner@kernel/name` across kernels, `kernel` being a petname or a raw key; resolution captures the `PrincipalID` beneath the friendly name (proxy `remote_owner_id`, Step `required_caller_remote_id`, receipts). Handles and petnames are **separate uniqueness scopes** — a user and a kernel may both be `minibox` here, the grammar telling them apart by position — but share one validator: bare, no `@` or `/`, never key- or id-shaped (§14), else the resolver's disjointness collapses. Two owners on B never collide (`alice@B/greet` vs `bob@B/greet`), yet both settle into B's single account. Key rotation is unsupported: a lost key is a lost identity and reachability at once.

**Kernel lifecycle.** Observing, naming, and funding are three separate operations:

| Path | binds petname | account |
| ---- | ------------- | ------- |
| gossip / discovery | no | no |
| verified outbound action or remote-user resolve | yes | yes |
| inbound signed call; deposit or suspend by key | **no** | yes |
| withdraw, unsuspend, settle | no | must already exist |
| explicit bind (`admin rename`) | yes | no |

Automatic binding keeps any existing petname, else the cached nickname when it is a valid bare name, else `k-<key8>`; collisions suffix (`acme-2`); one store transaction, so concurrent first use converges. An explicit bind is **exact** — an occupied petname is `ErrInvalidInput`. Binding only on our own outbound act is what preserves anti-squatting: a stranger cannot seed a local name by calling us or by advertising a nickname. Suspend provisions and freezes in **one** operation, so no inbound call meets a briefly-active account.

### Calling a remote action — resolve on use

Consuming another kernel's actions requires **no subscription** — naming `owner@B/name` resolves and caches that one action on demand (§8), a purely local read to which B makes no decision. **First meaningful use** — the first verified action resolve that creates a proxy row, or the first verified remote-user resolve for a Step — is also when A binds a petname for B (above). Binding is best-effort and never fails or delays the call; a cold call by raw key therefore always succeeds even when no friendly name is available.

```text
juice admin settle <kernel>             settle the bilateral position with a peer: exact if debt ≥ Q, else the probabilistic residual protocol (§13)
juice admin suspend <user>              freeze any account — local or peer — so it cannot act
juice admin unsuspend <user>            lift a suspension
juice admin rename <target> <new-name>  rename a local account, or bind a kernel's petname (by key or petname)
juice admin peers                       every known kernel — counterparties (with account + balance) and discovery-only alike, this kernel excluded (--all also shows suspended)
juice admin inspect <key|user>          view remote identity, public actions, retained evidence (execution summary + per-issuer experience), and reachability (no DB write)
```

Federation trust is superuser supervision, so these live under `admin`, served on the public TCP API as superuser-gated routes (§14). `admin inspect <key>` is the operator's window into a remote kernel (there is no browser-reachable federation endpoint): it reports the peer's identity and public actions (live from a gossip pull, else the locally-cached discovery docs), the **retained evidence** about that kernel derived from the local evidence cache as an execution summary (the subject's own rows) plus per-issuer counterparty experience (trade-backed ratings distinguished from unverified, §13), plus reachability diagnostics (direct / hole-punched / relayed, latency, protocol versions).

Federation commands are defined for an offline peer and bounded so they fail promptly: a cold call to an unreachable peer fails fast (§13 never-dispatched), `admin inspect` degrades to the last-known local data with reachability `unreachable`, and `admin suspend`/`unsuspend`/`peers`/`rename`/`identity` are local and always work.

`admin identity` prints this kernel's own federation identity — its public key (the value peers address it by, since there is no `.well-known`), handle, and libp2p listen addresses. Because every kernel runs a circuit-relay service and joins the discovery DHT, a **publicly-reachable `juice serve` automatically is the network's bootstrap + relay** — the meeting point NAT-bound kernels announce to and are reached through; there is no separate seed process. A public node binds the standard federation port `31313` for a stable address (a NAT-bound node uses an OS-assigned port and is found by key).

Calling is **permissionless**: public actions are callable, so resolve needs no approval or handshake. Payment is the trust decision — **global exposure** (Money, below): a caller with no account is auto-provisioned a zero-balance peer account on its first signed call (a price-0 action runs at once), a priced one runs against prepaid balance and/or global exposure headroom and is refused with a signed 402 once it would push receivables above `X`. Operators settle out of band with `deposit`/`withdraw` (§12) — Juice owns the ledger, external systems move the money. The petname is addressing only, never signed or sent.

Moderation is **`suspend`, the single durable peer lever** — it may name a kernel that has never called, provisioning and freezing its account atomically — (unified across humans and peers, §12): it freezes an account's ability to act — a suspended human cannot log in, a suspended peer's inbound calls are refused with a signed rejection — reversible with `unsuspend`. There is no consumer-side "unsubscribe" or "unfriend" and no `denied_at`: a caller that no longer wants a peer's actions simply stops calling them, and the proxy rows lapse through peer retention (§13); a suspended peer is the former deny, governed by the one `suspended_at` column (§3). Suspend is a pure freeze with no cascade.

The relation is **not transitive**: invoking B's actions requires resolving them from B directly (a `remote_proxy` cache row is never re-served, §8). Exposure is each side's lever: a serving kernel refuses further paid calls once its **global** gross receivables would breach `X` (§13); settlement or a deposit restores headroom.

### Gossip — discovery and reputation

Gossip is information, never authority. The `/juice/fed/gossip/1` protocol (open, read-only, cursored) returns, on **every** reply, the kernel's full first-party catalog snapshot — its identity (key, handle, and its **about**, `sys`'s description, §3), its searchable **users** (`sys` plus the owners of its active public actions, each `{user_id, handle, description}`), and its own active public **signed action manifests** — plus **one page** of trade **evidence** ordered by effective time after the request cursor and `counterparty_balance` (below). No third-party reputation aggregates, no transacted-peer endorsements, no URLs, and no membership: evidence is first-party only, a peer is a key resolved through the transport, and discovery of *which* kernels exist is routing discovery's job (Transport), never gossip's. A kernel serves manifests and evidence only for its **own** actions; an imported `remote_proxy` is never re-served, so federation stays non-transitive (§8).

**Discovery cache.** A receiver verifies each manifest's signature against the gossiping key and rebuilds that kernel's `DiscoveryDoc` rows (replace-all per source kernel) — one `kind=action` doc per verified manifest, one `kind=user` doc per user summary — indexed by the same lexical (BM25) + optional-semantic machinery as `sys/lookup` (§9). This is the searchable directory: `sys/lookup` and `sys/user-lookup` surface discovered-but-unresolved remote actions/users to authenticated local callers, who reach them by resolving from the home kernel (§8, still authoritative). A discovery doc grants nothing — it may be stale or absent without affecting callability, pricing, or settlement, and deleting the whole cache only forces a re-pull.

**Evidence.** Reputation is carried as signed, independently verifiable evidence, never opaque aggregates. Each evidence bundle is an `EvidenceReceipt` — a wire-only signed projection of one committed call exposing only `{receipt_hash, subject_kernel, subject_action, counterparty_kernel?, status, started_at, created_at, remote_receipt_hash?}`, and nothing that would leak a counterparty's business (no transaction/trace/process/caller/payer identity, no `args_hash`/`reply_hash`, no amounts, no `value_to`) — optionally paired with a signed **rating projection** `{rating, note, rated_receipt_hash, created_at}`, the public reputation signal only, never the rater or transaction identity the full `Rating` holds. `receipt_hash` is `SHA-256(CanonicalJSON(the stored receipt))`, the one definition used on both sides of the remote-receipt join. A kernel gossips two legs of its **own** receipts: (a) execution evidence for its own active public actions (`issuer = subject`; `counterparty_kernel` is set only when the caller was a peer kernel, never a user identity — the two-kernel trade proof), excluding value-transfer receipts; and (b) its `remote_proxy` receipts settled from a signed remote receipt for an **admitted execution** — every outbound call the peer actually ran, success or failure, not only the rated ones (subject = the remote principal's kernel/action, `remote_receipt_hash` linking to the serving kernel's execution receipt), an optional rating projection riding along when the caller rated it. A leg-(b) receipt is gossip-eligible only when it reached execution, so three classes are excluded: a locally-manufactured settlement (never-dispatched `ErrPeerUnreachable`, max-age bound, forced closure, crash-recovery `interrupted`, missing adapter — any settlement carrying no stored remote receipt); a signed zero-charge **rejection** (402 exposure, `refresh_proxy` cache fault, or non-executable action — a rejection receipt sets `tx_id` to the call's `idempotency_key`, which distinguishes it from a genuine execution at any price, including 0); and a quarantined invalid receipt. Learned evidence is never re-gossiped. Retention is a fixed cap **E = 200** most recent per `(issuer, subject_kernel, subject_action)`; a rating rides only while its receipt is retained. A rating with no `rated_receipt_hash` feeds local `Stats` but is never gossip-eligible; gossip ingress requires a non-empty hash.

**Deriving metrics.** A receiver derives two display views from its verified evidence cache (`admin inspect`, §14). The **execution summary** for a subject counts **only** `issuer = subject` rows — the subject's own self-reported execution evidence — so a remote call is never double-counted. The **counterparty-experience** view groups the remaining rows by issuer, each issuer's directly-observed interactions with the subject, shown alongside but never folded into the execution summary. A rating counts as **trade-backed** only when the two-kernel link holds — the rating's `remote_receipt_hash` equals a subject execution row's `receipt_hash` and that row names the rating's issuer as `counterparty_kernel`; an unlinked rating is retained but surfaced only as an *unverified* count. Two different valid ratings under one `(issuer, receipt_hash)` are **equivocation** (both excluded). This proves attribution and immutability, never honesty: a self-issued key can fabricate its own evidence at zero cost, so the receiving kernel's own settled experience and distinct-issuer trade history remain the only Sybil-resistant signals — the kernel mandates the signals and this discipline; how they weight ranking is the replaceable layer's formula (§9). Importing an action still initializes local `Stats` to defaults (§3); evidence never overwrites `Stats`.

**Cursor.** Evidence is ordered ascending by `(effective_at, receipt_hash)`, where `effective_at` is the rating's `created_at` when rated else the receipt's — so a rating created after its receipt was already pulled re-surfaces its bundle and attaches on the receiver. The cursor is the exclusive high-watermark of the last committed item, persisted per kernel (`Kernel.gossip_cursor`), advanced only after a page is verified and committed; one page per peer per discovery pass, so evidence catches up across passes while the catalog snapshot refreshes every pass. Sender-side eviction may leave gaps, never an endless replay.

The gossip response additionally carries `counterparty_balance`, populated only when the requesting peer authenticates as a known, non-suspended key: the requester's proxy-user `available` on the serving kernel — "your credit here". It is absent for strangers, suspended keys, and anonymous pulls. Like all gossip it is information, never authority: the money path is driven solely by authoritative signals (signed receipts and rejections, connection outcomes), never by this cached figure.

**Discovery and peer sync.** Discovery and peer sync are **one loop** (`discovery_interval_seconds`, §14): each pass enumerates the routing-discovery namespace's providers (Transport) and pulls gossip from the union of those providers, the `bootstrap_peers` seeds, and the kernel's counterparties, verifying each first-party. A pull is **verified** only when it is authenticated as the key dialed (the reply's `public_key` equals it) and carries a valid bare handle; a verified pull refreshes that kernel's `Kernel` row (nickname, about — never its petname) and its evidence cursor, while an unverified one is skipped this pass and retried the next. A successful pull from a known, non-suspended counterparty additionally persists `last_seen = now` and `peer_credit = counterparty_balance` on its `Kernel` row (§3): a display-only cache (§14) that never gates a call, price, or settlement, and never counts as activity for retention (§13 Retention) — answering gossip is free liveness and would otherwise immortalize a zombie counterparty; retention stays trade- and value-based.

### Money — global exposure and settlement

Juice is one global action market: any user calls any reachable action from their **local** balance — no remote account, prefunding, or credit negotiation — at a deterministic price and local deduction, while kernels privately absorb, price, cap, and settle the risk. The peer `User`'s `available` is the **bilateral position** (positive = the peer prepaid; negative = the peer owes this kernel), netting automatically as one row.

**Pricing components.** A remote call's user price is deterministic and pre-advertised:

```text
user_price = mp                                   // base action price → provider user
           + ceil(mp · remote_bps / 10000)        // serving-kernel markup (execution tax + risk premium) → serving sys
           + ceil(sr · import_bps / 10000)        // origin import fee → origin sys (retained locally)
  where sr = mp + ceil(mp · remote_bps / 10000)
```

`remote_bps` (serving-side execution tax + risk premium, advertised as one number in the signed manifest) accrues to the **serving** kernel's `sys` — it prices the credit, default, and settlement variance the serving kernel bears. `import_bps` (origin policy) is **retained** by the origin kernel to cover its outgoing rail cost and variance; it never crosses the wire. Only `sr` (base + serving markup, on the *actual* charge) becomes the cross-kernel obligation. A local call pays just `mp + fee_bps` (the local execution tax, unchanged). Every component is deterministic and advertised before execution.

**Global exposure control.** A serving kernel bounds its unsecured lending across **all** peers by two global parameters (§14): the settlement trigger `Y` and the maximum gross unsecured receivables `X`, with `0 < Y < X`. Gross receivables `G = Σ_peers max(0, −available)`. Admission of an inbound paid call reserves its worst-case obligation `W = mp + ceil(mp·remote_bps/10000)` and admits it iff the post-reservation `G ≤ X` (atomic in the same wallet move, so concurrent calls cannot jointly breach `X`, §6). `X` is **global, not per-peer**: minting additional kernel identities cannot multiply the serving kernel's maximum exposure (Sybil-proof). The default `X = 1000` (with `Y = 500`) lets a fresh kernel serve remote paid calls out of the box against a bounded, Sybil-proof maximum loss — global credit-based access is thus on by default but is an explicit operator setting; `X = 0` opts into prepaid-only. Reaching `X` refuses further obligation-increasing calls with a signed zero-charge 402 rejection, surfaced to the caller as `ErrPeerUnfunded` — an operator condition (settle), never the caller's own insufficient funds. A free (price-0) call adds no exposure and always runs.

At or above `Y`, the kernel sets `settlement_due` and surfaces it on `admin peers`/gossip/`admin identity` (information, never authority). **`Y` signals; it does not auto-initiate** — settlement moves external money, which is operator authority (like `deposit`/`withdraw`), so the operator runs `admin settle <peer>` (§14). Crossing `Y` never stops execution; only reaching `X` does.

**Settlement.** Kernel obligations settle asynchronously through the operator's chosen external rail; Juice owns the ledger and the obligation, the rail moves the real money. Let `Q` = `settlement_quantum` (§14), the smallest fee-rational external payment — an operator sets it from the rail fee `F` and a maximum acceptable fee ratio `r` as `Q = F/r` (e.g. `F=$0.15, r=5% ⇒ Q=$3`). Because the rail cannot batch bilateral payments, every payment below `Q` would be uneconomical. For a terminal bilateral debt `d`:

- **Exact** (`d ≥ Q`, or `Q = 0`): pay exactly `d` on the rail, recorded with the existing `deposit`/`withdraw --external-key <settlement_id>` (§12, idempotent).
- **Probabilistic** (`0 < d < Q`): a fair two-party lottery decides whether the debtor must pay `Q` (with probability `d/Q`) or nothing (`clear`). **The lottery moves no money.** Its outcome only *determines what must be paid*; the debt `d` remains on both bilateral rows, unchanged, until the debtor actually pays `Q` on the rail and both operators record it (below) — so `gross exposure = actual unpaid service value` at all times and no lottery outcome can push `G` toward `X`. `E[payment] = (d/Q)·Q = d`, EV-exact; the residual variance is absorbed by kernels, never users. No deterministic option satisfies all constraints (paying `d` is uneconomical; paying `Q` overpays; forgiving enables dust default; a minimum action price of `Q` destroys micropayments), so probabilistic residual settlement is required.

**Commit/reveal.** The probabilistic outcome uses two-party commit/reveal over `/juice/fed/settle/1` (§13 protocol), driven by the debtor. The creditor is **stateless**: it derives its secret `s = SHA-256(signing_seed ‖ "juice-settle-v1" ‖ settlement_id)`, so it holds nothing between rounds and the debtor carries the creditor-signed *open* record back on *finish*. Outcome: `pay iff (SHA-256(settlement_id ‖ s ‖ n) mod Q) < d`, where `n` is the debtor's nonce (modulo bias negligible for `Q ≪ 2⁶⁴`). The signed `SettlementRecord` binds both kernel keys, `d`, `Q`, mode, commitment `H(s)`, nonce, revealed `s`, outcome, expiry, and `settlement_id`; its key-set is disjoint from every other signed payload (§12). Creditor **non-reveal clears the debt for zero** — it cannot withhold an unfavorable result — and the debtor's `admin settle` retries with the *same* nonce until reveal or expiry.

**Outcomes and the rail record.** A **clear** outcome extinguishes the debt immediately and internally — no cash moves — creditor `row +d / sys −d`, debtor `row −d / sys +d` (the internal-only move is conservative: `Δrow + Δsys = 0`, sys absorbs the write-off). A **pay** outcome writes *nothing to the balances*: it stores the signed record (for anti-grinding) and leaves the debt of `d` on both rows, `pending_cash`. Both operators then move `Q` on their rail and record it with **`admin settle <peer> --cash <settlement_id>`** — the only settlement step where money crosses the boundary, and the sole non-conservative move, satisfying the per-kernel identity `Δbilateral_row + Δoperator_sys = external_cash`:

```text
finish → clear:   creditor row +d, sys −d ;  debtor row −d, sys +d ;  cash 0   (immediate, internal)
finish → pay:     no balance change on either kernel ;                cash 0   (records the pending obligation)
--cash (pay):     creditor row +d, sys +(Q−d), cash_in  Q ;  debtor row −d, sys −(Q−d), cash_out Q
```

The residual is realized **operator variance** on `sys` (`Q−d` gain to the creditor, `Q−d` loss to the debtor, only on a paid outcome). `E[variance] = (d/Q)(Q−d) + (1−d/Q)(0) = d(Q−d)/Q` on the creditor and its negative on the debtor, netting to the EV-exact `E[payment] = d`; kernels hold reserves against it. The debtor-side `--cash` leg requires `sys` reserve `≥ Q−d`; short of it, the whole record rolls back and the settlement stays `pending_cash` (no partial writes). While a paid outcome is `pending_cash`, the serving kernel refuses the debtor's further **credit-drawing** calls (it must finalize first); fully-prepaid calls are unaffected.

**Binding evidence, idempotency, abort.** The signed record is binding on **both** kernels; exactly one outcome per `settlement_id` is applied by each, ever. The completion is one `settlement_id`-keyed ledger entry (the `external_key` read-first idempotency of §12), so a replayed *finish* — including a debtor who learned `s` and grinds a winning nonce — is re-served the stored record and never a second signature. Creditor **non-reveal past expiry** ⇒ the debtor applies the clear-for-zero locally with the signed *open* record as evidence, and re-presents it on the next interaction via a `reconcile` request; the creditor MUST then apply the same clear-for-zero (idempotent) or be in **provable default** — the record proves it. Debtor **refusal** (no *finish*) leaves the debt outstanding on both sides consistently, consuming `X`, and further obligation-increasing calls are deniable at admission. Only current outstanding exposure is bounded by `X`; cumulative historical losses are not — hence operator reserves.

### Calls, receipts, settlement

Outbound: a remote-proxy call follows normal role law (`owner` = local process owner, `caller` = local call caller, `target` = the kernel account) and normal wallet mechanics, funded with the proxy's local price `q = sr + ceil(sr * import_bps / 10000)`, where `sr = mp + ceil(mp * remote_bps / 10000)`, `remote_bps` is the **serving** kernel's advertised markup (a signed manifest field, §8) and `import_bps` is local origin policy (§14). The call binds the cached contract hash as `expected_contract_hash` and the serving kernel's key as `recipient` (below). The handler records the UUID v4 `idempotency_key` and the outbound payload (`dispatch_json`: `mp`, `remote_bps`, `import_bps`, and the dispatched `expected_contract_hash`) on the proxy trace atomically with dispatch (§3, §5) — this makes retry after restart possible and keys the hash-conditional proxy deactivation at settlement (§8). On the remote kernel it arrives over `/juice/fed/call/1`, authenticated by the per-request federation signature, and is then an ordinary inbound call by this kernel's kernel account there, admitted against the serving kernel's global exposure cap `X` (§13), executed wholly under that kernel's §6.

A proxy call settles **only on a signed remote receipt** — never on a network timeout. The serving markup is charged by the **serving** kernel and carried in the receipt (`receipt.premium`, on the *actual* charge); the origin separately retains its import fee locally:

```text
remote success:   charge    = receipt.charge (= receipt.base_charge = mp)
                  premium   = receipt.premium (= ceil(charge * remote_bps / 10000))
                  paid      = charge + premium                  → kernel account (bilateral payable to the peer)
                  importfee = ceil(paid * import_bps / 10000)   → origin sys (retained locally)
                  refund q − paid − importfee to the caller's trace
                  local transaction: success; receipt JSON + SHA-256 stored atomically
remote failure (charge = receipt.charge ≤ mp; settled remote descendants stay paid, §6):
                  premium   = receipt.premium (on the actual charge; 0 when charge = 0)
                  paid      = charge + premium                  → kernel account
                  importfee = 0 (no import fee on a failed obligation)
                  refund q − paid to the caller's trace
                  local transaction: failure
signed rejection (charge = 0, premium = 0):
                  full refund; local transaction: failure
                  a refresh_proxy rejection (contract-hash mismatch) deactivates the cached
                  proxy iff the row still holds the dispatched expected_contract_hash (so a
                  stale dispatch settling after a re-resolve spares the refreshed row); the
                  next call re-resolves the new contract
                  a 402 rejection (exposure cap X exhausted) settles as ErrPeerUnfunded — an
                  operator condition, never the caller's own funds, and carries no
                  refresh_proxy (funding is not a cache fault)
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

The premium is the **serving** kernel's compensation for servicing and financing the remote call (execution tax + risk premium), credited to its `sys`; the origin separately retains its `import_bps` import fee on the obligation — a caller kernel keeps only that local fee, never the premium (contrast the local layer fee, §6). The serving kernel's `remote_bps` (default 500) is a signed manifest contract field; the caller can never be charged more than the local price `q` it authenticated and locked before dispatch (§8). Forced closure (`EndProcess`) fails an in-flight proxy call like any running call (§6) and refunds the caller; if the remote side did commit, its charge stands there and is absorbed by the bilateral account, surfacing at reconciliation.

The no-receipt state is the expected steady state of a network of intermittently-online home kernels, not an error: a dispatched call holds its allocation locked and its process open until a signed receipt settles it or the owner forces closure, while a peer unreachable at the very first dispatch instead fails fast with a full refund (the never-dispatched rule above) — so only a call that may already be executing remotely is parked. Process listings must surface this awaiting-receipt state with its age (the earliest parked call's start), so an operator sees funds parked on an unreachable peer and for how long; step listings likewise flag a waiting step whose required caller is a peer. Both are factual (age, peer-ness), never a liveness claim.

Inbound: calls sign `JCS({action, args_hash, counterparty, expected_contract_hash, idempotency_key, recipient, timestamp})`; `action` is the action's stable id on the serving kernel, never a handle-bearing reference — a rename must not strand a cached proxy; `counterparty` is the caller's public key; `recipient` is the serving kernel's key (binding the request to it, so a captured request cannot be replayed to a third kernel); `expected_contract_hash` is the caller's cached hash (§8 If-Match); `args_hash = SHA-256(raw body)`. The receiver verifies the signature, that `recipient` is its own key, the raw hash, and timestamp age ≤ 5 minutes; a valid unknown key is provisioned a zero-balance account (handshake-free peering); a suspended key is rejected. An inbound call the receiver can determine will not execute returns a **signed rejection receipt** (`status = failure`, zero charge) carrying the action's id, so the caller settles at once rather than pinning funds. It sets `refresh_proxy` only on a **contract-hash mismatch** (the action is servable but its contract differs from the cached hash), so the caller re-resolves; a non-executable action (inactive, not `public`, suspended owner) or a balance/exposure rejection omits it, since re-resolving would not help (the caller just settles and refunds, proxy untouched). A genuinely absent/unverifiable action stays a plain error, leaving the caller pending. Idempotency: insert pending before resolution and the contract-hash check, unique `(idempotency_key, counterparty_user_id)`; completed replay returns the stored receipt, pending replay 409, expiry 24h.

`VerifyRemoteReceipt(caller_id, tx_id)` requires `CanReadTransaction` and verifies entirely locally, in two parts. **Receipt integrity:** signature against the peer's `public_key`, stored JSON against its stored SHA-256, `receipt.action_id == proxy.remote_action_id`. **Settlement consistency:** the local transaction's outcome matches `receipt.status`; the amount paid to the kernel account equals `receipt.charge + receipt.premium`; `receipt.premium == ceil(receipt.charge * remote_bps / 10000)` for the dispatch-recorded `remote_bps` (zero when charge is zero); `receipt.charge + receipt.premium ≤ q` (the authenticated ceiling, §8); the origin import fee equals `ceil((receipt.charge + receipt.premium) * import_bps / 10000)` on success (0 on failure) for the dispatch-recorded `import_bps`, credited to origin `sys`; the local refund equals `q − (receipt.charge + receipt.premium) − importfee`; `args_hash` and `reply_hash` match the local record. The receipt's own `base_charge/premium` are the provider's economics and are checked as above; its `gross/net/fee` are reported, not compared. Returns per-check results and top-level `valid`; non-proxy transactions give `ErrInvalidState`.

### Value transfer

`sys/transfer(target, amount)` is an **ordinary priced action with one deferred, receipt-backed transfer effect**. Its execution channel is the normal `Call()` lifecycle (charge/premium/import on any operator-configured `price`, `native.transfer`, §14); its value channel moves `amount` from the immediate caller `C` (§1 role law — never an argument, never the process owner) to `target`. The two are funded from **different wallets** and never mix: the execution `price` from the trace (process budget, owner `P`), taxed by `ComputeFee`; the `value` from `C`'s own `available`, delivered untaxed. At a root call `C = P`; under composition they diverge — a subcall pays the value from the composing action's owner, a Step from the caller who completes it. One `Call()` supplies authorization, so there is no second money primitive.

The value channel is a `TransferEffect` staged at admission, not a handler-side ledger write: the runtime handler only validates and acknowledges; the effect's reserve is locked from `C.available` reserve-first, atomic with the call's funding, and the kernel commits it deferredly at settlement — one mechanism for local and cross-kernel alike. An action is value-bearing iff its **signed contract** declares `effect = "transfer"` (a manifest field, §12.2), never a name coincidence: a local native carries it from bootstrap, a remote proxy from the peer's signed manifest, so the origin never reserves value on a name match. A **local** target resolves to a local ordinary active account at admission (an unknown/suspended/peer beneficiary is rejected before any funds move; a peer/proxy row is a financial counterparty, never a recipient). `sys/transfer` is `local` (§9), so no kernel serves its own stdlib abroad and a kernel-qualified `target` is rejected. Value therefore crosses a boundary in two ways only: a peer completing a **payment step** parked here (below), which stages the inbound effect with that peer as `C`; or this kernel calling a peer's own transfer-effect action, whose proxy carries `effect` from that peer's signed manifest.

The two channels conserve independently and are audited separately (the receipt keeps distinct fields: `charge`/`premium` for execution, `value`/`value_premium`/`value_to` for the transfer). Value is **all-or-nothing**: `value ∈ {0, amount}`; a receipt claiming partial value is quarantined. The value reserve locked from `C` by destination is `value` (same-kernel), `value + value_premium` (inbound serving, `value_premium = ceil(value·remote_bps/10000)` owed to the serving `sys`), or `value + value_premium + value_import` (outbound, `value_import = ceil((value+value_premium)·import_bps/10000)` retained by the origin `sys`); the serving kernel admits the inbound reserve against its global exposure `X` (a transfer is unsecured lending until settlement, which is what `X` bounds). The premiums are computed per channel — `premium = ceil(charge·remote_bps/10000)` and `value_premium = ceil(value·remote_bps/10000)` separately, never `ceil((charge+value)·…)` — and the bilateral peer position moves by their sum; `VerifyRemoteReceipt` checks each channel on its own field. At settlement a **valid** success delivers `value` to the beneficiary (or `value + value_premium` to the peer proxy row outbound) and refunds the remainder to `C`; a **valid** failure/rejection refunds the whole reserve to `C`; an **invalid/inconsistent receipt** after a possibly-executed dispatch keeps the reserve **locked** (quarantined for operator reconciliation), never auto-refunded. The sender pays all fees on top; the beneficiary receives exactly `amount`. The relation stays non-transitive: a peer caller naming a remote beneficiary is rejected. The self-service `/v1/transfers` API (a direct atomic `CreateLedgerEntry`, no fee, `external_key` idempotency) stays a separate surface from this composable effect-bearing action.

### Steps across the wire

A Step's `required_caller_user_id` may be a peer's kernel account: `CreateStep` resolves it like any other account. Visibility is bound against the creator, not the peer (§4 binding rule), so a peer may be parked for any action its creator can call — including a `local` one the peer could never call directly; completion re-checks only liveness, and the peer never gains reach to the action. `/juice/fed/step/1` is what makes such a step completable — a kernel account holds no session token, so it can never reach the HTTP route (§12). Without it the step waits forever with its price parked and no actor able to free it, since the process owner is that same keyless account and `EndProcess` is process-owner-only (§10).

The protocol has two request kinds, each signed under its own canonical payload, both key-sets disjoint from every other signed payload (§12):

```text
list:      JCS({counterparty, recipient, scope: "step_list", timestamp})
           → the serving kernel's waiting steps whose required_caller is the requesting peer,
             oldest first, each with its id, partial_args, derived allowed_input (§10), price,
             and creation time; bounded, and a full page sets `truncated`
complete:  JCS({counterparty, idempotency_key, input_hash, recipient, step_id, timestamp})
           input_hash = SHA-256(input bytes); the request carries exactly those bytes
           → CompleteStep as the peer's account; returns {result, tx_id, trace_id, receipt}
```

`recipient` is the **serving** kernel's public key. A payload naming only its sender is replayable
to any kernel that would accept it — for a step list, one kernel could replay another's request to a
third and enumerate the steps parked there. Binding the recipient closes that; the call payload binds
it too (`/juice/fed/call/1`, §13). The step list is scoped to the required caller **in the query**,
never by filtering a capped page afterwards: the peer's own inbound-call processes are visible to
it under `CanListStep` and would otherwise crowd out precisely the steps it can complete.

Both verify the signature and a timestamp age ≤ 5 minutes, and the connection's authenticated key must match `counterparty`, exactly as an inbound call does. A suspended peer is refused on both (§12 — suspension is the one moderation axis for peers too). `list` is a pure read: an unknown key gets an empty list and is **not** lazily provisioned, since provisioning belongs to a call, which is what opens a billing relationship. `complete` refuses an unknown key outright — a stranger can hold no step here, because `CreateStep` resolves its required caller to an existing account.

**Payment steps.** A Step whose action bears the transfer effect is a *payment step*: a remote buyer completing it funds the value channel. The serving kernel advertises a `payment` descriptor on the step listing — `{beneficiary, amount, remote_bps, remote_max}`, `remote_max = amount + value_premium` — carrying only the remote obligation, never the buyer's local `value_import`/`max_total`. The buyer funds `max_total` reserve-first from its own balance into a dedicated `pending_transfers` record (not a fabricated trace), completes, and settles on the signed receipt: a valid success credits `value + value_premium` to the peer proxy row (the buyer owes it) and `value_import` to its own `sys`; a valid failure refunds; a mis-bound receipt keeps the reserve **locked** (quarantined). The payment binds into the completion with no new wire field: both sides fold the descriptor hash into the completion idempotency key (empty for a non-payment step), which is already signed, so the serving kernel rejects a key that does not match its own descriptor — the step cannot be completed for a different payment. The serving side stages the inbound effect over the merged args, locking the value reserve from the buyer's proxy row atomically with claiming the step; the execution `price` stays creator-parked. The beneficiary must be local to the serving kernel.

Unresolved `pending_transfers` records — `pending` (no settleable receipt yet) or `quarantined` (a receipt that failed validation) — are operator-visible through one superuser resource (§14): `GET /control/transfers` (default unresolved; a `status` filter also exposes terminal `settled`/`refunded`), `GET /control/transfers/{id}`, `POST /control/transfers/{id}/retry`. **Retry is the only mutation** and never picks an outcome: it re-presents the SAME signed completion (stored key + input rebuild the identical request, so the peer replays rather than re-executes) and settles strictly on receipt evidence — success settles, failure refunds, no receipt stays pending (a retry has no never-dispatched proof, the first attempt may already have paid), an invalid receipt stays quarantined. There is no `refund`/`force-settle`/`edit`/`delete`: quarantine means evidence is insufficient (reconciled out of band), not an operator-chosen result.

A peer is served **the request, not the requester**. The reply carries only what the completer needs to act: `partial_args` (the payload the creator bound for it — §14 has `sys/message` put its body there) and `allowed_input` (§14's substitute for reading a target action that may be private). The creating action's reference, the process owner's handle, the target action's `owner/name`, and every local trace/action/transaction id are withheld: a user identity crossing a kernel boundary is what the encapsulation forbids, and none of it is needed to complete a step. The peer-facing shape is therefore a distinct representation, not the local step view narrowed.

`complete` reuses the cross-kernel idempotency record (`(idempotency_key, counterparty_user_id)`, §3): pending before execution, completed with the stored result, a completed replay returning that result and a pending replay 409. The key is **derived from the request** — `SHA-256(protocol | peer key | step_id | input_hash)` — not minted per attempt, so a retry after a network failure presents the same key and recovers the stored outcome; a step completion has no local trace to persist a key on, unlike a remote-proxy call (§13 outbound). The record id is threaded **into the kernel**, exactly as an inbound call threads its own: whichever commit finally settles the completion writes the record's completion atomically with the transaction (§5). That includes a settlement that happens much later — the remote-dispatch retry loop, the pending max-age bound, or a forced closure — because the parked dispatch persists the record id (§3). The service layer therefore disposes of the record only where **no commit will ever occur**: a completion rejected before anything settled deletes it, so a corrected retry is not locked out. A completion **awaiting a peer's receipt** leaves it *pending*, which is now honest rather than terminal: a replay reports a duplicate in flight, and the eventual settlement completes it.

A replayed record is rebuilt from its two stored halves — the result and the signed receipt — and discriminates success on the **receipt's** status, not by probing the result for an `error` key, which a legitimate result carrying its own `error` field would trip. A stored failure never replays as success, and the in-flight reply carries an error code so the requester re-raises a typed error instead of a generic failure.

An outbound completion distinguishes *never dispatched* from *no reply* exactly as an outbound call does (§13): only a provably-unsent request is `ErrPeerUnreachable`; any other transport failure may already have executed remotely and is reported as `ErrTimeout`, recoverable by retrying under the derived key.

Unlike a proxy call, a step completion moves **no money on the requesting kernel**: the step's price was parked on the serving kernel at creation, and completion never checks funds (§10). The requester therefore parks nothing, creates no local trace or transaction, and needs no settlement — so failures are ordinary typed errors, not signed rejection receipts, and a timeout pins nothing. The completion settles wholly on the serving kernel under §6, with the role law giving `caller_user_id` = the peer's account. That settled call is ordinary peer activity for retention (below).

An operator drives it through the commands that already exist (§14), not a second vocabulary: `admin inspect <key>` surfaces the steps a peer holds for this kernel, and `step complete <id> --peer <key>` resumes one — the same command that completes a local step, because a step is a step. Completing a peer's step is superuser scope on that command, since the request is signed with this kernel's own federation identity and so acts as the whole kernel.

### Retention

A peer is kept only while it holds value or has been used recently. Its activity is the most recent settled call (either direction), gossip mention, or deposit/withdrawal; a nonzero balance or funds locked in flight always count as live. A peer idle past `peer_retention_days` (§14) — reachable only at zero balance, nothing locked, and **not suspended**, so a moderation decision outlives idleness — has its accumulated data purged: its proxy actions, their stats, its `DiscoveryDoc` rows, its `EvidenceRow` rows (as issuer and as subject), and its `Kernel` row — the account→kernel link is cleared first, since the foreign key is restrictive, and the identity is then forgotten (a later re-resolve starts fresh). The immutable transaction and receipt ledger is preserved — its party ids carry no foreign key, so a now-dangling peer id is harmless and every local counterparty's credits stay reconstructible (§11); the credentialless account row remains as a legible ledger anchor. Purge is reachable only through zero value, so it never deletes funds or a still-reconstructible credit. The same sweep also evicts a **directory-only** discovered kernel — one learned from gossip but never a peer here — with its `DiscoveryDoc` and `EvidenceRow` rows once its `Kernel` row is stale past `peer_retention_days`, so the regenerable discovery cache stays bounded on a large network; a re-pull re-learns it.

## 14. CLI, HTTP, logging, config

HTTP API is primary. Every user and operator workflow has a CLI command; a protocol-internal endpoint may instead be driven by a higher-level workflow. CLI uses the same service layer, supports human-readable and JSON output, and each command has at least one test. All commands — user-facing and admin/peer alike — are HTTP clients of the server (base URL from `--server`/`JUICE_SERVER`/`server_url`); admin and peer commands hit the same public TCP API, on routes gated by an `IsSuperuser` check, authenticated by the `sys` bearer token (no separate socket or filesystem authority — keep the bearer secret and run `serve` behind TLS or on loopback). `juice serve` is the sole process that opens SQLite; the CLI never touches the database directly. A command's primary identifier is a positional argument by its natural key — a user is a bare `handle` (a public key or raw id is also accepted), an action is `owner/name` locally or `owner@kernel/name` for a remote action (an id is also accepted), and processes, steps, and transactions are ids; a second mandatory value (amount, rating) is the second positional. The three account forms are **disjoint syntactic productions** — a raw id is a hex UUID (`8-4-4-4-12`), a public key is a 43-char base64url string decoding to 32 bytes, a handle is anything else that validates — so one resolver disambiguates by shape with no sigil; a `@`-prefixed handle is rejected, not stripped. Outputs render `handle`/`owner/name`/`owner@kernel/name`, never a raw user id, except `GET /v1/me` which returns the caller's own `id`. A detail (`show`) view exposes every field of the HTTP response; a list view is a summary, and `--json` gives the full HTTP shape.

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
juice action stats <action>              juice action ratings <action>
juice process list                        juice process show <id>
juice process end <id>                    juice run <action> [json]
juice step create <action>                juice step list
juice step show <id>                      juice step complete <id> [json]
juice tx list                             juice tx show <id>
juice tx rate <id> <0|1>                  juice tx verify <id>
juice health
juice admin users                         juice admin show <target>
juice admin suspend <target>              juice admin unsuspend <target>
juice admin rename <target> <new-name>
juice admin deposit <target> <amount>     juice admin withdraw <target> <amount>
juice admin settle <peer>                 juice admin transfer list
juice admin peers                         juice admin inspect <key>
juice admin identity
```

`admin` holds only the operator verbs no ordinary user performs — money, access, federation trust, and the global roster (`users`/`show`). Supervision over everything else is **scope on the normal commands**: a superuser sees all owners' rows on `action list`, `process list`, `tx list`, and `step list`, and may `action disable`/`enable` any action, all over the public TCP API. As for everyone, `action list` is active-only by default; inactive/private rows appear only with `--all` (`?all=1`), so a deactivated action — e.g. a drift-deactivated remote proxy — drops out of the default list. There is no `admin actions/disable/processes/txs/steps` — those were duplicates of the base commands with wider reach.

OpenAPI commands (the OpenAPI spec URL is the positional argument):

```text
juice action import <spec-url>
juice action unimport <spec-url>
juice action unimport <spec-url> --name <action-name>
```

`juice serve` handles `SIGTERM`/`SIGINT`, stops accepting new requests, drains in-flight calls, exits cleanly. No `juice stop`.

Server logs request, caller, process, trace, action, and transaction IDs where available; maps distinct auth, authorization, invalid input, insufficient funds, missing resource, and internal failures to distinct statuses; rate-limits auth and account creation per client with 429 (inbound federation traffic is limited at the transport instead, §13). A genuine-loopback client — the operator's own CLI, direct with no `X-Forwarded-For` — is exempt, since it is already inside the trust boundary the limiter defends; when a loopback peer does carry `X-Forwarded-For` (a co-located reverse proxy), the real client (the last forwarded hop the trusted proxy appended) is the rate-limit key, so external callers are limited per-client rather than sharing one bucket. Action read/list responses include computed `action=owner/name` and `quote_hash` (§4), plus the non-secret auth summary `requires_grant` (always) and `auth_scheme` (when the action has upstream auth) — the scheme name and flag only, never `auth_json`'s config or secrets (§8). `admin peers` lists every known kernel from one query — counterparties (with an `account`, bilateral balance, `peer_credit`, and `last_seen`) and discovery-only kernels (no account) alike, this kernel itself excluded, suspended counterparties shown only with `--all`. `PETNAME` and `NICKNAME` are separate columns because only the first resolves (§13); `—` marks an unbound kernel, still callable by key. Outputs render a name a command can consume, never a raw id: a local account its `handle`, a kernel account its petname, falling back to its key when unbound. Resolvers are **contextual** — the user position takes a handle or account id, the kernel position a petname or key, shapes being disjoint. Only `show`, `rename`, `suspend`/`unsuspend`, `deposit`/`withdraw` consult both, and a bare name matching a handle *and* a petname is refused with `ErrInvalidInput` rather than guessed: money and moderation never pick a target silently. `settle`, `inspect`, and `step --peer` are kernel-only. So transaction responses carry `owner_handle`/`caller_handle`/`target_handle` (not the stored `*_user_id`), step responses `required_caller_handle`, process responses `owner_handle`, ledger responses (deposit/withdraw/transfer) `operator_handle` plus `from_handle`/`to_handle` (each present only when that side is set — `from_handle` absent on a deposit, `to_handle` on a withdrawal), and the peer list carries no internal id — while the underlying immutable records keep their captured `*_user_id` fields (§3). A handle unresolvable at read time (a purged party, §13) falls back to the raw id. `GET /v1/me` is the sole exception: it returns the caller's own `id`.

Endpoint rules (notable rules only; the complete HTTP endpoint list is in `API.md`):

| Endpoint                                         | Rule                                                                                                            |
| ------------------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| `GET /health`                                    | unauthenticated → `{status, handle, public_key}`: liveness plus the kernel's advertised federation identity (§13), so a client sees which kernel it's on before logging in |
| `GET /v1/actions[?owner=&name=&all=]`             | unauthenticated → active public actions; authenticated → active public and local actions plus caller's own active actions (union, deduplicated) — a session caller is a local user, so `local` actions are visible to them but not to anonymous callers; superuser → all owners' active actions; a suspended owner's actions are excluded (§12); `?all=1` includes inactive/private rows in the caller's scope; `?owner=` further filters by that owner's handle; `?name=` filters by name |
| `GET /v1/me`                                     | authenticated `id`, `handle`, `description`, `available`, `locked`, `connectors`, and `connections`; suspended rejected before handler. `connectors` is the directory-grouped consent tree (§8): one node per folder (`directory` = the granted actions' shared folder, the ref up to its last `/`, e.g. `chat` or `chat/inbox`), each with the `connections` (backing upstream account(s), usually one) and the token-free `actions` under it (each: action `owner/name`, requested scopes, `provider_key` of its backing connection, `created_at`). `connections` is the full account inventory (each: `provider`, action count, `unused` flag, `provider_key`, `created_at`), so a zero-grant connection still surfaces as `unused`. The `directory` is a display grouping only — it never gates a credential (the token binding stays per-action and fact-derived, §8 confused-deputy defense); `provider_key` is the opaque account key that also addresses `DELETE /v1/grants?account=`, and is omitted when a grant has no backing connection |
| `GET /v1/grants/plan?selector=`                  | authenticated; expands the selector (§8) over delegated actions the caller may call and returns the consent plan `user connect` walks: groups by `provider_key` — `provider`, scheme, requested scope union, destination host(s), connected/covered status, and per-action `granted` flag — plus counts of skipped actions (loginless or not-callable) |
| `POST /v1/grants/start`                          | authenticated; `{selector, provider, [redirect_uri], [flow]}` begins delegated-OAuth consent for one provider group (§8); when the caller's existing `Connection` already covers the group's scope union, returns `{status:"granted", actions}` immediately with no browser; else `redirect_uri` is any http(s) target the client receives the code at, and code flow returns `{state, authorize_url}`, device flow `{state, verification_uri, user_code, interval, expires_in}` |
| `POST /v1/grants/complete`                       | authenticated as the grantor; `{state, [code]}`; exchanges the code (or reports device-poll `pending`), stores one `Connection`, and mints the group's grants; foreign/expired state rejected |
| `POST /v1/grants`                                | authenticated; `{selector, [provider], token}` stores a static token for a `delegated_bearer` group (§8) — the non-OAuth direct twin of start/complete — minting one grant per action against one `Connection`; `provider` optional when the selector resolves to exactly one bearer group; rejects a non-`delegated_bearer` group and actions the caller may not call |
| `DELETE /v1/grants?selector=` / `?account=`      | authenticated; `selector` revokes the caller's grants matching the selector (grants only; a bare `owner/name` is the degenerate single-action case); `account=<provider_key>` deletes the caller's `Connection` for that provider and cascades its grants |
| `PUT /v1/me`                                     | authenticated password account only; `{[description], [current_password, password]}`; `password` requires `current_password`; at least one field required (`description` may be `""` to clear); returns updated `id`, `handle`, `description`, `available`, `locked`; a kernel account (no password) returns `ErrInvalidState` |
| `POST /v1/transfers`                             | authenticated; `{recipient, amount, [reason], [external_key]}`; moves the caller's own credits to a local recipient (§12); resolves `recipient` by `handle`/key/id; rejects a non-positive amount, self-transfer, missing/suspended recipient, and a peer/proxy recipient; not superuser-gated; returns the ledger entry with `operator_handle`/`from_handle`/`to_handle` |
| `GET /v1/ledger[?limit=&offset=]`                | authenticated; returns the caller's own ledger entries (deposits, withdrawals, transfers where they are `from` or `to`), most recent first, paginated (default 50, cap 200), each with `operator_handle`/`from_handle`/`to_handle` |
| `GET /v1/actions/{id}/ratings[?limit=&offset=]`  | ratings as a public projection `[{value, note, created_at}]` — no rater/transaction/receipt id or signature; readable wherever the action is visible (§4, anonymous for `public`), independent of `active` |
| `PUT /v1/actions/{id}`                           | action-owner update; `visibility` (`private`/`local`/`public`) updatable and does not deactivate; widening beyond `private` on an OpenAPI import requires verified ownership (§8); deactivation rules apply                                               |
| `DELETE /v1/actions/{id}`                        | action-owner delete preserving history                                                                          |
| `POST /v1/actions/import`                        | authenticated OpenAPI supervision import                                                                        |
| `POST /v1/actions/unimport`                      | action-owner import-provenance deactivation                                                                     |
| `GET /v1/processes`                              | process owner's processes, descending `created_at`; each carries `awaiting_receipt` and, when set, `awaiting_receipt_since` (§13) |
| `GET /v1/steps`                                  | authenticated; returns steps visible to caller per `CanListStep`; optional `?process_id=` and `?status=` filters; each step carries `created_by` (the creating action `owner/name`, from its parent trace) beside `action` (the completion target) and `owner_handle` (the process owner / payer, mirroring a transaction's `owner_handle` — the step is the continuation that settles into that owner's transaction, §10); a waiting step also carries `allowed_input` (the derived completion schema `action.input_schema \ keys(partial_args)`, §10) so its required caller can complete it without separately reading a private target action; a waiting step whose required caller is a peer carries `waiting_on_peer` (§13) |
| `POST /v1/steps`                                 | JWT: creates a waiting step; requires `trace_id` (funding trace), `action` (`owner/name` or id, as `/v1/run`), `required_caller`, `partial_args`; the authenticated user must be authorized to use `trace_id` (§4 precondition 4). Capability (§9): `trace_id` comes from the capability and is rejected in the body; the creating authority is the trace's action owner |
| `GET /v1/steps/{id}`                             | `CanReadStep`; returns step fields                                                                              |
| `POST /v1/steps/{id}/complete`                   | JWT or capability (§9); `CanReadStep`; `args` required (`{}` valid); absent gives `ErrInvalidInput`; under a capability the caller is the action owner; returns `result`, `tx_id`, `trace_id`, `step_id` |
| `POST /v1/call`                                  | capability only (§9); the HTTP twin of `juice.call`: a subcall on the capability's trace; body `{action, args}`; no `run`/wallet path; returns `result`, `tx_id`, `trace_id`. Not a user-invocable command (no CLI), like inbound federation |
| `POST /v1/run`                                   | requires `args`; `{}` valid; absent gives `ErrInvalidInput`; action is `owner/name`; never accepts a capability |
| `POST /v1/auth/authorize`                        | unauthenticated; `{handle, password, code_challenge, [redirect_uri]}`; if `redirect_uri` provided → `302` redirect; if omitted → `200 {"redirect": "?code=CODE"}` for programmatic clients |
| `POST /v1/auth/token`                            | unauthenticated; exchanges auth code + `code_verifier` for `access_token` and `refresh_token`                   |
| `POST /v1/auth/logout`                           | refresh token body; missing/revoked gives `ErrUnauthenticated`                                                  |
| `POST /v1/auth/recover/start`                    | unauthenticated; `{handle}`; issues a single-use, TTL-bound recovery nonce (§12); `ErrInvalidState` if the account has no recovery key enrolled |
| `POST /v1/auth/recover/complete`                 | unauthenticated; `{handle, nonce, signature, password}`; verifies the Ed25519 signature over `{recovery_challenge: nonce}` against `recovery_public_key`, consumes the nonce, and resets the password (§12); invalid/expired nonce or bad signature gives `ErrUnauthorized` |
| `GET /v1/transactions`                           | transactions visible to the authenticated user under `CanReadTransaction`                                       |
| `GET /v1/transactions/{id}`                      | full detail to parties; `ErrNotFound` to non-parties                                                            |
| `GET /v1/transactions/{id}/receipt-verification` | parties; remote verification; local tx gives `ErrInvalidState`                                                  |

Transaction list/detail include `rating: {"value":0|1,"note":string|null}` or `null`, visible to all transaction parties.

Admin commands require configured `sys`, reject non-superusers with `ErrUnauthorized`, and stay outside `Call()`. The operator verbs (money, access, federation trust, roster reads) are served on the public TCP API as dedicated routes gated by an `IsSuperuser` check — not a separate surface. Superuser *scope* on the normal read/toggle endpoints (seeing all rows, disabling any action) is enforced the same way, mirroring the existing transaction/step widening. Admin authority is therefore the `sys` bearer token alone; operators keep it secret and expose `serve` only behind TLS or on loopback.

Logs go to stderr and optionally file; stdout is resource payloads only. Configurable format, level, file. Every kernel transition logs start/end; errors include stable codes; script logs include trace ID.

Required log fields:

```text
time level event request_id caller_user_id process_id trace_id action_id tx_id
status duration_ms error
```

Config lives in `config.json` under the kernel's home directory. Juice-family binaries share one root, `$JUICE_HOME` (default `~/.juice`), with one subdirectory per component; the kernel uses `$JUICE_HOME/kernel/`, so the config defaults to `$JUICE_HOME/kernel/config.json` and the database to `$JUICE_HOME/kernel/juice.db` (override either with `--config` / `--db`). This root is fixed and absolute — never cwd-relative — so the kernel attaches to the same identity and signing key wherever it is launched. `$JUICE_HOME/kernel/cache/` is reserved for regenerable data and is safe to delete. Top-level kernel keys: `db_path`, `fee_bps`, `remote_bps` (the serving-side markup — execution tax + risk premium — on inbound federated calls, default 500), `import_bps` (the origin-side import fee retained locally on outbound remote calls, default 500), `exposure_max` (`X`: the maximum gross unsecured receivables this kernel extends across **all** peers combined, in credits; default 1000 so a fresh kernel serves remote paid calls out of the box against a bounded loss; set 0 for prepaid-only), `settlement_trigger` (`Y`: the gross-receivables level at which peers are flagged `settlement_due`; when `exposure_max > 0`, require `0 < Y < X`; default 500), `settlement_quantum` (`Q`: the smallest fee-rational external payment, in credits — an operator sets it from the rail fee `F` and a maximum acceptable fee ratio `r` as `Q = F/r`; default 0 disables the probabilistic residual path, §13), `server_url` (the local base URL the CLI dials for user-facing commands — loopback for driving your own kernel, not a federation identity), `http_callback_url` (the base URL the kernel advertises to dispatched `kind=http` endpoints for capability callbacks, §9; default empty ⇒ derived from the listen address, `http://127.0.0.1:<port>`, which suffices for the co-located case), auth issuer/audience/token TTL, log file/format/level, script timeout and memory limits, plus the federation identity and discovery keys this kernel needs to satisfy §8 and §13: `kernel_handle` (the handle this kernel presents to the network in gossip), `bootstrap_peers` (the peer multiaddrs the transport dials to join the DHT and seed discovery, §13; defaults to the project's public node so `juice serve` works out of the box, and should list at least two independently-operated nodes once a second exists — two addresses on one machine are not redundancy; empty means no seeds and no directory discovery — the kernel skips advertise/enumeration and only syncs its existing counterparties), `credentials_key` (the §8 base64url AES-256-GCM key for `auth_json` and sealed OAuth grant refresh tokens, auto-generated at first boot; each action's OAuth provider config lives in its `auth_json`, so there are no OAuth keys in `config.json`), `allow_local_sources` (escape hatch over §7's private/LAN/link-local/reserved URL rejection for action source URLs, default `false`; loopback is permitted by default and needs no flag), and `remote_retry_interval_seconds` (seconds between passes of the running server's retry loop that re-drives pending remote-proxy calls so a returning peer settles parked calls — and the §13 max-age refund fires — without a restart; default 60, non-positive falls back to the default), `discovery_interval_seconds` (seconds between passes of the discovery loop that advertises the routing-discovery namespace, enumerates its providers, and pulls gossip from those providers, the bootstrap seeds, and the kernel's counterparties into `Kernel` rows, §13; default 300, non-positive falls back to the default), and `peer_retention_days` (days a peer may stay idle at zero balance before it and everything derived from it are purged, §13; default 90, non-positive disables purging). All native-action configuration lives under `native.<action>`; no deeper nesting:

```json
{
  "db_path": "./juice.db",
  "fee_bps": 2000,
  "remote_bps": 500,
  "import_bps": 500,
  "exposure_max": 1000,
  "settlement_trigger": 500,
  "settlement_quantum": 0,
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
    "user-lookup": { "price": 0 },
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

Federation is tested in three tiers. **Unit** (`go test ./...`, offline): kernel federation logic runs against a fake `fed` transport, exercising every §13 settlement rule without a real network. **Flows** (offline, real transport on loopback): the multi-kernel flow suite runs the actual libp2p transport over `127.0.0.1`, with one kernel serving as the bootstrap + relay for the others (every kernel runs the DHT and relay, so no separate seed process) — routing-discovery membership, relayed carriage, and restart-retry are exercised on one machine with no internet. **Real-network check** (release gate for any federation-touching change, not part of `go test ./...`): a scripted flow run from a machine behind a real NAT against one remote peer, asserting hole-punch and relay-fallback paths that loopback cannot reproduce.

Required suites:

```text
user creation; a taken handle and a duplicate owner/name give ErrInvalidInput with no SQL text, while kernel-minted unique keys and CHECK/FK violations stay ErrInternal; replay is unchanged
authentication token validation
user update description; change reflected in GET /v1/me
user update password with correct current_password; old password rejected after change
user update password with wrong current_password returns ErrUnauthenticated
user update with neither description nor password returns ErrInvalidInput
key-only account (no password) UpdateUser returns ErrInvalidState
password below the minimum length rejected at user creation, first boot, and password update
seed-phrase recovery: an enrolled recovery key signs a server nonce to reset a lost password; old password rejected, new works; the nonce is single-use (replay rejected); a wrong-key signature is rejected; StartRecovery without an enrolled key returns ErrInvalidState
CLI recovery key derivation is deterministic and its signed challenge verifies against the enrolled key under the kernel's payload
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
lookup results include action (owner/name), input_schema, and output_schema
lookup returns an all-in price for every result: action.price locally, a discovered hit's serving_price marked up by current import_bps, repricing with no re-pull; a manifest with a negative price or out-of-range remote_bps is skipped at ingest and refused at import
a pinned run succeeds unchanged and is refused with ErrTermsChanged, distinct from ErrInvalidState — balance and process count unmoved — when price, effect, description, either schema, or the stable id moved, including when the new schema rejects the args; a private action refuses on visibility, never disclosing its quote
a discovered hit's quote_hash equals that of the proxy it resolves to
lookup degrades to lexical (BM25) ranking with no embedder configured; a keyword query still finds actions
lookup skips an embedding vector whose dimension differs from the query's (no panic, no cross-space score)
lookup applies CanCall before truncating, so uncallable matches do not starve callable results
lookup query is sanitized: FTS5 operators/quotes in the query are treated as literal terms, not syntax
stats update
CLI commands
CLI primary identifiers are positional natural keys (user=handle, action=owner/name)
CLI human-readable output exposes the same fields as the corresponding HTTP response
logging smoke test
superuser first-boot prompt and config storage
suspended user rejected at authentication
direct buyer can rate transaction
non-buyer cannot rate transaction
native action callable through Call()
every native is registered local, so a fresh kernel gossips no action manifests while still advertising sys as a first-party user, and no peer can call one
sys/random returns value in [0, 1)
wasm script can call sys/random to obtain a random value
sys/web returns status, body, and content_type for a public URL (fake fetcher)
sys/web with missing or empty url returns ErrInvalidInput
sys/web with unconfigured fetcher returns ErrInvalidState
sys/web rejects private/link-local/reserved URLs unless allow_local_sources; loopback permitted by default
loopback action source (127.0.0.1/::1/localhost) permitted by default; private/link-local still rejected without allow_local_sources; a loopback source redirecting to a private/link-local address is still blocked
genuine-loopback client (no X-Forwarded-For) is exempt from the auth/account rate limiter; a loopback peer with X-Forwarded-For is limited by the forwarded client
sys/tinygo/compile returns base64 artifact and hash for valid source; status=failure with diagnostics on compile or import-validation error; empty source returns ErrInvalidInput
action create --artifact registers a wasm action from a pre-compiled base64 artifact
sys/llm/embed returns embedding array for valid text
sys/llm/embed with empty text returns ErrInvalidInput
sys/llm/embed with unconfigured embedder returns ErrInvalidState
sys/llm/json returns value matching output_schema for valid input
sys/llm/json with unsupported output_schema returns ErrSchemaViolation
sys/llm/json with unconfigured structured output returns ErrInvalidState
sys/llm/json rejects model output that fails schema validation
sys/llm/decide returns selected action and validated args
sys/llm/decide fetches action contract from DB by owner/name
sys/llm/decide rejects unknown action reference with ErrNotFound
sys/llm/decide rejects returned args that fail action input_schema
sys/llm/decide with unconfigured tool calling returns ErrInvalidState
sys/llm/decide with no valid selection returns ErrExecutionFailed
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
step creation binds visibility against the creator (trace action owner), not the required caller: a private/local action can be parked for a caller who could not call it directly; a creator that cannot call the action is rejected
step completion ignores visibility: an action narrowed to private after parking still completes (never bricked); only a liveness failure (deactivation/suspension) resets to waiting
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
startup reads config.superuser_handle to confirm first boot and identify sys
second rating on same transaction rejected with ErrInvalidInput
rating record created in ratings table, transaction row unchanged
action ratings projection is {value, note, created_at} only; public readable anonymously, private only by owner, deactivated still readable
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
transfer to a peer/kernel account (public_key set) rejected with ErrInvalidInput
transfer by a suspended caller and to a suspended recipient both rejected
transfer is idempotent over external_key: a replay returns the existing entry and moves no funds twice
user ledger lists the caller's deposits, withdrawals, and transfers (from or to), most recent first, with from_handle/to_handle
transfer resolves the recipient by key (global name) as well as by handle
receipt created atomically with successful transaction commit
receipt created atomically with failed transaction commit
a transaction's and its receipt's reason is the failure code — never an upstream URL, body, or SQL — and a peer's reason is never adopted locally
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
grant selector matches by path segment (tom/brief matches brief and brief/x, never briefing; trailing /* stripped; a full owner/name is the degenerate one-action selector)
consent plan groups the caller's delegated actions by provider_key, filters by CanCall, and marks connected/covered per group
connecting an action whose provider connection already covers the scope union grants instantly with no browser round-trip
one consent covers a multi-action provider group with the union of its scopes; a second action reuses the one connection
re-consent for a wider group widens the stored scope union so earlier grants keep coverage
disconnect by selector revokes only the matching grants; disconnect by account deletes the connection and cascades its grants
a connection with zero grants is listed unused in /v1/me and is never auto-expired
OpenAPI import/unimport flow for API-owned actions
OpenAPI import compiles parameters and JSON body into one input schema
OpenAPI activation rejects incomplete schemas or missing descriptions
OpenAPI public activation requires ownership proof
OpenAPI import affects only matching OpenAPI-provenance actions
OpenAPI import preserves Action.id, deactivates on contract change, and resets current stats
remote import/unimport flow for signed manifests
remote import preserves Action.id, deactivates on manifest contract change, and does not overwrite local Stats
remote import initializes local Stats to defaults
remote import names actions owner-qualified (addressed owner@peer/name); two owners on a peer with the same action name do not collide
petname lifecycle (§13): discovery creates a Kernel row but no account and no petname; a verified outbound resolve creates both; binding seeds from the cached nickname, falls back to k-<key8>, suffixes -2 on collision, and is exact (rejecting, not suffixing) on an explicit rename; a nickname change never retargets a bound petname; a key- or id-shaped petname is rejected; inbound call and deposit by key provision without binding
handle and petname may hold one string at once, each resolving only in its namespace; a bare name matching both is refused on the mixed commands; renaming a kernel account by id is rejected; an unbound kernel account renders as its key; admin users lists local accounts only
concurrent first use of one key converges on one petname and one account; distinct keys racing for one nickname get distinct petnames; suspend-by-key provisions and freezes atomically; a suspended kernel account survives retention; a kernel account holds no handle, password, or recovery key (DB CHECK); deleting a Kernel row while an account references it is rejected
migration 037 preserves every account id and transaction party, leaves no schema reference to `users`, and keeps foreign_key_check empty
kernel about: sys's description surfaces as gossip `about` and in admin identity; admin inspect renders action descriptions carried in gossip/manifests
admin deposit/withdraw/suspend resolve a peer by key (global name) as well as by handle
fresh boot with no configured handle derives a distinct k-<key> kernel handle, never sys
remote manifest signature is Ed25519 over canonical JSON excluding signature
remote proxy transaction has target_user_id = kernel account id
a peer's first signed call or a deposit by key provisions a zero-balance billing account
a call whose proxy is absent or inactive re-resolves and reactivates the row in place, id preserved
a federated call dispatches the peer's stable action id on first dispatch and on retry, so a stale cached reference or a renamed remote owner never parks the caller; an inbound call naming a cached remote_proxy id is refused, and migration 038 normalizes legacy public proxies to local
changing import_bps reprices imported actions with no re-resolve, while an already-dispatched call settles at the rate frozen on its dispatch record; rows and steps predating the snapshot heal on next funded use
a federated call binds recipient and expected_contract_hash; a contract-hash mismatch is refused pre-admission with a signed refresh_proxy rejection that deactivates the proxy (conditional on the dispatched hash), and the next call re-resolves and succeeds
a refresh_proxy rejection carrying a success/charged receipt is malformed and quarantines; a funding (402) rejection carries no refresh_proxy and leaves the proxy active
manual enable/disable, update, and delete on a remote_proxy action are all rejected with ErrInvalidState; the row stays active+local (a hand-set public proxy would pass a peer's CanCall)
a proxy is addressable only kernel-qualified (owner@kernel/name) or by raw action id; the legacy bare mount form (mount/owner/name) does not resolve
a directory-only discovered kernel (never a peer) is evicted with its discovery docs and evidence once stale past peer_retention_days; a fresh one and a peer-backed one survive
a sigil-prefixed handle is rejected at every boundary (user create, action ref, grant selector, step required-caller, manifest owner_handle), never stripped; owner@kernel/name and raw ids are unaffected
a signature-valid inbound call from an unknown key is lazily provisioned a zero-balance account; a price-0 call then succeeds, a priced one gets an insufficient-funds rejection
inbound call to a known-but-non-executable action (inactive, local/private so a peer fails CanCall, suspended owner) gets a signed zero-charge rejection receipt the caller settles on immediately
suspend freezes a peer's inbound calls (signed rejection receipt) and is reversible with unsuspend; there is no denied_at
gossip serves identity, first-party users (sys + public-action owners), own signed manifests, and one evidence page per pull with the catalog snapshot on every reply; evidence is never relayed (learned evidence is not re-gossiped); gossip carries no membership (no `known_kernels`), keyed by public key with no URLs
a gossip pull is verified only when authenticated as the dialed key (reply `public_key` matches) with a valid bare handle; a key-mismatch or invalid-handle reply is skipped, never accumulated
discovery is libp2p routing discovery: a DHT-client kernel connected only to a bootstrap server advertises the namespace, a second DHT-client kernel with no prior connection to it enumerates the namespace through that server, receives it with at least one address, and opens a gossip stream to it by public key (the loopback test forces DHT client/server modes, since AllowPrivateAddrs otherwise makes every node a server)
discovery creates no account and binds no petname: enumerating and pulling a kernel writes Kernel/DiscoveryDoc only, never an account or balance
admin peers merges counterparties and discovered kernels by public key, excludes this kernel, shows an account only for counterparties, dedups a kernel present in both sources into one row, and --all adds suspended counterparties
resolving a peer's action does not import that peer's own imports (no transitive re-export); manifests and gossip exclude remote_proxy actions
resolve-imported proxies get visibility=local; manifests and gossip serve only visibility=public actions (a local own action is excluded from both)
the visibility column rejects any value outside {private, local, public}
offline peer: inspect degrades to local data + unreachable, a cold call fails fast (ErrPeerUnreachable), peers/identity work locally
a validly-signed gossip manifest is indexed as a discovery doc and a badly-signed one is skipped; sys/lookup surfaces a discovered-but-unresolved remote action as owner@key/name to a local caller; sys/user-lookup returns the stable principal_id plus a display reference
evidence: a receipt hashes identically on both sides of the remote-receipt join (canonical JSON, not raw wire bytes); a remote rating counts only when the serving side names the rater's kernel as counterparty and the hashes join, else it is unverified; a late rating attaches (not equivocation) and two different ratings under one (issuer, receipt_hash) equivocate; the per-peer evidence cursor persists across passes; a signature verifies only under its own domain on the wire; the gossiped rating is a signed projection carrying no rater or transaction identity
outbound leg-(b) evidence covers every receipt-settled admitted execution (success or failure, rated or not, price 0 included); a never-dispatched (ErrPeerUnreachable) settlement, a signed zero-charge rejection (remote tx_id == the call's idempotency_key), and a quarantined receipt each produce no gossip evidence; an attached rating stays optional
admin inspect derives two evidence views about a subject kernel — an execution summary from issuer=subject rows and per-issuer counterparty experience from the rest, never folded together; local Stats are unaffected by evidence; PurgePeerCascade removes the peer's discovery docs and evidence
proxy call locks q = sr+ceil(sr·import_bps); success settles charge+premium to the peer row and ceil((charge+premium)·import_bps) to origin sys, refunding the difference
remote failure with charge refunds q−(charge+premium); settled remote work stays paid
failure counts against proxy stats regardless of charge
serving markup (premium) credited to the serving kernel's sys; charge+premium credited to the kernel account (the bilateral payable); origin retains its import fee to origin sys; caller kernel earns only that local fee, never the premium
local caller pays no remote premium; a local call to a local action is unaffected
inbound paid call admitted iff global gross receivables G=Σmax(0,−available) stays ≤ X after reserving the worst-case obligation; default X=1000; X=0 ⇒ prepaid-only; two peer identities cannot jointly exceed one X (Sybil-proof)
exposure admission is atomic under concurrency: parallel inbound calls cannot together breach X
serving-markup reserve is snapshotted on the root trace and released at settlement from that snapshot (not the in-memory request), so crash recovery and forced closure never leak it in owner.locked
probabilistic residual settlement: a pay outcome moves no balance and leaves the debt on the row (G unchanged, no lottery outcome can cause G>X); a clear outcome extinguishes the debt immediately; E[payment]=d
a paid outcome is pending_cash until admin settle --cash records the rail move (deriving each kernel's side): clears d, books ±(Q−d) on sys, records external cash Q; the debtor leg rolls back if sys reserve < Q−d (no partial writes); replay is a no-op; a pending pay blocks the debtor's further credit-drawing calls
cash finalization is the only non-conservative settlement move (Δrow+Δsys=Q=cash); clear/pay records are conservative (Δrow+Δsys=0); E[variance]=0
settlement outcome is deterministic in (settlement_id, s, n, Q, d) and idempotent by settlement_id: a replayed finish (even a ground nonce) returns the first record, never a second signature
creditor non-reveal past expiry lets the debtor clear the debt for zero with the signed open record; the expired-open reconcile makes the creditor apply the same clear-for-zero (idempotent); both ledgers end identical
settle payload domains (settle_open, settle_finish, settle_reconcile) are disjoint from each other and from step_auth
sys/transfer is an ordinary priced action (effect="transfer") with a deferred receipt-backed transfer effect; execution price funds from the trace (taxed), the delivered value funds from the immediate caller C's own balance (untaxed) — two channels, different wallets, never mixed
the value effect is staged reserve-first from C.available (atomic with funding) and committed by the kernel at settlement, not by the handler; value-bearing is decided by the SIGNED manifest effect field, never the action name; sys/transfer is local, so a kernel-qualified target is rejected and no peer resolves it
receipt.value ∈ {0, amount} (all-or-nothing); channels audited separately: premium=ceil(charge·remote_bps), value_premium=ceil(value·remote_bps) (never folded); a receipt short-changing value or mispricing either premium is quarantined (reserve kept LOCKED, not refunded)
value transfer is non-transitive (a peer caller naming a remote beneficiary is rejected); the serving markup applies to value; the origin import fee applies per channel
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
a cold resolve by key over the fake transport imports one action and creates a zero-balance proxy account
outbound call whose first dispatch provably never connects settles immediately as ErrPeerUnreachable with a full refund; a dispatch that may have reached the peer never fail-fasts — allocation stays locked, process stays open, retry resumes on reconnect; the retry loop never settles a parked trace on a connection failure
signed zero-charge 402 rejection settles as ErrPeerUnfunded with the peer handle in meta, never as the caller's own insufficient_funds
gossip response carries counterparty_balance only for an authenticated known non-suspended peer; absent for strangers, suspended keys, and anonymous pulls
successful peer gossip pull persists peer_last_seen and peer_credit; peer sync runs with empty bootstrap_peers; admin peers surfaces both
peer step list returns only steps whose required caller is the requesting peer; another peer sees none; an unknown key gets an empty list and is NOT provisioned an account
peer step complete resumes the step as the peer's account (role law: caller_user_id = kernel account), settles on the serving kernel, and is idempotent over (idempotency_key, counterparty): a replay returns the stored result and re-executes nothing
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
a park-invariant violation from BeginStepCall is NOT reported as a lost claim (only the two genuine claim races carry that marker)
a settled failure returns its committed transaction to the caller even when post-settlement bookkeeping fails (the caller was charged); a WASM timeout completion is reported as settled, not as a parked dispatch
a crashed federated call to a LOCAL action completes its inbound idempotency record on recovery, not only a remote-proxy one
a settled-failure replay carries the receipt and the settled transaction ids, and a success whose result contains an "error" field still replays as success (the receipt decides, not the body)
the peer step list carries partial_args and allowed_input and withholds owner_handle, created_by, the target action ref, and every local trace/action id (§5 boundary)
a replayed idempotency record returns the status its stored outcome implies: a settled failure never replays as 200, and the duplicate-in-flight reply carries an error code
CompleteStep reports its outcome: a claim failure carries ErrStepNotClaimed while still presenting code invalid_state and HTTP 409; a rejection before anything settles does not; a failure after a committed transaction returns that transaction alongside the error
```

Direct invariant tests:

```text
ordinary-account balances are never negative (DB CHECK `public_key IS NOT NULL OR available >= 0`); a peer account may go negative, bounded not per-peer but by the kernel-global exposure cap `X` enforced at admission (§13); `locked` is never negative
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
user signs up, deposits arrive (admin), runs a public action by owner/name, gets result;
  process auto-created, auto-closed, exact price debited, provider's net and sys fee observable in tx list
provider creates a WASM action that subcalls two cheaper actions, activates it, a caller runs it;
  caller pays one advertised price, subproviders paid from the provider's budget, provider keeps margin
caller runs an action that fails mid-tree; settled subcall stays paid, remainder refunded,
  process closes, transactions show success and failure with reasons that name the class and carry
  no upstream host or body, locally and across a federated proxy
two users sign up and one is funded; the funded user transfers credits to the other by handle;
  balances move by exactly the amount, both see the entry in `user ledger`, and a transfer
  exceeding the sender's balance is rejected with insufficient funds

— Async / steps —
action parks an approval step addressed to a human and returns; process stays open with price parked;
  the human sees it in step list, completes it; fulfillment runs on parked funds; process closes
external system (webhook) registers as a user, a purchase flow pre-creates a step addressed to it,
  the system POSTs the payload to /v1/steps/{id}/complete; transaction obeys role law
owner force-ends a process with waiting steps; steps cancelled, parked prices refunded, balances reconcile

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
a user attaches one per-user API key to a delegated_bearer directory via `user connect owner/path
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
B's operator deposits A's proxy by key (provisioning + funding it); A runs B's action by key (cold
  resolve caches the proxy); charge+premium lands in A's proxy balance on B, premium to B's sys,
  difference refunded; both sides' tx verify passes all checks
call settles over a forced-relay path: the two kernels are denied a direct dial, the call and its
  signed receipt travel through the relay, and settlement is byte-identical to the direct case
B changes a served action's contract; A's next call gets a signed refresh_proxy rejection, the proxy
  re-resolves to the new contract, and the following call succeeds with the row id preserved
a kernel calls a remote action (unrated), so it gossips receipt-backed trade evidence about that subject; a
  third kernel, connected only to the shared bootstrap server, enumerates routing discovery, finds both the
  caller and the subject, pulls gossip directly from each, sees the caller's evidence about the subject under
  the caller as issuer (creating no account for either), discovers the subject's action via its discovery
  cache (sys/lookup), resolves it directly by key, runs — its own Stats start at defaults and accumulate
a caller resolves and runs a discovered remote action, then continues past the charge: the action is
  still found by sys/lookup (the proxy is indexed, not merely cached), its rendered reference is
  owner@kernel/name with a non-empty owner_handle, re-running it by that reference needs no second
  resolve, and the petname bound itself on first use — including when the peer already held an account
B suspends A: A's next inbound call to B gets a signed rejection receipt; unsuspend restores it
inbound call from an underfunded peer yields a signed rejection receipt the caller settles on
caller runs a NAT-bound peer's action, the peer goes offline mid-call; the caller's allocation stays
  locked and the process stays open until the peer returns and a signed receipt settles it (no timeout settle)
A parks a step addressed to B (via sys/message to B's key); B sees it in `admin inspect`, with its
  derived allowed_input, and completes it with `step complete <id> --peer`; the step settles on A, a second
  completion is refused, and a suspended B is refused until unsuspended

— Federation (real-network release gate; excluded from `go test ./...`) —
from a machine behind a real NAT, resolve a remote peer's action by key, call it both directions with the path
  hole-punched (asserted via `admin inspect`), then force a relay fallback and a restart-mid-call recovery
```

## 16. Design rationale

Stable kernel interfaces and explicit transitions keep correctness independent of transports and adapters. Small function-named packages and per-file tests reduce coupling, keep replaceable implementations visible, and expose coverage gaps. Execution and supervision are separated because execution may err while supervision supplies correction signals execution must not manipulate.

Locking the full subtree price before execution prevents unfunded work anywhere in the tree; failure refunds of the remaining allocation keep accounting conservative, observable, and testable. Input-before-lock and output-before-settlement prevent charging invalid requests or paying malformed replies. Mediated WASM authority permits composition without credential leakage or authorization bypass.

Visibility is caller-scoped, not process-owner-scoped, because access and spend are different questions. The process owner's interests are already protected by the mechanisms that own them — spending authority (§4 precondition 4), the subtree price bound, and the grant binding (§8, which correctly stays with the paying human) — so scoping *access* to the owner too would only misplace it: it would break provider encapsulation (a sold composite could not use its author's private internals) and open a confused deputy (foreign code the owner funds could reach the owner's private actions). Scoping access to the immediate caller mirrors lexical visibility in a programming language and makes both failures impossible with a single correctly-placed variable. Visibility is therefore bound where a reference is written — a call site, or a step's creation — and a step is a closure over that binding: its target is captured in the creator's scope, so completion supplies only the missing argument and re-checks liveness alone. This keeps a step exactly as expressive as the `Call` it defers (a provider may suspend into its own `private` helper), and dissolves the old rule that re-judged visibility at completion — which both under-served that case and could brick a parked step by a later visibility change. The three levels — `private`, `local`, `public` — separate the two consents that a boolean conflated: `local` publishes to a kernel's own users (the operator has already consented by running them together), while `public` additionally exports across federation (the owner's explicit consent), so an imported proxy held `local` is unreachable by a further peer at the call layer, not merely absent from manifests, and resolution is non-transitive by construction.

Subtree pricing makes a price a price: the caller pays one advertised number, composition risk lives with the provider who composed, and the fee taxes each layer's margin (value added), not gross flows. `run` removes process bookkeeping from the user — funding is exact, closure automatic, a process simply the lifetime of a computation and its continuations. Steps are funded continuations: money reserved at suspension makes asynchronous composition safe, restartable, and honest about who pays.

Federation incentives align in both directions: users gain a larger action space and providers gain outside demand, while the operator earns the **serving markup** (`remote_bps`) on inbound calls — reward on the party that actually finances them, since the serving kernel extends unsecured credit (bounded globally by `X`) and carries the float until settlement, and each kernel prices its own risk through `remote_bps`, `X`, `Y`, `Q`; the origin separately retains `import_bps` for its rail cost. Neither side can move the price after signature (the caller locks the authenticated local price), and debtors cut cost by prepaying. Discipline is self-enforcing: breaching `X` refuses further paid calls until settlement, and executing unpaid work loses money twice — in service and in the failure stats that sink its rank abroad. Probabilistic residual settlement is EV-exact (`E[payment] = d`), so long-run solvency needs no write-offs, only reserves against zero-expectation variance. Because gossip carries only trade-backed opinions, reliable behavior compounds into discoverability.

Accounts and kernels are separate entities because they are separately named — a petname over a key, versus a handle — and one column could not carry both namespaces without making them compete. A single peer-to-peer carrier expresses a system whose identity was always a key, never an address: one transport means one code path, one failure mode, and one thing to verify against §13, and it lets a home-router kernel federate identically to one on a public host. HTTP federation was the lone layer dragging advertised addresses, `.well-known` documents, and peer-URL SSRF rules into a key-native system; removing it deletes a whole class of configuration rather than maintaining a parallel path. The honest cost: a small class of public helper nodes (bootstrap, relay) is load-bearing infrastructure for all federation — they pass encrypted bytes and hold no Juice data, but someone must run them, unpaid and out-of-protocol for now. Settlement never knew the carrier, so moving it onto the transport changes how bytes arrive and nothing about who pays or what settles.

Replaceable lookup ranking permits research changes without changing kernel semantics; fixed stats plus optional tags preserve deterministic baseline metrics while isolating experiments. Latency is derived from transaction timestamps, not cached on traces, so buyer-experienced wall time is always current — a subtree query reflects late descendants without a retroactive write — at the cost of that query at read time.

Prices follow the price-snapshot pattern: a catalog price derives from current components; the funding boundary freezes the total with every input, so settlement and audit never read live configuration. Fixed `sys` and signing keys give stable system action names and verifiable receipts/manifests. Structured logs make production operation and research reproduction reconstructable.

OpenAPI import as supervision keeps registration low-friction while preserving uniform execution. Federation imports signed action contracts without leaking implementation. Contract-change deactivation prevents silent interface drift for callers and LLMs. Unimport deactivates rather than deletes so history remains auditable. Peering is deliberately worthless (permissionless, zero balance) so that trust lives in deposits and reputation is carried as signed, independently verifiable evidence rather than opaque third-party aggregates: attribution and immutability are provable, honesty is not, so a receiver weights evidence by its own settled experience and distinct-issuer trade history — the only Sybil-resistant signals — and the exact weighting is the ranking layer's experiment, not kernel semantics.