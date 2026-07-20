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
No envelope objects. `GET /v1/steps` returns `[…]` directly. Metadata such as pagination belongs in response headers, not the body. Every list endpoint pages by `limit`/`offset` query params — default `limit` 50, ceiling 200, `offset` floored at 0 — so no single response is unbounded.

**R7 — Input validated at the HTTP boundary.**  
The handler rejects invalid inputs before calling the kernel. `rating` must be 0 or 1; returns `ErrInvalidInput` when violated.

**R8 — Action responses include both `id` and `action`.**  
Every read or list response for an action resource includes both `id` (UUID, for management operations) and a computed `action` field containing `@owner/name` (for running). Clients can copy the `action` value directly into run requests without a separate lookup. For `kind=http` actions, responses also include a decomposed `http` object `{method, url, params}` — identical in shape for manually-created and OpenAPI-imported actions — that round-trips with the `source`/`method`/`param` create and update inputs. Responses also carry the action's non-secret auth summary: `requires_grant` (always present — `true` iff a caller must connect a per-caller credential before calling, i.e. a delegated scheme) and, when the action has upstream auth, `auth_scheme` (the scheme name, e.g. `delegated_bearer`). These are the scheme and flag only, never config or secrets (R9). A `kind=remote_proxy` action additionally carries `peer_state` — `offline` (the owner peer's peer-sync is missing or stale) or `unfunded` (this kernel's cached credit on the peer is below the action's price), omitted when healthy — a display-only hint from the §13 peer-sync cache that never gates a call.

**R9 — Secrets never serialize.**  
Auth configs (`Action.source` upstream credentials) are write-only: accepted on create and update, never present in any read, list, log, receipt, hash, or manifest response. There is no read path for a stored secret. The non-secret `auth_scheme` name and `requires_grant` flag (R8) are not secrets and are exposed; the `config` and `secrets` of an auth are never returned.

### CLI

**C1 — A command's primary identifier is a positional argument.**  
The thing a command acts on is positional, not a flag. A second mandatory value (amount, rating) is the second positional. Only optional inputs use `--flag` style.

**C2 — Positional identifiers are natural keys.**  
A user is `@handle`, a public key, or a raw id — the shapes are disjoint, so one resolver disambiguates. An action is `@owner/name` (a raw id is also accepted). Processes, steps, and transactions, which have no human-readable name, are ids. Outputs still render users as `@handle`, never a raw id.

**C3 — `--source` is reserved for URLs and file paths.**  
`--source` is used for action source URLs and script paths (`action create --source`). For `kind=http`, `--method` sets the verb (default `POST`; GET/POST/PUT/PATCH/DELETE) and the repeatable `--param name:in` (`in` = `path`/`query`/`body`) binds input fields; omitting `--param` uses implicit routing (`{name}` placeholders in the URL become path params, remaining args go to the query for GET/DELETE or the JSON body otherwise). User-handle inputs use descriptively named flags: `--required-caller @handle` in `step create`.

**C4 — Creation uses `create`.**  
All resource-creation commands use `create`: `user create`, `action create`, `step create`. Processes are not created by users — `run` creates them (see Run).

**C5 — Deletion uses `delete`.**  
Deletion commands use `delete`. Side effects of deletion are documented in the command description.

**C6 — A command group's `list` subcommand lists that group's primary noun.**  
`juice step list` lists steps. Optional filters (`--process`, `--status`) narrow the result, and `--limit`/`--offset` page it, without changing the command group.

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

**C13 — All commands are TCP clients; `admin` verbs are superuser-gated routes.**  
Every command runs by calling the server over HTTP; the base URL resolves from `--server`, then `JUICE_SERVER`, then `server_url` in config, defaulting to `http://localhost:4040`. `admin *` are superuser supervision served on that same public TCP API, on routes gated by an `IsSuperuser` check — authority is the `@sys` bearer token (keep it secret; expose `serve` only behind TLS or on loopback), not a separate socket or filesystem access. `juice serve` is the sole process that opens the database. Supervision over ordinary resources is **scope**, not a separate surface: a superuser sees all rows on `action/process/tx/step list` and may `action disable`/`enable` any action, all over the normal TCP API.

---

## Operation Reference

### Server

| Operation | HTTP | CLI |
|-----------|------|-----|
| Health check | `GET /health` (open) → `{status, handle, public_key}`; identity banner — see which kernel you're on before login | `juice health` |

