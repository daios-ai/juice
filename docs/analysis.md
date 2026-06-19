
# Juice in Context

### An analysis of a priced-action execution kernel, its intellectual lineage, and its place in the agentic-web landscape

---

## Abstract

The web's eras can be told as a sequence of verbs: *read* (Web 1.0), *read/write* (Web 2.0), *read/write/own* (Web 3.0), and now *read/write/own/wish* — a web in which a user states a wish in natural language and services compose themselves to grant it. A wish, unlike a page view or a post, has to be turned into a bounded, payable, auditable computation before it can be safely granted. Juice is a kernel for that last step.

Concretely, Juice is a small Go kernel for *callable actions*: units of service that carry a price, run under explicit authority, and settle atomically with a signed receipt. Its design goal is unusual. Rather than build an agent platform, a marketplace, or a payment protocol, it isolates the **transaction law** such systems need and pushes everything else — orchestration intelligence, ranking formulas, the actual economy — to an application layer of actions.

This document does three things. It gives a self-contained account of what Juice is and the vision it serves. It maps the deep literature behind each of its mechanisms, since almost every one has decades of prior art. And it situates Juice against the 2025–2026 wave of agent protocols and agent-payment rails, where a striking pattern emerges: the contemporary field is rediscovering, often through published attacks, the two invariants Juice treats as axioms — that a payment proof must be bound to its request context, and that monetary movement and its audit record must commit together or not at all.

The conclusion is not that any single Juice mechanism is new. It is that the *invariant-preserving combination* — funded calls, subtree-bounded prices, trace-local authority, atomic settlement, signed receipts, funded continuations, bilateral federation, and trade-backed reputation, assembled as a minimal kernel without a blockchain — is not packaged together by any existing system, and that the combination answers a problem the rest of the field has left structurally open.

---

## 1. What Juice is

Juice's only execution primitive is a single dispatch call:

```
Call(caller, trace, action, args)
```

Everything runs through it: a user-initiated run, a script's subcall, the completion of a suspended step, an imported HTTP action, a federated remote call. `Call` dispatches on the action's *kind* (`native`, `wasm`, `http`, `remote_proxy`), never on who owns it. A separate band of **supervision** operations — creating users and actions, rating outcomes, depositing credits, importing APIs, peering with other kernels — deliberately never routes through `Call`. The separation is load-bearing: execution may err, and supervision supplies the correction signals (ratings, suspensions) that execution must not be able to manipulate.

Three ideas carry most of the weight.

**The role law.** Every call has a payer, a requester, and a payee, and every transaction records all three as immutable fields: the *process owner* who funds the work, the *caller* who immediately requested it, and the *action owner* who is paid on success. For a top-level run these collapse onto one person; for a subcall the requester is the parent action's owner, not the original user. Capturing all three at creation keeps financial history reconstructable even after an action is deleted.

**Subtree-bounded pricing.** An action's `price` is not a meter reading — it is a *bound on the entire call tree beneath it*, advertised worst-case by the provider. The caller is quoted one number that caps everything the call will spend, including its subcalls and any work it defers. Money is escrowed before execution: a run parks the price from the user's balance, and each subcall moves funds from a parent trace's available budget into its locked budget before the child runs. On success, whatever the call did **not** spend downstream is paid out to the provider as margin, taxed by a platform fee. Unused budget is *not* refunded — a price is a price, not an estimate. On failure, the unspent remainder rolls back up the tree to the caller, while any already-settled descendants stay paid.

**Funded continuations (Steps).** A Step is a partially applied future `Call`: a suspended computation that records enough context to resume when someone later supplies the missing input — a human approval, a webhook, an asynchronous result. Crucially, a Step is *funded at creation*: its price is parked the moment it is created and held until it completes or is cancelled. An outstanding Step keeps its process alive; completion spends the parked funds with no fresh solvency check. This is what makes long-running, human-in-the-loop composition safe and restartable.

Around these sit the supporting guarantees: file-backed storage where each monetary transition commits together with its audit record in one atomic store operation; crash recovery that settles interrupted work deepest-first; Ed25519-signed receipts over canonical JSON; a federation model in which a remote peer is simply a local user with a balance; and a stdlib of `@sys`-owned native actions (semantic lookup, LLM chat/embed/decide, action synthesis, time, messaging) that hold no special kernel privilege.

