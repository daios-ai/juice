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

```bash
git clone https://github.com/daios-ai/juice.git
cd juice
make build          # or: go build -o juice ./cmd/juice/
./juice serve acme --addr :4040
```

Requires Go 1.25+. `acme` is the kernel's nickname: what it calls itself on the network,
and the name of its directory. A kernel is created by its first boot, which asks for the
three things it can never revise afterwards:

```
First boot of kernel acme at /home/you/.juice/kernels/acme.
A new signing key is minted here; its nickname, its network and that key are fixed for the life of the kernel.
World — play, test, real, or a world file: play
Superuser password:
Confirm password:
sys recovery phrase (write this down; it is shown only once and cannot be recovered):
  bomb buffalo march shock slim obvious stairs time usage grace habit wear
Press Enter once you have written it down:
Superuser "sys" created.
INF server.ready handle=acme network=play addr=:4040 public_key=Kl8eObRJ…
```

**Write the phrase down**: it is the only way to reset the superuser password
(`juice auth recover sys`). Later boots read what that one wrote and are idempotent;
each one repeats the ready line, which is where the kernel says which nickname, which
network and which key answered.

To boot without a terminal, write the answers first and set the password in the
environment — no prompt then has anything to ask:

```bash
mkdir -p ~/.juice/kernels/acme
echo '{"world":"play"}' > ~/.juice/kernels/acme/config.json
JUICE_BOOTSTRAP_PASSWORD=… ./juice serve acme
```

A kernel's whole state lives in that one directory: the database (which holds the signing
key), `config.json`, and the rail's key and records. A second kernel is a second name, so
it is a sibling of the first rather than a second installation; there is no `--db` and no
`--config`. The `cache/` subdirectory is regenerable and safe to delete. A kernel made
before kernels were named moves itself into `kernels/<its name>/` on first boot.

The CLI is a pure client of the server. Under `$JUICE_HOME/client/` it keeps the kernels
it knows — each one's address, public key and network — and the *contexts* naming one
kernel and one login on it:

```bash
./juice use work --endpoint http://localhost:4040   # add and switch to a kernel
./juice user create alice                           # create an account on it
./juice auth login alice                            # bind this context to that account
./juice use                                         # list contexts
./juice use bot --kernel work                       # a second login on the same kernel
```

`juice use` dials the server and refuses one whose key or network is not what the
context recorded, so a command never reaches a kernel you did not mean. A login travels
only to the address its context recorded. `--server` sets the endpoint for one invocation
and carries no login. `--context` or `JUICE_CONTEXT` picks a context for one command
without switching the current one, which is how an agent or a script names the kernel it
works on.
`ecosystem-standard.md` describes the whole layout, including where agents, services and
the interface keep their own state.

## Accounts and credits

```bash
./juice user create alice        # prints alice's one-time recovery phrase
./juice user create bob
./juice auth login alice
./juice user me                  # handle, balance, locked funds
```

Credits enter only by operator deposit against a payment made outside the system, named
by the fact that witnesses it, and then move freely between local users:

```bash
./juice auth login sys
./juice admin deposit alice 1000 --ref wire-8823   # operator only
./juice auth login alice
./juice user transfer bob 250      # alice pays bob directly, no fee
./juice user ledger                # every deposit, withdrawal, and transfer
```

On `play` no crypto is involved at all: the operator records the payments they receive
and make, `--ref` is whatever names one in their own books, and amounts are whole credits.

On a world with a chain (`test`, `real`), money arrives and leaves over that chain, and
amounts are written the way that token is written — `1.50`, not `1500000`:

```bash
./juice user deposit               # where to send money, and whether you are registered
./juice user address 0xAbC...      # register a payout address, proving you control it
./juice user withdraw 100 --yes    # pays out to that address
```

Every command that moves money asks before it does, since none of them can be undone.
`--yes` answers in advance, which is how a script says it meant it.

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
kernel behind home NAT federates like any other. Joining is just booting with the
default `bootstrap_peers`. Serving is just marking an action `public`. Calling is just
naming it:

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
./juice admin users                    # all local accounts
./juice admin deposit carol 500 --ref wire-4471   # credit against a payment received
./juice admin deposit                  # payments held for a sender nobody has registered
./juice admin suspend carol            # one reversible lever, humans and kernels alike
./juice admin rename k-3f8a2c9d weather-farm # give a peer a memorable local name
./juice admin peers                    # counterparties and discovered kernels, last seen
./juice admin inspect <key|petname>    # a peer's identity, catalog, trade evidence, reachability
./juice admin identity                 # own key, addresses, rail position, money rules and credit
./juice step complete <id> --peer <key>  # complete a step a peer parked for this kernel
```

Every cross-kernel call is paid for on its own. A charge too small to be worth a rail
payment is settled by a ticket: it pays a fixed larger amount with the probability that
makes the average payment the charge, so a stream of small calls costs a handful of
payments rather than one apiece, and neither side can pick the outcome. Serving
strangers is bounded-risk by construction: one credit limit bounds all the work this
kernel has delivered and not been paid for, so minting identities buys an attacker
nothing. `admin identity` shows the position.

## Configuration

`config.json` sits next to the database, inside the kernel's own directory, and is written
once by first boot; nothing rewrites it afterwards. Safe defaults apply when a key is
absent, and a key that is not a key is a startup error rather than a silent default.
The ones you are most likely to touch:

| Key | Purpose |
|---|---|
| `kernel_handle` / `bootstrap_peers` | Federation identity and the peers dialed to join the network |
| `fed_listen_addrs` | Where this kernel answers peers; give each kernel its own when running more than one (as `--addr` does for clients) |
| `world` | The network this kernel serves for life: `play` (no crypto), `test`, `real`, or a path to a world file. There is no default: first boot asks, and the answer cannot be revised |
| `rail_rpc` | Endpoint of the chain the world names — required only for a world that has one |
| `fee_bps` | Kernel fee on each provider's margin (default `2000` = 20%) |
| `remote_bps` / `import_bps` | Markup for serving peers / import duty on remote calls (default `500` each) |
| `lottery` / `credit_limit` | The ticket a cross-kernel charge is settled by (`0` pays every charge exactly), and the ceiling on work delivered and unpaid |
| `native.*` | Stdlib prices and LLM URL/models (`native.llm`) |
| `allow_local_sources` | Permit private-network URLs for action sources (off by default; loopback always allowed) |
| `log_level` / `log_file` / `log_format` | Structured logging |

Environment variables are bootstrap overrides only: `JUICE_HOME`, `JUICE_SECRET_KEY`,
`JUICE_LOG_LEVEL`, `JUICE_CREDENTIALS_KEY`, `JUICE_BOOTSTRAP_PASSWORD`,
`JUICE_ALLOW_LOCAL_SOURCES`, and `JUICE_CONTEXT` for the client.

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
is real token transfers, credited at the world's settlement tag: `latest` on the shipped test world,
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
