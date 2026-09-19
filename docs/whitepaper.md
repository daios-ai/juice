# Juice: A Priced-Action Execution and Settlement Kernel for the Agentic Web

### White paper · v0.4

---

## Abstract

The web's eras can be told as a sequence of verbs: *read* (Web 1.0), *read/write*
(Web 2.0), *read/write/own* (Web 3.0), and now *read/write/own/wish* — a web in which a
user states a wish in natural language and services compose themselves to grant it. A
wish, unlike a page view or a post, has to be turned into a **bounded, payable, auditable
computation** before it can be safely granted. Juice is a kernel for that step.

Concretely, Juice is a small Go kernel for *callable actions*: units of service that
carry a price, run under explicit authority, and settle atomically with a signed receipt.
Its design goal is deliberately narrow. Rather than build an agent platform, a
marketplace, or a payment protocol, it isolates the **transaction law** such systems need
and pushes everything else — orchestration intelligence, ranking formulas, the actual
economy — out to an application layer of actions.

This paper does three things. It gives a self-contained account of what Juice is and the
vision it serves (§§2–3). It presents the architecture, economics, and security
properties as a first-class core (§§4–6). And it situates Juice against decades of prior
art and the 2025–2026 wave of agent protocols and agent-payment rails (§§7–9), where a
striking pattern emerges: the contemporary field is rediscovering, often through
published attacks, the two invariants Juice treats as axioms — that a payment proof must
be bound to its request context, and that monetary movement and its audit record must
commit together or not at all.

No single Juice mechanism is new. The claim of this paper is that the
**invariant-preserving combination** — funded calls, subtree-bounded prices, trace-local
authority, atomic settlement, signed receipts, funded continuations, bilateral
federation, and trade-backed reputation, assembled as a minimal kernel *without a
blockchain* — is not packaged together by any existing system, and that it answers a
problem the rest of the field has left structurally open.

---

## 2. The problem: granting a wish safely

The cleanest way to place Juice is on the arc of the web's defining verbs. Each era added
one capability to the user's relationship with the network:

- **Web 1.0 — *read*.** Static, hyperlinked pages; an interconnected library you could
  only consume.
- **Web 2.0 — *read / write*.** The participatory web; users create and share, at the
  cost of data silos and advertising-driven monetization.
- **Web 3.0 — *read / write / own*.** The blockchain era and Chris Dixon's framing; users
  can own digital assets, though in practice ownership has largely meant speculation.
- **Web 4.0 — *read / write / own / wish*.** The user states a *wish* — an intent, in
  natural language — and the network composes services to fulfil it. The new affordance is
  not a new thing to consume, publish, or hold, but the ability to *ask for an outcome*
  and have the web assemble itself around the request.

That fourth verb is the whole point, and it is also why a settlement kernel is required.
A wish is not free. To be granted, it must be turned into a bounded, payable, auditable
computation: someone composes the services, each provider must be paid, the total cost
must be capped in advance, and the result must be checkable after the fact. "Wish" is the
affordance; **bound, pay, settle, audit** is what has to happen underneath for the
affordance to be safe.

Concretely, Juice is the execution-and-settlement layer for the "Web 4.0" vision
articulated by DAIOS: an AI-orchestrated, composable web in which a user states an intent
in natural language and an orchestrator assembles online services on the fly to fulfil
it, paying each provider a small fee per use. That vision names three hard problems:
building a distributed orchestration AI; finding a sustainable micropayment-based
monetization model to replace advertising and subscriptions; and ensuring trust and
safety through *ex-post* feedback (ratings, reputation) rather than only *ex-ante* rules.
It also revives an older idea — Berners-Lee's Semantic Web — and argues that what stalled
the Semantic Web (the impracticality of hand-annotated ontologies and logical reasoners
over messy, dynamic data) is exactly what large language models now dissolve, because
they can interpret unstructured descriptions and select among services directly.

Juice is the *how* to that *why*, but narrowly. It does not try to be the orchestration
AI. It provides the primitives that make priced composition safe, atomic, and auditable,
and leaves the intelligence — which action to call, how to rank the catalog, how to
compose a workflow — to the application layer. This restraint is the design's central
virtue, and it recurs throughout the paper.

---

## 3. System overview

Juice's only execution primitive is a single dispatch call:

```
Call(caller, trace, action, args)
```

Everything runs through it: a user-initiated run, a script's subcall, the completion of a
suspended step, an imported HTTP action, a federated remote call. `Call` dispatches on the
action's *kind* (`native`, `wasm`, `http`, `remote_proxy`), never on who owns it. A
separate band of **supervision** operations — creating users and actions, rating outcomes,
depositing credits, importing APIs, peering with other kernels — deliberately never routes
through `Call`. The separation is load-bearing: execution may err, and supervision supplies
the correction signals (ratings, suspensions) that execution must not be able to manipulate.

