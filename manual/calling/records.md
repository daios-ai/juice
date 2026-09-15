---
title: Records, receipts and ratings
parent: Calling actions
nav_order: 6
---

# Records, receipts and ratings

Every attempted call writes one transaction and one signed receipt. Neither
changes afterwards.

## Transactions

```
$ juice tx list
$ juice tx show 116fd3a6-…
  id: 116fd3a6-…
  process_id: 25386daa-…
  trace_id: 29dde7d6-…
  parent_trace_id:
  action_id: bb7fe1a8-…
  action_name: echo
  args: { "msg": "hello" }
  result: { … }
  status: success
  gross: 0.50 credits
  net: 0.40 credits
  fee: 0.10 credits
  refund: 0.00 credits
  reason:
  started_at: 2026-09-14T12:05:10Z
  ended_at: 2026-09-14T12:05:10Z
  rating: null
  owner_handle: alice
  caller_handle: alice
  target_handle: bob
```

The three handles are the three roles a call records. `owner_handle` is the payer,
who owns the process the call ran in. `caller_handle` is whoever asked for this
particular call, which is the payer for a call you ran yourself and the composing
action's owner for a call made inside another action. `target_handle` is the
action's owner, who is paid.

The amounts: `gross` is what was set aside for the call, `net` what the provider
received, `fee` what the kernel took, and `refund` what came back to the caller.
On a failure `reason` names the class of failure. It never contains an upstream
URL, response body, or internal detail.

A transaction is readable by its payer, its caller and its payee, and by the
operator. It keeps the action's name even if the action is later deleted, so
history stays readable.

## Receipts

A receipt is the signed form of the transaction, issued by the kernel that
executed the call.

```
$ juice tx verify 116fd3a6-…
  transaction_id: 116fd3a6-…
  valid: true
  checks: {
    "action_id": true,
    "args_hash": true,
    "charge": true,
    "reply_hash": true,
    "settlement": true,
    "signature": true,
    "status": true
  }
  receipt: {
    "id": "15f83c40-…",
    "tx_id": "116fd3a6-…",
    "args_hash": "f7583f4a…",
    "reply_hash": "bae6b57e…",
    "status": "success",
    "gross": 500000,
    "net": 400000,
    "fee": 100000,
    "charge": 500000,
    "started_at": "2026-09-14T12:05:10Z",
    "created_at": "2026-09-14T12:05:10Z",
    "signature": "5yaZ7aWl6K0M…"
  }
```

Amounts in the receipt are base units, because the receipt is the signed artifact
rather than a rendering of it.

You can verify a receipt offline. It needs the stored receipt and the signing key
of the kernel that issued it, not a connection to anybody. Only the checks that apply are
reported.

For a call to another kernel, the receipt was signed by that kernel and is stored
with your transaction, so it stays verifiable even if the other kernel later
deletes its own copy:

```
$ juice tx verify 6cd9f6b6-…
  transaction_id: 6cd9f6b6-…
  valid: true
  remote_kernel_handle: beta-kernel
  remote_kernel_public_key: hqDr8oMX…
  checks: {
    "action_id": true,
    "args_hash": true,
    "charge": true,
    "charge_ceiling": true,
    "draw": true,
    "premium": true,
    "receipt_hash": true,
    "refund_conservation": true,
    "reply_hash": true,
    "settlement_arith": true,
    "signature": true,
    "status": true
  }
```

The extra checks confirm what a cross-kernel call adds: that the charge did not
exceed the quoted ceiling, that the markup and import fee follow the rates the
call was dispatched under, that the refund adds up, and that the settlement draw
matches the commitment both sides recorded.

A signature your kernel's network does not accept is reported invalid. It is never
re-signed.

## Rating

Only the account that paid for a call can rate it, once.

```
$ juice tx rate 116fd3a6-… 1 --note "did what it said"
  id: e599904f-…
  rated_tx_id: 116fd3a6-…
  rating: 1
  note: did what it said
  created_at: 2026-09-14T12:05:17Z
  signature: rx8TNj531MkA…
```

The value is `1` or `0`. The note is optional and at most 1024 bytes. A rating is
permanent: it cannot be changed or withdrawn, and it never alters what was paid.

Ratings are public wherever the action is visible, shown without the rater's
identity:

```
$ juice action ratings bob/echo
1  2026-09-14T12:05:17Z  did what it said
```

The code that executes an action can never rate anything, and rating never runs
through the call machinery. Reputation cannot be manufactured by the thing it
judges.

## Processes

A process is the wallet of one `run`. It closes by itself once the call has
returned, no step is waiting, and no call is awaiting a receipt from another
kernel.

```
$ juice process list
e3539f75-…  open    available:0.00 credits  locked:0.00 credits
25386daa-…  closed  available:0.00 credits  locked:0.00 credits
$ juice process show e3539f75-…
```

A process that stays open is holding money. The two reasons are a step waiting on
somebody and a call parked awaiting a receipt; a listed process says which by
carrying `awaiting_receipt` and, when set, `awaiting_receipt_since`. A process held
open only by a step can be ended by its owner, which cancels the waiting steps and
returns their money. One awaiting a receipt is better left to settle on its own.
See [Ending a process](../providing/steps.html#ending-a-process).

## What you can reconstruct

As a buyer, `tx list` and `user ledger` together account for every credit that
left your balance: transactions for work you bought, the ledger for deposits,
withdrawals, transfers and value delivered to you or by you.

As a provider, every credit that reached you is reconstructible from the
transactions you are party to, which you may read because you are the payee.
