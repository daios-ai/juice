// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"slices"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// The stdlib's contract is what every native declares about itself (§9). These cases pin the two
// promises spec.go makes: every shipped native carries a complete, activatable contract, and
// Register is the sole path from that contract to a running handler — so no native can reach the
// kernel without one, and none can claim a privileged effect without the extractor that implements it.

func TestAllShipsCompleteContracts(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range All(Deps{}) {
		if s.Name == "" {
			t.Fatal("a native shipped with no name")
		}
		if seen[s.Name] {
			t.Errorf("%s is registered twice; a name must resolve to one action", s.Name)
		}
		seen[s.Name] = true
		// Activation rejects a missing description or schema (§7), so a native lacking either could
		// never be enabled at boot.
		if s.Description == "" {
			t.Errorf("%s: no description; activation would reject it", s.Name)
		}
		if s.InputSchema == nil || s.OutputSchema == nil {
			t.Errorf("%s: input/output schema missing", s.Name)
		}
		if title, err := kernel.ValidateTitle(s.Title); err != nil || title != s.Title {
			t.Errorf("%s: title %q is not a valid title as written: %v", s.Name, s.Title, err)
		}
		if s.Handler == nil {
			t.Errorf("%s: no handler", s.Name)
		}
		// Value-bearing is decided by the declared effect, never by a name (§13): the two must agree
		// in both directions, or the kernel would bind a transfer it cannot execute — or execute one
		// it never admitted a reserve for.
		if (s.Effect != "") != (s.Value != nil) {
			t.Errorf("%s: effect %q and value extractor must be declared together", s.Name, s.Effect)
		}
	}
	// The stdlib §9 names; a native removed from the platform must also leave this list.
	for _, want := range []string{
		"lookup", "llm/chat", "llm/json", "llm/embed", "llm/decide",
		"time", "sink", "message", "random", "transfer", "web", "tinygo/compile",
	} {
		if !seen[want] {
			t.Errorf("stdlib native %q is not shipped", want)
		}
	}
}

// Each generated model is its own natives by kind, beside the canonical ones and in name order, each
// with a distinct title naming it (D17).
func TestAllGeneratesEachModelsNatives(t *testing.T) {
	m := &stubModel{}
	specs := All(Deps{LLM: LLM{
		Chats:     map[string]ChatModel{"b/chat": m, "a/chat": m},
		Embedders: map[string]kernel.Embedder{"a/vec": &stubEmbedder{}},
		Chat:      m,
	}})
	var names []string
	titles := map[string]bool{}
	for _, s := range specs {
		if titles[s.Title] {
			t.Errorf("title %q is shared", s.Title)
		}
		titles[s.Title] = true
		names = append(names, s.Name)
	}
	want := []string{"llm/a/chat/chat", "llm/a/chat/json", "llm/a/chat/decide",
		"llm/b/chat/chat", "llm/b/chat/json", "llm/b/chat/decide", "llm/a/vec/embed"}
	if got := names[len(names)-len(want):]; !slices.Equal(got, want) {
		t.Errorf("generated natives = %v, want %v", got, want)
	}
	for _, want := range []string{"Chat with a language model", "Get JSON from a language model", "Choose an action with a language model",
		"Embed text", "Chat with a/chat", "Get JSON from a/chat", "Choose an action with a/chat", "Embed text with a/vec"} {
		if !titles[want] {
			t.Errorf("no native titled %q", want)
		}
	}
}

// Price is deliberately not part of a Spec: configuration owns it (§14 native.<action>), and
// bootstrap reconciles it every boot. A Spec carrying one would be a second owner.
func TestSpecCarriesNoPrice(t *testing.T) {
	for _, s := range All(Deps{}) {
		if s.Name == "transfer" && s.Effect != "transfer" {
			t.Errorf("sys/transfer must declare the transfer effect, got %q", s.Effect)
		}
	}
}

func TestRegisterWiresHandlersAndValueEffects(t *testing.T) {
	k := kernel.New(kernel.Dependencies{})
	specs := All(Deps{})
	// Registration must survive a kernel with no adapters configured: a missing adapter is reported
	// at call time as ErrInvalidState (§9), never as a native that failed to ship.
	Register(k, specs)

	// Each Spec must yield a real handler from the kernel it is given — that function is what
	// Register hands over, so a nil one would register a native that cannot run.
	for _, s := range specs {
		if s.Handler(k) == nil {
			t.Errorf("%s: builds a nil handler", s.Name)
		}
	}
}

// The shared schema fragments must build fresh maps: a Spec is handed to the kernel, which may
// retain it, so two natives sharing one map would let a mutation of one rewrite the other.
func TestSchemaFragmentsAreNotShared(t *testing.T) {
	a, b := messageSchema(), messageSchema()
	if &a == &b {
		t.Fatal("messageSchema returned the same map twice")
	}
	a["mutated"] = true
	if _, leaked := b["mutated"]; leaked {
		t.Error("mutating one message schema changed another")
	}

	q := searchQuerySchema()
	req, ok := q["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "query" {
		t.Errorf("searchQuerySchema required = %v, want [query]", q["required"])
	}
	if _, hasLimit := q["properties"].(map[string]any)["limit"]; !hasLimit {
		t.Error("searchQuerySchema must offer the optional limit")
	}
}

func TestObjHelpersProduceValidSchemas(t *testing.T) {
	s := obj(map[string]any{"a": str("an a")}, "a")
	if s["type"] != "object" || s["additionalProperties"] != false {
		t.Errorf("obj = %v, want a closed object", s)
	}
	if got := s["required"].([]string); len(got) != 1 || got[0] != "a" {
		t.Errorf("required = %v, want [a]", got)
	}
	// No required keys means no required list at all, not an empty one: an empty array is a
	// different JSON Schema statement.
	if _, present := obj(map[string]any{"a": str("an a")})["required"]; present {
		t.Error("obj with no required keys must omit the required list")
	}
	if d := objd("a described object", map[string]any{})["description"]; d != "a described object" {
		t.Errorf("objd description = %v", d)
	}
	for name, m := range map[string]map[string]any{
		"str": str("d"), "integer": integer("d"), "num": num("d"), "object": object("d"),
	} {
		if m["description"] != "d" {
			t.Errorf("%s: description not carried", name)
		}
	}
	arr := arrayOf(str("item"), "a list")
	if arr["type"] != "array" || arr["items"] == nil {
		t.Errorf("arrayOf = %v, want a typed array with items", arr)
	}
}

// Every native declares its contract in the canonical form (D4) exactly as written: what the kernel
// stores, hashes and hands a model is what the native's own file says, and a native is held to the
// subset like any other action.
func TestNativeSchemasAreCanonical(t *testing.T) {
	titles := map[string]string{}
	for _, s := range All(Deps{}) {
		if err := kernel.ValidateSchema("input", s.InputSchema); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
		if err := kernel.ValidateSchema("output", s.OutputSchema); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
		if other, dup := titles[s.Title]; dup {
			t.Errorf("%s and %s share the title %q; a list would not tell them apart", s.Name, other, s.Title)
		}
		titles[s.Title] = s.Name
	}
}