Juice is meant to be a kernel in the operating-system sense: only minimal but general and
robust primitives, with the rest living on the application layer of actions. Three ideas
carry most of the weight, and the next section treats each concretely.

**The role law.** Every call has a payer, a requester, and a payee, and every transaction
records all three as immutable fields: the *process owner* who funds the work, the
*caller* who immediately requested it, and the *action owner* who is paid on success. For
a top-level run the payer and the requester are the same person — you fund and request the
work — while the payee is the action's provider; for a subcall the requester is the parent
action's owner, not the original user. Capturing all three at creation keeps financial
history reconstructable even after an action is deleted.

**Subtree-bounded pricing.** An action's `price` is not a meter reading — it is a *bound
on the entire call tree beneath it*, advertised worst-case by the provider. The caller is
quoted one number that caps everything the call will spend, including its subcalls and any
work it defers. Money is escrowed before execution. On success, whatever the call did
**not** spend downstream is paid out to the provider as margin, taxed by a platform fee;
unused budget is *not* refunded. On failure, the unspent remainder rolls back up the tree
to the caller, while any already-settled descendants stay paid.

**Funded continuations (Steps).** A Step is a partially applied future `Call`: a suspended
computation that records enough context to resume when someone later supplies the missing
input — a human approval, a webhook, an asynchronous result. Crucially, a Step is *funded
at creation*: its price is parked the moment it is created and held until it completes or
is cancelled. This is what makes long-running, human-in-the-loop composition safe and
restartable.

---

## 4. Architecture and core mechanisms

### 4.1 One primitive, dispatched by kind

`run(action, args)` is the user-facing entry point. It atomically creates a process funded
with exactly `action.price`, funds a root trace from that process, and issues the root
`Call`. Every `run` is therefore just a `Call` on a freshly funded root trace; the process
closes automatically once the root call has returned and no Steps remain outstanding.

From there, every execution path is the same primitive:

| Path                  | How `Call` is reached                                    |
| --------------------- | -------------------------------------------------------- |
| Root call (`run`)     | the kernel funds a root trace and issues the call        |
| WASM subcall          | a script invokes `juice.call` under the parent trace     |
| Step completion       | a caller supplies the missing input to a waiting step    |
| Imported HTTP action  | `Call` dispatches on `kind = http`                       |
| Remote (federated)    | `Call` dispatches on `kind = remote_proxy`               |

`Call` dispatches by `action.kind`, not by action-owner identity. Supervision operations
manage users, actions, processes, ratings, deposits, imports, and peering — and never route
through `Call`.

### 4.2 The role law

For every call, define three principals:

```
P = process.owner_user_id      // process owner; payer
C = caller                     // call caller; immediate requester
A = action.owner_user_id       // action owner; payee on success
```

The transaction created by the call records `owner_user_id = P`, `caller_user_id = C`, and
`target_user_id = A`. These meanings are fixed across every path:

| Case                    | `P` (payer)               | `C` (requester)          | `A` (payee)            |
| ----------------------- | ------------------------- | ------------------------ | ---------------------- |
| Root call (`run`)       | process owner             | authenticated requester (= P) | called action owner |
| WASM subcall            | parent process owner      | parent action owner      | subcalled action owner |
| Step completion         | step's process owner      | required completer       | step's action owner    |
| Remote proxy call       | local process owner       | local caller             | local proxy user       |

Because all three parties are captured at transaction creation, financial history remains
self-contained and reconstructable even after an action is deleted. Transaction-read
authority follows directly: a user may read a transaction if they are its payer, its
caller, or its payee. Every credit to a provider is reconstructable from transactions that
provider can read.

### 4.3 Wallets and subtree-bounded pricing

Every wallet in the chain — **user, process, trace** — has two balances, `available` and
`locked`, and money moves the same way at every level. A `run` parks the price in the
user's `locked`; the process holds it as `available`; the root trace is funded from the
process. Thereafter, calling something of price `q` requires `available ≥ q` on the
handed trace and moves `q` from that trace's `available` into its `locked`, becoming the
child trace's `available`:

```
require trace.available ≥ q
trace.available -= q ;  trace.locked += q
create child trace with available = q, locked = 0
execute action by dispatching on action.kind
validate output against action.output_schema
```

`locked` holds only *outstanding* commitments: a child's resolution always releases the
caller's lock. When a child succeeds, its remaining `available` is paid out and the caller's
lock for `q` is released; when it fails, the refunded amount returns to the caller's
`available` and the lock is released.

**Settlement (success).** The call's remaining `available` — what it did *not* commit to
subcalls and steps — is its value added, and is what gets paid out:

```
taxable = trace.available
fee     = ceil(taxable · fee_bps / 10_000)      // (taxable·fee_bps + 9_999) / 10_000
net     = taxable − fee
```

