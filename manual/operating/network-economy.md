---
title: The network economy
parent: Operating a kernel
nav_order: 3
---

# The network economy

How money moves between kernels. A buyer needs only
[Calling an action on another kernel](../calling/running.html#calling-an-action-on-another-kernel);
this chapter is for whoever sets the rates and answers for the balance.

## What a cross-kernel call costs and who gets it

Three numbers make the price.

```
provider's price                  mp
serving kernel's markup           sr = mp + ceil(mp × remote_bps / 10000)
buyer's kernel's import fee        q = sr + ceil(sr × import_bps / 10000)
```

The buyer pays `q`. The provider's own kernel takes its ordinary fee on the
provider's margin out of `mp`. The markup goes to the provider, who funded the
work on credit and is paid by a draw whose expected value, not whose outcome, is
the debt. The buyer's kernel keeps the import fee. No kernel keeps any part of a
draw: a kernel that took the difference would be paying its providers less than
they are owed on average.

Both rates default to 500, that is 5%. Every component is fixed before the call
runs and is carried in the signed record, so neither side can move the price
afterwards.

With `mp = 2.00` and both rates at 5%: `sr = 2.10`, `q = 2.205`. The seller's
receipt shows `charge 2000000` and `premium 100000`; the buyer's transaction shows
`gross 2.205`, of which `0.105` is the import fee.

## The seller advances the work

A call from another kernel runs on the seller's money. The provider's account funds
the execution and is repaid when the buyer's kernel settles.

The kernel tracks one number, its **exposure**: everything it has delivered to
other kernels and not yet been paid for, less the cash received for it. When a
call is admitted, exposure rises by the most that call could owe. When the call
finishes, the figure is corrected to what was actually charged. A call that would
take exposure past the limit is refused with a signed rejection, and so is a call
whose provider cannot fund the work.

```
Credit:     owed-to-us=4.20 credits limit=500.00 credits
```

### Bounding what strangers can cost you

`credit_limit` is one number covering every peer at once, not a limit per peer.
Creating new identities therefore buys an attacker nothing: a thousand new kernels
share the same ceiling as one. The limit defaults to 500 units.

It is independent of settlement: refusing every ticket would not stop this kernel
serving, and setting the limit to zero does not stop it either — only current
exposure is bounded.

Free calls add nothing to exposure.

## Settling one call

Every cross-kernel debt settles on its own, identified by a ticket both kernels
know the call by.

Paying every small debt individually would cost more in payment fees than the debts
are worth. So a debt is settled by a draw, as follows.

The buyer commits to a secret when it makes the call. The seller mints a nonce
after executing and signs it into the receipt. Neither number alone decides
anything, and neither side can choose the outcome.

Let `D` be the debt and `L` the buyer's kernel's `lottery` setting.

- If `D ≥ L`, or `L` is zero, the debt is paid exactly.
- Otherwise `L` is paid with probability `D / L`, and nothing is paid otherwise.

The expected payment is `D` either way. Over many calls the two kernels exchange
the right amount of money in a fraction of the payments.

The buyer stakes the whole of `L` from the immediate caller's balance when the call
is dispatched, releases it at settlement, and reserves the payment from it if the
draw pays. The seller refuses a call whose `L` is above its own `lottery_max`,
since no retry would change that.

Then the buyer reveals the secret: at once on a loss, and once the payment is final
on a win. The seller recomputes the draw against the commitment it stored. A loss
is accepted however late it arrives, because a deadline that turned a loss into a
win would make every outage cost the buyer money.

### What counts as payment

On a network with a chain, an obligation closes only against a finalised payment
from the address the buyer proved at admission, for the amount drawn.

On `play` there is no chain, so the buyer's signed reveal *is* the payment. Debts
close by themselves and no operator settles anything by hand. `play` credits are
backed by nothing, and this is the sense in which that is true.

### The settings

```
Rates:      … lottery=1.00 credits lottery_max=5.00 credits
```

`lottery` is the face value your kernel's buyers stake. Setting it to zero pays
every debt exactly, at the cost of a payment per call. `lottery_max` is the largest
face value you will accept from somebody else's buyer.

Both are your kernel's own settings. They are not network-wide, and a buyer whose
kernel sets a large `lottery` needs that much available on top of each cross-kernel
price.

## Federation does not chain

A kernel never re-serves an action it imported from a third kernel. To reach a
provider you resolve from the kernel that owns it. There is no chain of
intermediaries between a buyer and a provider, and therefore nothing to audit
through.

## Reputation across kernels

Kernels exchange evidence about trade, not scores.

An evidence record names the kernel and the action it is about, at most the
counterparty **kernel**, the outcome, and the times. It never carries the rater's
identity, any payer or caller, any transaction, trace or process id, any argument
or result hash, any amount, or any recipient of value. The evidence is public
about actions and silent about people.

A kernel gossips only its own receipts, never anything it learned from another
kernel. Because no kernel relays what it heard, every piece of evidence is
first-hand.

A rating is **trade-backed** when it points at a receipt the subject kernel also
issued for that action, naming the rater's kernel as counterparty. Only those are
evidence of a real trade; other ratings are counted separately as unverified. A
kernel that issues two different valid ratings under one key has equivocated, and
both are dropped from every derived figure.

```
$ juice admin peer inspect beta-kernel
```

shows this in two parts. The execution summary counts the kernel's own receipts.
Counterparty experience groups what other kernels report, by issuer, never folded
into the first.

What this proves is attribution and immutability: that a record was issued by the
key it claims and has not been altered. It does not prove honesty. The only
signals resistant to manufactured identities are your own settled experience and
the number of distinct kernels that report trade.

## Retention

A peer is kept while it has acted recently, is suspended, or has anything
unresolved in either direction. Once idle past `peer_retention_days`, its cached
actions, statistics, directory entries and evidence are purged and the kernel row
goes.

The account row stays, because it anchors the ledger. Every credit involving that
peer stays reconstructible; the immutable history is never purged.
