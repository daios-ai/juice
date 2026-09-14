# Inter-node Micropayment Settlement

Status: adopted. Its rules are the contract in `requirements.md` (P10, D14); this document is kept
as the argument for them, not as a second statement of them.

## 1. Problem

Accounts buy fixed-price services from accounts on other nodes. A node hosts accounts, keeps their
ledger, and holds one rail account backing it. Accounts are custodial: their money sits in the node's
rail account, so the node is the only party that pays on the rail and pays its fee. The node takes no
position in its accounts' trades: every risk and every random outcome lands on the buyer or the
seller, never on the node.

After node B performs a service for an account on node A, B signs the actual charge \(P\). The buyer
then owes \(D=P(1+r)\), up to the rounding rule, where \(r\) is the seller's advertised premium.

Typical \(P\) is one or two cents, while a rail payment costs \(F\) of one to fifty cents. Paying
every obligation directly is uneconomic. Most pairs trade once, so bilateral accumulation does not
help. Identities are free, so any limit that resets per counterparty is ineffective.

## 2. Design

\(L\) is the lottery size, \(L_{\max}\) its ceiling, \(E\) the node's outstanding credit, \(E_{\max}\)
its credit limit, \(r\) the risk premium.

\(L_{\max}\) is the node's own configuration, not the network's: it is the largest ticket the node
will accept from a buyer, and a node sets its own \(L\le L_{\max}\) for what it writes. A trade
happens where the buyer's \(L\) is one the seller accepts, so each side bounds its own variance
without either agreeing anything with the other. The rail fee is the operator's
cost, recovered through the import fee, so \(L\) fixes how many rail payments the node makes per unit
bought and the import fee must cover them, \(\text{import fee}\ge F/L\). For \(D<L\),

$$
\Pr(\text{pay }L)=\frac DL,\qquad \Pr(\text{pay }0)=1-\frac DL,
$$

so \(\mathbb E[\text{payment}]=D\) and \(\operatorname{Var}=D(L-D)\). If \(D\ge L\) the debt is paid
exactly. The binary \(0/L\) lottery minimises variance for a given probability of using the rail: at
\(\Pr(\text{pay}>0)=q\) and mean \(D\), Jensen gives second moment at least \(D^2/q\), attained only
by the constant payment \(D/q\). Probabilistic micropayments are standard (Rivest; Pass–Shelat);
Livepeer and Orchid use funded versions, which make winning tickets collectible without removing the
variance.

The buyer pays the exact price \(D\) from the call's budget, and its node charges the import fee on
\(D\). The lottery's swing is booked to the buyer's own balance: \(+D\) back on cancel, \(-(L-D)\)
more on pay. Expected charge \(D\). The seller funds the execution of its own action from its own
balance, so sub-calls and the node's fee settle exactly and locally as for a local trade, and it is
then credited what arrives: \(L\) or nothing, expected \(D\). Local trades are unchanged.

## 3. Exposure

Each **node** maintains one counter

$$
E=\text{service value delivered to remote buyers}-\text{final cash received for it},
$$

where *final cash* is whatever the world the node serves counts as a finalized payment: a settled
chain transaction where there is a chain, and the buyer's own signed reveal where there is none —
a world with no chain is play money, not cash-backed. It keeps one credit limit \(E_{\max}\). It admits new foreign work only if the advertised maximum charge,
plus all reserved unfinished work, keeps \(E\le E_{\max}\). Reservations are atomic. Only final cash
reduces \(E\); a cancelled draw discharges the obligation but leaves \(E\) unchanged. This is the
central security rule, and the counter is the node's own rather than any peer's: a bound that reset
per counterparty would be bypassed by minting a counterparty. There is no per-buyer rule beside it,
for the same reason — identities are free, so one would add an admission subsystem an attacker steps
around while \(E\) already bounds the loss.

The node's ledger is therefore backed by cash alone: every credit corresponds to money on its rail
account, and its solvency identity has no receivables term.

Admission enforces \(E\le E_{\max}\), so \(\text{cash received}\ge\text{service delivered}-E_{\max}\)
deterministically and independently of how many identities the buyers use. The bound is on net
drawdown: premium income drives \(E\) below zero, and later defaults consume that surplus and then up
to \(E_{\max}\). That is what the premium is for.

For honest service \(\mathbb E[\Delta E]=P-D=-rP\); for unpaid service \(\Delta E=P\). With a
fraction \(\alpha\) of volume ultimately unpaid, drift is non-positive iff \(\alpha\le r/(1+r)\).

Under honest traffic \(E\) is a random walk with negative drift. If \(\theta>0\) solves

$$
\left(1-\frac DL\right)e^{\theta P}+\frac DL e^{\theta(P-L)}=1,
$$

then \(\Pr(\sup E-E_0\ge x)\le e^{-\theta x}\). For \(P\ll L\), \(\theta\approx 2r/((1+r)L)\), so the
headroom needed to refuse honest buyers with probability at most \(\varepsilon\) is about
\((1+r)L/(2r)\cdot\ln(1/\varepsilon)\). At \(L=\$10\) and \(\varepsilon=10^{-3}\): about $725 at
\(r=5\%\), about $210 at \(r=20\%\). The approximate root is not the rigorous bound; a proof uses the
exact root or a uniform lower bound over all permitted \(L\) and \(P\).