`target_user_id` is credited `net`; the platform user `@sys` is credited `fee`. The default
fee is `fee_bps = 2000` (20%). Unused budget is the provider's margin, **not** a refund:
`price` is a price, not a metered estimate. Negative value added is structurally impossible
because `available ≥ 0` everywhere. Each kernel taxes only its own layer.

**Refund (failure).** A failed call is rolled up entirely: its remaining `available`, plus
the parked prices of all its outstanding steps and — recursively — everything outstanding
beneath them, is cancelled and returned to the caller's `available` (for a root call, to
the process, and from there to the owner at closure). The failed call charges zero fee and
net and records `status = failure` with a failure class in `reason`. Crucially,
**already-settled subcalls inside the failed call stay settled** — their providers were
paid from money the call had already spent. A refund destined for a trace that has already
settled routes to the process instead: settlement is final, and a trace never regains
`available`.

This is the design's defining economic choice, and it inverts the most familiar analogue.
Ethereum gas *meters resource consumption* and refunds the unused remainder; Juice *prices
composed service delivery under a bound* and keeps the unspent allocation as the provider's
margin. The caller pays one advertised number that caps the whole tree; the provider who
composed the tree bears its composition risk; the fee taxes each layer's *margin* — value
added — rather than gross flows.

### 4.4 Funded continuations (Steps)

A Step is a partially applied future `Call`:

```
CompleteStep(caller, id, input) ≡ Call(caller, step.parent_trace_id,
                                        action_id, partial_args ⊕ input)
```

It records the funding trace (which derives the process), a mandatory required completer,
the target action, pre-bound `partial_args`, and a status of `waiting → running → done`
(or `cancelled`). The completer may supply only the action's input keys not already bound
by `partial_args`; the final merged arguments are validated against the action's input
schema by the underlying `Call`.

The distinctive property is **funding at suspension**. When a step is created, the
action's price is snapshotted as `step.price` and moved from the creating trace's
`available` into its `locked` — parked. That parked price *is* the completion call's
allocation, so completion never checks funds: the money was reserved when the step was
created. An outstanding (`waiting` or `running`) step keeps its process open and its
allocation parked. Cancellation returns the parked price — to the creating caller through
that call's failure rollup, or to the process owner at forced process closure.

This is what makes asynchronous, human-in-the-loop composition safe and honest about who
pays: the money for a deferred continuation is reserved at the moment the work is
suspended, not hopefully re-collected when it resumes. A step parked for a human approval
that arrives a week later draws on funds that have been escrowed the whole time.

### 4.5 Atomicity, receipts, and recovery

**Atomicity.** Each monetary transition commits together with its audit record — the
transaction, the receipt, the stats, and any step or idempotency state — inside *one*
store operation. The money-path methods are therefore compound atomic operations, not
fine-grained primitives coordinated from above. A monetary movement and its audit record
either commit together or fail together; there is no window in which money has moved but
the record has not, or vice versa.

**Receipts.** Every committed success or failure has exactly one immutable receipt, issued
by `@sys` and signed with the platform's Ed25519 key over the receipt's canonical JSON
(RFC 8785 JCS). `args_hash` and `reply_hash` are SHA-256 over the JCS-canonical arguments
and reply. A receipt's `charge` is the amount actually drawn from the caller: equal to
`gross` on success, `≤ gross` on failure (settled descendants stay paid), and `0` on
rejection. Receipts are the verifiable, tamper-evident settlement record — and they are
achieved with signatures over canonical JSON and immutable transactions, with **no global
agreement protocol at all.**

**Recovery.** Atomicity has a crash-side guarantee. At startup, every trace that has no
transaction was mid-execution at shutdown and can never return: it is settled as a failure
with `reason = interrupted`, deepest first, applying the normal refund rollup — settled
descendants stay settled, refunds flow up the chain, and processes then close by the
automatic rule. Steps left `running` with no transaction are reset to `waiting` and their
allocation re-parked; `waiting` steps are untouched and survive restarts with their parked
prices intact. The one exception is a remote-proxy call that had already dispatched: it is
not "interrupted" but resumes retrying with its stored idempotency key until a signed
receipt settles it. Recovery is idempotent.

### 4.6 Federation: a peer is just a user

Federation adds **no new money model**. Every user is either *local* (authenticates by
password or token) or a *proxy* (a peer kernel's account here, identified by its Ed25519
`public_key` and `remote_base_url`, authenticating only by per-request federation
signature). Because a peer is simply a user, a proxy user holds credits, pays, and is paid
like anyone else, and bilateral netting falls out of the ordinary wallet model with no
separate machinery.

