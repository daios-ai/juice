---
title: Steps and processes
parent: Providing actions
nav_order: 4
---

# Steps and processes

A **step** represents a future action call waiting for input from a named party.
Your action supplies the arguments already known and reserves the execution
price. Later, the named party completes the input and the target action runs.
This supports approvals, messages, and external events without requiring your
endpoint or module to remain running during the wait.

Creating a step does not suspend execution at the next line of your code.
The creating action may return and settle while its process remains open for
the step. Put the work that should follow the response in the step's target
action.

For what it looks like to the party being asked, see
[Consent and assigned work](../calling/consent-and-steps.html#completing-work-addressed-to-you).

## The simplest case

The built-in `sys/message` action creates a step carrying a message to a named
recipient. Its target is `sys/sink`, so completion acknowledges the work without
performing a further service:

```
$ juice run sys/message '{"to":"bob","message":"approve the order?"}'
sys/message costs 0.00 fUSD. Run it? [y/N] y
  result: {
    "step_id": "b75366d1-…"
  }
  …
```

Bob can now see the waiting step. Although `sys/message` has returned, its
process remains open until the step completes or the owner cancels the work.

## Creating a step yourself

In your own workflow, choose a target action that performs the continuation.
The following WebAssembly call supplies an order identifier in advance and
addresses the remaining input to Bob:

```go
id := JuiceStepCreate(
	[]byte(`{"order":"42"}`),  // arguments already known
	"bob",                     // who may complete it
	"bob/approve",             // what runs when they do
)
```

An HTTP endpoint creates the same kind of step using its execution capability:

```
POST /v1/steps
X-Juice-Capability: <the capability it was dispatched with>

{"action": "bob/approve", "required_caller": "bob", "partial_args": {"order": "42"}}
```

The command-line form identifies the trace whose budget funds the step:

```
$ juice step create bob/approve --trace <trace-id> --required-caller bob \
    --partial-args '{"order":"42"}'
```

The trace must still be unsettled, and the authenticated caller must have
authority to use it. An open process with a waiting step does not, by itself,
make a settled trace spendable again. Attempting to fund a step from a settled
trace gives:

```
error: create step: the call has already settled
```

For steps created as part of normal execution, use the WebAssembly host or HTTP
callback. Both already identify the executing trace and its budget.

At creation, the kernel reserves the target action's current execution price.
Completion uses that reservation even if the price later changes. Separate
value or remote-ticket requirements still belong to the immediate caller.

The required party must be named and resolvable. Only that party can complete
the step; the operator has no override. Visibility is checked against the
creator when the target is bound, allowing you to reserve a private helper for
completion by someone else.

The `partial_args` contain known input. The kernel derives an `allowed_input`
schema for what remains and checks the completer's submission against it before
merging and validating the complete arguments. The completer can therefore
supply the missing information without needing access to the private target's
full definition.

## What happens to a step

A step begins as `waiting`. An authorized completion claims it as `running`,
and settlement records its transaction and marks it `done`. If a precondition
fails before execution, the step returns to `waiting` with its reservation
intact. The target must still be active and its owner unsuspended, although
narrowing visibility after creation does not prevent completion.

Waiting steps survive restarts. Interrupted local completions with no committed
transaction are re-parked for another attempt; work awaiting a remote receipt
continues through the remote retry mechanism. Ending the process or failing the
creating call cancels waiting steps and returns their funds. Running work must
settle before its parent can close.

## Processes

A process groups the calls and steps started by one `run`, together with their
funds. It closes automatically once the root call and its descendants have
settled and no steps remain outstanding. Any funds left at closure return to
the process owner.

```
$ juice process list
PROCESS     STATUS  AVAILABLE   LOCKED      AWAITING SINCE
e3539f75-…  open    0.00 fUSD  0.00 fUSD
```

### Ending a process

The process owner can end work that is no longer wanted:

```
$ juice process end e3539f75-…
Process e3539f75-… ended.
```

Ending cancels waiting steps and returns their reserved funds. It is useful for
abandoned approvals or messages whose recipients will not respond.

Closure is refused while a call awaits a peer's signed receipt or refusal;
the message says how long it has waited. Such calls have no timeout refund,
because the work may already have executed on the other kernel.
Closure is refused while other execution remains in flight; the owner can try
again after that work settles.

## External systems

An external system can deliver events using an ordinary Juice account. It can
either run an action when the event occurs or complete a step you created for
it in advance.

For the second arrangement, address the step to the system's account and give
it the step ID. When the event occurs, the system authenticates and completes
the step with the event data. The target action then performs the next part of
the workflow using the reserved execution budget.

## What the kernel does not provide

A step represents one future call assigned to one party. More elaborate
coordination, such as accepting the first of several replies, collecting a
quorum, or enforcing a deadline, belongs in the application that creates and
manages those steps.
