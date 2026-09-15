---
title: Getting started
nav_order: 2
---

# Getting started

This chapter follows a purchase from both sides: a provider publishes an action,
and a buyer finds it, runs it, and checks the resulting payment. You will run a
kernel on the `play` network and create accounts for both participants. Since
`play` uses credits with no real monetary value, the walkthrough requires no
blockchain wallet or payment.

The examples include the commands, their output, and the answers to interactive
prompts. Keep the complete identifiers returned by your kernel; the printed
examples abbreviate them with `…`. If you intend to use a kernel run by someone
else, the final section explains how that changes the setup.

## Install

Juice needs Go 1.25 or later.

```
$ git clone https://github.com/daios-ai/juice.git
$ cd juice
$ make build
```

The build produces `./juice` in the repository. Run `make install` to copy it to
`~/.local/bin`, and make sure that directory is on your command search path.
The examples below use the installed command, `juice`.

## Start a kernel

A kernel needs a name when it is first created. That name determines its local
directory and supplies the nickname it initially reports to other kernels.
In this walkthrough, the kernel is called `acme` and accepts HTTP clients on
port 4040:

```
$ juice kernel serve acme --addr :4040
```

Because this is the first start of `acme`, the command asks you to confirm its
creation and select its network. Choose `play` for this walkthrough:

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
> The network is fixed when the kernel is created. To use another network later,
> create a separate kernel.

The kernel remains running in this terminal. Open a second terminal for the
client commands that follow, leaving the first available for server output.

First boot also creates the superuser account, `sys`. You will use it to fund
the buyer's account on `play`; it also gives the operator access to account
records and administrative commands.

## Tell the client about the kernel

The `juice` executable provides both the server and its command-line client.
Starting the server does not register it with the client. Add its HTTP address
so that subsequent commands can refer to it by name:

```
$ juice kernel add http://localhost:4040
acme  network play  http://localhost:4040  L3ciw7zj…  (added)
```

The client contacts the address and records the kernel's public key and network.
Since this command supplies no local name, it uses the nickname reported by the
kernel, `acme`.

## Create an account and log in

Create the buyer's account, `alice`, on the registered kernel. The combined name
`alice@acme` tells the client where to create the account:

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

Save Alice's recovery phrase as you did the superuser's. Juice has no email
recovery, so this phrase is needed if the password is lost. Account creation
and login are separate operations; authenticate as Alice next:

```
$ juice auth login alice@acme
Password:
alice@acme
```

The client now selects `alice@acme` as the current login. Subsequent commands
act as Alice on `acme` until you switch to another login. Inspect the account to
confirm its identity and initial balance:

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

You can check execution before adding funds by calling `sys/time`, one of the
built-in actions. It is free under the default configuration and returns the
kernel's current time:

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

## Put money in the account

Alice will need funds for the paid action later in the walkthrough. Her balance
can pay for services on `acme` and for public services on other kernels in the
same network.

On `play`, the operator records deposits directly, using a reference from their
own records. Here, `demo-payment-1` identifies the demonstration deposit. Log
in as `sys` to credit Alice with ten credits. On a chain network, funding
instead requires a token payment, as described in
[Deposits and withdrawals](money/deposits-and-withdrawals.html).

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

The reference prevents the same deposit from being credited twice if the command
is repeated. Both logins are now saved in the client, so you can return to Alice
without entering her password again:

```
$ juice auth use alice@acme
alice@acme
```

## Buy something

The purchase uses a second account, `bob`, as the provider. Bob will publish an
action that wraps `https://httpbin.org/post`, a public endpoint that echoes the
data it receives. This gives Alice a service to find and buy, and lets you inspect
both the charge and the provider's earnings. The endpoint requires an internet
connection.

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

As Bob, register the endpoint with a description, a price of half a credit, and
an input schema requiring a text field named `msg`:

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

Registration creates an inactive, private action. Enable it to allow execution,
then choose `local` visibility so that Alice and other users of `acme` can call
it:

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

The search result contains the reference to call, its description and schemas,
and its price. Because this is an action's JSON result, `price` uses integer
base units: `500000` represents `0.50 credits`. Use the returned reference to
send Bob's action a message:

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

After the successful call, Alice's available balance has decreased by the
advertised half credit:

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

The transaction accounts for the half credit Alice paid: Bob receives `0.40`,
and the kernel receives `0.10`. Since the action bought no further work, its
whole price is margin, on which the default fee is 20%.
[Earnings](providing/earnings.html) extends this calculation to composed actions.

Alice can now rate the call because she paid for it. A rating can be submitted
only once and may include a note:

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

The rating is available to readers who can see the action. It remains attached
to this call and does not alter the payment.

## Using somebody else's kernel

To use an existing kernel, begin by registering the address its operator gives
you. You can then create an account and log in without running a server:

```
$ juice kernel add https://kernel.example.org work
$ juice user create alice@work
$ juice auth login alice@work
```

The name after the URL is this client's own name for the kernel. Without it, the
kernel's own name is used.

The operator handles the administrative work performed by `sys` in the
walkthrough. Your funding method depends on that kernel's network: the operator
records deposits on `play`, while chain networks accept payments from your
wallet. See [Deposits and withdrawals](money/deposits-and-withdrawals.html)
before sending funds.

## Next

- [Concepts](concepts.html) defines the terms used from here on.
- [Money](money/) covers balances, deposits and withdrawals in full, including
  putting real money into an account on `test` or `real`.
- [Calling actions](calling/) covers finding, running, paying and rating.
- [Providing actions](providing/) covers publishing, pricing and composition.
- [Operating a kernel](operating/) covers running the kernel you started above.
