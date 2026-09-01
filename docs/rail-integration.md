# Rail integration

How juice gets real money. `juice-rail` (v0.3) is consumed as-is: one chain account per
participant holding USDT0 plus gas; deposit, transfer, withdraw; no contract, no operator.
Supersedes `money-rail.md`.

## Networks

A Juice network is defined by a shared world file:

- **defining** (identical for every member): network name, chain id, token address.
- **operational** (each kernel's own): adaptor choice and config, RPC endpoint, gas policy.

A digest of the defining part is in every signature prefix, so any artifact from another network
fails verification everywhere (EIP-155 pattern). Discovery runs per network (`juice/<name>`) —
courtesy; the signature is the lock. Catalog, transactions, and rail live inside a network and
never cross; two networks on one chain can exchange only the raw asset, arriving as an ordinary
deposit.

The digest is a full federation protocol break, upgraded in lockstep: every signature domain,
discovery namespace, identity, settlement records, config, migrations, and the test catalog
change together. The revisions section enumerates them.

Shipped files: **play** (default, manual rail: same money rules, the operator's records are the
finalized facts, no chain), **test** (Arbitrum Sepolia, mock USDT0), **real** (Arbitrum One, USDT0).
Anyone can write a world file and gets an isolated economy — isolated, not private: anyone
holding the file can join.

## Kernels

A kernel = one `juice serve` + one home: ledger, identity key, world file, port, and — on a
chain rail — rail db and key. One kernel, one network, permanently; no switching, only
separate kernels.
First boot writes the digest into `juice.db`; startup refuses a mismatched world file (as D9
refuses missing keys).

## Rails are adaptors

`kernel` owns the interface, imports no chain code; `juice-rail` is the first adaptor. One set
of money rules runs everywhere; the defining part selects only the witness: no chain/token → the
**manual** adaptor, whose finalized facts are the operator's own records (`Pay` confirms at
once, `Refills` is empty); present → a chain adaptor. Never `if world == real`, and never
`if railed`.

The adaptor is a thin translation of the rail's actual outcomes — it adds none and hides none:

```go
type Rail interface {
    Address() string
    Balances(ctx) (token int64, gas *big.Int, err)     // display only, never booked
    Pay(ctx, id, to string, amount int64) (Outcome, error) // idempotent
    Status(ctx, id string) (unknown|pending|confirmed|failed, txhash, err)
    Deposits(ctx) ([]Deposit, error)                   // finalized incoming
    Refills(ctx) ([]Refill, error)                     // finalized gas buys: intent id, tx,
                                                       //   exact cost via the rail's read
    FinalizedBalances(ctx) (token, gas, block, err)    // the audit's cut
    Outcome(ctx, id) (Fact, error)                     // settled fact: tx, block, executed
    DepositsScannedTo() (block, error)                 // incoming coverage vs the cut
}
```

- `Outcome` is exactly one of: payment submitted (tx hash); **refill submitted instead**
  (refill intent id + tx hash — stored on the outbound row, or restart recovery cannot know
  what it is waiting for); **blocked(reason)**, nothing signed — blocked is not lost.
- `Status` keeps the rail's four states: `unknown` = no payment intent recorded, safe to Pay
  again from scratch; `pending` = the intent may still execute, never recreate it; `confirmed`
  and `failed` are final. Repeating Pay with the same id and terms is a no-op (the rail's own
  idempotence); changed terms are refused.
- The adaptor sequences with the rail's public verbs (`Prepare`/`Refill`/`Send`, which `Pay`
  merely composes), so the `sys` bound below is passed to `Refill` as its reserve argument and
  enforced by the rail before anything is signed. It re-implements no reserve rule — the bound
  is an input to the rail's one rule.
- Juice reproduces nothing the rail owns: no chain access, no amount handling, no deposit
  observation, no gas decisions. Finalized facts only; gas amounts are big integers.

## Money rules

- No mint: credits are created only by a crossing-in on `sys` — a finalized vault deposit, or
  the operator's record on the manual rail — or a peer receivable at admission; destroyed only
  by a crossing-out. Every other move is a transfer: conserving, on the owner's authority.
  Deposit = crossing-in plus a fused transfer to the attributed user; withdrawal = transfer to
  `sys` plus crossing-out at proof; attribution, assignment, and payout compensation are
  transfers from `sys`; unattributed money is `sys`-held balance, marked. The ledger operation
  refuses a crossing without its finalized fact. `sys` is otherwise an ordinary funded account.
