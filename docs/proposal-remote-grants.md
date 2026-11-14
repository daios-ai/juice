# Proposal: grants across kernels

Status: built. `requirements.md` (U23, U27, U41, P3–P7, D2, D10, D16) is the contract; this file is the reasoning behind it.

## Goal

A user creates a grant for a delegated action once, with the existing commands. The grant is then
consulted under that user for every call in a process the user owns, while the process is open.

This is the present local rule (D2, D10). The proposal makes it hold on every kernel that does
work for the process, whichever kernel hosts the delegated action and whichever kernels host the
actions that call it.

The user holds no account on another kernel and handles nothing that a local user does not.

## What the user experiences

1. The user signs up once with `user connect carol@k/chat`. The command is the same for an
   application on the user's kernel and on another.
2. The application recognises the user on every call.
3. It also does so when the user runs any action that calls the application for them, on any
   kernel.
4. It also does so when that work waits and continues later.
5. `process list` shows the run while any part of it is outstanding on any kernel. `process end`
   stops the use of the user's grants in that run on every kernel at once.
6. `user disconnect carol@k/chat` ends the sign-up.

## Terms

All terms are those of `requirements.md`, except the last two.

- A **delegated action** is an action whose auth scheme is `oauth_delegated` or `delegated_bearer`.
- A **grant** records one user's consent for one action. It holds no secret.
- A **connection** holds the user's credential for one provider.
- The **process owner** is the user who started the run and pays for it.
- A **principal** is `(kernel key, user id)`.
- The **user's kernel** is where the user has an account. The **serving kernel** is where the
  delegated action lives.
- A **reference** is a random value that the user's kernel creates for one process. It lets
  another kernel ask the user's kernel about that process (Design, section 5).
- A **registration** is a record on the user's kernel that another kernel holds waiting work for
  one of its processes. It keeps that process open (Design, section 6).

## Present behaviour

A grant belongs to one user and one action on the same kernel. The kernel refuses a call to a
delegated action before locking funds when the process owner holds no grant for it
(`kernel/call.go:933`). At dispatch it applies the credential when the grantor is the process
owner and the granted action is the executing action (`cmd/juice/authenticator.go:303`).

The check is keyed on the process, not on the caller. It therefore applies to every call in the
process: a direct call, a subcall made by a composing action, and a task completion, whose trace
is created in the original process (`kernel/tasks.go:608`, `kernel/call.go:564`). A composing
action is an action whose code calls other actions.

A process stays open while it has a task waiting or running, or a trace not yet settled
(`store/sqlite.go:1314`). A closed process cannot call (D2). The user can end a process. A grant
is therefore usable exactly while a process of its grantor is open.

## Why it fails across kernels

1. A delegated action is never served in a manifest (`kernel/federation.go:2566`), so no other
   kernel can resolve it.
2. A grant and a connection are keyed by a local user id.
3. A cross-kernel call names the immediate caller's user and nothing about the process owner (P4).
4. On a kernel that serves a call, the process owner is the action's owner (D2). The user who
   started the run is unknown one kernel away, and at every kernel after that.
5. Process closure counts only the kernel's own tasks and traces. When a call to another kernel
   returns and leaves a task waiting there, the user's process closes although its work
   continues.

P6 gives the reason for point 1 as "a peer's single account can neither consent nor hold a
per-caller token". Parties are now principals, so a remote user can be named individually and the
reason no longer applies.

## Design

The design has three parts beyond the sign-up itself.

- **Reference.** Every process has a random reference. Every cross-kernel call carries it.
- **Check.** A kernel that needs the user's grant asks the user's kernel about the reference.
- **Lifetime.** A kernel that holds waiting work registers it at the user's kernel, which keeps
  the process open.

The connection stays on the serving kernel. No credential and no user identity travels with a
call. All questions about a process go to the kernel that owns it.

### 1. Grantor is a principal

`Grant.grantor` and `Connection.user` become principals. A local user is the principal with an
empty kernel, as elsewhere (D15). The sealing AAD of a connection takes the principal in place of
the user id.

### 2. Manifest

A delegated action is served in a manifest. The manifest carries one new field, `requires_grant`,
which is part of the contract hash. It carries no scheme; `plan` reports the scheme.

### 3. Grant verbs

