// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestNet(t *testing.T) *Net {
	t.Helper()
	n := &Net{Root: t.TempDir(), Kernels: map[string]*Kernel{},
		Expected: map[string]int{}, Unexpected: map[string]int{}, Rail: playRail{}}
	f, err := openLog(n.Root)
	if err != nil {
		t.Fatal(err)
	}
	n.logFile = f
	t.Cleanup(func() { f.Close() })
	return n
}

// A run directory is meant to be read and copied around, so a secret that reaches one has already
// leaked whatever is done afterwards. Redaction happens at capture.
func TestSecretsNeverReachAnArtifact(t *testing.T) {
	n := newTestNet(t)
	n.Secret("0xabcdef0123456789abcdef", "<key>")
	got := n.redact("the key is 0xabcdef0123456789abcdef and that is that")
	if strings.Contains(got, "abcdef0123456789") {
		t.Errorf("a registered secret survived redaction: %q", got)
	}
	phrase := "abandon ability able about above absent absorb abstract absurd abuse access accident"
	if strings.Contains(n.redact("phrase: "+phrase), "abandon ability") {
		t.Error("a twelve-word recovery phrase survived redaction")
	}
	if strings.Contains(n.redact("Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVP"), "eyJhbGci") {
		t.Error("a token survived redaction")
	}
}

// The redactor once matched inside base64 payloads and silently corrupted the artifacts it was
// meant to protect. Requiring two dots is what stops it; this is the case that broke.

func TestRedactionDoesNotCorruptEncodedPayloads(t *testing.T) {
	n := newTestNet(t)
	payload := "eyJhbGciOiJIUzI1NiJ9AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKKKLLLL"
	if n.redact(payload) != payload {
		t.Errorf("an encoded payload with no dots was rewritten: %q", n.redact(payload))
	}
}

// A short string is not a secret. Registering one would redact ordinary words out of every
// artifact, which destroys the log the run exists to produce.
func TestShortValuesAreNotTreatedAsSecrets(t *testing.T) {
	n := newTestNet(t)
	n.Secret("ok", "<x>")
	if n.redact("that is ok") != "that is ok" {
		t.Error("a two-character value was treated as a secret")
	}
}

// The verdicts are the point of the suite, so their bookkeeping is checked directly: a success
// counted as a failure, or the reverse, would misreport the whole run.
func TestChecksAreCountedUnderTheRightHeading(t *testing.T) {
	n := newTestNet(t)
	n.Check("a", true, "")
	n.Check("b", false, "because")
	if n.Expected["a"] != 1 {
		t.Error("a check that held was not counted as expected")
	}
	if n.Unexpected["b"] != 1 {
		t.Error("a check that failed was not counted as unexpected")
	}
}

// A wait must end, and must say which way it ended. Every wait in the suite is on a condition with
// a bound, because a fixed sleep is either too short or far too long.
func TestWaitingIsBoundedAndReportsWhichWay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	_ = os.WriteFile(path, []byte(`x {"msg":"server.ready","addr":"127.0.0.1:8080"} y`), 0o644)
	got, err := waitFor(path, `"addr":"([^"]*)"`, time.Second)
	if err != nil || got != "127.0.0.1:8080" {
		t.Errorf("got %q, %v; want the address", got, err)
	}
	start := time.Now()
	if _, err := waitFor(path, `"never":"([^"]*)"`, 300*time.Millisecond); err == nil {
		t.Error("a wait for something absent should fail, not hang")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the wait exceeded its bound")
	}
	if poll(200*time.Millisecond, 20*time.Millisecond, func() bool { return false }) {
		t.Error("until reported success for a condition that never held")
	}
	if !poll(time.Second, 10*time.Millisecond, func() bool { return true }) {
		t.Error("until reported failure for a condition that held immediately")
	}
}

// The stub upstream is what every published action calls, so an economy worth measuring needs it
// to fail and to lie in the ways the story depends on.
func TestTheStubUpstreamAnswersInTheWaysTheStoryNeeds(t *testing.T) {
	base, srv, err := startBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	body := post(t, base+"/echo", `{"msg":"hello"}`)
	var echoed map[string]any
	if json.Unmarshal([]byte(body), &echoed) != nil || echoed["echo"] != "hello" {
		t.Errorf("/echo returned %q", body)
	}

	// Every third call must fail: often enough to be met in a short run, rarely enough that a run
	// is not mostly failures.
	failures := 0
	for i := 0; i < 9; i++ {
		if strings.Contains(post(t, base+"/flaky", `{}`), "unavailable") {
			failures++
		}
	}
	if failures != 3 {
		t.Errorf("/flaky failed %d times in nine calls, want 3", failures)
	}

	// A successful answer of the wrong shape is a different path from an upstream that failed, and
	// the story needs both.
	out := post(t, base+"/badout", `{}`)
	if strings.Contains(out, `"echo"`) {
		t.Errorf("/badout returned the expected shape: %q", out)
	}

	if got := post(t, base+"/headers", `{}`); !strings.Contains(got, `"echo"`) {
		t.Errorf("/headers should reflect the delegated credential, returned %q", got)
	}
}

