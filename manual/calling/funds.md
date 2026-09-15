---
title: Funds
parent: Calling actions
nav_order: 2
---

# Funds

## Available and locked

```
$ juice user me
  available: 9.50 credits
  …
  locked: 0.00 credits
```

**Available** money can be spent. **Locked** money is committed to work that has
not finished: the price of a call in progress, the price parked for a step waiting
on somebody, and the stake held against a call to another kernel.

When you start a paid action, the kernel moves its price from available to locked.
On success that reservation pays for the work. If the call fails, the part that was
not spent returns to available. Either way, nothing stays locked once the work has
settled.

Locked money is not lost. If you see a balance locked with nothing running,
`juice process list` shows which processes still hold it, and the owner of a
process can end it to release what it holds. See
[Creating steps and managing processes](../providing/steps.html).

## Amounts

Every amount is written in the unit of the kernel's network. The three shipped
networks — `play`, `test` and `real` — all have six decimal places, so the same
number means the same amount everywhere. The command line takes and shows display
units: `0.50 credits`. The HTTP API and the JSON arguments of an action use base
units: `500000`.

## Getting money in

How credits enter depends on the kernel's network.

### On `play`

There is nothing to send. The operator credits accounts against payments received
outside the system and keeps the records.

```
$ juice user deposit
Money on the play network has no addresses to send to.
The operator of this kernel records payments here; there is nothing to send from your side.
```

`play` money is backed by nothing. It exists so the system can be used and tested
without real money.

### On `test` and `real`

Money arrives and leaves as a payment on a blockchain: USDC on Arbitrum Sepolia
for `test`, USDC on Arbitrum One for `real`. You register the address you will pay
from, send USDC to the kernel's address, and the kernel credits you once the
payment is final.

That procedure has its own chapter: [Money on a chain](chain-money.html).

## Sending money to another user

A transfer moves money between two accounts on the same kernel. It is direct, has
no fee, and is not an action call.

```
$ juice user transfer bob 1 --reason "thanks"
Send 1.00 credits to bob, acting as alice@acme? This cannot be undone. [y/N] y
  amount: 1.00 credits
  reason: thanks
  created_at: 2026-09-14T12:05:34Z
  operator_handle: alice
  from_handle: alice
  to_handle: bob
```

{: .warning }
> A transfer is final. There is no reversal and no dispute: check the handle before
> you confirm.

Transfers are local to one kernel. There is no transfer to an account on another
kernel; money crosses a kernel boundary only as payment for work. See
[The network economy](../operating/network-economy.html).

## Taking money out

Here bob, who has earned credits, withdraws some. He is acting as `bob@acme`,
either because that is the login in use or by passing `--as bob@acme`:

```
$ juice user withdraw 0.5
Withdraw 0.50 credits on play, acting as bob@acme? This cannot be undone. [y/N] y
  id: a4ff7443-…
  kind: payout
  amount: 0.50 credits
  status: confirmed
  tx_hash: manual:a4ff7443-…
$ juice user withdrawals
```

On a chain network the money is paid to the address you registered. Changing that
address never redirects a withdrawal already in flight: each one carries the
destination it was created with. See
[Withdrawing](chain-money.html#withdrawing-taking-money-out).

Every command that moves money asks for confirmation first, because none of them
can be undone. `--yes` answers in advance; use it only in scripts.

## The ledger

The ledger is the record of money entering, leaving, and moving between accounts.

```
$ juice user ledger
[2026-09-14T12:05:34Z] amount:1.00 credits  from:alice  to:bob    thanks
[2026-09-14T12:04:54Z] amount:10.00 credits from:sys    to:alice
```

Deposits, withdrawals, transfers and value delivered by an action appear here.
Payments for executing actions do not: those are transactions, listed with
`juice tx list`. The distinction is that the ledger records money moving between
account holders, while a transaction records a call and what it cost.

## Moving money through an action

Some actions deliver money as part of what they do. The standard library's
`sys/transfer` is the simplest:

```
$ juice run sys/transfer '{"target":"bob","amount":1500000}'
```

The amount is in base units, because it is part of an action's JSON input.

Two things move separately in such a call. The execution price is charged the
usual way. The value is taken from the account of whoever called the action
directly, delivered whole to the named recipient, and not taxed. The transfer is
all-or-nothing: if the call fails, nothing is delivered.

{: .warning }
> Running a value-bearing action authorises the payment its arguments name. The
> confirmation you get for `user transfer` does not apply here: the run itself is
> the consent.

An action can only deliver value if its contract declares it, which only the
kernel can do when it registers the action. No action you create can move a
caller's money.

Value delivery is local to one kernel. The recipient must be an ordinary active
account on the same kernel.
