# Money Rail for Juice

A trust-minimized boundary between Juice's internal credit ledger and on-chain USDC. It defines how
real value enters Juice (deposit → mint), how it leaves (burn → release), and how the design
prevents any single operator component from over-issuing credits or releasing unbacked funds.

The governing principle, corrected from an earlier draft, is that the guardian must be **preventive,
not reactive**: issuance and release are gated on independent approvals *before* they happen, not
detected and bounded *after*. A reactive design meets neither "no operator may over-issue" nor
"every credit is backed," because Juice makes settlement final (§6) and offers no post-hoc reversal.

## 1. Goals

Juice settles work in **credits** — indivisible integers moved between user wallets on every call
(§6). Credits are meaningful only if redeemable, so the system needs a rail to a real asset that:

1. **Custodies trustlessly against operators.** No single operator component may abscond with, or
   over-issue against, user funds.
2. **Backs every credit.** Total credits outstanding never exceed USDC held in custody.
3. **Is exactly-once.** No reorg, retry, or replay may double-mint a deposit or double-pay a
   withdrawal.
4. **Leaves the kernel almost untouched.** Juice is deliberately payment-rail-agnostic (§3, §12);
   the rail rides that seam and adds only the schema the safety argument requires.

**Non-goals.** Per-call on-chain settlement is infeasible (finality is minutes, gas dwarfs a
sub-cent action price) and out of scope: the chain settles *funding*, not calls. The rail is
trustless with respect to Juice's *operational* components (Rail, Monitor day-to-day), **not** with
respect to the USDC issuer (issuer-freezable, upgradeable), the chain, or Bank *governance*, which
is part of the trust base (§10).

## 2. Trust model

Five roles, with authority split so that no single one can create or move value:

- **Bank** — an on-chain USDC contract. Holds custody. Releases only against three independent
  conditions (§6). Governed by immutable-or-timelocked governance (§10).
- **Rail** — an off-chain daemon. A chain observer and relay. It proposes and submits; it holds no
  signing authority that can, alone, mint or release. Boxed to liveness.
- **Monitor** — an independent chain observer and solvency guardian on separate keys and
  infrastructure. Co-attests deposits (gate on mint) and approves releases (gate on withdrawal),
  each after checks it performs from its *own* view.
- **Juice** — the kernel. Burns credits atomically against a user-signed intent and signs a **burn
  attestation** the Bank can verify. Enforces conflict-checked idempotency and emits the signed
  money-event log.
- **User** — holds a registered withdrawal key (§8) and signs intents, authorizing both the burn
  and the destination.

