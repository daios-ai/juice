---
title: The sys standard library
parent: Reference
nav_order: 2
---

# The sys standard library

The built-in actions are published under `sys` and use the ordinary calling,
payment, and recording interfaces. Most provide common services such as search,
time, and web access; the transfer action additionally has the kernel-declared
authority to deliver value.

Built-ins have local visibility. A provider wishing to offer one of these
capabilities remotely can compose it into a public action with its own price
and contract. The operator configures built-in prices under `native.<name>`;
all default to zero except `sys/tinygo/compile`. Amounts in action JSON use
base units.

## Search

**`sys/lookup`** — `{query, limit=10}` → `{results: [...]}`

Returns ranked candidates with `action`, `action_id`, `description`,
`input_schema`, `output_schema`, `price`, `quote_hash`, `evidence`, and `score`.
Evidence is the same view returned by `action show`. Remote
results may include observation and contact timestamps. Ranking combines
keywords with semantic matching when an embedding model is available, and
uses keywords alone otherwise. Access filtering precedes the result limit.
See [Finding an action](../calling/finding.html).

## Language model

These actions require the corresponding model capability configured through
`native.llm`. An unavailable capability is reported as `ErrInvalidState`
without charging for the request.

**`sys/llm/chat`** — `{messages, system?}` → `{message}`

**`sys/llm/embed`** — `{text}` → `{embedding}`

**`sys/llm/json`** — `{messages, system?, output_schema}` → `{value}`, validated
locally against the schema.

**`sys/llm/decide`** — `{messages, actions}` → `{action, args, message?}`

Selects a candidate and proposes schema-valid arguments without executing the
selected action. The kernel resolves candidate contracts by reference.
Unavailable remote candidates can be discarded; an unknown local reference
is an error, and a set with no resolvable candidate returns `ErrNotFound`. See
[Separate planning from spending](../programs.html#separate-planning-from-spending).

## Basics

**`sys/time`** — `{}` → `{unix, iso}`. Seconds since the epoch, and RFC 3339.

**`sys/random`** — `{}` → `{value}`. Returns a random value in `[0,1)` for
sandboxed code, which has no direct access to operating-system entropy.

**`sys/sink`** — anything → `{}`. Accepts input and returns an empty object.
It can complete a step that needs acknowledgment without further processing.

## Messaging and money

**`sys/message`** — `{to, message}` → `{step_id}`

Creates a step carrying the message for the named recipient. The recipient can
inspect it with `step list` and acknowledge it with `step complete`. See
[Consent and assigned work](../calling/consent-and-steps.html#completing-work-addressed-to-you).

**`sys/transfer`** — `{target, amount}` → `{amount}`

Reserves `amount` base units from the immediate caller and delivers them whole
on success. The execution price is charged separately, and failure returns the
value reservation. The recipient must be an ordinary, unsuspended account on
the same kernel. See
[Moving money through an action](../money/funds.html#moving-money-through-an-action).

## The web

**`sys/web`** — `{url}` → `{status, body, content_type, final_url}`

Fetches a URL with `GET`, without accepting custom headers or credentials.
A URL without a scheme defaults to `https`; an explicit scheme is preserved.
The kernel's outbound policy excludes private, link-local, and reserved
addresses unless enabled by the operator, while allowing loopback by default.
Non-2xx HTTP responses are returned as results, and bodies are capped at 10 MiB.

Sandboxed code can use this action or another registered HTTP action to reach
the network through the ordinary call interface.

## Building actions

**`sys/tinygo/compile`** — `{source}` → `{status, artifact, artifact_hash, diagnostics}`

Compiles a `Handle` function and returns a base64-encoded WebAssembly module.
The compiler supplies the surrounding package, imports, allocator, and entry
point. Source errors return `status: "failure"` with diagnostics as the paid
compilation result. An unavailable toolchain produces `ErrInvalidState`
without charging.

Register the decoded module using `action create --kind wasm --artifact <file>`.
See
[Composition](../providing/composition.html#composing-from-webassembly).
