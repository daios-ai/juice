---
title: The network economy
parent: Operating a kernel
nav_order: 4
---

# The network economy

Federation lets a buyer use a local balance to purchase a service hosted
elsewhere. The serving provider advances the work, the kernels record the
result, and an external payment settles the obligation. This chapter explains
the prices, funding, and evidence involved from the operator's perspective.
The buyer's procedure is covered in
[Calling an action on another kernel](../calling/running.html#calling-an-action-on-another-kernel).

## What a cross-kernel call costs and who gets it

The advertised price combines the provider's local price with a serving markup
and the originating kernel's import fee:

```
provider's price                  mp
serving kernel's markup           sr = mp + ceil(mp × remote_bps / 10000)
buyer's kernel's import fee        q = sr + ceil(sr × import_bps / 10000)
```

The buyer reserves `q` as the execution budget. The serving kernel applies its
ordinary fee to the provider's local execution margin. The serving markup
compensates the provider for advancing the work and accepting variation in
ticket settlement, while the origin retains the import fee. A paying ticket
is credited whole to the serving provider.

Both markup rates default to 500 basis points, or 5%. The serving rate is
bound to the call's terms, and the origin records its own import rate at
dispatch. Later configuration changes therefore do not reprice an existing
call. The import rate is local accounting information and is not part of the
remote receipt.

With `mp = 2.00` and both rates at 5%, `sr = 2.10` and `q = 2.205`. On
success, the seller's receipt contains `charge 2000000` and `premium 100000`
in base units. The buyer's allocation is `2.205`, including an import fee of
`0.105`. The final account charge also depends on the ticket mechanism below.

## The seller advances the work

The serving provider supplies the execution budget from its own account.
Payment from the remote buyer arrives through settlement, rather than being
available before execution. A provider therefore needs a working balance to
serve paid remote calls.

The kernel tracks **exposure** as delivered value less cash received for that
work. Admission temporarily adds the maximum obligation, including markup;
settlement of execution corrects that reservation to the actual charge.
The kernel returns a signed rejection if admitting the call would exceed its
limit or if the provider cannot fund the execution.

```
Credit:     owed-to-us=4.20 fUSD limit=50.00 fUSD
```

### Bounding what strangers can cost you

The `credit_limit` applies across all peers together. Its default is fifty
display units. Because admission uses a shared limit, an attacker cannot obtain
another allowance simply by creating a new kernel identity.

Exposure is reduced by actual receipts of money. A losing ticket closes its
obligation without reducing exposure, while a paying ticket can reduce exposure
below zero. The limit therefore bounds current exposure rather than requiring
each ticket to pay. With a zero limit, free work still proceeds and paid work
can be admitted when negative exposure leaves enough room.

## Settling one call

Each obligation has a ticket identified by the call's original retry key.
It settles independently, without accumulating a bilateral balance that must
later be netted against other calls.

For a small obligation, a draw avoids making a blockchain payment whose fee
would exceed the service's value. The buyer commits to a secret at dispatch.
After execution, the seller generates a nonce and signs it into the receipt.
These two contributions determine the draw, preventing either side from
choosing the result alone.

Let `D` be the obligation and `L` the face value configured as `lottery` on the
buyer's kernel:

- If `D ≥ L`, or `L` is zero, the debt is paid exactly.
- Otherwise `L` is paid with probability `D / L`, and nothing is paid otherwise.

Both cases have expected payment `D`. Small obligations therefore produce fewer
external payments, with individual payments larger than the services they
settle. Actual totals over a finite set of calls can differ from their expected
value.

### Who funds a ticket

When a paid call is dispatched with a nonzero `L`, the origin reserves the full
stake from the immediate caller's account. On receipt settlement it returns
the obligation amount from the execution budget to that account, releases the
stake, and reserves any payment due from the draw in the same operation. The
serving kernel refuses a face value above its own `lottery_max` before execution.

For a root call, the immediate caller is the buying user. For a composed call,
it is the composing action's owner. A small obligation's winning payment is
covered by this caller's reserved stake; an obligation at or above the face
value, or with lottery disabled, is paid from the funds returned from the
execution budget. Admission requires the execution budget and any required
stake before dispatch.

The lottery therefore requires no separate USDT reserve or minimum balance in
`sys`. The caller funds the payment, and the serving provider advances execution
from its own balance. If `sys` is itself the caller or provider, it meets the
same funding requirements in that role. An outgoing payment is temporarily
held on `sys` while in transit, but this reservation is not operator earnings
and cannot be spent on fuel.

On a chain network, sending the payment also consumes ETH. Available `sys`
USDT funds automatic ETH purchases, so a shortage of operator funds can prevent
a refill even though the ticket's USDT payment is fully reserved. Initial
funding and the conditions for refilling are described in
[Funding the kernel](running-a-kernel.html#funding-the-kernel).

### Revealing the result

The buyer reveals its secret immediately for a loss, or after the payment is
final for a win. The seller checks the reveal against the recorded commitment
and recomputes the outcome. A late losing reveal remains valid; a delay does
not change the draw into a payment.

### What counts as payment

On a chain network, a paying obligation closes against the finalized transaction
named in the reveal. Its sender must match the buyer's address proved at
admission, and its amount must match the draw. One payment can close only one
obligation. A losing draw closes after its reveal is verified, with no payment.

Settlement payments and user deposits arrive at the same rail address.
Reconciliation matches obligations first, then attributes remaining deposits
by registered sender. A payment from the payer of an unresolved obligation can
remain held until the reveal establishes its purpose; this prevents premature
crediting as a user deposit.

On `play`, the buyer's signed paying reveal supplies the manual rail's payment
fact. The same reconciliation can then close the obligation automatically.
No operator payment is needed, consistent with `play` having no cash backing.

### The settings

```
Tickets:    this kernel draws for 1.00 fUSD, and accepts tickets up to 5.00 fUSD
```

The `lottery` setting determines the face value your local callers stake.
Setting it to zero pays each obligation exactly, requiring an external payment
for every nonzero obligation. The `lottery_max` setting limits the face value
your kernel accepts when serving a buyer from elsewhere.

These are operator settings rather than network-wide constants. A larger
ticket can reduce payment frequency for small calls, but also requires a larger
available balance and produces greater variation in individual charges.

## Federation does not chain

A kernel's remote-action cache is available to its local users and cannot be
exported to a third kernel. Resolution therefore reaches the provider's home
kernel directly, and its signed receipt supplies the evidence for that remote
call. Providers can still compose other services within their own actions,
subject to ordinary budgets and attribution.

## Reputation across kernels

Kernels share signed projections of trade records so that another participant
can examine the evidence behind a rating. Each projection names the subject
kernel and action, the outcome and times, and optionally the counterparty
kernel. It excludes user identities, transaction and execution IDs, payload
hashes, amounts, and value recipients.

A kernel publishes evidence from its own records. It does not relay evidence
learned from other kernels, so consumers obtain each statement from its issuer.

A rating is **trade-backed** when its linked receipt matches an execution
receipt issued by the subject kernel for that action, with the rating issuer
named as counterparty and both sides reporting the same outcome. A linked pair
that disagrees counts as a contradiction; unlinked claims remain unverified.
An issuer that gives conflicting signed accounts of a trade or its rating is
marked as telling it two ways, and that trade contributes nothing to the derived
figures. Inspect these views with:

```
$ juice admin peer inspect beta-kernel
```

The execution summary counts the subject kernel's own evidence. Counterparty
experience separately groups the reports of other issuers, avoiding a combined
score that would obscure their sources.

Buyers see the same evidence on `action show` and in search results, alongside
this kernel's own experience. Each report shows the period its records cover.

Signature and receipt checks establish who issued a record, whether it has
changed, and whether a claimed trade links to the counterparty's evidence.
They do not prove that an assessment is honest. Interpret ratings alongside
your own experience and the independently recorded history of trade.

## Retention

The kernel retains peers with recent activity, suspended status, or unresolved
work in either direction. After an eligible peer has been idle beyond
`peer_retention_days`, its cached actions, statistics, discovery entries, and
evidence can be removed with its peer record.

The associated account remains as a reference for historical transactions and
ledger entries. Removing cached peer information therefore does not remove
the immutable financial history.
