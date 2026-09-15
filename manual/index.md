---
title: Introduction
nav_order: 1
---

# The Juice manual

Juice is a system for buying and selling calls to software. A unit of service is
called an **action**: it has an owner, a name, a description, typed input and
output, and a price. You find an action, run it with arguments, get a result, and
pay its price. The program that holds accounts, executes actions and settles
payments is a **kernel**. Kernels reach each other over a network, so an action
published on one kernel can be called from another.

Those parts make one exchange, which is the thing to understand first. You hold an
account on a kernel, and its balance pays for every action you call through that
kernel, including actions that live on other kernels: you need no account with the
provider. When an action you published runs successfully, your
earnings are credited to your account, which is an account like any other. You
spend what you have earned on other actions, or take it out.

Four rules govern everything in this manual.

1. **The price is the whole cost.** If an action calls other paid actions, waits
   for a person, or calls an action on another kernel, those costs come out of the
   price you were quoted. You are charged that price and nothing more. Calls to
   another kernel carry one qualification, described in
   [The ticket](calling/running.html#the-ticket).
2. **Every call leaves a record.** A completed call writes an immutable
   transaction and a receipt signed by the kernel that executed it. Neither can be
   altered afterwards. You can verify a receipt without contacting anyone.
3. **Only the payer rates, once.** A rating is permanent, carries an optional
   note, and never changes what was paid.
4. **Money moves in only four ways.** A deposit, a withdrawal, a transfer, or the
   settlement of a call. Each one is recorded at the moment it happens, and the
   record cannot be separated from the movement.

Juice implements the architecture described in the DAIOS vision paper,
[*Web 4.0: A New Paradigm for a Composable, AI-Powered Internet*](https://daios.ai/static/pdf/daios-vision-paper.pdf):
services that compose on demand, paid for per call rather than by advertising or
subscription, with user feedback as the mechanism that vets them.

## Who this manual is for

Readers are either people at a terminal or programs issuing the same commands.
The command-line interface is a client of the kernel's HTTP API and has no private
access to it, so a program can use either. Examples are shown as command lines;
[Using Juice from a program](programs.html) covers what a program must do
differently.

## Where to start

| You want to | Read |
|---|---|
| Call actions other people published | [Getting started](getting-started.html), then [Money](money/) and [Calling actions](calling/) |
| Put money in or take it out | [Deposits and withdrawals](money/deposits-and-withdrawals.html) |
| Publish and sell an action | the above, then [Providing actions](providing/) |
| Write software that uses Juice | the above, then [Using Juice from a program](programs.html) |
| Run a kernel | [Operating a kernel](operating/) |

[Concepts](concepts.html) defines every term the manual uses. Read it after
[Getting started](getting-started.html), or before, if you prefer definitions
first.

## Conventions

Commands are shown after a `$` prompt. Output follows without a prompt.
Identifiers, keys and signatures in output are shortened to `…`; yours will
differ. Long results are abbreviated, marked `…`.

An account is written `handle@kernel`, for example `alice@acme`. An action is
written `owner/name` on your own kernel, and `owner@kernel/name` on another.

Amounts are written in the unit of the kernel's network. Most examples use a
kernel on the `play` network, whose unit is called a credit and which has six
decimal places, so `0.50 credits` is half a credit.

How money gets into an account, and out again, is the [Money](money/) part.

A kernel serves one of three networks, chosen when it is created and fixed for
life. They are a progression rather than three equal options.

- **`play`** has no money in it. The operator creates credits, and they mean
  nothing outside that kernel. Use it to learn the system and to develop against.
- **`test`** is Arbitrum Sepolia, a test network. The machinery is the real one
  and the token is a worthless copy of USDC. Use it to rehearse.
- **`real`** is Arbitrum One, and is what the system is for. The money is USDC, a
  dollar stablecoin, so balances are dollars.

All three count in millionths, so the same number means the same amount on each.

This manual describes what the system does and how to use it. Two other documents
are authoritative on their own subjects and are referenced where relevant:
[`requirements.md`](https://github.com/daios-ai/juice/blob/master/requirements.md)
specifies the kernel, and
[`API.md`](https://github.com/daios-ai/juice/blob/master/API.md) lists every HTTP
route.
