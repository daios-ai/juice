# Availability in Juice: assessment and recommendations

Date: 2026-07-03. Covers the v0.5 codebase (tag `v0.5.0`, commit `f907d99`).

## 1. Summary

Juice knows whether an action is *registered* (`active`, `public`) but not whether it is *alive*. Behind every action sits something that can be down: an HTTP upstream, an Ollama instance, a chain of other actions, or a peer kernel on someone's home machine that is switched off. Nothing in the system checks this before a call. Callers discover unavailability by paying for the attempt — with waiting time, and in the federation case with money locked for up to a day.

The design handles the *money* side of unavailability well: failures refund, remote calls settle only on signed receipts, and a price caps the worst-case loss. What is missing is the *information* side: no signal, before or during a call, that says "this will probably not work right now."

The investigation also found two things that look like defects rather than design choices:

1. **The retry loop for pending remote calls does not exist.** The code comment says pending remote calls are retried "periodically by the server ticker," but no such ticker runs. A call to an offline peer is only retried when the server restarts or when a related step completes. The 24-hour give-up bound has the same problem — it is only checked at those moments. On a server that never restarts, a pending call can stay pending forever.
2. **Calling a remote action that its owner has disabled locks your money for 24 hours.** The remote kernel answers with a plain error and no receipt. The caller cannot tell this apart from a network timeout, so it keeps the funds locked and retries until the 24-hour bound gives up. Compare: a *denied peer* gets an instant signed rejection and settles immediately. A disabled action should get the same treatment.

Sections 2–5 explain the current model, what goes wrong, what already helps, and how other peer-to-peer systems deal with the same problem. Section 6 gives ranked recommendations.

## 2. How Juice models availability today

An action is callable when it is `active`, `public` (or yours), and its owner is not suspended (`CanCall`, §4). All of these are *declared* states — flags in the database. None of them say anything about whether the thing behind the action will answer.

What sits behind an action varies by kind:

| Kind | What must be alive for a call to succeed |
| --- | --- |
| `http` | The upstream API at the source URL |
| `native` (LLM family) | The local Ollama instance |
| `wasm` | Every action it calls, recursively — plus whatever *those* depend on |
| `remote_proxy` | The peer kernel (possibly a home machine behind NAT), plus whatever the action depends on *there* |

Two properties follow:

- **Availability composes as an AND.** A WASM action that calls three other actions is only as available as all three together. Nobody — not the caller, not the ranker, not even the action's own author after publishing — can see this dependency tree in advance. It only exists in the script's code.
- **The federation layer makes "down" a normal state, not an error.** The spec is explicit (§13): a network of intermittently-online home kernels is the expected steady state. A peer being offline for hours or days is by design not a failure — the call waits.

There is no health check, no probe, no freshness rule anywhere in the call path. The only availability check in the entire system is `admin inspect <key>`, an on-demand diagnostic an operator runs by hand (`fed/reachability.go:22`, `cmd/juice/control.go:206`).

## 3. What can go wrong

Each scenario below lists what triggers it, what the caller experiences, what happens to the money, and what happens to reputation (stats).

### 3.1 Local HTTP action, upstream dead

**Trigger:** the API behind a `kind=http` action is unreachable or returns 5xx.

**Experience:** the call fails with `ErrExecutionFailed` (`cmd/juice/http_exec.go:248,444`). The caller waits out the connection attempt first.

**Money:** handled correctly. The failure commits a transaction and receipt, and the caller's remaining allocation is refunded (`kernel/call.go:311,637`). For a plain HTTP action with no subcalls, the refund is total — the caller pays nothing.

**Reputation:** the failure counts — `uses+1, failures+1` (`kernel/kernel.go:1711-1719`).

**The problem:** the action stays `active`. Every future caller repeats the same discovery: wait, fail, refund. Nothing marks the action as "currently dead," and as §3.6 shows, ranking barely demotes it. The first caller pays the discovery cost — and so does the second, and the hundredth.

### 3.2 Native actions, LLM not running

**Trigger:** Ollama is off or unconfigured.

