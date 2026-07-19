# Design analysis: users, visibility, and federation

Status: discussion notes, not requirements. Nothing here is implemented, with one
exception: §8 is resolved and shipped (`/juice/fed/step/1`, `@sys/step/race`,
`@sys/step/join`) — see requirements.md §9, §10, §13 for the normative text.
Focus: ergonomics and user empowerment. Mechanism is referenced only where it
explains what a person can or cannot do.

## 1. Goals

Juice is a kernel for callable actions where every call costs money and actions
compose. The design goal that distinguishes it from a platform is structural:

**A laptop behind a home router federates identically to a machine in a
datacenter.** No advertised address, no port forwarding, no `.well-known`
document, no registration. `juice serve` makes you a peer.

This produces two tiers of participation, with a one-command promotion between
them:

| Tier | What you are | What you hold |
| --- | --- | --- |
| Tenant | a user on someone else's kernel | a handle, a balance, actions you own |
| Sovereign | your own kernel | a signing key, a global name, portable standing |

Most of the analysis below is about the gap between those two tiers, because
that gap is where the user's power actually lives.

## 2. The mental model

`action → user → kernel` maps onto `video → channel → platform`. The mapping
holds for naming (`@owner/name`) and for revenue (a platform cut on each sale),
and it breaks in one decisive place: **the payer is the caller, not an
advertiser.**

Three consequences follow, all of them good:

- There is no attention economy. The unit of value is a settled call, so nothing
  rewards prolonging interaction.
- A rating requires a purchase. Only the party who paid may rate, which makes
  ballot-stuffing cost real credits. YouTube cannot do this because its votes
  are free.
- Search only returns things you can buy. No result is a dead end for lack of
  access.

