// Package main is the network simulation suite: a five-kernel economy driven end to end over a
// real network, recording what it did and judging it.
//
// This is not the commit gate. The flow suite (flows/flows_test.sh) answers "did this change break
// something" in minutes and fails a commit; this answers "how does the network behave" and leaves
// artifacts to read. Run it after anything that touches federation or money.
//
//	go run ./netsim                  # play rail, no external dependencies
//	go run ./netsim -rail anvil      # local chain, needs Foundry
//	go run ./netsim -rail sepolia    # live testnet, needs an RPC and a funded key
//
// The suite drives the shipped binary through its own command line and HTTP API, exactly as an
// operator would. It never links the kernel, so nothing here can pass by reaching inside it.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Net is one run: the kernels, where its artifacts go, and what it has recorded.
type Net struct {
	Root    string // <repo>/netsim-runs/<rail>-<timestamp>
	Binary  string // the juice binary under test
	Rail    Rail
	Kernels map[string]*Kernel
	Backend string // base URL of the stub upstream the published actions call

	mu       sync.Mutex
	seq      int
	logFile  *os.File
	scenario string
	secrets  [][2]string // literal -> placeholder, applied to everything recorded

	Expected   map[string]int // checks that behaved as specified
	Unexpected map[string]int // checks that did not

	// Product is the subset of failures the suite is confident are defects in the system under
	// test rather than in itself. The distinction matters because the two need opposite responses:
	// a harness fault is fixed here and forgotten, while a product failure must survive being
	// noticed. Keeping them in one list is how a real finding gets quietly "fixed" by adjusting
	// the check that found it.
	Defects map[string]string // the failures that are the kernel's, not the suite's

	Run runInfo // what was measured
}

// runInfo identifies what ran. A commit alone does not: the worktree may be dirty, the binary older
// than the tree, and the network a chain rail talked to is part of the result.
type runInfo struct {
	Commit       string    `json:"commit"`
	Dirty        bool      `json:"worktree_dirty"`
	BinarySHA    string    `json:"binary_sha256"`
	WorldDigest  string    `json:"world_digest"`
	StoryVersion string    `json:"story_version"`
	StartedAt    time.Time `json:"started_at"`
}

// Kernel is one juice server and the client homes that talk to it.
type Kernel struct {
	Name   string // k1..k8, the directory name
	Handle string // the name it advertises to the network
	URL    string
	Key    string // its public key, the only global name it has
	Dir    string
	cmd    *exec.Cmd
	net    *Net
}

func NewNet(root, binary string, rail Rail) (*Net, error) {
	if err := os.MkdirAll(filepath.Join(root, "checkpoints"), 0o755); err != nil {
		return nil, err
	}
	f, err := openLog(root)
	if err != nil {
		return nil, err
	}
	n := &Net{Root: root, Binary: binary, Rail: rail, Kernels: map[string]*Kernel{},
		logFile: f, Expected: map[string]int{}, Unexpected: map[string]int{},
		Defects: map[string]string{}, Run: runInfo{StartedAt: time.Now(), BinarySHA: fileSHA(binary),
			StoryVersion: StoryVersion}}
	n.Run.Commit, n.Run.Dirty = gitState()
	return n, nil
}

// gitState is the commit under test and whether the tree was modified. A dirty worktree means the
// commit does not identify what ran.
func gitState() (string, bool) {
	commit, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", false
	}
	status, _ := exec.Command("git", "status", "--porcelain").Output()
	return strings.TrimSpace(string(commit)), len(strings.TrimSpace(string(status))) > 0
}

// fileSHA identifies the binary that was actually driven, which a commit does not when the tree is
// dirty or the binary was built earlier.
func fileSHA(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// Secret registers a value that must never appear in an artifact. A run directory is meant to be
// read and copied around, so a recovery phrase or funding key that reaches one has already leaked
// whatever is done afterwards. Redaction happens at capture, not at publication.
func (n *Net) Secret(literal, placeholder string) {
	if len(strings.TrimSpace(literal)) < 8 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.secrets = append(n.secrets, [2]string{literal, placeholder})
}

var (
	// Twelve lowercase words in machine output is a BIP-39 phrase, never prose.
	rePhrase = regexp.MustCompile(`\b(?:[a-z]{3,10} ){11}[a-z]{3,10}\b`)
	// Two dots are required: without them this matches inside any base64 payload, and a redactor
	// that rewrites binary silently corrupts the artifacts it was meant to protect.
	reJWT = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)
	reKey = regexp.MustCompile(`0x[0-9a-fA-F]{64}`)
)

