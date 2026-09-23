---
title: Running an action
parent: Calling actions
nav_order: 3
---

# Running an action

Once you have selected an action and prepared its input, use `run` to execute
it under the current login:

```
$ juice run ACTION [JSON]
```

`ACTION` is a reference: `owner/name` on this kernel, `owner@kernel/name` on
another, or a raw action id. `JSON` is the argument object, `{}` if omitted.
`@file.json` reads the arguments from a file.

```
$ juice run bob/echo '{"msg":"hello"}'
bob/echo costs 0.50 fUSD. Run it? [y/N] y
  result: {
    "json": { "msg": "hello" },
    …
  }
  tx_id: 116fd3a6-…
  trace_id: 009b8dc1-…
  receipt_id: b69abbd8-…
  process_id: a394b5c5-…
  charge: 0.50 fUSD
```

The response includes the action's result, its settled charge, and identifiers
for its records. Use `tx_id` with `tx show` to read the full record, `tx verify`
to check the receipt, or `tx rate` to record your assessment. `charge` is absent
while settlement is deferred.

## What a call costs

For a local action, the advertised price covers the call and the work it
performs through other actions. The provider must fit that work within its
budget. A direct call to another kernel also involves a settlement stake,
described under [The ticket](#the-ticket), so its final charge can vary around
the advertised price.

Starting a paid call reserves its price from your available balance. On success,
the full price is paid: the provider earns the unused margin after the kernel's
fee. The price therefore represents the service purchased, rather than a meter
of the resources consumed. A failed call returns the unspent part of the budget.
Zero-price actions require no execution funds.

## When something goes wrong

The kernel checks input against the action's schema before reserving funds.
If a required argument is absent or has the wrong type, the request is rejected
without charge:

```
$ juice run bob/echo '{}'
bob/echo costs 0.50 fUSD. Run it? [y/N] y
error: field msg: required field missing
```

Failure after execution has begun is different. The kernel refunds the budget
that remains, but preserves payment for work already completed by other actions.
An output that fails schema validation also causes the action to fail; the
provider receives no payment for that failed output.

Some actions require permission to use an upstream account belonging to you.
If that consent is missing, the kernel rejects the call before charging and
identifies the action to connect:

```
$ juice run bob/mail '{"body":"hi"}'
bob/mail costs 0.00 fUSD. Run it? [y/N] y
error: grant required for bob/mail
       Authorize it with: juice user connect bob/mail
```

## Pinning the terms you saw

`run` reads the action, shows its price, and pins those terms before calling it.
At a terminal it asks for confirmation; `--yes` skips the question. Without a
terminal it prints the price to stderr and proceeds. This applies to local and
remote actions, including free ones.

To use terms you read earlier, pass the `quote_hash` returned by search or
`action show`. The client sends it unchanged, without another read or prompt:

```
$ juice run bob/echo '{"msg":"hi"}' --quote-hash 4965342976414282…
```

If an otherwise callable action has different terms, the kernel rejects the
request before charging and reports the current quote:

```
error: the action's terms changed; it now costs 0.50 fUSD
       Nothing was charged. The price is now 500000; pass --quote-hash 4965342976414282… to accept it.
```

An explicit hash is useful whenever selection and execution happen at different
times, particularly in programs that prepare work in advance.

## Calling an action on another kernel

To reach a remote provider, include its kernel in the action reference. This
can be a petname known to your kernel or the remote kernel's public key:

```
$ juice run 'dave@beta-kernel/summarize' '{"text":"a long document"}'
dave@beta-kernel/summarize costs 2.205 fUSD. Run it? [y/N] y
```

Your local balance funds the purchase. The two kernels handle the exchange,
without requiring you to open or fund an account at the destination.

On first use, your kernel fetches and verifies the action's signed terms, then
caches them. Later calls can use the cache. The serving kernel refuses a call
whose cached terms no longer match; the local cache can then be refreshed for
a subsequent attempt.

The advertised remote price includes the provider's price, the serving markup,
and your kernel's import fee. The markup compensates the provider for advancing
the work and accepting the settlement draw described below. With a provider
price of `2.00` and both rates at 5%, the advertised total is `2.205`. When the
obligation is at least the ticket's face value, it is paid exactly, as in this
example with the default `1.00` ticket:

```
$ juice user me
  available: 7.00 fUSD
$ juice run 'dave@beta-kernel/summarize' '{"text":"a long document"}'
dave@beta-kernel/summarize costs 2.205 fUSD. Run it? [y/N] y
  …
$ juice user me
  available: 4.795 fUSD
```

### The ticket

A paid remote call may also reserve a **stake** from your balance. Your kernel's
`lottery` setting determines its size. This stake supports settlement of small
obligations, for which making an individual blockchain payment could cost more
than the service itself.

When the obligation is smaller than the ticket's face value, a draw determines
whether the full face value is paid or no payment is made. The probability is
chosen so that the expected payment equals the obligation, and the two kernels
contribute to the draw without either choosing its outcome. An obligation at
least as large as the face value is paid exactly.

{: .warning }
> A remote call can cost more than its advertised price when the draw pays.
> Allow for both the required stake and the possible final charge.

The first consequence is a funding requirement: both the price and the stake
must be available at dispatch. For a price of `2.205` and a stake of `1.00`, a
balance of `2.50` is insufficient:

```
$ juice user me
  available: 2.50 fUSD
$ juice run 'dave@beta-kernel/summarize' '{"text":"x"}'
dave@beta-kernel/summarize costs 2.205 fUSD. Run it? [y/N] y
error: insufficient user balance
```

The second consequence is variation in the final charge. For a successful call
whose obligation is below the face value, settlement returns the execution
budget except for the import fee and releases the stake. A paying draw then
reserves the face value as payment. The call therefore costs either the import
fee alone or the import fee plus the face value. Its expected cost is the
advertised price; a finite series of calls need not average to that exact amount.

If the obligation is at least the face value, the payment equals the obligation
and the successful call costs its advertised price. Setting `lottery` to zero
also pays every obligation exactly, with no stake or draw. Ask your operator
which setting applies, or inspect `juice admin kernel show` if you operate the
kernel yourself.

If the client cannot reach your local kernel, it reports the name and address:

```
error: cannot reach kernel acme at http://127.0.0.1:4040
       Start it with: juice kernel serve play
```

### When the other kernel cannot be reached

When contact fails during a remote call, the kernel distinguishes a request
known not to have reached the peer from one that may already be executing there.
The first case can fail immediately with a full refund:

```
error: peer beta-kernel is unreachable; the call was not sent and has been refunded
       Nothing was charged. Try again when beta-kernel is back.
```

In the second case, the call remains pending because its outcome is unknown.
Its funds stay locked, and the response identifies the process and when it
began waiting:

```
Your funds are reserved, not spent, on process 01d1da53-…, waiting for the peer's answer since 2026-09-14T12:06:27Z. It retries by itself; follow it with: juice process show 01d1da53-…
```

A pending call is retried under its original identity, including after a
restart, so a retry can recover the outcome without buying the work again.
Only the peer's signed receipt or refusal settles the call. There is no timeout
refund, and `juice process end` is refused while the call awaits that answer.
If the peer never answers, the funds remain reserved.

{: .warning }
> Do not re-run a parked call. `run` has no idempotency key, so running it again
> buys the work a second time and you pay twice.

Follow the process instead:

```
$ juice process show 01d1da53-…
```

### Actions are not re-sold

A cached remote action is available to local callers but cannot be exported
again to a third kernel. Each remote call therefore resolves directly from the
action's home kernel, keeping its terms and signed outcome tied to the provider.
