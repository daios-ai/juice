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
you have separate balances to fund and manage. Juice provides transfers between
local accounts; it does not provide a direct account transfer across kernels.

An account receives funds through deposits, local transfers, and earnings from
actions it owns. A provider's earnings are the margin remaining after the work
it purchased and the kernel's fee, as explained in
[Earnings](../providing/earnings.html). Once credited, those earnings are part
of the ordinary balance and can be spent or withdrawn.

## What differs between the three networks

The account model and command syntax are shared by `play`, `test`, and `real`.
Each uses six decimal places, and each records local transfers and call charges
in the same way. Their differences concern the value of the balance and the
external payment system that supports deposits, withdrawals, and settlement:

| | `play` | `test` | `real` |
|---|---|---|---|
| What a balance is | fUSDT (fake USDT) recorded by the operator | a test token on Arbitrum Sepolia | USDT on Arbitrum One |
| What it is worth | nothing | nothing | dollars |
| Register a paying address | refused: this network has no addresses | required, by signing a message | required, by signing a message |
| How you deposit | ask the operator; there is nothing to send | send the token to the kernel's address | send USDT to the kernel's address |
| When you are credited | when the operator records it | after block inclusion and detection by the kernel | after finality and detection by the kernel |
| What the operator credits against | a payment they received and recorded themselves | a payment the chain has already shown them | a payment the chain has already shown them |
| How you withdraw | an entry in the kernel's books, done at once | a token transfer to your registered address | a token transfer to your registered address |
| Does the kernel need its own ETH | no | yes, for every payment it makes | yes, for every payment it makes |
| How kernels settle with each other | the buyer's signed message is the payment | a finalised token transfer | a finalised token transfer |

The operator's responsibilities for transaction fees and remote settlement are
covered in [Funding the kernel](../operating/running-a-kernel.html#funding-the-kernel)
and [The network economy](../operating/network-economy.html).

[Funds](funds.html) explains available and locked balances, units, local
transfers, and the account ledger. [Deposits and withdrawals](deposits-and-withdrawals.html)
then follows payments entering and leaving the kernel.

Charges arising from execution are covered in
[Running an action](../calling/running.html) and
[Earnings](../providing/earnings.html). Automatic settlement between kernels
is described in [The network economy](../operating/network-economy.html).
