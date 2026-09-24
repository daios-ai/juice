# The network simulation

`netsim` builds a small economy of independent juice kernels, makes them trade, attacks them, and
writes down what happened. It exists to answer "how does this network behave", which the unit tests
and the flow suite do not: those check that a change did not break something, in minutes, and fail a
commit. This runs for longer, fails no commit, and produces artifacts to read.

```
make netsim                  # the manual rail, no external dependencies
make netsim RAIL=anvil       # a local chain (needs Foundry)
make netsim RAIL=sepolia     # the live testnet (needs an RPC and a funded key)
make netsim ROUNDS=4         # fewer trading rounds; the report says which count it ran
```

It drives the shipped binary through its own command line and HTTP API, exactly as an operator
would. It links no juice package, so nothing here can pass by reaching inside the kernel.

## Three meanings of "transaction"

The reports would be misread without this distinction.

| term | what it is | costs gas? |
|---|---|---|
| operation | one `juice` command the suite issued | no |
| top-level call | one action a user asked for | no |
| nested call | an action bought by another action while it ran | no |
| blockchain transaction | a mint, a deposit, a ticket payment, an ETH transfer | **yes** |

Hundreds of cross-kernel calls between the same pair of kernels cost a handful of payments and are paid
once. That is why the workload is the same on every rail: rounds are almost free, and only the
*shape* of the trade graph — how many pairs end up owing each other — decides what a chain run
costs.

## The participants

Five kernels, deliberately unlike each other, so a trade crossing any pair is priced differently and
a mistake in the pricing rule cannot cancel itself out.

| kernel | name | operator fee | remote premium | import fee |
|---|---|---|---|---|
| k1 | hub | 20% | 5% | 5% |
| k2 | shop | 10% | 7% | 3% |
| k3 | maker | 25% | 0 | 10% |
| k4 | buyer | 0 | 5% | 0 |
| k5 | late | 15% | 10% | 5% |

Eight users live on them. Money enters each kernel at **one** point and spreads by ordinary
fee-free transfers between local users. That is what an economy does, and on a chain it is forced:
a payout address belongs to one account, so two users on one kernel cannot both pay in from the
same wallet.

Three more kernels appear later — two attackers and a name squatter. They are not part of the
economy, which is why reports show eight kernels for a five-kernel story.

## What a run does, in order

1. **The kernels come up.** Four start and find each other over the real peer-to-peer transport.
   Each operator gives the others a local name; those names are what purchases like
   `cara@shop/quote` resolve through.
2. **Money enters.** One deposit per kernel, then internal transfers. The same payment reference is
   submitted three times and must be credited at most once.
3. **The catalogue.** Free and paid services; public, kernel-local and private ones; a service whose
   upstream fails intermittently; one that answers successfully with the wrong shape; one needing
   delegated authorization; and two composites — real WebAssembly modules, assembled by the suite,
   that buy other actions while they run. One crosses a kernel boundary. The other buys a service
   that succeeds and then one that fails, which is the only way to see a partial refund.
4. **Trading.** Each round makes 8 local and 10 cross-kernel purchases. Twelve rounds by default,
   the same everywhere. Then a burst: 8 concurrent callers making 10 cross-kernel calls each, which
   is the only figure here that says anything about capacity rather than latency.
5. **Refusals.** A private action, a wrong input type, an unknown action, a caller who cannot pay —
   each refused, and a refused call must move no money.
6. **Composition and partial refunds.** A composite charges its advertised price whatever it spent
   inside. A composite that fails after one purchase settled refunds the price less exactly what
   that purchase consumed.
7. **Steps.** An action parks, waiting for a person. The wrong party cannot complete it; the right
   one can; completing it twice does not pay twice.
8. **Value.** A transfer between users on one kernel. Overdrawing is refused, an unknown recipient
   is refused, and a transfer may not name someone on another kernel.
9. **Delegated authorization.** Refused before consent with nothing locked, works after connecting a
   credential, refused again after revoking it.
10. **Ratings and evidence.** Purchases are rated; the ratings a peer serves and the catalogue served
    to a stranger are fetched raw and checked for identifying fields; a genuine receipt is verified
    and must name the checks it made.