**Resulting properties** (the precise guarantee, replacing the earlier draft's imprecise claim):

| Attack | Requires |
|--------|----------|
| Over-issue (mint unbacked credits) | Rail ∧ Monitor collusion — mint needs two independent deposit attestations (§5) |
| Release unbacked reserves | Compromise of Juice's burn-attestation key ∧ Monitor collusion (§6) |
| Redirect an honest user's withdrawal | The user's withdrawal key — impossible without it |
| Freeze (delay/deny) | Any single component alone — the residual power the seam permits |
| Drain via Bank upgrade/pause | Bank governance — hence it must be immutable-or-timelocked (§10) |

The Rail is genuinely liveness-only: every value-creating or value-moving step needs a signature it
does not hold.

## 3. Peg

`1 credit = 1 USDC base unit`, both 6-decimal integers. The mapping is the identity — no conversion,
rounding, or dust — which respects that credits are indivisible integers and keeps the solvency
arithmetic exact.

## 4. Governing rule

> Credits are minted only on a **finalized** Bank deposit event **independently attested by both the
> Rail and the Monitor**; USDC is released only against a **Juice burn attestation** proving the
> corresponding credits were destroyed.

Finality (not an N-confirmation guess, not a provider callback) closes reorgs and forged/on-ramp
lies. Dual independent attestation closes single-component forgery. Burn-before-release closes
unbacked payout. The cost is latency: a deposit is unspendable until finalized and co-attested.

## 5. Deposit — finalize, co-attest, mint

```
finalized Bank event
  → Rail (observer A) and Monitor (observer B) each independently verify, from their own chain view:
      finality · chain_id · bank_contract · actual amount · deposit-address → Juice user attribution
  → each signs the canonical deposit attestation over the event identity + user + amount
  → Juice credits atomically iff BOTH attestations are present and agree:
      conflict-check event identity · credit user · create Adjustment · append signed ledger event
```

- **Event identity** (the idempotency key): `(source, chain_id, bank_contract, tx_hash, log_index)`.
  A replay with identical fields returns the existing deposit; a replay with **conflicting** fields
  is **rejected**, so one event can never be claimed for two amounts or two users.
- **Attribution by address.** Each user has a contract-controlled deposit address (a CREATE2 vault
  or a Bank `depositFor(user)`) provably swept into the Bank; the credited user is derived from the
  address, never asserted by an operator. Funds are never held at operator-controlled addresses.
- **Why two observers.** With a single attester, that one component can mint phantom credits. Two
  independent observers make over-issuance a collusion, satisfying "no single operator over-issues."

## 6. Withdrawal — intent, burn, attest, release

```
1. User signs the complete withdrawal intent (§8).
2. Rail relays it to Juice.
3. Juice verifies the account→key binding and atomically:
     available ≥ amount · burn credits · record debit Adjustment · sign a burn attestation ·
     append signed ledger event
4. Monitor verifies the intent, the burn attestation, deposit history, and solvency; approves.
5. Bank verifies, and releases exactly once, iff ALL hold:
     user signature (destination + amount) ·
     Juice burn attestation (credits were destroyed for this withdrawal_id) ·
     Monitor approval (solvency) ·
     unused withdrawal_id + nonce · unexpired · matching chain_id + bank_contract
```

- **Burn first, on user intent.** Juice re-checks `available ≥ amount` atomically and debits before
  any USDC moves — no TOCTOU, and funds locked in live processes/steps cannot be drained. The burn
  requires the *user's* signature, not an operator instruction, so a compromised operator cannot
  destroy a user's credits into the void (griefing).
- **Burn proof at the Bank.** The Bank's third condition — a Juice burn attestation — is what the
  earlier draft lacked. Without it, a compromised Monitor plus any attacker-controlled user could
  release against pooled custody with no burn behind it. Even an *honest* Monitor that checked only
  aggregate solvency would co-sign a burn-less release, because a single such release passes the
  pre-release solvency check and only shows as `Bank < Σ` afterward. The attestation is what lets
  the Bank and Monitor distinguish a real withdrawal from a fabricated one.
- **Crash safety.** A crash after burn, before release, leaves the system over-collateralized (the
  safe direction); the release resumes from the persisted withdrawal state, keyed on `withdrawal_id`.

## 7. Monitor duties: attribution, solvency, independence

The Monitor performs three duties; aggregate solvency alone is insufficient.

1. **Per-deposit attribution.** For every mint, the credited user must equal the user encoded by the
   on-chain deposit address. Aggregate solvency is blind to *misattribution* — a compromised Rail
   crediting itself for user A's real deposit keeps the books balanced. Attribution verification is
   what makes per-user addressing load-bearing.
2. **Solvency invariant:**

   ```
   Bank USDC ≥ Σ(user.available + user.locked)   over ALL users
             — local, proxy, suspended, denied, @sys
   ```

   Process, trace, and step balances are **not** added: those parked funds already sit inside a
   user's `locked` (a process is funded *from* its owner's locked, §6), so counting them
   double-counts. Legitimate slack (Bank > Σ) comes from pending withdrawals, finalized-but-unminted
   deposits, set-aside ambiguous deposits, and operator buffer — every transient state
   over-collateralizes.
3. **Independent derivation.** The Monitor's value comes from *not* trusting Juice's or the Rail's
   numbers: it reads the chain itself (Bank balance, deposit events, attribution) and cross-checks
   against Juice's signed money-event log. A read-replica of Juice's DB is insufficient — it
   faithfully mirrors well-formed fraudulent mints. Because ordinary execution conserves credits, the
   Monitor derives total issuance from the credit/debit sequence in the log, and a periodic **signed
   `Σ(available+locked)` checkpoint** additionally catches ledger bugs or out-of-band DB tampering.

## 8. Withdrawal-key binding and signed intent

Juice users authenticate by password/bearer; `User.public_key` is an **Ed25519 federation** identity,
not an EVM key. A withdrawal therefore requires an explicit, separately registered on-chain key.

- **Binding.** A `(juice_user_id → withdrawal_key)` registry. Registration and replacement are
  themselves authenticated, subject to a timelock, and have a recovery rule — a stolen session must
  not instantly rebind the payout key.
- **Signed intent payload** binds, at minimum:

  ```
  kernel_identity · juice_user_id · withdrawal_id · amount · destination ·
  chain_id · bank_contract · nonce · expiry · protocol_version
  ```

  Keyed on `juice_user_id`, **never the handle** — Juice permits superuser handle rename, so a
  handle-bound intent is forgeable by rename.
- **Signature scheme.** For an EVM Bank, EIP-712 typed data, with EIP-1271 support for contract
  wallets.

## 9. Signature compatibility

Juice's platform identity is Ed25519; an EVM Bank verifies secp256k1/ECDSA. So both the user's
withdrawal key and Juice's **burn-attestation key** must be EVM-verifiable — a separate, narrowly
scoped secp256k1 money-attestation key on Juice's side (or an EIP-1271 contract signer), distinct
from the Ed25519 federation key. This is a real architectural addition, not an implementation detail.

## 10. Bank governance

