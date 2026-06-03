# API Reference

## Design Rules

These rules define the consistent standard every operation must satisfy.

### HTTP

**R1 — Snake_case field names everywhere.**  
Every type exposed over HTTP carries `json:` struct tags with snake_case names. Go field names (`OwnerUserID`, `ArgsJSON`, `ProcessID`) never appear on the wire. `PasswordHash` is masked with `json:"-"`.

**R2 — No double-encoded JSON.**  
Fields that contain structured data use `json.RawMessage`, never `string`. `Transaction.ArgsJSON` and `Transaction.ReplyJSON` appear as inline objects tagged `"args"` and `"result"` respectively, matching `CallReply`.

**R3 — Action identity is `@owner/name`.**  
Wherever a request identifies a callable action, a single `action` field carries the combined `@owner/name` notation. The server resolves it. Split fields (`target` + `action_name`) are forbidden. Owner handles must not contain `/`; action names must not contain `/`. Parsing is unambiguous: split on the first `/` after `@`. When `@owner/name` appears in a URL query string, `/` must be percent-encoded.

**R4 — DELETE never carries a request body.**  
Sub-resource removal uses path parameters: `DELETE /v1/actions/{id}/acl/{subject_id}/{permission}`. Request bodies on DELETE are rejected by many proxies and clients.

**R5 — State mutations return the updated resource.**  
Any operation that changes resource state returns the new state as the response body. `POST /v1/processes/{id}/fund` returns the updated process. Purely destructive operations (`DELETE`, `POST .../end`) return 204.

**R6 — List responses are plain arrays.**  
No envelope objects. `GET /v1/listeners/{id}/events` returns `[…]` directly. Metadata such as pagination belongs in response headers, not the body.

**R7 — Input validated at the HTTP boundary.**  
The handler rejects invalid inputs before calling the kernel. `rating` must be 0 or 1. `permission` must be `read`, `call`, or `admin`. Both return `ErrInvalidInput` when violated.

**R8 — Action responses include both `id` and `action`.**  
Every read or list response for an action resource includes both `id` (UUID, for management operations) and a computed `action` field containing `@owner/name` (for calling). Clients can copy the `action` value directly into call requests without a separate lookup.

### CLI

**C1 — All arguments use `--flag` style.**  
No positional arguments anywhere. `remote add --url <url>`, `remote import --remote <handle> --action @handle/name`.

**C2 — `--action` always carries `@owner/name`; `--id` always carries a UUID.**  
`--action` is never a UUID. `--id` is never a name. Operations that manage a resource the caller owns use `--id`. Operations that reference a callable action use `--action @owner/name`.

**C3 — `--source-user` for user-handle inputs, never `--source`.**  
`--source` is reserved for URLs and file paths (`action create --source`). The source-user argument in `listener create` uses `--source-user`.

**C4 — Creation uses `create`.**  
All resource-creation commands use `create`: `user create`, `action create`, `listener create`, `process start`.

**C5 — Deletion uses `delete`.**  
Deletion commands use `delete`. `listener delete` purges pending events as a side effect, documented in the command description.

**C6 — A command group's `list` subcommand lists that group's primary noun.**  
`juice listener list` lists listeners. `juice event list --listener <id>` lists pending events for a listener. Subscriptions and queued work items are separate groups.

**C7 — Zero-cost native actions do not require process provisioning.**  
`juice lookup` calls `@sys/lookup` at price 0. The CLI creates a zero-funded ephemeral process internally and ends it after the call. No `--process` flag is needed.

**C8 — `stats` lives under `action`.**  
`juice action stats --id <id>`. Stats are a property of an action; the command belongs in the `action` group.

**C9 — Two machine-readable output modes: `--output json` and `--quiet`.**  
Default output is human-readable text. `--output json` returns the JSON matching the HTTP response body. `--quiet` prints only the primary resource ID, or nothing for mutations that return no resource. Both are global flags.

**C10 — `--args @file.json` for any JSON flag.**  
Any CLI flag that accepts a JSON value also accepts `@path/to/file.json`. The `@` prefix signals that the value is read from the named file. Applies to `--args`, `--input-schema`, `--output-schema`.