11. **A fifth kernel joins** an economy already running, is discovered, sells, and buys.
12. **A provider is killed** outright and restarted. See the honest scope below.
13. **Attacks.** Five, described below.
14. **Everything is settled.** Each obligation a buyer says it has paid is confirmed against the
    money that arrived, repeatedly until nothing anywhere is owed and no payment is still moving —
    reading the books mid-movement would report the instant, not the economy.

## What the report judges

Two kinds of check. The **consistency** checks ask whether the kernel agrees with itself. The
**independent** checks predict from what the story did and compare against what the kernels report;
they are the ones that can catch the kernel being wrong rather than merely inconsistent.

| check | kind | what it means |
|---|---|---|
| `gross − refund == fee + net + Σ children` on every transaction | consistency | what a caller paid equals what the call kept plus what it spent |
| money entering equals money held | independent | Σ deposits == Σ (available + locked) over every account on every kernel. A cross-kernel call sums to zero across the two kernels — the charge returns to the caller, whose own stake carries the draw — and a ticket payment moves tokens from one vault to the other |
| every call charged its advertised terms | independent | local: the price. Cross-kernel: `q` from the published rates. Local failure keeps nothing. **Remote failure may legitimately charge** — the peer may have done paid work before failing — so it must equal the receipt's own draw plus premium, with no import fee |
| every obligation was settled by a payment that was not short | independent | a draw pays what is owed or the whole face value, never less |
| no obligation outstanding, in either direction | consistency | an obligation is one row on the serving side, so both directions are read; and no peer row holds money at all |
| no funds left parked | consistency | read from `process list` (`awaiting_receipt`), not transaction rows: a call still waiting has no settled row |
| the evidence a peer serves carries no identifying field | independent | checked on the raw response, and a missing projection fails the run |
| latency, throughput, recovery | measurement | not a verdict |

Every snapshot fails closed. A read that errors, will not paginate or will not decode becomes
"insufficient evidence" and fails the run, because a checker given empty rows reports that
everything balances.

A failure the suite is confident is a defect in the kernel is reported under **Failures in the
system under test**, apart from failures of the suite's own checks. The two need opposite responses:
a fault in the harness is fixed and forgotten, while one of these must survive being noticed.

## The attacks

Five, and they are attacks on the economy and its authorization rules. They are not a complete
attack on the network protocol.

| the attacker wants | what stops it |
|---|---|
| more unpaid work than one identity could draw, by minting identities | one credit limit for the whole kernel, checked as an absolute ceiling rather than as what the attack added: a winning draw pays the whole face value, so cash received can exceed what was owed and carry the counter below zero, against which any increase overstates. The attackers are funded and the victim is put on a low limit, and somebody must be refused for want of credit, or it is never reached and the test proves nothing |
| paid work with no balance to pay for it | refusal before anything is locked |
| one payment credited more than once | a payment reference is honoured once |
| an action that was never exported | access is not transitive; a kernel does not relay on request; value may not cross a kernel |
| a name already in use | a petname is the local operator's own label and is never taken from the network |

Lower-level attacks live where they can be made properly: lost, duplicated and refused messages and
store failures in `cmd/juice/fedsim_test.go`; oversized frames and stream floods in
`fed/transport_test.go`; concurrent admission at the credit limit in `store/sqlite_test.go`; forged
and tampered receipts in `kernel/federation_test.go`, which can re-sign with a peer's key.

## Coverage