---

## 2. The vision Juice serves

The cleanest way to place Juice is on the arc of the web's defining verbs. Each era added one capability to the user's relationship with the network:

- **Web 1.0 — *read*.** Static, hyperlinked pages; an interconnected library you could only consume.
- **Web 2.0 — *read / write*.** The participatory web; users create and share, at the cost of data silos and advertising-driven monetization.
- **Web 3.0 — *read / write / own*.** The blockchain era and Chris Dixon's framing; users can own digital assets, though in practice ownership has largely meant speculation.
- **Web 4.0 — *read / write / own / wish*.** The user states a *wish* — an intent, in natural language — and the network composes services to fulfil it. The new affordance is not a new thing to consume, publish, or hold, but the ability to *ask for an outcome* and have the web assemble itself around the request.

That fourth verb is the whole point, and it is also the reason a settlement kernel is required. A wish is not free. To be granted, it must be turned into a bounded, payable, auditable computation: someone composes the services, each provider must be paid, the total cost must be capped in advance, and the result must be checkable after the fact. "Wish" is the affordance; **bound, pay, settle, audit** is what has to happen underneath for the affordance to be safe. Juice is the substrate for that underneath.

Concretely, Juice is the execution-and-settlement layer for the "Web 4.0" vision articulated by DAIOS: an AI-orchestrated, composable web in which a user states an intent in natural language and an orchestrator assembles online services on the fly to fulfil it, paying each provider a small fee per use.

That vision names three hard problems: building a distributed orchestration AI; finding a sustainable micropayment-based monetization model to replace advertising and subscriptions; and ensuring trust and safety through *ex-post* feedback (ratings, reputation) rather than only *ex-ante* rules. It also revives an older idea — Berners-Lee's Semantic Web — and argues that what stalled the Semantic Web (the impracticality of hand-annotated ontologies and logical reasoners over messy, dynamic data) is exactly what large language models now dissolve, because they can interpret unstructured descriptions and select among services directly.

Juice is the *how* to that *why*, but narrowly. It does not try to be the orchestration AI. It provides the primitives that make priced composition safe, atomic, and auditable, and leaves the intelligence — which action to call, how to rank the catalog, how to compose a workflow — to the application layer. This restraint is the design's central virtue and recurs throughout the analysis below.

---

## 3. Intellectual lineage

### 3.1 Agoric and object-capability computing — the primary ancestor

Juice's closest intellectual home is the *agoric* tradition: Miller and Drexler's *Markets and Computation: Agoric Open Systems* (1988), which framed computation itself as a market with prices on computational objects, escrow, and capabilities as the substrate. Mark Miller's later work — the E language, *Capability-based Financial Instruments* (Miller, Morningstar & Frantz, 2000), and the object-capability (ocap) security model formalized in *Robust Composition* (2006) — supplies the other half. Juice's WASM execution model is textbook ocap: scripts receive no ambient filesystem, network, environment, or token access, only explicit host functions (`juice.call`, `juice.step_create`, `juice.step_complete`). That is the *principle of least authority* and the "powerbox" pattern realized directly.

The sharpest single analogue lives here: **Zoe**, the smart-contract framework from Agoric (the present-day company continuing this lineage), guarantees *offer safety* — a participant either receives the payout they specified or gets their escrowed assets back, enforced by the framework rather than by contract code. Juice gives a structurally analogous kernel property for action calls: validate input before locking funds, execute under explicit authority, settle only after the output validates, and refund the unspent remainder on failure. The match to Zoe is much tighter at the *settlement-safety invariant* than at the broad "computation as a market" metaphor, and it is the right named reference for what Juice guarantees.

### 3.2 The money model — gas, micropayments, computational economies

The strikingly close operational analogue is **Ethereum gas**: a caller prepays against a worst-case execution bound, a fee is taken, and computation proceeds against the bound. But the comparison must be drawn precisely, because Juice *inverts* the central rule. Ethereum *meters resource consumption* and refunds unused gas; Juice *prices composed service delivery* and keeps the unspent allocation as the provider's margin. The economic equations are therefore different in kind — Ethereum charges for work performed, Juice charges for an outcome delivered under a bound — and the refund inversion is a deliberate, distinctive choice rather than an incidental one.

