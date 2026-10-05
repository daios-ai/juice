---
title: Money
nav_order: 4
has_children: true
---

# Money

Your account balance funds the actions you call through its kernel, including
public actions hosted by other kernels in the same network. A remote purchase
is settled between the kernels, so you do not need to hold an account with each
provider.

The balance itself belongs to one account on one kernel. If you use two kernels,
you have separate balances to fund and manage. You can transfer credits to
another account on either kernel, provided both kernels belong to the same
world. A recipient on another kernel is credited when the payment reaches it.

An account receives funds through deposits, transfers, and earnings from
actions it owns. A provider's earnings are the margin remaining after the work
it purchased and the kernel's fee, as explained in
[Earnings](../providing/earnings.html). Once credited, those earnings are part
of the ordinary balance and can be spent or withdrawn.

## How `play` and `polygon` differ

The account model and the commands are the same on both standard worlds. Each
counts to six decimal places and records transfers and call charges in the same
way. They differ in what a balance is worth and in how money enters and leaves
the kernel:

| | `play` | `polygon` |
|---|---|---|
| What a balance is | fUSD (fake dollars) recorded by the operator | USDT0, a dollar stablecoin on Polygon |
| What it is worth | nothing | dollars |
| How you deposit | ask the operator; there is nothing to send | send USDT0 from your registered wallet address to the kernel's address |
| When you are credited | when the operator records it | once Polygon reports the payment as final, usually within a minute |
| How you withdraw | the amount leaves your balance; nothing is sent | USDT0 is sent to your registered address |
| Fees the kernel pays | none | POL, for every payment it sends |
| How kernels settle with each other | the buyer's signed message is the payment | a final USDT0 payment on Polygon |

The two Arbitrum worlds work as `polygon` does, on another chain; see
[Other worlds](deposits-and-withdrawals.html#other-worlds).

The operator's responsibilities for transaction fees and remote settlement are
covered in [Funding the kernel](../operating/running-a-kernel.html#funding-the-kernel)
and [The network economy](../operating/network-economy.html).

[Funds](funds.html) explains available and locked balances, what local and
remote actions cost, transfers, and the account ledger.
[Deposits and withdrawals](deposits-and-withdrawals.html) then follows payments
entering and leaving the kernel.

[Running an action](../calling/running.html) covers making a purchase;
[Earnings](../providing/earnings.html) follows what a provider receives.
Automatic settlement between kernels is described in
[The network economy](../operating/network-economy.html).
