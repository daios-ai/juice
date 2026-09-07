# API Reference

## Design Rules

These rules define the consistent standard every operation must satisfy.

### HTTP

**R1 — Snake_case field names everywhere.**  
Every type exposed over HTTP carries `json:` struct tags with snake_case names. Go field names (`OwnerUserID`, `ArgsJSON`, `ProcessID`) never appear on the wire. `PasswordHash` is masked with `json:"-"`.

**R2 — No double-encoded JSON.**  
Fields that contain structured data use `json.RawMessage`, never `string`. `Transaction.ArgsJSON` and `Transaction.ReplyJSON` appear as inline objects tagged `"args"` and `"result"` respectively, matching `RunReply`.

**R3 — Action identity is `owner/name`.**  
Wherever a request identifies a callable action, a single `action` field carries the combined `owner/name` notation. The server resolves it. Split fields (`target` + `action_name`) are forbidden. Owner handles must not contain `/`; action names may (`bob/mail/inbox`), which is what makes directory selectors possible (D10). A reference resolves the exact action it names, else that path's `index` child — the web's index-page convention — at every depth including zero: `bob` reaches `bob/index`, `bob@kernel` reaches `bob@kernel/index`, `acme/mail` reaches `acme/mail/index`. `index` is an ordinary action; there is no application object. Parsing is unambiguous: split on the first `/` after `@` — the owner is that first segment, the name is everything after it. When `owner/name` appears in a URL query string, `/` must be percent-encoded.

**R4 — DELETE never carries a request body.**  
Sub-resource removal uses path parameters. Request bodies on DELETE are rejected by many proxies and clients.

**R5 — State mutations return the updated resource.**  
Any operation that changes resource state returns the new state as the response body. The action mutation endpoints (`POST /v1/actions/enable`, `PUT /v1/actions`, `DELETE /v1/actions`) return the rows they wrote, in name order, since one target may name a whole subtree. Other purely destructive operations (`POST .../end`) return 204.

**R6 — List responses are plain arrays.**  
No envelope objects. `GET /v1/steps` returns `[…]` directly. Metadata such as pagination belongs in response headers, not the body. Every list endpoint pages by `limit`/`offset` query params — default `limit` 50, ceiling 200, `offset` floored at 0 — so no single response is unbounded.

**R7 — Input validated at the HTTP boundary.**  
The handler rejects invalid inputs before calling the kernel. `rating` must be 0 or 1; returns `ErrInvalidInput` when violated.

**R8 — Action responses include both `id` and `action`.**  
Every read or list response for an action resource includes both `id` (UUID, for management operations) and a computed `action` field containing `owner/name` (for running). Clients can copy the `action` value directly into run requests without a separate lookup. For `kind=http` actions, responses include a decomposed `http` object `{method, url, params}` — identical in shape for manually-created and OpenAPI-imported actions — that round-trips with the `source`/`method`/`param` create and update inputs. That object is the read shape: `source` is a write-only input there, omitted from the response rather than repeated as an encoded string (R2). A `kind=wasm` action's detail read still carries its `source`, which is authored text. Responses also carry the action's non-secret auth summary: `requires_grant` (always present — `true` iff a caller must connect a per-caller credential before calling, i.e. a delegated scheme) and, when the action has upstream auth, `auth_scheme` (the scheme name, e.g. `delegated_bearer`). These are the scheme and flag only, never config or secrets (R9). Responses carry `effect` when the action declares one (today only `transfer`): a call to it moves value from the immediate caller's own balance on top of the price, so a client confirms the spend before running it. Absent on an ordinary action. Responses also carry `quote_hash`, a fingerprint of the quoted terms (P2): pin it as `quote_hash` on `POST /v1/run` and the call is refused with `ErrTermsChanged` (409) before any funds are locked if the terms have moved, `meta` carrying the current hash and price. Omitting it leaves behaviour unchanged.

**R9 — Secrets never serialize.**  
Auth configs (`Action.source` upstream credentials) are write-only: accepted on create and update, never present in any read, list, log, receipt, hash, or manifest response. There is no read path for a stored secret. The non-secret `auth_scheme` name and `requires_grant` flag (R8) are not secrets and are exposed; the `config` and `secrets` of an auth are never returned.

### CLI

**C1 — A command's primary identifier is a positional argument.**  
The thing a command acts on is positional, not a flag. A second mandatory value (amount, rating) is the second positional. Only optional inputs use `--flag` style.

**C2 — Positional identifiers are natural keys.**  
A user is `handle`, a public key, or a raw id — the shapes are disjoint, so one resolver disambiguates. An action is `owner/name` (a raw id is also accepted). Processes, steps, and transactions, which have no human-readable name, are ids. Outputs still render users as `handle`, never a raw id.