The broader lineage of market-based resource allocation includes Waldspurger et al.'s *Spawn* (1992), Stonebraker's *Mariposa* (market-based distributed query execution), and the Tycoon and POPCORN systems collected in *The Ecology of Computation*. The micropayment strand runs through Rivest and Shamir's *PayWord and MicroMint* (1996), DEC's *Millicent*, and *Mojo Nation* (c. 2000), the P2P token economy that was an ancestor of BitTorrent. This history matters for a reason returned to in §6: it is largely a history of systems that did not achieve liquidity.

### 3.3 Durable execution and long-lived transactions — what Steps are

Juice's failure semantics are the *saga* pattern (Garcia-Molina and Salem, 1987) given a money meaning: a long-running activity composed of steps, where failure triggers compensation rather than a global rollback, and committed sub-activities are not undone. A precision note is warranted here. Classic *closed* nested transactions (Moss, 1981) make a child's commit provisional until the top-level transaction commits — a parent abort rolls its committed children back. That is the **opposite** of Juice's "settled descendants stay paid." The accurate lineage for that property is sagas together with *open* nested transactions (Weikum and Schek), where subtransaction commits are irrevocable and failure is handled by compensation.

On the engineering side, Juice's Steps are durable continuations in the family of **Temporal, AWS Step Functions, Azure Durable Functions, and Cadence** — with one element those systems lack: *funding at suspension*. A Step does not merely record where to resume; it escrows the money that the resumption will cost. That funded-continuation property is the genuinely distinctive contribution to the durable-execution lineage.

### 3.4 Hierarchical resource accounting

The trace's `available`/`locked` budget, threaded through a tree of subcalls, is hierarchical resource accounting applied to money rather than CPU. The relevant prior art is **resource containers** (Banga, Druschel and Mogul, OSDI 1999), which separated the accounting principal from the thread of control, and **lottery/stride scheduling** (Waldspurger), whose hierarchical ticket-and-currency model — sub-budgets funded from parent budgets — is a close structural parallel to trace funding.

### 3.5 Accountability and verifiable receipts

Juice's signed, immutable receipts and its `tx verify` path are tamper-evident settlement records in the spirit of **PeerReview** (Haeberlen et al., SOSP 2007) and the accountability-in-distributed-systems line — and pointedly *not* in the spirit of blockchain consensus. Juice achieves auditability through signatures over canonical JSON and immutable transactions, with **no global agreement protocol at all**. Avoiding consensus is a feature, not a gap, and it is the axis on which Juice differs most cleanly from the entire deployed field (§5).

### 3.6 Reputation via gossip

Juice's gossip layer — trade-backed statistics, weighted toward introducers, dominated over time by a kernel's own settled experience — belongs to the P2P reputation family whose canonical member is **EigenTrust** (Kamvar et al., WWW 2003). But the relationship is one of *keeping the signals while rejecting the central mechanism*. EigenTrust's defining move is *transitive* trust propagation (a global trust vector computed across the network). Juice's gossip is deliberately **non-transitive for authority**: it is information only, it never grants callability, and a kernel still has to friend a peer directly and verify manifests from the owner. Reputation shapes *ranking*; it never shapes *permission*.

### 3.7 Semantic Web services — the lineage Juice is repairing

This is more than a neighboring field; it is the prior attempt at Juice's exact problem. **OWL-S** (originally DAML-S), **WSMO**, and **BPEL** tried to make services machine-discoverable, machine-invocable, and machine-composable through formal, machine-readable descriptions. OWL-S in particular explicitly targeted automatic service selection and composition. They did not reach broad adoption, for the reason the DAIOS vision itself diagnoses: formal ontology annotation and logical reasoning did not scale over unstructured, dynamic data.

