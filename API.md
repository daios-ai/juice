# API Reference

## Design Rules

These rules define the consistent standard every operation must satisfy.

### HTTP

**R1 — Snake_case field names everywhere.**  
Every type exposed over HTTP carries `json:` struct tags with snake_case names. Go field names (`OwnerUserID`, `ArgsJSON`, `ProcessID`) never appear on the wire. `PasswordHash` is masked with `json:"-"`.

**R2 — No double-encoded JSON.**  
Fields that contain structured data use `json.RawMessage`, never `string`. `Transaction.ArgsJSON` and `Transaction.ReplyJSON` appear as inline objects tagged `"args"` and `"result"` respectively, matching `RunReply`.

**R3 — Action identity is `@owner/name`.**  
Wherever a request identifies a callable action, a single `action` field carries the combined `@owner/name` notation. The server resolves it. Split fields (`target` + `action_name`) are forbidden. Owner handles must not contain `/`; action names must not contain `/`. Parsing is unambiguous: split on the first `/` after `@`. When `@owner/name` appears in a URL query string, `/` must be percent-encoded.

**R4 — DELETE never carries a request body.**  
Sub-resource removal uses path parameters. Request bodies on DELETE are rejected by many proxies and clients.

**R5 — State mutations return the updated resource.**  
Any operation that changes resource state returns the new state as the response body. `POST /v1/actions/{id}/enable` returns the updated action. Purely destructive operations (`DELETE`, `POST .../end`) return 204.

**R6 — List responses are plain arrays.**  
No envelope objects. `GET /v1/steps` returns `[…]` directly. Metadata such as pagination belongs in response headers, not the body.

**R7 — Input validated at the HTTP boundary.**  
The handler rejects invalid inputs before calling the kernel. `rating` must be 0 or 1; returns `ErrInvalidInput` when violated.

**R8 — Action responses include both `id` and `action`.**  
Every read or list response for an action resource includes both `id` (UUID, for management operations) and a computed `action` field containing `@owner/name` (for running). Clients can copy the `action` value directly into run requests without a separate lookup.

**R9 — Secrets never serialize.**  
Auth configs (`Action.source` upstream credentials) are write-only: accepted on create and update, never present in any read, list, log, receipt, hash, or manifest response. There is no read path for a stored secret.

### CLI

**C1 — All arguments use `--flag` style.**  
No positional arguments anywhere.

**C2 — `--action` always carries `@owner/name`; `--id` always carries a UUID.**  
`--action` is never a UUID. `--id` is never a name. Operations that manage a resource the caller owns use `--id`. Operations that reference a callable action use `--action @owner/name`.

**C3 — `--source` is reserved for URLs and file paths.**  
`--source` is used for action source URLs and script paths (`action create --source`). User-handle inputs use descriptively named flags: `--required-caller @handle` in `step create`.

**C4 — Creation uses `create`.**  
All resource-creation commands use `create`: `user create`, `action create`, `step create`. Processes are not created by users — `run` creates them (see Run).

**C5 — Deletion uses `delete`.**  
Deletion commands use `delete`. Side effects of deletion are documented in the command description.

**C6 — A command group's `list` subcommand lists that group's primary noun.**  
`juice step list` lists steps. Optional filters (`--process`, `--status`) narrow the result without changing the command group.

**C7 — `stats` lives under `action`.**  
`juice action stats --id <id>`. Stats are a property of an action; the command belongs in the `action` group.

**C8 — Two machine-readable output modes: `--output json` and `--quiet`.**  
Default output is human-readable text. `--output json` returns the JSON matching the HTTP response body. `--quiet` prints only the primary resource ID, or nothing for mutations that return no resource. Both are global flags.

**C9 — `--args @file.json` for any JSON flag.**  
Any CLI flag that accepts a JSON value also accepts `@path/to/file.json`. The `@` prefix signals that the value is read from the named file. Applies to `--args`, `--input-schema`, `--output-schema`, `--auth`.