**C3 — `--source` and `--artifact` are reserved for URLs and file paths.**  
`--source` is used for action source URLs and script paths (`action create --source`). `--artifact` takes a pre-compiled WASM module: a path always names its bytes, which are base64-encoded for the wire exactly as `--source` treats a binary module; only a non-path value is used as literal base64. For `kind=http`, `--method` sets the verb (default `POST`; GET/POST/PUT/PATCH/DELETE) and the repeatable `--param name:in` (`in` = `path`/`query`/`body`) binds input fields; omitting `--param` uses implicit routing (`{name}` placeholders in the URL become path params, remaining args go to the query for GET/DELETE or the JSON body otherwise). User-handle inputs use descriptively named flags: `--required-caller handle` in `step create`.

**C4 — Creation uses `create`.**  
All resource-creation commands use `create`: `user create`, `action create`, `step create`. Processes are not created by users — `run` creates them (see Run).

**C5 — Deletion uses `delete`.**  
Deletion commands use `delete`. Side effects of deletion are documented in the command description.

**C6 — A command group's `list` subcommand lists that group's primary noun.**  
`juice step list` lists steps. Optional filters (`--process`, `--status`) narrow the result, and `--limit`/`--offset` page it, without changing the command group.

**C7 — `stats` lives under `action`.**  
`juice action stats <action>`. Stats are a property of an action; the command belongs in the `action` group.

**C8 — Two machine-readable output modes: `--json` and `--quiet`.**  
Default output is human-readable text that surfaces the same fields as the HTTP response. `--json` returns the canonical JSON matching the HTTP response body. `--quiet` prints only the primary resource ID — one per line for a list, nothing for a mutation that returns no resource — on every command, reads included, so output pipes into the next one. Both are global flags.

**C9 — `@file.json` for JSON values.**  
A JSON value also accepts `@path/to/file.json`; the `@` prefix reads the value from the named file. Applies to the positional `json` argument of `run`/`step complete` and to `--input-schema`, `--output-schema`, `--auth`.

**C10 — `args` is always present; empty input is `{}`.**  
`POST /v1/run` and `POST /v1/steps/{id}/complete` require an `args` field in the request body. `{}` is the canonical representation of an empty argument set. The CLI passes `{}` when the positional `json` argument is omitted; the HTTP layer rejects a missing field with `ErrInvalidInput`.

**C11 — `--required-caller` carries a handle, local or remote.**  
The step completer is identified at creation time by a local `handle` or a remote `user@kernel` principal. The server resolves it to `required_caller_user_id`, plus `required_caller_remote_id` for a remote principal, whose completion additionally demands the home kernel's `step_auth` attestation (P8). Completion is rejected if the authenticated caller does not match.

**C12 — Diagnostic output goes to stderr; resource data goes to stdout.**  
Log lines, progress messages, and error text go to stderr. The only content written to stdout is the resource payload: human-readable summaries, `--json` bodies, and `--quiet` IDs. This makes every command pipeable and keeps `$(juice ... --quiet)` capture reliable.

**C13 — All commands are TCP clients; `admin` verbs are superuser-gated routes.**  
Every command runs by calling the server over HTTP. There is no `--db` and no `--config`: a kernel *is* its `$JUICE_HOME/kernel/`, which only `serve` reads; `--addr` binds the HTTP API, and `config.json`'s `fed_listen_addrs` binds the peer transport (empty ⇒ OS-assigned ports). The CLI keeps named profiles under `$JUICE_HOME/client/`, each pinning one kernel's endpoint, public key, and network digest and holding its tokens. `juice use [<name>] [--endpoint <url>]` switches profile and verifies it against `GET /health`, refusing a server whose public key or network digest is not the pinned one; bare, it lists the profiles. `JUICE_PROFILE` selects a profile for one invocation. `--server <url>` overrides one invocation's endpoint (default `http://localhost:4040`), and credentials travel only to the profile's own recorded address — never to an overridden one, since what a server says about itself is a claim rather than a proof. `admin *` are superuser supervision served on that same public TCP API, on routes gated by an `IsSuperuser` check — authority is the `sys` bearer token (keep it secret; expose `serve` only behind TLS or on loopback), not a separate socket or filesystem access. `juice serve` is the sole process that opens the database. Supervision over ordinary resources is **scope**, not a separate surface: a superuser sees all rows on `action/process/tx/step list` and may `action disable`/`enable` any action, all over the normal TCP API.

