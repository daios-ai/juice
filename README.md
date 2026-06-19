# Juice

Juice is a Go kernel and research platform for **callable actions**. An action is a named,
priced, owned unit of computation — an HTTP endpoint, a WebAssembly module, a native handler, or
a proxy to another kernel's action. Every call runs inside a funded process, is fully traced,
settles atomically, and produces a signed receipt.

Juice aims to be a kernel in the OS sense: a small set of general, robust primitives
(execution, accounting, tracing, settlement, federation), with everything else — including the
standard library of `@sys` actions — living in the application layer on top of those primitives.

## Core model

Execution starts with a single primitive:

```text
run(action, args)        // action is @owner/name
```

`run` atomically creates a process funded with exactly `action.price`, locked from the caller's
balance, creates a root trace from that process, and issues the root call. Every call — root
calls, WASM subcalls, step completions, OpenAPI HTTP actions, remote proxies — flows through one
dispatch primitive:

```text
Call(caller, trace, action, args)
```

The process closes automatically once the root call returns and no steps remain outstanding; you
never start or fund a process by hand.

**Money.** Every wallet — user, process, trace — has `available` and `locked`. `action.price` is
a *subtree bound*: the most the whole call tree can cost. A call's unspent allocation is its value
added and is paid to the action owner at settlement; the platform (`@sys`) takes `fee_bps`
(default `2000` = 20%). Failures refund the remaining allocation up the chain. Settlement is
final and atomic with its transaction and receipt.

## Features

- **Actions** — `http`, `wasm`, `native`, and `remote_proxy` kinds, with JSON Schema validation on
  inputs and outputs. Visibility is two booleans, `active` and `public`:
  `CanCall(P, a) = active(a) ∧ (public(a) ∨ P = a.owner)`. (No per-user ACLs.)
- **Funded processes & traces** — budgeted execution contexts; funds locked per call, settled on
  success, refunded on failure; a full trace tree per process.
- **Immutable transactions** with signed **Ed25519 receipts** and signed **ratings** (`0`/`1`).
- **Steps** — partially-applied future calls (`waiting → running → done | cancelled`) for
  human-in-the-loop and webhook completion.
- **Native `@sys` actions** (the platform stdlib): `lookup`, `llm/chat`, `llm/embed`, `llm/json`,
  `llm/decide`, `make`, `tinygo/compile`, `time`, `sink`, `message`, `random`.
- **Federation** — friend/unfriend peer kernels, proxy users, prepaid credits, signed manifests,
  and gossip-based discovery; `juice tx verify` checks a remote receipt locally.
- **OpenAPI import** — register representable HTTP operations as actions.
- **WASM via wazero** — sandboxed scripts with host functions `juice.call`, `juice.step_create`,
  `juice.step_complete`, and `juice.log`; no ambient filesystem, network, or token access.
- **Semantic lookup** — cosine search over action embeddings, re-ranked by stats.
- **SQLite** — single file, WAL mode, pure Go (no CGO); structured logging.

## Installation

```bash
git clone https://github.com/daios-ai/juice.git
cd juice
make build        # or: go build -o juice ./cmd/juice/
```

Requires Go 1.25+. Module path is `github.com/daios-ai/juice`.

On first boot the kernel prompts for a superuser password and atomically creates the `@sys` user,
its signing keypair, and a JWT secret, then registers the native `@sys` actions. Subsequent boots
are idempotent.

## Quick start

```bash
# Start the server (first run prompts for the @sys password)
./juice serve --addr :4040

# Create a user and log in (token stored under ~/.juice)
./juice user create @alice alice@example.com
./juice auth login @alice

# Register an action and activate it
./juice action create echo --kind http --source https://httpbin.org/post --price 0
./juice action enable @alice/echo

# Run it — creates a funded process, calls the action, closes the process
./juice run @alice/echo '{"msg":"hello"}'

# Inspect and rate the resulting transaction
./juice tx show <tx-id>
./juice tx rate <tx-id> 1

# Native actions work the same way
./juice run @sys/time
```