Federation has no HTTP surface: peer identity, gossip, manifests, and inbound calls travel over the cross-kernel transport (§13), not over this API. The known network (discovered kernels) is seeded automatically at `serve` from the bootstrap peers and grown by DHT provider-record enumeration; `admin peers --gossip` renders it as a grouped roster (kernel → introducer → action, with your own earned stats shown distinctly), and a remote kernel is inspected with `admin inspect <key>`.

### Authentication

| Operation | HTTP | CLI |
|-----------|------|-----|
| Password login | `POST /v1/auth/token` `{handle, password}` → `{token}` | `juice auth login <user> [--password]` |
| PKCE authorize | `POST /v1/auth/authorize` `{handle, password, code_challenge, [redirect_uri]}` → `302` if `redirect_uri` provided, else `200 {"redirect":"?code=CODE"}` | `juice auth login <user> --pkce --server <url>` |
| PKCE token exchange | `POST /v1/auth/token` `{grant_type:"authorization_code", code, code_verifier, [redirect_uri]}` → `{access_token, refresh_token}` | (handled internally by `--pkce` login) |
| Refresh token | `POST /v1/auth/refresh` `{refresh_token}` → `{access_token, refresh_token}` | `juice auth refresh` |
| Logout | `POST /v1/auth/logout` `{refresh_token}` → 204 | `juice auth logout` |
| Recover (start) | `POST /v1/auth/recover/start` `{handle}` → `{nonce, expires_in_seconds}` | (part of `juice auth recover`) |
| Recover (complete) | `POST /v1/auth/recover/complete` `{handle, nonce, signature, password}` → `{status}` | `juice auth recover <user> [--phrase] [--password]` |

`signature` is `base64url(Ed25519-sign(priv, JCS({"recovery_challenge": nonce})))`, where `priv = Ed25519 from seed[:32]`, `seed = BIP-39(phrase)`; `recovery_public_key` is `base64url(pub)`.

### Users

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create user | `POST /v1/users` `{handle, password, [recovery_public_key]}` → 201 user | `juice user create <user> [--password]` |
| Get self | `GET /v1/me` → user (`id`, `handle`, `description`, `available`, `locked`, `connectors` the directory-grouped tree each `{directory, connections, actions:[{action, scopes, provider_key, created_at}]}`, `connections` the account inventory each `{provider, actions, unused, provider_key, created_at}`) | `juice user me` |
| Update self | `PUT /v1/me` `{[description], [current_password, password]}` → user | `juice user update [--description] [--password]` |
| Transfer credits | `POST /v1/transfers` `{recipient, amount, [reason], [external_key]}` → ledger entry | `juice user transfer <recipient> <amount> [--reason --external-key]` |
| List ledger | `GET /v1/ledger[?limit=&offset=]` → ledger entry[] | `juice user ledger [--limit --offset]` |

`handle` is immutable. `description` and `password` are updatable by the authenticated user; `password` change requires `current_password` to verify the existing credential. At least one of `description` or `password` must be provided (`description` may be `""` to clear). There is no email; account recovery is by seed phrase (§12): `user create` generates a 12-word BIP-39 mnemonic client-side, sends only the derived `recovery_public_key`, and prints the phrase once; `juice auth recover <user>` resets a lost password by signing a server nonce with the phrase-derived key. Proxy users (federation peers) cannot be created here, cannot log in, and hold no tokens; they exist only through peer acceptance, authenticate per request by federation signature, and cannot use `PUT /v1/me`.

`user transfer` debits the caller and credits a local recipient in one fee-free ledger entry (rejects self-transfer, non-positive amount, and a suspended or peer recipient; `insufficient_funds` on low balance). `GET /v1/ledger` lists the caller's own movements — deposits, withdrawals, transfers — newest first, paginated, each with `operator_handle` plus `from_handle`/`to_handle` (null side omitted).

### Grants and connections (delegated auth)