**C14 — Help text is plain operator English.**
Every command's help is written for an operator who has not read requirements.md: no spec symbols (Q, X, bps), no section references, no protocol jargon without an in-sentence explanation; config concepts are named by their config key (`lottery`, never L). Use lines use uppercase metavariables; square brackets indicate optional arguments. Non-obvious metavariables are explained in the command's help. Flag descriptions are sentence case without trailing periods.

---

## Operation Reference

### Server

| Operation | HTTP | CLI |
|-----------|------|-----|
| Health check | `GET /health` (open) → `{status, handle, public_key, network, network_digest, decimals, rail_address}`; identity banner — what a client pins before it trusts a server (C13) | `juice health` |

Federation has no HTTP surface: peer identity, gossip, manifests, and inbound calls travel over the cross-kernel transport (D12), not over this API. Kernels discover each other in the background through libp2p routing discovery over their own network's namespace (D12), so kernels of different networks never meet; discovered actions surface through `sys/lookup`, and every known kernel — counterparties and discovery-only alike — appears in the merged `admin peers` roster and is inspected with `admin inspect <key>`.

### Authentication

| Operation | HTTP | CLI |
|-----------|------|-----|
| Authorize (PKCE) | `POST /v1/auth/authorize` `{handle, password, code_challenge, [redirect_uri]}` → `302` if `redirect_uri` provided, else `200 {"redirect":"?code=CODE"}` | `juice auth login <user> [--password]` |
| Token exchange | `POST /v1/auth/token` `{code, code_verifier, [redirect_uri]}` → `{access_token, refresh_token}` | (driven internally by `auth login`) |
| Refresh token | `POST /v1/auth/refresh` `{refresh_token}` → `{access_token, refresh_token}` | (automatic on any command's 401) |
| Logout | `POST /v1/auth/logout` `{refresh_token}` → 204 | `juice auth logout` |
| Recover (start) | `POST /v1/auth/recover/start` `{handle}` → `{nonce, expires_in_seconds}` | (part of `juice auth recover`) |
| Recover (complete) | `POST /v1/auth/recover/complete` `{handle, nonce, signature, password}` → `{status}` | `juice auth recover <user> [--phrase] [--password]` |

`signature` is `base64url(Ed25519-sign(priv, JCS({"recovery_challenge": nonce})))`, where `priv = Ed25519 from seed[:32]`, `seed = BIP-39(phrase)`; `recovery_public_key` is `base64url(pub)`.

### Users

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create user | `POST /v1/users` `{handle, password, [recovery_public_key]}` → 201 user | `juice user create <user> [--password]` |
| Get self | `GET /v1/me` → user (`id`, `handle`, `description`, `available`, `locked`, `rail_address`, `connectors` the directory-grouped tree each `{directory, connections, actions:[{action, scopes, provider_key, created_at}]}`, `connections` the account inventory each `{provider, actions, unused, provider_key, created_at}`) | `juice user me` |
| Update self | `PUT /v1/me` `{[description], [current_password, password]}` → user | `juice user update [--description] [--password]` |
| Transfer credits | `POST /v1/transfers` `{recipient, amount, [reason], [external_key]}` → ledger entry | `juice user transfer <recipient> <amount> [--reason --external-key]` |
| List ledger | `GET /v1/ledger[?limit=&offset=]` → ledger entry[] | `juice user ledger [--limit --offset]` |
| Register payout address | `PUT /v1/me/address` `{address, signature}` → `{address, attributed}` | `juice user address <address> [--signature <sig>]`; bare `juice user address` shows the registered one, read from `/v1/me` |
| Withdraw credits | `POST /v1/withdrawals` `{id, amount, [reason]}` → withdrawal row `{id, kind, amount, destination, status, tx_hash, reason, created_at, finalized_at}` | `juice user withdraw <amount> [--reason]` |
| List withdrawals | `GET /v1/withdrawals` → own withdrawal rows | `juice user withdraw` (bare) |
| Where to pay in | client-composed from `GET /health` + `GET /v1/me` | `juice user deposit` — prints where to send money and whether the caller has registered an address |

`handle` is immutable. `description` and `password` are updatable by the authenticated user; `password` change requires `current_password` to verify the existing credential. At least one of `description` or `password` must be provided (`description` may be `""` to clear). There is no email; account recovery is by seed phrase (D9): `user create` generates a 12-word BIP-39 mnemonic client-side, sends only the derived `recovery_public_key`, and prints the phrase once; `juice auth recover <user>` resets a lost password by signing a server nonce with the phrase-derived key. Kernel accounts (federation peers) cannot be created here, cannot log in, and hold no tokens — the schema forbids them a handle, password, or recovery key; they exist only through a peer's first call, authenticate per request by federation signature, and cannot use `PUT /v1/me`. They are named by their kernel's petname, in a namespace separate from user handles: a user and a kernel may both be `minibox` locally, and the five commands that accept either (`show`, `rename`, `suspend`/`unsuspend`, `deposit`) refuse an ambiguous bare name rather than guess.

`user transfer` debits the caller and credits a local recipient in one fee-free ledger entry (rejects self-transfer, non-positive amount, and a suspended or peer recipient; `insufficient_funds` on low balance). `GET /v1/ledger` lists the caller's own movements — deposits, withdrawals, transfers, and value delivered by a `transfer`-effect action (D18), whose entry carries the settling transaction's id as its `reason` — newest first, paginated, each with `operator_handle` plus `from_handle`/`to_handle` (null side omitted). A run that moves value is therefore two records: the transaction for what the call cost, and this entry for what moved.

`id` on a withdrawal is minted by the caller (the CLI mints one) and is the row's own id: a replay returns the same row (200), the same id with different terms is 422. A withdrawal snapshots its destination when it is created, so registering another address never redirects one already in flight. `insufficient_funds` (402) on a low balance, 409 when the destination is not registered, `rail_stopped` (503) while outgoing rail work is halted. `PUT /v1/me/address` proves control of the address by a signature over the kernel's registration message, stores the canonical form, and retroactively attributes the payments already received from that sender (`attributed` counts them); one address serves one account, replacement needs only a new signature, and a world whose money has no addresses answers 409.

### Grants and connections (delegated auth)

| Operation | HTTP | CLI |
|-----------|------|-----|
| Plan consent | `GET /v1/grants/plan?selector=` → `{groups: [{provider, scheme, scopes, destinations, connected, covered, actions: [{action, granted}]}], skipped}` | (driven by `user connect`) |
| Start consent | `POST /v1/grants/start` `{selector, provider, [redirect_uri], [flow]}` → `{status:"granted", actions}` (already covered) or `{state, authorize_url}` (code) or `{state, verification_uri, user_code, interval, expires_in}` (device) | `juice user connect <selector> [--device]` |
| Complete consent | `POST /v1/grants/complete` `{state, [code]}` → `{status:"granted", provider, actions, created_at}` or `{status:"pending"}` | (driven by `user connect`) |
| Attach token | `POST /v1/grants` `{selector, [provider], token}` → `{status:"granted", provider, actions, created_at}` | `juice user connect <selector> --token <pat>` |
| Disconnect grants | `DELETE /v1/grants?selector=` → `{revoked: [actions]}` (a full `owner/name` is the degenerate one-action selector) | `juice user disconnect <selector>` |
| Disconnect account | `DELETE /v1/grants?account=<provider_key>` → `{revoked: [actions], connection}` | `juice user disconnect --account <provider>` |

A `Grant` is per-action consent (D10): a pointer binding one action to a `Connection` — the caller's upstream account credential, stored once per `(user, provider)` and shared by every grant that points at it. A **selector** (`owner`, `owner/path`, or a full `owner/name`; path-segment matched, trailing `/*` stripped) names a set of delegated actions; `user connect` fetches the plan, groups them by upstream account, shows the delta, and covers each group with one gesture: one browser consent (requesting the union of the group's scopes) for `oauth_delegated`, or one token paste for `delegated_bearer`. Connecting an action whose account is already connected with covering scopes grants instantly with no browser. For `oauth_delegated` the client hosts the redirect target — a loopback listener for local clients (CLI/desktop), a registered callback for hosted ones — and the server holds only in-memory PKCE/device state and performs the token exchange, so the refresh token never transits the client. The CLI's `--token` reads without echo when the flag value is empty (keeping it out of shell history). Running an action that lacks a grant returns `grant_required` (403) with the action in `meta`; the CLI (`juice run`) offers consent inline on an interactive terminal and otherwise prints a `juice user connect <directory>` hint. Grants and connections are listed token-free in `GET /v1/me` and are never otherwise readable. `/v1/me` returns them as a **directory-grouped tree** under `connectors`: one node per folder (`directory` = the granted actions' shared folder, the ref up to its last `/`, e.g. `chat` or `chat/inbox`), each carrying the `connections` that back it (usually one) and the `actions` granted under it. A separate top-level `connections` array is the full account inventory, so a connection with zero grants still surfaces with `unused: true`. Each action and its backing connection carry the same opaque `provider_key`; the same value addresses `DELETE /v1/grants?account=`. Treat it as opaque — never parse it (`provider` is the display label). The `directory` is a display grouping only — it never gates a credential (the token binding stays per-action and fact-derived, D10), so grouping by it is safe. `provider_key` is omitted on an unbackfilled legacy grant that has no connection.

