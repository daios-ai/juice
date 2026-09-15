---
title: Getting started
nav_order: 2
---

# Getting started

This chapter takes you from an empty machine to your first call, and then through
one paid call between two accounts. It uses a kernel you run yourself on the
`play` network, where money is not real and the operator credits accounts by hand.
The last section explains what changes when you use a kernel somebody else runs.

Everything shown is what the commands print on a terminal. Where a command asks a
question, the question is shown and the answer follows it.

## Install

Juice needs Go 1.25 or later.

```
$ git clone https://github.com/daios-ai/juice.git
$ cd juice
$ make build
```

This produces `./juice` in the repository. `make install` copies it to
`~/.local/bin`, after which you can type `juice` from anywhere; the examples below
assume that.

## Start a kernel

A kernel is named. The name is what the kernel calls itself on the network and the
name of the directory that holds its state.

```
$ juice kernel serve acme --addr :4040
```

There is no kernel called `acme` yet, so the command asks before creating one:

```
There is no kernel named acme. No kernels here yet.
Create acme as a new kernel? [y/N] y

Which money will acme use? This cannot be changed later.
  play  no real money: you credit accounts yourself and keep the records
  test  fake USDC on the Arbitrum Sepolia test chain
  real  USDC on Arbitrum One
Choice [play/test/real]: play
Superuser password:
Confirm password:
sys recovery phrase (write this down; it is shown only once and cannot be recovered):
  depart motion moon climb useless hole learn usage delay fish brand lab
Press Enter once you have written it down:
Superuser "sys" created.
INF server.ready handle=acme network=play addr=[::]:4040 public_key=L3ciw7zj…
```

{: .warning }
> Write the recovery phrase down before pressing Enter. It is shown once and is the
> only way to reset the superuser password.

{: .warning }
> The choice of money cannot be changed later. Everything else about the kernel
> can.

The kernel now runs in the foreground of this terminal. Leave it running and open
a second terminal for everything below.

The superuser account is called `sys`. It credits accounts and can see every
account on the kernel.

## Tell the client about the kernel

`juice` is one program, but the kernel and the commands you type are separate
things. The commands are a client, and a client keeps a list of the kernels it can
reach.

```
$ juice kernel add http://localhost:4040
acme  network play  http://localhost:4040  L3ciw7zj…  (added)
```

The client dials the address, records the public key and network the kernel
reports, and registers the kernel under the name it reports about itself, here
`acme`.

## Create an account and log in

```
$ juice user create alice@acme
Password:
Confirm password:
Recovery phrase (write this down; it is shown only once and cannot be recovered):
  prepare divorce absurd cabin series excite lunar vicious approve brown fossil hard
Press Enter once you have written it down:
  available: 0.00 credits
  description:
  handle: alice
  id: 5b984930-…
  locked: 0.00 credits
```

The phrase is this account's only recovery route; there is no email in Juice.

Creating an account does not log you in:

```
$ juice auth login alice@acme
Password:
alice@acme
```

`alice@acme` is now the login in use. Every command from here on acts as alice on
acme until you switch.

```
$ juice user me
  available: 0.00 credits
  connections: []
  connectors: []
  description:
  handle: alice
  id: 5b984930-…
  locked: 0.00 credits
```

## Run a free action

Every kernel ships a small standard library of actions under the handle `sys`. The
operator sets their prices; on a fresh kernel all but the code compiler are free.

```
$ juice run sys/time
  result: {
    "iso": "2026-09-14T15:05:36Z",
    "unix": 1789398336
  }
  tx_id: ed7c98e8-…
  trace_id: 7f5bd164-…
  receipt_id: 13382701-…
  process_id: 8816e98e-…
```

That is a complete call: an action ran, returned a result, and left a transaction
(`tx_id`) and a receipt. Everything else in Juice is a variation on this.

## Put money in the account

To buy anything, alice needs credits. On the `play` network nothing is sent from
anywhere: the operator credits accounts against payments received outside the
system and keeps the records.

```
$ juice auth login sys@acme
Password:
sys@acme
$ juice admin user deposit alice 10 --ref demo-payment-1
Credit 10.00 credits to alice, acting as sys@acme? This cannot be undone. [y/N] y
  amount: 10.00 credits
  reason:
  created_at: 2026-09-14T15:05:37Z
  operator_handle: sys
  from_handle: sys
  to_handle: alice
```

`--ref` names the payment in the operator's own books. The same reference never
credits an account twice.

You now hold two logins. Switch back to alice:

