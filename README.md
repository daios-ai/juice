<p align="center">
  <img src="docs/juice-logo.svg" alt="Juice logo" width="110" />
</p>

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
- **Upstream auth** — per-action, sealed at rest and never returned: static (`header`, `query`,
  `bearer`, `basic`), owner-held OAuth (`oauth_client_credentials`, `oauth_jwt_bearer`), and two
  per-caller **delegated** schemes for wrapping multi-user APIs, where each caller connects their
  own account once and the action then calls the upstream as them — `oauth_delegated` (browser
  consent, `juice user connect`) and `delegated_bearer` (a pasted API key / personal access token,
  `juice user connect --token`, applied into a configurable header). Reads expose only the
  non-secret `auth_scheme` name and a `requires_grant` flag, never config or secrets. See
  **[docs/oauth.md](docs/oauth.md)**.
- **Native `@sys` actions** (the platform stdlib): `lookup`, `llm/chat`, `llm/embed`, `llm/json`,
  `llm/decide`, `tinygo/compile`, `time`, `sink`, `message`, `random`, `web`.
- **Federation** — friend/unfriend peer kernels by public key, proxy users, prepaid credits, signed
  manifests, and gossip-based discovery; `juice tx verify` checks a remote receipt locally.
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

State — the database (which holds the signing key), config, and auth tokens — lives under
`$JUICE_HOME/kernel/` (default `~/.juice/kernel/`); the binary is separate, on your `PATH`.
Set `JUICE_HOME` to relocate the whole juice suite, or point `--db` elsewhere to run several
kernels, or use `--db ./juice.db` for a portable per-folder kernel. The `kernel/cache/`
subdirectory holds regenerable data and is safe to delete.

## Quick start

```bash
# Start the server (first run prompts for the @sys password)
./juice serve --addr :4040

# Create a user and log in (token stored under $JUICE_HOME/kernel/)
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
juice user create <user> <email> | me | update | connect <action> | disconnect <action>
juice auth login <user> | logout | refresh
juice action create <name> | update <action> | enable/disable <action> | list | show <action>
juice action delete <action> | import <spec-url> | unimport <spec-url> | stats <action>
juice run <action> [json]
juice process list | show <id> | end <id>
juice step create <action> | list | show <id> | complete <id> [json]
juice tx list | show <id> | rate <id> <0|1> | verify <id>
juice admin users | show | suspend | unsuspend | rename | deposit | withdraw
juice admin friend <key> | unfriend <user> | peers | inspect <key|handle> | identity
```

Global flags: `--db <path>`, `--config <path>`, `--json`, `--quiet`. `admin` commands (including
federation trust) are superuser-only. See **[API.md](API.md)** for the full command/flag and HTTP
route reference.

## HTTP API

```bash
./juice serve --addr :4040
```

Most routes require `Authorization: Bearer <token>`. Public routes are `GET /health` (also an
identity banner: handle + public key) and `GET /v1/actions`. The execution entry point is
`POST /v1/run`. Federation has **no HTTP surface** — peers, manifests, gossip, the friend
handshake, and inbound calls travel over the libp2p transport (§13), not this API. The complete
route table — with request/response shapes and the R1–R9 / C1–C12 design rules — lives in
**[API.md](API.md)**.

## Configuration

Configuration is a `config.json` file, auto-created next to the database (override with `--config`).
Key groups:

| Key | Purpose |
|---|---|
| `native.*` | Per-action prices plus LLM URL/models (`native.llm`) |
| `fee_bps` | Platform fee in basis points (default `2000` = 20%; recipient is fixed to `@sys`) |
| `import_bps` | Federation import duty in basis points (default `500`) |
| `token_ttl` | Access-token lifetime (e.g. `15m`) |
| `log_level` / `log_file` / `log_format` | Structured logging |
| `kernel_handle` / `peer_auto_accept` / `bootstrap_peers` | Federation identity, friending behaviour, and the peers dialed to join the discovery network |
| `allow_local_sources` | Permit loopback/private URLs for action sources and OAuth endpoints (off by default) |
| `credentials_key` | Auto-generated AES-256 key sealing action upstream credentials and delegated-OAuth grant refresh tokens |

Environment variables are bootstrap and overrides only (everything else is configured
through `config.json`): `JUICE_HOME` (root for all juice state, default `~/.juice`; the kernel
uses `$JUICE_HOME/kernel/`), `JUICE_SECRET_KEY`, `JUICE_LOG_LEVEL`, `JUICE_CREDENTIALS_KEY`,
`JUICE_BOOTSTRAP_PASSWORD`, `JUICE_BOOTSTRAP_KERNEL_HANDLE`, and `JUICE_ALLOW_LOCAL_SOURCES`.
The database and config paths are the `--db` / `--config` flags; the HTTP listen address is
the `--addr` flag.

## Architecture

```text
cmd/juice/   CLI + HTTP server, config, bootstrap (wires everything together)
kernel/      Core types, interfaces, auth, call/settlement semantics, federation, OpenAPI import
store/       SQLite implementation of kernel.Store (migrations, WAL)
script/      WebAssembly execution via wazero
llm/         Language and embedding adapter (Ollama) for lookup, chat, json, decide
native/      Native @sys action handlers (lookup, llm/*, time, sink, message, random, web, tinygo)
log/         Structured logger (slog + tint, text + JSON)
```

`kernel/` imports no SQLite, wazero, Ollama, CLI, or HTTP code — those adapters are injected at
startup behind ordinary Go interfaces. Every source file has a corresponding `_test.go`, and
`go test ./...` requires no network access.

## Further reading

- **[API.md](API.md)** — authoritative HTTP/CLI contract and design rules.
- **[requirements.md](requirements.md)** — the kernel specification.
- **[docs/oauth.md](docs/oauth.md)** — building actions that call APIs as the caller (delegated OAuth or a per-user API key).
- **[flows/](flows/)** — runnable end-to-end shell flows (`flows_test.sh` drives the rest).