### Actions

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create action | `POST /v1/actions` `{name, kind, [source, wasm_artifact, method, params, description, price, input_schema, output_schema, auth]}` → 201 action | `juice action create <name> --kind [--source --artifact --method --param --description --price --input-schema --output-schema --auth]` |
| List actions | `GET /v1/actions[?owner=&name=&all=&limit=&offset=]` → action[]; active-only by default (unauthenticated → active public; authenticated → active public + local + own active; superuser → all owners' active); `?all=1` includes inactive/private in scope; `?owner=`/`?name=` filter | `juice action list [--all --limit --offset]` |
| Show action | `GET /v1/actions/{id}` → action | `juice action show <action>` |
| Update action(s) | `PUT /v1/actions` `{target[, price, description, source, wasm_artifact, method, params, input_schema, output_schema, visibility, auth]}` → action[] | `juice action update <target> [--price --description --source --artifact --method --param --input-schema --output-schema --visibility --auth]` |
| Enable action(s) | `POST /v1/actions/enable` `{target}` → action[] | `juice action enable <target>` |
| Disable action(s) | `POST /v1/actions/disable` `{target}` → action[] | `juice action disable <target>` |
| Delete action(s) | `DELETE /v1/actions?target=` → action[] | `juice action delete <target>` |
| Import OpenAPI | `POST /v1/actions/import` `{name[, spec_url, auth]}` → import result | `juice action import <name> [<spec-url>] [--auth]` |
| Get stats | `GET /v1/stats/{id}` → stats | `juice action stats <action>` |
| List ratings | `GET /v1/actions/{id}/ratings[?limit=&offset=]` → `[{value, note, created_at}]` | `juice action ratings <action> [--limit --offset]` |

`<action>` is `owner/name` (a raw id is also accepted); `GET /v1/actions?ref=<reference>` resolves one reference the way a call does, exclusive with `?owner=`/`?name=`, which stay exact filters; a kernel-qualified reference requires authentication, since only that one dials a peer. `<target>` on a mutation is either an action id, naming exactly that row, or `owner/path`, naming the action at that path and every action beneath it (`bob/mail` reaches `bob/mail/send`, never `bob/mailer`); the two shapes are disjoint, so the server decides which was meant. `visibility`, `price`, and `auth` apply to a whole subtree; a `description`, schema, or execution source needs a target that resolves to one action; a target matching nothing is a 404. `action import <name> <spec-url>` installs one OpenAPI document as the application at `<name>`: every operation lands at `<name>/<operation_key>` and the operation keyed `index` becomes its root. The URL is given once — afterwards `action import <name>` re-reads the recorded document and reconciles, keeping each action's id, history, credentials, visibility, and any price the owner set (the document owns the price only where it declares `x-juice-price`). One name holds one document; the same document may be installed under other names as independent applications. `--auth` attaches one upstream credential configuration to the whole application. Action responses (show and list) include a computed `action` field (`owner/name`) alongside `id`, plus the full `input_schema` and `output_schema` — the CLI text view shows the same fields the JSON returns. `visibility` is `private` (owner only), `local` (any local caller of this kernel, never peers), or `public` (anyone, and the only value served in manifests/gossip, P6); it is caller-scoped callability (requirements.md D5), settable only via update, and defaults to `private`. `price` is the subtree bound: the maximum total cost of the action and everything it calls. `auth` is the upstream credential config `{scheme, config, secrets}` (R9: write-only, never returned); reads instead expose only its non-secret summary — `auth_scheme` (scheme name, when present) and `requires_grant` (R8).

The `auth` object is `{scheme, config, secrets}`; valid schemes and their keys (semantics in requirements.md D10):

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

The per-caller schemes hold no secret in `auth`; each caller supplies their credential through the Grants surface above, and a call with no grant returns `grant_required` (403). Actions imported from OpenAPI take their credentials the same way any action does: pass `auth` at import to set one configuration for the whole application, or `PUT /v1/actions` over its path afterwards.

### Run

| Operation | HTTP | CLI |
|-----------|------|-----|
| Run action | `POST /v1/run` `{action, args, [quote_hash]}` → `{result, tx_id, trace_id, receipt_id, process_id}` | `juice run <action> [json] [--quote-hash H]` |

`run` is the single execution entry point: it atomically creates a process funded with exactly the action's price (parked from the caller's available balance), issues the root call, and the process closes itself when the root call has returned and no steps remain outstanding. A remote-proxy `run` may fail with a federation-relationship error distinct from the caller's own funds: `peer_unreachable` (502) — the peer was offline and the request provably never left this kernel, so the call was refunded rather than parked (retry when it is back); or `peer_unfunded` (402, `meta.peer` names the peer) — the peer will not serve this kernel on credit: its limit is reached, or its own provider cannot fund the work. An operator condition, never the caller's balance; or `unauthorized` (403, `meta.peer` set) — the peer refused the call outright (it suspended us, or will not serve that action), which no retry fixes (P7). An execution failure at the provider stays `execution_failed`, whatever it charged; a provider that has changed the action's contract also returns `execution_failed`, with `meta.retry = "refresh"` and nothing charged — the cached copy is discarded and the next call re-resolves. A dispatched call with no signed outcome yet returns `timeout` and is parked, not lost: `meta` carries `process_id`, `pending_since`, and `refund_eligible_at`, so the reserved funds can be followed with `process show` while the server retries. `refund_eligible_at` is a lower bound, not a deadline — from that time the call qualifies for the automatic full refund, which a subsequent retry pass applies; `process end` refunds it sooner. Only a signed receipt, that refund, or explicit closure settles it. The reply carries `process_id`, the handle for `process show`/`end` when work parks; there is no funding amount to choose. `args` defaults to `{}` when omitted. Zero-price actions (e.g. `sys/lookup`) run with zero funding — no special handling. A separate condition, never a run's: `rail_stopped` (503) refuses outgoing rail work — `user withdraw`, and the payments settling cross-kernel obligations — while a rail payment of this kernel's is blocked; deposits, paid work, and reads continue.