Juice's move is precisely the substitution that vision proposes: *weaken the ontology ambition, strengthen the operational semantics.* Schemas plus natural-language descriptions support lookup and LLM-based selection in place of formal ontologies and a logical reasoner, while `Call` supplies the authority, price, settlement, and trace state those service frameworks never had. The whole document set thus forms one argument: Semantic Web services were the first attempt; LLMs are the missing reasoner; Juice is the operational realization with the reasoner swapped out and a settlement law added.

### 3.8 Contract Net and multi-agent negotiation

The **Contract Net Protocol** (Reid Smith, 1980) is the antecedent for task allocation among distributed agents: a node announces a task, others bid, and work is awarded through negotiation. Juice's catalog, lookup, ratings, and prices make a related structure executable — but with escrow and binding receipts in place of merely communicative commitments. One distinction is worth flagging: Contract Net is an *auction*, whereas Juice is *posted-price* (providers set prices; `@sys/lookup` ranks rather than runs a bidding round). Contract Net is an ancestor of the market-for-tasks idea, not of Juice's specific price-discovery mechanism.

### 3.9 Capability authorization — local and distributed

Beyond generic authentication, the closer relatives are **caveat-bound, delegable** authorization schemes: **macaroons** (Birgisson et al., NDSS 2014), **SPKI/SDSI**, and the more recent DID-based **UCAN** (User-Controlled Authorization Networks). Juice's process authority and trace authority can be read as store-backed capabilities whose caveats are economic and causal — caller, process, trace, action, price, and required completer all constrain what a holder may do.

There is a gap in the usual telling, though: ocap *over the wire*. Juice's remote-proxy actions and signed-manifest federation are distributed object-capability messaging, and the lineage for that is **CapTP** (the capability transport protocol from the E/Agoric world) and its contemporary successor **OCapN** (the Object Capability Network effort from the Spritely Institute). Juice's federation is, in effect, a *money-bearing* cousin of CapTP/OCapN — distributed capability invocation with settlement attached.

### 3.10 Fair exchange and exactly-once — the formal frames

Two classical frames name what Juice enforces and what its contemporary rivals lack (§5).

The property "money and delivery commit together, or neither does" is **optimistic fair exchange** (Asokan, Shoup and Waidner). Juice realizes it at the store boundary by committing the monetary transition and its audit record in a single atomic write. Naming it gives the atomicity invariant a theoretical pedigree rather than leaving it an implementation detail.

The dedup mechanism behind Juice's `IdempotencyRecord` and its federation handshake — insert a pending record before execution, key it uniquely per counterparty, return the stored receipt on replay — is **exactly-once delivery** from the distributed-systems literature. As §5 shows, this is precisely the mechanism the agent-payment field is now patching in after the fact.

---

## 4. The contemporary landscape

### 4.1 Agent protocols: MCP, A2A, AP2, NANDA

A standards stack for the "agentic web" took shape rapidly across 2025. Each layer is real and useful, and each stops short of a transaction law.

- **MCP (Model Context Protocol, Anthropic, 2024)** standardizes how tools, resources, and prompts are exposed to model applications. It has no economic or settlement layer.
- **A2A (Agent2Agent, Google, April 2025; moved to the Linux Foundation in June 2025)** standardizes agent-to-agent discovery and task delegation via "Agent Cards" over ordinary web transports. It is payment-agnostic by design.
- **AP2 (Agent Payments Protocol, Google, September 2025)** is the payments companion to A2A and MCP, developed with 60-plus payments and technology partners. It introduces three cryptographically signed *Mandates* — Intent, Cart, and Payment — carried as W3C Verifiable Credentials, and treats stablecoin rails (via an A2A-x402 extension) as first-class alongside cards and bank transfers. It is an authorization-and-proof layer, not an execution semantics.
- **NANDA (MIT Media Lab)** is the most architecturally adjacent. It aims at an "Internet of AI Agents" with a decentralized registry that functions like DNS for agents, an AgentFacts schema, W3C Verifiable Credentials and Ed25519 signatures, gossip-based federation between registries, and portable reputation scoring.

NANDA deserves emphasis because it independently arrived at Juice's *exact federation vocabulary* — gossip + Ed25519 + portable reputation across a federated registry. The right framing is complementary halves rather than rivals: **NANDA is a candidate discovery/identity/reputation layer for the agentic web; Juice is the execution/settlement layer.** Like the others, NANDA carries no funded-`Call`, subtree settlement, or receipt-bound transaction semantics.

