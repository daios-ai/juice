# Juice

Juice is a callable action platform — a lightweight kernel for defining, securing, and invoking named actions with built-in accounting, tracing, and access control.

Actions can be HTTP endpoints, WebAssembly modules, or native handlers. Every call runs inside a funded process, is fully traced, and settles atomically.

## Features

- **Actions** — register HTTP, WASM, or native handlers with optional JSON Schema validation on inputs and outputs
- **Processes** — budgeted execution contexts; funds are locked per-call and settled on success, refunded on failure
- **ACL** — per-action `read`, `call`, and `admin` permissions; owners can grant and revoke per user
- **Tracing** — every call creates a child trace; nested WASM calls form a full trace tree across the process
- **Auth** — bcrypt passwords, short-lived JWT access tokens (15 min), rotating refresh tokens (30 days), PKCE S256 flow
- **Events** — named event listeners that fire an action when an event is emitted by a source user; pollable queues
- **WASM host functions** — scripts can call other actions, emit events, and log via `juice.call`, `juice.emit`, `juice.log`
- **Stats** — incremental mean tracking per action for latency, price, success rate, and rating
- **Feedback** — recursive cost and wall-clock latency for any subtree of the trace tree
- **Lookup** — cosine similarity search over action embeddings, re-ranked by success rate
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
./juice action create --name echo --kind http --source https://httpbin.org/post --price 0

# Activate it
./juice action enable --id <action-id>

# Start a funded process
./juice process start --funds 1000

# Call the action
./juice call --process <pid> --action @alice/echo --args '{"msg":"hello"}'

# Inspect the transaction
./juice tx show --id <txid>

# End the process (returns remaining funds to owner)
./juice process end --id <pid>
```

## Commands

| Command | Description |
|---|---|
| `juice user create` | Create a user account |
| `juice auth login` | Authenticate and store a token |
| `juice auth logout` | Remove the stored token |
| `juice auth refresh` | Rotate the refresh token |
| `juice action create` | Register a new action |
| `juice action update` | Update action metadata |
| `juice action enable / disable` | Activate or deactivate an action |
| `juice action list` | List actions |
| `juice action delete` | Delete an action |
| `juice action acl grant / revoke` | Manage per-user call permissions |
| `juice action stats` | View incremental stats for an action |
| `juice process start` | Open a funded process |
| `juice process fund` | Add credits to a running process |
| `juice process show` | Read process state |
| `juice process end` | Close a process and return remaining funds |
| `juice call` | Call an action within a process |
| `juice tx list / show` | View transactions |
| `juice listener create / delete` | Register and remove event listeners |
| `juice listener list / show` | List and inspect listeners |
| `juice event emit` | Emit a named event |
| `juice event list / consume` | Poll and consume pending events |
| `juice serve` | Start the HTTP API server |

All commands accept `--output json` for machine-readable output.

## HTTP API

```bash
./juice serve --addr :4040
```

All routes except `POST /v1/auth/token`, `POST /v1/auth/authorize`, and `POST /v1/users` require `Authorization: Bearer <token>`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health check (unauthenticated) |
| `GET` | `/.well-known/juice-kernel.json` | Kernel manifest (unauthenticated) |
| `POST` | `/v1/users` | Create user |
| `GET` | `/v1/me` | Read current user |
| `POST` | `/v1/auth/token` | Password grant or auth-code exchange |
| `POST` | `/v1/auth/authorize` | PKCE authorization |
| `POST` | `/v1/auth/refresh` | Rotate refresh token |
| `POST` | `/v1/auth/logout` | Revoke refresh token |
| `GET` | `/v1/actions` | List active public actions |
| `POST` | `/v1/actions` | Create action |
| `GET` | `/v1/actions/{id}` | Read action |
| `PUT` | `/v1/actions/{id}` | Update action |
| `DELETE` | `/v1/actions/{id}` | Delete action |
| `GET` | `/v1/actions/{id}/manifest` | Read action manifest |
| `POST` | `/v1/actions/{id}/enable` | Activate action |
| `POST` | `/v1/actions/{id}/disable` | Deactivate action |
| `POST` | `/v1/actions/{id}/acl` | Grant permission to a user |
| `DELETE` | `/v1/actions/{id}/acl/{subject_id}/{permission}` | Revoke permission from a user |
| `POST` | `/v1/actions/{id}/grant-all` | Make action publicly callable |
| `POST` | `/v1/actions/{id}/revoke-all` | Revoke public access |
| `GET` | `/v1/processes` | List processes |
| `POST` | `/v1/processes` | Start process |
| `GET` | `/v1/processes/{id}` | Read process |
| `POST` | `/v1/processes/{id}/fund` | Add funds |
| `POST` | `/v1/processes/{id}/end` | Close process |
| `POST` | `/v1/call` | Call an action |
| `GET` | `/v1/transactions` | List transactions |
| `GET` | `/v1/transactions/{id}` | Read transaction |
| `POST` | `/v1/transactions/{id}/rate` | Rate a transaction (0 or 1); returns signed rating |
| `GET` | `/v1/stats/{action_id}` | Read action stats |
| `GET` | `/v1/listeners` | List listeners |
| `POST` | `/v1/listeners` | Create event listener |
| `GET` | `/v1/listeners/{id}` | Read listener metadata |
| `GET` | `/v1/listeners/{id}/events` | Poll pending events for a listener |
| `DELETE` | `/v1/listeners/{id}` | Delete listener |
| `POST` | `/v1/events/emit` | Emit a named event |
| `POST` | `/v1/events/{id}/consume` | Consume a pending event |

## Configuration

| Environment variable | Default | Description |
|---|---|---|
| `JUICE_DB_PATH` | `juice.db` | SQLite file path |
| `JUICE_SECRET_KEY` | `dev-secret-change-me` | JWT signing secret — **change in production** |
| `JUICE_ADDR` | `:4040` | HTTP server listen address |
| `JUICE_FEE_RECIPIENT` | — | User ID that receives platform fees |
| `JUICE_FEE_BPS` | `2000` | Fee in basis points (2000 = 20%) |
| `JUICE_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |
| `JUICE_LOG_FILE` | — | JSON log file path (stderr only if unset) |
| `JUICE_OLLAMA_URL` | — | Ollama base URL for semantic lookup |
| `JUICE_OLLAMA_EMBED_MODEL` | `nomic-embed-text` | Ollama embedding model name |

## Architecture

```
cmd/juice/   CLI + HTTP server (wires everything together)
kernel/      Core types, interfaces, auth, call semantics, accounting
store/       SQLite implementation of kernel.Store
script/      WebAssembly execution via wazero
llm/         Ollama embedder for semantic lookup
log/         Structured logger (slog + tint, text + JSON)
```

`kernel/` has no dependencies on `store/`, `script/`, or `llm/` — those are injected at startup. Every source file has its own test file; `go test ./...` requires no network access.