The five grant operations are `plan`, `start`, `complete`, token submission and `disconnect`
(D20). For a selector naming another kernel, the user's kernel forwards the operation to that
kernel on a new stream, signed, bound to its recipient, and naming the user by stable id and
handle, as P8 does for task completion.

The serving kernel runs its existing consent code with the principal as grantor. It expands the
selector over its own actions under `CanCall` for that principal, derives `provider_key` as it
does today, performs the OAuth exchange, and stores the connection and grants.

A bearer token travels once, at submission. It is carried beside the signed payload, which signs
its hash, and it is stored only sealed.

`complete` carries the selector as well as the state, so the user's kernel knows where to forward
it. `user disconnect --account P@kernel` names the kernel that holds the connection and is
forwarded once; plain `--account P` is local.

**Ordering.** The signed timestamp of a forwarded operation is its number, at nanosecond
resolution. The serving kernel keeps the latest one per user and kernel and refuses an older
operation. Sign-up and disconnect are idempotent, so nothing else is stored and a repeat is
harmless. A delayed `disconnect` therefore cannot delete a later sign-up.

**Limits.** The serving kernel holds at most 10,000 connections for users of other kernels, and
at most 1,000 for the users of any one kernel. It holds at most 320 unfinished sign-ups, at most
32 per kernel, and one per user and provider: a new one replaces the old. The values are
constants, beside the transport's (`fed/transport.go:529`). A request beyond a limit is refused.
The connections of a suspended kernel are kept and count.

**Completion.** A sign-up records each action's last-update mark when it starts. The transaction
that writes the grants refuses if any mark has changed. An event that revokes an action's grants
(D10) therefore also defeats every sign-up started before it, including one whose completion is
already running. This corrects a defect that exists locally today
(`cmd/juice/authenticator.go:679`).

### 4. Consent record on the user's kernel

The user's kernel keeps one record per grant held on another kernel. The record is a grant row
keyed to the user and to the proxy of the remote action, with no connection. It holds no secret
and no scopes. It serves three purposes.

- `user me` lists the user's grants on other kernels.
- A direct call by a process owner with no record is refused before funds are locked, as locally
  (U28).
- It decides whether the user's kernel names the user to a kernel that asks (section 5).

### 5. Reference and check

**Reference.** The user's kernel creates a random reference with every process. The reference is
not the process id. It is never displayed and never enters a receipt or evidence.

**Carry.** Each cross-kernel call from the process carries two fields beside the signed call
request: the key of the user's kernel and the reference. A kernel that serves a call stores both
on the process it creates for that call. Every cross-kernel call from that process, including one
made by a task completion, carries them unchanged. A call carries nothing that identifies the
user.

**Check.** Before a kernel uses a grant in a served process, it sends the reference to the
user's kernel. The user's kernel answers with the process owner's user id and handle when both of
the following hold, and otherwise answers no.

- The reference belongs to an open process.
- The process owner holds a consent record for the asking kernel.

On a positive answer, the principal formed by the user's kernel and the returned user id is the
grantor for that call. The asking kernel then applies its existing rule: the grantor must hold a
grant for the executing action.

On a negative answer the call has no grantor and is refused as in section 7.

When the user's kernel cannot be reached, the call is refused with an error that says so. This is
not a revocation, and the call can be made again.

A positive answer is never cached. Each use of a grant is checked against the present state of
the process.

In a process on the user's own kernel the grantor is the process owner, as today, and no message
is sent. When a call returns to the user's kernel carrying its own key, that kernel looks the
reference up locally.

The grantor is separate from the process owner of the served process. Funding, settlement and
the immediate caller are unchanged.

In a served process the grantor is the user the user's kernel names, or nobody. The grant of the
served action's owner is never used. This changes present behaviour, where a subcall in a served
process consults the owner's grant (`kernel/call.go:564`).

A kernel in the middle of a run does not decide for a proxy that requires a grant. In a served
process it forwards the call, and the serving kernel decides.

The check, the registration and the deregistration are not signed. They ride the authenticated
connection between the two kernels, as `resolve` does.

### 6. Process lifetime across kernels

A process stays open while another kernel holds waiting work for it.