**Composition from a `kind=http` action (capability, D8).** When the kernel dispatches an HTTP action it sends the endpoint two headers: `X-Juice-Callback` (this kernel's callback base URL) and `X-Juice-Capability` (a signed token naming the call's live trace). While the call is in flight the endpoint composes by calling back with `X-Juice-Capability: <token>` instead of a bearer JWT: `POST /v1/call` `{action, args}` → `{result, tx_id, trace_id}` is a subcall on that trace (the HTTP twin of `juice.call`, with no `run`/wallet path); `POST /v1/steps` and `POST /v1/steps/{id}/complete` accept the same capability, the trace coming from the token (send no `trace_id`). Composition runs as the executing action's owner within its trace, funded from it and bounded by the action's advertised price — a leaf endpoint just ignores the headers. `/v1/call` is not a user-invocable command (there is no CLI for it; the capability is minted per dispatch and dies when the call settles).

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

A caller reads transactions where it is a captured party: payer, caller, or payee; a superuser reads all. Responses render each party as a `handle` — `owner_handle` (payer), `caller_handle` (caller), `target_handle` (payee); the raw `*_user_id` UUIDs are not returned, since a user is addressed by handle, never an id (a purged party falls back to its raw id). `rating` must be 0 (bad) or 1 (good); `note` is an optional string. Transaction and list responses include a `rating` field — `{"value": 0|1, "note": string|null}` when rated, `null` when unrated. Transaction `args` and `result` fields are inline JSON objects. `gross` is the call's full allocation; `fee + net` is the value added paid out at settlement. A cross-kernel transaction also carries `ticket_id`, the name both kernels know the obligation by, so either party can follow it and an operator recording its payment can name it. The obligation itself is a row on the side that is owed; the buyer's side of it is the receipt on this transaction and the payment on the rail (P10).

