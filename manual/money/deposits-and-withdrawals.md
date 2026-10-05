---
title: Deposits and withdrawals
parent: Money
nav_order: 2
---

# Deposits and withdrawals

A **deposit** moves money from outside Juice into your account; a
**withdrawal** moves it back out. What that money is depends on the kernel's
world. On `play` it is fUSD, a record kept by the operator, and nothing leaves
the kernel. On `polygon` it is a dollar stablecoin held on a blockchain, and
every deposit and withdrawal is a real payment.

This chapter covers `play` briefly, then follows real money on `polygon` from
your bank to your Juice account and back. The other worlds are summarized at
the end.

## On `play`

On `play`, the operator records deposits in the kernel's books, citing a
reference from their own records. No wallet or blockchain is involved, as the
deposit command explains:

```console
$ juice user deposit
Money on the play network has no addresses to send to.
The operator of this kernel records payments here; there is nothing to send from your side.
```

Ask the operator to credit your account; they do so with `admin user deposit`,
as described in [Crediting accounts](../operating/duties.html#crediting-accounts).
A withdrawal on `play` removes fUSD from your balance and sends nothing
anywhere, since fUSD exists only in the kernel's books.

## On `polygon`

### The terms

Real money reaches Juice through several hands. This section introduces them,
then traces the path the money takes.

**Polygon** is a public blockchain: a ledger of payments that no single party
controls. Wallets identify it by its chain ID, 137.

A **wallet** is software that holds the key to a blockchain **address**, a
string such as `0x7099…79C8` that can receive money and send it. MetaMask and
Rabby are common wallets. A wallet is not an exchange account: you control the
address in your wallet, whereas an exchange holds your money in addresses it
controls.

**USDT0** is the money of the `polygon` world. It is a **token**, a currency
recorded on a blockchain, and more precisely a **stablecoin**, a token whose
value is kept at one US dollar, issued by [Tether](https://tether.to). On
Polygon its contract address is `0xc2132D05D31c914a87C6611C10748AEb04B58e8F`.
Polygon's USDT was upgraded to USDT0 at that same address, so some wallets and
services still call it USDT.

**POL** is Polygon's own currency. Every Polygon transaction pays a small fee in
POL, so sending USDT0 also requires a little POL. POL never enters your Juice
balance.

An **on-ramp** converts money from a bank account or card into tokens delivered
to your wallet; an **off-ramp** does the reverse. Exchanges serve both
purposes.

| Term | In short |
|---|---|
| Polygon | the blockchain, chain ID 137 |
| wallet | software controlling your address, such as MetaMask or Rabby |
| address | where your money on Polygon is held, `0x…` |
| USDT0 | the dollar stablecoin Juice counts in on `polygon` |
| POL | Polygon's currency, which pays transaction fees |
| on-ramp, off-ramp | services converting between bank money and tokens |

Money therefore travels from your bank to Juice and back along this path:

```text
bank or card
   ↓  on-ramp
your wallet
   ↓  USDT0 on Polygon
your Juice account
   ↓  withdrawal
your wallet
   ↓  off-ramp
bank
```

### Getting USDT0 and POL

You can buy USDT0 through an on-ramp or an exchange, or receive it from another
wallet. Transak, MoonPay and Ramp are examples of on-ramps; availability, fees
and payment methods depend on your country. Choose Polygon as the network and
have the tokens delivered to your wallet's address. Buy a small amount of POL
in the same way, to pay transaction fees.

If you buy through an exchange, withdraw the tokens to your own wallet before
depositing them in Juice. Juice credits a deposit to the account registered for
the address that sent it, as the next step explains, and a payment sent directly
from an exchange comes from the exchange's address.

{: .warning }
> Check the network and the contract address, not only the token's name.
> Several tokens on Polygon call themselves USDT, and a payment in any of them
> other than `0xc2132D05D31c914a87C6611C10748AEb04B58e8F` is not credited.

### Step 1: register your wallet's address

Juice recognizes your deposits by the address they come from, and sends your
withdrawals to the same address. You therefore register that address once,
proving that you control it:

```console
$ juice user blockchain-address 0x70997970C51812dc3A010C7d01b50e0d17dc79C8
Sign this message with the wallet holding 0x70997970C51812dc3A010C7d01b50e0d17dc79C8:

juice address registration
kernel: qjMgb3LBOwxV…
user: cfeacc90-…
address: 0x70997970C51812dc3A010C7d01b50e0d17dc79C8

Signature:
```

The four lines beginning `juice address registration` are the message. Sign it
with your wallet:

1. Open Polygonscan's [Verified Signatures](https://polygonscan.com/verifiedSignatures)
   page and choose **Sign Message**.
2. Connect the wallet holding the address you are registering.
3. Paste the four lines exactly as printed, with their line breaks and nothing
   added.
4. Sign, and copy the signature, a long string beginning with `0x`.

Polygonscan also offers to publish the signature; that is not needed. Paste the
signature at the `Signature:` prompt, and give your password when asked:

```console
Signature: 0x5d1c…
Password:
  blockchain_address: 0x70997970c51812dc3a010c7d01b50e0d17dc79c8
  attributed: []
```

The signature proves that you hold the address, and the password proves that
the account's owner chose it, since withdrawals will go there. `attributed`
lists any earlier payments from this address that the kernel was holding and
now credits to you. If the message changed while being copied, Juice answers
`signature was not made by that address`; sign it again, copying the four lines
exactly.

Foundry users can sign from a terminal with `cast wallet sign --account NAME
"$MESSAGE"`, which uses a key from Foundry's encrypted keystore. A program
passes the signature and password with `--signature` and `--password` instead
of answering the prompts.

An address can be registered to only one account. Registering a different
address later changes where future withdrawals go; a withdrawal already
requested keeps the address it was made with.

### Step 2: find where to send

```console
$ juice user deposit
Send USDT0 on the polygon network to this kernel at:
  0xcaf2a882af8730c6ad92d76361b1952c71c0453f

Send only this token, and nothing else:
  0xc2132d05d31c914a87c6611c10748aeb04b58e8f  (USDT0)

Pay from your registered address:
  0x70997970c51812dc3a010c7d01b50e0d17dc79c8

Money is credited to whoever finally sent it, so it must arrive from that address.
A payment from any other address, an exchange paying on your behalf included, is held
for the operator to assign by hand: withdraw to your own wallet first, then pay from there.
```

The kernel's address is where all its deposits arrive. The token contract
printed here is the authority on which money the kernel accepts.

### Step 3: send the USDT0

In your wallet, send USDT0 from your registered address to the kernel's address.
Before confirming, check that the network is Polygon, that the token's contract
matches the one printed above, and that the recipient is the kernel's address.
The wallet pays the transaction fee in POL.

{: .warning }
> POL sent to the kernel's address is not a deposit. The kernel uses it to pay
> its own transaction fees, and it credits no account.

### Step 4: check the deposit

The kernel credits a payment once Polygon reports it as final, which happens
shortly after the payment is made, and it looks for new payments once a
minute. A deposit therefore usually appears within about a minute:

```console
$ juice user me
  …
  available: 250.00 USDT0
  …
```

If it does not appear, look up the payment on [Polygonscan](https://polygonscan.com)
by its **transaction hash**, the identifier your wallet shows for the payment.
Check that it succeeded, and that it went on Polygon, in the token printed in
step 2, to the kernel's address, and from your registered address. A payment
from any other address is held by the kernel rather than lost: give the operator
its transaction hash, and they can credit it to your account with
`admin user deposit`.

## Taking money out

```console
$ juice user withdraw 50
Withdraw 50.00 USDT0 on polygon to 0x70997970c51812dc3a010c7d01b50e0d17dc79c8, acting as alice@bank? This cannot be undone. [y/N] y
  id: 58e1e97f-…
  kind: payout
  amount: 50.00 USDT0
  credit: 50.00 USDT0
  destination: 0x70997970c51812dc3a010c7d01b50e0d17dc79c8
  status: submitted
  created_at: 2026-09-15T00:12:42Z
  party: alice@bank
```

A withdrawal sends USDT0 from the kernel to your registered address; without a
registered address it is refused. The amount is reserved from your balance at
once. The kernel pays the Polygon fee, so you receive exactly the amount and need
no POL to receive it. `amount` is what the payment carries and `credit` what
leaves your balance; for a withdrawal the two are equal.

A withdrawal begins as `pending`, becomes `submitted` once the kernel has sent
the payment, and reaches `confirmed` when Polygon reports it as final. If the
payment fails, the reserved amount returns to your balance. The `blocked` status
is described in the next section. Use `user withdrawals` to follow the outcome:

```console
$ juice user withdrawals
  id: 58e1e97f-…
  kind: payout
  amount: 50.00 USDT0
  credit: 50.00 USDT0
  …
```

To turn the USDT0 into money in a bank account, send it from your wallet to an
off-ramp or exchange that accepts USDT on Polygon; Transak is one example. That
transfer is an ordinary Polygon payment, so it needs a little POL for its fee.

{: .warning }
> Withdrawals cannot be undone or recalled. Check the destination in the
> confirmation line before answering it.

If you may need to retry after a lost reply, generate and save a UUID before
requesting the withdrawal, and pass it with `--id`. Repeating the request with
that ID and the same amount returns the existing withdrawal without taking more
money. Without `--id`, the client generates a new UUID each time, so a second
invocation requests a second withdrawal. For unattended withdrawals, `--yes`
supplies the confirmation in advance. See [Retries](../programs.html#retries)
for an example.

## When a payment does not go out

A kernel that cannot pay reports the reason on the withdrawal itself:

```
  status: blocked
  reason: native currency too low, top up: holding 0.00, a refill costs 0.0581…
          — send native currency to 0xcAf2a882aF8730C6ad92D76361b1952C71C0453F
```

A chain's **native currency** is the one its fees are paid in, POL on Polygon.
In this example the kernel has run out of POL for its own fees. The withdrawal
stays reserved and is retried once the cause clears; do not submit a second
withdrawal to replace it. Other causes are the cost of buying more POL, or too
little operator money to buy it with. Deposits, calls, and reads continue while
outgoing payments wait. The operator's remedies are in
[Money on a chain](../operating/duties.html#money-on-a-chain).

## Other worlds

The two Arbitrum worlds follow the same procedure, with Arbitrum in place of
Polygon, ETH in place of POL, and Arbiscan in place of Polygonscan;
`juice user deposit` prints the token contract each one accepts.

| World | Money | Fees paid in | How deposits arrive |
|---|---|---|---|
| `play` | fUSD, with no value | nothing | recorded by the operator |
| `polygon` | USDT0 on Polygon | POL | sent from your registered address |
| `arbitrum-one` | USDT0 on Arbitrum One | ETH | sent from your registered address |
| `arbitrum-sepolia` | a test token on Arbitrum Sepolia, with no value | test ETH | sent from your registered address |

`arbitrum-sepolia` credits a payment as soon as it is included in a block, rather
than waiting until it is final. Its test token has an open `mint` function anyone
may call, and test ETH comes from public faucets.
