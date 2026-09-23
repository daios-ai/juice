<p align="center">
  <img src="docs/juice-logo.svg" alt="Juice logo" width="110" />
</p>

# Juice

Juice is a kernel for **callable actions**: named, priced, owned units of service. An
action can be an HTTP endpoint, a WebAssembly module, a built-in, or another kernel's
action reached over federation. Anyone can publish one, set a price, and get paid per
use; anyone can find one, run it, and rate the result. Every call settles atomically
and leaves a signed receipt, so both sides can always prove what happened and what it
cost.

One number to keep in mind: an action's **price is the whole cost**. Whatever the
action does internally — call other paid actions, wait for a human, reach across the
network — you are never charged more than the price you saw.

## Install and boot

The ordinary way in is [juiceos.org](https://juiceos.org/download/), which is one command:

```bash
curl -fsSL https://juiceos.org/install.sh | sh
```

That puts `juice` in `~/.juice/bin` and adds it to your PATH. Options go after `sh -s --`,
since a piped script gets no arguments of its own:

```bash
curl -fsSL https://juiceos.org/install.sh | sh -s -- VERSION
curl -fsSL https://juiceos.org/install.sh | sh -s -- --no-modify-path
```

A release tag pins a version, and `--no-modify-path` leaves your shell profile alone.
`JUICE_HOME` moves the whole installation, binary included. To remove it, delete the binary;
the kernels beside it are untouched.

Or build it yourself, which needs Go 1.25 or later:

```bash
git clone https://github.com/daios-ai/juice.git
cd juice
make build          # or: go build -o juice ./cmd/juice/
make install        # copies it to ~/.juice/bin, where install.sh puts it
```

Either way, the first kernel is one command, naming the network to serve:

```bash
juice kernel serve play --listen-addr :4040
```

A **world** is a network: the money it uses and the servers to meet it through, written in
a file. Four are shipped and written into `~/.juice/worlds/` the first time you serve, to
read and to edit:

| World | Money |
|---|---|
| `play` | no real money: you credit accounts yourself and keep the records |
| `arbitrum-sepolia` | fake USDT0 on the Arbitrum Sepolia test chain |
| `arbitrum-one` | USDT0 on Arbitrum One |
| `polygon` | USDT0 on Polygon |

One installation runs one kernel per world, in `~/.juice/kernels/<world>/`. There is no
kernel on `play` yet, so `serve` says what is here, asks whether to create one, and asks
what it can never revise:

```
There is no kernel on play here. No kernels here yet.
play is no real money: you credit accounts yourself and keep the records.
Create a kernel on play? [y/N] y

What will this kernel call itself on the network? Other operators see this name.
Name: acme
Superuser password:
Confirm password:
sys recovery phrase (write this down; it is shown only once and cannot be recovered):
  bomb buffalo march shock slim obvious stairs time usage grace habit window
Press Enter once you have written it down:
Superuser "sys" created.
INF action.registered_native name=lookup
INF action.native_enabled name=lookup
…
INF client.self_registered kernel=acme outcome=added
INF server.ready handle=acme network=play addr=[::]:4040 public_key=Kl8eObRJ…
```

Declining, or interrupting before those answers, leaves nothing behind.

**Write the phrase down**: it is the only way to reset the superuser password
(`juice auth recover sys`). Later boots ask nothing at all — the network is recorded in
the kernel's own database, and a kernel offered another one refuses — and print only the
ready line, which is where the kernel says which nickname, which network and which key
answered. The pair of lines per standard action belongs to the boot that installs them.

To boot without a terminal, name the kernel on the command line and set the password in
the environment. Saying what the kernel is, is the consent a machine with no terminal can
give, so `serve` creates it without asking:

```bash
JUICE_BOOTSTRAP_PASSWORD=… ./juice kernel serve play --kernel-handle acme
```

To join a network juice does not ship, put its file in `~/.juice/worlds/` and serve it by
that file's name. Writing one of your own gives an economy of its own: its money is its
own, and nothing signed on it verifies anywhere else.

Every setting of `config.json` is an option here — the key with underscores written as
dashes, a nested key as a path (`--native.llm.url`) — and an option applies to that run
only, except on a first boot, which writes what you give it as the new kernel's
configuration. Writing the file yourself first does the same thing.

A kernel's whole state lives in that one directory: the database (which holds the signing
key), `config.json`, and the rail's key and records. A second kernel is a second name, so
it is a sibling of the first rather than a second installation; there is no `--db` and no
`--config`. The `cache/` subdirectory is regenerable and safe to delete. A kernel made
before kernels were named moves itself into `kernels/<its name>/` on first boot.

