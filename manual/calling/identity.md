---
title: Accounts, logins and identity
parent: Calling actions
nav_order: 1
---

# Accounts, logins and identity

## Accounts

An account is created on one kernel and exists only there. An account on
`acme` is unrelated to an account with the same handle on another kernel.

```
$ juice user create alice@acme
Password:
Confirm password:
Recovery phrase (write this down; it is shown only once and cannot be recovered):
  prepare divorce absurd cabin series excite lunar vicious approve brown fossil hard
Press Enter once you have written it down:
  available: 0.00 credits
  handle: alice
  …
```

The handle must be bare: letters, digits and the separators the kernel accepts,
with no `@` and no `/`. A leading `@` is rejected rather than removed. Handles are
unique on their kernel.

The twelve-word recovery phrase is shown once and is the only way to reset the
password. Write it down before pressing Enter.

Creating an account does not log you in.

## Logins

A **login** is one account at one kernel, written `handle@kernel`. It says who a
command acts as and which kernel it acts on.

```
$ juice auth login alice@acme
Password:
alice@acme
```

Logging in stores that login's tokens in their own file and selects it. The
selected login applies to every later command.

You may hold several logins at once, including several accounts on one kernel:

```
$ juice auth list
  alice@acme
* sys@acme
$ juice auth use alice@acme
alice@acme
```

`auth logout` ends a session. If the login was the selected one, nothing is
selected afterwards. Two programs logged in as the same account on the same
kernel share one session, so logging out ends it for both.

## Naming one login for one command

`--as` runs a single command as a login you already hold, without changing the
selection:

```
$ juice --as bob@acme user me
```

The environment variable `JUICE_AS` does the same. A name that is not a login on
this machine is refused; it never falls back to whoever is selected.

Programs that run unattended must name their login this way and never rely on the
selection, which a person at the same machine can change at any time.

## The three names a kernel has

Three different names can refer to a kernel, and they are not interchangeable.

| Name | Chosen by | Where it resolves |
|---|---|---|
| Nickname | the kernel's operator | nowhere; it is a label the kernel reports about itself |
| Client name | you, in `kernel add` | on this machine, in `handle@kernel` and `--server` |
| Petname | one kernel's operator, for another kernel | on that kernel only, in `owner@kernel/name` |

They often use the same word. `juice kernel add http://localhost:4040` with no
name registers the kernel under the nickname it reports, which is why the kernel
called `acme` is usually reached as `acme`. Nothing enforces that: a kernel's
nickname is not unique and not verified. What identifies a kernel is its public
key.

## Registering and trusting a kernel

```
$ juice kernel add http://localhost:4040 work
work  network play  http://localhost:4040  fdlMi64P…  (added)
$ juice kernel list
  KERNEL    NETWORK   ADDRESS                  KEY
  work      play      http://localhost:4040    fdlMi64P…
$ juice kernel health work
ok  acme  network play  fdlMi64P…
```

Registering dials the address and records the public key and network the kernel
reports. `auth login` and `auth use` refuse a server that no longer reports the
recorded key or network, so a stored credential is never sent to a different
kernel. The same key on the same network at a new address is taken as that kernel
having moved: the address is updated and the logins are kept.

`kernel forget` removes the record and the credentials of its logins.

`--server URL` sends one command to an address directly. It carries no login, so
it is only useful for unauthenticated routes.

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

The command takes the phrase printed when the account was created, proves to the
kernel that you hold it, and sets the new password. There is no email
anywhere in Juice, so the phrase is the only recovery route. An account whose
phrase is lost cannot be recovered.

## Suspension

An operator can suspend any account, reversibly. A suspended account is refused
at every authenticated request:

```
$ juice user me
error: account suspended
```

Nothing is deleted. Unsuspending restores the account with its balance and
history intact.
