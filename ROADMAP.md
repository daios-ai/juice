# Roadmap

## Money rail integration

### The frame

A kernel is sovereign at every layer: identity is a key, not a registrar;
communication is libp2p, not URLs; the market is actions, prices, and ratings
as public evidence, not a platform. Money is the last layer still borrowed —
credits exist by administrative fiat, and cross-kernel debts are bilateral
IOUs bounded by the exposure cap.

[juice-rail](../juice-rail) completes the set under the same ethos: one key,
one vault, no operator, nobody who can freeze anyone. Juice is a rail host —
authoritative for its own ledger, with the rail moving external money and
reporting finalized facts.

The consequence: a kernel becomes a full economic agent. It earns real money
selling actions, holds it without a bank, and spends it — on other kernels'
services and on its own substrate. A service can pay for its inference and
its existence out of what it sells, with no human in the payment loop.

### The plumbing

How the kernel's internal ledger docks onto its rail account:

1. **Backed deposits/withdrawals.** A user deposit is a USDT0 transfer to the
   kernel's rail address, credited after finality; withdrawal is the reverse.
   Both exactly-once by construction.
2. **Settlement on the rail.** Cross-kernel value is settlement, not an
   action call: kernel A pays kernel B between rail accounts; receiving
   requires nothing of B; finality replaces trust. Federation keeps what it
   is good at — metering who owes whom via signed receipts. The exposure cap
   becomes a bound on unsettled float, not on unbounded trust.
3. **Retire the internal cross-kernel transfer machinery.** The value channel
   that `sys/transfer` going local broke stays dead; it is not rebuilt inside
   the action-call path. Stale `prepareTransferEffect` guidance goes with it.