Every command is `juice [admin] <noun> <verb>`, with `juice run` the one exception. The
CLI is a pure client of the server: under `$JUICE_HOME/client/` it keeps the kernels it
knows — each one's address, public key and network — and one file per *login*, written
`handle@kernel`, which says both who a command acts as and which kernel it acts through.

A kernel you serve yourself is registered by `serve` as it starts, under the name it
serves as, so nothing has to be copied from its log. A kernel somebody else runs is
registered once, by address:

```bash
./juice kernel add https://their.example work   # register somebody else's under "work"
./juice user create alice@work                  # create an account on it
./juice auth login alice@work                   # log in, and act as alice@work
./juice kernel list                             # the kernels known, and which is in use
./juice auth list                               # the logins held, and which is in use
./juice auth use bot@work                       # switch to another login already held
```

Registering dials the server and records the key and network it presents; a key is one
record, so registering a kernel already known under a new name renames it and keeps its
logins, and a name another kernel holds is refused; logging in and
switching refuse a server that no longer presents them, so a command never reaches a
kernel you did not mean, and a login travels only to the address recorded for its kernel.
`--server` sets the endpoint for one invocation and carries no login. `--as alice@work`
or `JUICE_AS` names a login for one command without switching, which is how an agent or a
script says who it is; a name that is not a login here is refused rather than replaced by
whoever happens to be logged in.
`ecosystem-standard.md` describes the whole layout, including where agents, services and
the interface keep their own state.

## Accounts and credits

```bash
./juice user create alice@work   # prints alice's one-time recovery phrase
./juice user create bob@work
./juice auth login alice@work
./juice user me                  # handle, balance, locked funds
```

Credits enter only by operator deposit against a payment made outside the system, named
by the fact that witnesses it, and then move freely between local users:

```bash
./juice auth login sys@work
./juice admin user deposit alice 1000 --ref wire-8823   # operator only
./juice auth use alice@work
./juice user transfer bob 250      # alice pays bob directly, no fee
./juice user ledger                # deposits, withdrawals, transfers, and settlement postings
```

The ledger also shows provider payouts, operator fees, and import fees, each linked
to the transaction that settled it.

On `play` amounts are shown in fUSD (fake dollars), with six decimal places like
the other shipped worlds. No crypto is involved. The operator records the payments they
receive from people; `--ref` is whatever names one in their own books. What
another kernel owes needs no such record: its own signed message saying it paid
is the payment here, so those debts close by themselves. `play` money is backed
by nothing and is meant for trying the system out.

On a world with a chain (`arbitrum-sepolia`, `arbitrum-one`), money arrives and leaves over it, and
amounts are written the way that token is written — `1.50`, not `1500000`:

```bash
./juice user deposit               # where to send money, and whether you are registered
./juice user address 0xAbC...      # register a payout address, proving you control it
./juice user withdraw 100 --yes    # pays out to that address
./juice user withdrawals          # the ones you have made, and where each stands
```

`user transfer` and `user withdraw` ask before they act, since neither can be undone.
`--yes` answers in advance, which is how a script says it meant it. Running a
value-bearing action does not ask: the run is itself the authorisation for the value
its arguments name.

If you lose your password, `juice auth recover <user>` restores the account from the
recovery phrase. There is no email anywhere in the system.

## Publish and run an action

An action needs a name, a description, input/output schemas, and a price. It starts
disabled and private, so nothing is callable by accident:

```bash
./juice action create echo --kind http --source https://httpbin.org/post --price 5 \
  --description "Echo a message" \
  --input-schema '{"type":"object","properties":{"msg":{"type":"string","description":"text to echo"}}}'
./juice action enable alice/echo
./juice run alice/echo '{"msg":"hello"}'
```