Two kernels transact only as **friends** — a reciprocal relation established by exchanging
keys. But friendship by itself grants nothing: a zero-balance friend's calls are all
rejected. The trust decision is the **deposit** — an operator credits a friend's proxy user
only after real money has moved out of band. Friendship exchanges keys; funding expresses
trust. Federation is prepaid: credits cross the bank boundary only by supervision, and
execution never mints or burns. The depositor bears counterparty risk, bounded by the
deposit — the posture of correspondent banking, not of a trustless channel.

A remote-proxy call follows the normal role law and wallet mechanics, funded with the
proxy's local price `mp + maxduty`, where the worst-case **import duty** is
`ceil(mp · import_bps / 10_000)` and the default `import_bps = 500` (5%) per peer. The
local caller sees one price bounding the whole remote call, duty included. The outbound
handler records the call's UUID idempotency key and request payload on the proxy trace
atomically with dispatch — which is exactly what makes retry-after-restart possible.

A proxy call settles **only on a signed remote receipt — never on a network timeout**:

```
remote success:    charge = mp ; duty = ceil(charge·import_bps/10_000)
                   pay charge to the proxy user, duty to @sys,
                   refund (mp + maxduty) − charge − duty to the caller
remote failure:    charge ≤ mp (the remote draw) ; duty = 0
                   pay charge, refund (mp + maxduty) − charge
signed rejection:  charge = 0 ; full refund
timeout:           no settlement — retry with the same idempotency key
```

This is the local failure rule applied across the wire: a failed call refunds its remaining
allocation, and the receipt's `charge` is how the local kernel learns what remained. A
failure counts against the proxy's reputation regardless of charge, so a provider gains
nothing by failing-with-charge over succeeding. Inbound calls sign
`JCS({action, counterparty, idempotency_key, timestamp, args_hash})`; the receiver verifies
the signature, the raw-body hash, friendship, and a timestamp age `≤ 5 minutes`, then runs
the call as the proxy user, paid from its prepaid balance. Idempotency is enforced by a
record keyed `(counterparty, idempotency_key)` that exists only while execution may be running:
a replay is answered from the stored receipt, which names the same pair; a replay while the call
is still running returns 409. The whole remote receipt — JSON plus its
SHA-256 — is stored atomically with the local transaction, and `tx verify` re-checks it
entirely locally.

### 4.7 The `@sys` native stdlib

Native actions are a platform standard library shipped alongside the kernel and owned by
`@sys`. They have **no special kernel privileges** — any provider could supply equivalent
HTTP or WASM actions. They are registered at bootstrap and interact with the platform only
through the same injected dependencies and the same `Call()` / `CreateStep()` entry points
available to every action. The stdlib includes semantic catalog `lookup`; the LLM surface
`llm/chat`, `llm/embed`, `llm/json`, and `llm/decide` (which selects an action and proposes
arguments without executing); `make` (synthesizes and registers a WASM action from a
natural-language description); the WASM build action `tinygo/compile`; and utilities `time`,
`random`, `sink`, and `message`. Their prices and settings are configurable, but they are
ordinary actions all the way down.

WASM execution is textbook object-capability: scripts receive no ambient filesystem,
network, environment, process access, or raw user tokens — only explicit host functions
(`juice.call`, `juice.step_create`, `juice.step_complete`, `juice.log`), each with a memory
limit, timeout, deterministic cancellation, and an artifact-hash compiled-module cache.

---

## 5. The economic model and incentives

Three design decisions make the economy work without any enforcement machinery beyond the
kernel's accounting.

**A price is a price.** Subtree pricing collapses a composed execution tree to a single
visible number the user actually sees, and puts composition risk on the provider who
composed it. The fee taxes *value added* — the margin a layer keeps — not gross flows. A
provider who advertises a tight bound and delivers under it keeps the difference; a provider
who composes carelessly absorbs the overruns.

**Local supply always out-earns imports.** The domestic fee (default 20%) deliberately
exceeds the import duty (default 5%). A kernel therefore always earns more on local supply
than on a federated import, so federation *complements* local providers rather than
undercutting them. A kernel federates because its users gain a larger action space and its
providers gain outside demand; the operator's reward is the import duty.

**Reputation is earned by trade, never granted by friending.** Gossip carries only
*trade-backed* opinions: a kernel gossips a friend only once it has settled real calls with
it, and shares the local stats it earned. Third-party stats are stored namespaced by their
introducer and affect **ranking only** — never callability, pricing, or settlement —
dominated over time by a kernel's own settled experience. Discipline is self-enforcing: a
kernel whose counterparty account runs dry stops serving it manifests, because executing
unpaid work loses money twice — once in service, once in the failure stats that sink its
rank abroad. Reliable settlement compounds into discoverability; reputation is the long-run
asset a kernel earns by settling honestly.

The deeper economic bet is addressed in §9: that LLM orchestration removes the human
budgeting burden that sank earlier computational economies, while subtree pricing gives the
human a single bound to consent to.

---

## 6. Security properties and invariants

