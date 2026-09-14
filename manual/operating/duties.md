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

On `test` and `real` the kernel operates a rail account. Three things need your
attention.

**Fuel.** Making a payment costs gas, which comes out of your earnings. The kernel
locks what it may spend before asking the rail, and settles against the actual
cost. User balances are never spent on fuel.

**Halts.** If a payment is blocked, outgoing rail work stops and reports
`ErrRailStopped`. Deposits, execution and every read continue. The fees you earn in
the meantime are what clears it. `admin kernel show` names the halt and its cause.

**Custody.** The kernel compares what it thinks it holds against what the chain
says, at a settled point where nothing is in flight. A difference is reported with
the term that differs. Both solvency and custody are displays; neither moves money.

## What you are risking

Your own earnings, up to the credit limit you set. Serving other kernels is done on
credit, and that credit is bounded across all peers at once, so no number of new
identities increases it. A user's balance is never at risk from work you do for
strangers.
