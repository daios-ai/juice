---
title: Wrapping a web API
parent: Providing actions
nav_order: 5
---

# Wrapping a web API

An OpenAPI document can be installed as a set of actions in one command: one action
per operation, under one name you choose.

You need the document at a URL the kernel can fetch. The example below uses a
document served at `https://greeter.example.com/openapi.json`, describing two
operations, which is the smallest document the import accepts:

```json
{
  "openapi": "3.0.0",
  "info": {"title": "Greeter", "version": "1"},
  "servers": [{"url": "https://greeter.example.com"}],
  "paths": {
    "/": {"post": {
      "operationId": "index",
      "description": "What this application does",
      "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {}}}}},
      "responses": {"200": {"content": {"application/json": {"schema": {
        "type": "object", "properties": {"about": {"type": "string", "description": "description of the service"}}}}}}}
    }},
    "/greet": {"post": {
      "operationId": "greet",
      "description": "Greet someone by name",
      "x-juice-price": 500000,
      "requestBody": {"content": {"application/json": {"schema": {
        "type": "object", "properties": {"name": {"type": "string", "description": "who to greet"}}}}}},
      "responses": {"200": {"content": {"application/json": {"schema": {
        "type": "object", "properties": {"message": {"type": "string", "description": "the greeting"}}}}}}}
    }}
  }
}
```

`x-juice-price` is in base units. Install it:

```
$ juice action import greeter https://greeter.example.com/openapi.json
imported greeter/greet
imported greeter/index
greeter: 2 imported.
```

The name is the application's identity. One name holds one document. Installing a
second document under a name already in use is refused; installing the same
document under two names gives two independent applications.

As with any action, the imported rows start inactive and private:

```
$ juice action enable bob/greeter
enabled bob/greeter/greet
enabled bob/greeter/index
```

## What the document must declare

An operation is imported only if it gives all of these:

- an `operationId`, or an `x-juice-name`;
- a description or a summary;
- parameters, a request body schema, or both;
- exactly one unambiguous 2xx JSON response schema;
- an `x-juice-price` that is a non-negative integer, if it declares a price at all.

Methods `GET`, `POST`, `PUT`, `PATCH` and `DELETE` are supported. Operations that
are not JSON, that stream, that use multipart, that have an ambiguous success
schema, or that use an unsupported authentication scheme are installed but cannot
be activated.

The path and query parameters and the body schema are compiled into one input
schema. The chosen 2xx response becomes the output schema.

## The root of an application

An operation keyed `index` becomes `NAME/index`, which is what the bare name
`bob/greeter` resolves to. A document with no such operation installs fine, but the
group has no front door: callers must name an operation.

To give an application a root, name one operation `index`, either with
`operationId: index` or with `x-juice-name: index`.

```
$ juice action show bob/greeter
  id: 31004d30-…
  name: greeter/index
  kind: http
  description: What this application does
  …
```

## Re-importing

Re-run the import to take up a changed document. The name alone is enough; the URL
is remembered.

```
$ juice action import greeter
unchanged greeter/greet
unchanged greeter/index
greeter: 2 unchanged.
```

Reconciliation compares the document against what each row currently holds:

| Outcome | Effect |
|---|---|
| unchanged | the action keeps its identity, active state and statistics |
| changed | updated in place, deactivated, statistics reset, identity kept |
| gone from the document | deactivated, statistics reset |

Nothing is deleted, and no transaction, receipt or rating is ever touched.
Reconciliation is confined to that application's own rows; it never affects an
action you created by hand or one belonging to another application.

The document owns the description, both schemas, the base URL, method, path and
bindings, and the price only where it declares `x-juice-price`. You own the
visibility, the credentials, and the price where the document does not declare one.
Those survive every re-import. A document-owned field you edit by hand is restored
on the next import.

## Switching off and removing

The ordinary verbs act on the whole application:

```
$ juice action disable bob/greeter
$ juice action delete bob/greeter
```

Deletion is soft, as it is for any action.

## Credentials you hold

One credential can cover the whole application:

```
$ juice action import weather https://api.example.com/openapi.json \
    --auth '{"scheme":"bearer","secrets":{"token":"…"}}'
```

It is validated, encrypted, and applied when any of the application's actions is
dispatched. Replacing it revokes the consents callers have given.

## Credentials each caller holds

For an API where every caller uses their own account, configure a delegated scheme
instead. The kernel then holds one credential per caller, not one per action.

`oauth_delegated` performs the authorisation-code exchange with the provider:

```
--auth '{"scheme":"oauth_delegated","config":{"auth_url":"…","token_url":"…","client_id":"…","scopes":"…"}}'
```

`delegated_bearer` accepts a token the caller pastes:

```
--auth '{"scheme":"delegated_bearer"}'
```

A call by someone who has not consented is refused before any money moves, naming
what to connect. What the caller then does is described in
[Connecting an upstream account](../calling/consent-and-steps.html#connecting-an-upstream-account).

Two things follow from choosing this scheme. A delegated action
is never offered to other kernels, because a kernel has one account on yours and
cannot consent on behalf of its users. And the credential is bound to the exact
action consented for and to the caller who is paying: it is not inherited by
anything your action calls.

For the OAuth details, see
[`docs/oauth.md`](https://github.com/daios-ai/juice/blob/master/docs/oauth.md).

## Webhooks

An import installs the API you call. It does nothing about the API calling you.
Incoming events come in as an ordinary account running an action or completing a
step; see [External systems](steps.html#external-systems).