### 6.1 Two axioms

Juice treats two properties as non-negotiable, and both have classical names.

**Payment proof bound to request context.** An inbound federation signature covers an
`args_hash` over the exact request body, together with the action, counterparty,
idempotency key, and timestamp. A proof minted for one request cannot be replayed against
another: the proof *is* a statement about a specific call.

**Atomic settlement.** The monetary movement and its audit record commit in a single store
operation. There is no window between "verified" and "settled" in which one can succeed
while the other fails. This is the **optimistic fair exchange** property — money and
delivery commit together, or neither does — realized at the store boundary rather than
through a third-party adjudicator.

### 6.2 The contemporary validation

A wave of 2026 security research on the x402 agent-payment protocol has formalized
"security invariants" for agentic payments and shown that current implementations fail to
enforce exactly these two properties. The documented failure modes are precisely two:

1. **Proofs not bound to context.** A "semantic gap in signature design" permits
   *cross-resource substitution* — a payment proof minted for one context replayed against
   another — with no application-layer nonce binding proof to request.
2. **Non-atomic verify-then-settle.** The gap between off-chain verification and on-chain
   settlement lets the settlement path fail to bind caller, facilitator, resource, and
   service decision into one atomic object, producing *paid-but-denied* and *unpaid-service*
   outcomes. A signature-verification-bypass advisory was disclosed in the x402 SDK in
   March 2026, and the verify/settle gap was noted as unresolved in x402 v2.

These are the two properties Juice designed in from the start: the `args_hash` binding
closes (1), and the single-store-operation money path closes (2). The blunt summary is that
the agent-payment field is currently rediscovering, through CVEs and formal attack papers,
the invariants Juice treats as axioms.

### 6.3 The liability bound — a claim with its premise

Juice's core safety property can be stated as an induction, provided its premise travels
with it. At every call boundary the kernel checks `available ≥ price` before creating the
child, and the write that moves funds from `available` into `locked` and creates the child's
allocation is **atomic at the store boundary**. Therefore, by induction on the execution
tree, the sum of all outstanding and settled descendant commitments under any node cannot
exceed that node's allocation; at the root, the user's quoted price bounds the entire tree's
liability. Combined with transaction immutability and receipt signing, every settlement
claim is checkable after the fact.

The caveat must travel with the claim: *the mathematical invariant depends on the
store-boundary invariant.* The clean inductive guarantee is downstream of the atomicity
property of §6.1, not independent of it — which is exactly why the contemporary rails that
lack that atomicity (§6.2) cannot make the equivalent claim.

---

## 7. Related work

No single Juice mechanism is novel; the discipline is in assembling them without
contradiction. Each line below has decades of prior art, and naming the precise lineage
also sharpens where Juice deviates.

**Agoric and object-capability computing — the primary ancestor.** Juice's closest
intellectual home is the *agoric* tradition: Miller and Drexler's *Markets and Computation:
Agoric Open Systems* (1988), which framed computation as a market with prices, escrow, and
capabilities. Mark Miller's later work — the E language, *Capability-Based Financial
Instruments* (2000), and the object-capability model of *Robust Composition* (2006) —
supplies the other half. Juice's WASM model is textbook ocap: least authority via explicit
host functions, the "powerbox" pattern realized directly. The sharpest single analogue is
**Zoe**, Agoric's smart-contract framework, which guarantees *offer safety* — a participant
either receives the payout they specified or gets their escrowed assets back. Juice gives a
structurally analogous kernel property: validate input before locking, execute under
explicit authority, settle only after output validates, and refund the unspent remainder on
failure.

**The money model — gas, micropayments, computational economies.** The closest operational
analogue is **Ethereum gas**: prepay against a worst-case bound, take a fee, proceed. But
Juice *inverts* the central rule, as §4.3 describes — gas refunds unused consumption; Juice
keeps the unspent allocation as provider margin. The broader lineage runs through
Waldspurger's *Spawn* (1992), Stonebraker's *Mariposa*, the Tycoon and POPCORN systems, and
the micropayment strand of Rivest–Shamir *PayWord/MicroMint* (1996), DEC's *Millicent*, and
*Mojo Nation*. This history matters for §9: it is largely a history of systems that never
reached liquidity.

**Durable execution and long-lived transactions — what Steps are.** Juice's failure
semantics are the *saga* pattern (Garcia-Molina and Salem, 1987) given a money meaning:
compensation rather than global rollback, with committed sub-activities left intact. A
precision note: classic *closed* nested transactions (Moss, 1981) make a child's commit
provisional until the top level commits — the **opposite** of "settled descendants stay
paid." The accurate lineage for that property is sagas plus *open* nested transactions
(Weikum and Schek), where subtransaction commits are irrevocable. On the engineering side,
Steps are durable continuations in the family of **Temporal, AWS Step Functions, Azure
Durable Functions, and Cadence** — with one element those systems lack: *funding at
suspension*.

