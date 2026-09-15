---
title: Operator duties
parent: Operating a kernel
nav_order: 2
---

# Operator duties

The superuser is the account `sys`. It runs the ordinary commands with wider
scope — it sees every account and may disable any action — plus the `admin`
commands below, which nobody else may run.

Authority is the `sys` session alone. Keep it on the kernel's own machine or behind
a TLS front end.

## The one view to read first

```
$ juice admin kernel show
Handle:     acme
Public key: fdlMi64P…
Network:    play
Operator:   earned=0.710005 credits paying-out=4.20 credits unclaimed=0.00 credits held-for-gas=0.00 credits
Solvency:   user-balances=10.00 credits money-in=10.00 credits difference=0.00 credits
Credit:     owed-to-us=0.00 credits limit=500.00 credits
Rates:      fee_bps=2000 remote_bps=500 import_bps=500 lottery=1.00 credits lottery_max=5.00 credits
Listen addresses:
  /ip4/127.0.0.1/tcp/31401/p2p/12D3KooWJHdK…
```

**Operator** splits the `sys` balance. `earned` is yours to spend. `paying-out` is
money committed to payments that have not completed. `unclaimed` is money received
from a sender no account has registered. `held-for-gas` is locked against the cost
of making a chain payment. Only `earned` is spendable.

**Solvency** compares what you owe users against what has come in.
`user-balances` is every positive balance; `money-in` is everything that crossed in
minus everything that crossed out. The difference should be zero, and an alarm
names which term is wrong. What other kernels owe you is not counted here: it is
credit you extended, not money you hold.