- 1 credit = 1 USDT0 base unit. Humans see USDT; APIs keep integers; operators retune defaults.
- Backing, stated honestly: credits are backed by the vault **plus peer IOUs bounded by X**. In
  the worst case users jointly hold up to X more credits than the vault can pay. Deliberate,
  Sybil-proof, priced by `remote_bps`; the operator's accepted, bounded risk.
- Solvency identity, checked periodically and on `admin identity`. Terms defined so no account
  appears twice: **liabilities** = each account's balance where positive (available + locked) —
  pending payouts and unattributed deposits sit inside it, on `sys`; **receivables** = each
  account's shortfall where negative (peer accounts only, ≤ X at admission):

  ```text
  0 ≤ liabilities − (vault + receivables) ≤ active refill locks
  ```

  Equality holds at rest; the slack exists only between a refill's finality and its booking,
  bounded by the lock. `vault` is derived from the rail's finalized records — deposits, payment
  outcomes, refill costs; the operator's records on the manual rail — never the live balance
  read, which is display-only and would manufacture false alarms from work in flight. The
  in-transit split of `sys` remains persisted rows (pending payouts = open `rail_transfers`;
  unattributed deposits = the unassigned list), so a mismatch alarm always names its
  difference. What derived records alone cannot see — vault theft by the key outside the rail —
  is the custody audit: the derived vault compared against the rail's finalized-cut reads
  (balances at finalized block N, deposit cursor ≥ N, settled outcomes ≤ N), alarming with the
  difference; display-only, never booking.
- **Refill funding** — the cost is exclusively `sys`'s, never user backing; users never see it;
  prices and `fee_bps` carry it.
  - Gate, exact and fail-closed: when the rail answers "refill needed", juice calls `Refill`
    passing as its reserve everything that is not `sys`'s to spend — liabilities minus `sys`
    available, plus pending payouts and unattributed deposits — pure arithmetic on juice's own
    books. The rail's single reserve rule then refuses, before signing, any refill that would
    dip into user backing: no bound is guessed, no second reserve rule exists. On refusal the
    kernel persists a **stop signal**: outgoing rail operations stop, under their own typed
    error — never the caller's insufficient funds. Paid work, funding, deposits, reads, and
    reconciliation continue: local fees are what clear the stop, when `sys`
    can fund a refill again. Once signed, the recorded maximum is locked from `sys` until
    booking.
  - Booking: at finality juice asks the rail's refill-cost read for the **exact consumed
    amount**; `sys` is debited exactly that (`external_key` = refill tx) and the rest of the
    lock is released. The maximum is authorization, never cost — approximate books are
    rejected: two monetary records must not diverge. An unreachable RPC leaves the refill
    unbooked and the lock held — pending, never guessed.
- Credit: the global cap X is the **sole** hard bound on unsecured credit; `remote_bps` prices
  the risk. No balance checks: a foreign wallet balance proves nothing durable — emptied at
  will, shown to many creditors at once, satisfied by a trivial sum — and would add a chain
  read to call admission. Worst case, stated honestly: one malicious peer can consume the full
  X at each serving kernel and disable new unsecured paid calls there; local, free, and prepaid
  calls remain available. If that risk is ever unacceptable, the thing to reconsider is
  permissionless unsecured credit itself, not a wallet peek.
- Misconfigured rail (`CheckDomain`) refuses boot; unreachable RPC serves but blocks rail verbs.

## Addresses

Money is credited by finalized sender address, never by reporter (a tx hash is public — anyone
can point at it; only the sender address is a fact about who paid).

**The unavoidable friction**: sender attribution means exchange money cannot arrive directly.
It must hop: exchange → the user's own wallet → vault, and the user pays gas on the second hop.
The rule is juice's, not the rail's: the rail records every deposit's sender precisely so an
omnibus host can attribute shared-account money — to the rail, an exchange deposit is ordinary.
The hop is the price of attribution without per-user deposit addresses (which would each need a
gas reserve). `user deposit` states it beside the vault address and the caller's registration
state, and drives registration first when there is none — deposit is the only verb a depositor
must know.

- A user registers an address via `user address` (bare shows, an argument registers) or inline
  from an unregistered `user deposit`; either way registration is the same explicit act: an
  EIP-191 personal-message signature over `(kernel key, user id, address)` proves control of
  the address, not merely a claim. Common wallets sign only for a requesting page, so the CLI
  drives the signature through a local helper page — the same browser dance as login; a pasted
  signature is the fallback.