**C11 — `args` is always present; empty input is `{}`.**  
`POST /v1/call` and `POST /v1/events/emit` require `args` in the request body. `{}` is the canonical representation of an empty argument set. Omitting `args` is normalized to `{}` rather than rejected, for ergonomics.

**C12 — Emit source is always the authenticated subject.**  
`POST /v1/events/emit` does not accept a `source_user_id` field. The event source is set from the authenticated caller. The server ignores any `source_user_id` in the request body.

**C13 — Diagnostic output goes to stderr; resource data goes to stdout.**  
Log lines, progress messages, and error text go to stderr. The only content written to stdout is the resource payload: human-readable summaries, `--output json` bodies, and `--quiet` IDs. This makes every command pipeable and keeps `$(juice ... --quiet)` capture reliable.

---

## Operation Reference

### Server

| Operation | HTTP | CLI |
|-----------|------|-----|
| Health check | `GET /health` (open) → `{status}` | `juice health [--url]` |
| Federation metadata | `GET /.well-known/juice-kernel.json` (open) → manifest | — |

### Authentication

| Operation | HTTP | CLI |
|-----------|------|-----|
| Password login | `POST /v1/auth/token` `{handle, password}` → `{token}` | `juice auth login --handle [--password]` |
| PKCE authorize | `POST /v1/auth/authorize` `{handle, password, code_challenge, [redirect_uri]}` → `{redirect}` | `juice auth login --handle --pkce --server <url>` |
| PKCE token exchange | `POST /v1/auth/token` `{grant_type:"authorization_code", code, code_verifier, [redirect_uri]}` → `{access_token, refresh_token}` | (handled internally by `--pkce` login) |
| Refresh token | `POST /v1/auth/refresh` `{refresh_token}` → `{access_token, refresh_token}` | `juice auth refresh` |
| Logout | `POST /v1/auth/logout` `{refresh_token}` → 204 | `juice auth logout` |

### Users

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create user | `POST /v1/users` `{handle, email, password}` → 201 user | `juice user create --handle --email [--password]` |
| Get self | `GET /v1/me` → user | `juice user me` |

### Actions

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create action | `POST /v1/actions` `{name, kind, [source, description, price, input_schema, output_schema]}` → 201 action | `juice action create --name --kind [--source --description --price --input-schema --output-schema]` |
| List public actions | `GET /v1/actions[?owner=&name=]` → action[] | `juice action list [--all --limit --offset]` |
| Show action | `GET /v1/actions/{id}` → action | `juice action show --id` |
| Update action | `PUT /v1/actions/{id}` `{[price, description, source, input_schema, output_schema]}` → action | `juice action update --id [--price --description --source --input-schema --output-schema]` |
| Enable action | `POST /v1/actions/{id}/enable` → `{active:true}` | `juice action enable --id` |
| Disable action | `POST /v1/actions/{id}/disable` → `{active:false}` | `juice action disable --id` |
| Delete action | `DELETE /v1/actions/{id}` → 204 | `juice action delete --id` |
| Grant ACL | `POST /v1/actions/{id}/acl` `{subject_user_id, permission}` → 204 | `juice action acl grant --id --user --perm` |
| Revoke ACL | `DELETE /v1/actions/{id}/acl/{subject_id}/{permission}` → 204 | `juice action acl revoke --id --user --perm` |
| Make public | `POST /v1/actions/{id}/grant-all` → 204 | `juice action grant-all --id` |
| Make private | `POST /v1/actions/{id}/revoke-all` → 204 | `juice action revoke-all --id` |
| Import OpenAPI | `POST /v1/actions/import` `{spec_url}` → import result | `juice action import --openapi <url>` |
| Unimport OpenAPI | `POST /v1/actions/unimport` `{spec_url[, name]}` → action[] | `juice action unimport --openapi <url> [--name]` |
| Get manifest | `GET /v1/actions/{id}/manifest` → signed manifest | — (used internally by `remote import`) |
| Get stats | `GET /v1/stats/{action_id}` → stats | `juice action stats --id` |
| List ratings | `GET /v1/actions/{id}/ratings` → rating[] | — |
| List provider receipts | `GET /v1/actions/{id}/receipts` (owner) → receipt[] | `juice action receipts --id` |

