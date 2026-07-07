# OAuth actions: calling APIs as yourself

*Juice · Tutorial · v0.5*

How to build Juice actions that reach an outside API on behalf of the person running them — where each user connects their own account once, and the token is applied safely, automatically, forever after.

## Contents

1. [The one idea](#1-the-one-idea)
2. [Three ways to authenticate](#2-three-ways-to-authenticate)
3. [Before you start](#3-before-you-start)
4. [Anatomy of a delegated action](#4-anatomy-of-a-delegated-action)
5. [Walkthrough: build one](#5-walkthrough-build-one)
6. [What happens under the hood](#6-what-happens-under-the-hood)
7. [The owner-held OAuth schemes](#7-the-owner-held-oauth-schemes)
8. [Managing grants](#8-managing-grants)
9. [Federation & delegated actions](#9-federation--delegated-actions)
10. [Security properties](#10-security-properties)
11. [Troubleshooting](#11-troubleshooting)
12. [Reference](#12-reference)

---

## 1. The one idea

An action of kind `http` calls some outside API. That API needs credentials. Juice stores those credentials in the action's encrypted `auth_json` column and applies them automatically each time the action runs — the running code never touches the raw secret.

Until now, every credential belonged to the *action's owner* and was shared by everyone who called it — a single API key for the whole world. The new capability lets a credential belong to **the individual person running the action**. That person connects their own account once, in a browser, and from then on the action calls the API *as them*. "Summarize **my** inbox" finally means *my* inbox.

## 2. Three ways to authenticate

The question that picks your scheme is always the same: **whose identity is the call made under?**

| Family | Schemes | Whose identity | Per-user setup | Use it for |
|---|---|---|---|---|
| **Static** | `bearer` `basic` `header` `query` | the owner's, shared | none | a plain API key or username/password you hold |
| **Owner OAuth** | `oauth_client_credentials` `oauth_jwt_bearer` | the owner's app, shared | none | machine-to-machine APIs / service accounts |
| **Delegated** | `oauth_delegated` | **each caller's own** | one-time consent | "act as me" APIs — mail, calendar, drive |

The static schemes are unchanged. The two *owner OAuth* schemes are new but conceptually familiar: they still use one shared identity — Juice just fetches and refreshes the token for you instead of you pasting a static one. `oauth_delegated` is the genuinely new shape, and most of this tutorial is about it.

> **Rule of thumb** — Everyone should hit the API as the *same* account → static scheme or `oauth_client_credentials`. Everyone should hit the API as *themselves* → `oauth_delegated`.

## 3. Before you start

To publish a delegated action you (the action owner) need three things.

**An OAuth app registered with the provider.** Register an application in the provider's console (Google Cloud Console, GitHub Developer Settings, and so on). You get a `client_id` — and, for a confidential app, a `client_secret` — and you register the **redirect URL(s)** where your users' clients will receive the authorization code. For the Juice CLI that is a loopback address; for a hosted web client it is that client's own callback URL.

**The provider's endpoints.** From the provider's docs: its **authorize URL** and **token URL** (and, if you want the headless flow, its **device-authorization URL**).

**A Juice server with a credentials key.** The server encrypts every user's refresh token at rest with `credentials_key` — generated automatically on first boot. Nothing extra to configure; just know it must be present, or storing credentials fails closed.

## 4. Anatomy of a delegated action

It is an ordinary `kind=http` action with three parts: where it calls (`source`), its input/output `schema`s, and an `auth` block. For `oauth_delegated`, the auth block holds *provider configuration only* — never a user's token.

```jsonc
// auth block · oauth_delegated
{
  "scheme": "oauth_delegated",
  "config": {
    "auth_url":  "https://accounts.google.com/o/oauth2/auth",
    "token_url": "https://oauth2.googleapis.com/token",
    "client_id": "your-app.apps.googleusercontent.com",
    "scopes":    "https://www.googleapis.com/auth/gmail.readonly"
    // optional: "device_auth_url" for the headless flow
  }
  // optional: "secrets": { "client_secret": "..." }  for a confidential app
}
```

| Field | Where | Meaning |
|---|---|---|
| `auth_url` | config | provider's authorize endpoint (opens the consent screen) |
| `token_url` | config | endpoint that trades the code for tokens |
| `client_id` | config | your registered OAuth app id |
| `scopes` | config | space-separated permissions to request |
| `device_auth_url` | config | optional — enables the device-code flow for headless clients |
| `client_secret` | secrets | optional — only for confidential apps; sealed, never returned |

> **Note** — The action carries the *contract* and the *OAuth app*, but zero user secrets. Each user's token lives in a separate **grant**, created only when that user consents.

## 5. Walkthrough: build one

We'll build a "summarize my inbox" action against Gmail.

**1 · create the action**

```bash
juice action create inbox \
  --kind http \
  --source "https://gmail.googleapis.com/gmail/v1/users/me/messages" \
  --price 10 \
  --description "Summarize my inbox" \
  --auth '{"scheme":"oauth_delegated","config":{
           "auth_url":"https://accounts.google.com/o/oauth2/auth",
           "token_url":"https://oauth2.googleapis.com/token",
           "client_id":"your-app.apps.googleusercontent.com",
           "scopes":"https://www.googleapis.com/auth/gmail.readonly"}}'
```

**2 · enable it**

```bash
juice action enable <id>
```

**3 · a user runs it — one command**

```console
$ juice run @owner/inbox
This action needs your authorization. Authorize @owner/inbox now? [Y/n] y
  → browser opens, you approve on the provider's screen
{ "result": { "summary": "3 unread: a bill, a newsletter, mum" },
  "tx_id": "b436…", "receipt_id": "f5e6…" }
```

That is the whole experience. `run` notices the missing connection, walks the user through consent in their browser, and then **finishes the original run by itself** — no second command, no retyping. Every run after that just returns the result: the connection is stored and its token refreshes automatically.

**4 · confirm and (later) disconnect**

```bash
$ juice user me         # grants: [ { action: "@owner/inbox", scopes: "gmail.readonly", … } ]
$ juice user disconnect @owner/inbox
```

> **Works behind a home router** — The provider redirects the user's *browser* back to the client — nothing has to reach the Juice server from outside. No port forwarding, no public address, no `.well-known`.

## 6. What happens under the hood

### Consent is a precondition, not an error

A run with no matching grant is rejected **before any funds are locked and before any transaction exists** — so an unconnected call never charges the caller and never dents the provider's stats. The rejection is a structured signal, not a prose string, so any client can act on it:

```json
HTTP 403
{ "code": "grant_required",
  "error": "grant required for @owner/inbox",
  "meta": { "action": "@owner/inbox" } }
```

The CLI reads that code and offers consent inline on a terminal; a web app renders a "Connect" button; an agent pauses and asks its own user. The ergonomic lives in the protocol, so every surface gets it.

### The binding rule

At dispatch, a delegated token is applied **only when both are true**:

1. the grant's owner is **the process owner** — the human who ran the tree and pays for it
2. the grant names **the exact action executing** — the specific code they consented to

That single rule is the confused-deputy defense. A token never follows into a subcall of a *different* action, never crosses to another kernel over federation, and WASM scripts never see it. Your Gmail connection is usable only when *you* run *that* action.

### It maintains itself

- **Refresh** is automatic — the stored refresh token mints short-lived access tokens as needed.
- An **upstream 401** forces one token refresh and a single retry.
- If the provider says the connection is dead (`invalid_grant`), the grant is deleted and the next run asks you to reconnect.
- Access tokens live in memory only; only the refresh token is stored, sealed.

## 7. The owner-held OAuth schemes

When the API should be called under *one* identity (yours), but speaks OAuth rather than a static key, use one of these. There is no grant and no consent — the credential lives entirely in `auth_json`, and Juice fetches and refreshes the token for you.

### Client credentials

```jsonc
// auth block · oauth_client_credentials
{
  "scheme": "oauth_client_credentials",
  "config":  { "token_url": "https://api.example.com/oauth/token",
              "client_id": "abc123", "scopes": "read write" },
  "secrets": { "client_secret": "••••••••" }
}
```

### JWT bearer (service accounts, RFC 7523)

Juice signs an **RS256** assertion with your stored private key and trades it for a token — the shape Google service accounts use.

```jsonc
// auth block · oauth_jwt_bearer
{
  "scheme": "oauth_jwt_bearer",
  "config":  { "token_url": "https://oauth2.googleapis.com/token",
              "client_id": "svc@project.iam.gserviceaccount.com",
              "scopes": "https://www.googleapis.com/auth/cloud-platform" },
  "secrets": { "private_key": "-----BEGIN PRIVATE KEY-----\n…" }
}
```

> **Fails closed** — The scheme and its required keys are validated when the action is created or updated, and dispatch refuses an unknown scheme — a request is never sent unauthenticated because a scheme name was mistyped.

## 8. Managing grants

- **See your connections:** `juice user me` lists your grants — action, scopes, and when you connected. Never any token material.
- **Disconnect one:** `juice user disconnect @owner/inbox`. The next run asks to reconnect.
- **Pre-connect:** `juice user connect @owner/inbox` connects ahead of time without running. Add `--device` for the headless code-entry flow.
- **Contract changes revoke consent:** if the owner changes the action's price, schema, source, or credentials — anything that deactivates it — every standing grant is dropped. Consent never silently carries over to changed code. A plain enable/disable does not touch grants.

## 9. Federation & delegated actions

A delegated action needs a specific human at a browser, so a *remote* kernel — a machine account with a prepaid balance and no browser — can never call it. Juice therefore **never advertises delegated actions to peers**: they carry no manifest and are never gossiped. This keeps a provider's trade-backed reputation honest, since a peer can't rack up guaranteed failures against an action it could never complete. Owner-held OAuth actions (one shared identity) federate normally — only the per-human case is withheld.

## 10. Security properties

| Property | Guarantee |
|---|---|
| Confused-deputy safe | token applied only when *grantor = payer* and *grant = executing action* |
| No leakage | never crosses subcalls of other actions, federation, or into WASM scripts |
| Write-only at rest | refresh token sealed with the server key; no read path ever returns it |
| No charge before consent | missing grant is rejected before any funds lock or transaction is created |
| Consent tracks the contract | a deactivating change or credential swap revokes standing grants |
| Provider-bound redirect | the kernel never dials the redirect; the provider's registered allowlist binds the code |
| Fail closed | unknown scheme, missing box, or malformed config → refuse, never send unauthenticated |

## 11. Troubleshooting

### `grant_required` keeps coming back

Either you revoked it, the provider expired it (`invalid_grant` auto-removes the grant), or the owner changed the action's contract. Reconnect: `juice user connect @owner/inbox`, or just run and say yes.

### Error 401: `invalid_client` on the provider's page

Your `client_id` isn't a real registered OAuth app, or the `redirect_uri` the client used isn't on the app's allowed list. Register the app and add the redirect URL in the provider's console.

### "…did not respond in time (timed out)"

The Juice server was reachable but slow to finish — usually the provider's token endpoint was slow or briefly unreachable. Try again. (A genuinely stopped server says "cannot reach juice server" instead — the two are now reported distinctly.)

### The consent screen never returns

The client must be able to receive the redirect. For the CLI that means a local browser on the same machine; for a hosted client, its callback URL must be registered with the provider and reachable by the user's browser.

## 12. Reference

### CLI

| Command | Does |
|---|---|
| `juice run @owner/name` | run; offers inline consent if a grant is needed (on a terminal) |
| `juice user connect @owner/name` | connect an account ahead of time; `--device` for headless |
| `juice user disconnect @owner/name` | disconnect (delete the grant) |
| `juice user me` | your account, including your `grants` |

### HTTP

| Endpoint | Body → returns |
|---|---|
| `POST /v1/grants/start` | `{action,[redirect_uri],[flow]}` → `{state, authorize_url}` (or device fields) |
| `POST /v1/grants/complete` | `{state,[code]}` → `{status, action, created_at}` |
| `DELETE /v1/grants?action=` | → `{revoked, action}` |
| `GET /v1/me` | → user + `grants[]` (token-free) |
| `POST /v1/run` | on a missing grant → `403 grant_required` with `meta.action` |

### Auth schemes at a glance

| Scheme | Held by | Secret stored |
|---|---|---|
| `header` / `query` | owner | a header/param name + value |
| `bearer` | owner | a static token |
| `basic` | owner | username + password |
| `oauth_client_credentials` | owner | client id + secret |
| `oauth_jwt_bearer` | owner | RSA private key (RS256) |
| `oauth_delegated` | **each user** | app config only + a per-user grant |

---

*Juice v0.5 · delegated OAuth (§8) · a credential belongs to the person running the action.*