- Registration attributes retroactively: `sys`-held deposits from that address transfer to the
  registrant, because attribution is a pure function of the address→account map over finalized
  facts.
- One address, at most one account. Replacement requires current-password reauthentication —
  the recovery key gains no direct payout-address authority (it never authenticates, per D4); a
  user who lost the password completes the existing recovery flow first. Replacement freezes
  withdrawals for a fixed period (stolen-session redirect defense) either way.
- A chain-railed kernel identity **mandates** a proven rail address: an EIP-191 signature by
  the rail key over `(kernel key, address)`, domain-qualified. It authenticates the settlement
  destination; without it the identity is invalid on a chain rail. Juice custodies `rail.key`
  and hands it to the rail at construction, so producing this signature is plain cryptography,
  not chain access.
- Unrecognized senders sit on `sys`, listed, until the operator assigns an owner by transfer —
  a custodial support decision by the ledger authority, not cryptographic attribution. `sys`
  registers the operator's address like any user.

## Flows

Rail payments are two-phase and idempotent: record, pay, converge on the finalized fact. The
manual rail's `Pay` confirms at once, collapsing each machine to its final state.

- **Deposit**: observer records the crossing-in on `sys` once, `external_key =
  rail:<txhash>:<logindex>`; a registered sender's fused transfer delivers it, an unknown
  sender's stays on `sys`, listed.
- **Withdrawal** state machine — the destination is rail-resolved: the registered address on a
  chain rail (refused unregistered), the account itself on the manual rail (the operator pays
  outside the system; their record is the fact):
  1. Transfer to `sys` + pending `rail_transfers` row in one commit; funds are unavailable from
     this instant.
  2. `Pay`; the outcome is recorded on the row — `submitted` (tx hash), `refilling` (refill
     intent id + tx hash), or `blocked(reason)`.
  3. `confirmed` stores the payment tx hash; the finalized event makes the crossing-out final.
  4. A finalized failure is undone by a **compensating transfer from `sys`**, idempotent over
     the row id; the original entry is never edited.
  5. RPC errors and ambiguous outcomes stay `pending` — pending never guesses, and is re-driven
     at startup and by the retry worker; a retry re-presents the same intent, and a `refilling`
     row first asks the refill's status before re-presenting.
  Lifecycle visible throughout: id, status, tx hash, age.