| Operation | HTTP | CLI |
|-----------|------|-----|
| Plan consent | `GET /v1/grants/plan?selector=` → `{groups: [{provider, scheme, scopes, destinations, connected, covered, actions: [{action, granted}]}], skipped}` | (driven by `user connect`) |
| Start consent | `POST /v1/grants/start` `{selector, provider, [redirect_uri], [flow]}` → `{status:"granted", actions}` (already covered) or `{state, authorize_url}` (code) or `{state, verification_uri, user_code, interval, expires_in}` (device) | `juice user connect <selector> [--device]` |
| Complete consent | `POST /v1/grants/complete` `{state, [code]}` → `{status, provider, actions, created_at}` or `{status: "pending"}` | (driven by `user connect`) |
| Attach token | `POST /v1/grants` `{selector, [provider], token}` → `{status, provider, actions, created_at}` | `juice user connect <selector> --token <pat>` |
| Disconnect grants | `DELETE /v1/grants?selector=` → `{revoked: [actions]}` (legacy `?action=` accepted) | `juice user disconnect <selector>` |
| Disconnect account | `DELETE /v1/grants?account=<provider_key>` → `{revoked: [actions], connection}` | `juice user disconnect --account <provider>` |

A `Grant` is per-action consent (§8): a pointer binding one action to a `Connection` — the caller's upstream account credential, stored once per `(user, provider)` and shared by every grant that points at it. A **selector** (`@owner`, `@owner/path`, or a full `@owner/name`; path-segment matched, trailing `/*` stripped) names a set of delegated actions; `user connect` fetches the plan, groups them by upstream account, shows the delta, and covers each group with one gesture: one browser consent (requesting the union of the group's scopes) for `oauth_delegated`, or one token paste for `delegated_bearer`. Connecting an action whose account is already connected with covering scopes grants instantly with no browser. For `oauth_delegated` the client hosts the redirect target — a loopback listener for local clients (CLI/desktop), a registered callback for hosted ones — and the server holds only in-memory PKCE/device state and performs the token exchange, so the refresh token never transits the client. The CLI's `--token` reads without echo when the flag value is empty (keeping it out of shell history). Running an action that lacks a grant returns `grant_required` (403) with the action in `meta`; the CLI (`juice run`) offers consent inline on an interactive terminal and otherwise prints a `juice user connect <directory>` hint. Grants and connections are listed token-free in `GET /v1/me` and are never otherwise readable. `/v1/me` returns them as a **directory-grouped tree** under `connectors`: one node per folder (`directory` = the granted actions' shared folder, the ref up to its last `/`, e.g. `@chat` or `@chat/inbox`), each carrying the `connections` that back it (usually one) and the `actions` granted under it. A separate top-level `connections` array is the full account inventory, so a connection with zero grants still surfaces with `unused: true`. Each action and its backing connection carry the same opaque `provider_key`; the same value addresses `DELETE /v1/grants?account=`. Treat it as opaque — never parse it (`provider` is the display label). The `directory` is a display grouping only — it never gates a credential (the token binding stays per-action and fact-derived, §8), so grouping by it is safe. `provider_key` is omitted on an unbackfilled legacy grant that has no connection.

