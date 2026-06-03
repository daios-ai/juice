# API Design Reference

## Design Rules

These rules define the consistent standard every operation must satisfy.

### HTTP

**R1 — Snake_case field names everywhere.**  
Every type exposed over HTTP carries `json:` struct tags. No Go field names (`OwnerUserID`, `ArgsJSON`, `ProcessID`) leak to the wire. The rule applies to `Action`, `Process`, `Transaction`, `Listener`, `Event`, and `Trace`, which currently have no tags.

**R2 — No double-encoded JSON.**  
Fields that contain structured data use `json.RawMessage`, never `string`. `Transaction.ArgsJSON` and `Transaction.ReplyJSON` must appear as inline objects in responses, tagged `"args"` and `"result"` respectively, matching `CallReply`.

**R3 — Action identity is `@owner/name`.**  
Wherever a request identifies a callable action, a single `action` field carries the combined `@owner/name` notation. The server resolves it. Split fields (`target` + `action_name`) are forbidden.

**R4 — DELETE never carries a request body.**  
Sub-resource removal uses the path: `DELETE /v1/actions/{id}/acl/{subject_id}/{permission}`. Request bodies on DELETE are rejected by many proxies and clients.

**R5 — State mutations return the updated resource.**  
Any operation that changes resource state returns the new state as the response body. `POST /v1/processes/{id}/fund` returns the updated process; `POST /v1/processes/{id}/end` may return 204 (resource is gone). Purely destructive operations (`DELETE`) return 204.

**R6 — List responses are plain arrays.**  
No envelope objects. `GET /v1/listeners/{id}/events` returns `[…]`, not `{"listener_id": …, "events": […]}`. Metadata (e.g., pagination) belongs in response headers, not the body.

**R7 — Input validated at the HTTP boundary.**  
The handler rejects invalid inputs before calling the kernel. `rating` must be 0 or 1 (`ErrInvalidInput` otherwise). `permission` must be `read`, `call`, or `admin` (currently only validated in the kernel after a potential SQLite error path).

### CLI

**C1 — All arguments use `--flag` style.**  
No positional arguments. `remote add <url>` → `remote add --url <url>`. `remote import <handle> <name>` → `remote import --remote <handle> --action @handle/name`.

**C2 — `--action` always carries `@owner/name`; `--id` always carries a UUID.**  
`--action` is never a UUID. `--id` is never a name. Operations that manage a resource the caller owns use `--id`. Operations that reference a callable action use `--action @owner/name`.  
Currently broken: `action acl grant --action <uuid>`, `events listen --action <uuid>`, versus `call --action /name`.

**C3 — `--source-user` for user-handle inputs, never `--source`.**  
`--source` is reserved for URLs and file paths (`action add --source`). The source-user argument in `events listen` must be `--source-user`.

**C4 — Creation uses `create`, not `add`.**  
`action add` → `action create`. All resource-creation commands use the same verb.

**C5 — Deletion uses `delete`, not an inverted binding verb.**  
`events unlisten` → `events delete`. The short description clarifies what is deleted and what side-effects occur (purges pending events).

**C6 — A command group's `list` subcommand lists that group's primary noun.**  
`events list` currently lists listeners, not events. Either rename it `events listeners`, or move it to a `listeners` sub-group. The rule: `juice X list` lists X objects.

**C7 — Zero-cost native actions do not require process provisioning.**  
`lookup` calls `@sys/lookup` at price 0. Requiring `--process` for a free operation creates unnecessary friction. The CLI starts and ends an ephemeral process internally.

**C8 — `stats show` lives under `action`, not at root level.**  
`juice stats show --action <id>` → `juice action stats --id <id>`. Stats are a property of an action; the command belongs in the `action` group.

---

## Operation Reference

Legend: `•` = issue exists (see Issues column); `—` = not exposed on that surface; `(admin)` = requires superuser.

### Server

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Health check | `GET /health` (open) | `juice health [--url]` | |
| Federation metadata | `GET /.well-known/juice-kernel.json` (open) | — | |

