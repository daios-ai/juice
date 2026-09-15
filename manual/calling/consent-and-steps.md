---
title: Consent and assigned work
parent: Calling actions
nav_order: 7
---

# Consent and assigned work

Two things a buyer is sometimes asked to do: connect an account of their own so an
action can act on their behalf elsewhere, and complete a piece of work addressed
to them.

## Connecting an upstream account

Some actions work against a service where you, not the provider, hold the account:
your mailbox, your calendar, your repository. Such an action cannot run for you
until you have connected that account.

A call that needs a consent you have not given is refused before any money moves,
and names what to connect:

```
$ juice run bob/mail '{"body":"hi"}'

Authorize with:
  juice user connect bob/mail
error: grant required for bob/mail
```

Connect in one of two ways, depending on what the upstream service supports.

**By browser consent**, for services that use OAuth:

```
$ juice user connect bob/mail
```

The command prints a URL to open. You authorise at the provider's own site and
return. `--device` uses the device-code flow instead, for a machine with no
browser.

**By pasting a token**, for services that issue personal keys:

```
$ juice user connect bob/mail --token ghp_…
Connected bob/mail.
```

One consent covers a group of related actions. The selector is an owner
(`bob`), a directory (`bob/mail`), or a single action (`bob/mail/send`); the
consent applies to every action beneath the path you name that uses the same
upstream account.

## Seeing and revoking consents

```
$ juice user me
  connections: [
    {
      "provider": "httpbin.org",
      "actions": 1,
      "unused": false,
      "provider_key": "bearer:httpbin.org",
      "created_at": "2026-09-14T12:07:00Z"
    }
  ]
  connectors: [
    {
      "directory": "bob",
      "connections": [ … ],
      "actions": [
        { "action": "bob/mail", "provider_key": "bearer:httpbin.org", … }
      ]
    }
  ]
```

`connections` is your inventory of upstream accounts. `connectors` groups the
actions each one has been granted to. The credential itself never appears here or
anywhere else: not in inputs, outputs, logs, receipts, or any other read path.

```
$ juice user disconnect bob/mail
  revoked: [
    "bob/mail"
  ]
```

`user disconnect --account <provider_key>` removes an upstream account entirely
and every consent that points at it.

A credential is applied only when three things hold at once: you granted it, the
action being executed is the one you granted it for, and you are the one paying.
It is never inherited by an action that this action calls, never sent to another
kernel, and never visible to code running inside the kernel.

A consent is revoked automatically if the action's contract changes, so it can
never come back attached to terms you did not agree to.

## Completing work addressed to you

An action can set work aside and address it to a named party. This is called a
**step**. The money for it is reserved when the step is created, so completing it
costs you nothing.

```
$ juice step list
b75366d1-…  waiting  sys/message → sys/sink
$ juice step show b75366d1-…
  id: b75366d1-…
  partial_args: {
    "message": "approve the order?"
  }
  price: 0.00 credits
  status: waiting
  created_at: 2026-09-14T12:05:35Z
  action: sys/sink
  created_by: sys/message
  owner_handle: alice
  required_caller_handle: bob
  allowed_input: {
    "type": "object"
  }
```

`created_by` is the action that set the work aside, which is what the step means.
`action` is what will run when you complete it. `owner_handle` is who is paying.
`allowed_input` is the schema of what you must supply: the target action's input
minus what has already been filled in.

```
$ juice step complete b75366d1-… '{}'
  result: {}
  tx_id: 4a908ead-…
  trace_id: 3129b306-…
  receipt_id: 6954ae74-…
  step_id: b75366d1-…
```

Completion runs the action and settles it. Only the named party can complete a
step, and a step completes once. Waiting steps survive restarts of the kernel.

## Work held for you on another kernel

If a step is addressed to you but was created on another kernel, list and complete
it there by naming that kernel:

```
$ juice step list --peer beta-kernel
$ juice step complete <id> '{}' --peer beta-kernel
```

Your kernel proves to the other one that you are who the step names. The other
kernel is told only which step is being answered; it learns nothing else about
you.