```
$ juice auth use alice@acme
alice@acme
```

## A paid call between two accounts

For a paid call there must be something to buy. This section creates a second
account, bob, who publishes an action, and then has alice find it, buy it and rate
it.

Bob's action wraps `https://httpbin.org/post`, a public endpoint that echoes back
what it receives. It needs an internet connection.

### Bob publishes

```
$ juice user create bob@acme
Password:
Confirm password:
Recovery phrase (write this down; it is shown only once and cannot be recovered):
  slim appear diamond peanut unit funny net right circle raven blind youth
Press Enter once you have written it down:
  …
$ juice auth login bob@acme
Password:
bob@acme
```

Now acting as bob:

```
$ juice action create echo --kind http --source https://httpbin.org/post \
    --price 0.5 --description "Echo a message back to the caller" \
    --input-schema '{"type":"object","properties":{"msg":{"type":"string","description":"text to echo"}},"required":["msg"]}'
  id: 6a9e04ce-…
  owner_handle: bob
  name: echo
  kind: http
  active: false
  visibility: private
  price: 0.50 credits
  description: Echo a message back to the caller
  …
  quote_hash: 1f8ec43b…
```

A new action is inactive and private, so nothing is callable by accident. Bob
switches it on and lets other users of this kernel see it:

```
$ juice action enable bob/echo
enabled bob/echo
$ juice action update bob/echo --visibility local
  …
  active: true
  visibility: local
  …
```

### Alice finds it and buys it

```
$ juice auth use alice@acme
alice@acme
$ juice run sys/lookup '{"query":"echo a message"}'
  result: {
    "results": [
      {
        "action": "bob/echo",
        "action_id": "6a9e04ce-…",
        "description": "Echo a message back to the caller",
        "input_schema": { … },
        "output_schema": {},
        "price": 500000,
        "quote_hash": "1f8ec43b…",
        "score": 0.0333
      },
      …
    ]
  }
  …
```

The `price` in a search result is in base units: `500000` is `0.50 credits`.
Amounts inside an action's JSON are always base units; amounts the command line
takes and prints are in credits.

```
$ juice run bob/echo '{"msg":"hello"}'
  result: {
    "json": {
      "msg": "hello"
    },
    …
  }
  tx_id: e989c5e1-…
  trace_id: 709b18e0-…
  receipt_id: d2b67088-…
  process_id: 5027b6df-…
```

Alice's balance has gone down by exactly the price:

```
$ juice user me
  available: 9.50 credits
  …
```

### What the call cost, and rating it

```
$ juice tx show e989c5e1-…
  id: e989c5e1-…
  action_name: echo
  args: { "msg": "hello" }
  result: { … }
  status: success
  gross: 0.50 credits
  net: 0.40 credits
  fee: 0.10 credits
  refund: 0.00 credits
  started_at: 2026-09-14T15:05:54Z
  ended_at: 2026-09-14T15:05:54Z
  rating: null
  owner_handle: alice
  caller_handle: alice
  target_handle: bob
```

Alice paid `0.50`. Bob received `0.40`. The kernel took `0.10`, which is its fee of
20% on bob's margin. [Earnings](providing/earnings.html) explains the arithmetic.

Only the account that paid can rate a call, and only once:

```
$ juice tx rate e989c5e1-… 1 --note "did what it said"
  id: c84133a1-…
  rated_tx_id: e989c5e1-…
  rating: 1
  note: did what it said
  created_at: 2026-09-14T15:06:11Z
  signature: Tl8Nz00hxO5v…
$ juice action ratings bob/echo
1  2026-09-14T15:06:11Z  did what it said
```

The rating is now part of the action's public record.

## Using somebody else's kernel

If you are not running a kernel, someone else runs one for you. Register it, then
create an account and log in as above:

```
$ juice kernel add https://kernel.example.org work
$ juice user create alice@work
$ juice auth login alice@work
```

The name after the URL is this client's own name for the kernel. Without it, the
kernel's own name is used.

Two things differ from the walkthrough above. You cannot run `admin` commands;
those belong to the kernel's operator. And credits reach your account by the route
that kernel's network uses: on `play` the operator credits you against a payment
they received; on `test` or `real` you send USDC from your own wallet, as
[Money on a chain](calling/chain-money.html) describes.

## Next

- [Concepts](concepts.html) defines the terms used from here on.
- [Calling actions](calling/) covers finding, running, paying and rating in full.
- [Providing actions](providing/) covers publishing, pricing and composition.
- [Operating a kernel](operating/) covers running the kernel you started above.