### 4.2 Agent payment rails — and the validation of Juice's invariants

The "pay-per-call for agents" rails are **L402** (Lightning Labs; Lightning plus macaroons over HTTP 402), **x402** (Coinbase; HTTP 402 revived over stablecoin rails using EIP-3009 authorizations and EIP-712 domain separation), and startups such as **Skyfire** and **Nevermined**. These are payment and authorization neighbors, not execution neighbors — they attach a payment to an HTTP request; they do not define authority, funded execution, settlement, traceability, and recovery as one transition system.

The most important finding in this entire analysis lives here. A wave of 2026 security research on x402 has formalized "security invariants" for agentic payments and shown that current implementations **fail to enforce transactional atomicity and cryptographic context binding**. The documented failure modes are exactly two:

1. **Proofs not bound to context.** A "semantic gap in signature design" permits *cross-resource substitution* — a payment proof minted for one context is replayed against another. Related work documents replay and double-charge because there is no application-layer nonce binding the proof to the request.
2. **Non-atomic verify-then-settle.** The gap between off-chain verification and on-chain settlement lets the settlement path fail to bind caller, facilitator, resource, and service decision into one atomic object, producing *paid-but-denied* and *unpaid-service* outcomes. A real signature-verification-bypass vulnerability was disclosed in the x402 SDK in March 2026, and the verify/settle gap was noted as unresolved in x402 v2.

These are precisely the two properties Juice takes as axioms. Its inbound federation signature covers an `args_hash`, binding the payment proof to the exact request body and counterparty — closing failure (1). Its money path commits the monetary movement and the audit record in a single atomic store operation — closing failure (2), which is the optimistic-fair-exchange property of §3.10. The blunt summary: **the agent-payment field is currently rediscovering, through CVEs and formal attack papers, the invariants Juice designed in from the start.** This is the strongest, most timely point in Juice's favor.

### 4.3 AI-service and compute markets — the deployed comparators

Several live systems occupy adjacent ground:

- **SingularityNET** is the closest *deployed* AI-services marketplace by feature surface: service registry and invocation, payments, ratings, usage metrics, and a Multi-Party Escrow contract. Its resemblance to Juice is *institutional, not semantic* — it markets and pays for AI services, but defines no universal funded `Call`, no hierarchical trace wallets, no subtree settlement, and no signed-receipt semantics.
- **Bittensor** is as close on a *different* axis: a live network that prices and rewards model work under a validator-scored, reputation-weighted incentive, paralleling Juice's trade-backed reputation more directly than SingularityNET does.
- **Fetch.ai** (agent marketplace) and **Ocean Protocol** (data marketplace, with datatoken-gated access) round out the AI-economy field.
- **Decentralized compute markets** — **Golem, Akash, iExec, Bacalhau** — are the right *contrast* class. They price resources: CPU, containers, GPUs, compute-near-data. Juice prices *service outcomes* under a subtree bound. The distinction is exact and worth preserving.

The unifying observation across all of these is the through-line that most cleanly separates Juice from the entire live field: **every deployed comparator settles on external infrastructure — a blockchain or a Lightning-style channel.** SingularityNET's escrow is on-chain, Bittensor's emissions are on-chain, x402's rails are on-chain. Juice's distinctive deployment assumption is *bilateral credit plus signed receipts*: a peer is an ordinary user with a balance, netting and counterparty risk fall out of the normal wallet model (the trust posture of correspondent banking, not of trustless channels), and settlement is auditable **without global consensus**.

### 4.4 Workflow and mashup systems — demand-side evidence

**Yahoo Pipes, IFTTT, and Zapier** are product comparators rather than research, and they establish one fact cheaply: users genuinely want service composition. But their unit is a workflow rule or an integration, not a priced action with a settlement invariant. Juice makes the same composition economic and federated — every composed step is also a funded call with trace state.

### 4.5 Tool-use research — the AI-side lineage