For engineers, the complementary analogy: an action is a `setuid` function
(it makes onward calls as its *owner*, on the *caller's* budget), a price is a
budget passed by value into a stack frame, a step is a suspended continuation
with its stack pre-reserved, and `subscribe` is a module import that does not
re-export its own imports. That analogy explains authority and scope exactly and
says nothing about money or agency — which is precisely the axis where users
differ from actions.

## 3. Expected UX today

What a person can and cannot do, as flows.

### Works well

**Publishing.** An action starts private. You widen it in two deliberate steps —
first to everyone on your kernel, then to the network. Staged rollout is built
into the model rather than bolted on. A private helper still works when your own
public action calls it: private means "nobody else may depend on this directly,"
not "unusable."

**Searching.** You get actions you can actually call, ranked, with prices.
Everything returned is buyable.

**Buying.** One advertised price bounds the whole call tree, including anything
the action calls. You cannot be surprised by a bill. Failure refunds the
remainder automatically.

**Intermittency.** A call to a peer who goes offline parks rather than fails.
Funds stay locked, the process stays open, and settlement resumes when the peer
returns. Home kernels are a first-class case, not a degraded one.

### Hits a wall

**Finding something that isn't local.** If the action you want lives on a kernel
your operator hasn't subscribed to, you see nothing. No "found elsewhere," no
way to know it exists, no way to ask. A silent dead end.

**Finding a person.** There is no directory. To send credits, address an
approval step, or message someone, you must already know their handle from
outside the system. Results say `@alice/summarize`, but `@alice` is a dead link —
no profile page exists.

**Running an autonomous agent.** Effectively blocked. An agent cannot type a
password, so it authenticates with a key — and the kernel decides that anything
holding a key is a foreign peer. It then loses access to every `local` action on
its own machine, and nobody can transfer it credits. There is currently no way
to have a headless agent be a normal citizen of your own kernel.

**Working with another kernel's people.** You can call their actions. That is
the entire relationship. You cannot pay a specific person there, hand a task to
a named individual and await their reply, rate them, or discover who they are.

**Getting access.** Only the operator can subscribe to a new kernel, and there
is no in-product way to ask them. Your reachable market is whatever they chose.

**Being cut off.** Another kernel cannot see individual users, so it can only
block yours wholesale. You can be cut off for a stranger's behaviour, without
warning or appeal.

**Leaving.** You take nothing. Not ratings, not history, not a single receipt you
can prove to a third party — every receipt is signed by the kernel, so it
attests what the kernel currently says. This is the sharpest gap in the system.

**Your balance.** The operator can zero it without your involvement, and the
resulting ledger row is indistinguishable from you cashing out voluntarily.

## 4. Comparison to other systems

| System | Exit | Standing | Lesson for us |
| --- | --- | --- | --- |
| YouTube / Twitter | to another platform you also don't control | dies at the boundary | you cannot become YouTube; here you can become a kernel |
| Email | run your own server — permissionless in theory | domain-level, not per-sender | reputation settles at the level that can enforce; self-hosting is legal but taxed |
| NFS + `AUTH_SYS` | — | a local user number, shipped across a trust boundary unproven, and believed | never ship a local name as if it were global |
| Kerberos | — | a principal with cryptographic proof, separate from local accounts | the other road: give users real credentials so identity travels with evidence |
| Banks | move your money | your credit history is *not* held by one bank | custodial money is fine; custodial standing is not |

Juice avoids the NFS failure by refusing to send user identity across a boundary
at all. That is safe by subtraction, and the missing user tier is what it costs.

Email is the closest structural cousin and the most useful warning: permissionless
entry does not by itself produce a level field. Where Juice is better placed —
there is no centralised blocklist cartel, because access is a per-relationship
deposit rather than a global reputation score. Where it is similar — a new key
starts unknown, and being small is taxed.

## 5. Federation structure

Federation is deliberately **wholesale, not retail**. It is an economy of
kernels; users live inside them.

| Crosses a kernel boundary | Stays inside |
| --- | --- |
| kernel identity (a public key) | user identity — users have no global name |
| action contracts, owner-qualified | balances, transfers, direct payment |
| per-action stats, via gossip | per-user standing (there is none) |
| prepaid credit between kernels | who actually made the call |

That last row is the load-bearing one: an inbound call arrives as the calling
*kernel*, never as a person. Cross-kernel calls are anonymous at the user level
by construction.

### Why this encapsulation is good

- **Privacy that cannot be misconfigured.** No cross-kernel profile can
  accumulate, because the data is never sent. No bug or subpoena reveals it.
- **You cannot be contacted for free.** The only inbound a stranger can create is
  a call to an action you published, at a price you set. Every open addressing
  system ever built got spammed and had to retrofit pricing; this one starts
  with it.
- **Liability sits where enforcement power sits.** Another kernel cannot police
  your users, but you can — you hold the balances and the suspend button. Email
  reached the same equilibrium after thirty years of trying per-sender
  reputation.
- **Nothing to squat.** No global user namespace means no registrar, no name
  disputes, no rent.
- **Kernels can differentiate.** Curation, vetting, norms, pricing. If users were
  global, kernels would be interchangeable pipes.

### What it costs

Collective punishment, no path for a user to request access, and — the real one —
no way to carry your record when you leave or promote yourself.

## 6. Enhancements, ranked by UX payoff

**1. A graduation path (co-signed receipts).**
Give users their own signing key so the receipts naming them carry their
signature alongside the kernel's. Then a receipt reads "kernel B attests this,
and alice attests she was the party." Alice can walk to another kernel — or
stand up her own — and prove a year of settled work.

Critically, this is **pull, not push**: she presents evidence at her own
discretion. Nobody becomes reachable, no directory is created, no global names
are minted, and it is strictly more private than a profile. Sybil resistance is
unchanged, because a fresh key with no attestations is worth nothing.

The hook already exists: `recovery_public_key` is a user-held key today, fenced
to password reset.

**2. Let a local agent be a local citizen.**
Stop inferring "is foreign" from "holds a key." Declare it. This unblocks
headless agents as first-class users of their own kernel and costs one field.
It should not be fixed by splitting peers into a separate entity — federation
currently needs *no* separate money model precisely because a peer is a user,
and that is worth keeping.

**3. Recovery for the kernel key.**
A user who forgets a password recovers with a seed phrase. An operator whose
laptop dies loses their kernel identity, its reachability, and all its
accumulated standing, permanently. The higher-stakes credential has the weaker
safety net, and it bites hardest on exactly the laptop-kernel case the design is
built for.

**4. A want-signal.**
When a search finds nothing locally, let the user register the request with
their operator. Converts a silent dead end into a queue the operator can act on.

**5. Profile pages.**
`@alice` should be readable, scoped by what the viewer can already see. Note:
any count shown must be computed per viewer, or "@alice — 47 actions" tells an
anonymous visitor she has 44 private ones.

**6. Distinguish redemption from seizure.**
A withdrawal currently needs nothing from the account holder and leaves a row
identical to a voluntary cash-out. If users are subaccounts, this is the one
power that should leave a distinguishable trace.

**7. Make free samples a first-class onboarding flow.**
Price-0 actions are already callable with no deposit, and an unknown caller is
auto-provisioned an account. That is the cold-start escape for a new kernel, and
it deserves to be a documented path rather than an emergent property.

## 7. Simplifications

- **Search what is described; address what is named.** Only actions carry
  contracts, so only actions are retrieved by relevance. Users and kernels are
  *facets* over that result set — filters and groupings — never separately
  ranked result types. One corpus, one formula, one privacy rule.
- **Keep the encapsulation.** No cross-kernel user directory, no global user
  names, no reachability. Every good property in §5 comes from denying inbound;
  none comes from denying outbound.
- **Name the peer fact once.** `peer` is currently reconstructed ad hoc from
  whatever field is nearby, in at least two places. One declared property, read
  everywhere.

## 8. Steps: the primitive is right, the wire was incomplete (resolved)

The peer-step trap listed here previously was read as evidence that `Step` was
mis-designed. It is not. The analysis and its resolution, since the conclusion
changes what one should *not* do:

### A Step is a two-hole suspended call, and both holes are load-bearing

Juice's calculus is attributed at the primitive: `Call(caller, trace, action,
args)` takes the applier as a parameter, and every application produces a
transaction naming its parties. A suspended call is therefore a two-hole context:

```text
Step = Call( ◦caller , trace, action, partial_args ⊕ ◦input )
```

`required_caller` is not an access-control field bolted onto a continuation — it
is a constraint on the caller hole. That distinction decides the design:

- **User input.** The caller hole is the *payload*. "Alice approved" is worthless
  unless the record proves it was Alice, and the completion transaction's
  `caller_user_id`, carried into the signed receipt, is that proof.
- **Suspended execution.** The caller hole is incidental — but never *empty*,
  because Juice has no principal-less events. A webhook resolves a step by
  registering as a user; a peer acts as its proxy user; a timer would be a
  scheduler holding an account.

So one primitive covers both purposes: when identity matters it is the
attestation, when it doesn't it is free. This holds *because of*
everything-is-a-principal, not despite it.

**Rejected: replacing `required_caller` with a bearer capability.** It was the
obvious "unification" and it is wrong. A capability is transferable, so the
record would attest that *a token* approved, not that Alice did — breaking the
purpose that works to serve the one that didn't. §9's trace capability is not a
counterexample: presenting it makes you act *as the action owner*, so Juice
capabilities are proxies for principals, not bearer rights.

### What the trap actually was

Not a semantic defect. The calculus has two eliminators — `Call` (enter) and
`CompleteStep` (resume) — and federation transported only the first. A wire
carrying entry but not resumption strands every cross-boundary continuation.
That is why it presented as a trap rather than a type error: the step was
well-formed at every layer, with nothing able to speak the sentence.

Worse than first recorded: the exit was closed too. `EndProcess` is
process-owner-only, and the process owner of an inbound federation call *is* the
keyless proxy account — so not even a force-close could free the funds.

**Resolved** by `/juice/fed/step/1` (list + complete), with `admin steps` /
`admin complete` driving it. No restriction on what a continuation is, and no
capability refactor. Note what still does not cross: a cross-kernel step
addresses a *kernel*, never a person — people are addressed across kernels as
*action owners* (`@B/bob/inbox`), priced and therefore spam-resistant. That
boundary is the §5 encapsulation, kept intact.

### What the primitive does not do

Worth stating, since these are the real limits rather than the imagined one:

- **One resolver, fixed at creation.** "Whichever of these arrives first" is not
  expressible directly.
- **One shot.** A recurring subscription is a stream, not a promise — genuinely
  outside this primitive.
- **External resumption only.** Suspended code cannot wake itself; every
  resumption is a paid, attributed event.

The first is now *composed* rather than added: `@sys/step/race` and
`@sys/step/join` (§9) are ordinary actions parked into as contributor steps, both
resuming a shared onward step — any-of via the store's atomic claim, all-of via a
counter. The continuation is parked once, not once per contributor, and both are
confined to their own process so a leaked step id cannot spend another process's
funds. The kernel supplies funded, attributed, one-shot resumption; coordination
policy lives in actions above it, which is where a barrier's shared state belongs.

A Step is best named a **defunctionalized one-shot continuation**: not a closure
but a data structure naming a top-level action plus its saved environment
(`partial_args`), with a pre-paid frame. It is therefore not a Monad — there are
no closures to `bind` — but it is the compilation target monadic workflows lower
into, exactly as async/await lowers to state machines. Closures would be
unserializable, unpriceable, and unattributable; this is the right trade.

## 9. Open questions

- The default bootstrap peer is the project's own node. It is a list, and every
  public kernel is a valid seed, so this is not structural — but it is the one
  place where "anyone can run it from a laptop" has a default dependency on you.
- Enhancements 1–7 in §6 remain open; co-signed receipts (the graduation path) is
  still the highest-payoff and the one that changes the two-tier structure.

## 10. Summary

The encapsulation is the product. Keep inbound closed.

The missing affordance is outbound: a person should be able to leave with proof
of what they did. That single change converts the two-tier structure from
"tenant or start over" into "tenant, then graduate" — which is the difference
between a platform you can exit and a platform you can grow out of.
