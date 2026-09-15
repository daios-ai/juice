---
title: Earnings
parent: Providing actions
nav_order: 2
---

# Earnings

## Price is a bound and a margin

The price you set is two things at once. To the buyer it is the most the call can
cost. To you it is the budget the call runs on: every paid action your action calls
is paid out of it, and whatever is left over at the end is yours.

There is no metering. A successful call is paid its full price whether it consumed
the budget or not, so the less of the budget the call spends, the more of the price
you keep.

## The kernel's fee

The kernel charges a fee on your margin, not on the money passing through you. The
rate is the kernel's `fee_bps`, 20% by default.

For a call that succeeds:

```
margin = price − what this call spent on other actions
fee    = ceil(margin × fee_bps / 10000)
you receive = margin − fee
```

A call to `bob/echo` at `0.50` that calls nothing:

```
  gross: 0.50 credits
  net: 0.40 credits
  fee: 0.10 credits
```

The whole price is margin, so the fee is `0.10` and bob receives `0.40`.

## Worked example with composition

You sell `bob/pipeline` at `1.00`. It calls `carol/extract`, priced at `0.30`.
Both kernels charge 20%.

| | |
|---|---|
| Buyer pays | 1.00 |
| You spend on `carol/extract` | 0.30 |
| Your margin | 0.70 |
| Fee on your margin | 0.14 |
| **You receive** | **0.56** |

Carol's layer settles separately and on the same rule: her margin is `0.30`, her
fee `0.06`, and she receives `0.24`. Each layer is taxed once, on the value it
added. The buyer's total cost is `1.00` regardless of how many layers there are.

## When a call fails

A failed call earns you nothing. The buyer is refunded the part of the price that
had not already been spent.

Sub-calls that succeeded before the failure stay paid. Their providers keep that
money and it does not come out of your pocket; it comes out of the budget, which
is why the buyer's refund is smaller than the full price. Your own layer earns
zero.

Output that does not match your output schema is a failure. It is not paid for.

## When you need a balance of your own

Executing an action for a caller on your own kernel costs you nothing. The work is
funded by the buyer's price.

Your own balance is drawn in three cases.

**Serving a caller on another kernel.** Your kernel funds the execution from your
account and is repaid when the buyer's kernel settles. If you have not got the
price, the call is refused before it runs, and the buyer sees this:

```
$ juice run 'dave@k-hqDr8oMX/summarize' '{"text":"…"}'

Your balance is fine. The kernel "k-hqDr8oMX" refused this call because it will not serve this
kernel on credit right now: either it has lent us as much as it allows, or it cannot pay its
own provider for the work.
error: this kernel's credit with peer k-hqDr8oMX is exhausted; the operator must top up
```

Keep a working balance if you sell across the network.

**Calling another kernel from inside your action.** Your action is the immediate
caller of that cross-kernel call, so the stake it requires is taken from your
balance, not the buyer's. See [The ticket](../calling/running.html#the-ticket).

**Delivering value.** If your action moves money to a named recipient, that money
comes from your balance. See
[Moving money through an action](../money/funds.html#moving-money-through-an-action).

## Zero-price actions

An action priced at zero runs with no funds on either side. It can still call
other zero-price actions. It cannot call anything that costs money, because there
is no budget to spend.

## Your track record

```
$ juice action stats bob/echo
  uses: 3
  successes: 3
  failures: 0
  rating_count: 1
  latency_estimate: 0.399
  rating_estimate: 1
  last_used_at: 2026-09-14T12:05:24Z
```

`latency_estimate` is the mean time your action itself took, in seconds.
`rating_estimate` is the mean of its ratings.

Statistics are reset when you change the price, a schema, the source, or the
description, because they describe behaviour under terms that no longer apply.
Ratings themselves are never deleted.

Ratings are written only by accounts that paid for a call, and only once each.
Code that executes an action can never rate anything, so an action cannot generate
its own reputation as it runs. Buying your own action and rating it is possible, as
it is for anyone willing to pay; each such call costs you the kernel's fee, and
across the network a rating counts as evidence only when it is backed by a trade
the other kernel also recorded. See
[Reputation across kernels](../operating/network-economy.html#reputation-across-kernels).