**Register.** When the root call of a served process returns and the process is still open, the
serving kernel registers at the user's kernel. The registration names the reference and the
served call. The serving kernel sends its reply only after the user's kernel has acknowledged the
registration.

**Keep open.** The user's kernel does not close a process automatically while it holds a
registration for it. A registration is not money: it delays closure only.

**Deregister.** When the served process closes, the serving kernel records this in the same
commit and tells the user's kernel until it acknowledges. The user's kernel accepts a
deregistration only from the kernel that registered, and removes that one registration.

Four rules make this correct.

- **The process is always protected by one thing.** Before the reply, the user's process is open
  because the call it made has not returned. After the reply, it is open because of the
  registration. The registration is acknowledged before the reply is sent, so there is no moment
  between the two.
- **A failed registration delays the reply.** When the user's kernel cannot be reached, the
  serving kernel keeps the result, retries the registration, and replies after the
  acknowledgement. The caller sees a call that has not yet returned, which Juice already has and
  shows (U35).
- **A registration names one served call.** A kernel can serve several calls for the same
  reference. Finishing one removes only its own registration.
- **A deregistration is final.** A released registration is marked, not deleted, until its
  process closes. A late or repeated message never restores a registration and never reopens a
  process.
- **No result before the acknowledgement.** The rule covers the first reply and every answer to a
  repeated request, which Juice otherwise serves from the stored receipt
  (`cmd/juice/fedservice.go:333`).
- **Registrations are limited.** A process accepts at most 1,024 registrations over its life, a
  constant. All are removed when the process closes. A refusal is logged on both kernels.

The user's kernel refuses a registration when the process is closed or its limit is reached. The
serving kernel then replies as usual, and its waiting work loses its grantor when the process
closes.

Kernels in the middle of a run take no part. A kernel registers its own waiting work and nothing
else.

The user sees the open process in `process list` and can end it with `process end`, which drops
its registrations. From then on the user's kernel answers no to every check for that process. A
registration does not prevent the user from ending the process; a call that has not returned
still does (U23).

### 7. Stale records and refusals

The serving kernel is the authority on its grants.

- The grantor holds no grant for the action called directly. The serving kernel answers with a
  signed rejection that states a grant is required, and the caller's proxy stays active. When the
  caller is the user's kernel, it deletes its record for that action, refunds in full and reports
  `ErrGrantRequired`.
- A grant is missing inside a composing action, on any kernel. This is an ordinary subcall
  failure: work already delivered stays paid, as locally (U13).
- The user's kernel holds no record. It refuses a direct call locally and answers no to a check.
  The user connects again, and the serving kernel answers that the scopes are already covered.

### 8. Revocation and lifecycle

- `process end` makes every later check for that process fail, on every kernel. A use that was
  already approved may complete.
- `user disconnect` deletes the consent record at once and tells the serving kernel. A direct
  call to the disconnected action is refused by the user's kernel at once. A call through another
  kernel is refused once the serving kernel has applied the revocation. If the
  serving kernel cannot be reached, the command says so and the user runs it again; it is not
  retried in the background. A disconnect of a whole account removes the local records when the
  serving kernel answers, because only that kernel knows which actions the account covers.
- The serving kernel revokes as it does today: a contract-changing update, auth replacement,
  delete, or `invalid_grant` (D10). The next call meets the rule in section 7.
- A peer whose users hold connections is retained (D16). A sign-up on another kernel does not
  expire, as a local one does not.
- Suspending a kernel refuses its calls and the references of its processes, and keeps its
  users' grants (U37).

## Why this design

1. **The local rule is the specification.** Locally a grant follows the process, through
   subcalls, task completions, retries and restarts, and it ends when the process closes. A
   design keyed on the caller, on one hop, or on elapsed time behaves differently depending on
   where an action is hosted.
2. **Only the user's kernel knows whether the process is open, and whose it is.** Asking it gives
   every kernel the answer a local call gets, at the moment of use. Nothing is signed in advance
   and there is no validity period to choose. A kernel in the middle of a run carries a reference
   and cannot answer for it.
3. **The answer is correct only if the process outlives its remote work.** Without the
   registration, the user's kernel would answer "closed" while a task the run left on another
   kernel is still waiting. The registration extends an existing rule: a waiting task keeps its
   process open.
