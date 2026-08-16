# Juice: user stories, product constraints, and design decisions

Version: 0.12
Codename: `juice`

INSTRUCTIONS FOR EDITING THIS FILE:
- Four classes; **one owner per fact** — state each fact once at its owning entry, cross-reference by id (U/G/P/D), never restate.
- The section structure and numbering are fixed; never restructure or split content into other files.
- Every design fact carries a `[→U/G]` trace or a `[policy]`/`[convention]` marker; untraced facts are the simplification surface.
- §10 is a lag ledger: a decision folds into the body as current contract, and §10 records only what still diverges in the source contract or the code, each line deleted as its lag closes. An empty ledger means document, contract, and system agree.
- Be terse, measured in **characters** — except the user stories, which are complete, precise English sentences: they are the standard.
- Timeless present tense; no changelog framing; measure the character delta after every edit.

## 1. Taxonomy

1. **User story** — actor, goal, observable outcome at the product boundary. A statement is a story only if it survives replacing the database, transport, process model, and internal APIs.
2. **Product constraint** — a security, economic, durability, privacy, or interoperability property every implementation must preserve; acceptance criteria across stories. Stories and constraints are stable across implementation and protocol changes; they change only by explicit product-contract revision.
3. **Wire protocol** — what a conforming kernel must speak to another kernel: payloads, signature domains, manifest and receipt fields, settlement equations, idempotency semantics. Normative for interoperability — nothing observable between independently administered kernels is private design, since neither party can change it unilaterally — but **versioned**: replaceable wholesale by coordinated migration without breaking any story (the discriminating test: a story survives a protocol v2; the protocol does not). Much of its content is forced by constraints (signed receipts by U36/G7, recipient binding by replay resistance, idempotency by U35, evidence omissions by U39); encodings, field names, and stream ids are convention. Today the network upgrades in lockstep, so the wire is de facto design; it hardens into this class the moment an independent implementation or non-lockstep operator exists — the standard is written for that moment.
4. **Design decision** — the current mechanism one kernel implementation or operator may replace unilaterally while the stories, constraints, and current protocol version hold: database, store interface, wallet objects, package layout, and the server-authoritative user-facing surface. It may be observable at a kernel's own boundary but never determines federation interoperability.

`API.md` is a derivative interface reference; `README.md`, `ROADMAP.md`, and `docs/` are context. None is a competing contract, and this document governs on conflict.

## 2. Actors

**User** (account, balance, history), **caller** (buys a call), **provider** (owns and sells an action), **composer** (implements an action via other actions), **completer** (resumes a suspended computation), **API owner** (wraps an existing web API), **external system** (webhook client acting as a user), **operator** (`sys`; governs a kernel and its finances), **peer operator** (a remote kernel: financial counterparty, host of foreign principals), **agent** (software that discovers, selects, invokes), **auditor** (verifies history and economics). One party may hold several roles in a flow.

## 3. Basic user stories (one kernel)

Identity and funds:

- **U1** A person can create an account with a memorable handle and a password; the handle is unique on that kernel.
- **U2** A user who loses their password can recover the account by proving possession of a secret only they hold. No email exists in the system.
- **U3** The operator can credit or debit a user's balance to reflect a payment made outside the system; repeating the same request never moves money twice. The kernel owns the ledger; external rails move the real money.
- **U4** A user can send credits to another local user, directly and without fee. Funds move only on their owner's authority — a direct transfer, or the owner acting as the immediate caller of a value-bearing action (U49); foreign code a user merely funds can never move that user's balance.
- **U5** A user can see their balance, locked funds, and complete personal ledger.

Publishing:

- **U6** A provider can register a callable action with a name, a natural-language description, typed input and output, and a fixed price. It starts unpublished; a draft is never accidentally callable.
- **U7** A provider chooses the audience deliberately, with two separate consents: available to this kernel's own users, and exported to everyone across federation.
- **U8** A provider can change an action's terms freely, but no caller ever pays under terms they did not see: a changed contract is refused or re-quoted, never silently applied.
- **U9** A provider can retire an action; all history survives.

Buying:

- **U10** A user can search for actions in natural language and see ranked results, each with description, interface, and one all-in price. Search never discloses actions the caller cannot access, and remains useful without a language model.
- **U11** A user can run an action by name and receive a result. The advertised price bounds the total cost of the call **and everything it does internally**; the user is charged at most that one number.
- **U12** Invalid input is rejected before any charge; malformed output is never paid for.
- **U13** A failed call refunds what was not consumed; sub-work already delivered stays paid.
- **U14** The payer — and only the payer — can rate a completed call, once, with an optional note. Ratings feed the provider's public track record, are immutable, and never change what was paid.
- **U15** Every party can audit: the buyer sees exactly what they paid and why; the provider can reconstruct every credit received from records they may read.
- **U48** A refused or re-quoted call never discloses a private action’s terms.

Economics:

- **U16** On success the buyer pays the fixed price, not a metered cost: unused budget is the provider's margin, never a refund.
- **U17** The operator's fee taxes each layer's margin (value added), never gross flows.
- **U18** Zero-price actions run without any funds.

## 4. Advanced user stories

Composition:

- **U19** A provider can compose other providers' actions inside their own at one advertised price. Sub-providers are paid from the provider's budget; the caller sees one price, one result, one party to rate.
- **U20** A provider's public action may use that provider's own private helpers inside anyone's process — but foreign code a user merely funds can never reach that user's private actions. Encapsulation without confused deputy.
- **U21** A service reachable only as an HTTP endpoint can compose — call actions, suspend — with the same price bound and attribution as in-kernel code.

Asynchrony:

- **U22** An action can suspend awaiting one named party's input, with the money already reserved. That party sees the pending work addressed to them, supplies only what is missing, and the computation resumes and settles. Nobody else can resume it; it cannot resume twice.
- **U23** Suspended work survives restarts. An owner can force-close an abandoned process; all parked funds return.
- **U24** An external system delivers events by acting as an ordinary authenticated user — running an action or completing a step pre-created for it. No special webhook machinery.

Value:

- **U49** A user can move value through an action: an explicitly value-bearing action delivers an exact amount from its **immediate caller** to a named local beneficiary — all-or-nothing, the value itself untaxed and separate from the execution charge. Under composition the value comes from the composing action's owner, never from the funding user. There is no cross-kernel extension: value between kernels moves as settlement on the external rail.

Web-standard integration:

- **U25** An API owner can import an OpenAPI document and get one action per representable operation. Re-import reconciles changes without losing identity or history; unimport deactivates; public exposure requires proof of ownership.
- **U26** A provider can attach upstream credentials that never surface anywhere — not in inputs, outputs, logs, receipts, or any read path.
- **U27** A user can delegate their **own** upstream account (OAuth consent or a pasted personal token) to specific actions, one consent covering a coherent group. The credential is applied only for the exact consented action and only when that user is paying; consent is revocable; the credential is never visible to anyone.
- **U28** A call needing an absent consent is refused before any money moves or reputation is dented, with a machine-actionable signal naming the consent to request.

Selection by agents:

- **U46** An agent can ask a configured model to select one action from typed candidates and propose valid arguments **without executing anything** — planning and spending are separate decisions. Selection uses the kernel's canonical contracts, never caller-supplied imitations; proposed arguments are schema-valid; one dead remote candidate never blocks selection among live ones; an absent model degrades explicitly, never changing money semantics.

Federation:

- **U29** A user can call an action on another kernel from their **local** balance at a deterministic, pre-advertised, all-in price — no remote account, prefunding, registration, or approval.
- **U30** A provider gains outside demand by doing nothing beyond marking an action public.
- **U31** Both operators are compensated: the serving side earns a markup for serving and financing the call; the origin side retains an import fee. Every component is advertised before execution; nobody can move the price after it is authorized.
- **U32** Serving strangers is bounded-risk: an operator caps total unsecured credit across **all** peers at once, and minting additional identities cannot multiply that exposure. Sybil-proof by construction.
- **U33** Kernels settle obligations over any external rail the operators choose; the kernel meters who owes whom and records the payment. Debts too small to pay economically still settle fairly, with zero expected loss to either side.
- **U34** A home kernel behind NAT federates identically to one on a public host: no advertised address, port forwarding, or hosted document.
- **U35** Intermittent connectivity is safe: a call to an unreachable peer either fails fast (provably never sent, full refund) or stays parked until signed proof of the outcome arrives — never double-charged, never silently dropped, visible with its age.
- **U36** Any remote charge can be verified locally, offline, against signed evidence.
- **U37** Moderation is one uniform lever: the operator can suspend or unsuspend any account — human or kernel — reversibly, preserving all data.
- **U38** The operator can give peers memorable local names, securely: a remote party's self-chosen name can never seize a local name; an unnamed peer is fully usable by key.
- **U39** Reputation crosses kernels as independently verifiable evidence of real trade, never opaque scores. A consumer can distinguish trade-backed ratings from unverified claims and detect a party that tells two stories. Evidence names the subject (kernel, action) and at most the counterparty **kernel**; it never carries the rater's identity, any payer or caller user identity, transaction, trace, or process ids, payload hashes, amounts, or beneficiaries. Public evidence by design, privacy-preserving by design.
- **U40** A user can discover actions and users across the network through local search; anything discovered is verified with its home kernel before money moves. A stale directory affects only search.
- **U41** Work can park across kernels: a step addressed to a remote party is visible to and completable by exactly that party. The peer is served the request, never the requester — no local identities cross the boundary.
- **U47** Federation is non-transitive: an imported remote action is never re-served to a further kernel; reaching a provider requires resolving from its home kernel. No chains of unverifiable intermediaries.

Operations:

- **U43** First boot yields a working, network-joined kernel after supplying only a password and a kernel name; without a name it fails before writing anything. The operator recovers the superuser password like any user.
- **U44** The operator can observe everything material — parked remote calls with age, peers with balances, evidence about any subject, and unsettled bilateral obligations — and intervenes with the same commands users have, widened in scope, plus the money-and-trust verbs only an operator performs.
- **U45** Every user and operator workflow exists as both CLI and HTTP with machine-readable output; the CLI is a pure client of the server.

## 5. Cross-cutting guarantees

- **G1** Conservation: funds move only by enumerated operations; every monetary transition commits atomically with its immutable audit record; a call settles exactly once — no path spends the same allocation twice; ordinary balances never go negative.
- **G2** Attribution: every call durably records payer, immediate requester, and payee.
- **G3** Immutability: transactions, receipts, and ratings never change after commit; no purge deletes the ledger.
- **G4** Crash safety: restart loses no funds and never duplicates a committed effect; interrupted work refunds; work possibly executing remotely is never presumed dead — it is re-driven under its original identity until signed evidence settles it.
- **G5** Secrecy: credentials, tokens, and capabilities never appear in any readable surface.
- **G6** No unfunded work anywhere in a call tree; input validation precedes locking, output validation precedes payment.
- **G7** Offline verifiability: signed artifacts are storable and verifiable without the channel that carried them.
- **G8** Execution/supervision separation: execution code can never rate outputs or propagate ratings; supervision never routes through `Call()`. Reputation cannot be manufactured by the thing it judges.
- **G9** Egress confinement: kernel-mediated outbound HTTP — action sources, token endpoints, `sys/web` — cannot reach private, link-local, or reserved networks unless the operator opts in; loopback is permitted. The kernel is not a proxy into anyone’s LAN.