**Hierarchical resource accounting.** The trace's `available`/`locked` budget threaded
through a subcall tree is hierarchical resource accounting applied to money. The prior art
is **resource containers** (Banga, Druschel, Mogul, OSDI 1999) and **lottery/stride
scheduling** (Waldspurger), whose ticket-and-currency model of sub-budgets funded from
parent budgets is a close structural parallel.

**Accountability and verifiable receipts.** Juice's signed, immutable receipts and its
`tx verify` path are tamper-evident settlement records in the spirit of **PeerReview**
(Haeberlen et al., SOSP 2007) — and pointedly *not* in the spirit of blockchain consensus.
Auditability is achieved through signatures over canonical JSON and immutable transactions,
with **no global agreement protocol at all.** Avoiding consensus is a feature, and it is the
axis on which Juice differs most cleanly from the deployed field.

**Reputation via gossip.** Juice's gossip layer belongs to the P2P reputation family whose
canonical member is **EigenTrust** (Kamvar et al., WWW 2003) — but it *keeps the signals
while rejecting the central mechanism*. EigenTrust's defining move is *transitive* trust
propagation; Juice's gossip is deliberately **non-transitive for authority**. It is
information only: it never grants callability, a kernel must still friend a peer directly
and verify manifests from the owner. Reputation shapes *ranking*, never *permission*.

**Semantic Web services — the lineage Juice is repairing.** **OWL-S** (originally DAML-S),
**WSMO**, and **BPEL** tried to make services machine-discoverable, -invocable, and
-composable through formal, machine-readable descriptions, and did not reach broad adoption
because formal ontology annotation and logical reasoning did not scale over unstructured,
dynamic data. Juice's move is the substitution the DAIOS vision proposes: *weaken the
ontology ambition, strengthen the operational semantics.* Schemas plus natural-language
descriptions support lookup and LLM-based selection in place of formal ontologies and a
reasoner, while `Call` supplies the authority, price, settlement, and trace state those
frameworks never had. Semantic Web services were the first attempt; LLMs are the missing
reasoner; Juice is the operational realization with a settlement law added.

**Contract Net and capability authorization.** The **Contract Net Protocol** (Smith, 1980)
is the antecedent for task allocation among distributed agents — though it is an *auction*,
where Juice is *posted-price* (providers set prices; `lookup` ranks rather than runs a
bidding round). On authorization, the close relatives are caveat-bound, delegable schemes:
**macaroons** (Birgisson et al., NDSS 2014), **SPKI/SDSI**, and **UCAN**. Juice's process
and trace authority read as store-backed capabilities whose caveats are economic and
causal. And for ocap *over the wire*, Juice's signed-manifest federation is distributed
capability messaging in the lineage of **CapTP** and its successor **OCapN** (Spritely
Institute) — a *money-bearing* cousin, capability invocation with settlement attached.

**Fair exchange and exactly-once — the formal frames.** "Money and delivery commit together,
or neither does" is **optimistic fair exchange** (Asokan, Shoup, Waidner). The dedup
mechanism behind Juice's idempotency records — insert a pending record before execution, key
it uniquely per counterparty, return the stored receipt on replay — is **exactly-once
delivery** from the distributed-systems literature. As §8 notes, this is precisely the
mechanism the agent-payment field is now patching in after the fact.

---

## 8. The contemporary landscape, and what is distinctive

### 8.1 Agent protocols and payment rails

A standards stack for the "agentic web" took shape rapidly across 2025. Each layer is real
and useful, and each stops short of a transaction law.

- **MCP (Model Context Protocol, Anthropic, 2024)** standardizes how tools, resources, and
  prompts are exposed to model applications. It has no economic or settlement layer.
- **A2A (Agent2Agent, Google, 2025; Linux Foundation, June 2025)** standardizes agent
  discovery and task delegation via "Agent Cards." It is payment-agnostic by design.
- **AP2 (Agent Payments Protocol, Google, September 2025)** introduces three signed
  *Mandates* — Intent, Cart, Payment — as W3C Verifiable Credentials, with stablecoin rails
  first-class. It is an authorization-and-proof layer, not an execution semantics.
- **NANDA (MIT Media Lab)** is the most architecturally adjacent: an "Internet of AI Agents"
  with a decentralized registry, an AgentFacts schema, Ed25519 signatures, gossip-based
  federation, and portable reputation. It independently arrived at Juice's *exact federation
  vocabulary* — gossip + Ed25519 + portable reputation — but carries no funded `Call`,
  subtree settlement, or receipt-bound transaction semantics.

