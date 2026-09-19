---
title: Errors
parent: Reference
nav_order: 3
---

# Errors

The command line reports errors as messages on stderr with an exit status,
including when `--json` is selected. HTTP errors contain a JSON body with a
stable `code`, a message, and any relevant `meta` fields. Programs should
classify errors by status or code; message wording is intended for people.

The client prints one error line, followed by a remedy when one is available.
Declining a confirmation prints `cancelled` once, without an `error:` prefix;
the exit status is still nonzero.

| Code | Exit | HTTP | Meaning and what to do |
|---|---|---|---|
| `unauthenticated` | 2 | 401 | No valid session, or the account is suspended. Check the credentials or refresh the session; contact the operator if suspension is reported. |
| `unauthorized` | 3 | 403 | Authenticated, but not permitted. You are not the owner, the payer, or the named party. |
| `not_found` | 4 | 404 | No such action, transaction, step or process — or you may not see it. A reference that names nothing and has no `index` child lands here. |
| `invalid_input` | 5 | 422 | The request is malformed: a bad handle, a duplicate name, a non-positive amount. Nothing happened. |
| `schema_violation` | 5 | 422 | Arguments did not match the action's input schema, or its output did not match the output schema. Input is checked before any charge. |
| `insufficient_funds` | 6 | 402 | Not enough available balance. For a cross-kernel call you need the price **and** the stake. |
| `timeout` | 7 | 504 | The call may have executed. Do not re-run; find out what happened. |
| `grant_required` | 8 | 403 | You have not connected the upstream account this action needs. `meta.action` names what to connect. Nothing was charged and no failure was recorded against the action. |
| `peer_unreachable` | 9 | 502 | The client cannot reach a kernel, or a federated call provably never reached its peer and was fully refunded. Exit 9 also applies to an unreachable local server. `meta.kernel` names a registered server; `meta.peer` names a federation peer. |
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
| `kernel` | an unreachable registered server | its name in the client |
| `tx_id` | a settled execution failure | the committed transaction |
| `charge` | a settled execution failure | the amount drawn, in base units, encoded as a string |
| `process_id` | a parked run | the process to follow |
| `pending_since` | a parked run | when the call was dispatched |

## What is never in an error

A transaction's stored failure reason identifies the failure class. It excludes
upstream URLs, response bodies, database queries, and other internal details.
Remote error information is likewise limited, and a peer's reason is not adopted
as the local transaction's reason.

Access checks precede quote checks. A caller without access to a private action
therefore cannot use a mismatched quote to discover that action's terms.

## Rate limiting

Authentication and account creation may return HTTP `429` when the client's
rate limit is exceeded. Direct loopback requests are exempt; forwarded
requests arriving through a loopback proxy are still subject to the limit.
Federation applies its own transport limits.