**C10 — `args` is always present; empty input is `{}`.**  
`POST /v1/run` and `POST /v1/steps/{id}/complete` require an `args` field in the request body. `{}` is the canonical representation of an empty argument set. The CLI passes `{}` when `--args` is omitted; the HTTP layer rejects a missing field with `ErrInvalidInput`.

**C11 — `--required-caller` always carries `@handle`.**  
The step completer is identified by a user handle at creation time. The server resolves the handle to a user ID stored as `required_caller_user_id`. Completion is rejected if the authenticated caller does not match.

**C12 — Diagnostic output goes to stderr; resource data goes to stdout.**  
Log lines, progress messages, and error text go to stderr. The only content written to stdout is the resource payload: human-readable summaries, `--output json` bodies, and `--quiet` IDs. This makes every command pipeable and keeps `$(juice ... --quiet)` capture reliable.

---

## Operation Reference

### Server

| Operation | HTTP | CLI |
|-----------|------|-----|
| Health check | `GET /health` (open) → `{status}` | `juice health [--url]` |
| Federation metadata | `GET /.well-known/juice-kernel.json` (open) → `{public_key, handle, base_url}` | — |
| Gossip | `GET /v1/gossip` (open) → identity, own actions with manifests and stats, transacted friends with stats | — (consumed by `admin peer gossip`) |

### Authentication

| Operation | HTTP | CLI |
|-----------|------|-----|
| Password login | `POST /v1/auth/token` `{handle, password}` → `{token}` | `juice auth login --handle [--password]` |
| PKCE authorize | `POST /v1/auth/authorize` `{handle, password, code_challenge, [redirect_uri]}` → `302` if `redirect_uri` provided, else `200 {"redirect":"?code=CODE"}` | `juice auth login --handle --pkce --server <url>` |
| PKCE token exchange | `POST /v1/auth/token` `{grant_type:"authorization_code", code, code_verifier, [redirect_uri]}` → `{access_token, refresh_token}` | (handled internally by `--pkce` login) |
| Refresh token | `POST /v1/auth/refresh` `{refresh_token}` → `{access_token, refresh_token}` | `juice auth refresh` |
| Logout | `POST /v1/auth/logout` `{refresh_token}` → 204 | `juice auth logout` |

### Users

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create user | `POST /v1/users` `{handle, email, password}` → 201 user | `juice user create --handle --email [--password]` |
| Get self | `GET /v1/me` → user (`id`, `handle`, `email`, `available`, `locked`) | `juice user me` |
| Update self | `PUT /v1/me` `{[email], [current_password, password]}` → user | `juice user update [--email] [--password]` |

`handle` is immutable. `email` and `password` are updatable by the authenticated user; `password` change requires `current_password` to verify the existing credential. At least one of `email` or `password` must be provided. Proxy users (federation peers) cannot be created here, cannot log in, and hold no tokens; they exist only through peer acceptance, authenticate per request by federation signature, and cannot use `PUT /v1/me`.

### Actions

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create action | `POST /v1/actions` `{name, kind, [source, description, price, input_schema, output_schema, auth]}` → 201 action | `juice action create --name --kind [--source --description --price --input-schema --output-schema --auth]` |
| List actions | `GET /v1/actions[?owner=&name=]` → action[]; unauthenticated → active public actions; authenticated → active public actions plus caller's own active actions; `?owner=` filters by owner handle; `?name=` filters by name | `juice action list [--all --limit --offset]` |
| Show action | `GET /v1/actions/{id}` → action | `juice action show --id` |
| Update action | `PUT /v1/actions/{id}` `{[price, description, source, input_schema, output_schema, public, auth]}` → action | `juice action update --id [--price --description --source --input-schema --output-schema --public --auth]` |
| Enable action | `POST /v1/actions/{id}/enable` → `{active:true}` | `juice action enable --id` |
| Disable action | `POST /v1/actions/{id}/disable` → `{active:false}` | `juice action disable --id` |
| Delete action | `DELETE /v1/actions/{id}` → 204 | `juice action delete --id` |
| Import OpenAPI | `POST /v1/actions/import` `{spec_url}` → import result | `juice action import --openapi <url>` |
| Unimport OpenAPI | `POST /v1/actions/unimport` `{spec_url[, name]}` → action[] | `juice action unimport --openapi <url> [--name]` |
| Get manifest | `GET /v1/actions/{id}/manifest` → signed manifest; served to friends in good standing per the exposure lever | — (used internally by `peer friend`) |
| Get stats | `GET /v1/stats/{action_id}` → stats | `juice action stats --id` |
| List ratings | `GET /v1/actions/{id}/ratings` → rating[] | — |

