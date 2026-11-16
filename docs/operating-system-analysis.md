# Juice as an operating system

An evaluation of Juice against operating-system, protocol and blockchain precedents. Written
2026-11-14 and extended 2026-11-15, against requirements.md and the code at v0.20.1, not on
measurements of a live network. It is context, not contract: requirements.md governs. It records
analysis, not decisions: no remedy named here has been adopted.

## 1. Verdict

Juice merits existing as a research system. Its core mechanism is new and it answers a real,
timely problem. It does not yet merit the title of an operating system for the Web.

- The design is strong on authority and budgeting.
- It is weak on custody, on openness to a second implementation, and on what reputation buys.
- The implementation is unusually well tested, but it is structured for one author and one
  machine per kernel.

Almost every individual mechanism has a precedent. The novelty lies in a few specific mechanisms
and in the combination. The closest intellectual ancestor is the agoric open-systems programme
(Miller and Drexler, 1988). Juice is, to our knowledge, the first to implement that programme for
web services with real settlement.

## 2. Does it merit existing?

- **The problem is real.** Micropayments failed for people because of the mental cost of each
  decision (Szabo, 1999; Odlyzko, 2003). Millicent, Flattr and Coil all closed. Software agents
  do not bear that cost, so the old objection no longer applies.
- **The contribution is distinct.** x402 and L402 pay per HTTP request, before service. Neither
  bounds the cost of a tree of calls, separates payer from caller, or gives a signed receipt
  binding the reply. Juice pays after service, on a receipt, which is why it also needs identity,
  idempotency and credit.
- **The adoption cost is high.** x402 adds a status code and two headers to existing HTTP. Juice
  requires both parties to join a kernel, a two-sided cold start. The live network is three nodes
  upgraded in lockstep, and there is no evidence of outside users.

## 3. Evaluation by axis

