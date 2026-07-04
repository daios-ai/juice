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
Every read or list response for an action resource includes both `id` (UUID, for management operations) and a computed `action` field containing `@owner/name` (for running). Clients can copy the `action` value directly into run requests without a separate lookup. For `kind=http` actions, responses also include a decomposed `http` object `{method, url, params}` — identical in shape for manually-created and OpenAPI-imported actions — that round-trips with the `source`/`method`/`param` create and update inputs.

**R9 — Secrets never serialize.**  
Auth configs (`Action.source` upstream credentials) are write-only: accepted on create and update, never present in any read, list, log, receipt, hash, or manifest response. There is no read path for a stored secret.

### CLI

**C1 — A command's primary identifier is a positional argument.**  
The thing a command acts on is positional, not a flag. A second mandatory value (amount, rating) is the second positional. Only optional inputs use `--flag` style.

**C2 — Positional identifiers are natural keys.**  
A user is `@handle` (never a UUID — handles are unique). An action is `@owner/name` (a raw id is also accepted). Processes, steps, and transactions, which have no human-readable name, are ids. The CLI never asks the user to type a user UUID.

**C3 — `--source` is reserved for URLs and file paths.**  
`--source` is used for action source URLs and script paths (`action create --source`). For `kind=http`, `--method` sets the verb (default `POST`; GET/POST/PUT/PATCH/DELETE) and the repeatable `--param name:in` (`in` = `path`/`query`/`body`) binds input fields; omitting `--param` uses implicit routing (`{name}` placeholders in the URL become path params, remaining args go to the query for GET/DELETE or the JSON body otherwise). User-handle inputs use descriptively named flags: `--required-caller @handle` in `step create`.

**C4 — Creation uses `create`.**  
All resource-creation commands use `create`: `user create`, `action create`, `step create`. Processes are not created by users — `run` creates them (see Run).

**C5 — Deletion uses `delete`.**  
Deletion commands use `delete`. Side effects of deletion are documented in the command description.

**C6 — A command group's `list` subcommand lists that group's primary noun.**  
`juice step list` lists steps. Optional filters (`--process`, `--status`) narrow the result without changing the command group.

**C7 — `stats` lives under `action`.**  
`juice action stats <action>`. Stats are a property of an action; the command belongs in the `action` group.

**C8 — Two machine-readable output modes: `--json` and `--quiet`.**  
Default output is human-readable text that surfaces the same fields as the HTTP response. `--json` returns the canonical JSON matching the HTTP response body. `--quiet` prints only the primary resource ID, or nothing for mutations that return no resource. Both are global flags.

**C9 — `@file.json` for JSON values.**  
A JSON value also accepts `@path/to/file.json`; the `@` prefix reads the value from the named file. Applies to the positional `json` argument of `run`/`step complete` and to `--input-schema`, `--output-schema`, `--auth`.

**C10 — `args` is always present; empty input is `{}`.**  
`POST /v1/run` and `POST /v1/steps/{id}/complete` require an `args` field in the request body. `{}` is the canonical representation of an empty argument set. The CLI passes `{}` when the positional `json` argument is omitted; the HTTP layer rejects a missing field with `ErrInvalidInput`.

**C11 — `--required-caller` always carries `@handle`.**  
The step completer is identified by a user handle at creation time. The server resolves the handle to a user ID stored as `required_caller_user_id`. Completion is rejected if the authenticated caller does not match.

**C12 — Diagnostic output goes to stderr; resource data goes to stdout.**  
Log lines, progress messages, and error text go to stderr. The only content written to stdout is the resource payload: human-readable summaries, `--json` bodies, and `--quiet` IDs. This makes every command pipeable and keeps `$(juice ... --quiet)` capture reliable.