**Experience:** `@sys/llm/*` calls fail with `ErrInvalidState` at call time. Nothing checks at startup or in the background.

**The notable case:** `@sys/lookup` needs the embedder to embed the query. If the embedder is down, *discovery itself* is down (`kernel/kernel.go:1511-1513`). The system's tool for finding actions has its own hidden availability dependency, and no separate signal when it breaks.

### 3.3 Composed (WASM) actions

**Trigger:** any action in the dependency subtree is unavailable.

**Experience:** the script sees the subcall fail as an error value (`{"__juice_error__": ...}`) and *can* recover — retry, use a fallback action, or return a degraded result (`kernel/call.go:507-522`). This is a genuinely good property: composition failure is catchable, and the parent decides whether to propagate (`call.go:501-503`).

**Money:** if the parent propagates the failure, the caller is refunded the remainder — but subcalls that already succeeded stay paid (§6). So a caller of a failed composition pays a partial cost for a result they never got. This is by design (the sub-providers did their work), but it means composed actions leak money on partial failure in a way simple actions do not.

**Reputation:** the sub-action that failed gets the failure stat; the parent also records a failure if it propagates. Attribution is reasonable.

**The problem:** the dependency tree is invisible. A composed action can silently become unavailable because a sub-sub-action's upstream died. No one can inspect "what does this action depend on" before calling — even though the kernel actually has the data to answer it historically (every past call's subcall structure is recorded in traces and transactions).

### 3.4 Remote peer offline

**Trigger:** you call `@peer/action` and the peer's kernel is off.

**Experience:** the call does not fail — it *parks*. The full proxy price stays locked, the process stays open, and the call waits for a signed receipt. This is deliberate (§13): settling on a timeout could contradict what the remote actually did, so money truth requires waiting for the receipt.

**Money:** locked until a receipt arrives, the process owner force-ends the process, or the 24-hour `RemotePendingMaxAge` bound expires the call as a failure with full refund (`kernel/federation.go:387-397`).

**Reputation:** *nothing* while pending. Stats are only written at settlement. A peer that is offline 95% of the time accumulates no failures from all the calls parked against it — its actions keep clean stats and keep ranking (see 3.6). Only calls that eventually settle as failures count.

**The defect (found during this investigation):** the retry mechanism is supposed to re-drive pending calls "periodically by the server ticker" (comment at `kernel/federation.go:313-314`), but **no such ticker exists**. The only ticker in `serve.go` is the rate-limiter cleanup (`cmd/juice/serve.go:216`). `RetryPendingRemoteDispatches` runs at startup (`cmd/juice/bootstrap.go:88`) and on related step completions — nothing else. Consequences on a long-running server:

- A peer coming back online does not cause parked calls to settle. They wait for a restart.
- The 24-hour give-up bound is only *evaluated* during a retry, so it does not fire either. "Locked for up to 24h" is actually "locked until the next restart" in the worst case.

### 3.5 Remote action disabled or changed

**Trigger:** peer B disables an action that kernel A imported earlier.

**Staleness:** A only learns about it when its operator manually re-friends or re-reconciles (`cmd/juice/control.go:329-339`). There is no periodic manifest re-sync. Until then, A's catalog advertises an action that no longer exists in callable form — to its users and to its lookup ranking.

**The worse part — what happens on a call:** B's inbound handler rejects the call with `ErrUnauthorized` and **no receipt** (`cmd/juice/service.go:446-447`). On A's side, no receipt means "pending — retry" (`kernel/federation.go:200-201`). So the caller's funds sit locked, retrying against an action that will never answer, until the 24-hour bound (which, per 3.4, may effectively be "until restart").

Compare the handling of a **denied peer**: B returns a *signed rejection receipt*, and A settles the failure instantly with a full refund (`service.go:409-425`). The spec's own rationale for that receipt — "so the caller always has something to settle on" (§13) — applies word-for-word to the disabled-action case, but the code doesn't do it. This asymmetry looks like an oversight, not a decision.

### 3.6 Ranking is availability-blind

`@sys/lookup` scores actions as `cosine similarity × quality`, where quality is:

