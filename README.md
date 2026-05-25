# Juice

Juice is a callable action platform — a lightweight kernel for defining, securing, and invoking named actions with built-in accounting, tracing, and access control.

Actions can be HTTP endpoints, WebAssembly modules, or native handlers. Every call runs inside a funded process, is fully traced, and settles atomically.

## Features

- **Actions** — register HTTP, WASM, or native handlers with optional JSON Schema validation
- **Processes** — budgeted execution contexts; funds are locked per-call and settled on success
- **ACL** — per-action `read`, `call`, and `admin` permissions
- **Tracing** — every call creates a child trace; nested WASM calls form a full trace tree
- **Auth** — bcrypt passwords, short-lived JWT access tokens, rotating refresh tokens, PKCE flow
- **Stats** — incremental mean tracking for latency, price, and success rate
- **Lookup** — semantic search over actions using an Ollama embedding model
- **HTTP API** — full REST API mirroring the CLI
- **SQLite** — single-file database, WAL mode, pure Go (no CGO)

## Installation

```bash
git clone https://github.com/daios-ai/juice.git
cd juice
go build -o juice ./cmd/juice/
```

Requires Go 1.25+.

## Quick start

```bash
# Create a user
./juice user create --handle @alice --email alice@example.com

# Log in (stores token in ~/.juice/token)
./juice auth login --handle @alice

# Register an action
./juice action add --name /echo --kind http --source https://httpbin.org/post --price 0

# Activate it
./juice action enable --id <action-id>

# Start a funded process
./juice process start --funds 1000

# Call the action
./juice call --process <pid> --target @alice --action /echo --args '{"msg":"hello"}'

# Inspect the transaction
./juice tx show --id <txid>

# End the process (returns remaining funds)
./juice process end --id <pid>
```

## Commands

| Command | Description |
|---|---|
| `juice user create` | Create a user account |
| `juice auth login` | Authenticate and store a token |
| `juice auth logout` | Remove the stored token |
| `juice auth refresh` | Rotate the refresh token |
| `juice action add` | Register a new action |
| `juice action update` | Update action metadata |
| `juice action enable/disable` | Activate or deactivate an action |
| `juice action list` | List actions |
| `juice action delete` | Delete an action |
| `juice action acl grant/revoke` | Manage per-user permissions |
| `juice process start` | Open a funded process |
| `juice process fund` | Add credits to a process |
| `juice process end` | Close a process and return funds |
| `juice call` | Call an action within a process |
| `juice tx list/show` | View transactions |
| `juice stats show` | View action statistics |
| `juice lookup` | Semantic action search |
| `juice events listen/unlisten` | Register event listeners |
| `juice events emit/poll` | Emit events and poll queues |
| `juice serve` | Start the HTTP API server |

All commands accept `--output json` for machine-readable output.

## HTTP API

```bash
./juice serve --addr :8080
```

Endpoints mirror the CLI. All routes except `/v1/auth/*` and `POST /v1/users` require a `Authorization: Bearer <token>` header.

## Configuration

| Environment variable | Default | Description |
|---|---|---|
| `JUICE_DB_PATH` | `juice.db` | SQLite file path |
| `JUICE_SECRET_KEY` | `dev-secret-change-me` | JWT signing secret — **change in production** |
| `JUICE_ADDR` | `:8080` | HTTP server listen address |
| `JUICE_FEE_RECIPIENT` | — | User ID that receives platform fees |
| `JUICE_FEE_BPS` | `2000` | Fee in basis points (2000 = 20%) |
| `JUICE_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |
| `JUICE_LOG_FILE` | — | JSON log file path (stdout only if unset) |
| `JUICE_OLLAMA_URL` | — | Ollama base URL for semantic lookup |
| `JUICE_OLLAMA_EMBED_MODEL` | `nomic-embed-text` | Ollama embedding model |

## Architecture

```
cmd/juice/   CLI + HTTP server (wires everything together)
kernel/      Core types, interfaces, auth, call semantics, accounting
store/       SQLite implementation of kernel.Store
script/      WebAssembly execution via wazero
llm/         Ollama embedder for semantic lookup
log/         Structured logger (slog-based, text + JSON)
```

`kernel/` has no dependencies on `store/`, `script/`, or `llm/` — those are injected at startup.

## License

MIT