4. **Registration is direct, not passed along the chain.** If each kernel reported to its caller,
   a kernel in the middle that is offline or faulty would keep the user's process open too long
   or let it close too early. Reporting directly to the user's kernel removes that dependency and
   leaves the kernels in the middle with nothing to store.
5. **The reference exists for every process.** A rule with no condition is simpler. It is also
   required: a user can sign up while a run is waiting, and the run must then find the grant when
   it continues, as it does locally.
6. **A call does not identify the user; the user's kernel does.** It names the user to any kernel
   holding the run's reference, as an action on the user's own kernel would learn it. A kernel in
   the run learns whose run it is, exactly as a local action does (U41).
7. **The credential does not move.** The serving kernel already holds the action's OAuth
   configuration and performs the exchange and refresh. Keeping the connection there reuses that
   code.

## Example

`alice@a` uses `carol@k/chat`, a `delegated_bearer` application. `bob@c/assistant` is a composing
action on a third kernel that calls `carol@k/chat/post`.

1. Alice runs `user connect carol@k/chat --token T` on kernel `a`.
2. Kernel `a` forwards the submission to kernel `k`, signed, naming Alice.
3. Kernel `k` stores a connection and grants for `(a, alice)`. Kernel `a` stores consent records.
4. Alice runs `carol@k/chat/post`. Kernel `a` sends the call with the reference of her process.
   Kernel `k` asks kernel `a`, receives Alice, finds her grant for `chat/post`, and applies `T`.
5. Alice runs `bob@c/assistant`. Kernel `a` sends the call to kernel `c` with the reference.
   Kernel `c` stores it on the process it creates.
6. `assistant` calls `carol@k/chat/post`. Kernel `c` sends the call with the same reference.
   Kernel `k` asks kernel `a`, receives Alice, and applies `T`. Bob's account on kernel `c` pays
   kernel `k`, as for any call. Kernel `c` never learns that the run is Alice's.
7. `assistant` leaves a task waiting and returns. Kernel `c` registers at kernel `a`, receives
   the acknowledgement, and then replies. Alice's process stays open.
8. The task completes the next day and calls `carol@k/chat/post`. Kernel `k` asks kernel `a`,
   receives Alice, and applies `T`.
9. The process on kernel `c` closes. Kernel `c` deregisters, and Alice's process closes.
10. Had Alice run `process end` after step 7, kernel `a` would answer no in step 8.

## Attack analysis

| Attack | Outcome |
|---|---|
| A kernel claims a run belongs to a user of another kernel | Impossible. A call names no user. The user's kernel is asked directly and names the user itself. |
| A kernel guesses a reference | Impossible in practice. The reference is random and is not the process id. |
| A kernel in the run learns who started it | By design, as a local action does. Only a kernel holding the run's reference is told, while the process is open. |
| A kernel outside the run uses the grant | It holds no reference. A kernel in the run could pass one on, which gives nothing beyond relaying calls for that kernel. |
| A kernel in the run uses the grant for calls the user did not intend | Possible while the process is open. This equals the local rule: any action in the user's process can call the user's granted actions. |
| A kernel in the run uses the reference after the process has closed | Impossible. The user's kernel answers no. |
| A kernel registers work that does not exist, or never deregisters | The user's process stays open and visible, and the user ends it. A local action can do the same today by leaving a task waiting. |
| A kernel removes another kernel's registration | Impossible. A deregistration is accepted only from the kernel that registered. |
| A late message reopens a closed process | Impossible. A deregistration and a closure are final. |
| A credential is captured from a call | No credential travels with a call. |
| A hostile action obtains another action's credential | Prevented by the binding rule and `provider_key` derivation, evaluated on the serving kernel as today. |
| A captured call request is replayed | Prevented by recipient binding and the idempotency key (P3, P4). |
| A revoked grant is used before the serving kernel acknowledges the revocation | A direct call is refused by the user's kernel at once. A call through another kernel can reach the action until the acknowledgement, if the user holds another grant on the serving kernel. |
| A consent state is completed by another party | The pending state is bound to the grantor principal, as it is bound to the grantor today. |
| A kernel invents users and fills the connection table with worthless tokens | Storage is bounded by the total cap, and one kernel by its own cap. Several kernels together can still fill the table and block new sign-ups from other kernels (Residual trust). |
| A kernel starts many sign-ups and never finishes them | Bounded by the limits on unfinished sign-ups. A replaced or expired sign-up stops its poller. |
| A kernel holding one reference creates unlimited registrations | Bounded by the limit per process. Released registrations count until the process closes. |
| A sign-up started before an action changed completes afterwards | Impossible. The transaction that writes the grants refuses when the action has changed. |
| A delayed `disconnect` deletes a later sign-up | Impossible. The serving kernel does not execute an operation with an older number. |
| A repeated call request obtains the result before the registration is acknowledged | Impossible. No answer is released before the acknowledgement. |