Remote-proxy transactions include `remote_receipt_hash` and `remote_receipt_json`. `receipt-verification` verifies entirely from local data, in two parts: **receipt integrity** (signature against the peer's public key, stored JSON against its stored hash, `action_id` against the proxy) and **settlement consistency** (local outcome matches `receipt.status`; the obligation equals `receipt.charge + receipt.premium`; the refund arithmetic checks out; the ticket paid what the recorded secret, the receipt's nonce and the obligation decide). The receipt's own `gross/net/fee` are the remote kernel's economics and are reported, not compared. Returns `ErrInvalidState` for non-remote-proxy transactions.

### Steps

| Operation | HTTP | CLI |
|-----------|------|-----|
| Create step | `POST /v1/steps` `{trace_id, action, partial_args, required_caller}` → 201 step | `juice step create <action> --trace --required-caller <user> [--partial-args]` |
| List steps | `GET /v1/steps[?process_id=&status=&limit=&offset=]` → step[]; each carries `created_by` (the creating action `owner/name`, from the parent trace) alongside `action` (the completion target) | `juice step list [--process --status --limit --offset]` |
| Show step | `GET /v1/steps/{id}` → step (incl. `created_by`) | `juice step show <id>` |
| Complete step | `POST /v1/steps/{id}/complete` `{args}` → `{result, tx_id, trace_id, step_id}` | `juice step complete <id> [json]` |

