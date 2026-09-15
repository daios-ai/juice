---
title: Consent and assigned work
parent: Calling actions
nav_order: 5
---

# Consent and assigned work

Some services require your participation beyond the initial call. An action
that uses your mailbox or repository needs permission to access that account;
a workflow awaiting your input creates a step addressed to you. This chapter
explains how to give and revoke consent, and how to complete assigned work.

## Connecting an upstream account

An upstream service is the external system an action accesses while doing its
work. For a personal mailbox, calendar, or repository, the action needs a
credential for your account on that service. Connecting the account grants
specified actions permission to use it when you pay for their execution.

If a required consent is absent, the call is rejected before charging and
identifies what you need to connect:

```
$ juice run bob/mail '{"body":"hi"}'

Authorize with:
  juice user connect bob/mail
error: grant required for bob/mail
```

The connection procedure depends on the authentication scheme configured for
the action. With OAuth, start the consent flow using:

```
$ juice user connect bob/mail
```

The command prints a URL where you can authorize access at the upstream
provider's site. Follow the prompts to complete the connection. If supported
by the provider, `--device` uses a device-code flow suitable for a machine
without a browser.

For an action configured to use a personal token, supply the token directly:

```
$ juice user connect bob/mail --token ghp_…
Connected bob/mail.
```

The reference you connect is a selector: it can name an owner (`bob`), a
directory (`bob/mail`), or a particular action (`bob/mail/send`). The kernel
groups accessible delegated actions under that path by upstream provider,
allowing one consent to cover related operations. The grant applies to the
actions included in that consent; adding another action later requires
connecting it as well.

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

The `connections` list shows the upstream accounts stored for you, including
accounts no longer used by any action. The `connectors` view groups the actions
you have authorized by directory. These views contain no credentials; the
kernel also excludes credentials from call inputs, outputs, logs, and receipts.
To revoke access for a selection of actions, use:

```
$ juice user disconnect bob/mail
  revoked: [
    "bob/mail"
  ]
```

To remove the saved upstream account and all grants using it, use
`user disconnect --account <provider_key>`.

A grant authorizes a particular action when you are the payer. If that action
calls another, the second action needs its own grant. Delegated credentials
are not sent across federation or exposed to sandboxed code.

Changes to the action's source, schemas, or price revoke its grants, as do
credential replacement and deletion. Disabling the action or changing its
description alone does not revoke them. After revocation, reconnect before
using the action with your upstream account again.

## Completing work addressed to you

A **step** is a future action call awaiting input from a named party. Its
creator reserves the execution price when setting it up, so you do not pay
that price when completing it. An action that delivers value or calls a remote
kernel can still require the immediate caller's separate value or stake funds.
Use `step list` and `step show` to inspect the work addressed to you:

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

The `created_by` field identifies the action that created the step, while
`action` identifies the service that will execute when you complete it.
`owner_handle` names the process owner funding the work. Read `partial_args`
for the information already supplied and `allowed_input` for the schema of
the remaining input:

```
$ juice step complete b75366d1-… '{}'
  result: {}
  tx_id: 4a908ead-…
  trace_id: 3129b306-…
  receipt_id: 6954ae74-…
  step_id: b75366d1-…
```

Completion claims the step for execution, preventing a second caller from
executing it concurrently. Only the named party may do this. The result and
transaction then record the completed work; a waiting step remains available
across kernel restarts.

## Work held for you on another kernel

Work addressed to you may be held by another kernel. Specify that peer to list
or complete its steps through your own login:

```
$ juice step list --peer beta-kernel
$ juice step complete <id> '{}' --peer beta-kernel
```

Your home kernel signs an attestation naming your stable account ID as the
party addressed by the step. The peer checks it before allowing completion. The
listing returns the step's input requirements without disclosing the remote
process owner's identity or other local execution details.