## 4. Protocol

1. The buyer's node locks the advertised all-in price for the call's budget and \(L\) from the
   buyer's own balance, and sends the request with a settlement identifier, \(L\), a commitment
   to fresh randomness, and the proven rail address a winning ticket will be paid from. An
   insufficient balance is refused locally before anything is sent.
2. The seller's node atomically reserves the advertised maximum charge against its credit limit and
   checks the seller can fund the execution. Either failure is a signed rejection.
3. The seller performs the service. Its signed receipt carries the actual charge \(P\) and a fresh
   random value. It needs no prior commitment: it cannot know the joint outcome without the buyer's
   secret, so it cannot select on it. The reservation is corrected to \(D\).
4. If \(D\ge L\) the buyer owes \(D\) exactly. Otherwise the buyer reveals its secret and a uniform
   mapping of the two values and the identifier decides pay \(L\) or cancel; both sides recompute it.
   On cancel the lock is released and \(D\) returned, so the buyer paid nothing. On pay the buyer's
   balance carries the rest of \(L\) and the node pays \(L\) on the rail.
5. The seller closes the obligation only on a final payment from the buyer's node's proven rail
   address for the required amount; one rail transaction closes one obligation, and the seller is
   credited the whole of it. A payment from an address some admitted call named as its payer is
   held until that call's obligation is resolved, since its reveal may still be on its way.
6. If no valid receipt arrives the call is never presumed dead: it stays parked and is retried under
   its identifier until a receipt arrives or the maximum pending age expires, and only then settles
   locally as a failure with no obligation. Every state is durable under the identifier.

Because \(L\) is locked before the request, a pay outcome is funded internally. The rail sends one
payment at a time, so a burst of winning tickets drains in order; each parallel call locks its own
ticket, so a composite firing \(n\) remote calls needs \(n\) tickets' worth of free balance.

## 5. Consequences

**Buyer risk.** For \(n\) equal debts the number of \(L\)-payments is \(\mathrm{Binomial}(n,D/L)\); a
buyer with budget \(B\) exhausts it with an ordinary binomial tail probability. A buyer wanting
smaller jumps chooses a node with a smaller \(L\) and a larger import fee.

**Premium.** A seller valuing settlement by mean minus \(\beta\) times variance is indifferent at
\(D-\beta D(L-D)=P\), hence \(r\approx\beta L\) for \(P\ll L\). In practice \(r\) covers both
variance and expected default.

**Node.** It earns fees on realised flows, holds no reserve against trade variance or default, and
its credits are exactly cash-backed.

**Composition.** Recursive at the trade level and bilateral at every edge: if A buys from B and B
buys from C, the obligations \(A\to B\) and \(B\to C\) are independent, and B needs both a credit
limit and buyer-side liquidity. That liquidity is B's own balance, never the budget of the \(A\to B\)
call — a ticket of \(L\) inside it would force B's price above \(L\) for a one-cent sub-service. In
one realisation B pays \(L\) to C and receives nothing from A; in the mirror it gains nearly \(L\).

## 6. Attack surface

| Attack | Result |
|---|---|
| Default on a winning draw | Counted in \(E\); further net drawdown limited by \(E_{\max}\). Requires a dishonest node, since \(L\) was locked. |
| Trade until first win, then default | Cancelled draws remain in \(E\); exposure cannot be reset. |
| Identity churn | All buyers share one node-wide counter. |
| Concurrent requests | Atomic reservation of maximum charges. |
| Operator sets large \(L\) | Capped by the world's \(L_{\max}\); locked from the buyer's balance before each call. |
| Seller manipulates draw | Cannot know the outcome without the buyer's secret. |
| Buyer withholds reveal | Nothing is credited, and the exposure stands against \(E_{\max}\). |
| Seller withholds receipt | No obligation; reservation released; nothing gained. |
| Redraw or replay | One identifier, one stored outcome. |
| Payment misattribution | Final transaction from the proven address, exact amount, unused by another obligation. |
| Rail outage | Payment pending; exposure stands until cash lands. |
| Fee spike | Fraction rises as \(F/L_{\max}\); the cap moves with the rail. |
| Buyer liquidity exhaustion | Buyer's balance against the node's \(L\), and its probability bound. |
| Seller variance exhaustion | The premium \(r\), \(E_{\max}\), and the operator's reserve. |
| Operator loss | The rail fee and its spikes, recovered in expectation by the import fee; no position in any trade. |

## 7. Assessment

The economics are coherent and simpler than bilateral accumulation, manual settlement, or pairwise
collateral. The primitive is standard; the additions are the node-wide cash-only credit counter and
the pass-through node. The counter turns identity churn into a bounded drawdown. The pass-through
removes the operator's reserve and makes node solvency exact.

One thing changes in the product contract: a remote call's price becomes deterministic in
expectation, with the actual charge either zero or the lottery size. Local prices stay exact.