**C13 — User-facing commands are TCP clients; `admin` is a control-socket client.**  
Every user-facing command runs by calling the server over HTTP; the base URL resolves from `--server`, then `JUICE_SERVER`, then `server_url` in config, defaulting to `http://localhost:4040`. `admin *` are superuser supervision served over a local `0600` Unix control socket next to the DB (bearer token + filesystem access), never the public TCP API. `juice serve` is the sole process that opens the database. Supervision over ordinary resources is **scope**, not a separate surface: a superuser sees all rows on `action/process/tx/step list` and may `action disable`/`enable` any action, all over the normal TCP API.

---

## Operation Reference

### Server

| Operation | HTTP | CLI |
|-----------|------|-----|
| Health check | `GET /health` (open) → `{status}` | `juice health [--url]` |

Federation has no HTTP surface: peer identity, gossip, manifests, the friend handshake, and inbound calls travel over the cross-kernel transport (§13), not over this API. Gossip is surfaced locally by `admin peers --gossip`; a remote kernel is inspected with `admin inspect <key>`.

### Authentication

| Operation | HTTP | CLI |
|-----------|------|-----|
| Password login | `POST /v1/auth/token` `{handle, password}` → `{token}` | `juice auth login <user> [--password]` |
| PKCE authorize | `POST /v1/auth/authorize` `{handle, password, code_challenge, [redirect_uri]}` → `302` if `redirect_uri` provided, else `200 {"redirect":"?code=CODE"}` | `juice auth login <user> --pkce --server <url>` |
| PKCE token exchange | `POST /v1/auth/token` `{grant_type:"authorization_code", code, code_verifier, [redirect_uri]}` → `{access_token, refresh_token}` | (handled internally by `--pkce` login) |
| Refresh token | `POST /v1/auth/refresh` `{refresh_token}` → `{access_token, refresh_token}` | `juice auth refresh` |
| Logout | `POST /v1/auth/logout` `{refresh_token}` → 204 | `juice auth logout` |

### Users

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create user | `POST /v1/users` `{handle, email, password}` → 201 user | `juice user create <user> <email> [--password]` |
| Get self | `GET /v1/me` → user (`id`, `handle`, `email`, `available`, `locked`) | `juice user me` |
| Update self | `PUT /v1/me` `{[email], [current_password, password]}` → user | `juice user update [--email] [--password]` |

`handle` is immutable. `email` and `password` are updatable by the authenticated user; `password` change requires `current_password` to verify the existing credential. At least one of `email` or `password` must be provided. Proxy users (federation peers) cannot be created here, cannot log in, and hold no tokens; they exist only through peer acceptance, authenticate per request by federation signature, and cannot use `PUT /v1/me`.

### Actions

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create action | `POST /v1/actions` `{name, kind, [source, method, params, description, price, input_schema, output_schema, auth]}` → 201 action | `juice action create <name> --kind [--source --method --param --description --price --input-schema --output-schema --auth]` |
| List actions | `GET /v1/actions[?owner=&name=]` → action[]; unauthenticated → active public actions; authenticated → active public actions plus caller's own active actions; **superuser → all actions**; `?owner=` filters by owner handle; `?name=` filters by name | `juice action list [--all --limit --offset]` |
| Show action | `GET /v1/actions/{id}` → action | `juice action show <action>` |
| Update action | `PUT /v1/actions/{id}` `{[price, description, source, method, params, input_schema, output_schema, public, auth]}` → action | `juice action update <action> [--price --description --source --method --param --input-schema --output-schema --public --auth]` |
| Enable action | `POST /v1/actions/{id}/enable` → `{active:true}` | `juice action enable <action>` |
| Disable action | `POST /v1/actions/{id}/disable` → `{active:false}` | `juice action disable <action>` |
| Delete action | `DELETE /v1/actions/{id}` → 204 | `juice action delete <action>` |
| Import OpenAPI | `POST /v1/actions/import` `{spec_url}` → import result | `juice action import <spec-url>` |
| Unimport OpenAPI | `POST /v1/actions/unimport` `{spec_url[, name]}` → action[] | `juice action unimport <spec-url> [--name]` |
| Get stats | `GET /v1/stats/{action_id}` → stats | `juice action stats <action>` |
| List ratings | `GET /v1/actions/{id}/ratings` → rating[] | — |

