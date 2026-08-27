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


## Applications

Problem. Juice's only first-class unit is the action. Groups of actions that belong together — an application such as acme/mail with search, read, send; an imported HTTP server; an MCP-style toolset — exist nowhere: nothing names them, describes them, carries their track record, or installs and uninstalls them as a unit. The question was how juice should represent applications, for humans and agents alike, without a client-only workaround.

Justification. Every candidate that added a new kind of object was rejected on the same grounds. An application object, a descriptor row, or a first-class directory would duplicate lifecycle and authorization rules, add wire surface for federation, and — decisive for reputation — mint feedback for something that never transacts. In juice, history and ratings are honest only when they attach to things that trade; a grouping that does not trade can carry at most derived views, computed over historical membership (the index-fund lesson: aggregate the record of what was actually in the group at trade time, or providers launder failures by renaming). HTTP demonstrates the alternative: it has no directory object at all — a path prefix becomes real when something answers there, and applications on the web are nothing but names plus a page at the root. Juice already has the required ingredients: names may contain /, (owner, name) uniqueness lets an action sit at any prefix, and a zero-price call still produces transactions, receipts, and ratings.

Resolution. A group is declared by an ordinary action at its root — the index. No new kernel object, no schema, no containment graph; the convention is positional. Specifics:

1. The index of group P is always the action P/index, at every directory depth. Resolving P checks for an exact action P first and falls back to P/index on a miss, one line in the kernel's single name resolver. Aliases have priority, not strict identity.
2. The index is an ordinary action in every rule: its description names and explains the group; members are derived from the namespace by prefix, never asserted; if it composes members its price bounds the subtree; its transactions build genuine history, ratings, and federated evidence. agent@kernel/index therefore makes an owner — notably an agent persona — a ratable economic subject, honestly so when the index is the working entry point rather than a storefront.
3. Lookup stays flat, web-search style: every action indexed independently, relevance deciding whether the index or a member ranks; no collapsing, promotion, aggregation, or ancestor fields in the kernel.
4. Install and uninstall: OpenAPI unimport already removes by provenance, so imported sets uninstall as a unit today. Import gains a prefix (--as mail) so the set is a namespace group; an operation whose mapped name is index becomes the application's entry point — the spec author supplies the root's behavior by naming, and a --index flag was rejected as a second authority over the existing name mapping. Without such an operation the import is a toolbox until the owner writes a root. A spec-supplied index uninstalls with its set; a hand-written one survives, with a warning only.
5. Rejected en route, not to be revived: application/package/descriptor/directory objects, standardized index output, hypermedia link envelopes, generic schema-to-form rendering, kernel-side reputation aggregation.

Net kernel change: one resolver fallback and the import prefix. Everything else is convention over existing machinery.
