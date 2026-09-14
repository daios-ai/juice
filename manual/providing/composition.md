---
title: Composition
parent: Providing actions
nav_order: 3
---

# Composition

An action can call other actions. The buyer sees one price, one result and one
party to rate; you pay the sub-providers out of the price you charged.

## The budget rule

A sub-call spends from the budget of the call that made it. A sub-call costing
more than the budget has left is refused:

```
error: wasm execution failed: run failed: parent trace has 100000 credits, action costs 750000
```

In a WebAssembly action the refusal stops the module, so the whole call fails and
the buyer is refunded what was not consumed. Check the price of what you intend to
call against the budget you have, rather than relying on handling the failure. An
HTTP action receives the refusal as an ordinary error from `POST /v1/call` and can
carry on.

This is what makes the advertised price a bound. There is no path by which work
beneath a call can exceed the money set aside for it.

## Composing from WebAssembly

A `wasm` action is compiled code the kernel runs in a sandbox. It has no
filesystem, no network, no environment variables and no access to any credential.
It can do four things through the host:

| Host function | Effect |
|---|---|
| `JuiceCall(action, args)` | call another action from this call's budget |
| `JuiceStepCreate(partialArgs, requiredCaller, action)` | set work aside for a named party |
| `JuiceStepComplete(stepID, input)` | complete a step this call created |
| `JuiceLog(level, msg)` | write a log record against this call |

To reach the web, call `sys/web` or an `http` action of your own. Both go through
the ordinary call machinery, so they are priced and confined like anything else.

### Writing and compiling a handler

You write one function, `Handle`. The kernel supplies everything around it.

```go
func Handle(in map[string]any) (map[string]any, error) {
	raw, err := JuiceCall("sys/time", []byte("{}"))
	if err != nil {
		return nil, err
	}
	var now struct {
		ISO string `json:"iso"`
	}
	if err := json.Unmarshal(raw, &now); err != nil {
		return nil, err
	}
	return map[string]any{"note": in["note"], "stamped_at": now.ISO}, nil
}
```

`encoding/json`, `strings`, `strconv`, `math`, `sort` and `errors` are available.
`fmt` is not, because it pulls reflection into the binary.

Compile it on the kernel with `sys/tinygo/compile`, which returns the module as
base64:

```
$ juice run sys/tinygo/compile "$(jq -Rs '{source: .}' handler.go)" --json \
    | jq -r .result.artifact | base64 -d > stamp.wasm
```

Registration takes the decoded file:

```
$ juice action create stamp --kind wasm --artifact stamp.wasm --price 1 \
    --description "Stamp a note with the current time" \
    --input-schema '{"type":"object","properties":{"note":{"type":"string","description":"text to stamp"}},"required":["note"]}'
$ juice action enable bob/stamp
$ juice action update bob/stamp --visibility local
```

Running it shows the sub-call's result folded into your own:

```
$ juice run bob/stamp '{"note":"invoice 42"}'
  result: {
    "note": "invoice 42",
    "stamped_at": "2026-09-14T12:06:04Z"
  }
  …
```

A compile failure is reported in the result rather than as an error, and is
charged as a failed output:

```
  result: {
    "status": "failure",
    "diagnostics": [ … ]
  }
```

If the toolchain is not installed on the kernel, `sys/tinygo/compile` reports an
invalid state and charges nothing. You can also compile elsewhere and register the
`.wasm` file directly.

## Composing from an HTTP endpoint

An `http` action can compose too, without holding any Juice credential.

Each dispatch to your endpoint carries two headers:

```
X-Juice-Callback: http://127.0.0.1:4040
X-Juice-Capability: <trace-id>.<signature>
```

Send the capability back in that same header, to the callback address, to make
sub-calls under the same budget:

```
POST http://127.0.0.1:4040/v1/call
X-Juice-Capability: <the capability, verbatim>
Content-Type: application/json

{"action": "carol/extract", "args": {"text": "…"}}
```

```
{"result": {"…"}, "tx_id": "…", "trace_id": "…"}
```

It is not an `Authorization: Bearer` credential, and presenting it as one is
refused:

```
{"code":"unauthenticated","error":"capability required"}
```

The same header authorises `POST /v1/steps` and
`POST /v1/steps/{id}/complete`.

The capability acts as your action's owner, inside this one call, for as long as
the call is open. It is not a login: it carries no wallet, reaches no other call,
and cannot rate anything. It stops working the moment the call settles.

A service written this way holds nothing kernel-specific. It can serve several
kernels at once, because every request tells it where to call back and with what.

The capability never crosses to another kernel. An action of yours that composes
this way is served abroad as an ordinary endpoint, bounded by its advertised
price.

## Reaching your own private actions

Your public action may call your own private actions, in anyone's process. This is
how you build on helpers you do not want to sell.

The reverse does not hold. An action somebody else owns, running inside a process
your buyer funded, cannot reach that buyer's private actions. Funding code does not
lend it your authority.
