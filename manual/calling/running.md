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

`ACTION` is a reference, `owner@kernel/name` on this kernel and on any other
alike, or a raw action id. `JSON` is the argument object, `{}` if omitted.
`@file.json` reads the arguments from a file.

```
$ juice run bob@acme/echo '{"msg":"hello"}'
bob@acme/echo costs 0.50 fUSD. Run it? [y/N] y
  result: {
    "json": { "msg": "hello" },
    …
  }
  tx_id: 116fd3a64b0e
  trace_id: 009b8dc1-…
  receipt_id: b69abbd8-…
  process_id: a394b5c5e206
  charge: 0.50 fUSD
```

The response includes the action's result, its settled charge, and identifiers
for its records. Use `tx_id` with `tx show` to read the full record, `tx verify`
to check the receipt, or `tx rate` to record your assessment. `charge` is absent
while settlement is deferred.

## What a call costs

For a local action, the advertised price covers the call and the work it
performs through other actions. The provider must fit that work within its
budget. A call to another kernel can cost more or less than the advertised
price because of its ticket. [What an action costs](../money/funds.html#what-an-action-costs)
explains the fees, ticket, and final charge together.

Starting a paid call reserves its price from your available balance. On a
successful local call, the full price is paid: the provider earns the unused
margin after the local execution fee. The price represents the service
purchased, rather than a meter of the resources consumed. A failed call
returns the unspent part of the budget. Zero-price actions require no
execution funds.

## When something goes wrong

The kernel checks input against the action's schema before reserving funds.
If a required argument is absent or has the wrong type, the request is rejected
without charge:

```
$ juice run bob@acme/echo '{}'
bob@acme/echo costs 0.50 fUSD. Run it? [y/N] y
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
$ juice run bob@acme/mail '{"body":"hi"}'
bob@acme/mail costs 0.00 fUSD. Run it? [y/N] y
error: grant required for bob@acme/mail
       Authorize it with: juice user connect bob@acme/mail
```

## Pinning the terms you saw

`run` reads the action, shows its price, and pins those terms before calling it.
At a terminal it asks for confirmation; `--yes` skips the question. Without a
terminal it prints the price to stderr and proceeds. This applies to local and
remote actions, including free ones.

To use terms you read earlier, pass the `quote_hash` returned by search or
`action show`. The client sends it unchanged, without another read or prompt:

```
$ juice run bob@acme/echo '{"msg":"hi"}' --quote-hash 4965342976414282…
```

If an otherwise callable action has different terms, the kernel rejects the
request before charging and reports the current quote:

```
error: the action's terms changed; it now costs 0.50 fUSD
       Nothing was charged. The price is now 500000; pass --quote-hash 4965342976414282… to accept it.
```

An explicit hash is useful whenever selection and execution happen at different
times, particularly in programs that prepare work in advance.

## Retrying after a lost reply

Give a purchase a key before starting it if you may need to repeat the request
after a lost connection:

```
$ juice run bob@acme/echo '{"msg":"hi"}' --external-key echo-2026-09-14-001
```

Repeat with the same account, key, action reference, and input to recover that
run's outcome without buying the work again. If it has settled, you receive its
original success or failure; the reported charge is for that first run. If it
has not settled, the kernel reports that it is still pending and gives you the
process to follow. Reusing the key with another action or input is refused.

Use a new key for a new purchase. Without a key, running again starts another
purchase; adding a key afterwards cannot recover an earlier unkeyed run.

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

The advertised price includes the provider's price, its export fee, and
your kernel's import fee. [What an action costs](../money/funds.html#what-an-action-costs)
shows who receives each part and works through both ticket outcomes.

### The ticket

A paid remote call may also reserve a **stake** from your balance. Your kernel's
`lottery` setting determines its size. It covers a ticket that may pay the
provider when the call settles. Small purchases use tickets so the kernels
need not make a rail payment for every call.

{: .warning }
> A remote call can cost more than its advertised price when the draw pays.
> Allow for both the required stake and the possible final charge.

Both the price and the stake must be available before the call is sent to the
peer. The `charge` in the reply is what the call cost you when it finished; the
amount held while it ran is not its final cost. Ask your operator which ticket
setting applies, or inspect `juice admin kernel show` if you operate the kernel
yourself.

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
> If the first request used `--external-key`, you can safely repeat it with the
> same key, action, and input. Without that original key, submitting another run
> buys the work again; follow the existing process while its outcome is unknown.

To follow the process:

```
$ juice process show 01d1da53c418
```

### Actions are not re-sold

A cached remote action is available to local callers but cannot be exported
again to a third kernel. Each remote call therefore resolves directly from the
action's home kernel, keeping its terms and signed outcome tied to the provider.
