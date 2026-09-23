// SPDX-License-Identifier: AGPL-3.0-only

package rail_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/daios-ai/juice/rail"
	"github.com/ethereum/go-ethereum/rpc"
)

// The shipped networks are pinned. A network's fingerprint rides inside every signature and names
// the namespace its kernels find each other on, so if one moves, every kernel on that network stops
// verifying the others and every receipt already stored reports invalid. A change here must be a
// deliberate protocol break rather than an accident of refactoring.
const (
	playDigest     = "baed18ae3f0c2b63f04f593d113a8af947eea60410d2748771d4ad38df9ecf96"
	sepoliaDigest  = "42f84b8255e166d4c7b419e34d031bb7f6aed83afa5bde4d0f2a1c8a39313e08"
	arbitrumDigest = "04a8e2ce745261cc9cc92214d5ba3ce4b327c8949eb57c7e25b44695608dfef0"
)

// installed is an installation's worlds directory: what `kernel serve` writes before it looks a
// world up, and the only place a world is read from.
func installed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := rail.Install(dir); err != nil {
		t.Fatalf("install: %v", err)
	}
	return dir
}

// The shipped worlds are pinned whole: name, adaptor, digest, decimals, token and symbol together.
// The symbol is what a depositor is shown and the token is what they must send; a file that names
// one while holding the other tells them to send the wrong money. Changing either alone fails here.
func TestShippedWorldsLoad(t *testing.T) {
	dir := installed(t)
	for _, tc := range []struct {
		name     string
		rail     string
		decimals uint8
		digest   string
		token    string
		symbol   string
	}{
		{"play", rail.RailManual, 6, playDigest, "", "fUSD"},
		{"arbitrum-sepolia", rail.RailEVM, 6, sepoliaDigest, "0x8e87deee3bf1efe27e8e96abf205bedf802ed568", "USDT"},
		{"arbitrum-one", rail.RailEVM, 6, arbitrumDigest, "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", "USDT"},
	} {
		w, err := rail.Load(dir, tc.name)
		if err != nil {
			t.Fatalf("load %s: %v", tc.name, err)
		}
		// The file carries no name of its own: the file is the name, so the two cannot disagree.
		if w.Name != tc.name || w.Rail != tc.rail {
			t.Fatalf("%s: name=%q rail=%q", tc.name, w.Name, w.Rail)
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
		if tc.rail == rail.RailEVM {
			if _, err := w.Domain(); err != nil {
				t.Fatalf("%s: domain: %v", tc.name, err)
			}
		}
	}
}

// Two kernels of one installation may be started at once, and each installs the worlds it ships
// before looking one up. They must converge on one file per world rather than truncate or remove
// each other's half-written copies.
func TestConcurrentInstallsConverge(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rail.Install(dir); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent install: %v", err)
	}
	// Every world is there, whole, and nothing is left behind.
	for _, n := range []string{"play", "arbitrum-sepolia", "arbitrum-one"} {
		if _, err := rail.Load(dir, n); err != nil {
			t.Errorf("after concurrent installs, %s: %v", n, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("the worlds directory holds %d files, want the three shipped worlds", len(entries))
	}
}

// Installing is write-once: an operator's edit — their own node, their own meeting point — is what
// that kernel serves, and an upgrade carrying a newer file must not silently undo it. A world the
// operator adds is served by its own name like any other.
func TestInstallWritesOnceAndKeepsEdits(t *testing.T) {
	dir := installed(t)
	for _, n := range []string{"play", "arbitrum-sepolia", "arbitrum-one"} {
		if _, err := os.Stat(filepath.Join(dir, n+".json")); err != nil {
			t.Fatalf("install left no %s: %v", n, err)
		}
	}
	// An edited world survives the next boot's install, which finds the file already there.
	edited := filepath.Join(dir, "arbitrum-sepolia.json")
	raw, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["rpc"] = "https://my-own-node.example/rpc"
	writeDoc(t, edited, doc)
	if err := rail.Install(dir); err != nil {
		t.Fatalf("second install: %v", err)
	}
	w, err := rail.Load(dir, "arbitrum-sepolia")
	if err != nil {
		t.Fatal(err)
	}
	if w.RPC != "https://my-own-node.example/rpc" {
		t.Fatalf("an upgrade overwrote an edited world: rpc=%q", w.RPC)
	}
	// A world of one's own is a file: no registration, no list in the program.
	mine := map[string]any{"rail": "manual", "decimals": 6, "symbol": "MINE",
		"description": "a network of my own", "seeds": []string{}}
	writeDoc(t, filepath.Join(dir, "mine.json"), mine)
	own, err := rail.Load(dir, "mine")
	if err != nil {
		t.Fatalf("load own world: %v", err)
	}
	if own.Name != "mine" || own.Network().Digest == w.Network().Digest {
		t.Fatalf("own world %+v shares a network with a shipped one", own.Network())
	}
}

// Relabelling is not a new network: the symbol and description are what a person reads, and the
// fingerprint is what signatures carry. This is the guard that correcting a label never strands a
// kernel.
func TestLabelsDoNotMoveTheDigest(t *testing.T) {
	dir := installed(t)
	doc := readDoc(t, filepath.Join(dir, "arbitrum-one.json"))
	doc["symbol"] = "SOMETHING-ELSE"
	doc["description"] = "reworded entirely"
	if got := digestOf(t, dir, "arbitrum-one", doc); got != arbitrumDigest {
		t.Fatalf("a label moved the digest: %s (was %s)", got, arbitrumDigest)
	}
}

// The shipped worlds must be distinct networks, or an artifact of one would verify on another.
func TestShippedWorldsAreDistinct(t *testing.T) {
	dir := installed(t)
	seen := map[string]string{}
	for _, n := range []string{"play", "arbitrum-sepolia", "arbitrum-one"} {
		w, err := rail.Load(dir, n)
		if err != nil {
			t.Fatal(err)
		}
		d := w.Network().Digest
		if other, dup := seen[d]; dup {
			t.Fatalf("%s and %s share a digest", n, other)
		}
		seen[d] = n
	}
}

// The fingerprint covers what every member shares and nothing else: two operators of one network
// run different endpoints, meeting points and gas policies, and must still verify each other's
// signatures. What settles the money is shared, so it is covered: two worlds paying by different
// rules are different money even where everything else matches.
func TestOnlyTheAgreementMovesTheDigest(t *testing.T) {
	dir := installed(t)
	doc := readDoc(t, filepath.Join(dir, "arbitrum-one.json"))
	base := digestOf(t, dir, "arbitrum-one", doc)

	doc["rpc"] = "https://example.invalid/rpc"
	doc["finality"] = "safe"
	doc["seeds"] = []string{"/dns4/elsewhere.example/tcp/31313"}
	doc["gas"].(map[string]any)["slippageBps"] = float64(999)
	if moved := digestOf(t, dir, "arbitrum-one", doc); moved != base {
		t.Fatalf("an operational change moved the digest: %s vs %s", moved, base)
	}

	doc["chainId"] = float64(1)
	if digestOf(t, dir, "arbitrum-one", doc) == base {
		t.Fatal("a different chain must be a different network")
	}
	// The file's own name is the network's, so serving the same document under another name is
	// serving another network.
	doc = readDoc(t, filepath.Join(dir, "arbitrum-one.json"))
	if digestOf(t, dir, "elsewhere", doc) == base {
		t.Fatal("a different world name must be a different network")
	}
}

// A world settling by other rules is other money. The manual rail's payments are the operator's
// word and a chain's are the chain's, so the same name and the same token under two adaptors must
// not verify each other's receipts.
func TestTheAdaptorIsPartOfTheNetwork(t *testing.T) {
	dir := t.TempDir()
	manual := map[string]any{"rail": "manual", "decimals": 6, "seeds": []string{}}
	writeDoc(t, filepath.Join(dir, "twin.json"), manual)
	a, err := rail.Load(dir, "twin")
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.Rail = rail.RailEVM
	if a.Network().Digest == b.Network().Digest {
		t.Fatal("two adaptors under one name share a network")
	}
}

// A world once said where the payment scan begins. That is the block a kernel's own address came
// into existence at, which no file can know and no two kernels share, so the key is gone and a file
// still carrying it is refused by name rather than quietly ignored.
func TestLoadRejectsBadWorlds(t *testing.T) {
	dir := t.TempDir()
	if _, err := rail.Load(dir, "nope"); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "nope.json")) {
		t.Fatalf("a world that is not there must be refused, naming the file: %v", err)
	}
	// A name is one file in the directory of worlds, never a path through it, and it is refused on
	// its shape before the filesystem is consulted — so a name that reaches outside is refused as a
	// name, and never reports whether what it points at happens to be there.
	for _, bad := range []string{"../elsewhere/secret", "..", ".", "a/b", `a\b`, ""} {
		_, err := rail.Load(dir, bad)
		if err == nil {
			t.Errorf("a world named %q was read", bad)
			continue
		}
		if !strings.Contains(err.Error(), "bare name") {
			t.Errorf("a world named %q must be refused as a name, not by what is on disk: %v", bad, err)
		}
	}
	evm := func(over map[string]any) map[string]any {
		doc := map[string]any{"rail": "evm", "chainId": float64(1), "rpc": "https://node.example",
			"token": "0x0000000000000000000000000000000000000001", "decimals": float64(6)}
		for k, v := range over {
			if v == nil {
				delete(doc, k)
				continue
			}
			doc[k] = v
		}
		return doc
	}
	for name, tc := range map[string]struct {
		doc  map[string]any
		says string
	}{
		"no adaptor":          {map[string]any{"decimals": float64(6)}, "rail"},
		"unknown adaptor":     {map[string]any{"rail": "stripe", "decimals": float64(6)}, "stripe"},
		"chain without id":    {evm(map[string]any{"chainId": nil}), "chainId"},
		"chain without node":  {evm(map[string]any{"rpc": nil}), "rpc"},
		"chain without token": {evm(map[string]any{"token": nil}), "token"},
		"manual with a chain": {map[string]any{"rail": "manual", "chainId": float64(1), "decimals": float64(6)}, "chainId"},
		// the ledger holds 64-bit integers; a token needing more than 18 places could not be held
		"too many decimals": {evm(map[string]any{"decimals": float64(24)}), "decimals"},
		// the file is named by its file; a name inside it would be a second name to disagree with
		"a name of its own": {evm(map[string]any{"name": "other"}), "name"},
		// a setting this build does not know would silently have no effect at all
		"unknown key": {evm(map[string]any{"somethingElse": float64(1)}), "somethingElse"},
		// the venue names the wrapped native currency the router unwraps, which is not ETH on
		// every chain; a file still spelling the old key is refused by that key, never read as empty
		"the old venue key": {evm(map[string]any{"venue": map[string]any{"weth": "0x0000000000000000000000000000000000000002"}}), "weth"},
		// the scan cursor is each kernel's own, never the network's
		"where the scan begins": {evm(map[string]any{"fromBlock": float64(493710567)}), "fromBlock"},
		// the ticket ceiling moved to the kernel's own lottery_max; a file still naming it here
		// would be read as a rule that no longer applies
		"the retired ceiling": {evm(map[string]any{"lotteryMax": float64(100)}), "lotteryMax"},
	} {
		writeDoc(t, filepath.Join(dir, "w.json"), tc.doc)
		_, err := rail.Load(dir, "w")
		if err == nil {
			t.Errorf("%s: expected rejection", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: the refusal must name %q: %v", name, tc.says, err)
		}
	}
	// One document per file. A second one is a world somebody meant to serve, and reading only the
	// first would serve the other silently.
	if err := os.WriteFile(filepath.Join(dir, "two.json"),
		[]byte(`{"rail":"manual","decimals":6} {"rail":"manual","decimals":6}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rail.Load(dir, "two"); err == nil {
		t.Error("a file holding two worlds was read as one")
	}
}

// The settlement tag is the world's choice. Below true finality a settled fact is the sequencer's
// word and the kernel prices that risk through the remote premium, so a world may say `latest`,
// `safe` or `finalized`; anything else is refused, and an absent tag is the strictest.
func TestFinalityIsTheWorldsChoice(t *testing.T) {
	dir := installed(t)
	base, err := rail.Load(dir, "arbitrum-sepolia")
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
	one, err := rail.Load(dir, "arbitrum-one")
	if err != nil {
		t.Fatal(err)
	}
	if one.Finality != "finalized" {
		t.Errorf("the shipped real world settles at %q; changing it is an operator decision", one.Finality)
	}
}

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func writeDoc(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// digestOf writes doc under name and reports the network it defines, so a test changes one field
// and reads the consequence.
func digestOf(t *testing.T, dir, name string, doc map[string]any) string {
	t.Helper()
	writeDoc(t, filepath.Join(dir, name+".json"), doc)
	w, err := rail.Load(dir, name)
	if err != nil {
		t.Fatalf("load written world: %v", err)
	}
	return w.Network().Digest
}
