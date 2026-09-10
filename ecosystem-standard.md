# Juice ecosystem standard

How the programs of a Juice installation — kernels, the interface, agents, services and the command
line — are laid out on a machine, how each names the kernel it works on, and how a credential is
released. `requirements.md` governs the kernel and wins on any conflict; this document governs what
sits beside it, and binds every juice-family program, not only this repository.

It invents nothing. Servers are named, each with its own data directory, as PostgreSQL
clusters are. Clients keep kernels, logins and the contexts joining them as separate records, as
kubeconfig does. Only the terms are ours.

## 1. Terms

- **Installation** — one machine's Juice state, rooted at `$JUICE_HOME` (default `~/.juice`). One
  per operating-system user. Several people on one machine are several installations.
- **Kernel** — one server: its database and signing key, world, rail key, configuration and lock,
  in one directory named for its nickname.
- **Principal** — an account on one kernel. `sys` is one principal per kernel.
- **Session** — one login of one principal, holding an access token and a refresh token.
- **Context** — a name joining one kernel to one session. What every program names.
- **Component** — a program in the installation: the interface, an agent, a service, the command
  line.

## 2. Layout

```
$JUICE_HOME/
  kernels/<name>/        one kernel: juice.db, config.json, the rail key and its records,
                         serve.lock, cache/
  client/config.json     the kernels this client knows, the contexts on them, and the current one
  client/credentials/    one file per context, 0600, holding that session's tokens
  agents/<name>/         one agent: its configuration, persona, knowledge, memory, conversations
  services/<name>/       one installed service: its executable state and private configuration
  ui/                    the interface's own state
```

A kernel's home is one directory because everything in it binds to everything else: the ledger, the
signing key its receipts are signed with, the rail key that settles them, and the world all three
are valid on. It backs up, moves and locks as a unit. `cache/` is regenerable and safe to delete.

Everything else belongs to the component that owns it and outlives any kernel. Removing a kernel
never removes an agent's memory, a service's state, or the interface's history.

## 3. Kernels

`juice serve <name>` serves `kernels/<name>/`. The name may hold letters, digits, dot, dash and
underscore, up to 64 characters, may not begin with a dot, and is bare — it is a nickname as well as
a directory.

It is the kernel's nickname (D15): what the kernel calls itself on the network, and, because it is
the one name the operator has given by the time a home is created, the name of that home. Neither is
the kernel's identity, which is its key and does not exist until first boot. The directory may be
renamed and the network will not notice; a nickname already written in the configuration is kept.

Each kernel is started with its own `--addr` and carries its own `fed_listen_addrs` in its own
`config.json`. Nothing allocates ports for them; a collision is a bind failure at startup, for the
client port and the peer transport alike. One server per kernel, enforced by the lock in its home.

A kernel is created by its first boot, which asks for what it cannot revise — its network, and the
chain endpoint where the network has one — and refuses off a terminal, naming the configuration key
that would have answered. That first boot is the only writer of `config.json`; every later boot reads
it. Nothing is created before the answers are in hand, and the network is recorded only after the
rail has verified it, so a boot that cannot be answered leaves no half-made kernel behind.

Removing a kernel is not a lifecycle verb. Its directory holds a ledger, a signing key, a rail
key and possibly unsettled obligations, so it is archived or destroyed deliberately by the operator,
never as a step in switching kernels.

## 4. Client records

`client/config.json`:

```json
{
  "current": "default",
  "kernels": {
    "default": {
      "endpoint": "http://localhost:4040",
      "public_key": "<43-char base64url Ed25519 key>",
      "world_digest": "…", "network": "play", "decimals": 0
    }
  },
  "contexts": {
    "default": { "kernel": "default", "handle": "alice", "principal_id": "<uuid>" }
  }
}
```

`client/credentials/<context>.json`, mode 0600: `{"token": "…", "refresh_token": "…"}`.

One kernel, many contexts. One principal may hold several contexts on one kernel, which is how the
interface, an agent and the command line authenticate as the same account without sharing a session.
Rotating one session's refresh token leaves the others untouched. Two processes on one context are
safe as well: the whole read-rotate-write runs under an exclusive lock on that credential file, and
the second to arrive uses what the first stored rather than spending a token that no longer exists.