func post(t *testing.T, url, body string) string {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// Every command the suite issues is recorded, because the log is the account of the run and the
// report is computed from what it says.
func TestEveryCommandIsRecorded(t *testing.T) {
	n := newTestNet(t)
	n.Scenario("trading")
	n.record(record{Kernel: "k1", Actor: "ana", Cmd: "juice run ana/echo", Exit: 0, Ms: 12})
	n.record(record{Kernel: "k2", Actor: "ben", Cmd: "juice run ben/x", Exit: 1, Ms: 3})
	n.logFile.Sync()
	recs := readLog(n.Root)
	if len(recs) != 2 {
		t.Fatalf("recorded %d commands, want 2", len(recs))
	}
	if recs[0].Seq != 1 || recs[1].Seq != 2 {
		t.Error("records are not numbered in order")
	}
	if recs[0].Scenario != "trading" {
		t.Errorf("the record does not carry the part of the story it belongs to: %q", recs[0].Scenario)
	}
	if n.Ops() != 2 {
		t.Errorf("the run counted %d operations, want 2", n.Ops())
	}
}

// A snapshot that could not be read is not an empty economy: it is an unmeasured one. A checker
// given empty rows will happily report that everything balances, so every read that fails must
// block the verdict instead.
func TestAnUnreadableKernelBlocksTheVerdict(t *testing.T) {
	n := newTestNet(t)
	// A kernel whose server is not there: every read fails.
	n.Kernels["k1"] = &Kernel{Name: "k1", URL: "http://127.0.0.1:1", net: n}
	_, blocking := collect(n)
	if len(blocking) == 0 {
		t.Fatal("a kernel that answered nothing produced no blocking entry")
	}
	for _, want := range []string{"accounts", "peer rows", "transactions", "processes"} {
		found := false
		for _, b := range blocking {
			if strings.Contains(b, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("an unreadable kernel did not block on %s", want)
		}
	}
}

// A read the story judges by must not turn into zero when it fails: a balance of 0, a price of 0 or
// no recorded uses would each pass some check for the wrong reason.
func TestAFailedReadFailsACheck(t *testing.T) {
	n := newTestNet(t)
	k := &Kernel{Name: "k1", URL: "http://127.0.0.1:1", net: n}
	k.Num("ana", "available", "user", "me")
	k.Field("ana", "id", "user", "me")
	k.Uses("ana", "ana@hub/echo")
	if n.Unexpected["harness.read"] != 3 {
		t.Errorf("three failed reads recorded %d failed checks", n.Unexpected["harness.read"])
	}
}

// A missing privacy projection must fail the run. Skipping it means a run that never captured the
// evidence reports that nothing leaked.
func TestAMissingPrivacyProjectionFailsTheRun(t *testing.T) {
	n := newTestNet(t)
	rep, err := Judge(n, emptyStory(), 1, map[string]any{})
	if err == nil {
		t.Fatal("a run with no privacy evidence reported a pass")
	}
	found := false
	for _, b := range rep.Blocking {
		if strings.Contains(b, "leaks") {
			found = true
		}
	}
	if !found {
		t.Errorf("a run that captured no projection did not say so: %v", rep.Blocking)
	}
}

// A restart rewrites config.json, and the key first boot minted is what seals every stored
// credential. Losing it does not degrade the kernel: it refuses to boot, which in a run reads as a
// kernel that never came up.
func TestARestartKeepsWhatFirstBootMinted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	o := bootOpts{Handle: "shop", FeeBps: 2000}
	if err := writeKernelConfig(path, o); err != nil {
		t.Fatal(err)
	}
	// First boot mints the key into the file the harness wrote.
	first := map[string]any{}
	readJSON(t, path, &first)
	first["credentials_key"] = "0123456789abcdef0123456789abcdef0123456789a"
	b, _ := json.Marshal(first)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeKernelConfig(path, o); err != nil {
		t.Fatal(err)
	}
	again := map[string]any{}
	readJSON(t, path, &again)
	if again["credentials_key"] != first["credentials_key"] {
		t.Errorf("credentials_key after restart: got %v, want %v", again["credentials_key"], first["credentials_key"])
	}
	if again["kernel_handle"] != "shop" || again["fee_bps"] != float64(2000) {
		t.Errorf("restart lost the harness's own settings: %v", again)
	}
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}