`<action>` is `@owner/name` (a raw id is also accepted). Action responses (show and list) include a computed `action` field (`@owner/name`) alongside `id`, plus the full `input_schema` and `output_schema` — the CLI text view shows the same fields the JSON returns. `price` is the subtree bound: the maximum total cost of the action and everything it calls. `auth` is the upstream credential config `{scheme, config, secrets}` (R9: write-only, never returned).

### Run

| Operation | HTTP | CLI |
|-----------|------|-----|
| Run action | `POST /v1/run` `{action, args}` → `{result, tx_id, trace_id, process_id}` | `juice run <action> [json]` |

`run` is the single execution entry point: it atomically creates a process funded with exactly the action's price (parked from the caller's available balance), issues the root call, and the process closes itself when the root call has returned and no steps remain outstanding. There is no process handle to manage and no funding amount to choose. `args` defaults to `{}` when omitted. Zero-price actions (e.g. `@sys/lookup`) run with zero funding — no special handling.

### Processes

| Operation | HTTP | CLI |
|-----------|------|-----|
| List processes | `GET /v1/processes` → process[]; own processes, or **all for a superuser** | `juice process list` |
| Show process | `GET /v1/processes/{id}` → process (`available`, `locked`, `status`, `awaiting_receipt`, `awaiting_receipt_since?`) | `juice process show <id>` |
| End process | `POST /v1/processes/{id}/end` → 204 | `juice process end <id>` |

Processes are created only by `run` and close automatically. `end` is the forced abort: it fails running calls, cancels waiting steps with their parked prices refunded, and returns remaining funds to the owner. An in-flight remote call is not force-failed by restart recovery and settles on its receipt.

### Transactions

| Operation | HTTP | CLI |
|-----------|------|-----|
| List transactions | `GET /v1/transactions[?process_id=]` → transaction[] | `juice tx list [--process --limit --offset]` |
| Show transaction | `GET /v1/transactions/{id}` → transaction | `juice tx show <id>` |
| Rate transaction | `POST /v1/transactions/{id}/rate` `{rating, note?}` → rating | `juice tx rate <id> <0\|1> [--note]` |
| Verify remote receipt | `GET /v1/transactions/{id}/receipt-verification` → verification | `juice tx verify <id>` |

A caller reads transactions where it is a captured party: payer (`owner_user_id`), caller (`caller_user_id`), or payee (`target_user_id`); a superuser reads all. `rating` must be 0 (bad) or 1 (good); `note` is an optional string. Transaction and list responses include a `rating` field — `{"value": 0|1, "note": string|null}` when rated, `null` when unrated. Transaction `args` and `result` fields are inline JSON objects. `gross` is the call's full allocation; `fee + net` is the value added paid out at settlement.

