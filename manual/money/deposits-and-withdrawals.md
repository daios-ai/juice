---
title: Deposits and withdrawals
parent: Money
nav_order: 2
---

# Deposits and withdrawals

A deposit brings money into your account from outside the system. A withdrawal
takes it back out. They are the two acts you perform yourself to move money across
that edge; transfers, charges for the calls you make, and what you earn all move
money that is already inside.

How you do either depends on the kernel's network, so this chapter covers `play`
first and then the two chain networks.
[What differs between the three networks](index.html#what-differs-between-the-three-networks)
compares them at a glance.

## On `play`

There is nothing to send and nothing to install. The operator credits accounts
against payments they received outside the system and recorded themselves.

```
$ juice user deposit
Money on the play network has no addresses to send to.
The operator of this kernel records payments here; there is nothing to send from your side.
```

Ask them, and they credit you with `admin user deposit`, naming the payment it
stands for. Withdrawing works, and is an entry in the kernel's books rather than a
payment anywhere.

`play` credits are backed by nothing and mean nothing outside that kernel. They
exist so the system can be used and learned without real money.

## On `test` and `real`

Money arrives as a payment on a blockchain and leaves the same way: USDC on
Arbitrum Sepolia for `test`, USDC on Arbitrum One for `real`.

The examples below were run against a local chain standing in for Arbitrum. The
commands, the questions they ask and the messages they print are what you will
see. The addresses are not, and neither are the waiting times, which are given
here from each network's own settings.

### The two assets

You need two things in your wallet, and they do different jobs.

**USDC is the money.** It is what you deposit, what your balance is denominated
in, and what you withdraw. On `real` it is a dollar stablecoin, so a balance of
`250.00 USDC` is two hundred and fifty dollars. On `test` it is a worthless copy
of one, used for rehearsal.

**ETH pays transaction fees.** You need a small amount to pay for your own
transfer into the kernel. It is not money you are depositing, and it never reaches
your balance. Every transaction on an Ethereum network works this way; it is not
something Juice arranges.

### What you need before you start

A wallet on the right chain. Arbitrum One and Arbitrum Sepolia are Ethereum
networks, so any ordinary Ethereum wallet works once you point it at the right
one. You need to be able to do two things with it: sign a message, and send a
token.

| | `test` | `real` |
|---|---|---|
| Chain | Arbitrum Sepolia | Arbitrum One |
| The money | a test token, worth nothing | USDC, real dollars |
| Where it comes from | Sepolia ETH from a public faucet; the test token has an open `mint` anyone may call | bought or transferred like any other USDC |

If you cannot get test funds yourself, ask the operator to send you some. On a
chain nobody can add to your balance without a payment the chain has witnessed,
so there is no way for them to credit you directly; what they can do is pay you,
or pay in on your behalf and attribute it.

{: .warning }
> A symbol is not an identity. Several tokens on a chain call themselves USDC, and
> money sent in the wrong one cannot be recovered. Before your first deposit, get
> the exact token contract address from the kernel's operator and check that your
> wallet is sending that token. The kernel does not yet print it.

### Step 1: register the address you will pay from

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

### Step 2: find out where to send

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

### Step 3: send the USDC

From your own wallet, on that chain, send USDC to the kernel's address. You pay
the transaction fee in ETH, as you would for any transfer.

{: .warning }
> Send USDC, not ETH. The kernel's address takes both, and they are not the same
> thing: USDC is credited to your balance, while ETH pays the kernel's own
> transaction fees and reaches no account at all. Your wallet spends a little ETH
> as the fee for the transfer, which is normal; the amount you *send* must be
> USDC.

{: .warning }
> Send from the address you registered, and only from it. Money that arrives from
> any other sender is held, not credited, until somebody registers that address.
> Withdrawing straight from an exchange does not work, because the exchange is the
> sender and its address is not yours: the payment sits held until the operator
> sorts it out. Move the money to your own wallet first and send it from there.

{: .warning }
> Send it on the chain the kernel named. The right token on a different chain, or
> a different token altogether, does not reach the kernel and cannot be recovered
> through it.

### Step 4: wait

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

## Taking money out

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

On a chain the money goes to the address you registered, and the kernel drives the
payment to completion by itself: you do not confirm it again or push it along. On
`play` the same command moves an entry in the kernel's books and completes at once.

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

`transfer` and `withdraw` ask before acting, because neither can be undone;
`--yes` answers in advance and belongs in scripts rather than at a terminal.
Running a value-bearing action does not ask, because issuing the run is the
authorisation.

## When a payment does not go out

A kernel that cannot pay reports it on the withdrawal itself:

```
  status: blocked
  reason: native currency too low, top up: holding 0.00, a refill costs 0.002306…
          — send native currency to 0xcAf2a882aF8730C6ad92D76361b1952C71C0453F
```

This is the kernel's own problem, not yours. It has run out of the ETH it needs to
pay transaction fees, which its operator supplies. Your money is not lost and your
withdrawal is not cancelled: it is re-presented unchanged and goes out as soon as
the kernel is topped up. See
[Money on a chain](../operating/duties.html#money-on-a-chain).

Deposits, calls and every read carry on normally while payments are blocked. Only
outgoing money waits.