### Actions

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create action | `POST /v1/actions` `{name, kind, [source, wasm_artifact, method, params, description, price, input_schema, output_schema, auth]}` → 201 action | `juice action create <name> --kind [--source --artifact --method --param --description --price --input-schema --output-schema --auth]` |
| List actions | `GET /v1/actions[?owner=&name=&all=&limit=&offset=]` → action[]; active-only by default (unauthenticated → active public; authenticated → active public + local + own active; superuser → all owners' active); `?all=1` includes inactive/private in scope; `?owner=`/`?name=` filter | `juice action list [--all --limit --offset]` |
| Show action | `GET /v1/actions/{id}` → action | `juice action show <action>` |
| Update action | `PUT /v1/actions/{id}` `{[price, description, source, wasm_artifact, method, params, input_schema, output_schema, visibility, auth]}` → action | `juice action update <action> [--price --description --source --artifact --method --param --input-schema --output-schema --visibility --auth]` |
| Enable action | `POST /v1/actions/{id}/enable` → `{active:true}` | `juice action enable <action>` |
| Disable action | `POST /v1/actions/{id}/disable` → `{active:false}` | `juice action disable <action>` |
| Delete action | `DELETE /v1/actions/{id}` → 204 | `juice action delete <action>` |
| Import OpenAPI | `POST /v1/actions/import` `{spec_url}` → import result | `juice action import <spec-url>` |
| Unimport OpenAPI | `POST /v1/actions/unimport` `{spec_url[, name]}` → action[] | `juice action unimport <spec-url> [--name]` |
| Get stats | `GET /v1/stats/{action_id}` → stats | `juice action stats <action>` |
| List ratings | `GET /v1/actions/{id}/ratings[?limit=&offset=]` → rating[] | — |

`<action>` is `@owner/name` (a raw id is also accepted). Action responses (show and list) include a computed `action` field (`@owner/name`) alongside `id`, plus the full `input_schema` and `output_schema` — the CLI text view shows the same fields the JSON returns. `visibility` is `private` (owner only), `local` (any local caller of this kernel, never peers), or `public` (anyone, and the only value served in manifests/gossip, §13); it is caller-scoped callability (requirements.md §4), settable only via update, and defaults to `private`. Widening beyond `private` on an OpenAPI-imported action requires verified ownership. `price` is the subtree bound: the maximum total cost of the action and everything it calls. `auth` is the upstream credential config `{scheme, config, secrets}` (R9: write-only, never returned); reads instead expose only its non-secret summary — `auth_scheme` (scheme name, when present) and `requires_grant` (R8).

The `auth` object is `{scheme, config, secrets}`; valid schemes and their keys (semantics in requirements.md §8):

| `scheme` | `config` | `secrets` | per-caller credential |
|----------|----------|-----------|-----------------------|
| `header` | `name` | `value` | — |
| `query` | `name` | `value` | — |
| `bearer` | — | `token` | — |
| `basic` | — | `username`, `password` | — |
| `oauth_client_credentials` | `token_url`, `client_id` | `client_secret` | — |
| `oauth_jwt_bearer` | `token_url`, `client_id` | `private_key` | — |
| `oauth_delegated` | `auth_url`, `token_url`, `client_id`, opt. `device_auth_url`, `scopes`, `client_secret` | — | `user connect` |
| `delegated_bearer` | opt. `header`, `template` (default `Authorization` / `Bearer {token}`) | — | `user connect --token` |

The per-caller schemes hold no secret in `auth`; each caller supplies their credential through the Grants surface above, and a call with no grant returns `grant_required` (403). Actions imported from OpenAPI carry no `auth`: set it via `PUT /v1/actions/{id}` before activating.

### Run

| Operation | HTTP | CLI |
|-----------|------|-----|
| Run action | `POST /v1/run` `{action, args}` → `{result, tx_id, trace_id, process_id}` | `juice run <action> [json]` |

`run` is the single execution entry point: it atomically creates a process funded with exactly the action's price (parked from the caller's available balance), issues the root call, and the process closes itself when the root call has returned and no steps remain outstanding. A remote-proxy `run` may fail with a federation-relationship error distinct from the caller's own funds: `peer_unreachable` (502) — the peer was offline and the request provably never left this kernel, so the call was refunded rather than parked (retry when it is back); or `peer_unfunded` (402, `meta.peer` names the peer) — this kernel's prepaid credit on the peer is exhausted, an operator top-up condition, never the caller's balance (§13). There is no process handle to manage and no funding amount to choose. `args` defaults to `{}` when omitted. Zero-price actions (e.g. `@sys/lookup`) run with zero funding — no special handling.

**Composition from a `kind=http` action (capability, §9).** When the kernel dispatches an HTTP action it sends the endpoint two headers: `X-Juice-Callback` (this kernel's callback base URL) and `X-Juice-Capability` (a signed token naming the call's live trace). While the call is in flight the endpoint composes by calling back with `X-Juice-Capability: <token>` instead of a bearer JWT: `POST /v1/call` `{action, args}` → `{result, tx_id, trace_id}` is a subcall on that trace (the HTTP twin of `juice.call`, with no `run`/wallet path); `POST /v1/steps` and `POST /v1/steps/{id}/complete` accept the same capability, the trace coming from the token (send no `trace_id`). Composition runs as the executing action's owner within its trace, funded from it and bounded by the action's advertised price — a leaf endpoint just ignores the headers. `/v1/call` is not a user-invocable command (there is no CLI for it; the capability is minted per dispatch and dies when the call settles).

### Processes

| Operation | HTTP | CLI |
|-----------|------|-----|
| List processes | `GET /v1/processes[?limit=&offset=]` → process[]; own processes, or **all for a superuser** | `juice process list [--limit --offset]` |
| Show process | `GET /v1/processes/{id}` → process (`owner_handle`, `available`, `locked`, `status`, `awaiting_receipt`, `awaiting_receipt_since?`) | `juice process show <id>` |
| End process | `POST /v1/processes/{id}/end` → 204 | `juice process end <id>` |

Processes are created only by `run` and close automatically. `end` is the forced abort: it fails running calls, cancels waiting steps with their parked prices refunded, and returns remaining funds to the owner. An in-flight remote call is not force-failed by restart recovery and settles on its receipt.

### Transactions