- `0.5 + 0.5 × (successes/uses)` when local stats exist (`kernel/kernel.go:1543-1551`),
- a gossip-based prior in `[0.5, 0.75]` for imported actions never used locally,
- a flat `0.5` otherwise.

What this means in practice:

- **A 100%-failing action scores 0.5 — the same as a brand-new untested one.** It is demoted at most 2× and never excluded. If its description matches the query well, it can still outrank a working alternative.
- **No recency.** An action that worked fine for a year and died yesterday keeps its historical success ratio. `last_used_at` is stored but never read by ranking; there is no decay.
- **Latency is tracked but ignored.** `latency_estimate` never enters the score.
- **No reachability input.** For proxy actions, the transport knows whether the peer is currently connected — ranking never asks.
- **Pending calls are invisible** (see 3.4), so the most common federation failure mode — peer not there — never damages rank at all.

### 3.7 Steps addressed to a peer that never returns

A step whose `required_caller` is a peer user parks its price and waits. There is no expiry and no ageing signal (`kernel/steps.go:154-163`). It is released only by the peer completing it, the owner force-ending the process, or an unfriend cascade (`kernel/federation.go:502-512`). Deliberate — but an operator has no view of "steps that have been waiting on an unreachable peer for a month" without joining the data by hand.

### 3.8 The pattern across all of these

Every mechanism above is **reactive**: refunds, failure stats, receipts, recovery. They activate *after* an attempt fails. Nothing is **predictive or advisory**: no signal exists that lowers the chance of attempting a doomed call in the first place. The single most repeated cost in the system is a caller attempting something the infrastructure already had reason to believe would fail.

## 4. What the system already does well

Credit where due — the money side of this is genuinely solid:

- **Price caps loss.** The worst a caller can lose on any call is the advertised price, known in advance (§6).
- **Failures refund.** A failed call returns the unspent allocation automatically, with a signed receipt (§6).
- **No lying about remote outcomes.** Settling only on signed receipts means a network failure can never silently disagree with what the remote kernel actually did and charged. This is the right call, and other systems (see §5) validate the instinct.
- **Idempotent retry.** Retried remote calls carry the same idempotency key, and the remote replays its stored result — so a retry can never double-execute or double-charge (§13).
- **Failure stats exist and are honest.** A settled failure counts against the action regardless of what was charged, so a provider gains nothing by failing-with-charge (§13, `kernel/federation.go:285`).
- **Awaiting-receipt is visible.** Process listings surface calls parked on unreachable peers (§13), so the funds are at least not *silently* stuck.
- **Compositions can self-heal.** WASM scripts see subcall failures as catchable errors and can implement fallbacks — the building block for resilient composed actions exists today.

The gaps are: everything reacts, nothing predicts (3.8); pending calls are stat-invisible (3.4); and the retry/rejection machinery has the two defects flagged in the summary.

## 5. How other peer-to-peer systems handle this

The problem — "you know a provider is registered, not that it is alive" — is old, and there are four well-tested families of answers.

### 5.1 Liveness by expiry (BitTorrent, IPFS)

In DHT-based content networks, an announcement is a lease, not a fact. A BitTorrent peer must re-announce itself roughly every 30 minutes; an IPFS node must re-publish its provider records about every 24 hours. If you are not alive to re-announce, you fall out of the index automatically — nobody has to *detect* your death; your registration simply expires.

**What maps to Juice:** imported remote actions currently live forever until an operator manually reconciles (3.5). A freshness rule — "a proxy action whose manifest hasn't been re-confirmed in N days is marked stale in listings and demoted in ranking" — is the same idea. Note the fit: expiry is *information* (staleness), not *authority* (nobody deactivates anything by force), which matches Juice's gossip discipline exactly.

### 5.2 Gossip-based failure detection (SWIM, Serf, Consul)

Cluster systems in the SWIM family have members ping each other on a random schedule; a suspected-dead member is checked indirectly through other members before being declared down, and the verdict spreads by piggybacking on normal gossip traffic. The point is to make failure detection cheap, distributed, and resistant to one node's bad network view.

