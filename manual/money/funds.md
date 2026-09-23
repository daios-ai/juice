---
title: Funds
parent: Money
nav_order: 1
---

# Funds

Once money has reached your account, you can spend it on actions or transfer it
to another user of the same kernel. This chapter explains how the balance
reflects those commitments and where their records appear. The next chapter,
[Deposits and withdrawals](deposits-and-withdrawals.html), covers payments
between your account and the outside world.

## Available and locked

```
$ juice user me
  available: 9.50 fUSD
  …
  locked: 0.00 fUSD
```

The **available** balance is the amount you can spend. The **locked** balance
is reserved for existing commitments: running calls, steps awaiting input, and
stakes for calls to other kernels.

Starting a paid action moves its price from available to locked. Successful
settlement pays for the work from that reservation; failure returns the portion
that was not consumed. A step can keep funds reserved after the creating action
has returned, because its future execution still needs a budget.

If funds remain locked, inspect `juice process list` to find outstanding work.
The process owner can end abandoned work and recover unused reservations,
although a remote call awaiting a receipt is normally best left to settle.
See [Steps and processes](../providing/steps.html) before forcing closure.

## Amounts

The command line accepts amounts in the network's display unit, such as
`0.50 fUSD` on `play`. The HTTP API and action JSON use integer base units.
All three shipped networks use six decimal places, so `500000` base units
represent 0.50 fUSD on `play`. Equal numeric amounts on different networks do
not imply equal monetary value.

## Sending money to another user

A local transfer debits your available balance and credits the recipient by the
same amount. It has no fee and creates a ledger entry rather than an execution
transaction:

```
$ juice user transfer bob 1 --reason "thanks"
Send 1.00 fUSD to bob, acting as alice@acme? This cannot be undone. [y/N] y
  amount: 1.00 fUSD
  reason: thanks
  created_at: 2026-09-14T12:05:34Z
  operator_handle: alice
  from_handle: alice
  to_handle: bob
```

{: .warning }
> A transfer is final. There is no reversal and no dispute: check the handle before
> you confirm.

Both `transfer` and `withdraw` ask you to confirm the movement. An unattended
program supplies `--yes` to give that confirmation in advance. A value-bearing
action uses different consent semantics, described under
[Moving money through an action](#moving-money-through-an-action).

The recipient must be on the same kernel. Payments between kernels arise from
service purchases and follow the settlement procedure in
[The network economy](../operating/network-economy.html).

## The ledger

The account ledger records money entering, leaving, and moving between accounts.
Use `user ledger` to read entries involving your account:

```
$ juice user ledger
WHEN                  AMOUNT       FROM   TO     WHY
2026-09-14T12:05:54Z  0.40 fUSD   alice  bob    e989c5e1-…
2026-09-14T12:05:54Z  0.10 fUSD   alice  sys    e989c5e1-…
2026-09-14T12:05:34Z  1.00 fUSD   alice  bob    thanks
2026-09-14T12:04:54Z  10.00 fUSD  sys    alice
```

Deposits, withdrawals, transfers, and value delivered by an action appear in
this list, along with settlement postings: provider payouts, operator fees,
import fees, and obligations returned to another account during a composed
remote call. Each settlement entry names its transaction in `WHY`. A missing
source or destination is shown as `outside`.

Use `juice tx show` with that transaction ID to read the work behind a posting.
Older calls made before settlement postings were introduced remain in the
transaction history; they are not backfilled into the ledger.

## Moving money through an action

An action may deliver money in addition to charging for its execution. The
built-in `sys/transfer` illustrates the distinction:

```
$ juice run sys/transfer '{"target":"bob","amount":1500000}'
```

The amount is in base units, because it is part of an action's JSON input.

The execution price follows ordinary call accounting. The amount to deliver is
reserved separately from the immediate caller's account and transferred whole
to the recipient on success, without tax. Failure returns that reservation.
If a composing action calls `sys/transfer`, the immediate caller is the
composing action's owner, so the value comes from that owner's balance.

{: .warning }
> Running a value-bearing action authorises the payment its arguments name. The
> confirmation you get for `user transfer` does not apply here: the run itself is
> the consent.

Only the kernel can register an action with the contract declaration that
authorizes value delivery. A provider cannot add that declaration to a custom
action or use composition to debit the funding user's balance. Delivery is
local: the recipient must be an ordinary, unsuspended account on the same kernel.