"Trustless custody" is vacuous if an operator holds an upgrade or pause key — you upgrade the Bank
and drain it. Governance is therefore explicitly in the trust base. The Bank must be **immutable**,
or governed by a **delayed, independently-controlled** mechanism (timelock + a controlling set
disjoint from Rail/Monitor operators) with a guaranteed **user exit window** before any change takes
effect. Release caps are enforced **on-chain**, not instantly mutable by the Rail. Monitor
replacement, cancellation authority, and attestation-key rotation all run through this same
governed, delayed path.

## 11. Bank state machine and recovery

State machine, terminal and on-chain:

```
pending → released
pending → cancelled
```

A failed release compensates with a re-deposit **only after `cancelled` is final**, and the
compensation is itself a uniquely keyed, independently attested credit — otherwise the original
release may still fire and double-pay.

**Recovery.** Prevention (§4–§6) makes over-issuance a two-party collusion rather than a routine
risk, which matters because *post-hoc recovery is fundamentally limited*: by detection time, phantom
credits may already be spent, paid to providers, converted to `@sys` fees, locked in processes, or
moved through remote settlement, and Juice forbids negative balances and reversal (§6). "Burn the
unmatched mint" is therefore **not** a general recovery. Residual discrepancy (bugs, chain edge
cases) is absorbed by a **prefunded operator reserve** up to a chosen cap; beyond it the system
halts and falls back to an explicit, pre-declared loss-allocation policy (reserve top-up or
socialized haircut — clawback and tainted credits are rejected as incompatible with final
settlement). The fix of first resort is prevention; recovery is the bounded backstop.

**Guardian liveness.** The Monitor is a withdrawal-liveness dependency. The fallback for an
unavailable Monitor must be safe: it may only **complete an already-burned, Juice-attested**
withdrawal (the credits are gone, so releasing the matching USDC cannot break solvency), or replace
the Monitor via the timelocked governance of §10. A general "Monitor down ⇒ user may release" rule
is unsafe — without proof of burn, a user could withdraw USDC while retaining spendable credits
(double redemption).

## 12. Federation

Proxy balances are **operator settlement accounts** (§13), not end-user wallets: a proxy (key-only)
user has no local auth and cannot perform the user-signature self-service flow. Proxy deposit and
withdrawal are therefore **operator-only supervision**, and both proxy balances sit inside the
solvency sum (§7). Operator revenue — `@sys` fees and import duties — off-ramps as an ordinary
`@sys` withdrawal. Cross-kernel peer funding then rides the rail: a kernel funds its balance on a
peer by moving USDC between the two kernels' Banks, replacing today's out-of-band, trust-the-peer
prepaid model with settled, solvency-audited transfers.

## 13. What changes in Juice

**Already present (reuse, do not reimplement):**

- Idempotent external-keyed adjustments — `Adjustment.external_key` is globally unique and a create
  with an existing key returns the existing record without re-applying the balance change (§3).
- Atomic burn-first with idempotent replay — `Withdraw` re-checks `available ≥ amount` atomically
  and, on replay, returns the existing record *before* the balance check (§12).

**Genuinely required:**

1. **Conflict-detecting idempotency.** Today a matching key *silently returns*; the rail needs a
   replay with conflicting `(target, direction, amount)` to **reject**. This is changed semantics on
   the existing `external_key` seam (persist and compare the bound fields), not necessarily new
   columns — the canonical event identity serializes into `external_key`.
2. **A Juice-originated signed, append-only money-event log** exported to storage the Rail cannot
   rewrite (a signed table in the same DB is not independently append-only), plus periodic signed
   `Σ(available+locked)` checkpoints. Event fields:
   `sequence · previous_hash · event_type · adjustment_id · external_key · direction ·
   target_user_id · amount · deposit_event_hash | withdrawal_intent_hash · created_at · signature`.
3. **A scoped money-operator credential** (deposit, withdraw, reconciliation-read only). Juice
   authority is monolithic today — `@sys` also carries suspend, action-disable, peer ops, and
   process termination — far too broad for a rail host.
4. **A user withdrawal-key registry and intent verification** (§8), and a **secp256k1 burn-attestation
   signer** (§9) so the Bank can verify Juice's burn.

**Placement of withdrawal state.** Destination, intent hash, nonce, expiry, chain/contract, and
release status live in the **external Rail database and the signed event log**, not the kernel —
Juice keeps only the burn `Adjustment` and the event identity. The earlier claim that
conflict-detecting idempotency was the *only* forced persistence change holds solely under this
explicit placement decision.

Everything else — the Bank contract and its state machine, finality gating, dual chain observation,
Monitor approval, and governance — lives outside the kernel.

## 14. Headline

Minting requires two independent chain attestations; releasing requires the user's destination
signature, a Juice attestation that the credits were burned, and the Monitor's solvency approval,
against an immutable-or-timelocked Bank. No single operator component can over-issue credits or move
USDC out of the Bank. A compromised Rail is reduced to proposing and delaying; a compromised Monitor
or a governance action can freeze but not steal; and no honest user's withdrawal can be redirected
without their key. That is the most the seam allows, and no more.