**What maps to Juice:** the full protocol is overkill for a trust model where peers are few and hand-picked (friends). But the light version — "when I gossip about a transacted friend, I include when I last successfully reached them" — costs almost nothing and gives every kernel secondhand liveness hints. Juice already gossips earned stats about friends; a `last_seen` field is the same category of information. Caveat that SWIM doesn't have: gossiped liveness can be stale or dishonest, so it must stay a ranking input, never a callability input. Juice's "information, never authority" rule already says this.

### 5.3 Time-bounded pending money (Lightning Network)

Lightning is the closest cousin to Juice's hardest problem: money committed against a peer who may be offline. Its two relevant answers:

- **Payments cannot hang forever.** Every in-flight payment (HTLC) carries an absolute expiry; if it doesn't complete in time, the funds unlock, enforced by the blockchain if necessary. Pending is a *bounded* state.
- **Providers announce their own downtime.** A node can broadcast a channel update with a `disabled` flag — "don't route through me right now." Peers keep the channel in their maps but stop selecting it. Temporary unavailability is a first-class, self-declared, advisory signal.

**What maps to Juice:** Juice already has the first idea on paper — `RemotePendingMaxAge`, 24h — but as 3.4 showed, nothing reliably enforces it. Lightning's lesson is that the bound is only worth anything if it fires unconditionally. The second idea (self-declared "away") maps directly: a kernel that knows it's going offline could say so in gossip, and importers could demote — not deactivate — its actions.

**Where Juice rightly differs:** Lightning can settle a timeout *authoritatively* because the blockchain arbitrates. Juice has no arbiter, so its choice to never settle on a timeout — only on receipts or an explicit local give-up-and-refund — is correct for its model. The recommendation is not "settle on timeout"; it is "make the local give-up actually happen on schedule."

### 5.4 Health checks and circuit breakers (the service world)

Centralized service infrastructure solved this with three tools: registrations that expire unless renewed (Consul/etcd leases — same idea as 5.1), health endpoints polled by the infrastructure, and **circuit breakers**: after N consecutive failures, stop calling the target for a cooldown period, then let one trial call through ("half-open") to test recovery.

**What maps to Juice:** active polling of upstreams doesn't transfer well — in a P2P system, probes cost real resources and the prober must be trusted. But the circuit breaker transfers almost perfectly, because it needs no probing at all: it is built entirely from *observed local outcomes*, which Juice already records per action. "This action's last 5 calls all failed within the past hour → rank it near zero and label it in listings" is a circuit breaker in ranking clothes. Crucially it can stay advisory: the action remains callable (maybe the caller knows something), it just stops being *recommended*.

### 5.5 The shared principle

All four families converge on the same shape, which happens to match Juice's stated philosophy: **liveness information should decay, spread cheaply, and advise — never decide.** Nothing in prior art requires Juice to add authority anywhere; every useful mechanism fits inside "information only" if implemented as ranking inputs, listing labels, and expiring freshness — plus one hard rule (bounded pending money) that Juice already has and just needs to enforce.

## 6. Recommendations, ranked

Ordered by urgency. 1–2 are correctness fixes; 3–5 are cheap information wins; 6–8 are design work.

### 6.1 Fix the pending-call retry driver *(defect)*

