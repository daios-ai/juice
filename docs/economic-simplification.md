# Economic simplification

Status: design proposal. This document does not amend `requirements.md`, which remains the product
contract. No implementation should change until this proposal is resolved and folded into that
contract.

## Motivation

The present inter-kernel economy combines two optimizations:

1. Small obligations accumulate into larger payments to reduce transaction costs.
2. Providers extend credit so callers can use an action without waiting for rail finality.

That design introduces negative peer balances, aggregate exposure limits, settlement thresholds,
manual settlement, probabilistic settlement of residual debt, and extensive recovery and
reconciliation machinery. This may be premature optimization. Transaction costs should decline as
payment rails improve, while the protocol complexity remains a permanent cost.

The proposed model separates the two original concerns. A configurable minimum payment handles
transaction cost. A provider premium prices the short-lived risk of acting before an already
submitted payment becomes final. Neither concern requires accumulated bilateral credit.

## Proposed economic rule

Let:

- `d` be the all-in expected inter-kernel charge for one call, including the provider's finality
  premium and every fee disclosed to the caller;
- `Q` be the rail's minimum economical payment.

For each inter-kernel call:

- If `d >= Q`, the advertised charge is `d` with probability 1.
- If `0 < d < Q`, the advertised charge is `Q` with probability `d / Q`, and zero otherwise.
- If `d = 0`, the call is free.

Thus a sub-minimum action does not advertise `d` as the amount the caller authorizes. Its signed
terms advertise:

- maximum charge: `Q`;
- probability of that charge: `d / Q`;
- expected charge: `d`.

The caller must explicitly accept the maximum charge and have that amount available. For example,
when `Q = 100` and `d = 5`, a call costs either 100 with probability 5%, or zero with probability
95%. Its expected charge is 5, but its authorized maximum is 100.

## Call and payment sequence

Each call is economically independent:

1. The caller receives signed terms containing the maximum charge, probability, rail, and pricing
   inputs, and explicitly accepts them.
2. The caller's kernel reserves the maximum charge.
3. The payment outcome is determined once and bound to the call's idempotency key. Neither party
   may choose the outcome, and a retry must never produce a new draw.
4. A zero outcome releases the reserve without a rail transfer.
5. A non-zero outcome causes the payer kernel to submit the rail payment immediately. The payment
   is bound to this call and cannot be reused for another purchase.
6. The provider verifies the submitted payment and may execute before rail finality. The advertised
   premium compensates it for finality delay, reorganisation risk, and temporary capital exposure.
7. The same payment is followed or resubmitted until resolved. A retry creates neither a second
   payment nor a second draw.

"Immediate settlement" therefore means that the outcome and payment instruction are fixed as part
of the call, rather than accumulated as debt for later operator action. Blockchain finality may
arrive later. Until then, the system records an in-flight payment, not a reusable credit balance.

## Uniform treatment

Ordinary users and peer kernels follow the same solvency principle: a purchase cannot begin unless
the payer can cover the explicitly accepted maximum. A peer account cannot become negative, and a
provider does not grant it a standing credit line.

The superuser remains an administrative authority for configuration and recovery, but it is not a
special economic participant. It does not decide when ordinary inter-kernel obligations settle.

## Composition

The rule applies independently at every composition boundary:

- The original caller accepts only the outer action's signed maximum charge.
- A composer buying a subcall becomes the payer for that subcall and separately accepts and
  reserves its maximum charge.
- A subcall's probabilistic outcome cannot increase the original caller's charge beyond the outer
  contract.
- The composer bears and prices the variance of its own subcalls.

This preserves one bounded advertised contract at each layer while applying the same rule to every
payer.

## Machinery this model replaces

The following concepts should disappear rather than coexist with the new model:

- negative peer balances and accumulated bilateral debt;
- unsecured-credit exposure limits;
- settlement triggers;
- operator-initiated ordinary settlement;
- later netting of many calls;
- probabilistic settlement applied to residual accumulated debt;
- reconciliation of opposing peer debt balances.

The system still needs local balances, reservations, signed quotes and receipts, idempotency,
in-flight rail-payment records, finality observation, deposits, withdrawals, and recovery of an
interrupted payment.

`Q` becomes a rail policy for individual calls, not a threshold used later to settle accumulated
debt. As transaction costs decline, an operator can lower `Q`. If the rail makes every payment
economical, `Q` can effectively become zero and all calls can settle for their exact charge.

## Decisions still required

Before this becomes the product contract, the following must be decided explicitly:

1. Whether payment buys an accepted execution attempt or only a successful result. Conditional
   refunds would add another rail transfer and substantially more protocol machinery.
2. The exact fair-draw construction. It must prevent either party from selecting, withholding, or
   grinding outcomes and must survive retries and crashes.
3. What happens when a submitted payment is dropped, replaced, or reorganised after the provider
   begins work, and how many unresolved payments one counterparty may have at once.
4. How the premium is calculated and disclosed. It should price finality risk without becoming an
   implicit, undisclosed fee.
5. Which rail observation is sufficient for a provider to begin execution before finality.

## Consequence for current work

This is a replacement economic model, not a configuration change. The economic sections of the
network simulation currently exercise accumulated credit and later settlement. They should not be
treated as final acceptance tests for this proposal.

The safe order of work is:

1. Resolve the open decisions above.
2. Amend `requirements.md` and its wire protocol.
3. Identify and delete the superseded credit and settlement machinery.
4. Implement the smallest per-call pricing and payment path that satisfies the amended contract.
5. Rewrite the economic simulation around the new invariants while retaining transport, security,
   authorization, composition, finality, and recovery coverage that remains applicable.
