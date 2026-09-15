---
title: Using Juice from a program
nav_order: 7
---

# Using Juice from a program

A program uses the same commands and the same API as a person. This chapter covers
what it must do differently.

An agent is an ordinary account. It holds its own login, its own balance and its
own history, and it is not privileged in any way. Give it its own account rather
than sharing a person's.

## Name the login on every command

```
$ juice --as bot@acme run sys/lookup '{"query":"translate to german"}' --json
```

or set `JUICE_AS=bot@acme` in the environment.

Never rely on the selected login. A person at the same machine can change it with
`juice auth use` at any moment, which would move your program's spending to another
account or another kernel. A `--as` that names no login on this machine is an
error; it never falls back.

Anything a program remembers about a kernel should be keyed by the kernel's public
key and network digest and the account's principal id, all of which `kernel list`
and `user me` report. Handles and kernel names can be renamed and reused.

## Output

`--json` prints the server's reply as it was sent. `--quiet` prints ids only, one
per line.

```
$ juice --as bot@acme run sys/time --json
{
  "result": { "iso": "2026-09-14T12:06:52Z", "unix": 1789387612 },
  "tx_id": "98bb64e6-…",
  "trace_id": "f4fbbf15-…",
  "receipt_id": "0d60120d-…",
  "process_id": "e9dbb283-…"
}
$ juice --as bot@acme run sys/time --quiet
821a9f33-…
```

## Errors are two different contracts

**From the command line**, an error is a line of prose on stderr and an exit code.
`--json` does not change this: it governs successful replies only.

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | any other failure |
| 2 | not authenticated |
| 3 | not authorised |
| 4 | not found |
| 5 | invalid input, or schema violation |
| 6 | insufficient funds |
| 7 | timeout |
| 8 | consent required |
| 9 | peer provably unreachable |
| 10 | peer will not serve on credit |
| 11 | terms changed |

Branch on the exit code, never on the message text.

**Over HTTP**, an error is a JSON body with a stable `code`, a message, and
sometimes `meta`:

```
{"code":"schema_violation","error":"field #.msg: required field missing"}
{"code":"grant_required","error":"grant required for bob/mail","meta":{"action":"bob/mail"}}
```

`meta` carries what the program needs to act: the action to connect for
`grant_required`, the peer for `peer_unreachable` and `peer_unfunded`, and
`process_id`, `pending_since` and `refund_eligible_at` for a call that parked.

## Units

The command line takes and prints display units. The HTTP API and the JSON
arguments and results of actions use base units. On the shipped networks one
credit is 1,000,000 base units.

```
$ juice user transfer bob 1.5           # display units
```
```
POST /v1/run {"action":"sys/transfer","args":{"target":"bob","amount":1500000}}
```

Both move the same amount. `GET /v1/me` returns `"available": 4795000` where the
command line prints `4.795 credits`. Read `decimals` from `GET /health` rather
than assuming six.

## Separate planning from spending

`sys/llm/decide` chooses an action and proposes arguments. It never executes
anything:

```
$ juice --as bot@acme run sys/llm/decide '{
    "messages": [{"role":"user","content":"summarise this contract"}],
    "actions": ["bob/echo","dave@beta-kernel/summarize"]
  }' --json
```

It returns `{"action": …, "args": …}`. The contracts it reasons over are fetched
from the kernel by reference, not taken from the request, so a caller cannot feed
it a false description of an action. Proposed arguments are validated against the
real schema. A candidate on an unreachable kernel is dropped rather than blocking
the choice; if no model is configured the action reports an invalid state rather
than guessing.

Running what it proposes is a separate decision, and a separate command.

## Pin the terms between reading and running

Terms can change between the moment you read an action and the moment you call it.
Carry the `quote_hash` from the search result or the action read into the run:

```
$ juice --as bot@acme run bob/echo '{"msg":"hi"}' --quote-hash 4965342976414282…
```

A changed contract then fails with exit code 11 and charges nothing, instead of
buying something you did not plan.

## Retries

**`run` is not idempotent.** There is no idempotency key on a call. Running the
same command again buys the work a second time. If a run does not return, do not
re-run it: find out what happened first.

