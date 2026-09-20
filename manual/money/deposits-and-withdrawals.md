---
title: Deposits and withdrawals
parent: Money
nav_order: 2
---

# Deposits and withdrawals

A deposit credits your account against an external payment; a withdrawal sends
funds out of that account. Their external form depends on the kernel's network.
On `play`, they are records of fUSDT (fake USDT). On a chain network, they correspond
to token payments confirmed by the blockchain.

This chapter first covers `play`, then follows a chain deposit from address
registration to the resulting account credit. See
[What differs between the three networks](index.html#what-differs-between-the-three-networks)
for an overview of their payment arrangements.

## On `play`

On `play`, the operator records deposits using references from their own books.
No wallet or blockchain transaction is involved. The deposit command explains
this arrangement:

```
$ juice user deposit
Money on the play network has no addresses to send to.
The operator of this kernel records payments here; there is nothing to send from your side.
```

Ask the operator to credit your account. They use `admin user deposit` with a
reference identifying the deposit. A withdrawal likewise updates the kernel's
records without making an external payment. These funds have no real monetary
value, allowing you to learn and test the system without handling funds on a
blockchain.

## On `arbitrum-sepolia` and `arbitrum-one`

On `arbitrum-sepolia` and `arbitrum-one`, deposits and withdrawals use the token specified by the
network: a test token on Arbitrum Sepolia, or USDT on Arbitrum One.

The following examples use a local chain to demonstrate the commands and their
output. Substitute the addresses returned by your kernel and wallet. The
waiting periods described in the prose refer to the shipped networks rather
than the local demonstration chain.

### The two assets

Your wallet needs the network's token for the deposit and ETH for the
transaction fee. They serve different purposes.

**USDT** is the unit used for account balances on `arbitrum-one`. A deposit of
`250.00 USDT` credits that amount to the account, and a withdrawal pays USDT
back to the registered address. The `arbitrum-sepolia` network uses a test token with no
real monetary value.

**ETH** pays the blockchain fee for sending the deposit. This fee is spent by
your wallet in addition to the token amount and is not credited to your Juice
balance. The kernel pays its own blockchain fees when sending withdrawals.

### What you need before you start

Use a wallet configured for the kernel's chain that can sign a message and
send the required token. Message signing proves ownership of your address;
the token transfer supplies the deposit.

| | `arbitrum-sepolia` | `arbitrum-one` |
|---|---|---|
| Chain | Arbitrum Sepolia | Arbitrum One |
| The money | a test token, worth nothing | USDT, real dollars |
| Where it comes from | Sepolia ETH from a public faucet; the test token has an open `mint` anyone may call | bought or transferred like any other USDT |

If you need test funds, the operator may be able to supply them. Even on `arbitrum-sepolia`,
an account credit must be supported by a witnessed payment. The operator can
send you tokens or arrange and attribute a payment on your behalf.

{: .warning }
> Check the token's contract address before sending a deposit. A token symbol
> such as USDT does not uniquely identify it: one chain carries several tokens of
> that name. `juice user deposit` prints the contract this kernel takes; send that
> one. Payments in another token are not credited through this deposit procedure.

### Step 1: register the address you will pay from

The kernel attributes a deposit by its sender address. Register the address you
will pay from before sending funds, proving control by signing the kernel's
registration message with that wallet:

```
$ juice user address 0x70997970C51812dc3A010C7d01b50e0d17dc79C8
Sign this message with the wallet holding 0x70997970C51812dc3A010C7d01b50e0d17dc79C8:

juice address registration
kernel: qjMgb3LBOwxV…
user: cfeacc90-…
address: 0x70997970C51812dc3A010C7d01b50e0d17dc79C8

Signature:
```

Copy the complete message into your wallet's message-signing function, then
paste the resulting signature at the prompt. The response confirms the
registered address and lists any held deposits attributed to it:

```
  address: 0x70997970c51812dc3a010c7d01b50e0d17dc79c8
  attributed: []
```

A program supplies the signature with `--signature` instead of being asked. If you
work at a command line, `cast wallet sign --private-key … "$MESSAGE"` produces the
same thing.

An address can be registered to only one account. Registering a replacement
changes the destination of future withdrawals; an existing withdrawal retains
the address recorded when it was requested.

### Step 2: find out where to send

```
$ juice user deposit
Send arbitrum-one to this kernel at:
  0xcaf2a882af8730c6ad92d76361b1952c71c0453f

Send only this token, and nothing else:
  0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9  (USDT)

Pay from your registered address:
  0x70997970c51812dc3a010c7d01b50e0d17dc79c8

Money is credited to whoever finally sent it, so it must arrive from that address.
A payment from any other address, an exchange paying on your behalf included, is held
for the operator to assign by hand: withdraw to your own wallet first, then pay from there.
```

You can also run this command before registration. It will report that a sender
address still needs to be registered.

### Step 3: send the USDT

From your own wallet, on that chain, send USDT to the kernel's address. You pay
the transaction fee in ETH, as you would for any transfer.

{: .warning }
> Select the token by the contract address `juice user deposit` printed, not by
> its symbol. Your wallet will also spend ETH on the transaction fee, but ETH sent
> directly to the kernel supplies its fuel and does not credit your account.

{: .warning }
> Send from your registered address. A direct withdrawal from an exchange names
> the exchange as sender, leaving the payment held for attribution. Withdraw to
> your own wallet first, then send the deposit from that wallet.

{: .warning }
> Check that the wallet is using the kernel's chain. A payment on another chain
> will not be recognized as a deposit by this kernel.

### Step 4: wait

After sending the token, the kernel detects the payment and waits for the
network's required confirmation. It then credits the account registered to the
sender address without a further command from you.

The shipped `arbitrum-sepolia` world accepts a payment once it is included in a block.
The `arbitrum-one` world waits for finality, so confirmation takes longer. Actual
waiting times depend on the chain and the kernel's progress reading it.

```
$ juice user me
  available: 250.00 USDT
  …
```

If the expected credit has not appeared, check that the payment used the right
chain, token, destination, and registered sender. The operator can inspect held
payments and the kernel's view of chain progress.

## Taking money out

```
$ juice user withdraw 50
Withdraw 50.00 USDT on arbitrum-one to 0x70997970c51812dc3a010c7d01b50e0d17dc79c8, acting as alice@bank? This cannot be undone. [y/N] y
  id: 58e1e97f-…
  kind: payout
  amount: 50.00 USDT
  credit: 50.00 USDT
  destination: 0x70997970c51812dc3a010c7d01b50e0d17dc79c8
  status: submitted
  created_at: 2026-09-15T00:12:42Z
  party_handle: alice
```

On a chain network, the withdrawal reserves the amount from your balance and
uses your registered address as its destination. The kernel sends and confirms
the payment automatically. On `play`, the same operation completes through the
manual payment records. Use `user withdrawals` to follow the outcome:

```
$ juice user withdrawals
  id: 58e1e97f-…
  kind: payout
  amount: 50.00 USDT
  credit: 50.00 USDT
  …
```

A withdrawal begins as `pending`, becomes `submitted` after submission to the
rail, and reaches `confirmed` when payment is final. A finalized failure returns
the reservation to your balance. The `blocked` status means the kernel cannot
currently proceed, as described below.

{: .warning }
> Withdrawals cannot be undone or recalled. Check the destination in the
> confirmation line before answering it.

For unattended withdrawals, `--yes` supplies the confirmation in advance.
This is the same convention used for local transfers. Calls to value-bearing
actions differ: issuing `run` itself authorizes the value named in its input.

## When a payment does not go out

A kernel that cannot pay reports it on the withdrawal itself:

```
  status: blocked
  reason: native currency too low, top up: holding 0.00, a refill costs 0.002306…
          — send native currency to 0xcAf2a882aF8730C6ad92D76361b1952C71C0453F
```

In this example, the kernel lacks enough ETH to pay its transaction fees.
The withdrawal remains reserved and is retried when the cause clears; you
should not submit a second withdrawal to replace it. The operator can inspect
and address the cause using the procedures in
[Money on a chain](../operating/duties.html#money-on-a-chain).

Other causes include the cost of a fuel purchase or insufficient operator funds
for it. A halt affects outgoing rail work, while deposits, calls, and reads
continue.