**Credit** is the work this kernel has done for other kernels and not been paid
for, against the ceiling you set. See
[Bounding what strangers can cost you](network-economy.html#bounding-what-strangers-can-cost-you).

**Rates** are the money rules this kernel serves under.

## Crediting accounts

Money enters a user's balance only against a payment you received outside the
system.

```
$ juice admin user deposit alice 10 --ref demo-payment-1
Credit 10.00 credits to alice, acting as sys@acme? This cannot be undone. [y/N] y
  amount: 10.00 credits
  operator_handle: sys
  from_handle: sys
  to_handle: alice
```

`--ref` names the payment in your own books: a bank reference, an invoice number,
a transaction hash. The same reference never credits twice, so a repeated command
is safe.

{: .warning }
> Crediting cannot be undone. There is no matching command to take credits back,
> and the money becomes the user's to spend or withdraw. Credit only against a
> payment you have actually received.

On a network with a chain you do not do this by hand for ordinary deposits. The
kernel watches for finalised payments and credits the account that registered the
sending address. Your job is what it cannot attribute:

```
$ juice admin kernel deposits
```

This lists payments received from senders no account has registered, and work
delivered to other kernels that has not been paid for. A payment stays held until
the sender registers the address or you assign it.

Withdrawals need no action from you. A user's withdrawal is paid to the address
they registered, and the kernel drives it to completion.

## Moderation

Suspension is the one moderation tool. It is reversible and works the same way
for a person and for a kernel:

```
$ juice admin user suspend carol
$ juice admin user unsuspend carol
$ juice admin peer suspend beta-kernel
$ juice admin peer unsuspend beta-kernel
```

A suspended account is refused at every authenticated request and its actions
become uncallable and unlisted. Nothing is deleted, and unsuspending restores
everything.

Suspending a peer refuses its requests. It does not change what is true about the
network: the peer's reachability is still recorded, and evidence about it is still
held.

```
$ juice admin user list
$ juice admin user show carol
$ juice admin user rename carol carolyn
```

Renaming is the only way a handle changes. It vacates the old name, which someone
else may then take, which is why anything durable should be keyed by an account's
id rather than its handle.

## Peers

```
$ juice admin peer list
PETNAME       NICKNAME   TRADED  LAST SEEN  LAST FAILED  ACTIONS  PUBLIC KEY
k-hqDr8oMX               yes     just now   never        0        hqDr8oMX…
```

The list merges kernels you have traded with and kernels you have only discovered.

`PETNAME` is the name you gave that kernel; it is the only one of the two names
that resolves. `NICKNAME` is what the kernel calls itself, which is unverified and
not unique. A dash means no petname is bound.

A petname is bound automatically the first time your kernel successfully resolves
an action there. An inbound call from a kernel you have never contacted creates an
account but binds no name, so a stranger cannot take a name on your kernel by
calling you.

Bind one yourself:

```
$ juice admin peer rename hqDr8oMX… beta-kernel
hqDr8oMX… renamed to beta-kernel.
```

The name is taken exactly or refused. An occupied petname is an error rather than
being silently suffixed.

`LAST SEEN` advances when a peer answered. `LAST FAILED` advances only when a
request provably never left. Anything in between — a connection broken after
dispatch, a call still awaiting its receipt — advances neither, because it is
evidence of neither. The kernel draws no conclusion about whether a peer is up;
decide that yourself from the two timestamps.

```
$ juice admin peer inspect beta-kernel
Petname:      beta-kernel
Nickname:     beta
Public key:   hqDr8oMX…
Reachability: direct (0ms)
Traded here:  yes

Public actions (1):
  summarize                       2.205 credits
      Summarize a piece of text
```

Inspect also shows the evidence held about that kernel. It writes nothing, and
degrades to local data when the peer is unreachable.

## Money on a chain

On `test` and `real` the kernel holds money on the chain and pays out of it. Set
it up once, as described in
[Setting up on a chain](running-a-kernel.html#setting-up-on-a-chain); after that
there are three things to watch.

### How the kernel keeps itself in fuel

Every payment the kernel makes costs a fee in ETH, and the kernel buys its own
ETH. You fund it once at setup and it looks after itself after that.

The purchase is an ordinary swap on the chain: the kernel sells some of its USDC
for ETH at the exchange named in the network's world file — a Uniswap V3 pool on
both Arbitrum networks. Nothing is minted and no third party is involved; the
kernel trades like anyone else.

Four rules govern it, and you can see all four in `admin kernel show`.

**When it buys.** Below `gas.min` it buys; at or above it, it pays and leaves the
balance alone. It buys up to `gas.max` rather than back to the floor, so it is not
swapping on every payment. On Arbitrum One those are 0.001 and 0.003 ETH.

**What it spends.** Its own earnings, and only those. The USDC held on behalf of
users is off limits: the rule that decides the purchase refuses to touch it, so a
kernel short of earnings stops buying fuel rather than spending its users' money.
This is why `admin kernel show` splits the operator's balance, and why only
`earned` is yours.

**What it will pay.** Never more than `gas.feeBound` for one purchase, 0.0003 ETH
on Arbitrum One. When the chain is busy and fuel costs more than that, it waits.
It also sets a slippage limit on the trade, `slippageBps`, and abandons a swap
that would cost more than that above the quote.

**How a purchase is recorded.** The kernel writes the purchase down before it
sends it, and makes one at a time. A purchase that never reached the chain is
presented again rather than made twice, and one the chain accepted but the ledger
missed is adopted on the next pass. While a purchase is in flight its authorised
maximum shows as `held-for-gas`; the real cost is settled against it when the
purchase books, so that figure is an upper bound and not a charge.

### Payments that will not go out

Three situations stop the kernel paying, and `admin kernel show` names which one:

```
ALARM: outgoing payments are halted since 2026-09-15T00:12:42Z: native currency
too low, top up: holding 0.00, a refill costs 0.001972… — send native currency to
0xcAf2a882aF8730C6ad92D76361b1952C71C0453F
```

**Out of ETH.** The swap that buys ETH is itself a transaction and needs ETH to
send, so a kernel holding less than the swap costs cannot buy its way out. Send
ETH to the address in the message. This is the state a newly created kernel is in,
and the only one that needs you to send money.

**Fuel is temporarily too expensive.** The purchase would cost more than
`gas.feeBound`, so the kernel waits rather than overpaying. Do nothing; it
resumes when fees fall.

**Not enough USDC.** The kernel cannot cover the payment it is holding, or cannot
cover it and the fuel purchase together. The earnings that accrue meanwhile are
what clear it.

While payments are halted, deposits, calls and every read continue as normal. A
halted withdrawal is not cancelled: it is re-presented unchanged and goes out
when the cause clears.

### Payments nobody has claimed

The kernel credits the account that registered the sending address. Money from an
address nobody has registered is held, and listed:

```
$ juice admin kernel deposits
Payments received whose sender nobody has registered:
  id: rail:0xccb0975d…:0
  kind: deposit
  amount: 40.00 USDC
  status: held
  tx_hash: 0xccb0975d…
  party_handle: 0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc
Work delivered to foreign buyers and not yet paid for:
```

It also appears as `unclaimed` in `admin kernel show`. If the sender is one of
your users, the money reaches them as soon as they register that address with
`juice user address`; nothing needs undoing. The commonest cause is a user paying
from an exchange rather than from their own wallet.

A held payment is not always a user's. Incoming payments are matched against what
other kernels owe this one before anything else, so one of these may turn out to
be another kernel settling its obligations; see
[The network economy](network-economy.html#settling-one-call). ETH arriving at the
same address is the kernel's own fuel and never appears here.

### Holdings and custody

```
Holdings:   240.00 USDC (gas 0.04994…) as of block 28
Operator:   earned=0.00 USDC paying-out=0.00 USDC unclaimed=40.00 USDC held-for-gas=0.00 USDC
Solvency:   user-balances=240.00 USDC money-in=240.00 USDC difference=0.00 USDC
Custody:    the money the rail holds matches the books
```

`Holdings` is what the chain says the kernel has, with its ETH beside it, read at
a block where nothing is in flight. Custody compares that against the books and
reports any difference. Solvency and custody are both displays; neither moves
money.

## What you are risking

Your own earnings, up to the credit limit you set. Serving other kernels is done on
credit, and that credit is bounded across all peers at once, so no number of new
identities increases it. A user's balance is never at risk from work you do for
strangers.
