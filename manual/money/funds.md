---
title: Funds
parent: Money
nav_order: 1
---

# Funds

Once money has reached your account, you can spend it on actions or transfer it
to another user in the same network, whether on your kernel or another. This
chapter explains how the balance reflects those commitments and where their
records appear. The next chapter,
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
The process owner can end abandoned work and recover unused reservations.
A process awaiting a remote receipt cannot be closed until the call settles.
See [Tasks and processes](../providing/tasks.html) for the conditions of closure.

## Amounts

The command line accepts amounts in the network's display unit, such as
`0.50 fUSD` on `play`. The HTTP API and action JSON use integer base units.
All four shipped networks use six decimal places, so `500000` base units
represent 0.50 fUSD on `play`. Equal numeric amounts on different networks do
not imply equal monetary value.

## What an action costs

When an action on your own kernel succeeds, you pay its advertised price. The
action can spend some of that price on work from other actions. What remains
is its provider's **margin**. The kernel takes a **local execution fee** from
that margin to pay for its equipment and operation; the provider receives
the rest. The fee is already inside the price you see.

The quoted price for an action on another kernel also includes an **export
fee** (also called the serving markup) and an **import fee**. The serving
provider advances the money to execute the action. The export fee compensates
it for the risk of being paid later, or not at all. Your kernel's operator
keeps the import fee to help pay the costs of using the rail, the external
payment system between kernels. On chain networks, these costs include
transaction fees. The import fee is collected on successful calls even when
one makes no rail payment; it is not the cost of one particular transfer.

| Fee | Who receives it | Setting | Default |
|---|---|---|---:|
| Local execution fee, on each provider's margin | The operator of the kernel doing the work | `fee_bps` | 20% |
| Export fee, on the action's price | The remote provider advancing the work | `remote_bps` | 5% |
| Import fee, on the price including the export fee | Your kernel's operator | `import_bps` | 5% |

The operator of the serving kernel sets its execution and export rates. Your
kernel's operator sets its import rate and ticket face value. The fee settings
use **basis points**: `100` means 1%, `500` means 5%, and `2000` means 20%.
Fees are rounded up to the nearest base unit. The keys and instructions for
changing them are in [Configuration](../reference/config.html#money).

Suppose an action costs `0.10`, calls no other actions, and succeeds. On its
own kernel, the buyer pays `0.10`. The local execution fee is `0.02`, leaving
`0.08` for the provider. When the buyer uses another kernel, that kernel
applies the same execution fee to the provider's price. It is already included
in `0.10`. The buyer's remote price adds the export and import fees:

| Part of the remote price | Amount |
|---|---:|
| Action price | 0.10 |
| Export fee: 5% of 0.10 | 0.005 |
| Amount owed to the remote provider | 0.105 |
| Import fee: 5% of 0.105 | 0.00525 |
| **Price shown to the buyer** | **0.11025** |

The `0.11025` is the successful call's **average cost**, not necessarily what
leaves your balance on that one call. Small amounts are settled with a
**ticket** so the kernels need not make a rail payment for every purchase.
The ticket has a **face value**, set by your kernel's ticket size (`lottery`). For
this example, take its default of `1.00`. The remote provider is owed `0.105`,
so a draw pays the provider `1.00` on 10.5% of successful calls and pays
nothing on the other 89.5%. Both kernels contribute to the draw; neither can
choose its result.

| Result | Rail payment | Import fee | **What you pay** |
|---|---:|---:|---:|
| No payment drawn (89.5% chance) | 0 | 0.00525 | **0.00525** |
| Ticket pays (10.5% chance) | 1.00 | 0.00525 | **1.00525** |
| Expected amount per call | 0.105 | 0.00525 | **0.11025** |

The expected cost is `89.5% × 0.00525 + 10.5% × 1.00525 = 0.11025`.
Individual calls can cost more than the price you approved when their ticket
pays. The `charge` in a `run` reply records what was charged when the call
finished: `0.00525` or `1.00525` in this example.

Before the call, you need both the `0.11025` execution budget and a `1.00`
**stake** available, `1.11025` in all. The stake holds enough to pay a winning
ticket. These are reservations, not two charges: settlement returns the unused
budget and releases the stake, then reserves any payment the draw requires.
If a rail payment fails, the money remains committed and the kernel retries it.

When the amount owed is at least the ticket's face value, the kernel pays the
exact amount instead of drawing. A paid call still needs its stake available
before it starts, because it might fail after some work and owe less than the
face value. Setting `lottery` to zero pays every amount exactly and requires
no stake: this example would then cost exactly `0.11025`. The serving kernel
also limits the ticket sizes it accepts; operators set that limit in
[Configuration](../reference/config.html#money).

This example assumes success. On failure, completed work bought from other
actions can still cost money; the export fee applies to that work, but
the import fee is zero. A refusal before execution costs nothing. If the
other kernel has not returned a signed answer, the budget and stake remain
reserved while the call waits. See [Running an action](../calling/running.html)
for how to follow it.

### What the serving provider receives

The provider puts up the `0.10` execution budget. Its kernel later credits
`0.08` to the provider and takes `0.02` as the local execution fee. The whole
ticket payment, when there is one, goes to the provider. Ticket payments bring
in `0.105` per call on average. That leaves an expected net of
`0.08 − 0.10 + 0.105 = 0.085`: the `0.08` it would earn from a local buyer
plus the `0.005` export fee. On a losing draw, the provider receives no rail
payment. The export fee compensates for that risk over many calls; it does
not guarantee payment for any one call.

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
WHEN                  AMOUNT      FROM        TO          WHY
2026-09-14T12:05:54Z  0.40 fUSD   alice@acme  bob@acme    e989c5e1d21a
2026-09-14T12:05:54Z  0.10 fUSD   alice@acme  sys@acme    e989c5e1d21a
2026-09-14T12:05:34Z  1.00 fUSD   alice@acme  bob@acme    3f0a91c2e842
2026-09-14T12:04:54Z  10.00 fUSD  sys@acme    alice@acme
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