`run` debits exactly the price, executes, and settles. Invalid input is rejected
before you are charged; a failed call refunds what was not consumed. Inspect and rate
the result:

```bash
./juice tx show <tx-id>
./juice tx rate <tx-id> 1 --note "did what it said"
./juice action ratings alice/echo    # public track record: value, note, date
./juice action stats alice/echo      # uses, successes, latency
```

Only the payer can rate, once, and ratings are immutable — they are the market's
public evidence about the action.

Three visibility levels control the audience, each widened deliberately by the owner
(`action update alice/echo --visibility public`): `private` (owner only), `local`
(users of this kernel), `public` (everyone, including other kernels). Changing an action's terms never surprises a buyer: a call pinned to
terms that changed is refused and re-quoted, never silently repriced.

As a provider, the price is your bound and your margin: sub-actions you call are paid
from your budget, and what you don't spend is yours at settlement, minus the kernel's
fee on that margin. JSON arguments accept `@file.json`; `--json` gives machine-readable
output; `--quiet` prints only ids.

## Compose actions

A WASM action can call other actions within its own advertised price, using the host
functions `juice.call`, `juice.step_create`, `juice.step_complete`, and `juice.log` —
no filesystem, network, or token access. Write the handler in Go and compile it on the
kernel:

```bash
./juice run sys/tinygo/compile "$(jq -Rs '{source: .}' handler.go)" --json \
  | jq -r .result.artifact | base64 -d > pipeline.wasm
./juice action create pipeline --kind wasm --artifact pipeline.wasm --price 100
```

An HTTP-backed action can compose too: each dispatch carries a capability header the
endpoint presents back to `POST /v1/call` to make sub-calls under the same budget. The
caller still sees one price, one result, one party to rate.

## Wrap an existing web API

Install a whole OpenAPI document as one application — one action per operation, rooted
at `<name>/index`:

```bash
./juice action import weather https://api.example.com/openapi.json
./juice action enable alice/weather      # enables the whole subtree
./juice run alice/weather/forecast '{"city":"Lisbon"}'
```

Re-running the import reconciles a changed document without losing history or the
terms you set; `action disable alice/weather` and `action delete alice/weather`
switch off or remove the application with the ordinary verbs.

Upstream credentials attach per action and never surface anywhere — not in inputs,
outputs, logs, or receipts. Owner-held schemes (`header`, `query`, `bearer`, `basic`,
OAuth client-credentials, JWT-bearer) serve APIs where the owner holds one key. For
multi-user APIs, each caller connects their **own** upstream account once:

```bash
./juice user connect alice/mail            # browser consent (OAuth), or:
./juice user connect alice/mail --token KEY   # or a personal API key
./juice user me                            # lists connections, never tokens
./juice user disconnect alice/mail
```

One consent covers the whole directory of actions it names. A call that needs a
missing consent is refused before any money moves, telling the client exactly which
consent to request. See [docs/oauth.md](docs/oauth.md).

## Wait for a human or a webhook

An action can park a **step**: a prepaid continuation addressed to one named party.
The money is already reserved, so completing it needs no further funds:

```bash
./juice run sys/message '{"to":"bob","message":"approve the order?"}'
./juice step list                    # bob sees work addressed to him
./juice step complete <step-id> '{}'
./juice process list                 # a parked step keeps its process open
./juice process end <process-id>     # owner force-closes; parked funds return
```

External systems integrate the same way — they register as ordinary users and either
call `run` or complete a step pre-created for them. There is no separate webhook
machinery. Suspended work survives restarts.

## Search, and letting agents choose

```bash
./juice run sys/lookup '{"query":"translate text to german"}'
```

Lookup searches descriptions lexically and semantically and returns ranked candidates
with schemas and all-in prices — including actions discovered on other kernels. With a
local LLM configured (Ollama; `native.llm` in config), `sys/llm/decide` picks one
action from typed candidates and proposes valid arguments **without executing
anything**, so planning and spending stay separate decisions. The rest of the stdlib
(`sys/time`, `sys/random`, `sys/web`, `sys/llm/chat`, `sys/llm/embed`,
`sys/llm/json`, `sys/transfer`, `sys/sink`) works like any other action: `juice run
sys/time`.

