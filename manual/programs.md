---
title: Using Juice from a program
nav_order: 7
---

# Using Juice from a program

A program can use Juice through the command-line client or directly through
the HTTP API. Both use the same account permissions and payment rules as an
interactive user. Automation requires particular care with login selection,
units, errors, and retries, because the program must make decisions that a
person would otherwise make at the terminal.

Give an agent its own account so that its balance and history can be managed
independently. The account grants no special privileges; it authorizes the
agent's calls in the same way as any other user's.

## Name the login on every command

Specify the saved login for each invocation with `--as`:

```
$ juice --as bot@acme run sys/lookup '{"query":"translate to german"}' --json
```

Alternatively, set `JUICE_AS=bot@acme` in the environment. Omit `--as` for
`kernel` and `auth` commands and `user create`; those commands refuse it.

Explicit selection keeps later invocations tied to the intended account even
when a person changes the client's current login. If the named login does not
exist, the command fails instead of selecting another account.

For persistent records, identify the kernel by public key and network digest
and the account by its ID. You can obtain these from `kernel list` and
`user me`. Handles and local kernel names are useful for interaction but may
be renamed or reused.

## Output

Use `--json` when the program needs to parse a successful response. It preserves
the server's reply structure. Use `--quiet` when only the returned identifiers
are needed, one per line:

```
$ juice --as bot@acme run sys/time --json
{
  "result": { "iso": "2026-09-14T12:06:52Z", "unix": 1789387612 },
  "tx_id": "98bb64e6-…",
  "trace_id": "f4fbbf15-…",
  "receipt_id": "0d60120d-…",
  "process_id": "e9dbb283-…",
  "charge": 0
}
$ juice --as bot@acme run sys/time --quiet
821a9f33-…
```

`POST /v1/run` returns `charge` in integer base units once the call settles.
A free call has `charge: 0`; a deferred settlement omits the field until the
charge is known. Choose either `--json` or `--quiet`; using both is refused.

## Handling errors

The command line reports an error on stderr and sets an exit status. The
`--json` option applies to successful replies, so it does not make command-line
errors machine-readable JSON. Branch on the exit status using these meanings:

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
| 9 | server unreachable, or peer provably not reached |
| 10 | peer will not serve on credit |
| 11 | terms changed |

Message text is intended for people and should not be used as a program's error
classifier. Over HTTP, the response instead contains a stable `code`, a message,
and, where applicable, `meta`:

```
{"code":"schema_violation","error":"field msg: required field missing"}
{"code":"grant_required","error":"grant required for bob/mail","meta":{"action":"bob/mail"}}
```

The metadata supplies context for recovery. For example, `grant_required`
identifies the action requiring consent, while peer errors identify the remote
kernel. A pending call includes `process_id`, `pending_since`, and
`refund_eligible_at`, allowing the program to follow its existing execution.
A settled failure carries `tx_id` and `charge` in the error's `meta`; the charge
is a decimal string in base units, and can be zero.

## Units

Convert amounts at the interface boundary. The command line accepts display
units, while the HTTP API and action arguments and results use integer base
units. On the shipped networks, one display unit contains 1,000,000 base units:

```
$ juice user transfer bob 1.5           # display units
```
```
POST /v1/run {"action":"sys/transfer","args":{"target":"bob","amount":1500000}}
```

Both examples deliver the same amount, although `sys/transfer` may also have
an execution price. Similarly, `GET /v1/me` returns `"available": 4795000`
where the command line displays `4.795 fUSDT`. Read `decimals` from
`GET /health` when calculating conversions instead of hard-coding six.

## Separate planning from spending

The built-in `sys/llm/decide` action can choose among candidate actions and
propose their arguments. Calling it purchases the selection service, but does
not execute the action it selects:

```
$ juice --as bot@acme run sys/llm/decide '{
    "messages": [{"role":"user","content":"summarise this contract"}],
    "actions": ["bob/echo","dave@beta-kernel/summarize"]
  }' --json
```

The result contains `{"action": …, "args": …}`. The kernel resolves each
candidate's actual contract and validates proposed arguments against its schema.
An unreachable remote candidate can be discarded so that selection continues
among available choices; an unavailable model produces an invalid-state error.

Your program can inspect the proposal before issuing a separate `run`. This
keeps the decision to spend on the selected service under the program's control.