Action responses include a computed `action` field (`@owner/name`) alongside `id`. `price` is the subtree bound: the maximum total cost of the action and everything it calls. `auth` is the upstream credential config `{scheme, config, secrets}` (R9: write-only, never returned).

### Run

| Operation | HTTP | CLI |
|-----------|------|-----|
| Run action | `POST /v1/run` `{action, args}` → `{result, tx_id, trace_id, process_id}` | `juice run --action @owner/name [--args]` |

`run` is the single execution entry point: it atomically creates a process funded with exactly the action's price (parked from the caller's available balance), issues the root call, and the process closes itself when the root call has returned and no steps remain outstanding. There is no process handle to manage and no funding amount to choose. `args` defaults to `{}` when omitted. Zero-price actions (e.g. `@sys/lookup`) run with zero funding — no special handling.

### Processes

| Operation | HTTP | CLI |
|-----------|------|-----|
| List processes | `GET /v1/processes` → process[] | `juice process list` |
| Show process | `GET /v1/processes/{id}` → process (`available`, `locked`, `status`) | `juice process show --id` |
| End process | `POST /v1/processes/{id}/end` → 204 | `juice process end --id` |

Processes are created only by `run` and close automatically. `end` is the forced abort: it fails running calls, cancels waiting steps with their parked prices refunded, and returns remaining funds to the owner. An in-flight remote call is not force-failed by restart recovery and settles on its receipt.

### Transactions

| Operation | HTTP | CLI |
|-----------|------|-----|
| List transactions | `GET /v1/transactions[?process_id=]` → transaction[] | `juice tx list [--process --limit --offset]` |
| Show transaction | `GET /v1/transactions/{id}` → transaction | `juice tx show --id` |
| Rate transaction | `POST /v1/transactions/{id}/rate` `{rating, note?}` → rating | `juice tx rate --id --rating [--note]` |
| Verify remote receipt | `GET /v1/transactions/{id}/receipt-verification` → verification | `juice tx verify-receipt --id` |

A caller reads transactions where it is a captured party: payer (`owner_user_id`), caller (`caller_user_id`), or payee (`target_user_id`). `rating` must be 0 (bad) or 1 (good); `note` is an optional string. Transaction and list responses include a `rating` field — `{"value": 0|1, "note": string|null}` when rated, `null` when unrated. Transaction `args` and `result` fields are inline JSON objects. `gross` is the call's full allocation; `fee + net` is the value added paid out at settlement.