Names are local aliases. Anything durable — an agent's memory of a kernel, a record of what was
bought where — is keyed by `(network digest, kernel public key, principal id)`, never by a context
or kernel name, and never by a handle, which can be renamed.

Commands:

| Command | Effect |
|---|---|
| `juice use` | list the contexts, marking the current one |
| `juice use NAME --endpoint URL` | add or repoint kernel NAME and a context on it, record its identity, and strand every login on that kernel |
| `juice use NAME --kernel K` | add context NAME as a second login on a kernel already known |
| `juice use NAME` | switch, refusing a server that is no longer the recorded kernel or network |
| `juice auth login USER` | bind the current context to that principal and store its session |
| `--context NAME`, `JUICE_CONTEXT=NAME` | address one context for one command, without switching |

`current` is a convenience for a person at a terminal. An unattended program — an agent, a service,
a scheduled job — names its context explicitly and never reads `current`, so `juice use` in a
terminal can never move an agent's spending onto another kernel. An interface selects a context per
session or window and does not follow `current` while open.

A component finds its kernel in `client/config.json` by context name and its tokens in
`client/credentials/<context>.json`. A refresh is read, exchange, write under an exclusive lock on
that file. `config.json` is written by temp file and rename. A session is obtained with the `juice`
command line or by writing those two files. Executables go to `$PREFIX/bin`, default `~/.local`.

## 5. Releasing a credential

The kernel listens on plain HTTP on its own machine, as Ollama, Jupyter, MySQL and PostgreSQL over
TCP do. A client sends a stored credential only to the address its context recorded, and `juice use`
refuses to switch to a server that no longer reports the recorded key or network. That is the whole
rule.

It is not a defence against another user account on the same machine taking the port before the
kernel does. A personal installation has no such user, and a machine shared with people the operator
does not trust is out of scope. Exposing a kernel beyond its own machine is done behind an ordinary
TLS front end, as with any web service.

Credentials belong to a session, never to an installation. Two components sharing a root share no
token.

A context name is one path segment, on the same rule as a kernel name, because it becomes a file
under `credentials/` and a name free to hold a separator would address something else.

## 6. Components

**Agents** are users. An agent holds its own principal on every kernel it works on, created and
funded like any other, and never the operator's. It names a context explicitly. Its persona,
knowledge, memory, conversations and tasks live under `agents/<name>/` and survive any kernel it
used; what it remembers about a kernel is keyed by the tuple in §4, so a stale memory is detectable
rather than silently wrong.

**Services** are ordinary HTTP servers, installed once under `services/<name>/` and listening on a
port of their own. Installation and registration are separate acts: a service is installed once, and
registered on each kernel that should sell it, by a provider principal naming a context. A service
holds no kernel credential. It composes using the capability and callback address carried on each
dispatched request, which is how one service can serve several kernels with nothing kernel-specific
in its configuration. Upstream credentials belong to the provider and are sealed on the action, not
held by the service.

Registration is by adapter. An OpenAPI document installed with `action import` is one adapter, and
gets identity-preserving reconciliation across upgrades. A service that is not representable that
way is registered by ordinary action creation. Neither is a requirement of being a service.

**The interface** is a client like any other. It reads the same contexts, shows which one is in use,
and checks `/health` before it trusts. A port number is a default, never an identity: an interface
that dials a port without checking will talk to whichever kernel holds it.

## 7. Migration

A kernel made before kernels were named, at `$JUICE_HOME/kernel/`, moves to `kernels/<name>/` on the
first boot after the upgrade. It is one rename: both paths are under one root, so the move either
happened or it did not, and an interrupted boot leaves no half-moved ledger. It refuses, rather than
merging, when a server still holds the old home or when the destination already exists. Running
again finds nothing to move.

A `client/profiles.json` written before contexts existed becomes `client/config.json` plus one
credential file per profile, on the client's first run. The old file is kept as
`profiles.json.migrated`. An existing `config.json` is never overwritten.

The kernel and the command line migrate themselves; nothing here moves another component's files.
An interface, agent or service still keeping its own address or its own token moves to the layout
and the records above in its own repository, and until it does it is talking to whatever answers a
port.
