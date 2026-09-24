# Juice ecosystem standard

How the programs of a Juice installation — kernels, the interface, agents, services and the command
line — are laid out on a machine, how each names the kernel it works on, and how a credential is
released. `requirements.md` governs the kernel and wins on any conflict; this document governs what
sits beside it, and binds every juice-family program, not only this repository.

It invents nothing. Servers are named, each with its own data directory, as PostgreSQL
clusters are. Clients keep the kernels they know apart from the logins they hold, as kubeconfig
keeps clusters apart from users. Only the terms are ours.

## 1. Terms

- **Installation** — one machine's Juice state, rooted at `$JUICE_HOME` (default `~/.juice`). One
  per operating-system user. Several people on one machine are several installations.
- **Kernel** — one server: its database and signing key, rail key, configuration and lock, in one
  directory named for the world it serves. One installation runs one kernel per world.
- **Account** — a principal on one kernel. `sys` is one account per kernel.
- **Login** — one account at one kernel, written `handle@kernel`, holding that session's access and
  refresh tokens. What every program names, and what says both who a command acts as and where.
- **Component** — a program in the installation: the interface, an agent, a service, the command
  line.

## 2. Layout

```
$JUICE_HOME/
  bin/                   the family's executables, where the installer puts them
  worlds/<world>.json    one network's definition: its money, and the servers to meet it through
  kernels/<world>/       one kernel: juice.db, config.json, the rail key and its records,
                         serve.lock, cache/
  client/config.json     the kernels this client knows, and which login is selected
  client/credentials/    one file per login, named handle@kernel, 0600, holding its tokens
  agents/<name>/         one agent: its configuration, persona, knowledge, memory, conversations
  services/<name>/       one installed service: its executable state and private configuration
  ui/                    the interface's own state
```

A kernel's home is one directory because everything in it binds to everything else: the ledger, the
signing key its receipts are signed with, and the rail key that settles them. The world those three
are valid on is not in it — it is the network's, shared by every kernel on it and held once per
installation in `worlds/`, which is why the home is named for it. The home backs up, moves and locks
as a unit; `cache/` is regenerable and safe to delete.

Everything else belongs to the component that owns it and outlives any kernel. Removing a kernel
never removes an agent's memory, a service's state, or the interface's history. `bin/` is the one
directory holding no state at all: the programs are replaceable, so an installer overwrites them
and a person deletes them without losing anything.

## 3. Kernels

`juice kernel serve <world>` serves `kernels/<world>/`. The world is a file in `worlds/`, named by
that file: the name may hold letters, digits, dot, dash and underscore, up to 64 characters, and may
not begin with a dot. The worlds this installation's binary ships are written into `worlds/` the
first time it serves and never overwritten, so an operator's edit — their own node, their own
meeting point — outlives an upgrade, and a network this build does not ship is a file they add.

One installation runs one kernel per world, so the world is the whole of what `serve` is told. The
kernel's own nickname (D15) — what it calls itself on the network, since every kernel on one shares
the world's name — is asked for at first boot and kept in its `config.json`. Neither is the kernel's
identity, which is its key and does not exist until that boot.

Each kernel is started with its own `--listen-addr` and carries its own `fed_listen_addrs` in its own
`config.json`. Nothing allocates ports for them; a collision is a bind failure at startup, for the
client port and the peer transport alike. One server per kernel, enforced by the lock in its home.

A kernel is created by its first boot, and only on the operator's word: a `config.json` written in
advance, or an answer given at a terminal after they are told which kernels are here. It then asks
for what the kernel cannot revise — the name it goes by on the network — and refuses off a terminal,
naming the key and the file that would have answered. That first boot is the only writer of
`config.json`, and it writes nothing until the answers are in hand, so a boot that is declined or
unanswered leaves nothing behind. The network is recorded once the rail has verified it, and from
then on a boot offering that kernel another world is refused before anything is opened.

Removing a kernel is not a lifecycle verb. Its directory holds a ledger, a signing key, a rail
key and possibly unsettled obligations, so it is archived or destroyed deliberately by the operator,
never as a step in switching kernels.

## 4. Client records

`client/config.json`:

```json
{
  "current": "alice@work",
  "kernels": {
    "work": {
      "endpoint": "http://localhost:4040",
      "public_key": "<43-char base64url Ed25519 key>",
      "world_fingerprint": "…", "network": "play", "decimals": 0
    }
  }
}
```

`client/credentials/alice@work.json`, mode 0600:
`{"token": "…", "refresh_token": "…", "principal_id": "<uuid>"}`.

One kernel, many logins. Each account holds its own file, so an agent and a person working on one
kernel never share a session and rotating one leaves the others untouched. Two processes on one
login are safe as well: the whole read-rotate-write runs under an exclusive lock on that credential
file, and the second to arrive uses what the first stored rather than spending a token that no
longer exists. Two programs logged in as the *same* account on one kernel share that one session,
which is what "logged out" means: ending it ends it for both.