| Axis | What Juice does | Closest precedent | Assessment |
|---|---|---|---|
| Process model | One dispatch primitive (`Call`); a process is one funded run; natives are the only way into kernel state | Unix `fork`/`exec` and syscalls; Plan 9's single uniform interface | Strong: the uniformity is real, and one code path serves local, composed and remote calls |
| Identity | A principal is (kernel key, user id); names are translated at the edge | Kerberos `user@REALM`, Windows SIDs, NFSv4 | Sound and consistent, but bound to one kernel (section 5) |
| Authority | Every call records payer, caller and target; value moves only from the immediate caller | Ethereum `msg.sender` versus `tx.origin`; capability systems (Hardy's confused deputy) | Strong: the confused-deputy rule is built into the role law, where Ethereum leaves it to each contract author |
| Resource bound | A fixed advertised price bounds the whole call subtree; unused budget is the provider's margin | Ethereum gas limit; cloud per-request pricing | Novel (below); the right allocation of risk (section 7) |
| Settlement | Per-call, between kernels, with a probabilistic ticket for amounts too small to pay | Rivest's lottery micropayments (1997); Orchid; Lightning | Sound: the expected payment equals the debt, and neither side can pick the outcome. One scheme among possible ones, not the protocol's core (section 8) |
| Decentralization | Federation of sovereign kernels; no global ledger, no registry, no consensus | Email, ActivityPub; petnames (Stiegler) | Honest trade-off: cheap and fast, but each user trusts their own operator |
| Trust | Terms are verified before the call; conduct is judged afterwards by public, signed, privacy-preserving trade evidence | eBay feedback; EigenTrust | Coherent, but the weakest axis (section 6) |
| The Web | OpenAPI import, HTTP actions, OAuth consent delegated per action and honoured across kernels | RapidAPI; OAuth; x402 | Practical: existing services join without being rewritten |

### Novelty

1. **Price as a recursive budget.** Ethereum meters computation and makes the caller guess a
   limit. Juice lets the provider state one all-in price, and the kernel enforces it over
   everything the action calls. Agoric systems and Spawn (Waldspurger, 1992) priced computation,
   but we know of none that made the price a transitive bound enforced by the kernel. Two
   guarantees are involved, and they differ in kind:
   - the caller is protected by the fixed price alone, since no one downstream can charge it;
   - enforcement over sub-calls protects the provider from its own program, and guarantees
     sub-providers that their work is funded (G6). This is a guarantee of the host, not a
     property two parties agree on over the wire (section 8).
2. **Paid suspended computation.** A task is a continuation with its money already reserved,
   resumable only by one named party, across kernels. Unix has no equivalent; Ethereum has none
   without a custom contract.
3. **Bounded-risk service to strangers without a blockchain.** One exposure number per kernel
   caps unpaid work across all peers, so creating identities cannot multiply it. This replaces
   payment channels and on-chain escrow with a credit limit.
4. **Economic roles in the kernel.** Payer, caller and payee are distinguished on every call,
   and the fee taxes value added, not gross flow. Operating systems have user accounting; none
   has a value-added tax.

### Weaknesses

- **Custody.** A user's balance is an entry in their operator's ledger. Nothing lets the user
  verify solvency, as a blockchain would. Operators hold balances that settle in a stablecoin,
  which is custodial e-money; the spec does not address the regulatory barrier.
- **Identity bound to the host.** A principal cannot leave its kernel and keep its name, its
  actions' addresses or its reputation (section 5).
- **Reputation.** Self-bought ratings cost only fees. Identities are free, so a bad record can be
  discarded. A seller with a good record can spend it in one fraud. Nothing in the kernel grows
  with standing (section 6).
- **Variance.** Lottery settlement is fair in expectation only; a small kernel can see large
  swings.
- **Liveness coupling.** Every served call asks the kernel where the run began who owns it. This
  buys uniform treatment of the process owner at the cost of availability. OAuth has the same
  trade-off between token introspection (RFC 7662) and self-contained signed tokens (RFC 9068).
- **One implementation.** The wire protocol is about a tenth of a spec that changes with the
  code, and it has no versioning (section 8).
- **Structure.** The code is organised for one author and one machine per kernel (section 10).

An earlier version of these notes listed fixed prices as a weakness, saying providers must price
for the worst case. That was too broad and is withdrawn (section 7).

## 4. Is it an operating system?

- **By Tanenbaum's taxonomy it is a network operating system, not a distributed one.** Users see
  kernel names in every address, so the system does not appear as one machine. A key held by the
  principal would make the kernel a location and not part of the identity (section 5).
- **The symmetry claim is too strong as worded.** "Only the cost may differ" between local and
  remote calls overstates. Waldo et al. (1994) show that latency and partial failure always
  differ. What Juice guarantees is narrower: a call's crossing a kernel does not change what a
  program can see or do. The wording should say that.
- **The kernel is not minimal in the code.** Liedtke's rule is applied to natives, but the
  `kernel` package also holds OpenAPI import, the schema grammar, discovery and federation, in
  14,926 lines.

The framing is justified as a design method: it produced principals and `localhost`, and the
abstractions map cleanly. As a product claim it holds only in the sense in which Ethereum is
called a "world computer".

## 5. The principal as the fundamental unit

Juice is better described as an object-based system whose unit is the principal than as "kernels
are machines, actions are programs".

**Why the principal.**

- An economic system needs a unit that owns money, makes contracts, and bears liability and
  reputation. A process is transient, so it cannot own. A machine is not a party to a trade.
  Only the principal qualifies, as the legal person does in commerce.
- In Ethereum the account is the fundamental unit: an address with a balance and code.
- In Kerberos users and services are the same kind of principal.
- In Clouds, objects are passive and persistent, and threads travel through them. In Argus,
  guardians own resources and expose handlers. In Wegner's taxonomy these are object-based, not
  object-oriented, since there is no inheritance.

**The mapping.**

| Object-based OS | Juice |
|---|---|
| Object | Principal |
| Method | Action, already written `owner@kernel/name` |
| Interface | The principal's public actions and their schemas |
| State | Balance, grants and secrets in the kernel; all else in the principal's own services |
| Active object (actor) | A daemon holding the principal's login |
| Thread crossing objects | A process: one funded run crossing principals |
| Node | Kernel |
| System object | `sys`, whose methods are the natives |

Nothing in the table is new. Actions have owners, tasks address principals, and evidence concerns
a named principal (G10). The process remains the dynamic unit, since budgets belong to runs and
not to objects.

**What it settles.**

- *Kernel minimality.* The kernel keeps only what a principal cannot do for itself: hold
  balances, dispatch calls, enforce the price, and sign receipts. Everything else is a
  principal's action, the standard library included.
- *The protocol's unit.* Over HTTP the unit is a principal with a signed list of offers, as an
  ActivityPub actor is a document with an inbox.

**What it exposes.**

1. *A principal cannot leave its kernel.* Its identity is its user id plus its kernel's key, and
   the operator keeps its balance. On another kernel it is a new principal: its actions get new
   addresses and its ratings stay behind. Emerald and Amoeba gave objects identifiers that did
   not say where the object lived. AT Protocol does the same on the Web: the user holds a key,
   the key is the identity, and the user can change servers. A key held by the principal would
   also mean an operator could not act as the user without the user's signature.
2. *Access is decided by identity, and that has a known weakness.* Access can be controlled by
   who is asking (Unix permissions) or by possession of a token (a door key; Dennis and Van Horn,
   1966). Juice must record who paid whom and attach ratings to a named party, and a token says
   nothing about its holder, so identity is the right choice. Its weakness is the confused
   deputy (Hardy, 1988): an action runs with its owner's rights, and a caller may trick it into
   using them for the caller. The role law blocks this for money. Every new kind of right needs
   the same protection.
3. *A bad reputation can be thrown away,* because creating a principal costs nothing (section 6).

Custody, the portability of reputation, and the visibility of kernel names share one remedy: a
key held by the principal.

## 6. Reputation

**Standing is earned over time.** Where identities are free, making newcomers "pay their dues" is
the best outcome available (Friedman and Resnick, 2001). People accumulate reputation over a
life, and it gives them more affordances. Starting with nothing is therefore a property to design
for, not a flaw. Discarding a record costs the affordances built on it.

Three conditions make this work.

1. *An empty record must be visible.* Buyers need to see how much history stands behind a
   principal, not only its average rating. Evidence is published per trade, so the volume is
   countable.
2. *A record must be costly to build.* Evidence comes only from real trades, which cost fees.
   This holds.
3. *Standing must buy something.* Today a good record brings only more buyers. Nothing in the
   kernel grows with it: the credit limit is per kernel, and price and visibility are the same
   for everyone.

A record accumulated over a lifetime is a principal's most valuable asset. While it is bound to
one kernel the operator holds it hostage, which strengthens the case for a portable identity.

**Fraud by an established seller has no complete solution.** A rational seller cheats when the
one-time gain exceeds the value of its future honest income (Klein and Leffler, 1981; Shapiro,
1983). Deterrence needs the loss to be at least the gain. Only three things supply that: a future
worth keeping, forfeited collateral, or a limit on the gain. Eliminating the fraud would require
verifying conduct, which G10 rules out.

| Remedy | How it works | Fit with Juice |
|---|---|---|
| Bond | The seller deposits money that is lost on fraud | Poor: someone must judge that fraud occurred, and the kernel never judges conduct |
| Delayed payout | Earnings are held for a period, as eBay and Amazon do | Same difficulty: a hold is useful only if something can cancel it |
| Limiting the gain | Buyers accept from a seller only what its record justifies | Good: needs no judge |
| Fast, recent evidence | Bad ratings spread quickly and recent trades count most (Resnick et al., 2000) | Good: gossip serves the newest evidence per action |

Juice's structure favours the last two. Prices are small and paid per call, so one buyer loses
little. The gain from a fraud is roughly the price times the calls made before bad ratings
spread. If the price a buyer accepts is tied to the length of the seller's record, the gain stays
below the value of the record. This is a decision for buyers and their agents. The kernel's part
is to make the record's length and recency easy to read.

## 7. Fixed prices

The seller, not the buyer, should calculate the cost. In Ethereum the burden of deciding how much
to pay lies with the caller, which works but adds considerable friction. When the seller states a
price, the buyer only accepts or declines, as in a marketplace. This adds inefficiency, but
inefficiency always appears under uncertainty: someone bears the risk, and the question is who
bears it most cheaply.

**Why the seller.**

- *Information.* The seller knows the distribution of its own costs. A buyer sees one call and
  cannot estimate it. Ethereum wallets must guess a gas limit for this reason, and a transaction
  that runs out is still charged.
- *Pooling.* The seller serves many calls, so its average cost is stable by the law of large
  numbers. It can price near the mean, as an insurer does. The buyer has one draw.
- *Incentive.* Under metering the seller earns more by using more. Contract theory finds the same
  of cost-plus contracts, while fixed-price contracts reward efficiency (Bajari and Tadelis,
  2001).
- *Composition.* A fixed price is closed under composition: a provider that buys at fixed prices
  can quote a fixed price. One metered child makes every ancestor's cost uncertain, so the
  recursive bound requires fixed prices at every level.

**Precedents.**

| Precedent | What it shows |
|---|---|
| Ethereum | Gas estimation was a lasting source of friction. EIP-1559 was adopted to make the fee predictable, and paymasters let applications pay gas for their users |
| Telephony and Internet access | Users consistently prefer flat rates and pay more to avoid metering (Odlyzko, 2001) |
| Web APIs | Many already charge per call: per message, per search, per image |
| Trades and construction | A fixed quote after inspection is the norm where the buyer cannot judge the cost |

**Comparison.**

| | Fixed price by the seller | Metered with a buyer's limit |
|---|---|---|
| Buyer's cost before the call | Known | Unknown up to the limit |
| Who bears the variance | Seller, who can pool it | Buyer, who cannot |
| Seller's incentive | Reduce cost; may also reduce quality | Use more |
| Composition | Bounded at every level | Unbounded without a limit at every level |
| Efficiency | Light requests subsidise heavy ones | Each request pays its own cost |
| Open-ended work | Hard to price | Natural |

**The remaining costs.**

1. *Adverse selection.* At one flat price, buyers with costly inputs are the most eager to buy,
   as at a buffet. The standard remedy is to bound the input and sell tiers. Input is validated
   against the schema before any money is locked (G6), so a seller can cap input size and offer
   a small and a large action at different prices.
2. *No pooling inside a call.* A seller's own costs can be priced at the mean. Sub-calls are paid
   from that call's budget alone, and one that exceeds it fails (D2). A composed action must be
   priced at or above the worst-case cost of its sub-calls, or it fails on expensive inputs.
3. *Open-ended work.* A request such as "research this" has no bounded cost. The trade precedent
   is a quote: a cheap action inspects the request and names a price, and the buyer accepts or
   declines.

The quality risk in the incentive row is the moral hazard of section 6, and reputation is the
answer to it.

## 8. Juice as a protocol over HTTP

**The test.** A protocol specifies only what two independent parties must agree on. By the
end-to-end argument (Saltzer, Reed and Clark, 1984), everything one party can guarantee alone
stays out. HTTP, SMTP and OAuth were each separated from their first implementations this way.

**The invariants under that test.**

| Invariant | Needs agreement on | Layer |
|---|---|---|
| Terms seen before paying | A signed offer and its hash | Protocol |
| Attribution of payer, caller and payee | Signed principals on each request | Protocol |
| Exactly-once settlement, verifiable offline | Idempotency key and signed receipt | Protocol |
| A debt is discharged | A binding of payment to receipt | Protocol, with the method pluggable |
| Ex-post trust | A signed evidence format | Protocol (format only) |
| Price bounds the whole call tree | Nothing beyond a fixed price | Host |
| Bounded exposure to strangers | Nothing beyond a refusal | Host policy |
| Local and remote symmetry | Nothing | Host |

A call tree is a chain of bilateral fixed-price contracts, as an Interledger payment is a chain
of bilateral transfers.

**The minimal extension.**

1. *Identity.* One key per kernel, with requests and responses signed under HTTP Message
   Signatures (RFC 9421).
2. *Offer.* An OpenAPI operation with a price, whose quote hash is its `ETag`. A pinned call
   sends `If-Match`, and changed terms return 412.
3. *Call.* `Idempotency-Key`, plus headers for the caller, the process owner and the process.
   HTTP actions already receive the first two. Deadline propagation in gRPC is the precedent for
   carrying a bound down a tree.
4. *Receipt.* A signed response header binding request hash, reply hash and charge. A signed 402
   is the refusal.
5. *Settlement and evidence.* One endpoint to settle a receipt under a named scheme, and one
   well-known resource serving signed evidence.

**What the rest is.**

| Part | Share | Status |
|---|---|---|
| Wire protocol (P1–P11) | 22,000 of 227,000 spec characters | About half is core: P1, P2, P4, P5, P7 |
| Lottery tickets (P10) | within the above | One settlement scheme; periodic netting is the alternative |
| Tasks (P8), transfers (P11), grant forwarding | within the above | Extensions; the core works without them |
| libp2p, the DHT, gossip cursors (P3, P9) | `fed/` and most of `kernel/federation.go`, about 4,000 lines | Replaceable by HTTP and plain `GET` |
| Accounts, recovery, CLI, store, WASM, natives, LLM endpoints, OpenAPI import, schema subset | About 85% of the code, by estimate | The host: legitimate, but a product, not the standard |

Most of what is not protocol is not bloat. It is a reference host, standing to the protocol as
nginx stands to HTTP.

**Costs of moving to HTTP.**

- A kernel behind NAT could buy but not sell, which libp2p currently avoids.
- Locating a kernel would depend on DNS and TLS. The key must remain the identity, or the "no
  registry" property is lost.
- P3 and D12 mandate libp2p, so the move conflicts with the letter of requirements.md.

**Consequence.** A five-part protocol, versioned and separate from the product spec, would allow
a second implementation, such as middleware for an existing web server. Email and Ethereum
decentralised through stable protocols with several implementations.

## 9. Juice on Ethereum

The result is the inverse of the HTTP one. HTTP reaches every service but lacks money, identity
and receipts. Ethereum has all three but cannot reach a web service, keep a secret, or price a
call below its own fee. Fees and block times below are approximate and from memory.

**What Ethereum already provides.**

| Juice invariant | Ethereum | Juice machinery made redundant |
|---|---|---|
| Conservation, exactly-once, immutability, crash safety (G1, G3, G4) | Atomic transactions under consensus | Idempotency locks, recovery, the local ledger |
| Offline verifiability (G7) | Transaction receipts with Merkle proofs | Signed receipts (P5) |
| Attribution (G2) | `tx.origin`, `msg.sender`, callee | The role law's bookkeeping |
| Terms seen before paying (U8) | Code is fixed at an address (`EXTCODEHASH`) | Manifests and quote hashes (P2, P6) |
| Bounded exposure to strangers | Payment is atomic with execution, so there is no debt | Credit limit, lottery tickets, reveals (P7, P10, D14) |
| Custody | Users hold their own keys | — |
| Same stdlib everywhere | Precompiles at fixed addresses | `localhost` |

**The minimal extension, on-chain.** Three contract standards: a priced-call interface (a fixed
price and a terms hash per function, as ERC-20 standardised an interface), a task escrow, and an
evidence registry (ERC-8004 proposes one for agents). A paymaster (ERC-4337) can absorb gas so
that the caller sees one all-in price; without it the fixed-price invariant fails.

**What Ethereum cannot provide.**

- *The Web.* Contracts must be deterministic, so they cannot make an HTTP request. HTTP actions,
  OAuth consent and model calls cannot be contracts. This is the oracle problem.
- *Secrecy (G5).* A contract cannot hold a credential, because all state is public.
- *Privacy of trade (U39).* Every call, argument and party is public. Hiding who rated would need
  zero-knowledge proofs.
- *Micropayments.* A call cannot cost less than its transaction fee, about a cent on a rollup
  and far more on the main chain. Orchid met this limit and adopted lottery tickets off-chain,
  the same answer as P10.
- *Latency.* A block takes seconds, where an HTTP call takes milliseconds.
- *Sovereignty.* One global ledger replaces federated kernels.

**Consequence.** Once the service runs off-chain, payment and execution are no longer atomic, and
the off-chain protocol is needed again: signed terms, receipts, idempotency, credit and ex-post
trust. That is the extension of section 8. x402 and L402 have the same layering: HTTP carries the
call and the chain settles it. Ethereum is correct as the settlement rail, the role Juice already
gives it, and cannot be the substrate.

Two things are worth taking from Ethereum without moving onto it: user-held keys, and a failed
sub-call returning an error to its caller (gap 2 below).

## 10. The implementation

Measured 2026-11-15 at v0.20.1. No benchmarks were run.

| Aspect | Measurement | Assessment |
|---|---|---|
| Tests | About 56,000 test lines against 37,000 production lines; 795 flow checks; a network simulation; mutation checks on changes | Strong |
| Layering | Acyclic imports; `kernel` imports no internal package | Strong |
| Robustness tests | No fuzz tests and no benchmarks; tests do not run in CI; no race detector in the Makefile | Weak for code that parses untrusted peer input |
| Cohesion | `Store` interface has 159 methods; `Kernel` has 308 methods; `call` is 352 lines; three files exceed 3,000 lines | Weak: the "few files" rule produces very large units |
| Throughput | SQLite with one connection, so every read and write is serialised | One process and one file per kernel, with no path for a large operator |
| People | 571 of 577 commits by one author in six months | A single point of failure |

## 11. Missing capabilities

### The test

Operating systems split capabilities into three layers, each with a rule for what belongs there.

| Layer | Unix | Rule | Juice |
|---|---|---|---|
| Kernel | system calls | only what cannot be built outside it (Liedtke's microkernel principle) | natives; D17: "the only actions reaching kernel state" |
| Library | glibc | wrappers and pure code, giving programs portability | the SDK |
| Services | daemons (`cron`, file servers, `syslogd`) | everything else | actions that providers sell |

Listing what Unix or Ethereum has and Juice lacks, without this test, gives wrong answers. Under
the principal framing the kernel row is narrower still: balances, dispatch, the price bound and
signed receipts (section 5).

### Services, not system calls

Storage, timers, events and access lists belong to the service layer.

- **Storage.** In microkernels (Mach, L4, Plan 9) the file system is a server outside the kernel,
  and on the Web state lives in services. A storage action needs one thing from the kernel: the
  caller's identity, to know whose data it holds. Every action is now told it.
- **Timers.** `cron` is a daemon, not a system call. A one-off deferred job is already a task
  with its money reserved, addressed to a scheduler that completes it later (U22, U24).
- **Events and access lists.** Message brokers are user-level programs. An action can refuse
  callers by principal, as web applications do.

**Actions are short-lived; a principal implements a daemon.** An action runs for one call, like
a CGI program started by `inetd`. A daemon is a program holding a principal's login, running as
long as its operator keeps it up. It has what a Unix daemon gets from its kernel: an identity
(its principal), resources (its balance), a way to be reached (its HTTP actions and the tasks
addressed to it), a way to act (the ordinary API), and knowledge of whom it serves (the caller
and process owner the kernel reports). Storage, scheduling and publish-subscribe are daemons sold
through actions. A subscription needs no standing authority to spend: the user prepays the
daemon's principal, or reserves money in a task the daemon completes later. U4 (funds move only
on their owner's authority) therefore stands.

The three kernel gaps below concern the short-lived side only. A daemon has none of them,
because it uses the full API.

### Real gaps

**Kernel**

1. **No way for a program to refer to its own kernel.** A program had to compile in the kernel's
   name (`sys@acme/time`) and broke on any other kernel. Unix has `localhost`, `gethostname` and
   `uname`; Ethereum places its built-in contracts at the same fixed addresses on every chain
   and gives each contract `address(this)`. D17 promises an "identical stdlib everywhere", but
   programs could not address it identically. *Closed by `localhost` (D15).*
2. **A failed sub-call kills a WASM action.** The host stops the module, while an HTTP action
   receives an error and can decide what to do. The kind of action must not change what a
   program can do. Every Unix system call returns an error code; Ethereum's `CALL` returns a
   success flag, and Solidity added `try/catch`. A program that cannot survive a refusal cannot
   fall back to a second provider, so it cannot use the market Juice exists to create.
3. **A program cannot inspect before it spends.** It cannot read its remaining budget, or the
   price and terms of an action it knows by name; `sys/lookup` only searches. Unix has
   `getrlimit`, `getrusage` and `stat`; Ethereum has `GAS` and `EXTCODEHASH`. U8 promises that
   nobody pays under terms they did not see; a composing program is a buyer too.

**Library**

4. **The SDK serves one language in one kind of action.** glibc's role is that every program
   reaches the kernel through one tested library. The SDK covers Go compiled to WASM; HTTP
   actions, the Web's majority, hand-write reading the headers, the callback, and error decoding.

**Services**

5. **No standard interfaces for common services.** POSIX standardised the file interface, not the
   file system; Ethereum did the same with ERC-20. Without an agreed contract for a storage or a
   scheduler action, a program written against one provider cannot switch to another.

### Deliberately absent

Threads, shared memory and raw sockets are missing by design. Ethereum's single-threaded,
sandboxed execution is the precedent, and the price bound depends on the kernel mediating every
effect. Dynamic linking is covered by composition.

## 12. Open criticisms and their status

| Criticism | Status | Remedy identified |
|---|---|---|
| Fixed prices fit variable costs badly | Withdrawn (section 7) | Tiers, input bounds and quotes cover the remaining costs |
| The recursive budget is a hosting guarantee | A fact, no longer a criticism | — |
| Reputation can be discarded | Accepted as intended | Make record length and recency easy to read; let standing buy something |
| Fraud by an established seller | Bounded, not solved | Buyers tie the price they accept to the seller's record |
| The kernel is not minimal | Rule found, code does not follow it | Kernel keeps balances, dispatch, the price bound, receipts |
| Custody | Open | A key held by the principal |
| A principal cannot move kernels | Open | The same |
| Network, not distributed | Open | Largely the same |
| One implementation; extensions mixed with the core | Open | A versioned five-part core, with tickets, tasks, transfers and gossip as extensions |
| libp2p transport | Open | HTTP, at the cost of selling from behind NAT; conflicts with P3 and D12 |
| Structure | Open | Split `Store` and `Kernel` by responsibility; add fuzz tests and benchmarks |
| The symmetry claim | Open | Reword to what is guaranteed |
| A WASM action dies on a failed sub-call | Open (gap 2) | Return the error to the program |
| A program cannot inspect before it spends | Open (gap 3) | — |

## 13. Not verified

These points were not checked, and no claim is made about them.

- How a daemon learns that a task has arrived: whether it must poll the task list, or can block
  until notified, as a Unix daemon blocks in `accept` or `poll`.
- Whether a sub-call can pin a quote hash.
- Whether discovery shows a buyer how much history stands behind a principal.
- Whether a running call holds the database connection.
- The share of code that is host and not protocol (85%) is an estimate, not a measurement.