## Pin the terms between reading and running

An action's terms can change while a program prepares work. Carry the
`quote_hash` from the selected search result or action read into the execution
request:

```
$ juice --as bot@acme run bob/echo '{"msg":"hi"}' --quote-hash 4965342976414282…
```

A mismatch on an otherwise callable action produces exit code 11 before any
charge. The program can then obtain the new terms and decide whether to proceed.
If the action is inactive, that earlier precondition fails instead.

## Retries

Repeating `run` starts another purchase. The public run request has no
idempotency key, so a program must establish the outcome of an earlier request
before deciding whether to submit it again. Remote transport retries within
the kernel are different: they retain the original call's identity.

- A cross-kernel call that parked returns `process_id`, `pending_since` and
  `refund_eligible_at`. Poll `juice process show <id>` until it closes. A running
  kernel settles it when the receipt arrives, or as a refunded failure once
  `refund_eligible_at` has passed; a kernel that is stopped settles nothing until
  it is started again.
- A call that failed with exit code 9 provably never left your kernel and was
  fully refunded. It is safe to retry.

Transfers and withdrawals provide explicit retry keys: `--external-key` for a
transfer and `--id` for a withdrawal. Generate and save the key before issuing
the request, then reuse it with the same terms if a retry is needed. The kernel
returns the existing movement rather than creating a second one.

```
$ juice --as bot@acme user transfer bob 1 --external-key payout-2026-09-14-001 --yes
$ juice --as bot@acme user withdraw 5 --id wd-2026-09-14-001 --yes
```

Step completion also prevents duplicate execution. A repeated local completion
is refused after the step has been claimed or completed; read its record to
find the resulting transaction. A remote completion derives its retry key from
the step and input, allowing the same request to recover its stored outcome.

## Confirmation

Transfers and withdrawals require confirmation. Without a terminal or explicit
confirmation, the client refuses to act:

```
$ juice user transfer bob 1
error: re-run with --yes to confirm (no terminal to ask on)
```

Supply `--yes` when the program has authorized that movement. An action run
does not prompt: issuing the request authorizes the selected price and any
value named in its arguments.

## Non-interactive equivalents

| Interactive | Non-interactive |
|---|---|
| password prompt on `auth login`, `user create` | `--password` |
| first-boot questions | a `config.json` written in advance, plus `JUICE_BOOTSTRAP_PASSWORD` |
| confirmation on a money command | `--yes` |
| the selected login | `--as` or `JUICE_AS` |

A password supplied as a command-line argument may be visible to other
processes. For long-running programs, establish a saved login during setup
and use its managed session for subsequent commands.

## Speaking HTTP directly

A direct HTTP client uses the same API as the command line. Before sending
credentials, check `GET /health` against the expected kernel key and network,
as shown below. Authentication uses an authorization code with PKCE: the
client generates a verifier, sends its derived challenge when authenticating,
and presents the verifier when exchanging the code for tokens.

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

Access tokens expire. On a 401, the client can exchange its refresh token at
`POST /v1/auth/refresh` for a new pair. Persist the replacement refresh token
before the next refresh, since the old one is no longer valid.

Check `GET /health` before trusting a server, and compare what it reports against
the key and network you expect:

```
$ curl -s localhost:4040/health
{"decimals":6,"fed_addrs":["/ip4/127.0.0.1/tcp/31313/p2p/12D3KooWJHdK…"],
 "handle":"acme","network":"play","network_digest":"ef1fac03…",
 "public_key":"fdlMi64P…","rail_address":"","status":"ok","symbol":"fUSDT","token":""}
```

This check distinguishes the expected kernel from any other server occupying
the same address. Save the expected identity when establishing trust and compare
subsequent responses against it.

The full HTTP interface is documented in
[`API.md`](https://github.com/daios-ai/juice/blob/master/API.md).
Kernel-to-kernel federation uses a separate transport managed by the kernel.

## Implementing a service

A program implementing an HTTP action has a different role from an agent that
buys services. The endpoint receives an execution capability with each
dispatched call and can use it to request work within that call's budget.
It therefore needs no saved Juice login for composition. See
[Composing from an HTTP endpoint](providing/composition.html#composing-from-an-http-endpoint).

The action's owner holds the account that receives its earnings. An agent
making independent purchases needs its own account and login to authorize them.
