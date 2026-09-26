---
title: Operator duties
parent: Operating a kernel
nav_order: 3
---

# Operator duties

The operator administers the kernel through the `sys` account. This account can
inspect records across users and disable actions, as well as run the dedicated
`admin` commands for money, moderation, and peers.

Administrative authority comes from the `sys` session. Protect its credentials
and use a local connection or TLS when accessing the kernel remotely.

## The one view to read first

Begin with `admin kernel show` for an overview of the kernel's identity,
funds, configured rates, and exposure to remote trade:

```
$ juice admin kernel show
Handle:     acme
Public key: fdlMi64P…
Network:    play
Operator:   earned=0.710005 fUSD paying-out=4.20 fUSD unclaimed=0.00 fUSD held-for-gas=0.00 fUSD
Solvency:   user-balances=10.00 fUSD money-in=10.00 fUSD difference=0.00 fUSD
Credit:     owed-to-us=0.00 fUSD limit=50.00 fUSD
Fees:       20% of each layer's margin here, 5% on work served to another kernel, 5% on work imported from one
Tickets:    this kernel draws for 1.00 fUSD, and accepts tickets up to 5.00 fUSD
Listen addresses:
  /ip4/127.0.0.1/tcp/31401/p2p/12D3KooWJHdK…
```

The **Operator** line separates spendable earnings from funds held for other
purposes. `paying-out` covers payments in progress, `unclaimed` covers received
payments awaiting attribution, and `held-for-gas` covers fuel purchases. Only
`earned` is available for ordinary operator spending.

**Solvency** compares the kernel's account liabilities with net external
receipts. The display calls these `user-balances` and `money-in`: the former
counts all positive balances, including `sys` and reserved funds, and the latter
counts crossings in less crossings out. Their difference should be zero.
Receivables from other kernels do not
count as backing, because the payment has not yet been received.

