# Juice as an operating system

An evaluation of Juice against operating-system and blockchain precedents, and an analysis of
the basic capabilities it lacks. Written 2026-11-14, against requirements.md and the code at
v0.20.0. It is context, not contract: requirements.md governs.

## 1. Evaluation

**Verdict.** Juice is a sound and largely original synthesis. Almost every individual mechanism
has a precedent; the novelty lies in a few specific mechanisms and in the combination. Its
weakest points are economic and social (custody, variance, reputation gaming), not
architectural.

### By axis

| Axis | What Juice does | Closest precedent | Assessment |
|---|---|---|---|
| Process model | One dispatch primitive (`Call`); a process is one funded run; natives are the only way into kernel state | Unix `fork`/`exec` and syscalls; Plan 9's single uniform interface | Strong: the uniformity is real, and one code path serves local, composed and remote calls |
| Identity | A principal is (kernel key, user id); names are translated at the edge | Kerberos `user@REALM`, Windows SIDs, NFSv4 | Sound and consistent |
| Authority | Every call records payer, caller and target; value moves only from the immediate caller | Ethereum `msg.sender` versus `tx.origin`; capability systems (Hardy's confused deputy) | Strong: the confused-deputy rule is built into the role law, where Ethereum leaves it to each contract author |
| Resource bound | A fixed advertised price bounds the whole call subtree; unused budget is the provider's margin | Ethereum gas limit; cloud per-request pricing | Novel (below) |
| Settlement | Per-call, between kernels, with a probabilistic ticket for amounts too small to pay | Rivest's lottery micropayments (1997); Orchid; Lightning | Sound: the expected payment equals the debt, and neither side can pick the outcome |
| Decentralization | Federation of sovereign kernels; no global ledger, no registry, no consensus | Email, ActivityPub; petnames (Stiegler) | Honest trade-off: cheap and fast, but each user trusts their own operator |
| Trust | Terms are verified before the call; conduct is judged afterwards by public, signed, privacy-preserving trade evidence | eBay feedback; EigenTrust | Coherent, but the weakest axis (below) |
| The Web | OpenAPI import, HTTP actions, OAuth consent delegated per action and honoured across kernels | RapidAPI; OAuth; agent-payment proposals (x402) | Practical: existing services join without being rewritten |

### Novelty

1. **Price as a recursive budget.** Ethereum meters computation and makes the caller guess a
   limit. Juice lets the provider state one all-in price, and the kernel enforces it over
   everything the action calls. The caller's risk is known before spending; the provider bears
   the cost variance and keeps the margin. Agoric systems (Miller and Drexler, 1988) and Spawn
   (Waldspurger, 1992) priced computation, but none made the price a transitive bound enforced
   by the kernel.
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
  verify solvency, as a blockchain would.
- **Reputation gaming.** Evidence is backed by real trades, so faking it costs fees, but a
  determined provider can still buy its own ratings. Conduct is never verified, by design.
- **Variance.** Lottery settlement is fair in expectation only; a small kernel can see large
  swings.
- **Liveness coupling.** Every served call asks the kernel where the run began who owns it. This
  buys symmetry at the cost of availability.
- **Fixed prices.** Providers with variable costs must price for the worst case, so callers
  overpay on average.

The closest intellectual ancestor is the agoric open-systems programme. Juice is, to our
knowledge, the first to implement that programme for web services with real settlement.

## 2. Missing capabilities

### The test

Operating systems split capabilities into three layers, each with a rule for what belongs there.

| Layer | Unix | Rule | Juice |
|---|---|---|---|
| Kernel | system calls | only what cannot be built outside it (Liedtke's microkernel principle) | natives; D17: "the only actions reaching kernel state" |
| Library | glibc | wrappers and pure code, giving programs portability | the SDK |
| Services | daemons (`cron`, file servers, `syslogd`) | everything else | actions that providers sell |

Listing what Unix or Ethereum has and Juice lacks, without this test, gives wrong answers.

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

### Real gaps

**Kernel**

1. **No way for a program to refer to its own kernel.** A program had to compile in the kernel's
   name (`sys@acme/time`) and broke on any other kernel. Unix has `localhost`, `gethostname` and
   `uname`; Ethereum places its built-in contracts at the same fixed addresses on every chain.
   *Closed by `localhost` (D15).*
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

### Open question

How a daemon learns that a task has arrived: whether it must poll the task list, or can block
until notified, as a Unix daemon blocks in `accept` or `poll`.
