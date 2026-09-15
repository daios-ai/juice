---
title: Funds
parent: Money
nav_order: 1
---

# Funds

Money already inside a kernel: what your balance consists of, how amounts are
written, sending money to somebody else on the same kernel, and the record of it
all. Getting money in and taking it out are the next chapter,
[Deposits and withdrawals](deposits-and-withdrawals.html).

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

`transfer` and `withdraw` ask for confirmation before they act, because neither
can be undone; `--yes` answers in advance and belongs in scripts. Running a
value-bearing action does not ask, because issuing the run is the authorisation —
see [Moving money through an action](#moving-money-through-an-action).

Transfers are local to one kernel. There is no transfer to an account on another
kernel; money crosses a kernel boundary only as payment for work. See
[The network economy](../operating/network-economy.html).

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
