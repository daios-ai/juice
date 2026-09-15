---
title: Composition
parent: Providing actions
nav_order: 3
---

# Composition

Composition allows an action to use other services as part of its
implementation. The caller purchases the resulting service at your advertised
price, while your action allocates parts of that budget to the providers it
uses. You remain responsible for the combined result and receive the rating
for the call the buyer made.

## The budget rule

A child call receives its allocation from the remaining budget of its parent.
If the requested action costs more than the parent has available, the kernel
rejects the child call before it executes:

```
error: wasm execution failed: run failed: parent trace has 100000 credits, action costs 750000
```

The current WebAssembly host stops the module on this refusal, causing the
parent call to fail and refund its unused budget. Check expected child prices
when designing the action. An HTTP implementation receives an error from
`POST /v1/call` and can decide how to handle it.

Applying the same budget rule at each level bounds execution spending throughout
the call tree. Separate value transfers and remote ticket stakes use the
immediate caller's balance, as described in [Earnings](earnings.html).

## Composing from WebAssembly

A `wasm` action runs a compiled WebAssembly module inside the kernel's sandbox.
The module has no direct access to the filesystem, network, environment, or
credentials. It interacts with Juice through four host functions:

| Host function | Effect |
|---|---|
| `JuiceCall(action, args)` | call another action from this call's budget |
| `JuiceStepCreate(partialArgs, requiredCaller, action)` | set work aside for a named party |
| `JuiceStepComplete(stepID, input)` | complete a step this call created |
| `JuiceLog(level, msg)` | write a log record against this call |

A module can reach a web service by calling `sys/web` or a registered HTTP
action. These requests use the ordinary action budget and the kernel's outbound
network policy.

### Writing and compiling a handler

The supplied compilation interface expects a `Handle` function. It supplies
the surrounding module code, including the entry point and host bindings.
This example calls `sys/time` and combines its result with a note from the input:

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

The supplied imports include `encoding/json`, `strings`, `strconv`, `math`,
`sort`, and `errors`; they do not include `fmt`. Save the function in
`handler.go`, then call `sys/tinygo/compile`. Its result contains a base64
module, which the following pipeline decodes into a file:

```
$ juice run sys/tinygo/compile "$(jq -Rs '{source: .}' handler.go)" --json \
    | jq -r .result.artifact | base64 -d > stamp.wasm
```

Register the decoded module as a WebAssembly action, then enable it and choose
its audience:

```
$ juice action create stamp --kind wasm --artifact stamp.wasm --price 1 \
    --description "Stamp a note with the current time" \
    --input-schema '{"type":"object","properties":{"note":{"type":"string","description":"text to stamp"}},"required":["note"]}'
$ juice action enable bob/stamp
$ juice action update bob/stamp --visibility local
```

The action returns the input note together with the timestamp obtained by its
child call:

```
$ juice run bob/stamp '{"note":"invoice 42"}'
  result: {
    "note": "invoice 42",
    "stamped_at": "2026-09-14T12:06:04Z"
  }
  …
```

A source compilation failure is reported in the compiler action's result.
The compilation service is still charged, since it performed the requested
compilation attempt:

```
  result: {
    "status": "failure",
    "diagnostics": [ … ]
  }
```

If the kernel lacks the toolchain, the compilation action is unavailable and
reports an invalid state without charging. A module compiled elsewhere can also
be registered from its `.wasm` file.

## Composing from an HTTP endpoint

An HTTP implementation can request child calls through a callback to the kernel.
The dispatched request supplies a callback address and a capability authorizing
work within that call, so the endpoint does not need a saved Juice login.
When a callback address is available, the request includes:

```
X-Juice-Callback: http://127.0.0.1:4040
X-Juice-Capability: <trace-id>.<signature>
```

To make a child call, send the capability in the same header to the callback
address. The kernel allocates the child's price from the current call's budget:

```
POST http://127.0.0.1:4040/v1/call
X-Juice-Capability: <the capability, verbatim>
Content-Type: application/json

{"action": "carol/extract", "args": {"text": "…"}}
```

```
{"result": {"…"}, "tx_id": "…", "trace_id": "…"}
```

The current HTTP interface expects the `X-Juice-Capability` header. Supplying
the capability as an `Authorization: Bearer` token is rejected:

```
{"code":"unauthenticated","error":"capability required"}
```

The header also authorizes step creation and completion through
`POST /v1/steps` and `POST /v1/steps/{id}/complete`, subject to the step rules.
Completion is restricted to steps created by this trace and addressed to the
executing action's owner.

The capability acts with the action owner's authority inside the current call.
It cannot spend another call's budget, access account management, or submit
ratings, and it expires when the call settles. An endpoint can therefore serve
several kernels by using the callback information in each request.

This capability remains local to the executing kernel. Remote buyers invoke
your action through ordinary federation; the endpoint's child calls still use
the budget and callback supplied by your own kernel.

## Reaching your own private actions

Child calls are authorized as the composing action's owner. Your public action
can therefore use your private helpers even when another user funds the process.
It does not gain access to that user's private actions: paying for a service
does not grant its implementation the payer's permissions.