Action responses include a computed `action` field (`@owner/name`) alongside `id`.

### Processes

| Operation | HTTP | CLI |
|-----------|------|-----|
| Start process | `POST /v1/processes` `{funds}` → 201 `{process_id, available, ...}` | `juice process start [--funds]` |
| List processes | `GET /v1/processes` → process[] | `juice process list` |
| Show process | `GET /v1/processes/{id}` → process | `juice process show --id` |
| Fund process | `POST /v1/processes/{id}/fund` `{funds}` → 200 process | `juice process fund --id --funds` |
| End process | `POST /v1/processes/{id}/end` → 204 | `juice process end --id` |

### Call

| Operation | HTTP | CLI |
|-----------|------|-----|
| Execute action | `POST /v1/call` `{process_id, action, args[, parent_trace_id]}` → `{result, tx_id, trace_id}` | `juice call --process --action @owner/name [--trace] [--args]` |

`action` is `@owner/name`. `args` defaults to `{}` when omitted.

### Transactions

| Operation | HTTP | CLI |
|-----------|------|-----|
| List transactions | `GET /v1/transactions[?process_id=]` → transaction[] | `juice tx list [--process --limit --offset]` |
| Show transaction | `GET /v1/transactions/{id}` → transaction | `juice tx show --id` |
| Rate transaction | `POST /v1/transactions/{id}/rate` `{rating}` → rating | `juice tx rate --id --rating` |

`rating` must be 0 (bad) or 1 (good). Transaction `args` and `result` fields are inline JSON objects.

### Listeners

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create listener | `POST /v1/listeners` `{source_user_id, event_name, target_action_id}` → 201 listener | `juice listener create --source-user @handle --event --action @owner/name` |
| List listeners | `GET /v1/listeners` → listener[] | `juice listener list` |
| Show listener | `GET /v1/listeners/{id}` → listener | `juice listener show --id` |
| Delete listener | `DELETE /v1/listeners/{id}` → 204 | `juice listener delete --id` |

Deleting a listener also purges all pending events for that listener.

### Events

| Operation | HTTP | CLI |
|-----------|------|-----|
| Emit event | `POST /v1/events/emit` `{event_name, args}` → `{event_ids:[]}` | `juice event emit --event [--args]` |
| List pending events | `GET /v1/listeners/{id}/events` → event[] | `juice event list --listener` |
| Consume event | `POST /v1/events/{id}/consume` `{process_id[, parent_trace_id]}` → call reply | `juice event consume --id --process [--trace]` |

The event source is always the authenticated caller. `source_user_id` is not an input field.

### System Actions

`@sys/lookup` and `@sys/llm-chat` are native actions registered at bootstrap. They are public, price 0, and callable through `juice call` like any other action. No dedicated CLI command exists for them; no special process handling is applied.

### Remote Kernels

| Operation | HTTP | CLI |
|-----------|------|-----|
| Add remote kernel | — | `juice remote add --url` |
| List remote kernels | — | `juice remote list` |
| Import remote action | — | `juice remote import --remote --action @handle/name` |
| Unimport remote action | — | `juice remote unimport --remote --action @handle/name` |

### Federation (HTTP-only)

| Operation | HTTP |
|-----------|------|
| Inbound federation call | `POST /v1/federation/call?action=@owner%2Fname&counterparty=<pubkey>` |

### Admin (CLI-only, superuser)

| Operation | CLI |
|-----------|-----|
| List all users | `juice admin user list [--limit --offset]` |
| Show user | `juice admin user show --id\|--handle` |
| Suspend user | `juice admin user suspend --id` |
| Unsuspend user | `juice admin user unsuspend --id` |
| Deposit credits | `juice admin user deposit --id\|--handle --amount [--reason]` |
| List all actions | `juice admin action list [--limit --offset]` |
| Disable action | `juice admin action disable --id` |
| List all processes | `juice admin process list [--limit --offset]` |
| List all transactions | `juice admin tx list [--limit --offset]` |
