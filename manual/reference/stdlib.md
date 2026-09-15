---
title: The sys standard library
parent: Reference
nav_order: 2
---

# The sys standard library

Every kernel ships these actions under the handle `sys`. They are ordinary actions:
called with `run`, priced, recorded, and rated like any other. They hold no special
privilege, and an equivalent could be published by anyone as an `http` or `wasm`
action.

They are `local` to their kernel, so they are not served to other kernels. A kernel
with capacity to sell wraps its own public action around one.

The operator sets each one's price in `native.<name>`. The defaults are zero for
all but `sys/tinygo/compile`. Amounts in these schemas are base units.

## Search

**`sys/lookup`** — `{query, limit=10}` → `{results: [...]}`

Ranked candidates, each with `action`, `action_id`, `description`, `input_schema`,
`output_schema`, `price`, `quote_hash` and `score`. Results from other kernels also
carry `observed_at`, `last_seen` and `last_contact_failed_at`. Ranking combines
keyword and semantic matching; with no embedding model configured it falls back to
keyword matching. Results are filtered to what the caller may call before they are
ranked. See [Finding an action](../calling/finding.html).

## Language model

These need a model configured in `native.llm`. Without one they report
`ErrInvalidState` and charge nothing.

**`sys/llm/chat`** — `{messages, system?}` → `{message}`

**`sys/llm/embed`** — `{text}` → `{embedding}`

**`sys/llm/json`** — `{messages, system?, output_schema}` → `{value}`, validated
locally against the schema.

**`sys/llm/decide`** — `{messages, actions}` → `{action, args, message?}`

Selects one action from the candidates and proposes arguments. It never executes
anything. Contracts are read from the kernel by reference, never taken from the
request; proposed arguments are validated against the real schema. A candidate on
an unreachable kernel is discarded, and `ErrNotFound` is reported only if none
resolves. See
[Separate planning from spending](../programs.html#separate-planning-from-spending).

## Basics

**`sys/time`** — `{}` → `{unix, iso}`. Seconds since the epoch, and RFC 3339.

**`sys/random`** — `{}` → `{value}`. A cryptographically secure float in `[0,1)`.
It exists because sandboxed code has no source of entropy of its own.

**`sys/sink`** — anything → `{}`. Accepts and discards. Used as the target of a
step whose point is the waiting, not the work.

## Messaging and money

**`sys/message`** — `{to, message}` → `{step_id}`

Creates a waiting step for the named recipient, carrying the message. The
recipient sees it with `step list` and answers it with `step complete`. See
[Consent and assigned work](../calling/consent-and-steps.html#completing-work-addressed-to-you).

**`sys/transfer`** — `{target, amount}` → `{amount}`

Delivers `amount` base units from the immediate caller to a local recipient,
untaxed and all-or-nothing. The recipient must be an ordinary active account on
the same kernel. See
[Moving money through an action](../money/funds.html#moving-money-through-an-action).

## The web

**`sys/web`** — `{url}` → `{status, body, content_type, final_url}`

A read-only `GET`. It takes no headers and carries no credentials, so nothing
sensitive can enter arguments, receipts or logs. A URL with no scheme defaults to
`https`, and an explicit scheme is never downgraded. Private, link-local and
reserved addresses are refused unless the operator allowed them. A non-2xx status
is returned rather than raised. Responses are capped at 10 MiB.

This is the only way code running inside the kernel reaches the network.

## Building actions

**`sys/tinygo/compile`** — `{source}` → `{status, artifact, artifact_hash, diagnostics}`

Compiles a `Handle` function into a WebAssembly module, returned base64-encoded.
The surrounding code — package, imports, allocator, entry point — is supplied for
you. A compile error is reported as `status: "failure"` with diagnostics, and is
charged as a failed output. If the toolchain is not installed, the action reports
`ErrInvalidState` and charges nothing.

Registering the result is a separate step:
`action create --kind wasm --artifact <file>`. See
[Composition](../providing/composition.html#composing-from-webassembly).
