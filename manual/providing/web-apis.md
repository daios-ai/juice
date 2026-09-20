---
title: Wrapping a web API
parent: Providing actions
nav_order: 5
---

# Wrapping a web API

An OpenAPI document describes the operations exposed by a web API. Juice can
use that description to register each supported operation as an action under
a common application name, preserving the API's existing HTTP implementation.

The document must be available at a URL the kernel can fetch. The following
example describes a small application with an `index` operation that explains
the service and a `greet` operation that returns a greeting. Assume it is hosted
at `https://greeter.example.com/openapi.json`:

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

The optional `x-juice-price` extension gives an operation's price in base units.
Import the document under the application name `greeter`:

```
$ juice action import greeter https://greeter.example.com/openapi.json
imported greeter/greet
imported greeter/index
greeter: 2 imported.
```

The chosen name identifies this installation of the document. It cannot be
reused for another document, but you can install the same document under a
different name to create an independent application.

Imported actions begin inactive and private. Enable them using their shared
path, then set the visibility required for their intended audience:

```
$ juice action enable bob/greeter
CHANGE   ACTION             PRICE       ACTIVE  AUDIENCE
enabled  bob/greeter/greet  0.50 fUSDT  yes     private
enabled  bob/greeter/index  0.00 fUSDT  yes     private
```

## What the document must declare

To produce a usable action, an operation must provide:

- an `operationId`, or an `x-juice-name`;
- a description or a summary;
- parameters, a request body schema, or both;
- exactly one unambiguous 2xx JSON response schema;
- an `x-juice-price` that is a non-negative integer, if it declares a price at all.

Supported HTTP methods are `GET`, `POST`, `PUT`, `PATCH`, and `DELETE`.
Non-JSON responses, streaming, multipart data, ambiguous success schemas, and
unsupported authentication cannot produce an activatable action.

The importer combines path parameters, query parameters, and the request body
into the action's input schema. It preserves the bindings needed to reconstruct
the HTTP request, and uses the selected success response as the output schema.

## The root of an application

An operation named `index` becomes the application's default entry point through
Juice's reference convention. For example, `bob/greeter` resolves to
`bob/greeter/index` when no action occupies the shorter name.

Set `operationId: index` or `x-juice-name: index` on an operation that describes
the application. This is optional; without it, callers name individual
operations directly.

```
$ juice action show bob/greeter
  id: 31004d30-…
  name: greeter/index
  kind: http
  description: What this application does
  …
```

## Re-importing

Re-importing reads the document again and reconciles its operations with the
installed actions. The kernel remembers the document URL, so the application
name is sufficient:

```
$ juice action import greeter
unchanged greeter/greet
unchanged greeter/index
greeter: 2 unchanged.
```

The importer compares the document with each action's current definition:

| Outcome | Effect |
|---|---|
| unchanged | the action keeps its identity, active state and statistics |
| changed | updated in place, deactivated, statistics reset, identity kept |
| gone from the document | deactivated, statistics reset |

Reconciliation preserves historical transactions, receipts, and ratings, and
affects only actions belonging to this import. Manually registered actions and
other applications are outside its scope.

The document controls descriptions, schemas, HTTP routing, and any price
explicitly declared by `x-juice-price`. Re-importing restores these fields if
you edited them manually. Visibility and credentials remain under your control,
as does the price of an operation whose document declares none.

## Switching off and removing

Because an application shares a path, the ordinary action commands can disable
or retire all of its operations together:

```
$ juice action disable bob/greeter
$ juice action delete bob/greeter
```

Retirement preserves the application's historical records, as it does for an
individual action.

## Credentials you hold

If all operations use your account at the upstream provider, supply its
credential when importing the application:

```
$ juice action import weather https://api.example.com/openapi.json \
    --auth '{"scheme":"bearer","secrets":{"token":"…"}}'
```

The ordinary action credential checks validate and encrypt it, and the kernel
applies it when dispatching the imported actions. Replacing the credential
revokes any associated caller grants.

## Credentials each caller holds

An API such as a personal mailbox may need each caller's own account. Configure
a delegated authentication scheme for these actions. Each caller then creates
a connection that can serve several explicitly authorized actions.

The `oauth_delegated` scheme lets the kernel exchange an authorization code
with the upstream provider:

```
--auth '{"scheme":"oauth_delegated","config":{"auth_url":"…","token_url":"…","client_id":"…","scopes":"…"}}'
```

The `delegated_bearer` scheme uses a personal token supplied by the caller:

```
--auth '{"scheme":"delegated_bearer"}'
```

A caller without the required grant is refused before charging and told which
action to connect. The consent procedure is described in
[Connecting an upstream account](../calling/consent-and-steps.html#connecting-an-upstream-account).

Delegated actions are excluded from federation: the peer's kernel account
cannot stand in for each remote user's upstream consent. Locally, each
credential applies only to the action granted access and when its grantor is
the payer. A child action must have its own grant.

The upstream provider must know the application before any caller can consent:
register it in the provider's console and add the redirect address the consent
flow will use to the application's allowed list. A provider that does not
recognize the client id or the redirect address refuses the consent, and the
caller sees that refusal from the provider, not from the kernel.

## Webhooks

OpenAPI import describes outbound calls to a service. To receive an event from
that service, give the external system a Juice account through which it can
run an action or complete a prepared step. See
[External systems](steps.html#external-systems).
