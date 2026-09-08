package native

import (
	"context"
	"encoding/json"

	"github.com/daios-ai/juice/kernel"
)

// Spec is one native action's self-description: everything the platform stdlib entry declares
// about itself (§9) — its name, privileged effect, natural-language description, schemas, and the
// handler that runs it. Each native's own file supplies its Spec through a constructor, so a
// contract change lands in exactly one file instead of being restated in bootstrap and main.
//
// Price is deliberately absent: configuration owns prices and their defaults (§14 `native.<action>`),
// and bootstrap reconciles each Spec against the configured price on every boot.
type Spec struct {
	Name         string
	Effect       string // privileged execution effect ("transfer"); empty for an ordinary native (§13)
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	// Handler builds the action's runtime handler over the capabilities a native may reach. It is
	// an interface, not the kernel, so the confinement §9 states is checked by the compiler: a
	// native composes through the same public entry points any action has, and can reach nothing
	// else — not the ledger, not configuration, not the signing keys.
	Handler func(Host) kernel.NativeFunc
	// Value, when set, extracts (amount, beneficiary) for a value-bearing effect (§13). Registered
	// under Effect, so the kernel binds the effect without ever naming the action.
	Value kernel.ValueFunc
}

// Host is everything a running native may ask of the kernel: resolve an action, read one it may
// call, rank the catalogue, name a peer, park a step. *kernel.Kernel satisfies it, and nothing in
// this package can widen it — adding a capability is an edit here, in the open.
type Host interface {
	Lookup(ctx context.Context, req kernel.LookupRequest) ([]*kernel.LookupResult, error)
	KernelName(ctx context.Context, publicKey string) string
	ResolveAction(ctx context.Context, ref string) (*kernel.Action, error)
	ReadCallableAction(ctx context.Context, ref, callerID string) (*kernel.Action, error)
	ResolveRequiredCaller(ctx context.Context, ref string) (callerID, remoteID string, err error)
	CreateStep(ctx context.Context, traceID, actionID string, partialArgs json.RawMessage, requiredCallerID, requiredCallerRemoteID string) (*kernel.Step, error)
}

// Deps carries the adapters the stdlib natives need from cmd/juice. A nil adapter is a
// misconfiguration the handler reports at call time (ErrInvalidState), never a missing action.
type Deps struct {
	Chatter    kernel.Chatter
	Embedder   kernel.Embedder
	JSON       kernel.JSONChatter
	Decide     kernel.DecideChatter
	Web        WebDeps
	Compile    CompileDeps
	CompileSDK string
}

// All returns every native the platform ships, in registration order.
func All(d Deps) []Spec {
	return []Spec{
		Lookup(),
		Chat(d.Chatter), Embed(d.Embedder), JSON(d.JSON), Decide(d.Decide),
		Time(), Sink(), Message(), Random(), Transfer(),
		Web(d.Web), TinyGo(d.Compile, d.CompileSDK),
	}
}

// Register wires each Spec's handler (and value extractor, if any) onto k. It is the sole
// registration path, so a native cannot be shipped without its contract.
func Register(k *kernel.Kernel, specs []Spec) {
	var host Host = k // the fence: a handler is built over the interface, never the kernel
	for _, s := range specs {
		k.RegisterNativeHandler(s.Name, s.Handler(host))
		if s.Value != nil {
			k.RegisterValueAction(s.Effect, s.Value)
		}
	}
}

// ---- shared schema fragments ----

// obj builds an object schema from property pairs, with the named keys required.
func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// objd is obj with a description — a nested object that documents itself (activation requires a
// description on every property, §7).
func objd(desc string, props map[string]any, required ...string) map[string]any {
	s := obj(props, required...)
	s["description"] = desc
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func num(desc string) map[string]any    { return map[string]any{"type": "number", "description": desc} }
func object(desc string) map[string]any { return map[string]any{"type": "object", "description": desc} }
func arrayOf(items map[string]any, desc string) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}

// messageSchema is the {role, content} turn both plain LLM natives take. A fresh map per call: a
// Spec is handed to the kernel, which may retain it, so schemas must not share state.
func messageSchema() map[string]any {
	return obj(map[string]any{
		"role":    str("Role of the message sender (user or assistant)"),
		"content": str("Text content of the message"),
	}, "role", "content")
}

// searchQuerySchema is the {query, limit} input both lookup natives take.
func searchQuerySchema() map[string]any {
	return obj(map[string]any{
		"query": str("Semantic search query"),
		"limit": integer("Maximum number of results"),
	}, "query")
}