The "pay-per-call for agents" rails — **L402** (Lightning + macaroons over HTTP 402),
**x402** (HTTP 402 over stablecoin rails), and startups such as **Skyfire** and
**Nevermined** — attach a payment to an HTTP request; they do not define authority, funded
execution, settlement, traceability, and recovery as one transition system. §6.2 covers the
2026 attack literature on x402, which is the strongest external validation of Juice's
invariants.

The deployed AI-service and compute markets are the right *contrast* class. **SingularityNET**
(service registry, payments, ratings, Multi-Party Escrow) resembles Juice institutionally,
not semantically. **Bittensor** parallels its trade-backed reputation on a different axis.
**Golem, Akash, iExec, Bacalhau** price *resources* — CPU, containers, GPUs — where Juice
prices *service outcomes under a subtree bound*. The unifying observation is the through-line
that most cleanly separates Juice from the entire live field: **every deployed comparator
settles on external infrastructure — a blockchain or a Lightning-style channel.** Juice's
distinctive deployment assumption is *bilateral credit plus signed receipts*: a peer is an
ordinary user with a balance, netting falls out of the wallet model, and settlement is
auditable **without global consensus.**

### 8.2 What is genuinely distinctive

No single mechanism is novel. The distinctiveness is the combination, and it is real because
no existing system packages it together:

1. a single dispatch primitive (`Call`) for every execution path;
2. subtree-bounded prepaid escrow in which unused budget is provider margin, not a refund;
3. atomic settlement-with-signed-receipt as one store operation;
4. funded continuations (Steps) for safe, restartable asynchronous composition;
5. federation as "a peer is just a user," yielding bilateral netting with no separate money
   model; and
6. capability-mediated execution plus trade-backed, non-transitive reputation —

all as a *minimal kernel* with the economy pushed to the application layer, and **without a
blockchain.** The agoric program imagined much of this in 1988; the nearest living relative,
Agoric the company, realizes the capability-and-finance half but on a consensus chain.
Juice's clean separation from that nearest relative is the no-consensus, bilateral-trust,
signed-receipt deployment choice.

---

## 9. Limitations and open questions

A clear-eyed assessment must confront the fact that Juice's lineage is also a graveyard.
Spawn, Mariposa, Mojo Nation, Millicent — the computational-economy program has a
multi-decade record of *not* reaching liquidity, in large part because humans do not want to
manage fine-grained computational budgets.

Juice has a plausible answer to why this moment might differ, and it is worth stating as a
claim rather than a hope. **LLM orchestration removes the human budgeting burden** — the
model chooses the calls — while **subtree pricing collapses a composed execution tree to a
single visible bound** the user actually consents to. The success condition is therefore not
the elegance of the kernel; it is *liquidity in useful actions*. The kernel can make trade
safe and auditable. It cannot, by itself, make the market thick. Whether a population of
priced, callable actions worth composing actually materializes is an empirical question the
design cannot settle, and an honest assessment leaves it open.

Two narrower limitations follow from explicit design choices. Bilateral credit means the
depositor bears counterparty risk up to the deposited amount; the mitigation is small
deposits settled often, not a trustless guarantee. And the inductive liability bound of §6.3
is only as strong as the store-boundary atomicity it rests on — the guarantee is conditional
on an implementation property, not unconditional.

---

## 10. Conclusion

> Juice is best understood as an **agoric, object-capability execution kernel for AI
> actions.** Its novelty is not any single mechanism but the invariant-preserving synthesis
> of funded calls, subtree-bounded prices, trace-local authority, atomic settlement, signed
> receipts, funded continuations, bilateral federation, and trade-backed reputation. The
> bound on liability follows by induction on the call tree, conditional on atomic fund
> movement at each call boundary. Its deployment thesis is that LLM orchestration removes the
> human-budgeting failure that weakened earlier computational economies, while signed
> bilateral receipts avoid the cost and coordination burden of global consensus.

Positioned against the field, Juice sits at the intersection of semantic service
composition, agoric settlement, object-capability authorization, workflow durability,
bilateral credit, and LLM tool selection — supplying the transaction law that each of those
lines gestured toward and each left incomplete in the same place: bounded execution, durable
authority, and auditable settlement for composed agent actions.

If the web's arc runs *read → write → own → wish*, then the orchestrator is what hears the
wish and the catalog of actions is what can grant it — but neither can do so safely until the
wish becomes a computation with a price it cannot exceed, an authority it cannot escape, and
a receipt that proves what was done. That is the layer Juice occupies: not the wish, and not
the granting, but the law that makes a granted wish bounded, paid for, and auditable.

---

## References

**Foundational and classical**

- Miller, M. S., & Drexler, K. E. (1988). *Markets and Computation: Agoric Open Systems.* In
  B. Huberman (Ed.), *The Ecology of Computation.* North-Holland.
- Miller, M. S., Morningstar, C., & Frantz, B. (2000). *Capability-Based Financial
  Instruments.* Financial Cryptography.