## Federation

Kernels reach each other by public key over libp2p — no URLs, no port forwarding; a
kernel behind home NAT federates like any other. It joins through the seeds in its
world file, which is the one place a meeting point is named: to use another, edit
that file. `arbitrum-one` names this project's seed; on the others, operators write in one.
Serving is just marking an action
`public`. Calling is just naming it:

```bash
./juice run 'bob@<kernel-key-or-petname>/summarize' '{"text":"..."}'
./juice tx verify <tx-id>     # check the peer's signed receipt, offline
```

You pay from your **local** balance at the advertised all-in price — no remote
account, no prefunding. The first call resolves and caches the action; a changed
remote contract is refused and re-resolved, never silently repaid. If the peer is
unreachable the call either fails fast with a full refund or stays visibly parked
until its signed receipt arrives — never double-charged, never silently dropped.

## Operating a kernel

The operator (`sys`) uses the same commands as users, widened in scope, plus the money
and trust verbs:

```bash
./juice admin user list                # all local accounts
./juice admin user deposit carol 500 --ref wire-4471  # credit against a payment received
./juice admin kernel deposits          # payments held for a sender nobody has registered
./juice admin user suspend carol       # one reversible lever, humans and kernels alike
./juice admin peer rename k-3f8a2c9d weather-farm # give a peer a memorable local name
./juice admin peer list                # counterparties and discovered kernels, last seen
./juice admin peer inspect <key|petname>  # identity, catalog, trade evidence, reachability
./juice admin kernel show              # own key, addresses, rail position, money rules and credit
./juice step complete <id> --peer <key>  # complete a step a peer parked for this kernel
```

Every cross-kernel call is paid for on its own. A charge too small to be worth a rail
payment is settled by a ticket: it pays a fixed larger amount with the probability that
makes the average payment the charge, so a stream of small calls costs a handful of
payments rather than one apiece, and neither side can pick the outcome. Serving
strangers is bounded-risk by construction: one credit limit bounds all the work this
kernel has delivered and not been paid for, so minting identities buys an attacker
nothing. `admin kernel show` shows the position.

## Configuration

`config.json` sits next to the database, inside the kernel's own directory, and is written
once by first boot; nothing rewrites it afterwards. What belongs to the network rather than
to this kernel — its money, its chain endpoint, its seeds — is in the world file instead. Safe defaults apply when a key is
absent, and a key that is not a key is a startup error rather than a silent default.
The ones you are most likely to touch:

| Key | Purpose |
|---|---|
| `kernel_handle` | The nickname this kernel reports |
| `listen_addr` | Where this kernel answers clients (default `:4040`) |
| `fed_listen_addrs` | Where this kernel answers peers; empty binds the standard port 31313, and a second kernel on the same machine needs its own |
| `fee_bps` | Kernel fee on each provider's margin (default `2000` = 20%) |
| `remote_bps` / `import_bps` | Markup for serving peers / import duty on remote calls (default `500` each) |
| `lottery` / `lottery_max` / `credit_limit` | The ticket this kernel settles a cross-kernel charge by (`0` pays every charge exactly), the largest ticket it accepts from a buyer, and the ceiling on work delivered and unpaid |
| `native.*` | Stdlib prices and LLM URL/models (`native.llm`) |
| `allow_local_sources` | Permit private-network URLs for action sources (off by default; loopback always allowed) |
| `log_level` / `log_file` / `log_format` | Structured logging |

Environment variables are bootstrap overrides only: `JUICE_HOME`, `JUICE_SECRET_KEY`,
`JUICE_LOG_LEVEL`, `JUICE_CREDENTIALS_KEY`, `JUICE_BOOTSTRAP_PASSWORD`,
`JUICE_ALLOW_LOCAL_SOURCES`, and `JUICE_AS` for the client.

## HTTP API

Every command above is a thin client of the HTTP API: most routes take
`Authorization: Bearer <token>`; `GET /health` and `GET /v1/actions` are public;
`POST /v1/run` is the execution entry point. Federation has no HTTP surface — peer
traffic travels over libp2p only. The full route table lives in [API.md](API.md).

