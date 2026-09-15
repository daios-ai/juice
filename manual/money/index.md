---
title: Money
nav_order: 4
has_children: true
---

# Money

Every account on a kernel has a balance, and that balance pays for every action
you call through that kernel — whether the action belongs to somebody on the same
kernel or to somebody on another one. You need no account with the provider and no
money anywhere but here.

A balance belongs to one account on one kernel. You cannot move it to an account
on another kernel: you spend it through its own kernel, including when what you
are buying is hosted elsewhere. Hold accounts on two kernels and you have two
balances, each funded and spent on its own.

Money reaches an account in three ways. Somebody deposits it from outside the
system. Another account on the same kernel transfers it. Or it is earned: when an
action you published runs successfully, your earnings are credited to the account
that owns it, which is an ordinary account like any other. What you earn is not
the whole price the buyer paid — the kernel takes a fee, and anything your action
bought comes out of it too; [Earnings](../providing/earnings.html) does the
arithmetic. Earnings are not held apart and need no claiming: you spend them on
other actions, or withdraw them.

## What differs between the three networks

Most of what you do with money is the same everywhere. Running an action charges
the same way, a provider earns the same way, a transfer between two accounts on
one kernel is the same command with the same effect, and the ledger records all of
it identically. Amounts are written the same too: all three networks count in
millionths, so `1.50` means the same quantity on each.

What differs is how money gets in and out, and what it is worth.

| | `play` | `test` | `real` |
|---|---|---|---|
| What a balance is | credits the operator creates | a test token on Arbitrum Sepolia | USDC on Arbitrum One |
| What it is worth | nothing | nothing | dollars |
| Register a paying address | refused: this network has no addresses | required, by signing a message | required, by signing a message |
| How you deposit | ask the operator; there is nothing to send | send the token to the kernel's address | send USDC to the kernel's address |
| When you are credited | when the operator records it | once the payment is in a block: seconds | once the block is finalised: about a quarter of an hour |
| What the operator credits against | a payment they received and recorded themselves | a payment the chain has already shown them | a payment the chain has already shown them |
| How you withdraw | an entry in the kernel's books, done at once | a token transfer to your registered address | a token transfer to your registered address |
| Does the kernel need its own ETH | no | yes, for every payment it makes | yes, for every payment it makes |
| How kernels settle with each other | the buyer's signed message is the payment | a finalised token transfer | a finalised token transfer |

[Deposits and withdrawals](deposits-and-withdrawals.html) covers the first six
rows in full. The last two belong to whoever runs the kernel:
[Funding the kernel](../operating/running-a-kernel.html#funding-the-kernel) and
[The network economy](../operating/network-economy.html).

Two chapters cover this. [Funds](funds.html) is money already inside a kernel:
what your balance consists of, how amounts are written, sending money to another
account, and reading the record of it.
[Deposits and withdrawals](deposits-and-withdrawals.html) is the pair of acts that
cross the system's edge — putting money in from outside, and taking it out again.

Two kinds of movement are described elsewhere, because they happen as a
consequence of something else rather than as an act of their own. What a call
costs and what a provider is left with is under
[Running an action](../calling/running.html) and
[Earnings](../providing/earnings.html). Money owed between kernels is settled
automatically, without either operator doing anything, and is explained under
[The network economy](../operating/network-economy.html).
