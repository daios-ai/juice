// SPDX-License-Identifier: AGPL-3.0-only

package rail_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daios-ai/juice/rail"
	"github.com/ethereum/go-ethereum/rpc"
)

// playDigest is the network every kernel joins by default. It is pinned because it rides inside
// every signature: if it moves, every kernel on the play network stops verifying the others, so a
// change here must be a deliberate protocol break rather than an accident of refactoring.
const playDigest = "ef1fac03f5f78ca42dfa05b9eb975b5e0944e013ed1eb5ea30a2be9328e34a67"

// The chain networks are pinned for the same reason, and for one more: they are what says where
// real money is paid. A label may be corrected, a description reworded; the moment either of these
// moves, the file names a different network and nothing on the old one verifies here.
const (
	testDigest = "8e0041de41ec5e9ee0ebb2eed05d51f93e704b5e8c86975f6a30f72237b45a77"
	realDigest = "c86252cb887f73499149126ad3918f379592d59f2c2e1573c13dc681c2858e7a"
)

// The shipped worlds are pinned whole: name, digest, decimals, token and symbol together. The
// symbol is what a depositor is shown and the token is what they must send; a file that names one
// while holding the other tells them to send the wrong money, which is how these two worlds came to
// say USDC while holding Tether. Changing either alone fails here.
func TestShippedWorldsLoad(t *testing.T) {
	for _, tc := range []struct {
		name     string
		chained  bool
		decimals uint8
		digest   string
		token    string
		symbol   string
	}{
		{"play", false, 6, playDigest, "", "credits"},
		{"test", true, 6, testDigest, "0x8e87deee3bf1efe27e8e96abf205bedf802ed568", "USDT"},
		{"real", true, 6, realDigest, "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", "USDT"},
	} {
		w, err := rail.Load(tc.name)
		if err != nil {
			t.Fatalf("load %s: %v", tc.name, err)
		}
		if w.Name != tc.name || w.Chained() != tc.chained {
			t.Fatalf("%s: name=%q chained=%v", tc.name, w.Name, w.Chained())
		}
		n := w.Network()
		if n.Decimals != tc.decimals || n.Digest != tc.digest {
			t.Fatalf("%s: network %+v, want decimals %d digest %s", tc.name, n, tc.decimals, tc.digest)
		}
		if n.Token != tc.token || n.Symbol != tc.symbol {
			t.Fatalf("%s: token %q symbol %q, want %q %q", tc.name, n.Token, n.Symbol, tc.token, tc.symbol)
		}
		if w.Description == "" {
			t.Fatalf("%s: no description; it is the line an operator chooses a network by", tc.name)
		}
		if tc.chained {
			if _, err := w.Domain(); err != nil {
				t.Fatalf("%s: domain: %v", tc.name, err)
			}
		}
	}
}

// Relabelling is not a new network: the symbol and description are what a person reads, and the
// digest is what signatures carry. This is the guard that correcting a label never strands a kernel.
func TestLabelsDoNotMoveTheDigest(t *testing.T) {
	raw, err := os.ReadFile("worlds/real.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["symbol"] = "SOMETHING-ELSE"
	doc["description"] = "reworded entirely"
	if got := writeWorld(t, doc); got != realDigest {
		t.Fatalf("a label moved the digest: %s (was %s)", got, realDigest)
	}
}

// An empty world name means play, so a kernel with no configured world still joins one network.
// A world must be named. An empty name was play, which made a kernel whose configuration said
// nothing about its network bind itself to one anyway — the one choice it can never revise.
func TestEmptyNameIsRefused(t *testing.T) {
	if w, err := rail.Load(""); err == nil {
		t.Fatalf("an unnamed world resolved to %q", w.Name)
	}
}

func TestPlayDigestIsPinned(t *testing.T) {
	w, _ := rail.Load("play")
	if got := w.Network().Digest; got != playDigest {
		t.Fatalf("play digest moved: %s (was %s) — this breaks every signature on the play network", got, playDigest)
	}
}

// The shipped worlds must be three distinct networks, or an artifact of one would verify on another.
func TestShippedWorldsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, n := range []string{"play", "test", "real"} {
		w, _ := rail.Load(n)
		d := w.Network().Digest
		if other, dup := seen[d]; dup {
			t.Fatalf("%s and %s share a digest", n, other)
		}
		seen[d] = n
	}
}

