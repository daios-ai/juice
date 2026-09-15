---
title: Money on a chain
parent: Calling actions
nav_order: 3
---

# Money on a chain

On a kernel whose network is `test` or `real`, money reaches your account as a
payment on a blockchain, and leaves the same way. This chapter is the whole
procedure for depositing and withdrawing, in the order you do it.

If you operate the kernel rather than hold an account on it, the ETH it needs to
make payments at all is a separate matter:
[Funding the kernel](../operating/running-a-kernel.html#funding-the-kernel).

On `play` there is nothing to send and none of this applies. See
[Funds](funds.html#on-play).

The examples below were run against a local chain standing in for Arbitrum. The
commands, the questions they ask and the messages they print are what you will
see. The addresses are not, and neither are the waiting times, which are given
here from each network's own settings.

## The two assets

You need two things in your wallet, and they do different jobs.

**USDC is the money.** It is what you deposit, what your balance is denominated
in, and what you withdraw. On `real` it is a dollar stablecoin, so a balance of
`250.00 USDC` is two hundred and fifty dollars. On `test` it is a worthless copy
of one, used for rehearsal.

**ETH pays transaction fees.** You need a small amount of it to pay for your own
transfer into the kernel. It is not money you are depositing, and the kernel never
sees it. This is how every transaction on an Ethereum network works, and is not
something Juice arranges.

## What you need before you start

A wallet on the right chain. Arbitrum One and Arbitrum Sepolia are Ethereum
networks, so any ordinary Ethereum wallet works once you point it at the right
one. You need to be able to do two things with it: sign a message, and send a
token.

| | `test` | `real` |
|---|---|---|
| Chain | Arbitrum Sepolia | Arbitrum One |
| The money | a test token, worth nothing | USDC, real dollars |
| Where it comes from | Sepolia ETH from a public faucet; the test token has an open `mint` anyone may call | bought or transferred like any other USDC |

If you cannot get test funds, ask the kernel's operator, who can credit you
directly.

## Step 1: register the address you will pay from

The kernel credits whoever the chain says sent the money, so it has to know which
sending address is yours. You establish that by signing a message with the wallet
that holds the address.

```
$ juice user address 0x70997970C51812dc3A010C7d01b50e0d17dc79C8
Sign this message with the wallet holding 0x70997970C51812dc3A010C7d01b50e0d17dc79C8:

juice address registration
kernel: qjMgb3LBOwxV…
user: cfeacc90-…
address: 0x70997970C51812dc3A010C7d01b50e0d17dc79C8

Signature:
```

Copy the message into your wallet's "sign message" function, and paste the
signature it returns at the prompt. The command answers with the address it has
recorded, and with any payments already received from it:

```
  address: 0x70997970c51812dc3a010c7d01b50e0d17dc79c8
  attributed: []
```

A program supplies the signature with `--signature` instead of being asked. If you
work at a command line, `cast wallet sign --private-key … "$MESSAGE"` produces the
same thing.

One address serves one account. To change it, register a new one; a withdrawal
already on its way keeps the destination it was created with.

## Step 2: find out where to send

```
$ juice user deposit
Send real to this kernel at:
  0xcaf2a882af8730c6ad92d76361b1952c71c0453f

Pay from your registered address:
  0x70997970c51812dc3a010c7d01b50e0d17dc79c8

Money is credited to whoever finally sent it, so it must arrive from that address.
An exchange paying this kernel on your behalf would be crediting itself, not you:
withdraw to your own wallet first, then pay from there.
```

Before you have registered an address the same command says so, and nothing else
is needed from you first.

## Step 3: send the USDC

From your own wallet, on that chain, send USDC to the kernel's address. You pay
the transaction fee in ETH, as you would for any transfer.

{: .warning }
> Send from the address you registered, and only from it. Money that arrives from
> any other sender is held, not credited, until somebody registers that address.
> An exchange withdrawal does not work: the exchange is the sender, so the
> exchange would be the one credited. Move the money to your own wallet first and
> send it from there.

{: .warning }
> Send USDC on the chain the kernel named, and nothing else. A different token, or
> the right token on a different chain, does not reach the kernel and cannot be
> recovered through it.

## Step 4: wait

Nothing further is required of you. The kernel watches the chain and credits the
account that registered the sending address, once the payment is final.

"Final" is the network's own rule. On `test` a payment counts as soon as it is in
a block, which is seconds. On `real` the kernel waits for the block to be
finalised, which takes about a quarter of an hour.

```
$ juice user me
  available: 250.00 USDC
  …
```

The credit appears with no further command. If it has not appeared after the
waiting time, the usual reason is that the sender was not the registered address;
the money is held and the operator can see it.

## Withdrawing: taking money out

```
$ juice user withdraw 50
Withdraw 50.00 USDC on real to 0x70997970c51812dc3a010c7d01b50e0d17dc79c8, acting as alice@bank? This cannot be undone. [y/N] y
  id: 58e1e97f-…
  kind: payout
  amount: 50.00 USDC
  credit: 50.00 USDC
  destination: 0x70997970c51812dc3a010c7d01b50e0d17dc79c8
  status: submitted
  created_at: 2026-09-15T00:12:42Z
  party_handle: alice
```

The money goes to the address you registered. The kernel drives the payment to
completion by itself; you do not confirm it again or push it along.

```
$ juice user withdrawals
  id: 58e1e97f-…
  kind: payout
  amount: 50.00 USDC
  credit: 50.00 USDC
  …
```

A withdrawal is `pending` before it is sent, `submitted` once it is on the chain,
and `confirmed` when it is final. `failed` means it did not go through and the
money is back in your balance. `blocked` means the kernel cannot pay right now;
see below.

{: .warning }
> Withdrawals cannot be undone or recalled. Check the destination in the
> confirmation line before answering it.

## When a payment does not go out

A kernel that cannot pay reports it plainly, on the withdrawal itself:

```
  status: blocked
  reason: native currency too low, top up: holding 0.00, a refill costs 0.002306…
          — send native currency to 0xcAf2a882aF8730C6ad92D76361b1952C71C0453F
```

This is the kernel's own problem, not yours: it has run out of the ETH it needs to
pay transaction fees. Your money is not lost and your withdrawal is not cancelled.
It is re-presented as it is, and goes out as soon as the operator tops the kernel
up. See [Operator duties](../operating/duties.html#money-on-a-chain).

Deposits, calls and every read carry on normally while payments are blocked. Only
outgoing money waits.
