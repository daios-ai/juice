---
title: Introduction
nav_order: 1
---

# Introduction

Juice is an economic peer-to-peer network of capabilities for people and AI
agents. It brings existing web services into a common system in which they can
be discovered, called, paid for, and combined into new services. A provider can
connect an HTTP endpoint or import an API described by an OpenAPI document;
the service itself continues to use the web infrastructure it already runs on.

The unit of service in Juice is an **action**. An action has an owner, a name,
a description, input and output schemas, and a fixed price. A schema describes
the structure and types of the data exchanged with the service. Together, these
elements provide a common interface: what the action does, what information it
needs, what it returns, and what it costs. People and agents can use that
interface to select services and prepare their inputs without learning a
different calling convention for each provider.

Actions can call other actions. For example, a service that prepares a report
might buy data from one provider and a summary from another, then return the
finished report to its caller. Its owner sets a price for the whole service;
the component actions are paid from that budget, and the owner earns the
margin remaining after costs and fees. This allows providers to build and
sell new capabilities using services that others have already supplied.

The server that manages accounts, executes actions, and records payments is
called a **kernel**. You use Juice through an account on a kernel, whose balance
pays for the actions you call. Kernels communicate with one another through
federation, so that account also gives you access to public actions hosted
elsewhere on the same network. You do not need a separate account or payment
arrangement with each provider. If you publish actions yourself, their earnings
reach your account and can be spent on other services or withdrawn.

## Finding and using capabilities

Juice provides natural-language search over the actions available on your kernel
and those it has discovered on other kernels. Each result includes a description,
the input and output schemas, and an advertised price. These let you compare
candidates before deciding which one to run. Remote results are checked against
their home kernel when resolved for use, since a discovery entry may be out of
date.

The kernel can use a language model to support search and action selection. The
operator chooses the model, with a local model as the default arrangement;
keyword search remains available without one. Selecting an action and spending
money on it are separate operations. This separation allows an agent to inspect
or propose a plan before it executes the selected service.

Work need not finish in a single interaction. An action can reserve part of its
budget for a later call and address that work to a particular person or agent.
The work waits until that party supplies the missing input. The kernel retains
the reservation across restarts, allowing services to include approvals,
messages, and responses from external systems.

## Running a kernel

A kernel can run on a laptop behind a home router as well as on a public server.
Other kernels identify it by its public key and can reach it without a public
address or port forwarding. Running a kernel therefore lets you participate in
the network from your own machine, while using somebody else's kernel requires
only a client and an account there.

Providers have several ways to implement an action. An HTTP action connects an
existing endpoint; a WebAssembly action runs code in the kernel's sandbox; and
built-in actions provide common facilities such as search and the clock. These
implementations share the same action interface, so callers use them in the
same way.

## Payment and accountability

For a successful local call, the advertised price covers the action and the work
it performs through other actions. The provider receives the unspent margin,
less the kernel's fee. If the call fails, work already delivered remains paid,
and the unused part of the budget is refunded.

Cross-kernel payments require an additional mechanism because a small service
may cost less than the fee for an individual payment. Juice settles small
obligations by a draw: a larger amount is paid with a probability that makes its
expected value equal to the obligation. Consequently, a particular remote call
can cost more or less than its advertised price, and the caller must have funds
for the stake as well as the execution budget. [Running an action](calling/running.html)
explains these amounts before you make a remote purchase.

Every settled call produces an immutable transaction and a signed receipt. The
records identify the payer, requester, and payee, and account for the charge.
The receipt can be verified against the issuing kernel's signing key without
contacting that kernel. Money movements and their records are committed
together, so the account history accompanies the movement of funds.

The payer can rate a completed call once, with an optional note. Ratings do not
alter the payment. When evidence of a trade is shared between kernels, it
supports the rating without exposing the payer's identity or the call's inputs
and outputs. This gives participants a record of experience with a service,
while preserving the distinction between evidence of a trade and an assurance
of quality.

## The shipped networks

A kernel belongs to one network, named when it is created. This determines the
money it uses and cannot be changed later. A network is a **world**: a file in
`~/.juice/worlds/` naming its money and the servers to meet it through, so a
network this build does not ship is a file you add. The three shipped ones serve
different stages of use:

- **`play`** counts in fUSDT (fake USDT), with six decimal places and no real
  monetary value. The operator records deposits, and no blockchain payment is required. It is suitable for learning
  Juice and developing services.
- **`arbitrum-sepolia`** uses a test token on Arbitrum Sepolia. It lets you rehearse deposits,
  withdrawals, and settlement on a blockchain without using real money.
- **`arbitrum-one`** uses USDT on Arbitrum One. Accounts and prices are denominated in
  USDT, and external payments settle on that chain.

Kernels federate within their own network. All three use six decimal places, but
their balances remain separate and have different monetary value. [Money](money/) explains funding and withdrawals for each.

## Where to start

[Getting started](getting-started.html) walks through a complete exchange on
`play`: starting a kernel, creating accounts, funding a buyer, publishing an
action, and making a purchase. It provides a working example of the concepts
introduced here. The later chapters develop each part in more detail.

| If you want to | Read |
|---|---|
| Buy and run actions other people have published | [Money](money/), then [Calling actions](calling/) |
| Put money into an account, or take it out | [Deposits and withdrawals](money/deposits-and-withdrawals.html) |
| Publish an action and be paid for it | [Providing actions](providing/) |
| Write a program or an agent that uses Juice | [Using Juice from a program](programs.html) |
| Run a kernel of your own | [Operating a kernel](operating/) |

[Concepts](concepts.html) explains the terminology and the relationships among
accounts, actions, calls, and their records. You can read it after the walkthrough
or consult it as those terms arise.

## How to read the examples

Commands appear after a `$` prompt, followed by their output. When a command asks
a question, the example includes both the question and the answer. Long
identifiers, keys, signatures, and omitted portions of output are abbreviated
with `…`; use the complete values returned by your own kernel.

A login is written as an account handle followed by the client's name for its
kernel, as in `alice@acme`. An action on the current kernel is written as
`owner/name`, for example `bob/echo`. A remote action adds its kernel after the
owner's handle: `bob@weather/forecast`. The [identity chapter](calling/identity.html)
explains how these names are assigned and resolved.

Most examples use the `play` network, whose display unit is fUSDT. The
command line accepts amounts such as `0.50`, while the HTTP API and action JSON
use integer base units: `500000` represents the same amount. With six decimal
places, the smallest unit is `0.000001` fUSDT.

## Background and related documents

Juice develops the service and payment infrastructure described in the DAIOS
vision paper, [*Web 4.0: A New Paradigm for a Composable, AI-Powered Internet*](https://daios.ai/static/pdf/daios-vision-paper.pdf).
The paper envisages services composed in response to a user's request, with
providers rewarded per use and human feedback informing future choices.

The kernel supplies discoverable actions, bounded execution budgets, payment,
and records of trade. An agent that interprets a broader request and orchestrates
services uses these facilities as an account holder. Keeping that agent outside
the kernel allows different approaches to orchestration to use the same
execution and payment system.

This manual explains how to use that system. Every command in it is one request
to the kernel over HTTP, and a program can make those requests itself; that is
covered in [Using Juice from a program](programs.html).
