# Launch strategy and the rail integration

Date: 2026-09-02. Inputs: `requirements.md` v0.13, `docs/rail-integration.md`, `ROADMAP.md`.

## 1. Problem

A person who wants to try Juice should not have to create an external account, buy a
stablecoin, hold gas, and register an address before the first useful action. Under the
planned rail integration those steps precede the first priced call on a chain-railed kernel.
The question is whether the rail document is the minimal launch path, what else a launch must
be judged on, how the servers are run in practice, and what the literature says about the same
problem.

## 2. What the rail document specifies

- A network is defined by a world file (name, chain id, token address). Three ship: **play**
  (manual rail, no chain), **test** (Arbitrum Sepolia, mock USDT0), **real** (Arbitrum One,
  USDT0).
- One kernel is one `juice serve`, one home, one identity key, one world, permanently. The
  world digest is written at first boot and a mismatched world file refuses to start.
- Money has two primitives: a **crossing** on `sys` against a finalized fact (creates or
  destroys credits) and an owner-authorized **transfer** (conserving). Deposit, withdrawal,
  attribution, compensation, and settlement decompose into these.
- The rail is an adaptor behind a kernel-owned interface. The manual adaptor's finalized facts
  are the operator's own records; the chain adaptor wraps `juice-rail`.
- Every signature carries the world digest; discovery runs per network; catalog, transactions,
  and rail never cross worlds.
- The client is the only thing spanning networks: named profiles, `juice use <name>`
  verifies the server and prints its name, key, and world.

## 3. Minimality

Two separate judgments are needed.

**As a design for real money, the document is close to minimal.** Two primitives, one rule set
in every world, chain logic left in the rail, existing idempotent ledger operations reused.
Two items are not minimal and are deferrable:

1. The world digest in every signature domain is a lockstep protocol break needed only when a
   second world exists. With a network of a few nodes the break is cheap whenever it is made,
   so it should be made when the test or real world ships, not before.
2. The kernel-owned `Rail` interface carries chain vocabulary (refills, gas as big integers,
   block cursors, finalized-cut balances) that the manual adaptor must stub. The minimal
   kernel interface is pay, status, and deposits; refill and custody reads belong to the chain
   adaptor. This is a factorization change, not a semantic one.

**As a launch, the document is not the minimal thing, because the launch does not need it.**
The play world with the manual adaptor is, operationally, the current kernel:

- U3 already lets the operator credit an account against any external fact, idempotently.
- U18 already runs zero-price actions with no funds at all.
- A promotional credit is a transfer from a funded `sys`, which the document already names as
  the mechanism for attribution and compensation. It is conserving in every world.
- Registration of an address is already deferred to the first deposit or withdrawal. A user
  who never withdraws never needs a wallet.

So onboarding asks nothing of the kernel. A newcomer needs an account (handle and password)
and either free actions or credits somebody transfers to them. Everything that makes this
convenient (a sign-up grant, a card top-up adaptor, sponsored calls) is operator policy or a
second adaptor built on U3, which is the requested ordering: minimal kernel now, convenience
on top later.

Conclusion: launch on the play kernel as it stands. Do not gate launch on the rail.

## 4. Evaluation axes beyond minimality

1. **Sybil exposure of sponsored credit.** On a real rail, a gift spent on the giver's own
   action becomes withdrawable earnings minus fee: free credits are cash. The ledger has no
   restricted-credit type. Sponsor only in the play world, or accept a bounded loss per
   account as the exposure cap already accepts per network. Account-creation rate limiting is
   the only existing bound and it is weak.
2. **Custody and legal regime.** Play credits that cannot be withdrawn are closed-loop points.
   Redeemable credits make the operator a holder of stored value. The play world is the launch
   precisely because it avoids the second regime.
3. **Continuity of value across worlds.** Networks never cross, so play balances and play
   prices do not carry into the real world. A migration is expressible (a `sys` deposit on the
   real rail plus transfers, conserving), but whether play credits will ever convert must be
   stated to launch users up front.
4. **Two-sided cold start.** Buyers need actions, providers need buyers. The levers are
   zero-price actions and sponsored credits; which side to subsidize is a market decision.
5. **Trust concentration.** One kernel holding everyone's credits is a centralized service at
   launch. Acceptable while small; the narrative should say so.
6. **Operator load.** Manual crediting serves tens of users, not thousands. Self-service
   funding is a second adaptor, later.