### Authentication

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Password login | `POST /v1/auth/token` JSON `{handle, password}` → `{token}` | `juice auth login --handle --password` | • HTTP1 |
| PKCE login | `POST /v1/auth/authorize` (form) + `POST /v1/auth/token` (form) | `juice auth login --handle --pkce --server` | |
| Refresh token | `POST /v1/auth/refresh` `{refresh_token}` → `{access_token, refresh_token}` | `juice auth refresh` | |
| Logout | `POST /v1/auth/logout` `{refresh_token}` → 204 | `juice auth logout` | |

### Users

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Create user | `POST /v1/users` → 201 `{id, handle, email, available, locked}` | `juice user create --handle --email [--password]` | • R1 |
| Get self | `GET /v1/me` → `{id, handle, email, available, locked}` | `juice user me` | |

### Actions

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Create action | `POST /v1/actions` → 201 action | `juice action add --name --kind [--source --description --price --input-schema --output-schema]` | • R1, C4 |
| List public actions | `GET /v1/actions[?owner=&name=]` → action[] | `juice action list [--all --limit --offset]` | • R1 |
| Show action | `GET /v1/actions/{id}` → action | `juice action show --id` | • R1 |
| Update action | `PUT /v1/actions/{id}` → updated action | `juice action update --id [--price --description --source --input-schema --output-schema]` | • R1 |
| Enable action | `POST /v1/actions/{id}/enable` → `{active:true}` | `juice action enable --id` | |
| Disable action | `POST /v1/actions/{id}/disable` → `{active:false}` | `juice action disable --id` | |
| Delete action | `DELETE /v1/actions/{id}` → 204 | `juice action delete --id` | |
| Grant ACL | `POST /v1/actions/{id}/acl` `{subject_user_id, permission}` → 204 | `juice action acl grant --action <id> --user --perm` | • R4, R7, C2 |
| Revoke ACL | `DELETE /v1/actions/{id}/acl` (body: `{subject_user_id, permission}`) → 204 | `juice action acl revoke --action <id> --user --perm` | • R4, C2 |
| Grant-all (make public) | `POST /v1/actions/{id}/grant-all` → 204 | `juice action grant-all --id` | |
| Revoke-all (make private) | `POST /v1/actions/{id}/revoke-all` → 204 | `juice action revoke-all --id` | |
| Import OpenAPI | `POST /v1/actions/import` `{spec_url}` → import result | `juice action import --openapi <url>` | |
| Unimport OpenAPI | `POST /v1/actions/unimport` `{spec_url[, name]}` → action[] | `juice action unimport --openapi <url> [--name]` | |
| Get manifest | `GET /v1/actions/{id}/manifest` → signed manifest | — (used internally by `remote import`) | |
| List ratings | `GET /v1/actions/{id}/ratings` → rating[] | — | |

### Processes

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Start process | `POST /v1/processes` `{funds}` → 201 `{process_id, trace_id, available}` | `juice process start [--funds]` | • R1 |
| List processes | `GET /v1/processes` → process[] | `juice process list` | • R1 |
| Show process | `GET /v1/processes/{id}` → process | `juice process show --id` | • R1 |
| Fund process | `POST /v1/processes/{id}/fund` `{funds}` → 204 | `juice process fund --id --funds` | • R1, R5 |
| End process | `POST /v1/processes/{id}/end` → 204 | `juice process end --id` | • R1 |

### Call

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Execute action | `POST /v1/call` `{process_id, action, args[, parent_trace_id]}` → `{result, tx_id, trace_id}` | `juice call --process --target --action [--trace --args]` | • R3, C2 |

### Transactions

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| List transactions | `GET /v1/transactions[?process_id=]` → transaction[] | `juice tx list [--process --limit --offset]` | • R1, R2 |
| Show transaction | `GET /v1/transactions/{id}` → transaction | `juice tx show --id` | • R1, R2 |
| Rate transaction | `POST /v1/transactions/{id}/rate` `{rating}` → rating | `juice tx rate --id --rating` | • R7 |

### Stats

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Show action stats | `GET /v1/stats/{action_id}` → stats | `juice stats show --action <id>` | • C8 |