func (n *Net) redact(s string) string {
	n.mu.Lock()
	subs := append([][2]string(nil), n.secrets...)
	n.mu.Unlock()
	for _, p := range subs {
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	s = rePhrase.ReplaceAllString(s, "<recovery-phrase>")
	s = reJWT.ReplaceAllString(s, "<jwt>")
	return reKey.ReplaceAllString(s, "<private-key>")
}

// Scenario names the part of the story now running; every record carries it.
func (n *Net) Scenario(s string) { n.mu.Lock(); n.scenario = s; n.mu.Unlock() }

type record struct {
	Seq      int    `json:"seq"`
	Scenario string `json:"scenario"`
	Kernel   string `json:"kernel"`
	Actor    string `json:"actor"`
	Cmd      string `json:"cmd"`
	Exit     int    `json:"exit"`
	Out      string `json:"out"`
	Ms       int64  `json:"ms"`
}

func (n *Net) record(r record) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seq++
	r.Seq = n.seq
	r.Scenario = n.scenario
	b, _ := json.Marshal(r)
	fmt.Fprintln(n.logFile, string(b))
}

// Ops is how many commands the run has issued, which is the volume figure the report quotes.
func (n *Net) Ops() int { n.mu.Lock(); defer n.mu.Unlock(); return n.seq }

func (n *Net) Close() { _ = n.logFile.Close() }

// home is one actor's client directory. Each user keeps their own credentials, as they would.
func (n *Net) home(actor string) string {
	d := filepath.Join(n.Root, "homes", actor)
	_ = os.MkdirAll(filepath.Join(d, ".juice"), 0o700)
	return d
}

// Run invokes the binary as an actor and records the result. Everything the suite does to a kernel
// goes through here, so log.jsonl is a complete account of the run.
func (k *Kernel) Run(actor string, args ...string) (string, error) {
	n := k.net
	t0 := time.Now()
	full := append([]string{"--server", k.URL}, args...)
	cmd := exec.Command(n.Binary, full...)
	cmd.Env = append(os.Environ(), "HOME="+n.home(actor))
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	text := n.redact(string(out))
	n.record(record{Kernel: k.Name, Actor: actor, Cmd: "juice " + n.redact(strings.Join(args, " ")),
		Exit: code, Out: text, Ms: time.Since(t0).Milliseconds()})
	return strings.TrimSpace(text), err
}

// read runs a command in machine-readable mode and decodes it into T.
func read[T any](k *Kernel, actor string, args ...string) (T, error) {
	var v T
	out, err := k.Run(actor, append([]string{"--json"}, args...)...)
	if err != nil {
		return v, fmt.Errorf("%s", firstLine(out))
	}
	return v, json.Unmarshal([]byte(out), &v)
}

// Field reads one field of a command's JSON object as a string; Num as a number.
func (k *Kernel) Field(actor, field string, args ...string) string {
	m, _ := read[map[string]any](k, actor, args...)
	if f, ok := m[field].(float64); ok {
		return strconv.FormatInt(int64(f), 10)
	}
	return str(m, field)
}

func (k *Kernel) Num(actor, field string, args ...string) int64 {
	m, _ := read[map[string]any](k, actor, args...)
	return num(m, field)
}

// Balance is what an account can spend right now.
func (k *Kernel) Balance(user string) int64 { return k.Num(user, "available", "user", "me") }

// Holdings is everything a user has, spendable or committed. A cross-kernel call commits a lottery
// stake as well as its price, so what a caller can spend falls while calls are in flight and rises
// again when they resolve; only the total says whether the caller is better or worse off (P10).
func (k *Kernel) Holdings(user string) int64 {
	return k.Num(user, "available", "user", "me") + k.Num(user, "locked", "user", "me")
}

// Get reads an HTTP path as an actor, or with no credential at all when the actor is "" — which is
// how a stranger sees the kernel. It exists for the few reads the command line caps below what a
// run needs: its own response limit is 10 MiB, which a full transaction list exceeds.
func (k *Kernel) Get(actor, path string) string {
	req, err := http.NewRequest("GET", k.URL+path, nil)
	if err != nil {
		return ""
	}
	if tok := k.token(actor); actor != "" && tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return k.net.redact(string(b))
}

// token reads an actor's own login from the client's credential store, where each context's session
// is its own file (ecosystem-standard.md). The suite reads what the command line wrote rather than
// logging in a second time, so an actor here is exactly the actor the kernel saw.

func (k *Kernel) token(actor string) string {
	dir := filepath.Join(k.net.home(actor), ".juice", "client", "credentials")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var c struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(b, &c) == nil && c.Token != "" {
			return c.Token
		}
	}
	return ""
}