| Operation | HTTP | CLI |
|-----------|------|-----|
| List transactions | `GET /v1/transactions[?process_id=&limit=&offset=]` → transaction[] | `juice tx list [--process --limit --offset]` |
| Show transaction | `GET /v1/transactions/{id}` → transaction | `juice tx show <id>` |
| Rate transaction | `POST /v1/transactions/{id}/rate` `{rating, note?}` → rating | `juice tx rate <id> <0\|1> [--note]` |
| Verify remote receipt | `GET /v1/transactions/{id}/receipt-verification` → verification | `juice tx verify <id>` |

A caller reads transactions where it is a captured party: payer, caller, or payee; a superuser reads all. Responses render each party as a `@handle` — `owner_handle` (payer), `caller_handle` (caller), `target_handle` (payee); the raw `*_user_id` UUIDs are not returned, since a user is addressed by handle, never an id (a purged party falls back to its raw id). `rating` must be 0 (bad) or 1 (good); `note` is an optional string. Transaction and list responses include a `rating` field — `{"value": 0|1, "note": string|null}` when rated, `null` when unrated. Transaction `args` and `result` fields are inline JSON objects. `gross` is the call's full allocation; `fee + net` is the value added paid out at settlement.

Remote-proxy transactions include `remote_receipt_hash` and `remote_receipt_json`. `receipt-verification` verifies entirely from local data, in two parts: **receipt integrity** (signature against the peer's public key, stored JSON against its stored hash, `action_id` against the proxy) and **settlement consistency** (local outcome matches `receipt.status`; payment to the proxy user equals `receipt.charge`; the local refund arithmetic checks out). The receipt's own `gross/net/fee` are the remote kernel's economics and are reported, not compared. Returns `ErrInvalidState` for non-remote-proxy transactions.

### Steps

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create step | `POST /v1/steps` `{trace_id, action_id, partial_args, required_caller}` → 201 step | `juice step create <action> --trace --required-caller <user> [--partial-args]` |
| List steps | `GET /v1/steps[?process_id=&status=&limit=&offset=]` → step[]; each carries `created_by` (the creating action `@owner/name`, from the parent trace) alongside `action` (the completion target) | `juice step list [--process --status --limit --offset]` |
| Show step | `GET /v1/steps/{id}` → step (incl. `created_by`) | `juice step show <id>` |
| Complete step | `POST /v1/steps/{id}/complete` `{args}` → `{result, tx_id, trace_id, step_id}` | `juice step complete <id> [json]` |

A step is a funded continuation: creation snapshots the action's price as `step.price` and parks it from `trace_id`; the process is derived from `Trace(trace_id).process_id`. Completion spends the parked price — no funds check occurs, and the completion's allocation and transaction `gross` are `step.price`. `required_caller` is a `@handle`; the server resolves it to `required_caller_user_id`. Step listing returns the caller's own steps (as process owner or required caller), or all of them for a superuser. `status` filter accepts `waiting`, `running`, `done`, or `cancelled`. The `args` field in the complete request is merged with the step's `partial_args` (completion `args` overwrites on key collision); the allowed completion input is derived as `action.input_schema \ keys(partial_args)` — a violation rejects the completion and leaves the step `waiting`, never recorded as an action failure. Step read and list responses include `price`, `action_id`, a computed `action` field (`@owner/name`), `owner_handle` (the process owner / payer as a `@handle`, mirroring a transaction's `owner_handle` — the step settles into that owner's transaction), `required_caller_handle` (the required caller as a `@handle`, not the raw `required_caller_user_id`), `waiting_on_peer` on a waiting step whose required caller is a peer, and `allowed_input` on a waiting step — the derived completion schema (`action.input_schema \ keys(partial_args)`) so the required caller can complete it without separately reading the target action (which, when private, they may not be able to read). An outstanding step keeps its process open.

### System Actions

Native actions registered at bootstrap, owned by `@sys`, public, runnable like any other action — `juice run @sys/lookup '{"query":"…"}'`. `@sys/lookup` results carry `action_id`, `action` (`@owner/name`), `description`, `score`, `input_schema`, and `output_schema`. `score` is a dimensionless relevance-times-quality rank score (lexical and semantic legs fused by reciprocal-rank fusion, weighted by stats); it is comparable only for ordering within one response, not across queries or as a probability:

| Action | Price | Purpose |
|--------|-------|---------|
| `@sys/lookup` | 0 | Rank active actions by query |
| `@sys/llm/chat` | 0 | Platform LLM chat |
| `@sys/llm/embed` | 0 | Text embedding vector |
| `@sys/llm/json`  | 0 | Structured JSON output from LLM, locally validated |
| `@sys/llm/decide` | 0 | LLM-driven action selection; returns chosen action and args without executing |
| `@sys/time` | 0 | Current time |
| `@sys/sink` | 0 | Universal no-op step target |
| `@sys/message` | 0 | Message a user by creating a step they acknowledge |
| `@sys/random` | 0 | Random float in `[0, 1)` |
| `@sys/web` | 0 | Fetch a public web page (read-only GET) |
| `@sys/tinygo/compile` | 5 (configurable, `native.tinygo`) | Compile TinyGo source to a WASM artifact |

### Federation

Subscribing to a peer imports its actions owner-qualified, so a peer action is addressed `@peer/owner/name` and is called through `POST /v1/run` like any local action; there are no federation HTTP endpoints. Imported proxies are enabled with `visibility = local`, so a kernel exposes only its **own** `public` actions to peers — a peer's own imports are never re-advertised *and* an inbound peer call to one is denied by `CanCall` (a peer caller fails the `local` branch, §4), so subscription is non-transitive at both the manifest and the call layer (reach a peer's imported action by subscribing to its true owner). Trust and peering are managed via the admin commands below. The cross-kernel transport is an implementation detail (§13).