Remote-proxy transactions include `remote_receipt_hash` and `remote_receipt_json`. `receipt-verification` verifies entirely from local data, in two parts: **receipt integrity** (signature against the peer's public key, stored JSON against its stored hash, `action_id` against the proxy) and **settlement consistency** (local outcome matches `receipt.status`; payment to the proxy user equals `receipt.charge`; the local refund arithmetic checks out). The receipt's own `gross/net/fee` are the remote kernel's economics and are reported, not compared. Returns `ErrInvalidState` for non-remote-proxy transactions.

### Steps

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create step | `POST /v1/steps` `{trace_id, action_id, partial_args, required_caller}` → 201 step | `juice step create <action> --trace --required-caller <user> [--partial-args]` |
| List steps | `GET /v1/steps[?process_id=&status=]` → step[] | `juice step list [--process --status]` |
| Show step | `GET /v1/steps/{id}` → step | `juice step show <id>` |
| Complete step | `POST /v1/steps/{id}/complete` `{args}` → `{result, tx_id, trace_id, step_id}` | `juice step complete <id> [json]` |

A step is a funded continuation: creation snapshots the action's price as `step.price` and parks it from `trace_id`; the process is derived from `Trace(trace_id).process_id`. Completion spends the parked price — no funds check occurs, and the completion's allocation and transaction `gross` are `step.price`. `required_caller` is a `@handle`; the server resolves it to `required_caller_user_id`. Step listing returns the caller's own steps (as process owner or required caller), or all of them for a superuser. `status` filter accepts `waiting`, `running`, `done`, or `cancelled`. The `args` field in the complete request is merged with the step's `partial_args` (completion `args` overwrites on key collision); the allowed completion input is derived as `action.input_schema \ keys(partial_args)` — a violation rejects the completion and leaves the step `waiting`, never recorded as an action failure. Step read and list responses include `price`, `action_id`, a computed `action` field (`@owner/name`), and `waiting_on_peer` on a waiting step whose required caller is a peer. An outstanding step keeps its process open.

### System Actions

Native actions registered at bootstrap, owned by `@sys`, public, runnable like any other action — `juice run @sys/lookup '{"query":"…"}'`. `@sys/lookup` results carry `action_id`, `action` (`@owner/name`), `description`, `score`, `input_schema`, and `output_schema`:

| Action | Price | Purpose |
|--------|-------|---------|
| `@sys/lookup` | 0 | Rank active actions by query |
| `@sys/llm/chat` | 0 | Platform LLM chat |
| `@sys/llm/embed` | 0 | Text embedding vector |
| `@sys/llm/json`  | 0 | Structured JSON output from LLM, locally validated |
| `@sys/llm/decide` | 0 | LLM-driven action selection; returns chosen action and args without executing |
| `@sys/make` | 20 (configurable, `native.make`) | Synthesize a WASM action from a description |
| `@sys/time` | 0 | Current time |
| `@sys/sink` | 0 | Universal no-op step target |
| `@sys/message` | 0 | Message a user by creating a step they acknowledge |
| `@sys/random` | 0 | Random float in `[0, 1)` |
| `@sys/web` | 0 | Fetch a public web page (read-only GET) |
| `@sys/tinygo/compile` | 5 (configurable, `native.tinygo`) | Compile TinyGo source to a WASM artifact |

### Federation

Friending a peer imports its actions owner-qualified, so a peer action is addressed `@peer/owner/name` and is called through `POST /v1/run` like any local action; there are no federation HTTP endpoints. Trust and peering are managed via the admin commands below. The cross-kernel transport is an implementation detail (§13).

### Admin (control-socket, superuser)

The operator verbs no ordinary user performs — money, access, federation trust, and the global roster. Served over the local control socket, not the public TCP API. Everything else a superuser does (see all actions/processes/txs/steps, disable any action) is *scope* on the normal commands above, not an admin command.

| Operation | CLI |
|-----------|-----|
| List all users | `juice admin users [--limit --offset]` |
| Show user | `juice admin show <user>` |
| Suspend user | `juice admin suspend <user>` |
| Unsuspend user | `juice admin unsuspend <user>` |
| Deposit credits | `juice admin deposit <user\|key> <amount> [--reason --external-key]` |
| Withdraw credits | `juice admin withdraw <user\|key> <amount> [--reason --external-key]` |
| Friend a kernel | `juice admin friend <key>` |
| Unfriend a kernel | `juice admin unfriend <user\|key>` |
| List peers | `juice admin peers [--gossip]` |
| Inspect a kernel | `juice admin inspect <key\|user>` — by key, or `@handle` if already friended; identity, public actions, transacted friends, and reachability |
| Show own identity | `juice admin identity` — this kernel's public key, handle, listen addresses |

`<user>` is a `@handle` — or, for a peer, its base64url public key (the global name); `<action>` is `@owner/name` (or an id); `<key>` is a peer's base64url public key. `withdraw` requires `target.available ≥ amount`; it redeems credits and obliges the out-of-band payout. `admin friend <key>` on a denied key clears the denial and restarts the handshake. `admin unfriend` deny-lists the key, deactivates the peer's proxies, cancels steps addressed to it (parked prices refunded), and preserves balance and history.