## Residual trust

The following are assumptions. No design of this kind removes them.

- The serving kernel's operator can read the credentials it stores, as for its local users.
- The user's kernel's operator can act as its users, as it can locally.
- A kernel that does work for the user's process can use the user's grants while the process is
  open. Locally the same holds for any action in the process.
- Kernel identities are free. The limits protect a kernel's storage and background work; they do
  not guarantee that a new user of another kernel is admitted while an attacker with several
  kernels holds the slots. Slots are freed only by disconnect and by revocation. Juice has no
  operator command that removes another kernel's connections, and this change adds none. This
  cannot be closed without making identities cost something (Douceur, 2002).

## Costs and boundaries

- One message from the serving kernel to the user's kernel for each use of a grant in a served
  process, and two for a call made directly to the delegated action: once before funds lock and
  once at dispatch.
- The user's kernel must be reachable when a grant is used in a served process, and when a kernel
  registers waiting work. While it is not, the first is refused and the second delays the reply.
- A deregistration can be delayed, so a process can stay open for some time after its remote work
  has finished. The guarantee is that it never closes early.
- Ending a process stops the use of its grants everywhere. It does not cancel a task waiting on
  another kernel, which is in the seller's process. This is the case today and is unchanged.
- `delegated_bearer` still requires the user to paste a token once, as it does locally.

## Precedent

- **OAuth 2.0 token introspection (RFC 7662).** A service holds an opaque reference and asks its
  issuer whether it is currently active and whom it stands for. This is the check. The RFC notes
  that caching a positive answer opens a revocation window, which is why section 5 forbids it.
- **Distributed garbage collection by reference listing (Network Objects, 1993; Java RMI).** An
  owner keeps an object alive while remote holders are registered with it; a holder registers
  and later deregisters, directly with the owner. This is the registration. A holder must
  complete its registration before it may use the reference, and a failed registration fails the
  operation rather than being skipped. The same work names the ordering of the two messages as
  the main correctness concern, hence the acknowledged registration and the final
  deregistration.
- **Hand-over-hand locking.** The next lock is acquired before the current one is released, so
  protection is never absent. This is the rule that the acknowledgement precedes the reply.
- **The Sybil attack (Douceur, 2002).** Where identities are free, one party can hold many. The
  established defence bounds what each identity and the whole system can consume, and leaves
  removal of an abusive identity to the operator.
- **TCP half-open connection queue (RFC 4987) and Juice's transport limits.** State created by
  unproven parties is bounded in total and per source. Juice already allows 8 streams per peer
  and 64 in total (`fed/transport.go:529`). These are the limits of section 3.
- **TCP sequence numbers and ERC-2612 nonces.** A receiver that tracks the last number applied
  cannot be made to apply an old instruction late. This is the ordering rule of section 3.
- **OAuth 2.0 authorization code binding (RFC 6749, section 4.1.3).** A code is honoured only for
  the request it was issued for. This is the completion rule of section 3.
- **Structured concurrency.** A parent scope does not end while work it started is running.
- **Temporal parent close policy.** A workflow system states explicitly what happens to child
  work when the parent closes.
- **OAuth 2.0 token exchange (RFC 8693).** The represented user and the immediate caller are
  separate identities in one request.
- **OAuth 2.0 authorization-code flow (RFC 6749).** The application's server holds and refreshes
  the user's tokens.
- **OpenID Connect and Shibboleth.** A service keys a user's state on `(issuer, subject)`,
  asserted by the user's home organisation.
- **Juice itself.** A waiting task keeps its process open (D2). A call whose outcome is not yet
  confirmed stays parked and visible (U23, U35). The user's kernel attests its user's stable id
  for task verbs (P8).