A step is a funded continuation: creation snapshots the action's price as `step.price` and parks it from `trace_id`; the process is derived from `Trace(trace_id).process_id`. Completion spends the parked price — no funds check occurs, and the completion's allocation and transaction `gross` are `step.price`. `required_caller` is a `handle`; the server resolves it to `required_caller_user_id`. Step listing returns the caller's own steps (as process owner or required caller), or all of them for a superuser. `status` filter accepts `waiting`, `running`, `done`, or `cancelled`. The `args` field in the complete request is merged with the step's `partial_args` (completion `args` overwrites on key collision); the allowed completion input is derived as `action.input_schema \ keys(partial_args)` — a violation rejects the completion and leaves the step `waiting`, never recorded as an action failure. Step read and list responses include `price`, `action_id`, a computed `action` field (`owner/name`), `owner_handle` (the process owner / payer as a `handle`, mirroring a transaction's `owner_handle` — the step settles into that owner's transaction), `required_caller_handle` (the required caller as a `handle`, not the raw `required_caller_user_id`), `waiting_on_peer` on a waiting step whose required caller is a peer, and `allowed_input` on a waiting step — the derived completion schema (`action.input_schema \ keys(partial_args)`) so the required caller can complete it without separately reading the target action (which, when private, they may not be able to read). An outstanding step keeps its process open.

### System Actions

Native actions registered at bootstrap, owned by `sys`, `local` — callable by this kernel's own users, never served abroad (D17) — and runnable like any other action — `juice run sys/lookup '{"query":"…"}'`. `sys/lookup` results carry `action_id`, `action` (`owner/name`), `description`, `price`, `score`, `input_schema`, and `output_schema`. A hit hosted by another kernel also carries `observed_at` (when its terms were last verified there) and that kernel's reachability, `last_seen` and `last_contact_failed_at` — raw timestamps, so a client shows how stale a result is and decides for itself what counts as offline; the kernel neither hides nor reorders a hit for being unreachable. `price` is the all-in local price; for a discovered-but-unresolved remote action it is indicative, re-quoted authoritatively at resolve (D13). `score` is a dimensionless relevance rank score (lexical and semantic legs fused by reciprocal-rank fusion; stats-based quality weighting is UNDER REVISION and temporarily removed, D17); it is comparable only for ordering within one response, not across queries or as a probability:

| Action | Price | Purpose |
|--------|-------|---------|
| `sys/lookup` | 0 | Rank active actions by query |
| `sys/llm/chat` | 0 | Platform LLM chat |
| `sys/llm/embed` | 0 | Text embedding vector |
| `sys/llm/json`  | 0 | Structured JSON output from LLM, locally validated |
| `sys/llm/decide` | 0 | LLM-driven action selection; returns chosen action and args without executing |
| `sys/time` | 0 | Current time |
| `sys/sink` | 0 | Universal no-op step target |
| `sys/message` | 0 | Message a user by creating a step they acknowledge |
| `sys/random` | 0 | Random float in `[0, 1)` |
| `sys/web` | 0 | Fetch a public web page (read-only GET) |
| `sys/transfer` | 0 (configurable, `native.transfer`) | Deliver value from the immediate caller to `target` — a receipt-backed transfer effect (D18) |
| `sys/tinygo/compile` | 5 (configurable, `native.tinygo`) | Compile TinyGo source to a WASM artifact |

### Federation

There is no subscription: a call to a remote action `owner@kernel/name` resolves and caches it on demand — the sole cache-fill path (D13). A peer action is addressed as `owner@kernel/name` (the sole name form; a cached proxy is also reachable by its raw action id) and called through `POST /v1/run` like any local action; there are no federation HTTP endpoints. Cached proxies are enabled with `visibility = local`, so a kernel exposes only its **own** `public` actions to peers — a peer's own imports are never re-advertised *and* an inbound peer call to one is denied by `CanCall` (a peer caller fails the `local` branch, D5), so federation is non-transitive at both the manifest and the call layer (reach a peer's imported action by resolving its true owner). The proxy cache is kernel-managed: a proxy that is absent or inactive re-resolves on the next call, and a contract-hash mismatch, an invalid receipt, or any non-funding signed rejection deactivates it, so a withdrawn remote action leaves search after one refusal (D13); manual `action enable`/`disable` is rejected on a `remote_proxy`, and the durable peer lever is `admin suspend`. Trust and peering are managed via the admin commands below. The cross-kernel transport is an implementation detail (D12).

### Admin (superuser-gated TCP routes)

The operator verbs no ordinary user performs — money, access, federation trust, and the global roster. Served on the public TCP API, on routes gated by an `IsSuperuser` check (authority is the `sys` bearer token). Everything else a superuser does (see all actions/processes/txs/steps, disable any action) is *scope* on the normal commands above, not an admin command.