### Listeners and Events

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Create listener | `POST /v1/listeners` `{source_user_id, event_name, target_action_id}` → 201 listener | `juice events listen --source <handle> --event --action <id>` | • R1, C2, C3 |
| List listeners | `GET /v1/listeners` → listener[] | `juice events list` | • R1, C6 |
| Show listener | `GET /v1/listeners/{id}` → listener | — | • R1 |
| Delete listener | `DELETE /v1/listeners/{id}` → 204 | `juice events unlisten --id` | • R1, C5 |
| Poll events | `GET /v1/listeners/{id}/events` → `{listener_id, events:[]}` | `juice events poll --id` | • R1, R6 |
| Emit event | `POST /v1/events/emit` `{event_name, args}` → `{event_ids:[]}` | `juice events emit --event --args` | |
| Consume event | `POST /v1/events/{id}/consume` `{process_id[, parent_trace_id]}` → call reply | `juice events consume --id --process [--trace]` | |

### Lookup

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Lookup actions | Routed through `POST /v1/call` to `@sys/lookup` | `juice lookup --query [--limit] --process` | • C7 |

### Remote Kernels

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Add remote kernel | — (fetches remote well-known) | `juice remote add <url>` | • C1 |
| List remote kernels | — | `juice remote list` | |
| Import remote action | — (fetches remote manifest) | `juice remote import <handle> <action-name>` | • C1 |
| Unimport remote action | — | `juice remote unimport <handle> <action-name>` | • C1 |

### Federation (HTTP-only)

| Operation | HTTP | CLI | Issues |
|-----------|------|-----|--------|
| Inbound federation call | `POST /v1/federation/call?action=@owner/name&counterparty=<pubkey>` | — | |

### Admin (CLI-only)

| Operation | CLI | Issues |
|-----------|-----|--------|
| List all users | `juice admin user list [--limit --offset]` | |
| Show user | `juice admin user show --id\|--handle` | |
| Suspend user | `juice admin user suspend --id` | |
| Unsuspend user | `juice admin user unsuspend --id` | |
| Deposit credits | `juice admin user deposit --id\|--handle --amount [--reason]` | |
| List all actions | `juice admin action list [--limit --offset]` | |
| Disable action | `juice admin action disable --id` | |
| List all processes | `juice admin process list [--limit --offset]` | |
| List all transactions | `juice admin tx list [--limit --offset]` | |

---

## Inconsistency Index

**HTTP1 — Dual content-type on `POST /v1/auth/token`.**  
The endpoint detects JSON vs. `application/x-www-form-urlencoded` at runtime. Password grant (JSON) and authorization-code grant (form) share one route. Split into two routes or require a consistent envelope.  
*Proposed:* `POST /v1/auth/token` accepts JSON only: `{grant_type, handle, password}` for password grant; `{grant_type, code, code_verifier, redirect_uri}` for code exchange.

**R1 — Missing JSON struct tags on core types.**  
`Action`, `Process`, `Transaction`, `Listener`, `Event`, and `Trace` have no `json:` tags. They serialize with Go PascalCase field names (`OwnerUserID`, `ArgsJSON`, `ProcessID`) while `Stats`, `Receipt`, `Rating`, and `ActionManifest` use snake_case. Every external client receives an inconsistent wire format.  
*Proposed:* Add `json:"snake_case"` tags to all fields on all exported types used in HTTP responses.

**R2 — `Transaction.ArgsJSON` and `Transaction.ReplyJSON` are double-encoded.**  
Both fields are `string`; they hold serialized JSON. A client receives `"args_json": "{\"query\":\"hello\"}"` and must parse twice. The `CallReply` response already uses `result map[string]any` correctly.  
*Proposed:* Change both fields to `json.RawMessage`, retag as `"args"` and `"result"`, and update all store reads/writes accordingly.