## 6. Wire protocol [P]

Normative for interoperability; versioned; today upgraded in lockstep.

### P1 Identity and signatures
- One Ed25519 keypair per kernel; the federation network identity derives deterministically from it; no second key. Keys render as 43-char base64url (32 bytes). [→U34, U36]
- Every Juice payload signs `"juice/v1/" + domain + "\n" ‖ CanonicalJSON(v)` (RFC 8785 JCS), one fixed domain per kind: `receipt, rating, manifest, evidence_receipt, fed_call, step_complete, step_list, step_auth, settle_open, settle_finish, settle_reconcile, settlement_record, capability, recovery`. A signature valid in one domain never verifies in another; the transport-handshake domain (libp2p's) is disjoint from all. ASCII property names make UTF-8 ordering equal RFC 8785 UTF-16 ordering. [→G7]

### P2 Contract digest
`quote_hash = SHA-256(JCS({action_id, effect, description, input_schema, output_schema, price}))` over the **stable** id, so a discovered hit and the proxy it resolves to hash identically. Served on action reads, listings, and lookup; a pinned run mismatching is refused with `ErrTermsChanged` carrying current hash and price in meta. `effect` is included because it alone decides value-channel engagement. Binds the execution quote, not the implementation or live-read value fees. [→U8]

### P3 Streams
Versioned libp2p streams: `/juice/fed/call/1` (inbound call), `/juice/fed/step/1` (step list + completion), `/juice/fed/resolve/1` (open read-only single-action manifest or handle→PrincipalID resolution — the sole cache-fill path), `/juice/fed/gossip/1` (cursored catalog + evidence), `/juice/fed/settle/1` (residual settlement). No inspect protocol — inspect is served by gossip. A payload change keeps its stream id and surfaces as a signature failure; the network upgrades in lockstep. Timestamp windows: request age ≤ 5 min (call, step). For every signed request carrying `counterparty`, the transport-authenticated peer key must equal it. [carrier: D12]

### P4 Inbound call
- Request signs `JCS({action, args_hash, counterparty, expected_contract_hash, idempotency_key, recipient, timestamp})`; `action` is the stable id, never a handle-bearing ref (rename-proof); `recipient` is the serving key (a captured request cannot replay to a third kernel); `args_hash = SHA-256(raw body)`. Receiver verifies signature, recipient = own key, body hash, timestamp. A valid unknown key is provisioned a zero-balance account (handshake-free peering); a suspended key is refused. An inbound call naming a cached `remote_proxy` id is refused. [→U29, U37, U47]
- **Signed rejection receipt** (status failure, charge 0) when the receiver can determine non-execution, carrying the action id so the caller settles at once. `refresh_proxy` is set **only** on contract-hash mismatch; a non-executable action (inactive, non-public, suspended owner) or a funding/exposure rejection omits it; a genuinely absent action stays a plain error (caller stays pending). A rejection receipt's `tx_id` = the call's `idempotency_key`, distinguishing it from a 0-price execution. [→U8, U35]
- Idempotency: insert pending before resolution and hash check; unique `(idempotency_key, counterparty)`; completed replay returns the stored receipt; pending replay 409; expiry 24 h. [→U35, G4]

### P5 Receipt
Fields: `id, issuer_user_id, tx_id, trace_id, action_id, caller_user_id, process_id, args_hash, reply_hash, status, gross, net, fee, charge, premium, value, value_to, reason, started_at, created_at, signature`. `args_hash`/`reply_hash` = SHA-256 over JCS args/reply. `gross` = full allocation; `fee + net` = amount remaining at settlement; `gross − fee − net` = committed downstream; failure has `fee = net = 0`. `charge` = actually drawn: `= gross` success, `≤ gross` failure (settled descendants stay paid), `0` rejection. `premium = ceil(charge·remote_bps/10000)`. `value ∈ {0, amount}` (all-or-nothing), local and untaxed, so no premium rides it; JCS omitempty keeps pre-transfer receipts verifying. Signature Ed25519 over canonical JSON minus `signature`. Exactly one receipt per committed call; `started_at` call start, `created_at` settlement. [→U36, G1, G7]

### P6 Manifest
Required fields: `action_id, owner_id, owner_handle, name, description, input_schema, output_schema, price, remote_bps, kind, artifact_hash, stats, updated_at, signature` (Ed25519 over canonical JSON minus signature, against the serving key). Contract fields (drift detection): `action_id, artifact_hash, description, input_schema, kind, name, output_schema, owner_id, price, remote_bps`; `owner_handle` is display, never contract — a rename never deactivates a proxy. A manifest declares no `effect`, and it is not a contract field: no remote action is ever value-bearing. The key survives in the hash payload at its empty value, so no cached proxy is re-keyed. Served only for own, active, `public`, non-delegated-auth actions, `kind ∈ {http, wasm, native}` — never `remote_proxy` [→U47], never delegated-auth actions (a peer's single account can neither consent nor hold a per-caller token). Invalid signature, negative price, or `remote_bps ∉ [0,10000]` skips the action. Manifest stats and `updated_at` affect neither contract comparison nor local Stats. Descriptions and schemas are the canonical interface for importer lookup and LLM function calling.

### P7 Cross-kernel pricing and settlement
`sr = mp + ceil(mp·remote_bps/10000)` is the quoted cross-kernel maximum; the origin adds locally `q = sr + ceil(sr·import_bps/10000)` (import fee retained, never on the wire). The actual obligation derives from the signed receipt's `charge` below. `remote_bps` prices the serving side's credit, default, and settlement variance; the caller can never be charged more than the authenticated `q`. Settlement occurs **only** on a signed remote receipt — never on timeout:
- success: `paid = charge + premium` → peer row; `importfee = ceil(paid·import_bps/10000)` → origin sys; refund `q − paid − importfee`.
- failure: `paid = charge + premium` (premium on actual charge, 0 at charge 0); importfee 0; refund `q − paid`; counts against proxy stats regardless of charge.
- signed rejection: full refund; `refresh_proxy` deactivates the proxy iff the row still holds the dispatched hash (a stale dispatch spares a refreshed row). A rejection is identified by `tx_id` = the dispatched `idempotency_key` (P4) — never by charge or transport status, both of which an executed failure shares — and classifies: 402 → `ErrPeerUnfunded` (operator condition, no refresh); `refresh_proxy` → ordinary failure, the cache heals; otherwise → `ErrUnauthorized`, the peer refuses us. An executed failure stays `ErrExecutionFailed` at any charge.
- never dispatched (first dispatch only; transport provably never connected): ordinary local failure `ErrPeerUnreachable`, full refund; never applied to a retry.
- no receipt: no settlement; allocation stays locked, process open; retry with the same `idempotency_key` until a receipt arrives, bounded by the fixed 24-hour max pending age (not configurable — it is a property of the protocol's record lifetime), past which the call settles locally as a failure with full refund. [→U29, U31, U35]
Local verification (offline): receipt signature against peer key; stored JSON against stored SHA-256; `receipt.action_id = proxy.remote_action_id`; outcome matches; peer-row credit `= charge + premium`; premium formula at the dispatch-recorded `remote_bps`; `charge + premium ≤ q`; import-fee formula at dispatch-recorded `import_bps` (0 on failure); refund equality; args/reply hashes match. Receipt's own `gross/net/fee` are reported, not compared. [→U36]

### P8 Steps across kernels
- `list` signs `JCS({counterparty, recipient, scope: "step_list", timestamp})` → the serving kernel's waiting steps whose required caller is the requester, oldest first, bounded with a `truncated` flag, scoped **in the query** (the peer's own processes must not crowd out completable steps). Per step: `id, partial_args, allowed_input, price, created_at` — withholding the creating action, process owner's handle, target ref, and every local id [→U41]. Pure read: an unknown key gets an empty list and is not provisioned.
- `complete` signs `JCS({counterparty, idempotency_key, input_hash, recipient, step_id, timestamp})`; `input_hash` = SHA-256 of the exact bytes carried (sender normalizes input to the transported bytes); key **derived** = `SHA-256("juice/fed/step/1|" ‖ recipient ‖ "|" ‖ step_id ‖ "|" ‖ input_hash ‖ "|")` so a retry recovers the stored outcome; different input or step derives a different key. Unknown key refused outright. Executes as the peer's account (role law: caller = kernel account); settles wholly on the serving kernel; the requester parks nothing, creates no trace or transaction — failures are typed errors, not rejection receipts. Reuses the P4 idempotency record; a replay is rebuilt from stored result + receipt and discriminates on the **receipt's** status (a result containing an `error` key still replays as success); a stored failure replays as failure with a typed code. Recipient binding prevents cross-kernel replay of list and complete alike. Outbound distinguishes never-dispatched (`ErrPeerUnreachable`) from possibly-executed (`ErrTimeout`, retry under the derived key).
- No payment descriptor rides the step listing, and a step whose action bears the transfer effect is not completable by a peer: value is local to a kernel. The trailing component of the completion key is reserved and empty, so keys stayed stable across that removal.
- `step_auth`: a home-kernel attestation naming `required_caller_remote_id`, demanded when a step is addressed to a remote principal rather than the peer kernel itself — a remote handle rename never mis-addresses a parked step (D4, D15).

### P9 Gossip and evidence
- Every reply carries the full first-party catalog — identity (key, handle, about = `sys`'s description), searchable users (`sys` + owners of active public actions, each `{user_id, handle, description}`), own signed public manifests — plus **one page** of evidence after the request cursor, plus `counterparty_balance` (only for an authenticated known non-suspended key; display-only, never authority). No membership lists, no URLs, no third-party relay: evidence is first-party only; learned evidence is never re-gossiped. [→U39, U40]
- `EvidenceReceipt` projection: `{receipt_hash, subject_kernel, subject_action, counterparty_kernel?, status, started_at, created_at, remote_receipt_hash?}` — no transaction/trace/process/caller/payer identity, no arg/reply hashes, no amounts, no `value_to` [→U39]. Optional rating projection `{rating, note, rated_receipt_hash, created_at}` — no rater or transaction identity. `receipt_hash = SHA-256(JCS(stored receipt))`, the single definition on both sides of the join.
- Two legs of a kernel's **own** receipts: (a) execution evidence for its own manifest-eligible actions (P6, so delegated-auth is excluded here too) (`issuer = subject`; `counterparty_kernel` set only when the caller was a peer kernel), excluding value-transfer receipts; (b) proxy receipts settled from a signed remote receipt for an **admitted execution** — success or failure, rated or not, price 0 included. Excluded from (b): locally-manufactured settlements (never-dispatched, max-age, forced closure, `interrupted`, missing adapter), signed zero-charge rejections (identified by `tx_id = idempotency_key`), quarantined receipts.
- Ordering: ascending `(effective_at, receipt_hash)`, `effective_at` = the rating's `created_at` when rated else the receipt's — a late rating re-surfaces its bundle. Cursor = exclusive high-watermark of the last committed item, advanced only after verified commit; one page per peer per pass; sender eviction may gap, never endlessly replay. A rating without a `rated_receipt_hash` is never gossip-eligible; ingress requires a non-empty hash.

### P10 Residual settlement
- Exact (`d ≥ Q` or `Q = 0`): pay `d` on the rail, recorded via the idempotent `external_key = settlement_id` deposit/withdraw.
- Probabilistic (`0 < d < Q`): debtor-driven commit/reveal. Creditor is stateless: `s = SHA-256(signing_seed ‖ "juice-settle-v1" ‖ settlement_id)`; the debtor carries the creditor-signed *open* record to *finish*. Outcome: pay iff `SHA-256(settlement_id ‖ s ‖ n) mod Q < d` (`n` = debtor nonce; modulo bias negligible for `Q ≪ 2⁶⁴`). The lottery moves no money; the debt stays on both rows until cash. `E[payment] = d`. [→U33]
- `SettlementRecord` binds both keys, `d`, `Q`, mode, commitment `H(s)`, nonce, revealed `s`, outcome, expiry, `settlement_id`; its key-set is disjoint from every other signed payload. Exactly one outcome per `settlement_id` per kernel, ever; a replayed finish (including a ground nonce) is re-served the stored record, never a second signature. Creditor non-reveal past expiry ⇒ debtor applies clear-for-zero with the signed open record as evidence and re-presents it via `reconcile`; the creditor must apply the same (idempotent) or be in provable default. Debtor refusal leaves the debt outstanding consistently, consuming exposure.
- Ledger identity per kernel: `Δrow + Δsys = external_cash`. finish→clear: creditor `row +d / sys −d`, debtor `row −d / sys +d`, cash 0 (immediate, internal). finish→pay: no balance change (records the pending obligation). `--cash`: creditor `row +d, sys +(Q−d)`, cash in `Q`; debtor `row −d, sys −(Q−d)`, cash out `Q`; the debtor leg requires `sys ≥ Q−d` else full rollback. Residual variance is operator variance on `sys`: the creditor gains `Q−d` on pay and loses `d` on clear, the debtor the mirror, so `E[Δsys] = (d/Q)(Q−d) + (1−d/Q)(−d) = 0`; kernels hold reserves against it.

## 7. Design decisions [D]

`[→…]` names the constraint served; an untraced fact is `[policy]` (operator-changeable number) or `[convention]` (arbitrary but frozen) — together the enumerated simplification surface. Wire-visible content lives in §6.

### D1 Implementation frame
Go; `go build ./...` and `go test ./...` must pass. Packages: `cmd/juice` (CLI + server entrypoint), `kernel` (core objects and operational semantics), `store` (persistence interface + SQLite), `script` (WASM execution), `llm` (language/embedding interface), `fed` (federation transport interface + libp2p), `log` (structured logging), `native` (native actions); `kernel` imports no CLI/HTTP/SQLite/wazero/Ollama/libp2p; package names `sqlite, wazero, ollama, libp2p` forbidden (concrete types/filenames may carry them); small package and file counts, no size-only splits; every production file has a `_test.go` with independent tests. [maintainability; no story]

### D2 Execution model
- `run(action, args)` atomically parks exactly `action.price` from the caller's `available` into `locked`, creates the process (holding it as `available`), funds the root trace from it, issues the root call. The process closes automatically when the root call has returned and no steps remain outstanding; closure returns remaining funds and releases the owner's lock. Closed processes cannot call.
- `Call(caller, trace, action, args)` is the sole dispatch primitive, dispatching on `action.kind`, never owner identity; every execution path uses it (run, native, WASM subcall, step completion, imported HTTP, remote proxy). The trace carries wallet, process, and causal parent. [→U11, G2]
- Role law: `owner_user_id = P` (process owner, payer), `caller_user_id = C` (immediate requester), `target_user_id = A` (action owner); cases: root `C = P`; WASM subcall `C` = parent action owner; step completion `C` = required caller; remote proxy `A` = the peer's kernel account. [→G2]
- Preconditions, exact order: (1) `C` authenticated, not suspended; (2) process open; (3) supplied parent trace exists in the process; (4) `C` may use the process — `C = P`, or parent trace's action owner = `C`, or step completion by required caller; (5) action exists; (6) `CanCall(C, action)`; (7) quote pin matches; (8) args satisfy input schema; (9) trace `available ≥ price` (step completion funded by its parked price instead). Precondition 4 governs spending authority, 6 governs access — distinct questions; the 6-before-7 order is why a pin mismatch never leaks a private action's terms [→U48] and terms-invalidating-args still reports `ErrTermsChanged`.
- Transition: require `trace.available ≥ q`; move `q` available→locked; child trace `available = q`, `action_owner = A`; execute; validate output against output schema; commit (success: transaction + receipt + payout + lock release + stats; failure: + refund rollup). A child's resolution always releases the caller's lock — `locked` holds only outstanding commitments.
- Subcall law: `juice.call(target, args) = Call(parent_action.owner, parent_trace, target, args)`; no ephemeral process; same-process spending from the parent trace; trace-scoped authority requires the parent trace's action owner = caller; an over-budget subcall fails `ErrInsufficientFunds` and the parent decides propagation. [→U19]
- Settlement (success): `taxable = trace.available`; `fee = ceil(taxable·fee_bps/10000)`; `net = taxable − fee`; `A` credited net, `sys` fee; unused budget is margin, never refund [→U16, U17]; each kernel taxes only its own layer. Exception: `remote_proxy` settles per P7.
- Refund (failure): remaining `available` plus parked prices of outstanding steps, recursively, cancelled and returned to the caller (root → process → owner at closure); a refund to an already-settled trace reroutes to the process — settlement is final, a trace never regains `available`; failed call charges zero fee/net, exposes the failure class in `reason`; settled descendants stay settled. [→U13, G1]
- Zero-credit processes run zero-price actions [→U18]. Trace relation: root parent null; subcall parent = executing trace, same process; step completion parent = `step.parent_trace_id`. `process_id` determines payment; `parent_trace_id` records causality only. Schema validation: input before locking, output before settlement [→G6]; unsupported JSON Schema forms fail creation/update; a node without `type` is unconstrained by intent.

- Conformance invariants: `user.locked` equals the sum of funds held in the user's open processes; wallet totals (user, process, trace) change only by run, call entry, settlement, refund, step park/unpark, deposit, withdrawal, transfer, closure.

### D3 Persistence and recovery
File-backed SQLite, WAL, behind a store interface `kernel` depends on (never SQLite directly); modernc `_pragma=` DSN forms; deterministic in-repo migrations; tests on temp DBs; no production dependence on in-memory stores. Money paths are compound atomic operations — each monetary transition commits with its audit record in one store call, so atomicity lives at the store boundary, not above it: `BeginRun` (user-wallet park + process creation/funding + funded root trace), `BeginSubcall` (parent-trace move + funded child trace), `BeginStepCall` (unpark step price + completion trace + step waiting→running), `CommitCall` (success transaction + receipt + payout + lock release + stats, + step/idempotency), `CommitFailedCall` (failure transaction + receipt + subtree refund/cancellation + stats, + step/idempotency), `CommitRemoteSettlement` (signed-receipt settlement: charge/premium/refund + transaction + receipt, + step/idempotency), `EndProcess` (cancel waiting steps + return funds + close), `CreateStep` (step record + park), `CreateLedgerEntry` (debit + credit + ledger record), `CreateRatingAndUpdateStats` (rating + rating stats). Reads/supervision are ordinary methods; the inventory is representative, not exhaustive: CreateUser, ReadUser, ReadUserByHandle, ReadAccountByKernelKey, ListUsers, SuspendUser, UnsuspendUser, UpdateUser, RenameUser; CreateAction, ReadAction, ReadActionByOwnerName, UpdateAction, UpdateActionAndResetStats, DeleteAction, ListAllActions; ReadProcess, ListProcesses, ListAllProcesses; ReadTrace, ReadRootTrace, ListTraces; ReadTransaction, ListTransactions; ReadStats, UpsertStats; ReadStep, ListSteps, ResetStepAndRepark, ResetRunningSteps; ReadReceipt, ReadReceiptByTxID; ReadRatingByTxID, ListRatings; ListLedgerByUser; InsertPendingIdempotencyRecord, ReadIdempotencyRecord, CompleteIdempotencyRecordIfPending, DeleteIdempotencyRecord; GetConfig, SetConfig, InitFirstBoot; UpsertKernel, BindPetname, ListKernels, ReadKernel, ReadKernelByPetname, SuspendKernelAccount; DeactivateActionsOwnedBy; CreateOrReplaceGrant, ReadGrant, ListGrantsByUser, DeleteGrant, DeleteGrantsForAction; CreateOrUpdateConnection, ReadConnection, ReadConnectionByUserProvider, ListConnectionsByUser, UpdateConnectionSecret, DeleteConnectionCascade. `UpdateTransaction` is forbidden. Atomic write sets, in full: run = wallet park (available→locked) + process creation/funding + root trace funded from the process; call entry = caller-wallet move + child trace with its allocation; success = transaction + receipt + payout of trace available (net, fee) + caller lock release + metrics + stats; failure = transaction + receipt + subtree rollup (refund to caller available + cancellation of outstanding steps beneath) + caller lock release + metrics + stats; process closure = close + remaining funds returned to owner; deposit = user credit + ledger (null→user); withdrawal = user debit + ledger (user→null); transfer = sender debit + recipient credit + ledger (sender→recipient), one commit; rating = rating record; user update = description and/or password hash; step creation = creator-trace move + step record; step completion = status done + tx_id + price unpark + call transaction + receipt + settlement + metrics + stats; step cancellation = status cancelled + parked price returned. A settled failure always reports its committed transaction to the caller even if post-settlement bookkeeping fails. [→G1, G3, G4]
Recovery at startup: every trace without a transaction settles as failure `reason = interrupted`, deepest first, normal rollup — except a remote-proxy trace with a recorded dispatch, which resumes retrying (allocation locked, process open); steps `running` with null `tx_id` reset to `waiting` with allocation re-parked; `waiting` steps untouched; recovery idempotent. [→G4]
DB checks: `kernel_public_key IS NOT NULL OR available ≥ 0` (ordinary accounts non-negative; peer rows row-unbounded, globally bounded at admission); `locked ≥ 0`; visibility column rejects values outside the enum. [→G1]

### D4 Domain model and action lifecycle
Wire shapes live in P5/P6; local rows and rules here. IDs stable and opaque; action IDs globally unique; prices non-negative indivisible integers; balances indivisible integers.
- **Account**: `id, handle, description, available, locked, suspended_at, kernel_public_key, recovery_public_key, created_at, updated_at`. User accounts hold handle + password and/or recovery key; kernel accounts hold only `kernel_public_key` (FK to Kernel) — disjoint by CHECK (no handle/password/recovery on a peer, so a peer never holds a session credential); `peer(C) := kernel_public_key ≠ null`. `suspended_at` is the single moderation axis, rejected with `ErrUnauthenticated` at every authenticated request [→U37]. Recovery key never authenticates and never makes a peer. `description` is the about text; `sys`'s is the kernel's advertised about. A kernel account is financial counterparty only, never semantic owner/caller [→U41].
- **Action**: `id, owner_user_id, name, kind ∈ {http, wasm, native, remote_proxy}, active, visibility, price, effect, description, input_schema, output_schema, source, auth_json, artifact_hash, remote_action_id, remote_owner_id, remote_bps, base_price, created_at, updated_at`. `(owner, name)` unique; `/` allowed in names, not handles, so `owner/name` is unambiguous. Default private, widened only by update. `effect` is the privileged value-bearing declaration (D18): never accepted from a client, written only by bootstrap registration, so no caller mints a transfer action [→G1]. `auth_json` write-only, encrypted; reads expose only `auth_scheme` + `requires_grant` [→G5]. Active requires non-empty description, valid schemas, field descriptions sufficient for lookup and LLM calling. Inactive ⇒ uncallable.
- Lifecycle: create inactive; validate owner/name/kind/price; WASM validates/compiles only with executor configured; HTTP stores one structured source (verb — default POST — base URL, path, bindings) shared by manual and imported rows, its configuration validated without dialing the endpoint; field routing is explicit when bindings are present, else implicit (path placeholders bound by name, the remainder the body). Source-URL safety per G9: reject non-HTTP(S), RFC 1918, link-local, unspecified, CGNAT at create and activate; loopback permitted by default; `allow_local_sources` widens; a loopback source redirecting to a private address is still blocked. Activate: owner authority, init stats, reject invalid schema/source/artifact/runtime. Update: owner authority; changing source, schema, kind, price, or endpoint deactivates unless explicitly safe; recompute artifact hash; preserve historical source/hash. Delete: owner authority, soft, history preserved [→U9]. `RegisterNativeAction` bootstrap-only, `sys`-owned; normal create rejects `kind=native` (D17).
- **Process**: `id, owner_user_id, available, locked, status ∈ {open, closed}, created_at, ended_at`; bijective with root trace; a longer-lived wallet only because traces settle eagerly — absorbs refunds to settled traces, holds parked steps; `available + locked` = total held across the computation.
- **Trace**: `id, process_id, parent_trace_id, action_owner_id, available, locked, idempotency_key, dispatch_json, premium_bps, premium_parked, value, value_to, idempotency_record_id, created_at`. The call's wallet. `premium_*` snapshot the inbound serving markup (D14); `value`/`value_to` snapshot a transfer effect (D18) — the amount locked from the caller and its beneficiary, riding the trace so every settlement path releases the lock from the snapshot and rates are pinned against config changes [→G4]; all 0 on a non-transfer call. `idempotency_key/dispatch_json` null except on a dispatched proxy trace (D19); `idempotency_record_id` = the inbound record this call answers, set for any kind, so whichever settlement resolves the trace completes the record. `process_id` denormalized. Latency never cached (D22).
- **Transaction**: immutable; `id, process_id, trace_id, parent_trace_id`, the role triple, `action_id, action_name` (captured at creation — history self-contained after deletion), `args_json, reply_json, status ∈ {success, failure}, gross, net, fee, refund, reason, remote_receipt_hash/json, started_at, ended_at`. One per attempted call [→G1, G2]. `refund` is what returned to the caller: on local failure the unspent allocation plus the cancelled steps' parked prices — not `gross − net − fee`, since settled descendants stay paid; on a remote settlement per P7; 0 on local success [→U13, U15]. Remote-receipt fields are null on a local call; a proxy settlement stores the full receipt JSON and its SHA-256 atomically with the transaction [→G7]. `reason` is the failure class only — never an upstream URL, body, or SQL, and a peer's reason is never adopted locally [→G5].
- **Stats**: `uses, successes, failures, rating_count, latency_estimate, rating_estimate, last_used_at`; `uses = successes + failures`; missing stats have defined defaults; no cost estimate (the advertised price is the cost); estimators in D22.
- **Step**: `id, parent_trace_id` (creating/funding trace; derives process; inherited by completion), `required_caller_user_id` (mandatory), `required_caller_remote_id` (nullable: remote principal, completion demands `step_auth`, P8), `action_id, price` (snapshot, parked), `import_bps` (nullable origin-fee freeze), `partial_args, status, tx_id, created_at`. Semantics in D6.
- **LedgerEntry**: `id, operator_user_id, from_user_id, to_user_id, amount, reason, external_key, created_at`; deposit from-null, withdrawal to-null, transfer both set; at least one side non-null; `operator_user_id` is the authorizer — `sys` for a deposit or withdrawal, the sender for a transfer; amount positive; debit requires funds; `external_key` globally-unique opaque idempotency token, never interpreted — kernel stays rail-agnostic [→U3].
- **Rating**: `id, rated_tx_id, rated_receipt_id` (null only pre-receipt), `rated_receipt_hash` (P9's `receipt_hash` taken over the rated receipt — the portable evidence link; empty ⇒ local-stats-only, never gossiped), `rater_user_id, rating ∈ {0,1}` [convention], `note ≤ 1024 B` (nullable, inside the single signature), `created_at, signature`. One per transaction.
- **Kernel**: `public_key, petname, nickname, about, gossip_cursor, last_seen, peer_credit, first_seen, updated_at`. Owns naming state (D15); cursor advanced only after verified commit (P9); `last_seen/peer_credit` display cache only; location never stored; discovery creates no account.
- **Grant / Connection**: D10. **DiscoveryDoc / EvidenceRow**: D16. **IdempotencyRecord**: `id, idempotency_key, counterparty_user_id, receipt_id, status ∈ {pending, complete}, result_json, created_at, expires_at`; cross-kernel only; semantics P4/P8.
- Ledger operations: `Deposit`/`Withdraw` superuser-only, positive amount, atomic credit/debit + entry, idempotent over `external_key` — the withdrawal replay returns the existing entry **before** the balance check, so replays never fail on a dropped balance [→U3]. `Transfer` is user self-service: atomic debit+credit+entry; rejects non-positive, self, missing/suspended recipient, and any peer/proxy recipient (would corrupt the bilateral account); never routed through `Call()` — no composition surface moves user funds [→U4]; not superuser-gated. `UpdateUser`: self-service description/password; password change demands current password (`ErrUnauthenticated` on mismatch); key-only accounts `ErrInvalidState`; atomic. `RenameUser`: superuser-only, sole handle-change path, validates uniqueness, vacates old handle; `sys` and kernel accounts refused. `RenameKernel`: the exact petname bind (D15). All served as public-API routes; money/rename superuser-gated. [→U44]

### D5 Visibility and authorization
`Live(a) := active ∧ ¬suspended(owner)`; `Visible(C,a) := public ∨ (local ∧ ¬peer(C)) ∨ C = owner`; `CanCall := Live ∧ Visible`. Visibility checked where the reference is **bound** — a call site against the immediate caller, a step's target at creation against the creating trace's action owner; liveness checked at every dispatch; completion re-checks liveness only, so a visibility change never bricks a parked step [→U22]. Caller-scoping is lexical-visibility semantics: a provider's public composite reaches its own private helpers in anyone's process; foreign funded code cannot reach the funder's private actions [→U20]. `peer(C)` holds for kernel accounts, so inbound federation reaches `public` only — what keeps a `local` proxy unreachable at the second hop [→U47]. Suspended owner ⇒ uncallable and unlisted; unsuspend restores [→U37]. A local caller may always wrap a private/local action in a public one — encapsulation bounds the dependency surface, not effect reachability.

### D6 Steps
A Step is a funded, partially applied future `Call`. `CompleteStep(caller, id, input) = Call(caller, step.parent_trace_id, action, partial_args ⊕ input)` — shallow merge, `input` overwrites. Funded at creation: price snapshotted and parked from the creating trace; completion never checks funds; the completion's allocation and `gross` are the snapshot, not the current price. Allowed input = `input_schema \ keys(partial_args)`, derived live, never stored — safe because a schema change deactivates the action and completion against a changed/deactivated action resets the step to `waiting`. `required_caller` mandatory — no open completion, no superuser exception; may be a peer account (P8 the only completion path — a keyless account has no session token, and without the protocol the step would strand: the process owner is that same account and `EndProcess` is owner-only). `CreateStep` requires the required caller to resolve to an existing account, so the step is completable. A step never duplicates transaction state: completion timing, result, and failure reason come from the transaction at `tx_id`.
States: `waiting` (completable, price parked, holds process open) → `running` (`ClaimStep` atomic, prevents double execution) → `done` (`tx_id` recorded atomically) | `cancelled` (terminal, no `tx_id`, price returned — on process closure or creating call's failure). Completion preconditions, in order: step exists; status = waiting; process open; caller = required_caller; for **in-execution** completion (D7 host, D8 capability) the step's `parent_trace_id` = the authorizing trace; input satisfies the derived allowed input (a violation rejects, leaves the step waiting, and records no action failure). Any rejection before a transaction resets to `waiting`, price parked. Completion reports its own outcome: a never-taken claim is distinguishable (`ErrStepNotClaimed` marker under code invalid_state/409, only for the two genuine claim races); a post-transaction failure returns the transaction; awaiting-receipt leaves `running` for retry. Startup: `running` + null tx → `waiting`; `cancelled` never reset. Completion role law: owner = step's process owner, caller = required caller, target = action owner. Access: `CanListStep(u) := process owner ∨ required caller ∨ superuser`; `CanReadStep` identical; list ordered `created_at` desc, filters `process_id`/`status`. Richer coordination (first-of-N, quorum, deadline) is user-land; the kernel ships no coordination natives [→minimality]. WASM hosts (arguments JSON-encoded strings): `juice.step_create(partial_args, required_caller, action) → step_id` binds current process/trace; `juice.step_complete(step_id, input) → {result, tx_id, trace_id}` completes as the executing action's owner, trace-confined per the preconditions above. External creation (`POST /v1/steps`) requires precondition-4 authority over the funding trace. [→U22–U24, U41]

### D7 Scripting
wazero; scripts get no ambient filesystem, network, environment, process access, or tokens — only host functions `juice.call, juice.step_create, juice.step_complete, juice.log`; per-execution memory limit, timeout, deterministic cancellation; artifact-hash compiled-module cache; `ScriptAuthority ⊆ KernelAuthority(trace, process, subject)`; store source + artifact; authorized source inspection; activation precompiles; typed compile failures. Network only via `sys/web` (or a pinned http action) through `Call()`, charged and SSRF-bound [→G9]. [→U19, G5, G6]

### D8 HTTP composition capability
Every dispatched HTTP call receives a trace-scoped capability: the platform key signs `{"cap": trace_id}` (JCS, domain `capability`, P1), delivered as a header beside a callback base-URL header; never in payloads, `args_json`, `reply_json`, receipts, hashes, or logs [→G5]. Presented as a bearer on a callback it authorizes composition **as the executing action's owner within its trace** — the WASM subcall law exactly; nothing else (no wallet, no other trace, no supervision). Routes mirror the host surface: `POST /v1/call` ≡ `juice.call` (capability-only, no run/wallet path), `POST /v1/steps` (trace from the capability, rejected in body), `POST /v1/steps/{id}/complete` (iff required caller = owner, trace-confined per D6); each reuses `BeginSubcall`/`CreateStep` — no new money path; over-budget fails `ErrInsufficientFunds`, so the advertised price bounds the subtree [→U11]. Valid only while the trace is unsettled (the settled-once transaction row invalidates it; spend and settlement are mutually exclusive, so concurrent callbacks cannot exceed the bound or race the payout). In-flight capability traces sweep to `interrupted` by ordinary recovery; a retry never mints a fresh trace or capability. Callback URL: `http_callback_url` else derived from the listen address, and when none can be derived neither header is injected (an ordinary leaf dispatch); loopback callback plain HTTP, public requires TLS; headers stripped across host-changing redirects; issuance ambient (leaf endpoints ignore it). Local to the executing kernel; never crosses federation — a composing HTTP action is served abroad as an ordinary `kind=http` action bounded by its manifest price. [→U21]

### D9 Sessions, recovery, first boot
OAuth-style authorization code + PKCE; CLI device or loopback login; short-lived bearer tokens; rotatable refresh tokens; server-side logout revocation; scripts never receive tokens [→G5]. Recovery: client generates a 12-word BIP-39 mnemonic at `user create`/first boot, derives Ed25519, sends the public half only; `recover/start {handle}` issues a single-use TTL nonce; `recover/complete {handle, nonce, signature, password}` verifies over `{recovery_challenge: nonce}` (domain `recovery`), consumes the nonce, sets the password; under the auth rate limiter [→U2]. Password ≥ 8 chars server-side at create/first-boot/update, no composition rules [policy]. First boot: prompts for password and kernel name; atomically creates `sys`, `superuser_handle = sys`, Ed25519 signing keypair, 32-byte JWT secret; enrolls `sys` recovery and prints the phrase once; partial boot rerunnable; requires `kernel_handle` (config → `JUICE_BOOTSTRAP_KERNEL_HANDLE` → repeating prompt; a headless boot without one **fails**, resolved before anything is written so a nameless boot leaves no half-created kernel, and no name is ever derived). Secrets never logged or returned; `JUICE_SECRET_KEY` runtime-only override. Every startup: confirm first boot via `superuser_handle`, verify keys (abort if absent), register/enable natives + reconcile config, soft-delete unregistered natives, run recovery; bootstrap idempotent. [→U1–U2, U43]

### D10 Upstream and delegated auth
Owner-held schemes in `auth_json` `{scheme, config, secrets}`, AES-256-GCM at rest, applied by a replaceable `Authenticator.Apply` adapter at dispatch (kernel imports no scheme): `header, query, bearer, basic, oauth_client_credentials, oauth_jwt_bearer` (RFC 7523 RS256). Scheme + keys validated at create/update; dispatch fails closed on unknown scheme; HMAC signing unsupported (never-active). Secrets excluded from contract comparison [→G5, U26].
Delegated schemes hold the per-caller credential on a `Connection` (`id, user_id, provider_key, sealed_secret, scopes_json, created_at, updated_at`; unique per `(user, provider_key)`; `sealed_secret` AES-256-GCM with AAD `user_id|connection_id`, write-only; `scopes_json` = requested union, authoritative for coverage; rotation updates the one row; `invalid_grant` deletes it and cascades; zero-grant kept, listed `unused`, never auto-expired), the `Grant` holding consent only (`id, grantor_user_id, action_id, connection_id, created_at`; unique per `(grantor, action)`; a pointer to the connection, no secret; re-consent overwrites; deleted on revoke, cascade, invalid_grant, and deactivating update / auth replacement / delete — invalidation never deletes the Connection). `provider_key` fact-derived, never from names: `bearer:<pinned source host>`; `oauth:<token_url>|<client_id>|<source eTLD+1>` — the domain term prevents a hostile action riding a victim's connection with a public client id (confused deputy); the plan surfaces destination hosts. `oauth_delegated` stores provider config only (`auth_url, token_url, device_auth_url?, client_id, scopes, client_secret?`); consent via `grants/start` + `grants/complete` authenticated as the grantor; the kernel performs the code→token exchange, holds ~10-min single-use in-memory PKCE/device state, has no unauthenticated callback route, never dials `redirect_uri` (any http(s) accepted — the provider's registered-redirect allowlist closes phishing). `delegated_bearer` stores only optional `{header, template}` (default `Authorization: Bearer {token}`); token pasted once via `POST /v1/grants`; no exchange/refresh/invalid_grant — a rejected token is an ordinary execution failure.
Binding at dispatch: token applied iff `grant.grantor = process.owner` ∧ `grant.action = executing action`, fetched through `grant.connection_id`; never inherited by subcalls, never across federation (a proxy executes the proxy), never visible to WASM [→U27, G5]. Missing consent → `ErrGrantRequired` pre-lock, structured, before any transaction — no charge, no failure stat [→U28]. Selector: `owner` or `owner/path`, path-segment match (`tom/brief` matches `brief`, `brief/x`, never `briefing`; `/*` stripped; full `owner/name` degenerate), expanded over `CanCall`, grouped by `provider_key`; one consent covers a group with the union of scopes; covering connections grant instantly; re-consent widens the stored union; every connect is a reconcile acting on the delta, which the client must display as the consent act. Token cache in memory, keyed by connection; an upstream 401 drops it, refreshes once, and retries once, the resulting failure returned as-is; token-endpoint fetches under G9. `credentials_key` (D20) is the base64url AES-256-GCM key sealing both `auth_json` and every `sealed_secret`. Delegated actions never in manifests or gossip (P6). Confinement line: data at the run boundary, tokens absolutely.

### D11 Local audit artifacts
Every committed call: one immutable transaction + one signed receipt (`issuer = sys`), atomic (shape P5). Every transaction references a trace; listing a process's traces yields its execution tree, and deleting a trace never removes transaction history [→G3]. Ratings: platform-key Ed25519 over all fields minus signature; only `tx.owner_user_id` rates; once; duplicate `ErrInvalidInput`; non-cascading; supervision, never through `Call()` [→G8]; `rated_receipt_hash` links into evidence (P9). `CanReadTransaction(u,t) := u ∈ {owner, caller, target} ∨ superuser` — captured fields, valid after action deletion; every credit to an owner reconstructible from transactions that owner can read [→U15]. Latency derived, never cached: own = `ended − started`; buyer-experienced = subtree query (D22). Receipts are internal settlement/federation artifacts — no provider-receipt endpoint. Public ratings projection `{value, note, created_at}` readable wherever the action is visible, anonymous for public, independent of `active` [→U14, U39].

### D12 Federation transport
libp2p is the sole carrier behind the replaceable `fed` interface (`kernel` never imports it). Terminology: a **peer** is any known remote kernel; a **counterparty** is a peer with a bilateral account; discovery finds identities/addresses, gossip exchanges catalogs/evidence. Peers addressed by key only — direct, hole-punched, or relayed; streams mutually authenticated by key, yet per-request signatures retained deliberately (offline artifacts, G7). Startup joins via `bootstrap_peers` (default: the project's public node; should list two independent nodes when they exist); routing discovery over a fixed namespace — advertise + enumerate providers each `discovery_interval_seconds`; provider addresses are ephemeral peerstore data, never identity or authority; empty bootstrap ⇒ no advertise/enumeration, counterparty sync only. Every kernel runs the DHT and a circuit relay — a public `juice serve` **is** the network's bootstrap + relay; public nodes bind port 31313 [convention], NAT nodes use OS-assigned ports and are found by key. Discovery grants nothing: execution requires authoritative resolve + admission [→U40]. Inbound limits at the transport: per-source-address where visible, per-peer stream/byte budgets, global cap, stricter for relayed traffic — per-key limits alone are Sybil-insufficient (replaces the HTTP per-IP peer limit). A bootstrap multiaddr inlines its Ed25519 key, so it is inspectable by the same base64url key. The CLI's `--server` is a local base URL, never a federation address. Federation commands are defined for offline peers and fail promptly. [→U29, U34, U35]

### D13 Remote action cache
No subscription: naming `owner@B/name` with an absent **or inactive** proxy row resolves that one action's signed manifest over `/juice/fed/resolve/1`, verifies, and creates/refreshes the `kind=remote_proxy` cache row — the sole cache-fill path, a purely local read B makes no decision on [→U29]. Row: owned by the local peer user; `visibility = local` (a second hop fails `CanCall` [→U47]); wholly kernel-managed — manual enable/disable/update/delete rejected `ErrInvalidState`; the durable peer lever is suspend; `active` is cache state (set by resolve, cleared by `refresh_proxy` or receipt quarantine, removed only by retention). Reconstructible from `(peer key, owner id, remote action id, contract)`; semantic identity is `PrincipalID = (kernel key, owner id)`, never the kernel account. The row stores no source URL and no peer location: the peer is a key the transport resolves (D12). Match key `proxy.owner + remote_owner_id + remote_action_id` — a remote handle rename never re-keys. Reconcile preserves `Action.id`; unchanged contract preserves row and stats; changed updates in place and resets stats; every successful resolve re-enables. `base_price` stores the seller's manifest price so an `import_bps` change reprices at read with no re-resolve (older rows heal by re-resolving). Local price `q = sr + ceil(sr·import_bps/10000)` (P7). Healing is per-call via `refresh_proxy`, not bulk sync. First verified outbound resolve also binds the petname (D15), best-effort, never delaying the call. The legacy mount form does not resolve. Imports never touch manual actions or other provenance; never delete history; stat reset writes defaults only [→U25's reconciliation discipline, shared]. [→U29–U31, U8]

### D14 Exposure engine
Gross receivables `G = Σ_peers max(0, −available)`. Admission of an inbound paid call reserves the worst case `W = mp + ceil(mp·remote_bps/10000)` and admits iff post-reservation `G ≤ X`, atomic with the wallet move (concurrent calls cannot jointly breach) [→U32]. `X` is global — additional identities cannot multiply exposure [→U32]; default 1000, `Y` 500 [policy]; config validation requires `0 < Y < X` whenever `X > 0`; at `X = 0`, `Y` is ignored; `X = 0` ⇒ prepaid-only; a free call adds no exposure and always runs [→U18]. At `G ≥ Y`: `settlement_due` flagged on peers/gossip/identity — signal only, never stops execution; settlement moves external money, hence operator-initiated (`admin settle`) [→U33]. Only reaching `X` refuses, with the signed 402 (P4) surfaced as `ErrPeerUnfunded`. `Q = F/r` from rail fee and acceptable fee ratio [policy]; `Q = 0` disables the probabilistic path. A `pending_cash` outcome blocks the debtor's further credit-drawing calls until `--cash`; prepaid calls unaffected (P10). The inbound premium reserve is parked in the owner's `locked` at admission and snapshotted on the trace (`premium_bps/premium_parked`) so every settlement path — commit, failure, recovery, forced closure, max-age — releases it from the snapshot [→G4]. Only current exposure is bounded; cumulative historical losses require operator reserves. The bilateral position nets as the one peer-account row (positive = prepaid, negative = owed).

### D15 Naming
Petname system (Stiegler 2005; Zooko's triangle: no registry, no ledger). Name classes: **key** (self-certifying, always resolves), **nickname** (self-asserted, never resolves), **petname** (local, unique, resolves in kernel position), **handle** (local, unique, resolves in user position). `PrincipalID = (kernel key, user id)` is stable under renames; resolution captures it beneath friendly names (proxy `remote_owner_id`, step `required_caller_remote_id`, receipts). References: `owner/name` local, `owner@kernel/name` remote (petname or raw key). A petname is local addressing only — never signed, never transmitted, never learned from a peer. Handles and petnames are separate uniqueness scopes sharing one validator: bare, no `@` or `/`, never key- or id-shaped; sigils rejected, never stripped. Lifecycle: gossip/discovery binds nothing and creates no account; a verified outbound resolve binds petname + account; an inbound call or deposit provisions an account without a petname (anti-squatting: only this kernel's own outbound acts bind names); withdraw/unsuspend/settle require an existing account; explicit `admin rename` binds exactly — an occupied petname is `ErrInvalidInput`, never suffixed. Auto-bind preference: existing petname > cached valid nickname > `k-<key8>` [convention]; collisions suffix `-2`; one store transaction converges concurrent first use. Suspend provisions and freezes atomically, so no inbound call meets a briefly-active account [→U37]. Key rotation unsupported: a lost key is lost identity and reachability. Three disjoint syntactic productions — hex UUID in `8-4-4-4-12` form, 43-character base64url key decoding to 32 bytes, or validated handle — one resolver by shape, no sigil; resolvers are contextual (user vs kernel position); only `show, rename, suspend/unsuspend, deposit/withdraw` consult both, and a bare name matching both namespaces is refused `ErrInvalidInput` — money and moderation never guess [→U38, U44].

### D16 Discovery and evidence cache
- **Discovery cache**: a verified gossip pull (authenticated as the dialed key, reply key matches, valid bare handle — else skipped, retried next pass) rebuilds that kernel's `DiscoveryDoc` rows (`kernel_public_key, kind, user_id, handle, description, action_id, name, input_schema, output_schema, serving_price, embed_vec, observed_at`) replace-all: one `kind=action` doc per verified manifest (`serving_price = mp + ceil(mp·remote_bps/10000)`; local read adds `import_bps` — indicative, resolve re-quotes), one `kind=user` doc per catalog user; indexed by the same lexical+semantic machinery as lookup; carries no execution semantics; truncatable with zero effect; purged with the peer [→U40].
- **EvidenceRow**: `issuer_public_key, receipt_hash, subject_kernel_public_key, subject_action_id, counterparty_kernel_public_key, evidence_receipt_json, rating_json, remote_receipt_hash, receipt_created_at, effective_at, observed_at, equivocated`; keyed `(issuer_public_key, receipt_hash)`. Two different valid ratings under one key = equivocation, both excluded from derived metrics [→U39]. Learned evidence never re-gossiped; regenerable; purged with the peer. Retention cap E = 200 most recent per `(issuer, subject kernel, subject action)` [policy]; a rating rides only while its receipt is retained.
- **Derived views** (`admin inspect`): execution summary counts only `issuer = subject` rows (no double count); counterparty experience groups the rest by issuer, never folded in. A rating is **trade-backed** iff its `remote_receipt_hash` equals a subject execution row's `receipt_hash` and that row names the rating's issuer as counterparty; unlinked ratings surface as unverified counts. This proves attribution and immutability, never honesty — own settled experience and distinct-issuer trade history are the only Sybil-resistant signals; how signals weight ranking is the replaceable layer [→U39]. Evidence never overwrites local `Stats`; imports initialize Stats to defaults.
- **One loop** (`discovery_interval_seconds`): each pass enumerates namespace providers and pulls gossip from providers ∪ bootstrap seeds ∪ counterparties; a verified pull refreshes the `Kernel` row (nickname, about — never petname) and evidence cursor; a counterparty pull additionally persists `last_seen` and `peer_credit = counterparty_balance` — display cache, never gating anything, never retention activity (answering gossip is free liveness; a zombie must not be immortal).
- **Retention**: a peer is kept while it holds value (nonzero balance or locked funds), acted recently (settled call either direction, gossip mention, deposit/withdrawal), or is suspended (moderation outlives idleness). Idle past `peer_retention_days` [policy]: purge proxies + stats, discovery docs, evidence rows (issuer and subject), and the `Kernel` row (the account→kernel link is cleared first — the foreign key is restrictive, so deleting a `Kernel` row still referenced by an account is rejected); the credentialless account row remains as ledger anchor — the immutable ledger is preserved and every local counterparty's credits stay reconstructible [→G3, U15]. Purge is reachable only through zero value. Directory-only kernels stale past the same window are evicted with their docs/evidence; a re-pull re-learns them.

### D17 Native stdlib
Standard actions shipped with the kernel: no kernel privileges — equivalents could ship as HTTP/WASM actions; bootstrap-registered under `sys`, not user-creatable/updatable/deletable, callable only through `Call()`, interacting only via injected dependencies and the public entry points, never extending kernel interfaces (G8; borderline ruling 11). All `local`: identical stdlib everywhere makes serving it abroad pure duplication; backed by scarce local resources unbounded at price 0; a kernel with capacity to sell wraps its own `public` action [→U47 rationale]. Startup reconciles config (prices, settings) every boot and soft-deletes natives whose handler is absent from the build. LLM adapters are the replaceable `llm` interface (`Embed`, `Chat`); concrete adapters call Ollama; defaults `http://localhost:11434`, `gemma4:26b`, `nomic-embed-text` [policy]; tests use fakes. Catalog (all price 0 [policy] unless noted; config `native.<x>`):
- `sys/lookup`: `query, limit=10` → `results[{action_id, action ref, description, price, score, input_schema, output_schema, quote_hash}]`. Ranking: BM25 + cosine fused by RRF — replaceable research surface, its storage replaceable too and brute-force cosine acceptable; stats weighting UNDER REVISION, temporarily removed (see `ranking.md`); embedder optional (lexical-only degrade); dimension-mismatched vectors skipped; FTS operators sanitized to literals; `CanCall` applied before truncation [→U10]; discovery-doc third leg for authenticated local callers — a discovered hit renders `owner@<raw-key>/name` with the remote action id, indicative all-in price (`serving_price` + `import_bps`), and identical `quote_hash`; a local proxy shadows its discovery row. Direct (non-`Call`) lookup is diagnostics-only.
- `sys/user-lookup`: `query, limit` → `results[{principal_id {kernel_public_key, user_id}, reference, handle, description, kernel_public_key, score}]`; searches `sys` + owners of active public actions + discovered users; separate surface keeps typed results so `decide` consumes action refs only [→U40].
- `sys/llm/chat`: `messages[{role, content}], system?` → `message {role, content}`; `ErrInvalidState` unconfigured.
- `sys/llm/embed`: `text` (string) → `embedding` (array of numbers); `ErrInvalidInput` empty; `ErrInvalidState` unconfigured.
- `sys/llm/json`: `messages, system?, output_schema` → `value` validated locally; `ErrSchemaViolation` unsupported schema; `ErrInvalidState`; `ErrExecutionFailed` no valid JSON.
- `sys/llm/decide`: `messages[{role ∈ {system, user, assistant, tool}, content?, tool?{action, args, result}}], actions[]` (action refs) → `{action, args, message?}`; contracts fetched from the DB by ref, args validated; selects, never executes [→U46]; kernel-qualified candidates resolve via D13 — dead ones discarded, `ErrNotFound` only when none resolves; bare unknown refs strict `ErrNotFound`; `ErrInvalidState` no tool calling; `ErrExecutionFailed` no valid selection.
- `sys/time`: → `{unix` integer seconds since the UTC epoch, `iso` RFC 3339`}`; `sys/random`: → `value ∈ [0,1)`; `sys/sink`: any → `{}` — the explicit clock, randomness, and no-op the deterministic sandbox withholds (random exists because scripts have no OS entropy).
- `sys/message`: `to, message` → `step_id`; parks a `sys/sink` step for the resolved recipient with `partial_args {message}`; `ErrInvalidInput` unresolvable [→U22].
- `sys/transfer`: `target, amount` → `amount`; `effect = "transfer"`; D18 [→U49].
- `sys/web`: `url` (required string) → `{status` integer, `body` string, `content_type` the response Content-Type, `final_url` the URL actually fetched`}`; GET only; no caller headers/auth (nothing sensitive enters args/receipts/logs); scheme-less defaults https, explicit scheme never downgraded; SSRF per G9; non-2xx returned, not raised; 10 MiB cap [policy]; UA derived from the binary version; empty `url` `ErrInvalidInput`, unconfigured fetcher `ErrInvalidState`, transport failure `ErrExecutionFailed`. The mediated web path for scripts [→G9].
- `sys/tinygo/compile`: `source` → `{status, artifact` base64 WASM, `artifact_hash` SHA-256 hex, `diagnostics}`; empty source `ErrInvalidInput`; price 5 [policy]; SDK prepended so authors write `func Handle(in map[string]any) (map[string]any, error)` while it owns `package`, imports, `alloc`, `run`, and `main`; author compile/validation errors are charged output-failures; missing toolchain `ErrInvalidState` uncharged; registration separate (`action create --kind wasm --artifact`).

### D18 Value transfer — local only
One deferred, receipt-backed `TransferEffect`, staged at admission, committed by the kernel at settlement — the handler only validates. Two channels, different wallets, never mixed [→G1]: execution price from the trace (taxed normally); value from the **immediate caller C**'s own `available` (role law — never an argument, never `P`), delivered untaxed, all-or-nothing. Value-bearing is decided by the signed `effect` contract field alone, never a name; `sys/transfer` is `local`, and the target must resolve to an ordinary active local account at admission (unknown, suspended, peer, or kernel-qualified rejected before funds move). Settlement: success delivers `value` to the beneficiary and refunds the remainder to `C`; failure refunds all. Sender pays fees; beneficiary receives exactly `amount`. `/v1/transfers` (D4 ledger ops) remains the separate direct surface. Serves U49. Because what is locked is exactly what is delivered, the trace carries one number (`value`) and settlement is a single release: to the beneficiary on success, to `C` on failure. No fee, no reserve remainder, no quarantine — those existed only to price value across a boundary it no longer crosses.

### D19 Dispatch records and retry
Outbound proxy call: UUID v4 `idempotency_key` + `dispatch_json` (`mp, remote_bps, import_bps, expected_contract_hash`) recorded on the trace atomically with dispatch — enables restart-proof retry, keys the hash-conditional proxy deactivation, and freezes the fee rates the settlement uses (rows/steps predating the snapshot heal on next funded use; an already-dispatched call settles at its frozen rate) [→U35, G4, U8]. The running server's retry loop (`remote_retry_interval_seconds`) re-drives pending calls so a returning peer settles parked work without a restart, and fires the 24-hour max-pending-age bound; the loop never settles a parked trace on a connection failure. Forced closure fails an in-flight proxy call locally with refund; a remote commit stands there, absorbed bilaterally, surfacing at reconciliation. Inbound: the `idempotency_record_id` threads through the kernel so whichever commit finally settles — normal, retry-loop, max-age, forced closure, crash recovery, local-action crash included — completes the record atomically with the transaction; the service layer deletes the record only where no commit can ever occur, so a corrected retry is not locked out; awaiting-receipt stays honestly `pending` (replay = duplicate-in-flight with an error code). Step-completion keys are derived, not minted (P8). A failed stored settlement stores an error body so replays return failure status. [→U35, G4]

### D20 Surfaces
- **Shape**: the user-facing HTTP/CLI surface is a **public-interface contract** — visible at one kernel's boundary yet server-authoritative and unilaterally versionable, so it is design rather than federation wire protocol. HTTP API primary; CLI a pure client of it (`--server`, default `http://localhost:4040` — the only override); `juice serve` the sole SQLite opener; every command has at least one test; admin/peer commands hit the same public TCP API on `IsSuperuser`-gated routes, authority = the `sys` bearer alone (keep secret; TLS or loopback) [→U44–U45]. Handlers are thin wires; enrichment lives in the service layer, and a handler invokes the kernel directly only where it transforms nothing. Primary identifiers positional by natural key (user = handle, also key/id; action = `owner/name` or `owner@kernel/name`, also id; processes/steps/txs = ids); second mandatory value is second positional. Outputs render `handle`/`owner/name`/petname, never raw user ids; `GET /v1/me` alone returns the caller's own `id`, and a purged party falls back to its raw id. Resource ids remain where the surface identifies that resource; transaction-party, step-caller, process-owner, and ledger-party fields render as `*_handle`; the peer list carries no internal id. Detail views expose the full HTTP shape; lists summarize; `--json` full.
- **Commands** (complete): `serve; user create/me/update/connect/disconnect/transfer/ledger; auth login/logout/recover; action create/show/update/delete/enable/disable/list/import/unimport/stats/ratings; process list/show/end; run; step create/list/show/complete; tx list/show/rate/verify; health; admin users/show/suspend/unsuspend/rename/deposit/withdraw/settle/peers/inspect/identity`. `admin` holds only operator verbs (money, access, federation trust, roster); other supervision is scope on normal commands (superuser sees all rows; may disable any action); `action list` active-only by default, `--all` widens; no duplicate admin read commands. `admin peers` merges counterparties (account, balance, `peer_credit`, `last_seen`) and discovery-only kernels, self excluded, suspended under `--all`, PETNAME and NICKNAME as separate columns (only the first resolves), `—` for unbound. `admin inspect <key|petname>`: identity, public actions (live gossip else cached docs — one projection either way, priced all-in indicative per D16), retained-evidence views (D16), reachability diagnostics (direct/hole-punched/relayed, latency, versions); degrades offline to local data + `unreachable`; writes nothing. `admin identity`: own key, handle, listen addresses. `step complete <id> --peer <key>` completes a peer-held step (superuser scope — the request signs as the whole kernel). `serve` handles SIGTERM/SIGINT: stop accepting, drain, exit; no `juice stop`.
- **Endpoints** (full list in `API.md`): `GET /health` unauthenticated `{status, handle, public_key}`; `GET /v1/actions` scoped (anonymous: active public; session: + local + own; superuser: all owners; suspended owners excluded; `?all=1` adds inactive/private rows in the caller's scope, `?owner=` filters by owner handle — self-match lists all the owner's own rows regardless of `active`/`visibility` — `?name=` filters by name); `GET /v1/me` (authenticated `id, handle, description, available, locked`, suspended rejected before handler; `connectors` = the directory-grouped consent tree — one node per folder (the granted actions' shared path up to its last `/`), each with its backing connection(s) and token-free actions (`owner/name`, requested scopes, `provider_key`, `created_at`) — a display grouping only, never gating a credential; `connections` = the full account inventory (provider, action count, `unused` flag, `provider_key`, `created_at`); `provider_key` also addresses `DELETE /v1/grants?account=`, omitted when a grant has no backing connection); `GET /v1/grants/plan?selector=` (expands the selector over delegated actions the caller may call; groups by `provider_key` — provider, scheme, requested scope union, destination host(s), connected/covered status, per-action `granted` flag — plus counts of skipped loginless or not-callable actions); `POST /v1/grants/start` (`{selector, provider, [redirect_uri], [flow]}`; already-covered scopes → immediate `{status:"granted", actions}`; code flow → `{state, authorize_url}`; device flow → `{state, verification_uri, user_code, interval, expires_in}`); `POST /v1/grants/complete` (`{state, [code]}`; device poll may report `pending`; foreign or expired state rejected); `POST /v1/grants` (`{selector, [provider], token}` — static bearer, `provider` optional when the selector resolves to one bearer group); `DELETE /v1/grants?selector=|account=`; `PUT /v1/me` (password account only; `{[description], [current_password, password]}`; at least one field required, `description` `""` clears; password change requires `current_password`; returns id, handle, description, available, locked); `POST /v1/transfers` (`{recipient, amount, [reason], [external_key]}`; returns the ledger entry with `operator_handle`/`from_handle`/`to_handle`, each present only when that side is set); `GET /v1/ledger[?limit=&offset=]` (own entries, newest first, default 50 cap 200, same handle projection); `GET /v1/actions/{id}/ratings[?limit=&offset=]` (projection `{value, note, created_at}`); `PUT/DELETE /v1/actions/{id}` (visibility updatable without deactivation; widening an OpenAPI import beyond private needs ownership proof); `POST /v1/actions/import|unimport`; `GET /v1/processes` (the owner's processes, descending `created_at`; each carries `awaiting_receipt` and, when set, `awaiting_receipt_since` — the earliest parked remote call's start, a factual age and never a liveness claim); `GET /v1/steps` (visible per `CanListStep`, filters `?process_id=`/`?status=`; each step carries `created_by` — the creating action `owner/name`, from its parent trace — beside `action` (the completion target) and `owner_handle` (the process owner/payer whose transaction the step settles into); a waiting step carries `allowed_input` (the derived completion schema, so a required caller completes without reading a private target) and, when its required caller is a peer, `waiting_on_peer`); `POST /v1/steps` (JWT: requires `trace_id` (funding trace), `action` (`owner/name` or id, as `/v1/run`), `required_caller`, `partial_args`, and precondition-4 authority over `trace_id`; capability: `trace_id` comes from the capability and is rejected in the body); `GET /v1/steps/{id}`; `POST /v1/steps/{id}/complete` (JWT or capability, `CanReadStep`; `args` required, `{}` valid, absent → `ErrInvalidInput`; returns `{result, tx_id, trace_id, step_id}`); `POST /v1/call` (capability-only; body `{action, args}`; returns `{result, tx_id, trace_id}`; no CLI, like inbound federation); `POST /v1/run` (never capability; `args` required; returns `{result, tx_id, trace_id, receipt_id, process_id}`); `POST /v1/auth/authorize` (unauthenticated; `{handle, password, code_challenge, [redirect_uri]}`; with `redirect_uri` → 302 redirect, without → 200 `{"redirect": "?code=CODE"}`); `POST /v1/auth/token` (auth code + `code_verifier` → `access_token` + `refresh_token`); `POST /v1/auth/logout` (refresh-token body; missing or revoked → `ErrUnauthenticated`); `POST /v1/auth/recover/start` (`{handle}`; no enrolled key → `ErrInvalidState`); `POST /v1/auth/recover/complete` (invalid/expired nonce or bad signature → `ErrUnauthorized`); `GET /v1/transactions[/{id}]` (`ErrNotFound` to non-parties; `rating` inline as `{value, note}` or null); `GET /v1/transactions/{id}/receipt-verification`.
- **Errors**: `ErrUnauthenticated, ErrUnauthorized, ErrNotFound, ErrInvalidInput, ErrInvalidState, ErrInsufficientFunds, ErrExecutionFailed, ErrSchemaViolation, ErrTimeout, ErrInternal, ErrGrantRequired, ErrPeerUnreachable, ErrPeerUnfunded, ErrTermsChanged`; stable exit codes and HTTP statuses; structured meta (grant → action ref; `ErrPeerUnreachable`/`ErrPeerUnfunded`, and `ErrUnauthorized` on a peer's refusal (P7) → peer handle under `Meta["peer"]`); concise messages [→U28]. Duplicate-key user errors are `ErrInvalidInput` without SQL text; internal CHECK/FK violations stay `ErrInternal`.
- **Rate limiting**: auth + account creation per client, 429; genuine loopback (no XFF) exempt; a loopback proxy's XFF keys on the last forwarded hop; federation limited at the transport (D12).
- **Logging**: stderr (+ optional file), stdout payloads only; configurable format/level/file; required fields `time level event request_id caller_user_id process_id trace_id action_id tx_id status duration_ms error`; every kernel transition logs start/end; stable error codes; script logs carry trace id [→U44].
- **Config**: `config.json` under `$JUICE_HOME/kernel/` (`$JUICE_HOME` default `~/.juice`, absolute, never cwd-relative — same identity wherever launched); DB default `$JUICE_HOME/kernel/juice.db` (`--config`/`--db` override); `cache/` regenerable, deletable. Keys [defaults = policy]: `db_path; fee_bps 2000; remote_bps 500; import_bps 500; exposure_max 1000; settlement_trigger 500; settlement_quantum 0; http_callback_url` (empty ⇒ derived loopback); `kernel_handle; bootstrap_peers` (shipped default `/dns4/daios.ai/tcp/31313/p2p/12D3KooWJ5ZwPSAV17q2hv6ttZ8J3hHsTMNaSC61kbVbvxvtArjK` [policy]; list two independently-operated nodes once a second exists); `credentials_key` (auto-generated first boot); `allow_local_sources false; remote_retry_interval_seconds 60; discovery_interval_seconds 300; peer_retention_days 90` (a non-positive interval falls back to its default; a non-positive retention disables purging); auth issuer, audience, and token TTL; log file, format, and level; script timeout and memory limits; `native.<action>` (one level, no deeper nesting; per-action price + settings; llm url/models; lookup default_limit 10; web price only, its User-Agent derived from the binary version; transfer price; tinygo price 5). Safe defaults when absent; invalid config rejected at startup; no committed secrets. Env (bootstrap/overrides only): `JUICE_HOME, JUICE_SECRET_KEY, JUICE_LOG_LEVEL, JUICE_CREDENTIALS_KEY, JUICE_BOOTSTRAP_PASSWORD, JUICE_BOOTSTRAP_KERNEL_HANDLE, JUICE_ALLOW_LOCAL_SOURCES`; nothing else.

### D21 OpenAPI import
`action import <spec-url>` imports representable operations as inactive `kind=http`, `source.type=openapi` actions owned by the importer, `Call(args) → object`. Methods GET/POST/PUT/PATCH/DELETE, stored in source, not `Call` semantics; manual and imported http rows share one source representation, distinguished by `source.type`. Required else rejected/kept-inactive with messages: `operationId` or `x-juice-name`; description or summary; parameters and/or body schema; one 2xx JSON response schema; `x-juice-price` default 0. Path/query/body compile into one canonical input schema; the selected 2xx response becomes the output schema. Never active: non-JSON, streaming, multipart, ambiguous success schemas, unsupported auth, unsafe URLs, invalid schemas (the last two rejected at import, not stored). Ratings/stats never imported. Provenance `{type, spec_url, base_url, method, path, operation_key, operation_hash}`; `operation_key = x-juice-name || operationId || canonical(method, path)`; match key `owner + source.type + spec_url + operation_key`; contract fields: description, method, path, bindings, both schemas, selected response, price, execution source (`operation_hash` excludes stats/ratings/timestamps/formatting). Reconcile: unchanged preserves active + stats; changed updates, deactivates, refreshes lookup, resets current stats, preserves id; removed/uncallable deactivates + resets; unimport deactivates by `spec_url` (optionally name or operation_key); scoped by provenance and match key — never touches manual rows, other servers, other mechanisms; never deletes transaction/receipt/rating/trace history; stat reset writes defaults only. Manual name collision rejects import. Draft import needs no ownership proof; public activation requires well-known challenge, in-document challenge, or verified credential [→U25]. Webhooks are not import machinery: external systems authenticate as users and use run/step-complete [→U24].

### D22 Metrics
Incremental mean `mean_{n+1} = mean_n + (x−mean_n)/(n+1)`. `latency_estimate`: mean over completed calls, denominator `uses`, sample = own transaction elapsed. `rating_estimate`: mean over rated calls, denominator `rating_count`. `uses = successes + failures`; missing stats have defined defaults; no cost estimate (the advertised price is the cost). Buyer-experienced latency = `max(descendant.ended_at) − root.started_at`, a subtree query — wall time including step dormancy, always current, never cached or retroactively written; ranking must treat it as wall time, not compute [→U15].

## 8. Test catalog

The catalog is carried below **in full** as this specification's conformance suite: every line is individually binding. The mapping assigns each block to the entries it verifies; redundancy between a conformance line and a normative rule is deliberate — the line freezes the rule's observable form.

- **Method (D)**: three tiers — unit (`go test ./...`, offline, fakes for Ollama/script/fed, temp SQLite, no global state or ordering), flows (real libp2p on loopback, one kernel as bootstrap+relay), real-NAT release gate for federation-touching changes (excluded from `go test`). Fakes behind every interface seam.
- **User-flow tests (acceptance — keep with the stories)**: local execution → U1, U3, U11, U16, U17, U13, U4; async/steps + restart → U22–U24, U23, G4; OpenAPI → U25, U26; delegated OAuth + bearer → U27, U28; ratings/reconciliation → U14, U15; federation loopback suite → U29, U31, U8, U34 (key-only reach), U39–U40, U37, U35, U41; NAT gate → U34.
- **Unit/required tests (mechanism-freezing — move beside their entries)**: accounts/auth/recovery block → D9; action lifecycle + visibility block → D4, D5; run/settlement/wallet block → D2; steps block → D6; WASM block → D7; capability block → D8; lookup block → D17; native-action blocks → D17; delegated-auth block → D10; OpenAPI block → D21; ledger ops block → D4; ratings/receipts block → D11, P5; naming/petname block → D15; federation call/settlement blocks → P4, P7, D13, D14, D19; gossip/evidence blocks → P9, D16; steps-over-wire block → P8; settle block → P10; transfer block → D18; transport/domain blocks → P1, D12; CLI/HTTP conventions block → D20.
- **Direct invariant tests**: balance non-negativity, settlement identity, wallet-transition closure, immutability, role law, reconstructibility, park/refund rules → G1–G6 (mechanically asserted via D2/D3/D6).

### Conformance suite

The suite below descends from the source contract and is amended with it; its internal `§N` references retain the source numbering mapped in the coverage table in §14.

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
failed call refunds remaining allocation to caller's trace and records that amount in the transaction's refund (the whole gross when nothing settled beneath it)
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
a pinned run succeeds unchanged and is refused — balance and process count unmoved — when price, effect, description, either schema, or the stable id moved, including when the new schema rejects the args: ErrTermsChanged against an action still active, ErrInvalidState while a terms-changing update holds it deactivated (D4), since liveness is checked before the pin; a private action refuses on visibility, never disclosing its quote
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
a capability is refused on the --peer completion path, dispatching nothing: that request signs as the whole kernel, and a capability carries no session caller, so the empty-caller form would cross federation as the operator
in-execution completion is trace-confined: a capability and a WASM host are each refused (ErrUnauthorized) on a step another process's trace parked, even when addressed to that same owner; the step stays waiting, no balance moves, and the JWT and peer paths are unaffected
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
the schema preserves every account id and transaction party, holds no reference to `users`, and keeps foreign_key_check empty
a schema change that drops records may not strand funds: the migration retiring the cross-kernel value legs aborts while an unresolved payment reserve or an unsettled cross-kernel value lock exists — identified by shape (fees in the reserve, no local beneficiary, or a peer-funded lock), never by amount, since zero fees make a cross-kernel lock numerically identical to a local one — and applies once only local locks remain
kernel about: sys's description surfaces as gossip `about` and in admin identity; admin inspect renders action descriptions carried in gossip/manifests
admin deposit/withdraw/suspend resolve a peer by key (global name) as well as by handle
a boot with no configured kernel name fails before writing anything: no superuser is created, and the next boot demands the name again rather than deriving one
remote manifest signature is Ed25519 over canonical JSON excluding signature
remote proxy transaction has target_user_id = kernel account id
a peer's first signed call or a deposit by key provisions a zero-balance billing account
a call whose proxy is absent or inactive re-resolves and reactivates the row in place, id preserved
a federated call dispatches the peer's stable action id on first dispatch and on retry, so a stale cached reference or a renamed remote owner never parks the caller; an inbound call naming a cached remote_proxy id is refused
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
a delegated-auth action's execution produces no gossip evidence, while an ordinary public action's does
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
the value effect is staged from C.available atomic with funding and committed by the kernel at settlement, not by the handler; value-bearing is decided by the action's contract effect field, never its name; a composed subcall pays the value from the composing action's owner, and a failure refunds it whole
value is local to one kernel: a peer caller, a kernel-qualified target, and a peer or suspended beneficiary are each rejected before funds move; no manifest carries an effect, so no proxy is value-bearing; a remote receipt claiming value is quarantined, never settled
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
signed zero-charge 402 rejection settles as ErrPeerUnfunded with the peer handle in meta, never as the caller's own insufficient_funds; any other signed rejection settles as ErrUnauthorized with that meta, a refresh_proxy rejection keeps the terms-change path, and an executed failure stays ErrExecutionFailed even at charge 0 on transport 402 — the marker (tx_id == the dispatched idempotency_key), not charge or status, decides
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
ordinary-account balances are never negative (DB CHECK `kernel_public_key IS NOT NULL OR available >= 0`); a peer account may go negative, bounded not per-peer but by the kernel-global exposure cap `X` enforced at admission (§13); `locked` is never negative
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

## 9. What is not a user story

Never presentable as user needs: "use SQLite/wazero/libp2p/Ollama"; "create a Process and Trace" or "route everything through `Call()`"; "add a table/column/interface/endpoint/native"; "use `X`, `Y`, `Q`" or a markup formula; "return this internal id" or "run these store methods in this order"; "every Go file has a test file". Their justification must be a story or guarantee, not incumbency. The required tests split likewise: acceptance tests belong with stories; mechanism-freezing tests belong beside the design they verify.

## 10. Deviations from the source contract

The sixteen rulings of 2026-08-14 are executed: `requirements-old.md`, the code, the tests, and the flows agree with this document, so the ledger that tracked their lag is empty. Engineering choices made while executing them, recorded because they bind future edits:

- The value channel is local, so what is locked from the caller is exactly what the beneficiary receives: the trace's separate value reserve collapsed into `value` (migration `043` drops the column), and the settlement primitives lost their credit/sys-credit/refund generality.
- Two canonical formats are preserved rather than shortened: the step idempotency key keeps its reserved trailing component, so a completion retried across the upgrade recovers its stored outcome instead of re-executing under a fresh key; the remote contract hash keeps `effect` as a reserved empty key, so no cached proxy is re-keyed. Both are verified by unchanged hash fixtures. `Receipt.value_premium` is not among them: with value local, every issuer wrote 0 and the check compared 0 against 0, so field, column, and check are deleted rather than carried (migration `044`).
- Migration `043` refuses to apply while any unresolved payment reserve or cross-kernel value lock exists, so no record backing locked funds is ever dropped. The locks are identified by shape — fees in the reserve, no local beneficiary, or a peer-funded lock — never by amount: both fee rates may be zero, which makes a cross-kernel lock numerically identical to a local one.

## 11. Borderline rulings

1. Subtree price — constraint (U11); trace-wallet enforcement design (D2).
2. Role law — attribution constraint (G2); schema and precondition order design (D2).
3. Caller-scoped visibility — the behaviors (encapsulation, no confused deputy, parked steps never bricked) constraints (U20, U22); enum and binding rule design (D5).
4. Margin, not refund — constraint (U16): purchase at fixed price, not metered cost.
5. Receipts — offline verifiability constraint (U36, G7); JCS/Ed25519/domains design (D11).
6. Ratings as privacy-preserving public evidence — constraint (U39); projection shapes and 0/1 scale design (D16).
7. Global exposure cap — Sybil-proof bounded loss constraint (U32); a single global counter near-forced (per-identity schemes fail the Sybil clause).
8. Probabilistic residual settlement — zero-expected-loss constraint (U33): the spec's impossibility argument eliminates deterministic options, and a biased scheme transfers value systematically; commit/reveal design (D14).
9. Resolve-on-use — permissionless calling constraint (U29); proxy cache and healing design (D13).
10. Web standards vs libp2p — user-facing edge constrained to web standards (U25, U27, U45); kernel-to-kernel edge constrained by U34, which HTTPS-with-URLs cannot meet; libp2p design (D12).
11. Native encapsulation — constraint (source contract former §9): natives are ordinary actions behind `Call()`, no kernel privileges; which natives exist design (D17).
12. Credits by fiat — operator-rail deposits/withdrawals current constraint (U3); ROADMAP's rail-backed money is a planned constraint change, made deliberately.

## 12. Rule for future simplification

1. Name the user story served.
2. Name the constraints that must remain true.
3. Identify the design being replaced or deleted.
4. Show money, authority, privacy, durability, and interoperability remain at least as strong.
5. Prefer an existing web standard or existing kernel path.
6. Delete the old path; never keep both.
7. Update `requirements.md`, design docs, `API.md`, and acceptance tests at the same boundary.

## 13. Non-normative residue

- Source contract former §16 (design rationale) is justification, not contract: its content is preserved as commentary beside the entries it argues for (D2, D5, D12, D14, D15, D22; ruling texts in §11).
- The source contract's file-header editing instructions are editorial meta.
- Contract deviations, when any exist, are recorded in §10.

## 14. Coverage

Every section of the source contract is absorbed as follows; nothing else remains:

| §  | Content | Class home |
|----|---------|------------|
| 1  | run/Call, role law, minimality, exec/supervision split | D2; G2, G8; minimality (§9) |
| 2  | packages, constraints | D1 |
| 3  | data model | D4 (wire shapes P5, P6; steps D6; grants D10; evidence D16; kernel D15/D16) |
| 4  | authorization, preconditions, quote hash | D5, D2, P2, U48 |
| 5  | persistence, atomicity, recovery | D3 |
| 6  | transition and settlement | D2 |
| 7  | action lifecycle, URL safety | D4, G9 |
| 8  | imports, auth schemes, delegation, remote | D21, D10, D13, P6 |
| 9  | adapters, capability, natives, stats | D7, D8, D17, D22, G9 |
| 10 | steps | D6 |
| 11 | receipts, ratings, tx access | D11, P5, D22 |
| 12 | auth, errors, first boot, supervision ops | D9, D20, D4, P1 |
| 13 | federation | D12–D16, D18, D19, P1, P3–P10 |
| 14 | CLI, HTTP, config, logging | D20 |
| 15 | tests | §8 |
| 16 | rationale | §13 |

Facts marked `[policy]` or `[convention]`, plus every design clause lacking a `[→…]` trace, constitute the enumerated simplification surface.