### Admin (superuser-gated TCP routes)

The operator verbs no ordinary user performs — money, access, federation trust, and the global roster. Served on the public TCP API, on routes gated by an `IsSuperuser` check (authority is the `@sys` bearer token). Everything else a superuser does (see all actions/processes/txs/steps, disable any action) is *scope* on the normal commands above, not an admin command.

| Operation | CLI |
|-----------|-----|
| List all users | `juice admin users [--limit --offset]` |
| Show user | `juice admin show <user>` |
| Suspend account | `juice admin suspend <user\|key>` — freezes any account: a human cannot log in; a peer's inbound calls are refused with a signed rejection |
| Unsuspend account | `juice admin unsuspend <user\|key>` |
| Rename handle | `juice admin rename <user\|key> <new-handle>` — frees the old handle for reuse; also renames a peer's local mount |
| Deposit credits | `juice admin deposit <user\|key> <amount> [--reason --external-key]` |
| Withdraw credits | `juice admin withdraw <user\|key> <amount> [--reason --external-key]` |
| Subscribe to a kernel | `juice admin subscribe <key>` — imports the peer's active public actions, mounted under its self-reported handle (auto-suffixed on collision); rename the mount with `admin rename <key> <new-handle>`. Calls stay rejected until the peer holds credit here — fund it with `admin deposit <key> <amount>` |
| Unsubscribe from a kernel | `juice admin unsubscribe <user\|key>` — deactivates that peer's imported proxies here; balance and history survive |
| List peers | `juice admin peers [--gossip] [--all]` — active peers by default (`handle`, `public_key`, `available`, `locked`, plus the §13 sync cache `peer_credit` (our credit on the peer) and `last_seen`; no internal id); `--all` (`?all=1`) also lists suspended peers; `--gossip` adds the known-network directory roster |
| Inspect a kernel | `juice admin inspect <key\|user>` — identity (incl. its `about`), public actions (with descriptions), transacted peers, reachability. `source` is `live`/`local`/`none`: an offline but known peer degrades to last-known local data (`online:false`) |
| Show own identity | `juice admin identity` — this kernel's public key, handle, `about` (`@sys`'s description), listen addresses |

`<user>` is a `@handle` — or, for a peer, its base64url public key (the global name); `<action>` is `@owner/name` (or an id); `<key>` is a peer's base64url public key. `withdraw` requires `target.available ≥ amount`; it redeems credits and obliges the out-of-band payout. `admin deposit <key>` on a not-yet-known peer both provisions its billing account and funds it (subscription is handshake-free); the `about` shown by `identity`/`inspect` is `@sys`'s user description, set with `juice user update --description`. `admin unsubscribe` deactivates the peer's imported proxies and preserves balance and history; blocking a peer's inbound calls is `admin suspend <key>` (reversible with `unsuspend`). A peer account holds no session token, so a step whose required caller is a peer is completed by this kernel's operator with `juice step complete <id> --peer <key>` (superuser scope on the ordinary command); `admin inspect <key>` lists what a peer holds for us. Both are refused for a suspended peer.