## What it changes in `requirements.md`

- U23: a process with a registration stays open, and the owner can end it.
- U27, U28: the grant applies on any kernel; "when that user is paying" becomes the process being
  that user's.
- U37: the references of a suspended kernel's processes are refused.
- U41: a call carries the user's kernel and a reference, and no user identity.
- P1: signature domain `grant`.
- P3: streams `/juice/fed/grant/1` for grant verbs and `/juice/fed/process/1` for the check, the
  registration and the deregistration.
- P4: the fields `process_kernel` and `process_ref` beside the call request; the grant-required
  rejection; a reply that waits for its registration.
- P5: the `grant_required` flag on a rejection receipt, beside `refresh_proxy`.
- P6: delegated actions are served; `requires_grant` is a contract field of the manifest.
- P7: a grant-required rejection does not deactivate the proxy.
- P9: delegated actions become eligible for execution evidence; the reference is never evidence.
- D2: the grantor of a call in a served process; closure also requires that no registration is
  held.
- D4, D10: the reference on a process; the registration and its limit; grant and connection keyed
  by principal; the consent record; the ordering of grant operations; the limits on connections
  and unfinished sign-ups; the completion rule; no use of the action owner's grant in a served
  process.
- D16: retention of a peer whose users hold connections.
- D19: a deregistration is recorded with the process's closure and delivered until acknowledged.
- D20: the grant commands accept a selector on another kernel; `complete` carries the selector;
  `--account` accepts `P@kernel`.

The call format changes, so the network upgrades by coordinated cutover (P3). The simulation is
updated and run on play and anvil.

## Facts confirmed in the code

- Streams between kernels are authenticated in both directions: the peer key comes from the
  Noise handshake (`fed/transport.go:506`).
- A call can settle while a task beneath it waits: closure counts waiting tasks separately from
  unsettled traces (`store/sqlite.go:1314`), and a served call uses the same call path as a local
  one (D2).

## Rejected alternatives

- **Each kernel reports waiting work to its caller, along the chain.** It depends on every kernel
  in the middle being online and correct, and each must store a record per call.
- **A kernel that cannot register replies anyway.** The user's process would close, and the
  waiting work would be refused the grant later, far from the cause.
- **A cap per kernel with no total cap.** Each further kernel key adds storage without end, and a
  full disk stops the whole kernel.
- **Deleting a kernel's connections after 90 days without contact.** A local sign-up does not
  expire, and a run that waits longer would lose its credential.
- **Sending grant operations in order, with no number.** A first attempt delayed inside the
  serving kernel can still run after a later sign-up.
- **A counter and stored replies for grant operations.** The operations are idempotent, so the
  timestamp already on each one is enough.
- **Retrying a disconnect in the background.** It needs stored state and a resend loop. The
  local effect is immediate without it.
- **Limits as configuration keys, and not counting a suspended kernel's connections.** Constants
  suffice, and stored credentials occupy storage whether or not their kernel is suspended.
- **Failing a call when its registration is refused.** It reorders settlement for a case that
  arises only under abuse.
- **A signed statement with an expiry, forwarded along the run.** Behaviour would depend on
  elapsed time: work that resumes after the expiry loses the grant, and a statement stays usable
  after its process has closed. Renewing it requires asking the user's kernel, which is the
  check of section 5 with more parts.
- **A reference only for users who hold a grant.** A run started before the user signs up would
  not find the grant when it continues.
- **The call names the process owner.** Every kernel in the run would learn who started it.
- **The process owner is forwarded with no check.** Any kernel could impersonate any user.
- **The user's kernel answers "open" for a fixed time after closure.** Behaviour would again
  depend on elapsed time.
- **The serving kernel withholds its receipt until all its tasks resolve.** The caller would not
  get its result until then, which changes how every call behaves.
- **The user's kernel holds the credential and sends it with each call.** It fails for
  `oauth_delegated` with a client secret, because the secret needed for refresh belongs to the
  action's owner. It also puts the credential on the network at every call.
- **The kernel passes the caller's identity to the action in a header.** It replaces grants and
  does not extend them. It was rejected on 2026-07-21.
- **The user opens a second account on the serving kernel.** It contradicts U29.
