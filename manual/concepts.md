---
title: Concepts
nav_order: 3
---

# Concepts

This chapter defines the terms the rest of the manual uses. It describes what
each thing is, not how to use it.

## Accounts and identity

**Kernel.** One server, together with the accounts it holds, the actions
published on it, its ledger, its signing key, and the network it belongs to. A kernel's identity is
its public key. Its **nickname** is the name it calls itself on the network; that
name is not unique and proves nothing.

**Account.** A principal on one kernel, with a balance and a history. Accounts
are either user accounts, which have a handle and a password, or kernel accounts,
which stand for another kernel and exist only so that trade with it can be
recorded.

**Handle.** An account's name on its kernel, unique there. A handle is bare:
`alice`, not `@alice`.

**Login.** One account at one kernel, written `handle@kernel`, holding that
session's tokens. A login says both who a command acts as and which kernel it
acts on. The `kernel` half is this client's own name for that kernel and is not
necessarily its nickname.

**Superuser.** The account `sys`, of which each kernel has one. It performs the
operator's money and moderation commands and can read every account.

**Petname.** A name one kernel gives another kernel locally. It resolves only on
the kernel that assigned it, and is never transmitted. A remote kernel cannot
claim a petname on yours.

**World, or network.** The money a kernel deals in, fixed when the kernel is
created. `play` has no money: the operator creates credits and they mean nothing
elsewhere. `test` is Arbitrum Sepolia, carrying a worthless copy of USDC for
rehearsal. `real` is Arbitrum One, where the money is USDC and balances are
dollars. Kernels on different networks never meet.

## Actions

**Action.** A callable unit of service with an owner, a name, a description,
typed input and output, and a price. Its **kind** says how it executes: `http` (a
web endpoint), `wasm` (a WebAssembly module the kernel runs), `native` (one of
the kernel's built-in `sys` actions), or `remote_proxy` (the kernel's cached
handle on another kernel's action).

**Reference.** How an action is named: `owner/name` on your own kernel,
`owner@kernel/name` on another, where `kernel` is a petname or a public key. A
reference that names no action resolves to that path's `index` child instead, at
any depth: `bob` reaches `bob/index`, `bob/mail` reaches `bob/mail/index`.

**Visibility.** Who may call an action: `private` (its owner), `local` (accounts
on the same kernel), `public` (anyone, including other kernels). An action is also
either active or inactive; an inactive action cannot be called by anyone.

**Application.** A set of actions installed together from one OpenAPI document
under one name. It is a naming convention, not an object: the actions are
ordinary actions and the document's `index` operation is the root.

## Calls and their records

**Call.** One execution of one action. A call by a user is started with `run`.

**Process.** The wallet of one `run`. It is created when the run starts, holds
the money set aside for it, and closes by itself when the work is finished and
nothing is outstanding. Closing returns what was not spent.

**Trace.** One call inside a process, with its own share of the process's money.
The root trace is the call you asked for; a call it makes in turn gets a child
trace. Traces are how the kernel keeps the price a bound on everything beneath.

**Step.** A call that has been set aside, paid for in advance, and addressed to
one named party. It waits until that party supplies the missing input, then runs
and settles. Nobody else can complete it and it cannot complete twice.

**Transaction.** The immutable record of one attempted call: who paid, who asked,
who was paid, the arguments, the result, the amounts, and whether it succeeded.

**Receipt.** The signed form of that record, issued by the kernel that executed
the call. A receipt can be checked against the signing key of the kernel that
issued it, without contacting it.

**Rating.** A `0` or `1` with an optional note, written once by the account that
paid for a call. Ratings are permanent and public wherever the action is visible.

## Money

**Available and locked.** Available money can be spent. Locked money is committed
to work in progress: the price of a call that is running, the price parked for a
waiting step, or a stake held against a call to another kernel. When the work
settles, what it consumed is paid out and the rest returns to available.

**Ledger.** The record of money entering, leaving, or moving between accounts:
deposits, withdrawals, transfers, and value delivered by an action. Payments for
executing an action are not ledger entries; they are transactions.

**Base units and display units.** Internally every amount is a whole number of
base units. The `play`, `test` and `real` networks all have six decimal places,
so one credit is 1,000,000 base units. The command line takes and prints display
units (`0.50 credits`); the HTTP API and the JSON arguments of an action use base
units (`500000`).

**Peer and counterparty.** A peer is any kernel yours knows about. A counterparty
is a peer yours has traded with, which therefore has an account on your kernel.

**Exposure.** The total work a kernel has performed for other kernels and not yet
been paid for. It is capped by one credit limit for all peers together.
