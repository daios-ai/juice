---
title: Consent and assigned work
parent: Calling actions
nav_order: 5
---

# Consent and assigned work

Some services require your participation beyond the initial call. An action
that uses your mailbox or repository needs permission to access that account;
a workflow awaiting your input creates a task addressed to you. This chapter
explains how to give and revoke consent, and how to complete assigned work.

## Connecting an upstream account

An upstream service is the external system an action accesses while doing its
work. For a personal mailbox, calendar, or repository, the action needs a
credential for your account on that service. Connecting the account grants
specified actions permission to use it when you pay for their execution.

If a required consent is absent, the call is rejected before charging and
identifies what you need to connect:

```
$ juice run bob@acme/mail '{"body":"hi"}'
bob@acme/mail costs 0.00 fUSD. Run it? [y/N] y
error: grant required for bob@acme/mail
       Authorize it with: juice user connect bob@acme/mail
```

The connection procedure depends on the authentication scheme configured for
the action. With OAuth, start the consent flow using:

```
$ juice user connect bob@acme/mail
```

The command prints a URL where you can authorize access at the upstream
provider's site. Follow the prompts to complete the connection. If supported
by the provider, `--device` uses a device-code flow suitable for a machine
without a browser.

For an action configured to use a personal token, supply the token directly:

```
$ juice user connect bob@acme/mail --token ghp_…
Connected bob@acme/mail.
```

The reference you connect is a selector: it can name an owner (`bob@acme`), a
directory (`bob@acme/mail`), or a particular action (`bob@acme/mail/send`). The kernel
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
      "directory": "bob@acme",
      "connections": [ … ],
      "actions": [
        { "action": "bob@acme/mail", "provider_key": "bearer:httpbin.org", … }
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
$ juice user disconnect bob@acme/mail
  revoked: [
    "bob@acme/mail"
  ]
```

To remove the saved upstream account and all grants using it, use
`user disconnect --account <provider_key>`.

A grant authorizes a particular action when you are the payer. If that action
calls another, the second action needs its own grant. Delegated credentials
are not sent across federation or exposed to sandboxed code.

Changes to the action's source, schemas, or price revoke its grants, as do
credential replacement and deletion. Disabling the action or changing its
title or description alone does not revoke them. After revocation, reconnect
before using the action with your upstream account again.

## Completing work addressed to you

A **task** is a future action call awaiting input from a named party. Its
creator reserves the execution price when setting it up, so you do not pay
that price when completing it. An action that delivers value or calls a remote
kernel can still require the immediate caller's separate value or stake funds.
Use `task list` and `task show` to inspect the work addressed to you:

```
$ juice task list
TASK          STATUS   CREATED BY        COMPLETES      OWNER       CALLER
b75366d19c2e  waiting  sys@acme/message  sys@acme/sink  alice@acme  bob@acme
$ juice task show b75366d19c2e
  id: b75366d19c2e
  revision: 1
  status: waiting
  partial_args: {
    "message": "approve the order?"
  }
  allowed_input: {
    "type": "object"
  }
  price: 0.00 fUSD
  created_at: 2026-09-14T12:05:35Z
  action: sys@acme/sink
  created_by: sys@acme/message
  owner: alice@acme
  required_caller: bob@acme
```

The `created_by` field identifies the action that created the task, while
`action` identifies the service that will execute when you complete it.
`owner` names the process owner funding the work. The `revision` increases
whenever the task changes. Read `partial_args` for the information already
supplied and `allowed_input` for the schema of the remaining input:

```
$ juice task complete b75366d19c2e '{}'
  result: {}
  tx_id: 4a908ead-…
  trace_id: 3129b306-…
  receipt_id: 6954ae74-…
  charge: 0
  task_id: b75366d19c2e
```

The completion reply prints `charge` in base units and its transaction, trace
and receipt IDs in full, abbreviated here with `…`.

Completion claims the task for execution, preventing a second caller from
executing it concurrently. Only the named party may do this. The result and
transaction then record the completed work; a waiting task remains available
across kernel restarts.

To refuse the work instead, decline it. Its reserved price returns to the
process that set it aside:

```
$ juice task cancel b75366d19c2e
```

## Work from another kernel

A task addressed to you by a user of another kernel arrives in the same list,
and you answer it with the same commands. Its `owner` is the kernel that holds
it: that kernel withholds who funds the work and which actions are involved,
and sends only the request, its input requirements and its state. Your kernel
signs your completion or decline with your stable account ID, and the holder
checks it against the task before acting. Its reply updates your list before
the command returns.

A task that transfers money, such as one calling `sys/transfer`, cannot be
completed from another kernel. Completion would run as the account representing
your kernel there, and that account holds no funds to pay the transfer.
