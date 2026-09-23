---
title: Finding an action
parent: Calling actions
nav_order: 2
---

# Finding an action

You can find a service by searching for what it does or by reading a known
action reference. In either case, inspect its description, input requirements,
and price before running it.

## Search

`sys/lookup` accepts a description of the service you need and returns ranked
action candidates. For example, a search for an echo service might return:

```
$ juice run sys/lookup '{"query":"echo a message"}'
sys/lookup costs 0.00 fUSD. Run it? [y/N] y
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
        "evidence": { … },
        "score": 0.0333
      },
      …
    ]
  }
```

The result contains the interface needed to assess the action and prepare a
call. The main fields are:

| Field | Meaning |
|---|---|
| `action` | the reference to pass to `run` |
| `description` | what the provider says it does |
| `input_schema`, `output_schema` | the contract: what it takes and returns |
| `price` | the all-in price, in base units |
| `quote_hash` | a fingerprint of the terms shown, for pinning |
| `evidence` | this kernel's experience, the provider's reports, and other kernels' observations |
| `score` | ranking score, comparable only within one result set |

`limit` controls how many results are returned; the default is ten.

When an embedding model is available, ranking combines keyword matching with
similarity in meaning. Without that model, the kernel uses keyword matching.
In either case, access filtering takes place before the result limit is
applied, so inaccessible local actions cannot displace accessible results.

## Results from other kernels

The kernel periodically learns about public actions hosted by its peers and
includes them in search. Their references identify both the provider and the
hosting kernel:

```
"action": "dave@beta-kernel/summarize",
"price": 2205000,
"observed_at": "2026-09-14T12:06:20Z",
"last_seen": "2026-09-14T12:06:20Z"
```

Remote results may carry three dates. `observed_at` records when the entry was
last verified against its home kernel. `last_seen` and
`last_contact_failed_at` record successful contact and a failed attempt known
not to have reached that kernel. These observations help you judge freshness,
but cannot guarantee that the service is reachable now.

A discovered action's price is indicative until its signed terms are resolved
from the home kernel. Your kernel caches verified terms for subsequent calls;
the serving kernel checks them again when admitting a call. Use the
`quote_hash` to pin the terms you selected, as explained in
[Running an action](running.html#calling-an-action-on-another-kernel).

## Reading an action directly

If you already know an action's reference, `action show` reads its interface
directly:

```
$ juice action show bob/echo
  id: bb7fe1a8-…
  owner_handle: bob
  name: echo
  kind: http
  active: true
  visibility: local
  price: 0.50 fUSD
  description: Echo a message back to the caller
  input_schema: { … }
  output_schema: {}
  quote_hash: 4965342976414282…
  requires_grant: false

This kernel's own calls
  3 calls, 3 succeeded  ~399ms  rating 1.00 from 1
```

The `requires_grant` field indicates that the action uses an upstream account
belonging to its caller. You must connect that account before the action can
run; see [Consent and assigned work](consent-and-steps.html).

A provider can give a group of actions a common entry point by publishing an
`index` action. If `bob/greeter` names no action directly, for example,
`action show` tries `bob/greeter/index`.

The same command accepts remote references such as
`dave@beta-kernel/summarize`. Below the contract it shows the evidence available
here: **This kernel's own calls**, **Reported by the provider**, and **Reported
by other kernels**. Sections without evidence are omitted. Search results carry
the same information in `evidence`, under `local_experience`, `provider_reported`,
and `observed_by_others`; these sources are kept separate.

Other kernels' reports are marked `[verified]`, `[N/M verified]`, or
`[unverified]` according to how many trades the provider's records confirm.
`[N contradicted]` counts disagreements about outcomes; `[N told two ways]`
counts trades an issuer described inconsistently. Dates give the period covered
by each report. Verification confirms a matching record of trade, not quality.

## Listing

```
$ juice action list
ACTION                      PRICE        AUTHORIZE
bob/mail                    0.00 fUSD   your own login
dave@beta-kernel/summarize   2.205 fUSD
bob/stamp                   1.00 fUSD
bob/echo                    0.50 fUSD
  …
```

The default list shows active actions within your access: your own actions,
local and public actions hosted here, and cached remote actions. `your own login`
in the `AUTHORIZE` column means an upstream connection is required. Use `--all`
to include inactive actions within your permitted scope; it also adds an
`ACTIVE` column between `PRICE` and `AUTHORIZE`.

## What other buyers thought

```
$ juice action ratings bob/echo
RATING  WHEN                  FROM   NOTE
good    2026-09-14T12:05:17Z  local  did what it said
```

Ratings present the payer's assessment of a completed call, including an optional
note and the date, without naming the payer. `local` identifies ratings recorded
here; `peer` identifies trade-backed ratings from other kernels, including for
remote actions. The call counts and mean execution time appear on `action show`.

Evidence shared between kernels contains less information than the private call
record. Its interpretation is explained in
[The network economy](../operating/network-economy.html#reputation-across-kernels).