The file name is a label. A handle can be renamed, and its old name taken by someone else, so the
account is recorded inside as `principal_id`, and it is the token — never the name — that
authenticates. Anything durable an agent remembers is keyed by
`(network fingerprint, kernel public key, principal id)`, never by a login or kernel name, and never by
a handle.

Commands:

| Command | Effect |
|---|---|
| `juice kernel add URL [NAME]` | register the kernel answering there, under its advertised nickname unless NAME is given; selects nothing. The same key on the same network at a new address is that kernel having moved: the address is updated and its logins are kept; any other answer is refused |
| `juice kernel list` | the kernels known, marking the one in use |
| `juice kernel forget NAME` | drop the record and those logins' credentials |
| `juice auth login USER@KERNEL` | authenticate there, and act as that login |
| `juice auth use USER@KERNEL` | switch to a login already held, verifying the kernel first |
| `juice auth list` | the logins held, marking the one in use |
| `juice auth logout [USER@KERNEL]` | end it; if it was in use, nothing is in use afterwards |
| `--as USER@KERNEL`, `JUICE_AS=USER@KERNEL` | act as one login for one command, without switching |

`current` is a convenience for a person at a terminal. An unattended program — an agent, a service,
a scheduled job — names its login explicitly and never reads `current`, so `juice auth use` in a
terminal can never move an agent's spending onto another kernel. An interface selects a login per
session or window and does not follow `current` while open. A selector naming no login here is an
error rather than a fallback: a misspelled `--as` must not act as somebody else.

A component finds its kernel in `client/config.json` by the kernel half of its login, and its
tokens in `client/credentials/<login>.json`. A refresh is read, exchange, write under an exclusive
lock on that file. `config.json` is written by temp file and rename. A session is obtained with the
`juice` command line or by writing those two files. Executables go to `$JUICE_HOME/bin`, which is
where the installer puts them and where a component looks for a sibling program.

## 5. Releasing a credential

The kernel listens on plain HTTP on its own machine, as Ollama, Jupyter, MySQL and PostgreSQL over
TCP do. A client sends a stored credential only to the address recorded for that login's kernel, and
`juice auth login` and `juice auth use` refuse a server that no longer reports the recorded key or
network. That is the whole rule.

It is not a defence against another user account on the same machine taking the port before the
kernel does. A personal installation has no such user, and a machine shared with people the operator
does not trust is out of scope. Exposing a kernel beyond its own machine is done behind an ordinary
TLS front end, as with any web service.

Credentials belong to a session, never to an installation. Two components sharing a root share no
token.

A login is two ordinary local names with an `@` between them, because it becomes a file under
`credentials/` and a name free to hold a separator would address something else.

## 6. Components

**Agents** are users. An agent holds its own principal on every kernel it works on, created and
funded like any other, and never the operator's. It names its login explicitly. Its persona,
knowledge, memory, conversations and tasks live under `agents/<name>/` and survive any kernel it
used; what it remembers about a kernel is keyed by the tuple in §4, so a stale memory is detectable
rather than silently wrong.

**Services** are ordinary HTTP servers, installed once under `services/<name>/` and listening on a
port of their own. Installation and registration are separate acts: a service is installed once, and
registered on each kernel that should sell it, by a provider naming its login. A service
holds no kernel credential. It composes using the capability and callback address carried on each
dispatched request, which is how one service can serve several kernels with nothing kernel-specific
in its configuration. Upstream credentials belong to the provider and are sealed on the action, not
held by the service.

Registration is by adapter. An OpenAPI document installed with `action import` is one adapter, and
gets identity-preserving reconciliation across upgrades. A service that is not representable that
way is registered by ordinary action creation. Neither is a requirement of being a service.

**The interface** is a client like any other. It reads the same logins, shows which one is in use,
and checks `/health` before it trusts. A port number is a default, never an identity: an interface
that dials a port without checking will talk to whichever kernel holds it.

## 7. Migration

A kernel made before kernels were named, at `$JUICE_HOME/kernel/`, moves to `kernels/<name>/` on the
first boot after the upgrade. It is one rename: both paths are under one root, so the move either
happened or it did not, and an interrupted boot leaves no half-moved ledger. It refuses, rather than
merging, when a server still holds the old home or when the destination already exists. Running
again finds nothing to move.

A client written before logins converts once, on its first run. A context that recorded which
account it held becomes that login and keeps its session; one that did not holds tokens nobody can
name, so its file is kept aside with `.unmigrated` rather than guessed at or deleted; and two
contexts that would become one login never merge — the selected one keeps the name. A
`client/profiles.json`, older still, yields its kernels and their pinned keys the same way, and is
kept as `profiles.json.migrated`. An existing `config.json` is never overwritten.

The kernel and the command line migrate themselves; nothing here moves another component's files.
An interface, agent or service still keeping its own address or its own token moves to the layout
and the records above in its own repository, and until it does it is talking to whatever answers a
port.