// The digest covers what every member shares and nothing else: two operators of one network run
// different endpoints and gas policies, and must still verify each other's signatures.
func TestOperationalFieldsDoNotMoveTheDigest(t *testing.T) {
	raw, err := os.ReadFile("worlds/real.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	base := writeWorld(t, doc)

	doc["rpc"] = "https://example.invalid/rpc"
	doc["fromBlock"] = float64(1)
	doc["gas"].(map[string]any)["slippageBps"] = float64(999)
	moved := writeWorld(t, doc)

	if base != moved {
		t.Fatalf("an operational change moved the digest: %s vs %s", base, moved)
	}

	doc["chainId"] = float64(1)
	if writeWorld(t, doc) == base {
		t.Fatal("a different chain must be a different network")
	}
}

func writeWorld(t *testing.T, doc map[string]any) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "w.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := rail.Load(p)
	if err != nil {
		t.Fatalf("load written world: %v", err)
	}
	return w.Network().Digest
}

func TestLoadRejectsBadWorlds(t *testing.T) {
	if _, err := rail.Load("prod"); err == nil || !strings.Contains(err.Error(), "unknown world") {
		t.Fatalf("a bare typo must not be read as a path: %v", err)
	}
	if _, err := rail.Load("./nope.json"); err == nil {
		t.Fatal("missing file must error")
	}
	for name, doc := range map[string]map[string]any{
		"no name":     {"decimals": 0},
		"half chain":  {"name": "x", "chainId": float64(1)},
		"no decimals": {"name": "x", "chainId": float64(1), "token": "0x0000000000000000000000000000000000000001"},
		// the ledger holds 64-bit integers; a token needing more than 18 places could not be held
		"too many decimals": {"name": "x", "chainId": float64(1), "token": "0x0000000000000000000000000000000000000001", "decimals": float64(24)},
		// a setting this build does not know would silently have no effect at all
		"unknown key": {"name": "x", "decimals": float64(6), "somethingElse": float64(1)},
		// the ticket ceiling moved to the kernel's own lottery_max; a file still naming it here
		// would be read as a rule that no longer applies
		"the retired ceiling": {"name": "x", "decimals": float64(6), "lotteryMax": float64(100)},
	} {
		b, _ := json.Marshal(doc)
		p := filepath.Join(t.TempDir(), "w.json")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := rail.Load(p)
		if err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
		if name == "the retired ceiling" && !strings.Contains(err.Error(), "lotteryMax") {
			t.Errorf("the refusal must name the key it refused: %v", err)
		}
	}
	// One document per file. A second one is a world somebody meant to serve, and reading only the
	// first would serve the other silently.
	p := filepath.Join(t.TempDir(), "two.json")
	if err := os.WriteFile(p, []byte(`{"name":"x","decimals":6} {"name":"y","decimals":6}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rail.Load(p); err == nil {
		t.Error("a file holding two worlds was read as one")
	}
}

// The settlement tag is the world's choice. Below true finality a settled fact is the sequencer's
// word and the kernel prices that risk through the remote premium, so a world may say `latest`,
// `safe` or `finalized`; anything else is refused, and an absent tag is the strictest.
func TestFinalityIsTheWorldsChoice(t *testing.T) {
	base, err := rail.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tag  string
		want rpc.BlockNumber
	}{{"latest", rpc.LatestBlockNumber}, {"safe", rpc.SafeBlockNumber},
		{"finalized", rpc.FinalizedBlockNumber}, {"", rpc.FinalizedBlockNumber}} {
		w := base
		w.Finality = tc.tag
		d, err := w.Domain()
		if err != nil {
			t.Fatalf("finality %q: %v", tc.tag, err)
		}
		if d.Finality != tc.want {
			t.Errorf("finality %q: domain has %s, want %s", tc.tag, d.Finality, tc.want)
		}
	}
	w := base
	w.Finality = "soon"
	if _, err := w.Domain(); err == nil {
		t.Error("an unknown settlement tag was accepted; the rail would credit on nothing")
	}
	// The shipped test world credits at Arbitrum's own confirmation, which is the speed it was
	// chosen for. The real world stays at finalized until the operator decides otherwise.
	if base.Finality != "latest" {
		t.Errorf("the shipped test world settles at %q, want latest", base.Finality)
	}
	real, _ := rail.Load("real")
	if real.Finality != "finalized" {
		t.Errorf("the shipped real world settles at %q; changing it is an operator decision", real.Finality)
	}
}
