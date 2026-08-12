# Juice — Adversarial Analysis

White-box review of the Juice kernel (requirements.md v0.12) covering the money
paths, the composition-capability system, steps, SSRF guards, authentication, and
the federation receipt verifier, plus light live probing of a throwaway kernel
(since torn down). Findings are ranked by how well they can be substantiated from
the source.

## Finding 1 (confirmed by code) — Composition capability exceeds its documented "same-trace" blast radius via step completion

§9 promises the trace-scoped capability "grants nothing else — no other trace or
process… its blast radius equals what the owner's WASM code could do **in the same
trace**." The implementation honors this for `/v1/call` (`postCall` pins
`ParentTraceID = capTrace`, `cmd/juice/serve.go:1375`) and for step *creation*
(`postStep` pins `traceID = capTrace`, `cmd/juice/serve.go:1269`). But step
*completion* is not trace-scoped:

- `postCompleteStep` (`cmd/juice/serve.go:1334-1338`) sets
  `callerID = capabilityOwner` and calls `completeStep(callerID, pathID(r), …)`
  with **no reference to the capability's trace or process**.
- `CompleteStep` (`kernel/steps.go:313`) authorizes solely on
  `callerID == step.RequiredCallerUserID`. There is no check that the step belongs
  to the capability's trace/process.

So a capability minted for trace *T* (owner = bob) can complete **any waiting step
anywhere in the kernel whose `required_caller` is bob**, spending those steps'
parked prices and producing their effects — not just steps in *T*.

**Why this is reachable.** The capability is delivered as a bearer header
(`X-Juice-Capability`) to the action's configured upstream host on *every* HTTP
dispatch when a callback URL is set (`cmd/juice/http_exec.go:533-535`), including
third-party APIs a provider merely wraps and leaf endpoints that never intend to
compose. It is stripped only on a host-*changing* redirect, not on the initial
hop. Any party that controls, proxies, logs, or compromises that upstream holds,
for the duration of the call, bob's authority to auto-complete bob's unrelated
pending steps (e.g. an "approve payout" step waiting on bob in another process).
It is confined to bob's own identity, but not to the trace — contradicting the
spec's confinement claim and creating a confused deputy across bob's processes.

**Sub-notes.**

- The owner-scoped-but-not-trace-scoped reach of `CompleteStep` is shared by the
  WASM `juice.step_complete` host function (`kernel/call.go:947`), but there it
  stays inside the owner's own code; the capability path *exports* it over the
  network.
- The token is a pure bearer (`VerifyCapability`, `kernel/capability.go:34`) with
  no binding to the endpoint it was issued to, so any leak is usable while the
  trace is unsettled.

**Suggested remediation.** Either scope `CompleteStep` to the capability's
process/trace on the capability path, or stop sending the capability header to
non-loopback upstreams by default.

## Finding 2 (design risk, spec-sanctioned) — loopback SSRF is open to any authenticated user

`UnsafeIP` (`kernel/kernel.go:1502`) deliberately omits loopback, and the dial
guard permits it by default (`cmd/juice/http_exec.go:90-99, 145-190`). Any
authenticated user can create a `kind=http` action or call `sys/web` pointing at
`http://127.0.0.1:<port>` / `localhost` and make the kernel issue requests to
co-located services (internal admin panels, unauthenticated databases/caches with
HTTP interfaces, cloud sidecars, the kernel's own non-superuser API).

The classic bypasses *are* correctly closed: the guard resolves the hostname,
validates every resolved address, then dials the validated IP
(resolve → validate-all → dial-the-validated-IP), so DNS-rebinding and numeric-IP
encodings are caught, and private/link-local resolved addresses are rejected even
behind a loopback redirect. This finding is specifically the *intentional*
loopback allowance — but on any multi-service host it is a real attack surface
handed to every user, not just the operator.

**Suggested remediation.** An explicit deployment warning, or an opt-in flag to
disable the loopback allowance for hosts that run other services alongside the
kernel.

## What was checked and found sound

The core local money paths held up under reading:

- `lockReserveTx` / `BeginRun` / `BeginSubcall` / `CommitCall` /
  `CommitFailedCall` / `EndProcess` use atomic guarded updates with
  `RowsAffected` checks and fail closed (funds-would-be-destroyed errors) on
  missing recipients.
- The failure refund arithmetic (`refund = trace.available + cancelled step
  prices`, with settled descendants staying paid) matches §6.
- Step claiming is a proper `waiting → running` compare-and-set.
- Auth codes, refresh-token rotation, and recovery challenges are single-use and
  atomic.
- The rate limiter's `X-Forwarded-For` handling only trusts the last hop from a
  loopback peer.
- PKCE cannot be downgraded to `plain` (the exchange always verifies S256).
- The remote-receipt verifier quarantines mispriced / mis-charged receipts.

## Scope and limitations

The two-kernel federation settlement math was **not** exercised live (it needs a
second peer), so the global exposure cap `X`, probabilistic residual settlement,
and value-transfer conservation are read-only-verified, not empirically tested.
No money-conservation break or authentication bypass reachable by an
unauthenticated or ordinary user was found in the local money paths that were
reviewed.

The strongest actionable item is **Finding 1**.