- Miller, M. S. (2006). *Robust Composition: Towards a Unified Approach to Access Control and
  Concurrency Control* (PhD thesis; the E language and object-capability security).
- Garcia-Molina, H., & Salem, K. (1987). *Sagas.* ACM SIGMOD.
- Moss, J. E. B. (1981). *Nested Transactions: An Approach to Reliable Distributed Computing*
  (closed nested transactions; contrast with open nesting, Weikum & Schek).
- Banga, G., Druschel, P., & Mogul, J. (1999). *Resource Containers: A New Facility for
  Resource Management in Server Systems.* OSDI.
- Waldspurger, C. A., et al. (1992). *Spawn: A Distributed Computational Economy.* IEEE TSE.
  (See also Waldspurger, lottery/stride scheduling.)
- Stonebraker, M., et al. (1996). *Mariposa: A Wide-Area Distributed Database System.*
- Haeberlen, A., Kouznetsov, P., & Druschel, P. (2007). *PeerReview: Practical Accountability
  for Distributed Systems.* SOSP.
- Kamvar, S., Schlosser, M., & Garcia-Molina, H. (2003). *The EigenTrust Algorithm for
  Reputation Management in P2P Networks.* WWW.
- Rivest, R., & Shamir, A. (1996). *PayWord and MicroMint: Two Simple Micropayment Schemes.*
- Birgisson, A., et al. (2014). *Macaroons: Cookies with Contextual Caveats for Decentralized
  Authorization in the Cloud.* NDSS.
- Asokan, N., Shoup, V., & Waidner, M. (1998). *Optimistic Fair Exchange of Digital
  Signatures.* EUROCRYPT.
- Smith, R. G. (1980). *The Contract Net Protocol.* IEEE Transactions on Computers.
- Thomas, S., & Schwartz, E. (2015). *A Protocol for Interledger Payments* (ILP).

**Semantic Web services**

- Berners-Lee, T., Hendler, J., & Lassila, O. (2001). *The Semantic Web.* Scientific American.
- The OWL-S / DAML-S coalition; WSMO (Web Service Modeling Ontology); WS-BPEL (OASIS).

**Object-capability networking**

- CapTP (Capability Transport Protocol), from the E language lineage.
- OCapN (Object Capability Network) and Goblins, Spritely Institute.
- UCAN (User-Controlled Authorization Networks).

**Tool-use research**

- Yao, S., et al. *ReAct: Synergizing Reasoning and Acting in Language Models.*
- Schick, T., et al. *Toolformer: Language Models Can Teach Themselves to Use Tools.*
- Patil, S., et al. *Gorilla: Large Language Model Connected with Massive APIs.*
- Qin, Y., et al. *ToolLLM / ToolBench.*

**Contemporary protocols and deployed systems** *(fast-moving; treat as pointers)*

- Anthropic (2024). *Model Context Protocol.* https://modelcontextprotocol.io
- Google / Linux Foundation (2025). *Agent2Agent (A2A) Protocol.* https://a2a-protocol.org —
  introduced April 2025; governance transferred to the Linux Foundation, June 2025.
- Google (2025). *Agent Payments Protocol (AP2).* https://ap2-protocol.org — announced
  16 September 2025; Intent/Cart/Payment Mandates as W3C Verifiable Credentials.
- Project NANDA, MIT Media Lab. https://nanda.mit.edu — "Internet of AI Agents"; decentralized
  registry, AgentFacts, gossip-based federation, Ed25519, portable reputation. See *Beyond
  DNS: Unlocking the Internet of AI Agents via the NANDA Index and Verified AgentFacts*
  (arXiv 2507.14263).
- Coinbase (2025). *x402: Payments for Agentic HTTP* (HTTP 402 over stablecoin rails;
  EIP-3009 / EIP-712). Lightning Labs, *L402.*
- x402 security analyses (2026): *Free-Riding in the AI Economy: Demystifying Logic Flaws in
  x402-Enabled Payment Systems* (arXiv 2605.30998); *Five Attacks on x402 Agentic Payment
  Protocol* (arXiv 2605.11781); Halborn, *x402 Explained: Security Risks & Controls* (2026).
  SDK signature-verification-bypass advisory disclosed March 2026.
- SingularityNET (AI marketplace, Multi-Party Escrow); Bittensor (validator-scored model
  work); Fetch.ai (agent marketplace); Ocean Protocol (data marketplace).
- Decentralized compute markets: Golem, Akash, iExec, Bacalhau.
- Workflow/mashup comparators: Yahoo Pipes, IFTTT, Zapier.
- Agent payment services: Skyfire, Nevermined (payment/authorization neighbors).

---

*This white paper is descriptive and positional. It reflects requirements specification
v0.4. Citations to recent protocols and security findings reflect material available as of
mid-2026 and should be treated as pointers to a fast-moving literature rather than settled
references.*