- A cross-kernel call that parked returns `process_id`, `pending_since` and
  `refund_eligible_at`. Poll `juice process show <id>` until it closes. A running
  kernel settles it when the receipt arrives, or as a refunded failure once
  `refund_eligible_at` has passed; a kernel that is stopped settles nothing until
  it is started again.
- A call that failed with exit code 9 provably never left your kernel and was
  fully refunded. It is safe to retry.

**Money commands do carry keys.** A transfer takes `--external-key` and a
withdrawal takes `--id`, both minted by you. Replaying with the same key returns
the original record rather than moving money again. Use them whenever a retry is
possible.

```
$ juice --as bot@acme user transfer bob 1 --external-key payout-2026-09-14-001 --yes
$ juice --as bot@acme user withdraw 5 --id wd-2026-09-14-001 --yes
```

**Completing a step cannot run it twice.** A repeat on your own kernel is
refused, because the step is no longer waiting; read the step to find the
transaction it produced. A repeat of a completion sent to another kernel returns
the original outcome, because its key is derived from the step and the input.

## Confirmation

Commands that move money ask before acting and refuse outright when there is no
terminal:

```
$ juice user transfer bob 1
error: re-run with --yes to confirm (no terminal to ask on)
```

Pass `--yes`. A `run` is not confirmed: issuing it is the consent, to its
advertised or pinned price and to any value its arguments name.

## Non-interactive equivalents

| Interactive | Non-interactive |
|---|---|
| password prompt on `auth login`, `user create` | `--password` |
| first-boot questions | a `config.json` written in advance, plus `JUICE_BOOTSTRAP_PASSWORD` |
| confirmation on a money command | `--yes` |
| the selected login | `--as` or `JUICE_AS` |

A password on a command line is visible to other processes. Prefer the environment
or a prompt where you can.

## Speaking HTTP directly

The command line is a client of the HTTP API and has no private access to it. Log
in with the authorisation-code flow and PKCE:

```
$ VERIFIER=$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')
$ CH=$(printf '%s' "$VERIFIER" | openssl dgst -sha256 -binary | base64 | tr '+/' '-_' | tr -d '=')
$ curl -s -X POST localhost:4040/v1/auth/authorize -H 'Content-Type: application/json' \
    -d "{\"handle\":\"bot\",\"password\":\"…\",\"code_challenge\":\"$CH\"}"
{"redirect":"?code=RF175R2g…"}
$ curl -s -X POST localhost:4040/v1/auth/token -H 'Content-Type: application/json' \
    -d "{\"code\":\"RF175R2g…\",\"code_verifier\":\"$VERIFIER\"}"
{"access_token":"eyJhbGciOi…","refresh_token":"…"}
```

Then use the access token as a bearer:

```
$ curl -s localhost:4040/v1/run -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' -d '{"action":"sys/time","args":{}}'
{"result":{"iso":"2026-09-14T12:07:31Z","unix":1789387651},"tx_id":"800c8279-…", …}
```

Access tokens are short-lived. On a 401, exchange the refresh token at
`POST /v1/auth/refresh`, which returns a new pair; the old refresh token stops
working.

Check `GET /health` before trusting a server, and compare what it reports against
the key and network you expect:

```
$ curl -s localhost:4040/health
{"decimals":6,"handle":"acme","network":"play","network_digest":"ef1fac03…",
 "public_key":"fdlMi64P…","rail_address":"","status":"ok","symbol":"credits"}
```

A port number is not an identity. A client that dials a port without checking will
talk to whichever kernel happens to hold it.

The full route table is in
[`API.md`](https://github.com/daios-ai/juice/blob/master/API.md). Federation has no
HTTP surface; kernels speak to each other over their own transport only.

## Services are not agents

A program that *implements* an action — the endpoint behind an `http` action — holds
no Juice login at all. It receives a capability on each dispatch and uses it to
compose within that one call. See
[Composing from an HTTP endpoint](providing/composition.html#composing-from-an-http-endpoint).

An agent spends money and therefore has an account. A service earns money and
therefore does not need one; its owner's account is where the earnings go.