The orchestration half of the picture has its own literature: **ReAct** (interleaved reasoning and acting), **Toolformer** (self-supervised learning of when to call an API), **Gorilla** (selecting from a massive API set), and **ToolLLM/ToolBench** (training and benchmarking tool use). `@sys/lookup` and `@sys/llm/decide` sit on this same axis — which tool to call, and how to form its arguments, under uncertainty. Juice adds the term this research omits: a tool choice is not only a semantic match but a *priced, authorized, auditable commitment* with downstream settlement consequences.

---

## 5. What is genuinely distinctive

No single mechanism above is novel. The distinctiveness is the combination, and it is real because no existing system packages it together:

1. a single dispatch primitive (`Call`) for every execution path;
2. subtree-bounded prepaid escrow in which unused budget is provider margin, not a refund;
3. atomic settlement-with-signed-receipt as one store operation;
4. funded continuations (Steps) for safe, restartable asynchronous composition;
5. federation as "a peer is just a user," yielding bilateral netting with no separate money model; and
6. capability-mediated execution plus trade-backed, non-transitive reputation —

all as a *minimal kernel* with the economy pushed to the application layer, and **without a blockchain**. The agoric program imagined much of this in 1988; the nearest living relative, Agoric the company, realizes the capability-and-finance half but on a consensus chain. Juice's clean separation from that nearest relative is the no-consensus, bilateral-trust, signed-receipt deployment choice.

---

## 6. The central technical claim and its operational premise

Juice's core safety property can be stated as an induction, provided its premise is stated alongside it.

At every call boundary, the kernel checks `available ≥ price` before creating the child, and the write that moves funds from `available` into `locked` and creates the child's allocation is **atomic at the store boundary**. Therefore, by induction on the execution tree, the sum of all outstanding and settled descendant commitments under any node cannot exceed that node's allocation; at the root, the user's quoted price bounds the entire tree's liability. Combined with transaction immutability and receipt signing, every settlement claim is checkable after the fact.

The caveat must travel with the claim: *the mathematical invariant depends on the store-boundary invariant.* The induction is only as strong as the atomic precondition it rests on. The clean inductive guarantee is downstream of the §5/atomicity property, not independent of it — which is exactly why the contemporary rails that lack that atomicity (§4.2) cannot make the equivalent claim.

---

## 7. The skeptical judgment

A clear-eyed analysis must confront the fact that Juice's lineage is also a graveyard. Spawn, Mariposa, Mojo Nation, Millicent — the computational-economy program has a multi-decade record of *not* reaching liquidity, in large part because humans do not want to manage fine-grained computational budgets.

Juice has a plausible answer to why this moment might differ, and it is worth stating as a claim rather than a hope. **LLM orchestration removes the human budgeting burden** — the model chooses the calls — while **subtree pricing collapses a composed execution tree to a single visible bound** the user actually sees. The success condition therefore is not the elegance of the kernel; it is *liquidity in useful actions*. The kernel can make trade safe and auditable. It cannot, by itself, make the market thick. Whether a population of priced, callable actions worth composing actually materializes is an empirical question the design cannot settle, and an honest assessment leaves it open.

---

## 8. Final formulation

> Juice is best understood as an **agoric, object-capability execution kernel for AI actions**. Its novelty is not any single mechanism but the invariant-preserving synthesis of funded calls, subtree-bounded prices, trace-local authority, atomic settlement, signed receipts, funded continuations, bilateral federation, and trade-backed reputation. The bound on liability follows by induction on the call tree, conditional on atomic fund movement at each call boundary. Its deployment thesis is that LLM orchestration removes the human-budgeting failure that weakened earlier computational economies, while signed bilateral receipts avoid the cost and coordination burden of global consensus.

Positioned against the field, Juice sits at the intersection of semantic service composition, agoric settlement, object-capability authorization, workflow durability, bilateral credit, and LLM tool selection — supplying the transaction law that each of those lines gestured toward and each left incomplete in the same place: bounded execution, durable authority, and auditable settlement for composed agent actions.

If the web's arc runs *read → write → own → wish*, then the orchestrator is what hears the wish and the catalog of actions is what can grant it — but neither can do so safely until the wish becomes a computation with a price it cannot exceed, an authority it cannot escape, and a receipt that proves what was done. That is the layer Juice occupies: not the wish, and not the granting, but the law that makes a granted wish bounded, paid for, and auditable.