JSON arguments accept the `@file.json` convention (a leading `@` reads the value from a file), and
omitted args default to `{}`. Add `--json` for canonical machine-readable output (the HTTP shape)
or `--quiet` to print only a created resource's id.

## CLI

Every HTTP endpoint has a CLI command. Primary identifiers are positional natural keys — a user is
`@handle`, an action is `@owner/name` (an id is also accepted), and processes, steps, and
transactions are ids.

```text
juice serve | health
juice user create <user> <email> | me | update
juice auth login <user> | logout | refresh
juice action create <name> | update <action> | enable/disable <action> | list | show <action>
juice action delete <action> | import <spec-url> | unimport <spec-url> | stats <action>
juice run <action> [json]
juice process list | show <id> | end <id>
juice step create <action> | list | show <id> | complete <id> [json]
juice tx list | show <id> | rate <id> <0|1> | verify <id>
juice admin users | show | suspend | unsuspend | deposit | withdraw | actions | disable | processes | txs | steps
juice peer friend <url> | unfriend <user> | list | inspect <url>
```

Global flags: `--db <path>`, `--config <path>`, `--json`, `--quiet`. Admin and peer commands are
superuser-only. See **[API.md](API.md)** for the full command/flag and HTTP route reference.

## HTTP API

```bash
./juice serve --addr :4040
```

Most routes require `Authorization: Bearer <token>`. Public routes include `GET /health`,
`GET /.well-known/juice-kernel.json`, `GET /v1/actions`, and the federation endpoints
(`POST /v1/peers`, `POST /v1/federation/call`, `GET /v1/gossip`). The execution entry point is
`POST /v1/run`. The complete route table — with request/response shapes and the R1–R9 / C1–C12
design rules — lives in **[API.md](API.md)**.

## Configuration

Configuration is a `juice.json` file, auto-created next to the database (override with `--config`).
Key groups:

| Key | Purpose |
|---|---|
| `native.*` | Per-action prices plus LLM URL/models (`native.llm`) and `@sys/make` settings |
| `fee_bps` | Platform fee in basis points (default `2000` = 20%; recipient is fixed to `@sys`) |
| `import_bps` | Federation import duty in basis points (default `500`) |
| `token_ttl` | Access-token lifetime (e.g. `15m`) |
| `log_level` / `log_file` / `log_format` | Structured logging |
| `peer_auto_accept` / `peer_handle` | Federation friending behaviour |
| `allow_local_sources` / `allow_local_peer_urls` | Permit loopback/private URLs (off by default) |
| `credentials_key` | Auto-generated AES-256 key encrypting action upstream credentials |

Runtime-only environment overrides (never written to `juice.json`): `JUICE_DB_PATH`,
`JUICE_SECRET_KEY`, `JUICE_CREDENTIALS_KEY`, `JUICE_LOG_LEVEL`, `JUICE_LOG_FILE`,
`JUICE_OLLAMA_URL`, `JUICE_OLLAMA_CHAT_MODEL`, `JUICE_OLLAMA_EMBED_MODEL`, `JUICE_FEE_BPS`, and
related `JUICE_*` keys. The HTTP listen address is the `--addr` flag.

## Architecture

```text
cmd/juice/   CLI + HTTP server, config, bootstrap (wires everything together)
kernel/      Core types, interfaces, auth, call/settlement semantics, federation, OpenAPI import
store/       SQLite implementation of kernel.Store (migrations, WAL)
script/      WebAssembly execution via wazero
llm/         Language and embedding adapter (Ollama) for lookup, chat, json, decide
native/      Native @sys action handlers (lookup, llm/*, make, time, sink, message, random, tinygo)
log/         Structured logger (slog + tint, text + JSON)
```

`kernel/` imports no SQLite, wazero, Ollama, CLI, or HTTP code — those adapters are injected at
startup behind ordinary Go interfaces. Every source file has a corresponding `_test.go`, and
`go test ./...` requires no network access.

## Further reading

- **[API.md](API.md)** — authoritative HTTP/CLI contract and design rules.
- **[requirements.md](requirements.md)** — the kernel specification.
- **[flows/](flows/)** — runnable end-to-end shell flows (`flows_test.sh` drives the rest).