| requirement | where |
|---|---|
| P1 signatures, domains, digest | `kernel` signature fixtures, conformance |
| P2 quote hash, terms changed | flow `terms_changed_refused` |
| P3 request freshness | `fedsim_test.go` |
| P4 idempotency | `fedsim_test.go` (duplicate delivery); netsim act 2 (payment references) |
| P5 receipt shape, tampering | `kernel/federation_test.go` |
| P6 manifest, proxy exclusion | conformance; netsim act 10 |
| P7 cross-kernel pricing | **netsim price fidelity** (independent) |
| P8 steps across kernels | netsim act 7; flow `fed_step_complete` |
| P9 gossip, evidence | netsim act 10 |
| P10 ticket settlement | **netsim settlement fidelity**; anvil and Sepolia rails |
| U13 partial refunds | **netsim refund law**, act 6 |
| U29–U31 provisioning, exporting, both operators paid | netsim acts 2, 3, 4 |
| U32 credit limit, Sybil | netsim attack 1; `store/sqlite_test.go` (concurrent) |
| U33 settle over a rail | anvil, Sepolia |
| U34 NAT, relay | `flows_network.sh` — **manual gate, needs a second host** |
| U35 intermittent connectivity | `fedsim_test.go`; flow `fed_provider_crash_recovery`; netsim act 12 (offline only) |
| U36 offline verification | netsim act 10; `kernel` tests |
| U37–U40 suspend, petnames, privacy, discovery | netsim acts 10, 11, attack 5 |
| U41 remote steps | flow `fed_step_complete` |
| U44 observability | netsim act 12 (parked funds and their age) |
| U47 non-transitive access | netsim attack 4 |
| G1 conservation | **netsim conservation** (independent) |
| G4 crash safety | `fedsim_test.go`; flow `fed_provider_crash_recovery` |
| G6 no unfunded work | netsim act 5, attack 2 |
| D12 transport limits | `fed/transport_test.go` |
| D14 credit engine | `store/sqlite_test.go` |
| D23 rail | anvil refill gate; Sepolia |

## What it does not claim

- **Act 12 is not a mid-call crash.** The provider is stopped *before* the call, so the request was
  never dispatched. The hard case — request received, answer lost — needs the provider killed while
  it is executing, and lives in `flows_federation.sh` and `fedsim_test.go`. A stranded-funds result
  from act 12 alone should not be treated as proof.
- The suite does not judge whether an operator would find a refusal message intelligible.
- It does not reconstruct per-kernel solvency or custody; the kernel reports those itself.
- Ratings' free-text notes are not checked for what a person might have written in them.

## The rails

One economy, run unchanged on all three. A rail supplies only how money enters, how a payment is
made and becomes final, and what the run cost. A test fails if `story.go` so much as names a rail.

- **play** — no chain. A person's payment is the operator's own record of it; a peer's is the
  buyer's own signed reveal, so cross-kernel debts close with nobody acting. Play money.
- **anvil** — a local chain, real contracts and signatures, blocks made on demand, currency free.
- **sepolia** — Arbitrum Sepolia, and nowhere else: the rail refuses any chain but 421614. The
  shipped `arbitrum-sepolia` world settles at `latest`, Arbitrum's own confirmation; a run that must
  wait for Ethereum finality instead sets `finality` to `finalized` in
  `rail/worlds/arbitrum-sepolia.json`.

Before spending anything the Sepolia rail prices the complete story — every mint, deposit, kernel
gas provision and ticket payment, from the declared shape — and refuses if it exceeds the cap
(`JUICE_SEPOLIA_BUDGET`, 0.005 ETH by default). It never spends from the funding account: it moves
exactly the cap into a wallet made for the run and spends only from there, so whatever the estimate
got wrong, the run cannot exceed it. At the last measurement the canonical story costs about
0.0217 ETH, so a Sepolia run needs `JUICE_SEPOLIA_BUDGET` raised deliberately, to 0.03 or so.

That figure is a count of payments, not of trading partners. Every cross-kernel call that owes
draws its own ticket and every winning draw is its own chain payment, so twelve rounds and an
eighty-call burst come to roughly a hundred and thirty payments across seven kernels; the story
declares the calls each act makes and the shape prices them at the odds the lottery gives each one
(`payingOdds`), with half again for the variance of the draw. Funding a kernel by the number of
creditors it has instead — which is what a settlement was before it became per call — starves the
busiest buyer partway through, and it stalls with an unpayable debt rather than failing outright.
The report separates gas actually burned from ETH merely parked in temporary wallets.

## The artifacts

`netsim-runs/<rail>-<timestamp>/` holds `log.jsonl` (every command, its output, exit status and
duration), `checkpoints/final.json` (the state read from every kernel), `metrics.json` and
`report.md`.

**The run directory is not safe to hand to anyone.** Alongside the logs — which are redacted as they
are written — it contains each kernel's whole home: its database, its signing key, its rail key and
its issued tokens. Read it in place; share `report.md` and `metrics.json`; delete the directory when
you are done.

`metrics.json` carries the commit, whether the worktree was dirty, the hash of the binary that was
driven, the network fingerprint and the story version. Two runs are comparable only when those agree —
otherwise a graph compares two different economies and calls the difference a protocol improvement.
