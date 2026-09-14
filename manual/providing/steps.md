---
title: Steps and processes
parent: Providing actions
nav_order: 4
---

# Steps and processes

A **step** is a call your action sets aside, pays for in advance, and addresses to
one named party. The call suspends; when that party supplies what is missing, it
resumes and settles. This is how an action waits for a person, for an approval, or
for an external system, without holding anything open in your own code.

For what it looks like to the party being asked, see
[Consent and assigned work](../calling/consent-and-steps.html#completing-work-addressed-to-you).

## The simplest case

`sys/message` creates a step addressed to a user, carrying a message:

```
$ juice run sys/message '{"to":"bob","message":"approve the order?"}'
  result: {
    "step_id": "b75366d1-…"
  }
  …
```

Bob now has work waiting. The process that ran `sys/message` stays open until he
answers it.

## Creating a step yourself

From WebAssembly:

```go
id := JuiceStepCreate(
	[]byte(`{"order":"42"}`),  // arguments already known
	"bob",                     // who may complete it
	"bob/approve",             // what runs when they do
)
```

From an HTTP endpoint, using the capability it was dispatched with:

```
POST /v1/steps
X-Juice-Capability: <the capability it was dispatched with>

{"action": "bob/approve", "required_caller": "bob", "partial_args": {"order": "42"}}
```

From the command line, naming the call whose budget pays for it:

```
$ juice step create bob/approve --trace <trace-id> --required-caller bob \
    --partial-args '{"order":"42"}'
```

`--trace` must name a call that is still running, so this form is for adding a
step to a call already suspended on one. A call that has returned is settled and
cannot fund anything:

```
error: create step: the call has already settled
```

Steps created as part of doing the work come from the first two forms.

Three things are fixed when the step is created.

**The price is taken then.** The target action's price is deducted from the
current call's budget and parked. Completion needs no further funds, and the price
cannot move underneath it afterwards.

**The party is mandatory.** There is no open step that anyone may complete and no
superuser override. The named account must exist.

**What is missing is derived.** `partial_args` is what you already know;
completion supplies the rest of the target action's input, and the two are merged
with the completer's values winning. The completer never needs to read the target
action, which may be private to you.

## What happens to a step

A step is `waiting` until someone completes it, `running` while it executes, and
then `done` with a transaction, or `cancelled`.

Waiting steps survive a restart of the kernel. A step that was executing when the
kernel stopped returns to `waiting` with its money re-parked, so it can be
completed again.

A step is cancelled, and its money returned, when the process is ended or when the
call that created it fails.

## Processes

A process holds the money for one `run` and everything beneath it. It closes by
itself when the root call has returned, no step is waiting, and no call is
awaiting a receipt from another kernel. Closing returns whatever is left to the
owner.

```
$ juice process list
e3539f75-…  open    available:0.00 credits  locked:0.00 credits
```

### Ending a process

The owner of a process can end it:

```
$ juice process end e3539f75-…
Process e3539f75-… ended.
```

Ending cancels the waiting steps and returns their parked money. Use it for work
that was abandoned: an approval nobody will give, a message nobody will answer.

Ending a process that still has a call in flight to another kernel fails that
call locally and refunds it. If the other kernel did execute it, the two operators
reconcile the difference between them afterwards. Prefer to wait: a parked call
settles on its own within 24 hours.

## External systems

There is no webhook machinery. An external system participates as an ordinary
account: it authenticates, and either runs an action or completes a step created
for it in advance.

To have a system deliver an event, create a step addressed to its account and give
it the step id. When the event happens, it completes the step, and your suspended
computation resumes with the event's data as input. The money was reserved when the
step was created, so nothing depends on that system holding a balance.

## What the kernel does not provide

There are no coordination primitives: no waiting for the first of several steps, no
quorum, no deadline that fires by itself. A step is one call, addressed to one
party. Anything more is built out of steps in your own action.