## Repository layout

```text
cmd/juice/   CLI + HTTP server, config, bootstrap
kernel/      Core objects and operational semantics
store/       SQLite persistence (migrations, WAL)
fed/         Federation transport (libp2p)
script/      WebAssembly execution (wazero)
llm/         Language/embedding adapter (Ollama)
native/      The sys stdlib actions
log/         Structured logging
```

## Further reading

- [requirements.md](requirements.md) — the kernel specification (authoritative).
- [API.md](API.md) — the full HTTP/CLI reference.
- [docs/worlds.md](docs/worlds.md) — what a world is, and how to write one of your own.
- [docs/oauth.md](docs/oauth.md) — wrapping APIs that need per-user consent.
- [flows/](flows/) — runnable end-to-end shell flows (`flows_test.sh` drives them).

## Testing

Three cadences, by what each costs and what it answers.

| | command | when |
|---|---|---|
| Unit, including the in-process federation simulator | `go test ./...` | every change |
| End-to-end flows against the real binary | `JUICE=./juice bash flows/flows_test.sh` | before merging |
| Network simulation: a five-kernel economy, with a report | `make netsim` (or `go run ./netsim`) | now and then, and after anything touching federation or money |

The first two are gates: they fail a change. `make netsim` drives a whole economy across five
kernels and writes what happened to `netsim-runs/<rail>-<timestamp>/`: every command and its output
in `log.jsonl`, a full snapshot of each kernel in `checkpoints/`, and `report.md`. The report checks
money in against money held, every call's charge against its advertised terms, every payment against
the debt it moved, and reports latency, throughput and recovery. It exits non-zero when a check
fails. **[docs/network-simulation.md](docs/network-simulation.md)** describes the economy it builds,
what each measurement proves, and what it does not claim.
`RAIL=anvil` runs the same economy against a local chain (needs Foundry) and `RAIL=sepolia` against
the live testnet (needs `JUICE_SEPOLIA_RPC` and `JUICE_SEPOLIA_KEY_FILE`, mode 600); on both, money
is real token transfers, credited at the world's settlement tag: `latest` on `arbitrum-sepolia`,
so a credit lands in seconds; `finalized` waits for Ethereum.

It is one economy on all three. The participants, actions, prices, trades, compositions, attacks
and assertions are fixed in `netsim/story.go` and run unchanged everywhere; a rail supplies only
how money enters, how a payment is made and becomes final, and what the run cost. A test fails if
the story so much as names a rail. The one thing a rail chooses is how often the trading rounds
repeat, because a live testnet charges for each round in gas and in a quarter of an hour of
finality; every distinct event still happens at least once, and the report says which count it ran.
Before spending anything, a rail prices what the story will ask of it and refuses if it does not
fit, rather than running a cheaper economy under the same name.

A run directory is git-ignored, and **it is not safe to hand to anyone**. Alongside the logs — which
are redacted as they are written — it contains each kernel's whole home: its database, its signing
key, its rail key and its issued tokens. Read it in place; publish `report.md` and `metrics.json`
if you need to share something, and delete the directory when you are done with it.

Two release gates are opt-in and need more than a laptop: `JUICE_RAIL_FLOWS=1` for the local-chain
rail gate, and `JUICE_NETWORK_FLOWS=1` for the real-NAT federation gate, which needs a second host
behind a different NAT.

## License

Copyright (C) 2026 Pedro A. Ortega <pedro.ortega@gmail.com>

Juice is free software: you can redistribute it and/or modify it under the terms of the GNU
Affero General Public License as published by the Free Software Foundation, version 3 of the
License only (`AGPL-3.0-only`). It is distributed WITHOUT ANY WARRANTY; see `LICENSE` for the
full text. Every Go file carries the matching `SPDX-License-Identifier` line.

The one exception is `script/sdk.tmpl`, the source prepended to every action compiled by
`sys/tinygo/compile`: it is licensed under the Apache License, Version 2.0 (`Apache-2.0`, text in
`script/LICENSE-APACHE`), so that the compiled actions it becomes part of remain their authors'
own work. Programs that talk to a kernel over HTTP or the federation transport are separate
works and are not affected by the kernel's license.