Add the ticker the docstring already promises: run `RetryPendingRemoteDispatches` on a modest interval (e.g. every 1–5 minutes) in `serve`. This makes parked calls settle when a peer returns *without* a restart, and — equally important — makes the 24h `RemotePendingMaxAge` refund actually fire on time. Small change, no spec impact (the spec and the code comments already claim this behavior; the code just doesn't do it). Test: §15's "timeout does not settle; retry recovers receipt" flow extended with "…without a restart."

### 6.2 Signed rejection for calls to disabled/missing actions *(defect-adjacent spec gap)*

When an inbound federation call targets an action that is inactive, non-public, or gone, return a **signed rejection receipt** (charge 0) — exactly what insufficient balance and denied peers already produce. The spec's own rationale ("the caller always has something to settle on," §13) covers this case; the implementation just skips it. This turns "money pinned for 24 hours" into "instant clean failure with refund." One code path (`service.go:446` area), plus a spec sentence making rejections cover *all* non-executable inbound calls, plus tests.

### 6.3 Count pending-aged calls in visibility, and surface peer reachability

Two cheap information wins, no new machinery:

- `process list` already marks awaiting-receipt; add *age* ("awaiting receipt for 3d") and do the same for `step list` on steps whose required caller is a peer (3.7).
- For proxy actions, surface transport connectedness — which the fed layer already knows per peer — as a label in `action list` / `action show` ("peer currently unreachable"). Advisory text only; callability unchanged.

### 6.4 Availability-aware ranking *(spec-sanctioned experimentation)*

Ranking is explicitly the replaceable, experimentable layer (§9, §16) — this needs no kernel semantics change:

- **Recency decay:** weight recent outcomes more than old ones, so an action that died yesterday falls fast and one that recovered rises fast.
- **Widen the failure penalty:** a persistently-failing action should approach 0, not floor at 0.5 — today it ties with untested actions (3.6).
- **Circuit-breaker demotion:** N recent consecutive failures → near-zero rank for a cooldown, then let it recover. Built purely from data the kernel already stores.
- **Reachability input for proxies:** currently-unreachable peer → rank its actions down for the moment. The transport already knows.

### 6.5 Manifest freshness — expire staleness, don't force sync

Adopt the lease idea (5.1) at the information level: record when each proxy action's manifest was last confirmed, mark actions stale past a threshold in listings and ranking, and optionally re-fetch a friend's manifests on a slow background timer or after a first call failure. Deactivation stays where it is today (reconcile detecting a real contract change or removal); staleness itself is only a label and a ranking input.

### 6.6 Provider-declared "away" *(Lightning's disabled flag)*

Let a kernel mark itself (or individual exposed actions) as temporarily away in its gossip/manifest data — set by the operator before planned downtime. Importers demote and label; nothing deactivates. Purely informational, fits the gossip discipline, costs one field. Worth doing only after 6.3–6.5 exist to consume the signal.

### 6.7 Optional fail-fast on unresolvable peers

Today a call to an unreachable peer always parks (by design — the call should execute when the peer returns). Some callers would rather fail immediately. An opt-in flag on `run` ("don't park; fail if the peer can't be reached now") preserves the default semantics while giving interactive callers an escape hatch. This is a precondition check, not a settlement change — it refuses to *start*, never lies about what a remote *did*. Needs a spec decision because it adds caller-visible semantics; do it last, if demand appears.

### 6.8 Dependency visibility for composed actions *(future research)*

The kernel already records every call tree in traces/transactions. From history, it could answer "what does `@x/pipeline` actually call, and what is the current health of those dependencies?" — an observed dependency graph, no static analysis needed. This is the long-term answer to 3.3 and a natural research feature for the platform. Not urgent; noted so the idea isn't lost.

## 7. Closing assessment

The economics of unavailability in Juice are sound: nobody can lose more than an advertised price, failures refund, and remote settlement can't be spoofed by a flaky network. That part needs no rework.

What needs attention, in order:

1. **Two defects** make the federation experience worse than the design intends: pending calls don't actually retry on a schedule (6.1), and disabled remote actions pin caller funds instead of rejecting cleanly (6.2). Both are small fixes to behavior the spec already describes or clearly implies.
2. **The system is information-poor about liveness**, and every piece of prior art says the fix is advisory signals that expire and decay — not authority, not probing infrastructure. Recommendations 6.3–6.6 add exactly that, inside the existing "gossip informs, never decides" rule.
3. **Ranking is the cheapest big win.** It is the layer the spec explicitly reserves for experimentation, it already has most of the data it needs, and today it happily recommends dead actions. Fixing 3.6 improves the everyday experience of every user who discovers actions through lookup.

None of this requires changing what a price means, how settlement works, or the receipt rule. The model's core bet — money truth over liveness guesses — survives contact with prior art. It just needs the information layer the bet was always meant to sit on top of.
