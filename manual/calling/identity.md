---
title: Accounts, logins and identity
parent: Calling actions
nav_order: 1
---

# Accounts, logins and identity

Your account belongs to a kernel, while your client keeps the information needed
to reach that kernel and authenticate to the account. Understanding this
distinction makes it easier to work with several accounts or kernels from the
same machine.

## Accounts

Each account has its own balance and history on the kernel where it was created.
Its handle is unique there, but another kernel may have an unrelated account
with the same handle. The following command creates `alice` on the kernel that
your client knows as `acme`:

```
$ juice user create alice@acme
Password:
Confirm password:
Recovery phrase (write this down; it is shown only once and cannot be recovered):
  prepare divorce absurd cabin series excite lunar vicious approve brown fossil window
Press Enter once you have written it down:
  available: 0.00 fUSD
  handle: alice
  …
```

The handle itself contains neither `@` nor `/`; the `@` in this command
separates the handle from the kernel name. A leading `@` in a handle is invalid.

Account creation prints a twelve-word recovery phrase. Save it before pressing
Enter, since it is shown only once and is required to recover a lost password.
After creating the account, log in to establish a session.

## Logins

A **login** is the client's saved session for an account, identified by
`handle@kernel`. Logging in authenticates you to that account and selects it
for subsequent commands:

```
$ juice auth login alice@acme
Password:
alice@acme
```

The client stores each login's session tokens separately. It can hold several
logins at once, including logins for different accounts on the same kernel.
Use `auth list` to see them and `auth use` to change the selected one:

```
$ juice auth list
LOGIN       IN USE
alice@acme
sys@acme    yes
$ juice auth use alice@acme
alice@acme
```

`auth logout` ends a saved session and leaves no login selected. Programs using
that same saved login share its session, so logging it out also affects them.

## Naming one login for one command

When you want to use a different account for one command, `--as` names an
existing login without changing the client's selection:

```
$ juice --as bob@acme user me
```

The environment variable `JUICE_AS` provides the same choice. An unknown login
causes an error, so a misspelling cannot cause the command to use the selected
account instead. Unattended programs should always specify a login: the client
selection may change as a person uses other accounts on the same machine.

## The three names a kernel has

Kernel names have three roles. A nickname labels a kernel, a client name selects
it from your machine, and a petname identifies a remote kernel in references
resolved by your own kernel:

| Name | Chosen by | Where it resolves |
|---|---|---|
| Nickname | the kernel's operator | nowhere; it is a label the kernel reports about itself |
| Client name | `serve` uses the nickname; you can choose another with `kernel add` | on this machine, in logins such as `handle@kernel` |
| Petname | one kernel's operator, for another kernel | on that kernel only, in `owner@kernel/name` |

These names may happen to be the same. For example, `kernel add` uses the
reported nickname when you supply no client name. Their meanings still depend
on where they are used, and the public key remains the kernel's identity even
when its names change. A kernel you serve is registered on every boot. Each
key has one client record: registering a known kernel under a new name renames
that record and keeps its logins. A name held by a different kernel is refused.

## Registering and trusting a kernel

```
$ juice kernel add https://kernel.example.org work
work  network play  https://kernel.example.org  fdlMi64P…  (added)
$ juice kernel list
KERNEL  NETWORK  ADDRESS                     IN USE  KEY
work    play     https://kernel.example.org          fdlMi64P…
$ juice kernel health work
ok  acme  network play  fdlMi64P…
```

Registration records the public key and network returned by the address you
provide. Later login and selection operations check those values before trusting
the server. If you register a new address under an existing client name, the
client accepts it only when it identifies the same kernel on the same network;
the saved logins are then retained.

Use `kernel forget` to remove a registration and its saved credentials.
For an unauthenticated request, `--server URL` can address a server directly;
it does not carry a saved login to that address.

## Password and profile

```
$ juice user update --description "I run the nightly summariser"
$ juice user update --password
Current password:
New password:
Confirm password:
```

Passwords are at least eight characters; there are no composition rules.

## Recovering a lost password

```
$ juice auth recover alice@acme
Recovery phrase:
New password:
Confirm password:
  status: ok
```

The client uses the recovery phrase to prove that you hold the account's
recovery key, allowing the kernel to set a new password. Juice provides no
email recovery. If both the password and the phrase are lost, this recovery
procedure is unavailable.

## Suspension

An operator may suspend an account to prevent it from making authenticated
requests. A command using a suspended account therefore fails even if its
session credentials are otherwise valid:

```
$ juice user me
error: account suspended
```

Suspension preserves the account's balance and history. The operator can restore
access by unsuspending it.