type bootOpts struct {
	Handle       string
	Bootstrap    string
	CreditLimit  int64
	Lottery      int64
	FeeBps       int
	RemoteBps    int
	ImportBps    int
	RetrySeconds int
}

// Boot starts (or restarts) a kernel that outlives the call. Restarting on the same directory is
// how the story kills and revives a provider, so the two paths are one function.
func (n *Net) Boot(name string, o bootOpts) (*Kernel, error) {
	dir := filepath.Join(n.Root, name)
	if err := os.MkdirAll(filepath.Join(dir, "kernels", "default"), 0o755); err != nil {
		return nil, err
	}
	if o.Handle == "" {
		o.Handle = name
	}
	cfg := map[string]any{
		"script_timeout_ms": 10000, "script_memory_bytes": 67108864,
		"fee_bps": o.FeeBps, "remote_bps": o.RemoteBps, "import_bps": o.ImportBps,
		"credit_limit": o.CreditLimit, "lottery": o.Lottery,
		"token_ttl": "60m", "log_level": "info", "log_format": "json",
		"allow_local_sources": true, "kernel_handle": o.Handle,
		"bootstrap_peers":               []string{},
		"remote_retry_interval_seconds": o.RetrySeconds,
		"discovery_interval_seconds":    2,
	}
	if o.Bootstrap != "" {
		cfg["bootstrap_peers"] = []string{o.Bootstrap}
	}
	for key, val := range n.Rail.Config() {
		cfg[key] = val
	}
	b, _ := json.MarshalIndent(cfg, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "kernels", "default", "config.json"), b, 0o644); err != nil {
		return nil, err
	}

	logPath := filepath.Join(dir, "server.log")
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(n.Binary, "serve", "--addr", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "JUICE_BOOTSTRAP_PASSWORD=sys-pass",
		"JUICE_HOME="+dir, "HOME="+n.home("sysop-"+name))
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	k := n.Kernels[name]
	if k == nil {
		k = &Kernel{Name: name, Dir: dir, net: n}
		n.Kernels[name] = k
	}
	k.Handle, k.cmd = o.Handle, cmd

	// The address is assigned by the operating system, so it is read from the server's own
	// readiness line rather than guessed. Waiting for the line is also the only honest way to know
	// the kernel is up.
	addr, err := waitFor(logPath, `"msg":"server.ready".*"addr":"([^"]*)"`, 90*time.Second)
	if err != nil {
		return nil, fmt.Errorf("kernel %s did not come up: %w", name, err)
	}
	k.URL = "http://" + addr

	// First boot prints the superuser's recovery phrase to this log. Register it for redaction and
	// remove it from the file: the run directory is an artifact people share.
	if ph, err := waitFor(logPath, `recovery phrase[^\n]*\n\s*([a-z][a-z ]{40,})`, 2*time.Second); err == nil {
		n.Secret(strings.TrimSpace(ph), "<phrase:"+name+"-sys>")
		if raw, err := os.ReadFile(logPath); err == nil {
			_ = os.WriteFile(logPath, rePhrase.ReplaceAll(raw, []byte("<recovery-phrase>")), 0o644)
		}
	}
	// The superuser signs in here rather than at each call site: every boot needs it, including the
	// restarts the story uses to kill and revive a provider, and a kernel whose operator is not
	// signed in fails every administrative command with a message about logging in.
	k.Login()
	k.Key = k.Field("sysop-"+name, "public_key", "admin", "identity")
	if k.Key == "" {
		return nil, fmt.Errorf("kernel %s came up but would not report its own key", name)
	}
	return k, nil
}

// Stop kills a kernel outright. Nothing is asked of it first: the story uses this to find out what
// survives a machine losing power, and a graceful shutdown would answer a different question.
func (k *Kernel) Stop() {
	if k.cmd == nil || k.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-k.cmd.Process.Pid, syscall.SIGKILL)
	_, _ = k.cmd.Process.Wait()
	k.cmd = nil
	time.Sleep(300 * time.Millisecond)
}

// FedAddr is the multiaddress a peer bootstraps from.
func (k *Kernel) FedAddr() string {
	s, err := waitFor(filepath.Join(k.Dir, "server.log"),
		`(/ip4/127\.0\.0\.1/tcp/[0-9]+/p2p/[A-Za-z0-9]+)`, 10*time.Second)
	if err != nil {
		return ""
	}
	return s
}

// poll waits for a condition with a bound, and says which way it ended. Every wait in the suite is
// of this form: a fixed sleep is either too short, reporting a failure that was merely early, or far
// too long, turning a run into hours.
func poll(within, every time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(every)
	}
	return true
}