7. **Protocol-break cost.** Lockstep upgrade is cheap now and grows with the peer count. Breaks
   that are certain (the world digest) should be batched with the rail release.

## 5. Running the servers

**One kernel per rail.** This follows from the ledger, not from convenience: every account has
one balance, a balance is a quantity of one unit, and play credits, test USDT0, and real USDT0
are three units. A single ledger would need a currency on every balance, trace, receipt, and
settlement record, which the document rules out. Operationally a rail is a separate process
with its own home and port:

```text
JUICE_HOME=/srv/juice-play  juice serve --addr :4040
JUICE_HOME=/srv/juice-real  juice serve --addr :4041
```

Each has its own identity key, so the same operator is a different kernel on each network.
That is consistent with isolation, since evidence and names never cross either. For launch,
exactly one runs: play. The real one is added when the rail ships. Cost is N worlds = N
processes, N homes, N keys, N peer sets; for the foreseeable network N is two.

**The switch is on the client.** A server has no switch by design. The client has
`juice use <profile>`, which dials, verifies the token, and prints name, key, and world;
withdraw and settle confirm and name the world; `JUICE_PROFILE` overrides per invocation for
scripts.

**Supervision.** A UI supervising several kernels is a set of profiles, one per server. Each
server's `admin identity` yields the rail address, balances, the earnings and in-transit split
of `sys`, the solvency identity with named terms, and the stop signal with reason and age.
The UI needs to know nothing about the adaptor beyond labeling the world.

**Not mixing rails** is enforced at four layers, each sufficient for what crosses it:

| Layer | Guard |
|---|---|
| Database | first-boot digest; mismatched world file refuses to start |
| Signatures | world digest in every domain; foreign artifacts fail verification |
| Discovery | per-network namespace; different worlds do not find each other |
| Chain adaptor | domain check refuses boot on wrong chain or token |

**Unguarded, in order of importance:**

1. **The client cannot verify the world it switches to.** `/health` carries no world, so a
   profile labeled Real pointing at a Play server switches silently. Fix: `/health` gains
   `world` and `world_digest`; the profile stores the expected digest (from the world file,
   else pinned at first login); `use` compares digests, not names, and refuses on mismatch.
2. **A paid run is not confirmed.** Agents are first class, so a client on the wrong profile
   spends there. Mitigation is the verifying `use` step and the world shown in every listing.
3. **Operator errors outside the kernel.** Copying a home directory duplicates an identity on
   two hosts. Real tokens sent to a test vault address are recoverable only because the
   operator holds that key. Neither is rail-specific; both belong in a deployment note.

## 6. Comparable systems

1. **KARMA** (Vishnumurthy, Chandrakumar, Sirer, P2PEcon 2003). A peer-to-peer currency in
   which new nodes receive a seeded initial balance. It is the canonical treatment of seeding
   newcomers and exhibits the same tension as axis 4.1: seed credit invites Sybil harvesting
   unless account creation is costly or the seed is non-convertible.
2. **Credit networks**: Fugger's Ripple (2004) and Dandekar, Goel, Govindan, Post, "Liquidity
   in Credit Networks: A Little Trust Goes a Long Way" (EC 2011). Bilateral IOUs bounded by
   trust limits are the exposure cap X. Their result that small credit lines yield high
   liquidity supports a launch in which `sys` extends bounded credit to newcomers rather than
   requiring prefunding.
3. **Lightning Network** (Poon and Dryja, 2016). A user must transact on-chain and obtain
   inbound liquidity before the first payment, the same wallet-plus-gas friction as here. The
   ecosystem's answer was custodial wallets and liquidity providers, which is the manual-rail
   operator role.

The residual settlement lottery in P10 descends from Rivest's electronic lottery tickets
(1997); that part of the design is already anchored in this literature.

## 7. Recommendations

1. Launch on the play kernel as it exists. No rail work precedes launch.
2. Fund newcomers by operator deposit or by transfer from a funded `sys`; treat the grant size
   and any account-creation cost as operator policy.
3. State to launch users whether play credits will ever convert to real credits.
4. When the rail ships: world digest on `/health`, on the profile, and checked by `use`;
   batch the signature-domain break with that release; narrow `Rail` to pay, status, and
   deposits.
5. Sponsor credit on real-rail kernels only under an explicit per-account bound, or not at all.
6. Write a deployment note covering one home per world, service units, key custody, and the
   recovery path for tokens sent to the wrong vault.
