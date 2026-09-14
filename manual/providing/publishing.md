---
title: Publishing
parent: Providing actions
nav_order: 1
---

# Publishing

## Creating an action

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

The name is yours to choose and must be unique among your own actions. It may
contain `/`, which is how a group of related actions is laid out: `bob/mail/send`
and `bob/mail/inbox` are two actions, not a folder and its contents.

`--kind` says how the action executes:

| Kind | `--source` / `--artifact` | Use |
|---|---|---|
| `http` | `--source URL` | an endpoint you already run |
| `wasm` | `--artifact FILE` | code the kernel runs in a sandbox |

For an `http` action, `--method` sets the verb (default `POST`) and `--param`
binds individual fields to path or query positions. Without bindings, path
placeholders are filled by name and the rest of the arguments become the body.

The source URL is checked when the action is created and again when it is enabled.
Private, link-local and reserved addresses are refused unless the operator has
allowed them; loopback is always permitted. A loopback URL that redirects to a
private address is still refused.

## Description and schemas are the contract

An action cannot be enabled without a description and valid schemas, and the
descriptions must be good enough to select on. They are what a buyer reads, what
search ranks, and what a language model is given when it proposes arguments. Give
every field a description, not just the action.

The output schema is checked against what the action returns. Output that does not
match is a failure and is not paid for.

## Enabling and choosing an audience

A new action is inactive and private. Two separate acts widen it.

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

`public` is all that publishing to the network requires. There is no registration,
no listing step and no approval.

An inactive action cannot be called by anyone, whatever its visibility. Disabling
is the reversible switch; visibility is the audience.

## Changing terms

You may change an action's terms at any time. No buyer ever pays under terms they
did not see.

Changing the price, a schema, or the source deactivates the action, resets its
current statistics, and revokes the consents callers had given it. Enable it again
when you are ready. This is deliberate: a consent must never survive into a
contract nobody agreed to.

```
$ juice action update bob/echo --price 0.75
$ juice action enable bob/echo
```

Changing the description resets statistics without deactivating. Changing the
visibility does neither.

A buyer who pinned the old terms is refused with the new price rather than being
charged silently. See
[Pinning the terms you saw](../calling/running.html#pinning-the-terms-you-saw).

## Retiring an action

```
$ juice action disable bob/echo
$ juice action delete bob/echo
```

Deleting is soft: the action stops being callable and stops being listed, and every
transaction, receipt and rating it ever produced survives. Transactions keep the
action's name, so history stays readable after the action is gone.

## Acting on a whole path

`enable`, `disable`, `delete` and `update` take either one action or a path. A path
applies to that action and everything beneath it:

```
$ juice action enable bob/greeter
enabled bob/greeter/greet
enabled bob/greeter/index
```

Price, visibility and upstream credentials can be set across a subtree this way.
A description, a schema or a source must name a single action, because those belong
to one contract.

## Groups and the index convention

A reference that names no action resolves to that path's `index` child. So
`bob/greeter` reaches `bob/greeter/index`, and `bob` reaches `bob/index`.

Publish an action named `index` at the root of a group to give the group a front
door: a caller who names the group gets a description of what it is, and the group
is bought and rated like any other action. There is no separate group object.

## Upstream credentials you hold

If your action calls a service where you hold the key, attach the credential to
the action:

```
$ juice action create weather --kind http --source https://api.example.com/v1/forecast \
    --price 1 --description "Forecast for a city" \
    --input-schema '…' \
    --auth '{"scheme":"bearer","secrets":{"token":"…"}}'
```

The schemes are `header`, `query`, `bearer`, `basic`, OAuth client credentials, and
JWT bearer. The credential is encrypted at rest and applied when the action is
dispatched. It never appears in inputs, outputs, logs, receipts, or any read path,
and reading the action shows only which scheme is in use.

Replacing a credential revokes the consents given to the action without
deactivating it.

If instead each caller must use *their own* account on the upstream service, see
[Wrapping a web API](web-apis.html#credentials-each-caller-holds).
