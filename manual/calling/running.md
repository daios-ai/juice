---
title: Running an action
parent: Calling actions
nav_order: 3
---

# Running an action

```
$ juice run ACTION [JSON]
```

`ACTION` is a reference: `owner/name` on this kernel, `owner@kernel/name` on
another, or a raw action id. `JSON` is the argument object, `{}` if omitted.
`@file.json` reads the arguments from a file.

```
$ juice run bob/echo '{"msg":"hello"}'
  result: {
    "json": { "msg": "hello" },
    …
  }
  tx_id: 116fd3a6-…
  trace_id: 009b8dc1-…
  receipt_id: b69abbd8-…
  process_id: a394b5c5-…
```

`tx_id` identifies the transaction, and is what you pass to `tx show`,
`tx verify` and `tx rate`.

## What a call costs

The price you were quoted is the whole cost. Whatever the action does internally —
calling other paid actions, waiting for a person, calling an action on another
kernel — comes out of that price. You are charged the price and nothing more.

Running the call takes the price from your available balance and locks it for the
duration. On success the whole price is paid: the provider keeps what the work did
not consume. There is no metering and no discount for an action that finished
cheaply.

Actions priced at zero run without any funds.

## When something goes wrong

**Invalid arguments cost nothing.** Input is checked against the action's schema
before any money moves.

```
$ juice run bob/echo '{}'
error: field #.msg: required field missing
```

**A failed call refunds what was not consumed.** If the action ran and failed, you
are refunded the part of the price that had not already been spent. Work that
sub-providers completed successfully before the failure stays paid; that money is
gone and the refund does not cover it. Invalid output is never paid for.

**A missing consent costs nothing.** An action that needs you to connect an
upstream account of your own is refused before any charge, naming what to connect:

```
$ juice run bob/mail '{"body":"hi"}'

Authorize with:
  juice user connect bob/mail
error: grant required for bob/mail
```

## Pinning the terms you saw

A provider may change an action's price or contract at any time. To be sure you
are buying what you read, pass the `quote_hash` you saw:

```
$ juice run bob/echo '{"msg":"hi"}' --quote-hash 4965342976414282…
```

If the terms have moved, the call is refused before any charge and the new terms
are named:

```
Nothing was charged. The action's terms changed since you quoted them; its price is now 500000.
Re-read the action and pass --quote-hash 4965342976414282… to accept the new terms.
error: the action's terms changed; it now costs 0.50 credits
```

Without a pin, a call is made at whatever the current terms are. Pin whenever
time passes between reading an action and calling it.

## Calling an action on another kernel

Name the kernel in the reference. The kernel part is a petname your operator
assigned, or the kernel's public key.

```
$ juice run 'dave@beta-kernel/summarize' '{"text":"a long document"}'
```

You pay from your balance on your own kernel. You need no account on the other
kernel, no prefunding and no approval from its operator.

The first such call fetches the action's signed terms from its home kernel,
verifies them, and caches them. Later calls use the cache. If the remote contract
has changed, the call is refused and the terms are fetched again rather than
being repriced silently.

The price is all-in and is fixed before the call runs. It has three parts: the
provider's price, a markup that compensates the provider for doing the work on
credit and being paid by a draw, and your own kernel's import fee. With a provider
price of `2.00`, a markup of 5% and an import fee of 5%, you pay `2.205`:

```
$ juice user me
  available: 7.00 credits
$ juice run 'dave@beta-kernel/summarize' '{"text":"a long document"}'
  …
$ juice user me
  available: 4.795 credits
```

### The ticket

A call to another kernel also **stakes** a fixed amount from your balance while
it runs. The stake exists because paying every small cross-kernel debt
individually would cost more in payment fees than the debts are worth. Instead,
debts smaller than the stake are settled by a draw: the debt is paid at the
stake's full face value with a probability that makes the average payment equal
the debt. Neither kernel can influence the outcome. A debt at least as large as
the stake is paid in full.

Two consequences follow for you as a buyer.

{: .warning }
> One call to another kernel can cost more than the price you were shown. See the
> two consequences below.

**You need the stake as well as the price.** Both must be available when the call
is dispatched. With a price of `2.205` and a stake of `1.00`, a balance of `2.50`
is not enough:

```
$ juice user me
  available: 2.50 credits
$ juice run 'dave@beta-kernel/summarize' '{"text":"x"}'
error: insufficient user balance
```

**What one call finally costs depends on the draw.** When the call settles, the
whole price comes back to you except your kernel's import fee, and the stake is
released. If the draw pays, the stake's full face value is then taken. So a call
whose debt was below the stake costs you either the import fee alone, or the
import fee plus the face value. Across many calls the average is the price you
saw, which is the sense in which the advertised price bounds a cross-kernel call.
A debt at least as large as the stake is paid exactly, and the call costs the
price. Calls within your own kernel have no stake and no such variation.

The stake is your kernel's setting, not the provider's. An operator who sets it
to zero pays every debt exactly, and calls from that kernel have no stake and no
draw. Ask your operator, or read `juice admin kernel show` if you run the kernel
yourself.

### When the other kernel cannot be reached

A cross-kernel call ends in exactly one of two ways.

It **fails fast**, with a full refund, when the call provably never left your
kernel:

```
error: peer unreachable
```

Or it **parks**, when the call may have been received and its outcome is not yet
known. The money stays locked and the run reports where to follow it:

```
process_id: 01d1da53-…
pending_since: 2026-09-14T12:06:27Z
refund_eligible_at: 2026-09-15T12:06:27Z
```

A parked call is retried until a signed receipt arrives, surviving restarts of
either kernel. If none arrives within 24 hours it settles as a failure with a full
refund. You are never charged twice and the call is never silently dropped.

{: .warning }
> Do not re-run a parked call. `run` has no idempotency key, so running it again
> buys the work a second time and you pay twice.

Follow the process instead:

```
$ juice process show 01d1da53-…
```

### Actions are not re-sold

A kernel never serves another kernel's imported action onward. To call an action
you must resolve it from the kernel that owns it, so there is never a chain of
intermediaries between you and the provider.