**R3 — `POST /v1/call` splits action identity across `target` + `action_name`.**  
`{"target": "@alice", "action_name": "/weather"}` is two fields for one concept. The rest of the system (manifests, lookup results, federation endpoint) uses `@owner/name` as a unit.  
*Proposed:* `{"action": "@alice/weather", "process_id": "...", "args": {}}`. The server splits on the first `/` after the handle prefix.  
*CLI:* `juice call --action @alice/weather --process ... [--args --trace]`. Remove `--target`; `--action` takes `@owner/name`.

**R4 — `DELETE /v1/actions/{id}/acl` carries a request body.**  
Many HTTP clients and intermediaries drop or reject DELETE bodies. The permission and subject are part of the resource identity, not state being submitted.  
*Proposed:* `DELETE /v1/actions/{id}/acl/{subject_id}/{permission}` — no body.

**R5 — `POST /v1/processes/{id}/fund` returns 204.**  
After funding, the caller immediately needs the new balance. A second `GET /v1/processes/{id}` round-trip is required.  
*Proposed:* Return 200 with the updated `Process` object.

**R6 — `GET /v1/listeners/{id}/events` wraps response in an envelope.**  
Returns `{"listener_id": "...", "events": [...]}`. The listener ID is already in the URL path.  
*Proposed:* Return the array directly: `[…]`.

**R7 — Rating and permission inputs not validated at the HTTP boundary.**  
`POST /v1/transactions/{id}/rate` accepts any `float64`; the domain requires `{0, 1}`. `POST /v1/actions/{id}/acl` passes `permission` directly to the kernel, where an unknown value surfaces as `ErrInternal` after a SQLite constraint error.  
*Proposed:* Handler-level guards: `if rating != 0 && rating != 1 → 422`; `if perm ∉ {read, call, admin} → 422`.  
*(Note: the permission validation was added to the kernel in a recent fix; the HTTP handler should redundantly validate before reaching the kernel.)*

**C1 — `remote` commands use positional arguments.**  
`remote add <url>`, `remote import <handle> <action-name>`, `remote unimport <handle> <action-name>` are the only commands in the CLI that take bare positional arguments.  
*Proposed:*
```
juice remote add --url <url>
juice remote import --remote <handle> --action @handle/name
juice remote unimport --remote <handle> --action @handle/name
```

**C2 — `--action` carries a UUID in some commands, a name in others.**  
In `action acl grant/revoke` and `events listen`, `--action` is an action UUID. In `call`, `--action` is an action name. Same flag, different types, in the same binary.  
*Proposed:* `--action` always carries `@owner/name`. `action acl grant/revoke` use `--id` (consistent with every other `action` subcommand). `events listen` resolves `@owner/name` to an ID internally.

**C3 — `--source` in `events listen` means a user handle.**  
`events listen --source @alice` conflicts with `action add --source ./my.wasm` where `--source` is a URL/file path.  
*Proposed:* Rename to `--source-user` in `events listen`.

**C4 — `action add` uses `add`, not `create`.**  
`user create`, `process start`, `events listen` (creates a listener) all use distinct verbs. `action` alone uses `add`.  
*Proposed:* `juice action create` (rename `add` → `create`).

**C5 — `events unlisten` is an invented verb.**  
`delete`, `remove`, and `revoke` are standard; `unlisten` is not. It also hides that the operation purges all pending events.  
*Proposed:* `juice events delete --id <listener-id>`.

**C6 — `events list` lists listeners, not events.**  
`juice events list` calls `ListListeners` and prints listener rows. A user reading the command tree expects `events list` to list events.  
*Proposed:* Rename to `juice events listeners`. The `list` subcommand within `events` should list events (pending event queue view), if added later.

**C7 — `lookup` requires `--process` for a zero-cost action.**  
`@sys/lookup` has price 0. The kernel requires a process for `Call()`, but the CLI can start and end a zero-funded ephemeral process transparently.  
*Proposed:* Drop `--process` from `lookup`. The CLI creates a throw-away process with `funds=0`, runs the call, and ends it.

**C8 — `stats show` is a root-level command group.**  
`juice stats show --action <id>` sits at the root. Stats are a property of an action; the command belongs under `juice action`.  
*Proposed:* `juice action stats --id <action-id>`. Remove the `stats` root group.