// waitFor polls a file until a pattern's first group appears.
func waitFor(path, pattern string, within time.Duration) (string, error) {
	re, got := regexp.MustCompile(pattern), ""
	if poll(within, 150*time.Millisecond, func() bool {
		b, _ := os.ReadFile(path)
		if m := re.FindSubmatch(b); m != nil {
			got = string(m[1])
		}
		return got != ""
	}) {
		return got, nil
	}
	return "", fmt.Errorf("%s did not appear in %s within %s", pattern, filepath.Base(path), within)
}

// MakeUser creates an account, captures its recovery phrase for redaction, and logs it in.
func (k *Kernel) MakeUser(handle string) {
	out, _ := k.Run("sysop-"+k.Name, "user", "create", handle, "--password", "userpass")
	if m := rePhrase.FindString(out); m != "" {
		k.net.Secret(m, "<phrase:"+handle+">")
	}
	_, _ = k.Run(handle, "auth", "login", handle, "--password", "userpass")
}

// Login signs the superuser in after a restart.
func (k *Kernel) Login() {
	_, _ = k.Run("sysop-"+k.Name, "auth", "login", "sys", "--password", "sys-pass")
}

// ---- checks -----------------------------------------------------------------
// Each check says what it expects, so a refusal the story was testing for is never confused with
// one that means the economy is not the one the report describes.

// Check records one judgement. A failure is printed as it happens.
func (n *Net) Check(label string, ok bool, detail string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ok {
		n.Expected[label]++
		return true
	}
	n.Unexpected[label]++
	fmt.Printf("    NOT AS SPECIFIED [%s]: %s\n", label, detail)
	return false
}

// Product records a defect in the system under test. It fails the run like any other failed check
// and is reported apart from them, because the two need opposite responses: a harness fault is
// fixed and forgotten, while one of these must survive being noticed.
func (n *Net) Product(label, detail string) {
	n.mu.Lock()
	n.Unexpected[label]++
	n.Defects[label] = detail
	n.mu.Unlock()
	fmt.Printf("    PRODUCT FAILURE [%s]: %s\n", label, detail)
}

// MustWork is a call that is supposed to succeed, judged by its exit status: some verbs print
// nothing on success.
func (n *Net) MustWork(label string, k *Kernel, actor string, args ...string) bool {
	out, err := k.Run(actor, append([]string{"--json"}, args...)...)
	return n.Check(label, err == nil && !strings.Contains(out, `"error"`), "expected success: "+firstLine(out))
}

// MustRefuse is a call that is supposed to be refused, and for the stated reason: a refusal on
// other grounds means the defence that held was not the one under test.
func (n *Net) MustRefuse(label, because string, k *Kernel, actor string, args ...string) bool {
	out, err := k.Run(actor, args...)
	if err == nil {
		return n.Check(label, false, "was not refused at all")
	}
	return n.Check(label, regexp.MustCompile(`(?i)`+because).MatchString(out),
		"refused for another reason, wanted /"+because+"/: "+firstLine(out))
}

func firstLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 140 {
		return s[:140]
	}
	return s
}

// ---- the stub upstream ------------------------------------------------------
// The published actions are ordinary HTTP actions pointing at this server, so the economy exercises
// the real call path rather than a special one. It answers in four ways, because a network worth
// measuring has services that fail and services that lie about their output.
func startBackend(root string) (string, *http.Server, error) {
	mux := http.NewServeMux()
	var calls int64
	var mu sync.Mutex
	next := func() int64 { mu.Lock(); defer mu.Unlock(); calls++; return calls }

	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		msg, _ := in["msg"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"echo": msg})
	})
	// A service whose upstream fails intermittently. Every third call fails, which is frequent
	// enough to be met in a short run and rare enough that the run is not mostly failures.
	mux.HandleFunc("/flaky", func(w http.ResponseWriter, r *http.Request) {
		if next()%3 == 0 {
			http.Error(w, `{"error":"upstream unavailable"}`, http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"echo": "ok"})
	})
	// A service that answers, successfully, with the wrong shape. The kernel must refuse it on the
	// output schema and refund, which is a different path from an upstream that failed.
	mux.HandleFunc("/badout", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"unexpected": true})
	})
	// Reflects the headers it was sent, so a delegated credential can be seen to arrive.
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"echo": r.Header.Get("X-Api-Key")})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), srv, nil
}

// openLog is the log file a run appends to, separated so a report can be judged in a test without
// standing up a network.
func openLog(root string) (*os.File, error) {
	return os.Create(filepath.Join(root, "log.jsonl"))
}
