---
title: Concepts
nav_order: 3
---

# Concepts

Juice connects accounts, services, and payments through a small set of concepts.
This chapter introduces their meanings and relationships. The task chapters
show how to work with them at the command line.

## Accounts and identity

A **kernel** is a server that manages accounts, hosts actions, and records their
execution and payment. Its public key identifies it to other kernels. It also
has a **nickname**, chosen by its operator, which provides a readable label but
need not be unique.

An **account** holds a balance and a history on one kernel. A user account has a
**handle**, such as `alice`, that is unique on that kernel. The same handle on
another kernel refers to a different account. Kernels also keep accounts that
represent their peers for recording and authorizing trade; these accounts have
no user login and hold no money.

A **login** is the client's saved session for a user account. It is written as
`handle@kernel` and determines both the account a command acts as and the kernel
it contacts. The kernel name in a login is the name registered by that client,
which may differ from the kernel's nickname.

Each kernel has a **superuser** account named `sys`. The operator uses this
account to administer money and access, and can inspect records across the
kernel. Ordinary users see the records their own roles permit them to read.

A **petname** is a name one kernel assigns to another for use in remote
references. It is meaningful only on the assigning kernel. Since the remote
kernel cannot choose this name, it cannot claim an existing local name merely
by announcing a matching nickname.

A **world**, also called a **network**, defines the money and external payment
system a kernel uses. It is a file in `~/.juice/worlds/`, named by that file, and
a kernel serves the world it is started with. The shipped worlds are `play`,
which counts in fUSD (fake dollars), with six decimal places and no real monetary
value; `arbitrum-sepolia`, which uses a test token on Arbitrum Sepolia; and
`arbitrum-one` and `polygon`, which use USDT0 on Arbitrum One and on Polygon. A world you write yourself is a
network of its own. The choice is fixed when the kernel is created, and kernels
federate only within the same network.

## Actions

An **action** is a service that can be called through Juice. Its description
states what it does, its input and output schemas describe the data it accepts
and returns, and its price states the execution budget. Together with its owner
and name, these form the interface a caller uses to find and select the service.

An action's **kind** specifies how it runs. An `http` action calls a web endpoint;
a `wasm` action runs a WebAssembly module in the kernel's sandbox; and a `native`
action uses a built-in handler. A `remote_proxy` is the local cache of an action
hosted on another kernel. These kinds share the same calling interface.

A **reference** names an action as `owner@kernel/name`, on this kernel and on any
other alike. The kernel is named as this kernel knows it: its own name for its own
actions, a petname or public key for another kernel's. If the reference names no action directly, Juice tries its `index` child:
`bob` can resolve to `bob/index`, and `bob/mail` to `bob/mail/index`. This
convention gives a group of related actions an entry point.

**Visibility** determines an action's audience. A `private` action is available
to its owner, a `local` action to users of its kernel, and a `public` action to
callers across the network. Activity is a separate setting: an inactive action
cannot be called, regardless of its visibility.

An **application** is a collection of actions imported from one OpenAPI document
under a common name. Each operation becomes an ordinary action. An operation
named `index` can provide the application's entry point through the reference
convention above.

## Calls and their records

A **call** is an execution of one action. A user starts a call with `run`;
the action may then call further actions to perform parts of its work.

A **process** groups the computation initiated by one `run` and holds the funds
reserved for it. It includes the initial call, calls made within it, and any
work awaiting input. The process closes when all its work has finished, returning
any remaining funds to its owner.

A **trace** represents an individual call within that process and holds its
share of the budget. The initial call has a root trace. Each further call has a
child trace linked to the call that requested it. These links let you follow
the execution and let the kernel account for spending within each budget.

A **step** reserves a future action call for completion by a named party. Its
creator supplies the arguments already known and reserves the execution price;
the named party later supplies the missing input. The creating action can return
while the step waits, but its process remains open. Completion executes the
step's target action using the reserved funds.

A **transaction** records a settled call: its payer, requester, and payee, its
arguments and result, its outcome, and the amounts charged or refunded. Once
committed, this record cannot be changed. A request rejected before execution,
such as one with invalid input, does not produce a call transaction.

A **receipt** is the kernel's signed record of a call's outcome and charge. It
contains hashes of the input and output and can be verified against the issuing
kernel's key without contacting that kernel. For a remote call, the local
transaction also retains the remote receipt used to settle it.

A **rating** records the payer's assessment of a completed call as `0` or `1`,
with an optional note. A call may be rated once. Ratings are permanent and can
be read wherever the action is visible, without disclosing the rater's identity.

## Money

An account's **available** balance is money it can spend. Its **locked** balance
is reserved for commitments, including running calls, waiting steps, and stakes
for remote calls. Settlement pays for the completed work and releases unused
reservations. The operator's account also holds funds committed to external
payments and fuel purchases.

The **ledger** records deposits, withdrawals, transfers, value delivered by
actions, and settlement postings such as provider payouts and fees. Each
settlement posting names its transaction, which records the work and its cost.

**Base units** are the integer amounts used by the HTTP API and action JSON.
**Display units** are the amounts accepted and shown by the command line.
The shipped networks use six decimal places, so `500000` base units correspond
to `0.50` fUSD on `play`, or `0.50` tokens on a chain network.

A **peer** is another kernel known to yours. A **counterparty** is a peer for
which your kernel holds an account, allowing it to authorize requests and record
trade. Discovering a peer does not by itself create such an account.

**Exposure** measures the value of work delivered to foreign buyers less the
cash received for that work. One credit limit bounds admission across all peers.
Because small obligations settle by a draw, exposure can remain after a losing
draw or become negative after a payment larger than the obligation it settles.