---

## References

**Foundational and classical**

- Miller, M. S., & Drexler, K. E. (1988). *Markets and Computation: Agoric Open Systems.* In B. Huberman (Ed.), *The Ecology of Computation.* North-Holland.
- Miller, M. S., Morningstar, C., & Frantz, B. (2000). *Capability-Based Financial Instruments.* Financial Cryptography.
- Miller, M. S. (2006). *Robust Composition: Towards a Unified Approach to Access Control and Concurrency Control* (PhD thesis; the E language and object-capability security).
- Garcia-Molina, H., & Salem, K. (1987). *Sagas.* ACM SIGMOD.
- Moss, J. E. B. (1981). *Nested Transactions: An Approach to Reliable Distributed Computing* (closed nested transactions; contrast with open nesting, Weikum & Schek).
- Banga, G., Druschel, P., & Mogul, J. (1999). *Resource Containers: A New Facility for Resource Management in Server Systems.* OSDI.
- Waldspurger, C. A., et al. (1992). *Spawn: A Distributed Computational Economy.* IEEE TSE. (See also Waldspurger, lottery/stride scheduling.)
- Stonebraker, M., et al. (1996). *Mariposa: A Wide-Area Distributed Database System.*
- Haeberlen, A., Kouznetsov, P., & Druschel, P. (2007). *PeerReview: Practical Accountability for Distributed Systems.* SOSP.
- Kamvar, S., Schlosser, M., & Garcia-Molina, H. (2003). *The EigenTrust Algorithm for Reputation Management in P2P Networks.* WWW.
- Rivest, R., & Shamir, A. (1996). *PayWord and MicroMint: Two Simple Micropayment Schemes.*
- Birgisson, A., et al. (2014). *Macaroons: Cookies with Contextual Caveats for Decentralized Authorization in the Cloud.* NDSS.
- Asokan, N., Shoup, V., & Waidner, M. (1998). *Optimistic Fair Exchange of Digital Signatures.* EUROCRYPT.
- Smith, R. G. (1980). *The Contract Net Protocol: High-Level Communication and Control in a Distributed Problem Solver.* IEEE Transactions on Computers.
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
- Google / Linux Foundation (2025). *Agent2Agent (A2A) Protocol.* https://a2a-protocol.org — introduced April 2025; governance transferred to the Linux Foundation, June 2025.
- Google (2025). *Agent Payments Protocol (AP2).* https://ap2-protocol.org — announced 16 September 2025; Intent/Cart/Payment Mandates as W3C Verifiable Credentials.
- Project NANDA, MIT Media Lab. https://nanda.mit.edu — "Internet of AI Agents"; decentralized registry, AgentFacts, gossip-based federation, Ed25519, portable reputation. See *Beyond DNS: Unlocking the Internet of AI Agents via the NANDA Index and Verified AgentFacts* (arXiv 2507.14263).
- Coinbase (2025). *x402: Payments for Agentic HTTP* (HTTP 402 over stablecoin rails; EIP-3009 / EIP-712). Lightning Labs, *L402.*
- x402 security analyses (2026): *Free-Riding in the AI Economy: Demystifying Logic Flaws in x402-Enabled Payment Systems* (arXiv 2605.30998); *Five Attacks on x402 Agentic Payment Protocol* (arXiv 2605.11781); Halborn, *x402 Explained: Security Risks & Controls* (2026). SDK signature-verification-bypass advisory disclosed March 2026.
- SingularityNET (AI marketplace, Multi-Party Escrow); Bittensor (validator-scored model work); Fetch.ai (agent marketplace); Ocean Protocol (data marketplace).
- Decentralized compute markets: Golem, Akash, iExec, Bacalhau.
- Workflow/mashup comparators: Yahoo Pipes, IFTTT, Zapier.
- Agent payment services: Skyfire, Nevermined (payment/authorization neighbors).

---

*This analysis is descriptive and positional. Citations to recent protocols and security findings reflect material available as of mid-2026 and should be treated as pointers to a fast-moving literature rather than settled references.*