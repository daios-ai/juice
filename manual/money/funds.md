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
is reserved for existing commitments: running calls, tasks awaiting input, and
stakes for calls to other kernels.

Starting a paid action moves its price from available to locked. Successful
settlement pays for the work from that reservation; failure returns the portion
that was not consumed. A task can keep funds reserved after the creating action
has returned, because its future execution still needs a budget.

If funds remain locked, inspect `juice process list` to find outstanding work.
The process owner can end abandoned work and recover unused reservations,
although a remote call awaiting a receipt is normally best left to settle.
See [Tasks and processes](../providing/tasks.html) before forcing closure.

## Amounts

The command line accepts amounts in the network's display unit, such as
`0.50 fUSD` on `play`. The HTTP API and action JSON use integer base units.
All three shipped networks use six decimal places, so `500000` base units
represent 0.50 fUSD on `play`. Equal numeric amounts on different networks do
not imply equal monetary value.

## Sending money to another user

`user transfer` runs your kernel's own `sys/transfer` action. The recipient
may be a user of your kernel or of another; the command is the same. It
debits the amount and the action's price from your balance, and the recipient
receives exactly the amount:

```
$ juice user transfer bob@acme 1
Send 1.00 fUSD to bob@acme, for a price of 0.00 fUSD, acting as alice@acme? This cannot be undone. [y/N] y
```

A recipient on your kernel is credited when the call settles. A recipient on
another kernel is credited when your kernel's payment reaches that kernel:
your kernel pays the recipient's kernel through the rail and then tells it
whom the payment is for. `juice tx show` on the transfer's transaction reports
that payment's `payment` status; it cannot report the credit, which the other
kernel makes.

`--external-key KEY` names the transfer. Repeating the command with the same
key, for example after a lost reply, returns the first transfer's outcome and
moves no money; the same key with another recipient or amount is refused.

{: .warning }
> A transfer is final. There is no reversal and no dispute: check the handle before
> you confirm.

Both `transfer` and `withdraw` ask you to confirm the movement. An unattended
program supplies `--yes` to give that confirmation in advance. A value-bearing
action uses different consent semantics, described under
[Moving money through an action](#moving-money-through-an-action).

A recipient on another kernel is resolved there before anything is charged;
an unknown recipient costs nothing.

## The ledger

The account ledger records money entering, leaving, and moving between accounts.
Use `user ledger` to read entries involving your account:

```
$ juice user ledger
WHEN                  AMOUNT       FROM   TO     WHY
2026-09-14T12:05:54Z  0.40 fUSD   alice  bob    e989c5e1-…
2026-09-14T12:05:54Z  0.10 fUSD   alice  sys    e989c5e1-…
2026-09-14T12:05:34Z  1.00 fUSD   alice  bob    3f0a91c2-…
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
$ juice run sys@acme/transfer '{"target":"bob@acme","amount":1500000}'
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
action or use composition to debit the funding user's balance. The recipient
must be an ordinary, unsuspended account, on this kernel or on another.
