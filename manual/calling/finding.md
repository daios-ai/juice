---
title: Finding an action
parent: Calling actions
nav_order: 2
---

# Finding an action

## Search

`sys/lookup` searches in natural language and returns ranked candidates.

```
$ juice run sys/lookup '{"query":"echo a message"}'
  result: {
    "results": [
      {
        "action": "bob/echo",
        "action_id": "bb7fe1a8-…",
        "description": "Echo a message back to the caller",
        "input_schema": {
          "type": "object",
          "properties": {
            "msg": { "type": "string", "description": "text to echo" }
          },
          "required": ["msg"]
        },
        "output_schema": {},
        "price": 500000,
        "quote_hash": "4965342976414282…",
        "score": 0.0333
      },
      …
    ]
  }
```

Each result carries everything needed to decide and to call:

| Field | Meaning |
|---|---|
| `action` | the reference to pass to `run` |
| `description` | what the provider says it does |
| `input_schema`, `output_schema` | the contract: what it takes and returns |
| `price` | the all-in price, in base units |
| `quote_hash` | a fingerprint of the terms shown, for pinning |
| `score` | ranking score, comparable only within one result set |

`limit` controls how many results are returned; the default is ten.

Ranking combines keyword matching with semantic similarity. A kernel with no
embedding model configured falls back to keyword matching alone, so search
degrades but keeps working.

Search never returns an action you could not call. Results are filtered by what
is visible to you before they are ranked and truncated.

## Results from other kernels

A kernel learns about actions on other kernels in the background and includes them
in search. Such a result names the kernel it lives on:

```
"action": "dave@beta-kernel/summarize",
"price": 2205000,
"observed_at": "2026-09-14T12:06:20Z",
"last_seen": "2026-09-14T12:06:20Z"
```

Three extra fields appear on these results. `observed_at` is when your kernel last
verified this entry against the kernel that owns it. `last_seen` and
`last_contact_failed_at` are when the hosting kernel was last reached and last
provably unreachable. They say how current the entry is; they are not a promise
that the kernel is up now.

The price shown for a remote action is indicative. Nothing is committed until the
call is made, at which point your kernel fetches the action's signed terms from
its home kernel and quotes you the real price. A stale directory therefore affects
only what you find, never what you pay. See
[Running an action](running.html#calling-an-action-on-another-kernel).

## Reading an action directly

If you already know the reference:

```
$ juice action show bob/echo
  id: bb7fe1a8-…
  owner_handle: bob
  name: echo
  kind: http
  active: true
  visibility: local
  price: 0.50 credits
  description: Echo a message back to the caller
  input_schema: { … }
  output_schema: {}
  quote_hash: 4965342976414282…
  requires_grant: false
```

`requires_grant` says whether the action needs you to connect an account of your
own before it will run. See [Consent and steps](consent-and-steps.html).

A reference that names no action resolves to that path's `index` child instead.
`juice action show bob/greeter` returns `bob/greeter/index`, which is how a group
of related actions describes itself.

## Listing

```
$ juice action list
  bob/mail                        0.00 credits [grant]
  dave@beta-kernel/summarize      2.205 credits
  bob/stamp                       1.00 credits
  bob/echo                        0.50 credits
  …
```

This lists the actions you can call on this kernel: your own, this kernel's
`local` actions, its `public` actions, and any remote actions your kernel has
cached. `[grant]` marks an action that needs you to connect an account of your own
first. `--all` adds inactive and private rows within your own scope.

## What other buyers thought

```
$ juice action ratings bob/echo
1  2026-09-14T12:05:17Z  did what it said
$ juice action stats bob/echo
  action_id: bb7fe1a8-…
  uses: 3
  successes: 3
  failures: 0
  rating_count: 1
  latency_estimate: 0.399
  rating_estimate: 1
  last_used_at: 2026-09-14T12:05:24Z
```

Ratings show the value, the note and the date, without the rater's identity.
Every rating was written by an account that paid for a call to that action; there
is no way to rate an action you did not buy. `latency_estimate` is in seconds.

For actions on other kernels, the evidence that crosses the network is narrower
and is described in
[The network economy](../operating/network-economy.html#reputation-across-kernels).