| Operation | HTTP | CLI |
|-----------|------|-----|
| List all users | `GET /control/users[?limit=&offset=]` | `juice admin users [--limit --offset]` |
| Show account or kernel | `GET /control/users/{handle}` | `juice admin show <user\|kernel>` — a kernel target returns one flat record: `petname`, `nickname`, `public_key`, `about`, the sync cache, and the account state when one exists |
| Suspend account | `POST /control/users/{handle}/suspend` | `juice admin suspend <user\|key>` — freezes any account: a human cannot log in; a peer's inbound calls are refused with a signed rejection |
| Unsuspend account | `POST /control/users/{handle}/unsuspend` | `juice admin unsuspend <user\|key>` |
| Rename | `POST /control/users/{handle}/rename` | `juice admin rename <target> <new-name>` — a local account target renames its handle (freeing the old one); a kernel target (petname or key) binds its **petname**, exactly, rejecting an occupied name. This is the only way to name a kernel this node has merely discovered |
| Deposit credits | `POST /control/deposit` `{handle, ref, [amount], [reason]}` | `juice admin deposit <target> [<amount>] --ref <fact> [--reason]` — three forms: record a payment made outside the system (`<user> <amount> --ref <fact>`; a second use of the same fact moves nothing, the same fact with a different amount is 422, no `--ref` is 422); attribute a payment already received whose sender was unknown (`<user> --ref <txhash>[:<index>]`); confirm the payment that settles a peer's obligation (`<peer> [<amount>] --ref <ticket id>` — the only form naming a peer, and its money reaches the seller, never the peer). 404 for a fact nobody has seen, 409 for one already attributed |
| List money awaiting attribution | `GET /control/deposits` → `{deposits, owed}`: payments held for a sender nobody has registered (every observed payment is booked held and reconciled — obligations first, then the account that registered the sending address), and obligations a buyer says it has paid whose money this kernel has not yet seen (each with its `peer`, its `obligation`, the `amount` the draw decided and the `tx_hash` it named). A payment from an address an admitted foreign call named as its payer is never attributed to a local account while that obligation is unresolved, so a winning payment that lands before its reveal waits for it. Every row's `id` is what `--ref` names to close it | `juice admin deposit` (bare) |
| List peers | `GET /control/peers[?all=&limit=&offset=]` | `juice admin peers [--all --limit --offset]` — every known kernel from one query, this kernel excluded: counterparties (`has_account=true` and the D16 contact cache `last_seen`/`last_contact_failed_at`) and discovery-only kernels (`has_account=false`, `actions` count). `petname` is the local name that **resolves** a reference; `nickname` is what the kernel calls itself and never resolves (D15) — an unbound kernel shows `—` and stays callable by key. No internal id; `--all` also lists suspended counterparties |
| Inspect a kernel | `GET /control/peers/inspect?key=` | `juice admin inspect <key\|user>` — identity (incl. its `about`), public actions (with descriptions), retained evidence grouped by issuer (trade-backed vs unverified), reachability. `source` is `live`/`local`/`none`: an offline but known peer degrades to last-known local data (`online:false`) |
| Show own identity | `GET /control/identity` | `juice admin identity` → `{handle, public_key, about, addrs, network, network_digest, rail_address, finalized{token, gas, block}, sys{earnings, pending_payouts, held_deposits, refill_locks}, solvency{liabilities, vault, gap}, custody{checked, difference, ok}, stop{reason, since}, fee_bps, remote_bps, import_bps, lottery, credit_limit, exposure}` — identity, the rail position (D23), the money rules this kernel serves under, and its credit position (D14): what it has delivered unpaid, against the limit it set |

`handle` on both bodies is the full identifier set, not only a handle: a local user's handle or id, or a kernel's petname or public key — a bare name matching both namespaces is refused rather than guessed (D15). `<user>` is a `handle` — or, for a peer, its base64url public key (the global name); `<action>` is `owner/name` (or an id); `<key>` is a peer's base64url public key. `admin deposit` never credits a peer: a peer account is identity, not a wallet, so its only accepted form is the one naming an obligation it owes, whose money is credited to the seller (peering itself is handshake-free, established by a peer's first call); the `about` shown by `identity`/`inspect` is `sys`'s user description, set with `juice user update --description`. Blocking a peer's inbound calls is `admin suspend <key>` (reversible with `unsuspend`); a caller that no longer wants a peer's actions simply stops calling them, and the cached proxies lapse through peer retention (D16). A peer account holds no session token, so a step whose required caller is a peer is completed by this kernel's operator with `juice step complete <id> --peer <key>` (superuser scope on the ordinary command); `admin inspect <key>` lists what a peer holds for us. Both are refused for a suspended peer.