- **Settlement** — the same pattern as every outgoing payment: reserve → sign → finalize or
  compensate; on the manual rail the operator's confirmation is the finalized fact. The
  debtor's signed exact-settlement record snapshots `(settlement_id, amount,
  destination address)` on the settle stream — as the probabilistic open already does. Before
  paying, one commit reserves the internal sources: the peer row is debited with the pending
  `rail_transfers` row, and probabilistic cash additionally locks `Q−d` from `sys` — so
  external money never leaves before the books can cover it (a creditor spending down its
  balance, or `sys` dipping, mid-flight can no longer strand the booking). Then pay, intent id
  from `settlement_id`. After finality the debtor announces the binding `settlement_id →
  (txhash, log index)`; the creditor verifies that finalized event against the snapshot
  (sender, destination, amount) and books per P10; a finalized failure compensates the
  reserving commit, never edits it. Matching by named chain fact, never by `(sender, amount)`
  — two equal-amount settlements cannot collide. The rail remains unaware of settlements.

Outbound rows: `rail_transfers` (`id, kind ∈ {payout, settlement}, party, amount, status,
tx_hash, refill_id, refill_tx, created_at, finalized_at`).

## Trust

The operator holds the rail key: users' backing money is in operator custody. "No operator" is
a property of `juice-rail`, not of a chain-railed juice — juice's operator was always the ledger
authority; the rail extends that same trusted role to custody of the backing. Users additionally
carry the operator's credit decisions: up to X of backing is peer IOUs.

## Refill cost: a read, not a record

Refill cost is part of the rail's v0.3 library surface: answered on demand from the finalized
receipt, stored nowhere, decode tested on both router encodings. A read was chosen over a
stored field deliberately — no durable state, schema, or storage-interface change, and a wrong
decode is a fixable bug in a read, never a wrong number frozen write-once in every host's
database. Receipts persist on nodes indefinitely, so the answer is always recoverable. The
rail owns the decode because every accounting host needs it: logic a host has to write is
logic missing from the library.

## Client

Pure client, the only thing spanning networks: named profiles (endpoint, token, label).
`juice use <name>` switches **and verifies** — dials the kernel, checks the token, prints the
server's name, key, world; fails loudly if unreachable. `juice use` lists; `auth login` stores
what it creates. A `JUICE_PROFILE` env var overrides the sticky selection per invocation, for
scripts. Withdraw and settle confirm before moving money, naming the world; paid runs are not
pre-confirmed (agents are first-class). No per-command `--profile`.

## Operator surface

`admin identity` gains rail address, balances, the split of `sys` into earnings and in-transit
(unattributed deposits, pending payouts), the solvency identity with its named terms, and the
stop signal with its reason and age. `admin deposit` bare lists the unattributed money on
`sys`; `--tx <hash>` attributes one deposit by transfer; on the manual rail `admin deposit
<user> <amount> --ref <fact>` is the crossing-in fused with its transfer. There is no
`admin withdraw`: money out is always the owner's `user withdraw`, `sys`'s profit included.
Alarms: identity gap beyond active refill locks, custody-audit mismatch, stop signal set.
Refill shows as a network cost; shortage shows as pending payout/settlement with age.

## Factorization

`kernel` defines `Rail`; new package `rail/` implements it: world-file construction, the
adaptors (the manual one trivially; for the chain: verb sequencing, `sys` gate and refill
booking, deposit attribution), and the storage implementation supplied to the rail. Amount
handling and deposit observation stay in
`juice-rail` — logic juice would have to rewrite is logic the rail already owns. `juice-rail`
owns everything on-chain, state in `$JUICE_HOME/kernel/rail.db`, key in `rail.key`. Outbound
intent id = `SHA-256("juice-rail-op|" ‖ record id)`, so retries carry the rail's at-most-once
end-to-end. Kernel core untouched; existing idempotent ledger ops reused; signature prefix and
identity fields change lockstep.

## Acceptance

1. **Play**: new user boots on the manual rail, is funded by an operator-recorded deposit, runs
   a paid remote action — no crypto concept anywhere.
2. **Isolation**: cross-network discovery yields nothing; calls, gossip, settlement fail
   signature verification; ratings/evidence never cross; test funds never buy real actions; a
   database refuses a foreign world file.
3. **Rail state machine** (test): finalized deposits only; wrong chain/token/destination/sender
   rejected; one chain event moves the ledger once; crash recovery at every withdrawal state,
   including `refilling` (the stored refill id resolves before the payment is re-presented);
   `unknown` after a blocked attempt is retried from scratch, `pending` is never recreated;
   refill books the exact consumed amount to `sys` once and releases the lock — the maximum is
   never booked, and an unreachable RPC at booking leaves it locked and unbooked; a refill that
   would dip into user backing is refused by the rail pre-sign, the stop persists across
   restart, refuses outgoing rail operations under its own typed error while paid work and
   deposits continue, and clears after funding; a settlement reserves its internal sources in one commit before paying and its
   finalized failure compensates that commit; a finalized withdrawal failure produces one
   compensating transfer, never an edit; registration attributes prior `sys`-held deposits; one address one account; replacement
   guarded and freezing; an identity without a proven rail address is invalid; unannounced peer
   transfers settle nothing; two equal-amount settlements resolve by their bound chain facts; no
   re-pay under a new identity; the chain-vs-ledger reconciliation detects a deliberately broken
   invariant **and names the differing term**.
4. **Economic loop** (three kernels): A funds a buyer → buyer runs B's action → A and B settle on
   the rail → B spends earnings on C — through restarts, disconnection, exact and probabilistic
   settlement; ledgers, signed records, chain facts converge.

## Contract revisions this requires (yours)

- Every P1 signature domain gains the world digest (lockstep protocol break); discovery
  namespace becomes per-network; migrations and the §8 test catalog change with them.
- Identity and gossip gain network name, digest, and the mandatory proven rail address; accounts
  gain the registered address.
- U3 rewritten over the two primitives (crossing on `sys` against a finalized fact; transfer on
  the owner's authority; idempotency preserved); U33 narrowed to *the* rail; the stop signal
  joins the operator surface (U44).
- D20 gains `use`, profiles, `user address`, `user deposit`, `user withdraw`, and the rail
  operator surface; `admin withdraw` and `--cash` are retired.

## Non-goals

Per-call on-chain settlement; multi-domain kernels; bridging inside the kernel; balance-based
credit checks (rejected — see Money rules); modifying `juice-rail`.
