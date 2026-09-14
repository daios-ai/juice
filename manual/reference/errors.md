---
title: Errors
parent: Reference
nav_order: 3
---

# Errors

An error reaches you in two forms. From the command line it is a line of prose on
stderr plus an exit code; `--json` does not change that. Over HTTP it is a JSON
body carrying a stable `code`, a message, and sometimes `meta`.

Branch on the code or the exit status, never on the message.

| Code | Exit | HTTP | Meaning and what to do |
|---|---|---|---|
| `unauthenticated` | 2 | 401 | No valid session, or the account is suspended. Log in again; if it persists, the account is suspended and the operator must lift it. |
| `unauthorized` | 3 | 403 | Authenticated, but not permitted. You are not the owner, the payer, or the named party. |
| `not_found` | 4 | 404 | No such action, transaction, step or process — or you may not see it. A reference that names nothing and has no `index` child lands here. |
| `invalid_input` | 5 | 422 | The request is malformed: a bad handle, a duplicate name, a non-positive amount. Nothing happened. |
| `schema_violation` | 5 | 422 | Arguments did not match the action's input schema, or its output did not match the output schema. Input is checked before any charge. |
| `insufficient_funds` | 6 | 402 | Not enough available balance. For a cross-kernel call you need the price **and** the stake. |
| `timeout` | 7 | 504 | The call may have executed. Do not re-run; find out what happened. |
| `grant_required` | 8 | 403 | You have not connected the upstream account this action needs. `meta.action` names what to connect. Nothing was charged and no failure was recorded against the action. |
| `peer_unreachable` | 9 | 502 | The call provably never left your kernel and was fully refunded. Safe to retry. `meta.peer` names the kernel. |
| `peer_unfunded` | 10 | 402 | The other kernel will not serve on credit: either it has lent your kernel as much as it allows, or its provider cannot fund the work. `meta.peer` names it. |
| `terms_changed` | 11 | 409 | The pinned contract no longer matches. Nothing was charged; the message carries the current price and hash. |
| `invalid_state` | 1 | 409 | The operation does not apply here: completing a step that is not waiting, using a model that is not configured, editing a cached remote action. |
| `execution_failed` | 1 | 500 | The action ran and failed. What was not consumed is refunded. |
| `rail_stopped` | 1 | 503 | Outgoing payments are halted. Deposits, execution and reads continue. The operator clears it. |
| `internal` | 1 | 500 | A fault in the kernel. |

## meta

| Field | On | Contains |
|---|---|---|
| `action` | `grant_required` | the action reference to connect |
| `peer` | `peer_unreachable`, `peer_unfunded`, `unauthorized` from a peer | the petname if one is bound, otherwise the public key |
| `process_id` | a parked run | the process to follow |
| `pending_since` | a parked run | when the call was dispatched |
| `refund_eligible_at` | a parked run | when it will settle if nothing arrives; eligibility, not a settlement time |

## What is never in an error

A failure reason names the class of failure only. It never carries an upstream URL,
a response body, a query, or any internal detail. A reason a peer sent is never
adopted as your kernel's own. Between kernels only the code and a short message
cross.

A refused or re-quoted call never discloses the terms of an action you may not see.
The access check runs before the contract check, so a wrong guess about a private
action returns `not_found`, not its price.

## Rate limiting

Authentication and account creation are rate limited per client and answer `429`
when exceeded. Genuine loopback traffic is exempt. Traffic between kernels is
limited at its own transport.
