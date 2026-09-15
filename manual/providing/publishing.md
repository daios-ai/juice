---
title: Publishing
parent: Providing actions
nav_order: 1
---

# Publishing

Publishing gives an existing service a Juice interface: a name, a description,
typed input and output, a price, and an audience. Registration and activation
are separate, so you can prepare that interface before allowing calls.

## Creating an action

The following example registers an HTTP endpoint that echoes a message. Its
input schema describes the `msg` field, and its price is half a credit on
`play`:

```
$ juice action create echo --kind http --source https://httpbin.org/post \
    --price 0.5 --description "Echo a message back to the caller" \
    --input-schema '{"type":"object","properties":{"msg":{"type":"string","description":"text to echo"}},"required":["msg"]}'
  id: bb7fe1a8-…
  owner_handle: bob
  name: echo
  kind: http
  active: false
  visibility: private
  price: 0.50 credits
  …
  quote_hash: 4965342976414282…
```

Choose a name that is unique among your actions. Slashes let you organize related
operations under a shared path, such as `bob/mail/send` and `bob/mail/inbox`.
The shared path also allows later changes to be applied to the group.

The `--kind` option determines where the implementation runs:

| Kind | `--source` / `--artifact` | Use |
|---|---|---|
| `http` | `--source URL` | an endpoint you already run |
| `wasm` | `--artifact FILE` | code the kernel runs in a sandbox |

For an HTTP action, `--method` selects the HTTP verb, defaulting to `POST`.
Use `--param` when input fields need explicit positions in the URL path or
query. Otherwise, matching arguments fill path placeholders and the remaining
arguments form the request body.

The kernel checks source URLs at creation and activation. By default it permits
loopback endpoints but refuses private, link-local, and reserved networks.
An operator can widen that policy. Redirects remain subject to the same checks,
including redirects from an initially permitted loopback endpoint.

## Description and schemas are the contract

The description and schemas serve both people and software. A buyer uses them
to judge whether the action suits a task; search uses the description to find
it; and an agent uses the input schema to construct arguments. Explain what
the service does and describe each field sufficiently for someone unfamiliar
with your implementation to use it.

Activation requires a nonempty description and valid schemas. The input schema
is checked before funds are reserved, and the output schema before the provider
is paid. A result that violates the output schema causes a failed call.

## Enabling and choosing an audience

A new action is inactive and private. Enable it to permit execution, then choose
who may call it by setting its visibility:

```
$ juice action enable bob/echo
enabled bob/echo
$ juice action update bob/echo --visibility local
  …
  visibility: local
```

| Visibility | Who can call it |
|---|---|
| `private` | you only |
| `local` | accounts on this kernel |
| `public` | anyone, including other kernels |

For an eligible action, `public` makes it available through federation without
a separate listing or approval procedure. Actions using callers' delegated
credentials remain local, as explained in the web API chapter.

Visibility and activity can be changed independently. Disabling temporarily
prevents all calls while retaining the chosen audience for a later reactivation.

## Changing terms

An update can affect the interface, the implementation, or the conditions under
which the action is used. Changing its price, either schema, or source
deactivates it, resets current statistics, and revokes delegated grants.
Reactivation is then an explicit step, and callers must renew any required
consent:

```
$ juice action update bob/echo --price 0.75
$ juice action enable bob/echo
```

Changing only the description resets statistics but preserves activity and
grants. Changing visibility preserves both activity and statistics. Callers
who pinned a previous quote must read and accept changed terms before running
again. See
[Pinning the terms you saw](../calling/running.html#pinning-the-terms-you-saw).

## Retiring an action

```
$ juice action disable bob/echo
$ juice action delete bob/echo
```

Use `disable` when you may want to offer the action again, and `delete` to
retire it. Retirement removes it from use and listings while preserving its
transactions, receipts, and ratings. Historical transactions retain the action
name needed to interpret them.

## Acting on a whole path

The mutation commands accept an action ID for one action, or an `owner/path`
reference for the action at that path and its descendants. For example,
enabling `bob/greeter` can enable both operations in an application:

```
$ juice action enable bob/greeter
enabled bob/greeter/greet
enabled bob/greeter/index
```

Price, visibility, and credentials can be updated across the selected path.
Description, schema, and source changes require a selection resolving to one
action, since those fields describe a particular service interface.

## Groups and the index convention

The `index` convention gives a group an entry point. If no action is named
`bob/greeter`, a caller using that reference reaches `bob/greeter/index`;
similarly, `bob` can reach `bob/index`.

Implement this entry point as an action that describes the group. It has the
same price, execution, and rating rules as any other action. The convention
therefore provides a common name for related services without requiring a
separate application interface.

## Upstream credentials you hold

When an upstream service uses your provider account, attach its credential
to the action. The kernel will apply it when sending requests to the endpoint:

```
$ juice action create weather --kind http --source https://api.example.com/v1/forecast \
    --price 1 --description "Forecast for a city" \
    --input-schema '…' \
    --auth '{"scheme":"bearer","secrets":{"token":"…"}}'
```

Supported schemes include `header`, `query`, `bearer`, `basic`, OAuth client
credentials, and JWT bearer. Credentials are encrypted in storage and excluded
from readable action details, call data, logs, and receipts. An action read
reports the scheme and whether a caller grant is required. Replacing the
credential revokes associated grants but does not deactivate the action.

If instead each caller must use *their own* account on the upstream service, see
[Wrapping a web API](web-apis.html#credentials-each-caller-holds).
