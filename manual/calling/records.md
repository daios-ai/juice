---
title: Records, receipts and ratings
parent: Calling actions
nav_order: 4
---

# Records, receipts and ratings

When a call settles, the kernel records its outcome and payment in a transaction
and issues a signed receipt. These records are permanent. They let the parties
inspect what happened, verify the charge, and relate a rating to the call that
produced it.

## Transactions

Use `tx list` to find calls you are entitled to read and `tx show` to inspect one
in detail:

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

The three handle fields distinguish the participants. `owner_handle` names the
owner of the process that funded the work; `caller_handle` names the immediate
requester; and `target_handle` names the action's owner. For a call you start
with `run`, you are both process owner and requester. If that action calls
another, the original process owner stays the same, while the requester of the
child call is the composing action's owner.

For a local call, `gross` is the allocation, `net` is the provider's payment,
`fee` is the kernel's fee, and `refund` is the amount returned to the funding
budget. On failure, `reason` identifies the failure class without exposing an
upstream URL, response body, or internal error detail. Remote settlement uses
the additional amounts explained below.

The process owner, requester, action owner, and operator may read the
transaction. Its captured action name remains available even after the action
is retired, so the history can still be interpreted.

## Receipts

A receipt records the call's outcome and charge under the kernel's signature.
It contains hashes of the arguments and result, allowing those values to be
checked without including their full contents in the receipt. Use `tx verify`
to inspect the verification result:

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

Receipt amounts are integer base units. Verification uses the stored record
and the issuing kernel's public key; it does not require contact with the
issuer. The command still uses your local kernel's API to retrieve and check
those records, and reports only the checks relevant to the call.

For a remote call, your kernel retains the remote receipt and its signing key
with the transaction. It can therefore verify the charge after losing contact
with the peer or removing that peer from its local roster:

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

Remote verification checks the receipt against the dispatched terms, including
the charge ceiling, markup, import fee, and refund. It also checks that the
ticket's payment agrees with the recorded draw. A signature that is invalid
under your kernel's network remains invalid; verification does not replace it
with a new signature.

## Rating

The payer may submit one assessment of a completed call. Use its transaction
identifier to associate the rating with the work you purchased:

```
$ juice tx rate 116fd3a6-… 1 --note "did what it said"
  id: e599904f-…
  rated_tx_id: 116fd3a6-…
  rating: 1
  note: did what it said
  created_at: 2026-09-14T12:05:17Z
  signature: rx8TNj531MkA…
```

The rating is `1` for a positive assessment or `0` for a negative one. A note
may add context, up to 1024 bytes. Once submitted, the rating cannot be changed
or withdrawn and has no effect on the payment. Readers who can see the action
can also see its ratings, without the payer's identity:

```
$ juice action ratings bob/echo
1  2026-09-14T12:05:17Z  did what it said
```

Rating belongs to the account's supervisory interface. Code executing inside
an action has no authority to submit ratings through its execution capability.

## Processes

A process groups the work and reserved funds of one `run`. It closes
automatically after all calls have settled and no steps remain outstanding.
Use the process commands to follow work that has not yet finished:

```
$ juice process list
e3539f75-…  open    available:0.00 credits  locked:0.00 credits
25386daa-…  closed  available:0.00 credits  locked:0.00 credits
$ juice process show e3539f75-…
```

A process may remain open while a step waits for input or a remote call awaits
a receipt. The fields `awaiting_receipt` and `awaiting_receipt_since` identify
the latter condition and its age. An open process can have a zero balance when
its outstanding work is free.

The owner can end abandoned work to cancel waiting steps and recover their
reserved funds. A process awaiting a remote receipt should normally be allowed
to settle through the retry mechanism. See
[Ending a process](../providing/steps.html#ending-a-process) for the consequences
of forced closure.

## What you can reconstruct

Use call transactions together with the account ledger to follow your balance.
Transactions explain execution charges, while the ledger records deposits,
withdrawals, transfers, and delivered value. For a provider, readable call
records identify the work behind its earnings; deposits and other account
movements remain visible in the ledger.
