---
title: Earnings
parent: Providing actions
nav_order: 2
---

# Earnings

An action's price funds its execution and determines the provider's margin.
This chapter follows that allocation through successful and failed calls, then
explains when a provider also needs funds in its own account.

## Price is a bound and a margin

The price you set provides a budget for the action and any work it buys from
other actions. For a successful local purchase, the buyer pays that price and
you earn what remains after downstream costs and the kernel's fee. Remote
purchases add serving and import charges and use the settlement procedure
described in [The ticket](../calling/running.html#the-ticket).

Since a successful call is paid at a fixed price, reducing its downstream costs
increases your margin. The buyer receives the agreed service at the agreed
price regardless of how much of the execution budget it consumed.

## The kernel's fee

The kernel applies its fee to the margin remaining after downstream work has
been paid. The rate is configured as `fee_bps`, in hundredths of a percent;
the default value of 2000 is 20%. For a successful call:

```
margin = price − what this call spent on other actions
fee    = ceil(margin × fee_bps / 10000)
you receive = margin − fee
```

A call to `bob/echo` at `0.50` that calls nothing:

```
  gross: 0.50 fUSD
  net: 0.40 fUSD
  fee: 0.10 fUSD
```

Because this action buys no downstream work, its whole price is margin.
The 20% fee is therefore `0.10`, leaving Bob `0.40`.

## Worked example with composition

Suppose you sell `bob/pipeline` at `1.00` and it calls `carol/extract` on the
same kernel for `0.30`. With a 20% fee, your layer settles as follows:

| | |
|---|---|
| Buyer pays | 1.00 |
| You spend on `carol/extract` | 0.30 |
| Your margin | 0.70 |
| Fee on your margin | 0.14 |
| **You receive** | **0.56** |

Carol's call settles under the same rule. Assuming it buys no further work,
its margin is `0.30`, its fee `0.06`, and Carol receives `0.24`. Each provider
is charged on its own margin, so adding a layer does not tax the same gross
payment again. The buyer's total remains `1.00`.

## When a call fails

If your call fails, your layer receives no execution payment and incurs no
margin fee. The unused budget is refunded, while downstream work already
delivered remains paid. This explains why the refund can be smaller than the
original price even though your own action earned nothing.

The same rule applies when the action returns a result that violates its output
schema: the result is treated as a failure, with completed downstream work
preserved.

## When you need a balance of your own

For a local buyer, the execution budget comes from the buyer's allocation.
This covers payments through Juice; any cost of operating your own HTTP service
remains yours. Three additional circumstances require an available account
balance.

**Serving a remote buyer.** Your account advances the execution budget while
waiting for the remote payment. If it cannot fund the advertised price, the
call is rejected before execution. The buyer's client explains that the peer
declined the call, nothing was charged, and only that kernel's operator can
change the condition.

Maintain a working balance when selling to remote buyers, allowing for the
delay and variation in settlement. The client warns at publication or activation
if your balance is too low. When the kernel refuses a call for that reason,
it logs `call.provider_unfunded` with the action, balance, and price.

**Buying remote work within your action.** You are the immediate caller of
that remote action, so your account supplies its ticket stake in addition to
the budget allocated by the parent call. See
[The ticket](../calling/running.html#the-ticket).

**Delivering value through a child action.** If your action calls a
value-bearing action such as `sys/transfer`, the amount delivered comes from
your balance. See
[Moving money through an action](../money/funds.html#moving-money-through-an-action).

## Zero-price actions

A zero-price action needs no execution funds and can call other free actions.
It has no budget for paid downstream work. Separate value delivery still
requires the immediate caller's funds, even if the transfer action's execution
price is zero.

## Reading your earnings

`juice user ledger` shows provider payouts as settlement entries, each naming
the transaction that produced it. Use `juice tx show <id>` for the full call
record. Calls made before settlement postings were introduced remain in the
transaction history.

## Your track record

```
$ juice action show bob/echo
  …

This kernel's own calls
  3 calls, 3 succeeded  ~399ms  rating 1.00 from 1
```

The duration is the mean execution time, and the rating is the mean of the
recorded assessments. Current statistics
reset when the description, price, schemas, or source changes. Historical
transactions and ratings remain available.

Only a call's payer may rate it, once, and executing action code has no rating
authority. This does not make ratings proof of quality: an account holder can
buy and rate its own action, subject to the usual fees. Remote evidence adds a
check that the reported trade is linked to the other kernel's record. See
[Reputation across kernels](../operating/network-economy.html#reputation-across-kernels).