The **Credit** line reports exposure against the configured admission limit.
Its meaning is developed in
[Bounding what strangers can cost you](network-economy.html#bounding-what-strangers-can-cost-you).
The **Fees** and **Tickets** lines report the rates and ticket settings used by the kernel.

## Crediting accounts

On the manual `play` rail, credit an account by recording a deposit and its
external reference:

```
$ juice admin user deposit alice@acme 10 --ref demo-payment-1
Credit 10.00 fUSD to alice, acting as sys@acme? This cannot be undone. [y/N] y
  amount: 10.00 fUSD
  operator_handle: sys
  from: sys@acme
  to: alice@acme
```

The `--ref` value identifies the payment in your records. Repeating the same
deposit reference does not credit it again, allowing a retry to recover the
existing result.

{: .warning }
> Crediting cannot be undone. There is no matching command to take credits back,
> and the money becomes the user's to spend or withdraw. Credit only against a
> payment you have actually received.

On a chain network, the kernel detects finalized payments and credits known
sender addresses automatically. Operator attribution is needed when a received
payment cannot yet be assigned:

```
$ juice admin kernel deposits
```

The listing includes held incoming payments and unpaid remote obligations.
A user's held deposit can be attributed when they register its sender address,
or assigned by the operator against the witnessed payment. A chain deposit
cannot be created merely by declaring a new reference.

Users request their own withdrawals. The kernel sends each to its recorded
destination and follows it to completion without an additional operator
approval.

## Moderation

Suspension prevents an account from making authenticated requests while keeping
its records intact. Separate user and peer commands identify the kind of account
being moderated:

```
$ juice admin user suspend carol@acme
$ juice admin user unsuspend carol@acme
$ juice admin peer suspend beta-kernel
$ juice admin peer unsuspend beta-kernel
```

A suspended user's actions also become unavailable for calls and listings.
Unsuspending restores access with balances and history preserved. Suspending
a peer refuses its requests but does not erase its evidence or prevent the
kernel from recording observations of its reachability.

```
$ juice admin user list
$ juice admin user show carol@acme
$ juice admin user rename carol@acme carolyn@acme
```

The operator can rename a user through the dedicated rename command. The
account ID and history remain the same, while the old handle becomes available
for reuse. Programs keeping durable references should therefore store the ID.

## Peers

```
$ juice admin peer list
PETNAME     NICKNAME  TRADED  LAST SEEN  LAST FAILED  ACTIONS  STATUS  PUBLIC KEY
k-hqDr8oMX  —         yes     just now   never        0                hqDr8oMX…
```

The roster combines known counterparties with kernels learned through discovery.
`PETNAME` is the local name usable in references, while `NICKNAME` is the label
reported by the peer. A dash in the petname column means the peer must be
addressed by key. With `--all`, suspended peers are included and marked in
the `STATUS` column.

A successful outbound action resolution can assign a petname automatically.
An incoming call may provision an account but does not assign a local name,
preventing a remote caller from claiming a petname by its own choice. To assign
one explicitly, use:

```
$ juice admin peer rename hqDr8oMX… beta-kernel
hqDr8oMX… renamed to beta-kernel.
```

Explicit renaming requires the chosen petname to be available. Unlike automatic
naming, it does not add a suffix to resolve a collision.

The contact columns report observations rather than a current online status.
`LAST SEEN` records a reply, and `LAST FAILED` records a request known not to
have reached the peer. A connection lost after dispatch provides neither kind
of evidence and advances neither timestamp. Inspect a peer for more detail:

```
$ juice admin peer inspect beta-kernel
Petname:      beta-kernel
Nickname:     beta
Public key:   hqDr8oMX…
Reachability: direct (0ms)
Traded here:  yes

Public actions (1):
  summarize                       2.205 fUSD
      Summarize a piece of text
```

Inspection includes retained trade evidence and can fall back to cached
information if the peer is unreachable. It does not update the stored contact
observations.

## Money on a chain

On `arbitrum-sepolia` and `arbitrum-one`, the kernel holds tokens and sends payments on the chain.
After [initial setup](running-a-kernel.html#setting-up-on-a-chain), supervision
centres on fuel, blocked payments, unattributed deposits, and the agreement
between custody and the account books.

### How the kernel keeps itself in fuel

The funding model and initial deposits are explained in
[Setting up on a chain](running-a-kernel.html#setting-up-on-a-chain).
Refills spend available `sys` USDT0 through the venue configured in the world
file. The shipped chain worlds use Uniswap V3.

The refill policy has a lower threshold, `gas.min`, and a target, `gas.max`.
When an outgoing payment requires a refill, the rail attempts to buy enough ETH
to reach the target. On Arbitrum One the shipped values are 0.001 and 0.003 ETH;
on Polygon, where fuel is POL, they are 5 and 15 POL.
Buying above the threshold reduces the need to refill on every payment.

Fuel is an operator expense. The reservation excludes USDT0 backing other
accounts, so a shortage of operator earnings can block a refill without using
those balances. The `gas.feeBound` setting limits the purchase's transaction
fee, while `slippageBps` limits the swap's deviation from its quote.

Before requesting a purchase, the kernel locks the operator funds available
for it. Once the purchase's authorized maximum is known, the lock is adjusted
to that maximum; final booking charges the actual cost and releases the rest.
The `held-for-gas` figure is therefore a reservation rather than a completed
expense.

The rail records a purchase before broadcast and handles one at a time.
An unbroadcast purchase can be presented again, and a purchase missing from
the kernel's books can be recovered from the rail's durable record. These
steps let recovery continue an existing purchase without creating a duplicate.

### Payments that will not go out

When a payment becomes blocked, `admin kernel show` reports the cause and
the age of the halt. For example:

```
ALARM: outgoing payments are halted since 2026-09-15T00:12:42Z: native currency
too low, top up: holding 0.00, a refill costs 0.001972… — send native currency to
0xcAf2a882aF8730C6ad92D76361b1952C71C0453F
```

An **ETH shortage** can prevent even the refill transaction from being sent.
In that case, send ETH to the address in the message. This may be necessary
both at initial setup and after the kernel has exhausted its fee balance.

A **fee-bound failure** means the refill would exceed the configured transaction
fee limit. The kernel retries without exceeding that limit, so the payment can
proceed when the required fee falls within it.

An **operator-funds shortage** means available `sys` USDT0 cannot support the
fuel purchase while preserving the reserve. Further fees or a deposit to `sys`
can supply those funds; inspect the reported amounts before deciding how much
to add. This shortage does not mean that user balances lack USDT0 backing.

Blocked payments remain reserved and are retried in place. The halt clears
when no blocked payments remain. Deposits, execution, and reads continue while
outgoing rail work waits.

### Payments nobody has claimed

An incoming token payment whose sender is not registered cannot immediately be
credited to a user. The kernel holds it and includes it in the deposits view:

```
$ juice admin kernel deposits
Payments received whose sender nobody has registered:
  id: rail:0xccb0975d…:0
  kind: deposit
  amount: 40.00 USDT0
  status: held
  tx_hash: 0xccb0975d…
  party_handle: 0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc
Work delivered to foreign buyers and not yet paid for:
```

Held funds also appear as `unclaimed` in `admin kernel show`. If the sender
belongs to a user, registering that address can attribute the payment.
Payments sent directly from an exchange need operator attention because the
user generally cannot prove control of the exchange's sender address.

A held payment may instead settle a remote obligation. Reconciliation checks
those obligations before attributing user deposits, and can wait for a peer's
reveal when the sender has an unresolved ticket. See
[The network economy](network-economy.html#settling-one-call). ETH received
at the address supplies fuel and does not appear as a user deposit.

### Holdings and custody

```
Holdings:   240.00 USDT0 (gas 0.04994…) as of block 28
Operator:   earned=0.00 USDT0 paying-out=0.00 USDT0 unclaimed=40.00 USDT0 held-for-gas=0.00 USDT0
Solvency:   user-balances=240.00 USDT0 money-in=240.00 USDT0 difference=0.00 USDT0
Custody:    the money the rail holds matches the books
```

The **Holdings** line reports finalized rail balances. **Custody** compares
those holdings with the books at a point where the payment scan covers the
block being read and no payment is in flight. Active refill locks bound any
allowed difference. These checks report discrepancies for investigation;
they do not alter the ledger to make it agree.

## What you are risking

Remote service requires the provider to advance its execution budget while
waiting for payment. The kernel limits admission using one exposure figure
across all peers, so creating more peer identities cannot multiply the allowance.
The operator's risk allowance does not make unpaid obligations part of the
backing for user balances. See [The network economy](network-economy.html)
for the relationship between this limit, provider funding, and ticket settlement.