Remote-proxy transactions include `remote_receipt_hash` and `remote_receipt_json`. `receipt-verification` verifies entirely from local data, in two parts: **receipt integrity** (signature against the peer's public key, stored JSON against its stored hash, `action_id` against the proxy) and **settlement consistency** (local outcome matches `receipt.status`; payment to the proxy user equals `receipt.charge`; the local refund arithmetic checks out). The receipt's own `gross/net/fee` are the remote kernel's economics and are reported, not compared. Returns `ErrInvalidState` for non-remote-proxy transactions.

### Steps

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create step | `POST /v1/steps` `{process_id, parent_trace_id, action, partial_args, input_schema, required_caller}` → 201 step | `juice step create --process --parent-trace --action @owner/name --partial-args --input-schema --required-caller @handle` |
| List steps | `GET /v1/steps[?process_id=&status=]` → step[] | `juice step list [--process --status]` |
| Show step | `GET /v1/steps/{id}` → step | `juice step show --id` |
| Complete step | `POST /v1/steps/{id}/complete` `{args}` → `{result, tx_id, trace_id, step_id}` | `juice step complete --id --args` |

A step is a funded continuation: creation snapshots the action's price as `step.price` and parks it from the funding trace, which must belong to the same process (`Trace(parent_trace_id).process_id = process_id`). Completion spends the parked price — no funds check occurs, and the completion's allocation and transaction `gross` are `step.price`. `required_caller` is a `@handle`; the server resolves it to `required_caller_user_id`. `status` filter accepts `waiting`, `running`, `done`, or `cancelled`. The `args` field in the complete request is merged with the step's `partial_args` (completion `args` overwrites on key collision); a violation of `input_schema` rejects the completion and leaves the step `waiting` — it is never recorded as an action failure. Step read and list responses include `price` and a computed `action` field (`@owner/name`) alongside `next_action_id`. An outstanding step keeps its process open.

### System Actions

Native actions registered at bootstrap, owned by `@sys`, public, runnable like any other action — `juice run --action @sys/lookup --args '{"query":"…"}'`:

| Action | Price | Purpose |
|--------|-------|---------|
| `@sys/lookup` | 0 | Rank active actions by query |
| `@sys/llm/chat` | 0 | Platform LLM chat |
| `@sys/llm/embed` | 0 | Text embedding vector |
| `@sys/llm/json`  | 0 | Structured JSON output from LLM, locally validated |
| `@sys/llm/tools` | 0 | LLM tool selection over Juice action contracts |
| `@sys/make` | 20 (configurable, `native.make`) | Synthesize a WASM action from a description |
| `@sys/time` | 0 | Current time |
| `@sys/sink` | 0 | Universal no-op step target |
| `@sys/message` | 0 | Message a user by creating a step they acknowledge |
| `@sys/random` | 0 | Random float in `[0, 1)` |

### Federation (HTTP-only protocol surface)

| Operation | HTTP |
|-----------|------|
| Friend request | `POST /v1/peers` — signed; auto-accepted by default (`peer_auto_accept`), pending under manual mode; rate-limited per IP |
| Inbound federation call | `POST /v1/federation/call?action=@owner%2Fname&counterparty=<pubkey>` — signature-authenticated; runs as the proxy user from its prepaid balance; underfunded or denied callers receive a signed rejection receipt |

### Admin (CLI-only, superuser)

| Operation | CLI |
|-----------|-----|
| List all users | `juice admin user list [--limit --offset]` |
| Show user | `juice admin user show --id\|--handle` |
| Suspend user | `juice admin user suspend --id` |
| Unsuspend user | `juice admin user unsuspend --id` |
| Deposit credits | `juice admin user deposit --id\|--handle --amount [--reason]` |
| Withdraw credits | `juice admin user withdraw --id\|--handle --amount [--reason]` |
| Friend a kernel | `juice peer friend --url` |
| Unfriend a kernel | `juice peer unfriend --handle` |
| List peers | `juice peer list` |
| Inspect a kernel | `juice peer inspect --url` — identity, public actions, and transacted friends |
| List all actions | `juice admin action list [--limit --offset]` |
| Disable action | `juice admin action disable --id` |
| List all processes | `juice admin process list [--limit --offset]` |
| List all transactions | `juice admin tx list [--limit --offset]` |
| List all steps | `juice admin step list [--limit --offset]` |

`withdraw` requires `target.available ≥ amount`; it redeems credits and obliges the out-of-band payout. `peer friend --url` on a denied key clears the denial and restarts the handshake. `peer unfriend` deny-lists the key, deactivates the peer's proxies, cancels steps addressed to it (parked prices refunded), and preserves balance and history.