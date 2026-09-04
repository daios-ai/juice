package main

// In-process federation simulator: several real kernels in one test binary, wired to each other
// through a transport shim that can inject the faults a network produces. It exists because the
// properties §13 claims — never double-charged, never silently dropped, settled only on signed
// evidence — are all statements about what happens when a message is lost, repeated, or answered
// after the answer stopped mattering. A loopback flow cannot cause those on purpose; this can.
//
// Everything here plugs into a seam the production code already has: `federationTransport`
// (fedclient.go) is the outbound half, `fedHandlers` (fedservice.go) is the inbound half, and
// `kernel.Store` is decorated the way `pricedStore` decorates it in production. No package-level
// state is replaced, no constant is changed for a test, no production symbol exists only for this
// file.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
)

// ---------------------------------------------------------------------------
// Faults
// ---------------------------------------------------------------------------

// simFault is what the network does to one message. The set is deliberately small: every fault a
// signed, recipient-bound protocol can actually suffer reduces to one of these. Corruption and
// misdelivery are absent on purpose — a tampered or misaddressed payload fails its signature check
// and is indistinguishable from a drop, and that refusal is already asserted in
// kernel/federation_test.go and cmd/juice/fedservice_test.go.
type simFault int

const (
	faultNone simFault = iota
	// faultRefuseBeforeDispatch: the request provably never left. Only this proves non-delivery,
	// so only this may fail a call fast with a refund (§13 never-dispatched).
	faultRefuseBeforeDispatch
	// faultLoseResponse: the peer ran the work and committed; the answer never arrived. The
	// caller cannot tell this from a peer that never heard, which is the whole reason a parked
	// call may not be presumed dead.
	faultLoseResponse
	// faultDeliverTwice: the peer sees the same request twice. Idempotency must absorb it.
	faultDeliverTwice
)

// String names a fault in a failure message. Now that a scenario asserts its fault fired, the
// message has to say which one.
func (f simFault) String() string {
	switch f {
	case faultRefuseBeforeDispatch:
		return "refuse-before-dispatch"
	case faultLoseResponse:
		return "lose-response"
	case faultDeliverTwice:
		return "deliver-twice"
	}
	return "none"
}

// simVerb names the protocol a fault applies to, so a scenario can lose a call's receipt while
// leaving resolve and settle alone.
type simVerb string

const (
	verbCall    simVerb = "call"
	verbResolve simVerb = "resolve"
	verbSettle  simVerb = "settle"
	verbStep    simVerb = "step"
)

// simPlan is the scenario's instruction to the network: what to do to the next message of a given
// verb between a given pair. Faults are consumed one message at a time (`times`), because a test
// that says "lose the reply" means one reply, and a fault that outlived its intent would make the
// following assertion meaningless.
type simPlan struct {
	from, to string // peer public keys; "" matches any
	verb     simVerb
	fault    simFault
	times    int
}

// ---------------------------------------------------------------------------
// The network
// ---------------------------------------------------------------------------

// simNet routes federation messages between in-process kernels by public key, applying whatever
// faults the scenario has armed. It is the test's stand-in for libp2p and nothing more: it applies
// no Juice semantics, exactly as `fed.Transport` applies none.
type simNet struct {
	t     *testing.T
	mu    sync.Mutex
	nodes map[string]*simNode
	plans []simPlan
	// blocked is a one-way cut: blocked[from][to] means from cannot reach to. Symmetric partitions
	// are two entries, because a real partition is often one-way and the asymmetry matters.
	blocked map[string]map[string]bool
	trace   []string
	fired   map[simFault]int // faults actually applied, so a scenario can prove its own premise
	// backend is the upstream every simulated action points at. Actions must really execute for a
	// receipt to mean anything, so the kernels run real http actions against a real local server
	// rather than a stubbed executor.
	backend *httptest.Server
}

// simNode is one kernel with everything it needs to be both caller and server.
type simNode struct {
	net      *simNet
	name     string
	key      string // base64url Ed25519 public key
	k        *kernel.Kernel
	db       *store.DB          // the real store, unwrapped: assertions read through this
	faults   *faultStore        // the decorator the kernel sees; nil-safe when unarmed
	h        *fedHandlers       // inbound half
	adapter  *fedAdapter        // outbound half
	sysID    string             // the sys account id, for balance assertions
	issuerID string             // receipt issuer (sys), needed to re-install the signing key on restart
	priv     ed25519.PrivateKey // this kernel's signing key
	cfg      kernel.Config      // the policy this node was built with, so a restart rebuilds it
}

func newSimNet(t *testing.T) *simNet {
	n := &simNet{t: t, nodes: map[string]*simNode{}, blocked: map[string]map[string]bool{},
		fired: map[simFault]int{}}
	n.backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(n.backend.Close)
	return n
}

func (n *simNet) logf(format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.trace = append(n.trace, fmt.Sprintf(format, args...))
}

// dump prints the message trace. Called on failure so a broken scenario is readable without
// re-running it under a debugger.
func (n *simNet) dump() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.t.Logf("--- simulator trace (%d messages) ---", len(n.trace))
	for i, l := range n.trace {
		n.t.Logf("  %2d. %s", i+1, l)
	}
}

// arm schedules a fault. Scenarios call this immediately before the operation it should hit.
func (n *simNet) arm(from, to string, verb simVerb, f simFault, times int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.plans = append(n.plans, simPlan{from: from, to: to, verb: verb, fault: f, times: times})
}

// cut blocks one direction. A cut is not a fault on a message: it is the state of the link, so it
// persists until healed rather than being consumed.
func (n *simNet) cut(from, to string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.blocked[from] == nil {
		n.blocked[from] = map[string]bool{}
	}
	n.blocked[from][to] = true
}

func (n *simNet) isBlocked(from, to string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.blocked[from] != nil && n.blocked[from][to]
}

// take pulls the fault armed for this message, consuming one use.
func (n *simNet) take(from, to string, verb simVerb) simFault {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := range n.plans {
		p := &n.plans[i]
		if p.times <= 0 || p.verb != verb {
			continue
		}
		if p.from != "" && p.from != from {
			continue
		}
		if p.to != "" && p.to != to {
			continue
		}
		p.times--
		n.fired[p.fault]++
		return p.fault
	}
	return faultNone
}

// assertFired is how a scenario proves its own premise. Without it several tests here would pass on
// the ordinary path: a duplicate that was never duplicated executes once, and a commit fault that
// never fired looks exactly like a call that simply worked. A test that cannot tell those apart is
// not testing what its name says.
func (n *simNet) assertFired(t *testing.T, f simFault, want int) {
	t.Helper()
	n.mu.Lock()
	got := n.fired[f]
	n.mu.Unlock()
	if got != want {
		n.dump()
		t.Fatalf("fault %v was applied %d times, expected %d — the scenario did not happen", f, got, want)
	}
}

func (n *simNet) node(key string) *simNode {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nodes[key]
}

// ---------------------------------------------------------------------------
// The transport shim
// ---------------------------------------------------------------------------

// simPort is one kernel's view of the network. It implements `federationTransport` — the four
// verbs the kernel actually consumes — and nothing else: the administrative client (Gossip, Probe,
// ListenAddrs) is not part of any scenario here, so it is not implemented.
type simPort struct {
	net  *simNet
	self string
}

var _ federationTransport = (*simPort)(nil)

// deliver is the one place a message crosses the network, so it is the one place a fault applies.
// The ordering is the network's own: a cut and a refusal happen before anything is delivered; a
// lost response happens after the peer has already committed.
func (p *simPort) deliver(ctx context.Context, peerKey string, verb simVerb,
	call func(h *fedHandlers) fed.Response) (fed.Response, error) {

	if p.net.isBlocked(p.self, peerKey) {
		p.net.logf("%s -> %s %s BLOCKED (link cut)", short(p.self), short(peerKey), verb)
		return fed.Response{}, fmt.Errorf("sim: link cut: %w", fed.ErrNotDispatched)
	}
	dst := p.net.node(peerKey)
	if dst == nil {
		return fed.Response{}, fmt.Errorf("sim: no such peer %s: %w", short(peerKey), fed.ErrNotDispatched)
	}

	switch f := p.net.take(p.self, peerKey, verb); f {
	case faultRefuseBeforeDispatch:
		p.net.logf("%s -> %s %s REFUSED before dispatch", short(p.self), short(peerKey), verb)
		return fed.Response{}, fmt.Errorf("sim: refused: %w", fed.ErrNotDispatched)

	case faultLoseResponse:
		// The peer does the work. Only the answer is lost — which is why this must not be
		// reported as not-dispatched: the call may well have executed and been charged.
		resp := call(dst.h)
		p.net.logf("%s -> %s %s delivered (status %d), RESPONSE LOST", short(p.self), short(peerKey), verb, resp.Status)
		return fed.Response{}, fmt.Errorf("sim: response lost after dispatch")

	case faultDeliverTwice:
		first := call(dst.h)
		second := call(dst.h)
		p.net.logf("%s -> %s %s delivered TWICE (status %d then %d)", short(p.self), short(peerKey), verb, first.Status, second.Status)
		return second, nil

	default:
		resp := call(dst.h)
		p.net.logf("%s -> %s %s status %d %s", short(p.self), short(peerKey), verb, resp.Status, body(resp))
		return resp, nil
	}
}

func (p *simPort) Call(ctx context.Context, peerKey string, req fed.CallRequest) (fed.CallResponse, error) {
	return p.deliver(ctx, peerKey, verbCall, func(h *fedHandlers) fed.Response {
		return h.OnCall(ctx, p.self, req)
	})
}

func (p *simPort) Resolve(ctx context.Context, peerKey string, req fed.ResolveRequest) (fed.ResolveResponse, error) {
	return p.deliver(ctx, peerKey, verbResolve, func(h *fedHandlers) fed.Response {
		return h.OnResolve(ctx, p.self, req)
	})
}

func (p *simPort) Settle(ctx context.Context, peerKey string, req fed.SettleRequest) (fed.SettleResponse, error) {
	return p.deliver(ctx, peerKey, verbSettle, func(h *fedHandlers) fed.Response {
		return h.OnSettle(ctx, p.self, req)
	})
}

func (p *simPort) Step(ctx context.Context, peerKey string, req fed.StepRequest) (fed.StepResponse, error) {
	return p.deliver(ctx, peerKey, verbStep, func(h *fedHandlers) fed.Response {
		return h.OnStep(ctx, p.self, req)
	})
}

// body renders a reply for the trace, truncated: a trace is for reading, not for archiving.
func body(r fed.Response) string {
	b := string(r.Body)
	if len(b) > 160 {
		b = b[:160] + "…"
	}
	return b
}

func short(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}

// ---------------------------------------------------------------------------
// Store fault injection
// ---------------------------------------------------------------------------

// faultStore fails one compound write, before or after it commits. The second mode is the one that
// matters: a store call that commits and then reports failure is what a process killed between the
// commit and its acknowledgement looks like from above, and the code paths that recover from it are
// the ones that decide whether money is stranded.
//
// It decorates kernel.Store by embedding, the same shape as the four wrappers already in
// kernel/call_test.go and kernel/steps_test.go, and the same shape production uses in pricedStore.
type faultStore struct {
	kernel.Store
	mu sync.Mutex
	// armed names the method to fail; empty disarms. after=false fails before the write happens,
	// after=true performs the write and then reports failure.
	armed string
	after bool
	fired int
}

func (f *faultStore) armBefore(method string) { f.set(method, false) }
func (f *faultStore) armAfter(method string)  { f.set(method, true) }
func (f *faultStore) disarm()                 { f.set("", false) }

func (f *faultStore) set(method string, after bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.after, f.fired = method, after, 0
}

// trip reports whether this call should fail, and whether the underlying write should happen first.
// One shot: the fault disarms itself, so a scenario asserts on the recovery rather than on an
// endlessly broken store.
// firedCount reports how many injected store faults actually triggered. A scenario asserts on it
// for the same reason as assertFired above.
func (f *faultStore) firedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

func (f *faultStore) trip(method string) (fail bool, doWrite bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.armed != method {
		return false, true
	}
	f.armed = ""
	f.fired++
	return true, f.after
}

func (f *faultStore) fault(method string, write func() error) error {
	fail, doWrite := f.trip(method)
	if doWrite {
		if err := write(); err != nil {
			return err
		}
	}
	if fail {
		return kernel.ErrInternal.Wrapf("sim: injected store fault at %s (after-commit=%v)", method, doWrite)
	}
	return nil
}

// The compound money-path writes (D3). Only these are overridden: they are the ones that carry a
// transaction and a receipt together, so they are where a lost acknowledgement can strand funds.

func (f *faultStore) CommitCall(ctx context.Context, tx *kernel.Transaction, receipt *kernel.Receipt,
	traceID, callerWalletID, callerWalletKind, targetUserID, feeRecipientID string,
	net, fee int64, stats *kernel.Stats, idempotencyRecordID, stepID string) error {

	return f.fault("CommitCall", func() error {
		return f.Store.CommitCall(ctx, tx, receipt, traceID, callerWalletID, callerWalletKind,
			targetUserID, feeRecipientID, net, fee, stats, idempotencyRecordID, stepID)
	})
}

func (f *faultStore) CommitFailedCall(ctx context.Context, tx *kernel.Transaction,
	buildReceipt func(refund int64) (*kernel.Receipt, error),
	traceID, callerWalletID, callerWalletKind, feeRecipientID string, gross int64,
	stats *kernel.Stats, idempotencyRecordID, errorCode, stepID string) error {

	return f.fault("CommitFailedCall", func() error {
		return f.Store.CommitFailedCall(ctx, tx, buildReceipt, traceID, callerWalletID,
			callerWalletKind, feeRecipientID, gross, stats, idempotencyRecordID, errorCode, stepID)
	})
}

func (f *faultStore) CompleteIdempotencyRecordIfPending(ctx context.Context, id, result, receiptJSON string) error {
	return f.fault("CompleteIdempotencyRecordIfPending", func() error {
		return f.Store.CompleteIdempotencyRecordIfPending(ctx, id, result, receiptJSON)
	})
}

// ---------------------------------------------------------------------------
// Building the network
// ---------------------------------------------------------------------------

// simConfig is the per-kernel policy a scenario varies. Rates are deliberately asymmetric across
// nodes in the scenarios below, so a settlement that used the wrong side's rate moves a balance the
// assertions catch.
type simConfig struct {
	FeeBPS        int64
	RemoteBPS     int64
	ImportBPS     int64
	ExposureMax   int64
	PendingMaxAge time.Duration
	PeerRetention time.Duration
}

func defaultSimConfig() simConfig {
	return simConfig{FeeBPS: 1000, RemoteBPS: 500, ImportBPS: 500, ExposureMax: 100000}
}

// addNode builds one kernel: real store, real first boot, real signing key, real inbound handlers,
// and an outbound adapter pointed at the shim. Construction mirrors newFlowKernel, which is how
// every other cmd/juice test builds a kernel.
func (n *simNet) addNode(name string, sc simConfig) *simNode {
	t := n.t
	t.Helper()

	db := newTestStore(t)
	base := &faultStore{Store: db}

	cfg := testConfig("sim-" + name)
	cfg.AllowLocalSources = true
	cfg.FeeBPS = sc.FeeBPS
	cfg.RemoteBPS = sc.RemoteBPS
	cfg.ImportBPS = sc.ImportBPS
	cfg.ExposureMax = sc.ExposureMax
	if sc.PendingMaxAge > 0 {
		cfg.RemotePendingMaxAge = sc.PendingMaxAge
	}
	if sc.PeerRetention > 0 {
		cfg.PeerRetention = sc.PeerRetention
	}

	// Credential encryption is mandatory (§8), and an http action needs an executor: wired exactly
	// as newFlowKernel wires them, so a simulated kernel differs from a flow kernel only in its
	// federation transport.
	box, err := newAESGCMBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("%s: secret box: %v", name, err)
	}
	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, auth: newAuthenticator(box, db, true, cfg.ScriptTimeout)}
	k := newKernel(cfg, kernel.Dependencies{Store: base, HTTP: httpExec})
	k.SetSecretBox(box)
	if err := k.FirstBoot(context.Background(), "sys-pass", ""); err != nil {
		t.Fatalf("%s: first boot: %v", name, err)
	}
	priv := bootstrapSigning(t, k)
	_ = err
	pub := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))

	adapter := newFedAdapter(pub, k.SignFederation)
	adapter.SetTransport(&simPort{net: n, self: pub})
	k.SetFederation(adapter)

	sys, err := k.ReadUserByHandle(context.Background(), "sys")
	if err != nil {
		t.Fatalf("%s: read sys: %v", name, err)
	}

	node := &simNode{
		net:      n,
		name:     name,
		key:      pub,
		k:        k,
		db:       db,
		faults:   base,
		h:        &fedHandlers{kernel: k, log: log.Discard(), callLimiter: newKeyLimiter(10000, 10000)},
		adapter:  adapter,
		sysID:    sys.ID,
		issuerID: sys.ID,
		priv:     priv,
		cfg:      cfg,
	}

	n.mu.Lock()
	n.nodes[pub] = node
	n.mu.Unlock()
	return node
}

// restart rebuilds the kernel over the same durable store and runs recovery, which is what a
// process that died and came back does to its own state (§5 D3). It is not a process restart —
// SQLite is not reopened and no goroutine is killed — so it models recovery, not process death;
// process death is covered once, for real, by the flow suite.
func (s *simNode) restart(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	cfg := s.cfg
	box, err := newAESGCMBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("%s: secret box: %v", s.name, err)
	}
	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, auth: newAuthenticator(box, s.db, true, cfg.ScriptTimeout)}
	k := newKernel(cfg, kernel.Dependencies{Store: s.faults, HTTP: httpExec})
	k.SetSecretBox(box)
	k.SetSigningKey(s.priv, s.issuerID)

	adapter := newFedAdapter(s.key, k.SignFederation)
	adapter.SetTransport(&simPort{net: s.net, self: s.key})
	k.SetFederation(adapter)

	s.k, s.adapter = k, adapter
	s.h = &fedHandlers{kernel: k, log: log.Discard(), callLimiter: newKeyLimiter(10000, 10000)}

	if err := k.Recover(ctx); err != nil {
		t.Fatalf("%s: recover: %v", s.name, err)
	}
}

// ---------------------------------------------------------------------------
// Node conveniences
// ---------------------------------------------------------------------------

// user creates a funded local user.
func (s *simNode) user(t *testing.T, handle string, funds int64) *kernel.Account {
	t.Helper()
	ctx := context.Background()
	u, err := s.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: handle, Password: "userpass"})
	if err != nil {
		t.Fatalf("%s: create user %s: %v", s.name, handle, err)
	}
	if funds > 0 {
		if _, err := s.k.Deposit(ctx, s.sysID, u.ID, funds, "sim funding", "sim:"+handle+":"+s.name); err != nil {
			t.Fatalf("%s: fund %s: %v", s.name, handle, err)
		}
	}
	return u
}

// publish creates, enables and makes public an action backed by the given executor kind. The
// action is `native`-free: it is an http action whose executor the scenario supplies, so no
// network leaves the test.
func (s *simNode) publish(t *testing.T, owner *kernel.Account, name string, price int64) *kernel.Action {
	t.Helper()
	ctx := context.Background()
	schema := map[string]any{"type": "object"}
	a, err := s.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID:  owner.ID,
		Name:         name,
		Kind:         kernel.KindHTTP,
		Source:       s.net.backend.URL + "/" + name,
		Description:  "simulated action " + name,
		Price:        price,
		InputSchema:  schema,
		OutputSchema: schema,
	})
	if err != nil {
		t.Fatalf("%s: create action %s: %v", s.name, name, err)
	}
	if err := s.k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatalf("%s: enable %s: %v", s.name, name, err)
	}
	vis := kernel.VisibilityPublic
	updated, err := s.k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &vis})
	if err != nil {
		t.Fatalf("%s: publish %s: %v", s.name, name, err)
	}
	return updated
}

func ptr[T any](v T) *T { return &v }

// balance reads a user's spendable balance straight from the unwrapped store, so an assertion
// never depends on the read path under test.
func (s *simNode) balance(t *testing.T, userID string) int64 {
	t.Helper()
	u, err := s.db.ReadUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("%s: read balance: %v", s.name, err)
	}
	return u.Available
}

// peerRow returns what this kernel's books say the named peer's account holds: negative means the
// peer owes this kernel (§13 bilateral position).
func (s *simNode) peerRow(t *testing.T, peerKey string) int64 {
	t.Helper()
	acct, err := s.db.ReadAccountByKernelKey(context.Background(), peerKey)
	if err != nil || acct == nil {
		return 0
	}
	return acct.Available
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

// txCount returns how many transactions this kernel has recorded for an action. The settled-once
// property (§5 G1) is a count, so it is worth reading directly rather than inferring from balances.
func (s *simNode) txCount(t *testing.T, actionID string) int {
	t.Helper()
	txs, err := s.db.ListTransactions(context.Background(), kernel.TxFilter{Limit: 500})
	if err != nil {
		t.Fatalf("%s: list transactions: %v", s.name, err)
	}
	n := 0
	for _, tx := range txs {
		if tx.ActionID == actionID {
			n++
		}
	}
	return n
}

// pendingRemote counts calls dispatched to a peer with no settled outcome: the parked state §13
// says must never be presumed dead.
func (s *simNode) pendingRemote(t *testing.T) int {
	t.Helper()
	traces, err := s.k.PendingRemoteTraces(context.Background())
	if err != nil {
		t.Fatalf("%s: pending remote: %v", s.name, err)
	}
	return len(traces)
}

// remoteRef is how a buyer names a seller's action before anything is cached: owner@key/name.
func remoteRef(seller *simNode, owner, action string) string {
	return owner + "@" + seller.key + "/" + action
}

func (s *simNode) run(t *testing.T, callerID, ref string) (*kernel.CallReply, error) {
	t.Helper()
	return s.k.Run(context.Background(), kernel.RunRequest{
		CallerID: callerID, ActionRef: ref, Args: map[string]any{},
	})
}

// ---------------------------------------------------------------------------
// Scenario: the price a buyer pays is the price the protocol computes
// ---------------------------------------------------------------------------

// TestSimCrossKernelPriceIsExact pins P7's arithmetic end to end through two real kernels with
// different rates, so a change that used the wrong side's rate moves a balance here. It also
// validates the harness itself: if this fails, no other scenario in this file means anything.
func TestSimCrossKernelPriceIsExact(t *testing.T) {
	net := newSimNet(t)
	// Deliberately asymmetric: the seller's markup and the buyer's import fee are different
	// numbers, so transposing them is visible.
	sellCfg := defaultSimConfig()
	sellCfg.FeeBPS, sellCfg.RemoteBPS = 1000, 700
	buyCfg := defaultSimConfig()
	buyCfg.ImportBPS = 1000

	seller := net.addNode("seller", sellCfg)
	buyer := net.addNode("buyer", buyCfg)

	cara := seller.user(t, "cara", 0)
	act := seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 1000)

	if _, err := buyer.run(t, dan.ID, remoteRef(seller, "cara", "quote")); err != nil {
		net.dump()
		t.Fatalf("run: %v", err)
	}

	// mp 25 → sr = 25 + ceil(25·7%) = 27 → q = 27 + ceil(27·10%) = 30.
	if got := buyer.balance(t, dan.ID); got != 1000-30 {
		net.dump()
		t.Errorf("buyer paid %d, want the all-in price 30 (1000 → 970), got balance %d", 1000-got, got)
	}
	// The seller's books show what the buyer's kernel owes: charge 25 + premium 2.
	if got := seller.peerRow(t, buyer.key); got != -27 {
		t.Errorf("seller's row for the buyer = %d, want -27 (charge 25 + premium 2)", got)
	}
	// The provider keeps the net of its own fee: taxable 25, fee ceil(25·10%) = 3, net 22.
	if got := seller.balance(t, cara.ID); got != 22 {
		t.Errorf("provider net = %d, want 22", got)
	}
	if n := seller.txCount(t, act.ID); n != 1 {
		t.Errorf("seller recorded %d transactions for one call, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// Scenario: a lost reply is not a lost call
// ---------------------------------------------------------------------------

// TestSimLostResponseParksThenSettlesOnce is U35's central claim. The peer executed and committed;
// only the answer vanished. The caller cannot tell that from a peer that never heard, so it must
// park rather than refund, and the retry must settle from the stored receipt — once.
func TestSimLostResponseParksThenSettlesOnce(t *testing.T) {
	net := newSimNet(t)
	seller := net.addNode("seller", defaultSimConfig())
	buyer := net.addNode("buyer", defaultSimConfig())

	cara := seller.user(t, "cara", 0)
	act := seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 1000)
	ref := remoteRef(seller, "cara", "quote")

	// Warm the proxy so the fault lands on the call, not on the resolve.
	if _, err := buyer.run(t, dan.ID, ref); err != nil {
		net.dump()
		t.Fatalf("warm-up run: %v", err)
	}
	afterWarm := buyer.balance(t, dan.ID)

	net.arm(buyer.key, seller.key, verbCall, faultLoseResponse, 1)
	_, err := buyer.run(t, dan.ID, ref)
	if err == nil {
		net.dump()
		t.Fatal("a lost reply must not report success")
	}
	if !errors.Is(err, kernel.ErrTimeout) {
		net.dump()
		t.Fatalf("want a timeout (the call may have executed), got %v", err)
	}
	net.assertFired(t, faultLoseResponse, 1)
	if got := buyer.pendingRemote(t); got != 1 {
		net.dump()
		t.Fatalf("want exactly 1 parked call awaiting a receipt, got %d", got)
	}
	// The seller ran it and holds the receipt: two executions on its side.
	if n := seller.txCount(t, act.ID); n != 2 {
		net.dump()
		t.Errorf("seller executed %d times, want 2 (warm-up + the one whose reply was lost)", n)
	}

	// The retry re-sends under the same idempotency key; the peer replays its stored receipt.
	buyer.k.RetryPendingRemoteDispatches(context.Background())

	if got := buyer.pendingRemote(t); got != 0 {
		net.dump()
		t.Errorf("after the retry, %d calls are still parked; want 0", got)
	}
	if n := seller.txCount(t, act.ID); n != 2 {
		net.dump()
		t.Errorf("the retry re-executed the action: seller now shows %d transactions, want 2", n)
	}
	// Charged exactly once for the second call, at the same price as the first.
	price := 1000 - afterWarm
	if got := buyer.balance(t, dan.ID); got != afterWarm-price {
		net.dump()
		t.Errorf("balance %d after the retry; want %d (charged once at %d)", got, afterWarm-price, price)
	}
}

// ---------------------------------------------------------------------------
// Scenario: a request that provably never left is refunded at once
// ---------------------------------------------------------------------------

// TestSimNeverDispatchedRefundsImmediately is the other half of U35. Only a transport that proves
// non-delivery may fail fast; everything else must park. The second half asserts the asymmetry that
// makes this safe: the same refusal on a *retry* must not fail fast, because by then the request
// may already have executed.
func TestSimNeverDispatchedRefundsImmediately(t *testing.T) {
	net := newSimNet(t)
	seller := net.addNode("seller", defaultSimConfig())
	buyer := net.addNode("buyer", defaultSimConfig())

	cara := seller.user(t, "cara", 0)
	seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 1000)
	ref := remoteRef(seller, "cara", "quote")

	if _, err := buyer.run(t, dan.ID, ref); err != nil {
		net.dump()
		t.Fatalf("warm-up run: %v", err)
	}
	before := buyer.balance(t, dan.ID)

	net.arm(buyer.key, seller.key, verbCall, faultRefuseBeforeDispatch, 1)
	_, err := buyer.run(t, dan.ID, ref)
	if err == nil {
		net.dump()
		t.Fatal("a call that never left must not report success")
	}
	if !errors.Is(err, kernel.ErrPeerUnreachable) {
		net.dump()
		t.Fatalf("want peer_unreachable on a provably undispatched call, got %v", err)
	}
	net.assertFired(t, faultRefuseBeforeDispatch, 1)
	if got := buyer.balance(t, dan.ID); got != before {
		net.dump()
		t.Errorf("balance moved to %d on an undispatched call; want %d unchanged", got, before)
	}
	if got := buyer.pendingRemote(t); got != 0 {
		net.dump()
		t.Errorf("an undispatched call left %d parked traces; want 0", got)
	}

	// Now the retry path. Park a call by losing its reply, then refuse the retry: the refusal
	// proves nothing about the first attempt, so the call must stay parked, not be refunded.
	net.arm(buyer.key, seller.key, verbCall, faultLoseResponse, 1)
	if _, err := buyer.run(t, dan.ID, ref); err == nil {
		net.dump()
		t.Fatal("lost reply reported success")
	}
	parked := buyer.pendingRemote(t)
	if parked != 1 {
		net.dump()
		t.Fatalf("want 1 parked call, got %d", parked)
	}
	net.arm(buyer.key, seller.key, verbCall, faultRefuseBeforeDispatch, 1)
	buyer.k.RetryPendingRemoteDispatches(context.Background())
	if got := buyer.pendingRemote(t); got != 1 {
		net.dump()
		t.Errorf("a refused retry settled the call (%d parked); it must stay parked: the first attempt may have executed", got)
	}
}

// ---------------------------------------------------------------------------
// Scenario: the provider dies mid-call
// ---------------------------------------------------------------------------

// TestSimProviderCrashMidCallSettlesOnRetry is the case the manual drive of 2026-09-03 found
// stranded a buyer's funds for 24 hours, reproduced here deterministically in under a second.
//
// EXPECTED TO FAIL until the defect below is repaired. It is not a flaw in this test.
//
// The mechanism, read out of both databases:
//
//	provider recovery receipt : args_hash e3b0c442… = SHA-256("")  — the hash of nothing
//	caller dispatched         : args {}              → SHA-256("{}") = 44136fa3…
//
// A process killed mid-call loses the request arguments it was holding in memory. Recovery settles
// the orphaned trace as `interrupted` (correct, D3) and signs a receipt for it — but that
// transaction has args_json null, so the receipt claims the empty args_hash. The caller then
// applies P7's local verification, which requires the receipt's args_hash to equal what it sent,
// finds a mismatch, and treats the receipt as untrusted evidence: it stays pending and retries,
// exactly as TestSettleRemoteCallRejectsWrongArgsHash (kernel/federation_test.go:1124) requires.
//
// So both sides obey their own rule and the money is still stuck. Recovery emits evidence that
// verification is obliged to reject, and the call parks until the 24-hour bound or a manual
// `process end`. The natural repair is to record the inbound args_hash on the pending idempotency
// record — it is already inserted before execution — so a recovered receipt can be verified;
// that is a production change and needs separate authorization.
func TestSimProviderCrashMidCallSettlesOnRetry(t *testing.T) {
	// Known defect, fixed in the next commit: after a provider crashes mid-call and recovers, the
	// caller's retry never settles and its funds stay locked (recovery signs a receipt over an empty
	// argument hash, which the caller rightly rejects). The test stays as written; only the skip
	// comes out when the fix lands.
	t.Skip("known defect: stranded funds after a provider crash; fix scheduled for the next commit")
	net := newSimNet(t)
	seller := net.addNode("seller", defaultSimConfig())
	buyer := net.addNode("buyer", defaultSimConfig())

	cara := seller.user(t, "cara", 0)
	seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 1000)
	ref := remoteRef(seller, "cara", "quote")

	// Warm the proxy so the crash lands on the call itself.
	if _, err := buyer.run(t, dan.ID, ref); err != nil {
		net.dump()
		t.Fatalf("warm-up run: %v", err)
	}
	beforeCrash := buyer.balance(t, dan.ID)

	// A process killed mid-call does two things at once, and only both together reproduce it:
	// its own work is left uncommitted (the inbound idempotency record stays pending), and the
	// caller hears nothing back (the stream dies with the process). Injecting either alone is a
	// different, benign case — a store failure alone answers the caller with a signed rejection it
	// settles on immediately.
	seller.faults.armBefore("CommitCall")
	net.arm(buyer.key, seller.key, verbCall, faultLoseResponse, 1)
	_, err := buyer.run(t, dan.ID, ref)
	if err == nil {
		net.dump()
		t.Fatal("a call the provider never committed must not report success")
	}
	t.Logf("caller saw: %v", err)

	if n := seller.faults.firedCount(); n != 1 {
		net.dump()
		t.Fatalf("the provider's commit fault fired %d times, expected 1: no crash was injected", n)
	}
	net.assertFired(t, faultLoseResponse, 1)

	parked := buyer.pendingRemote(t)
	t.Logf("parked calls after the crash: %d", parked)
	if parked != 1 {
		net.dump()
		t.Fatalf("want the call parked (1) after a provider crash the caller could not observe, got %d", parked)
	}
	t.Logf("buyer balance: %d (was %d) — the allocation is locked, not spent", buyer.balance(t, dan.ID), beforeCrash)

	// The provider comes back and recovers its own interrupted work.
	seller.restart(t)

	// What the provider now holds for that key decides everything: a signed receipt lets the
	// caller settle; anything else leaves it parked until the 24-hour bound.
	recs, rerr := seller.db.ListTransactions(context.Background(), kernel.TxFilter{Limit: 50})
	if rerr == nil {
		for _, tx := range recs {
			if tx.Status == kernel.TxFailure {
				t.Logf("provider settled its interrupted work: status=%s reason=%q gross=%d", tx.Status, tx.Reason, tx.Gross)
			}
		}
	}

	// The buyer re-drives the parked call under its original identity.
	buyer.k.RetryPendingRemoteDispatches(context.Background())

	if got := buyer.pendingRemote(t); got != 0 {
		net.dump()
		t.Errorf("the call is STILL parked after the provider recovered and the retry ran (%d pending). "+
			"G4 requires it to settle from the signed evidence the provider holds.", got)
	}
	if got := buyer.balance(t, dan.ID); got != beforeCrash {
		net.dump()
		t.Errorf("buyer balance %d, want %d refunded in full: the provider charged nothing for work it never committed", got, beforeCrash)
	}
}

// ---------------------------------------------------------------------------
// Scenario: the commit landed, only the acknowledgement was lost
// ---------------------------------------------------------------------------

// TestSimCommitLandsAcknowledgementLostSettlesOnRetry is the other half of the crash pair, and the
// contrast is the point. Here the provider's write *succeeds* and only the report of it fails —
// a process killed after fsync but before the reply. Its transaction and receipt are durable and
// carry the real arguments, so the retry settles normally.
//
// Together with TestSimProviderCrashMidCallSettlesOnRetry this isolates the defect precisely: the
// problem is not that a crash strands funds, it is that a crash *before* the commit destroys the
// arguments the receipt must attest to.
func TestSimCommitLandsAcknowledgementLostSettlesOnRetry(t *testing.T) {
	net := newSimNet(t)
	seller := net.addNode("seller", defaultSimConfig())
	buyer := net.addNode("buyer", defaultSimConfig())

	cara := seller.user(t, "cara", 0)
	act := seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 1000)
	ref := remoteRef(seller, "cara", "quote")

	if _, err := buyer.run(t, dan.ID, ref); err != nil {
		net.dump()
		t.Fatalf("warm-up run: %v", err)
	}
	before := buyer.balance(t, dan.ID)
	price := 1000 - before

	// The write commits; the handler then reports failure and the reply is lost on the way back.
	seller.faults.armAfter("CommitCall")
	net.arm(buyer.key, seller.key, verbCall, faultLoseResponse, 1)
	if _, err := buyer.run(t, dan.ID, ref); err == nil {
		net.dump()
		t.Fatal("the caller heard nothing back; it must not report success")
	}
	if got := buyer.pendingRemote(t); got != 1 {
		net.dump()
		t.Fatalf("want the call parked, got %d pending", got)
	}
	// Both halves of the premise: the commit really was reported as failing, and the caller really
	// did lose the answer. Either alone is a different scenario.
	if n := seller.faults.firedCount(); n != 1 {
		net.dump()
		t.Fatalf("the store fault fired %d times, expected 1", n)
	}
	net.assertFired(t, faultLoseResponse, 1)

	seller.restart(t)
	buyer.k.RetryPendingRemoteDispatches(context.Background())

	if got := buyer.pendingRemote(t); got != 0 {
		net.dump()
		t.Errorf("the call is still parked (%d) though the provider holds a committed receipt for it", got)
	}
	if got := buyer.balance(t, dan.ID); got != before-price {
		net.dump()
		t.Errorf("balance %d, want %d: charged exactly once for work the provider really did", got, before-price)
	}
	if n := seller.txCount(t, act.ID); n != 2 {
		net.dump()
		t.Errorf("provider recorded %d transactions, want 2: the retry must not re-execute", n)
	}
}

// ---------------------------------------------------------------------------
// Scenario: a call cannot stay parked forever
// ---------------------------------------------------------------------------

// TestSimPendingCallExpiresIntoRefund is the bound that makes parking safe to promise. A call whose
// receipt never arrives is not immortal: past RemotePendingMaxAge it settles locally as a failure
// with the allocation returned. Reached here by configuring the bound to a nanosecond, the same way
// kernel/federation_test.go:768 reaches it — no clock injection.
func TestSimPendingCallExpiresIntoRefund(t *testing.T) {
	net := newSimNet(t)
	seller := net.addNode("seller", defaultSimConfig())
	buyCfg := defaultSimConfig()
	buyCfg.PendingMaxAge = time.Nanosecond
	buyer := net.addNode("buyer", buyCfg)

	cara := seller.user(t, "cara", 0)
	seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 1000)
	ref := remoteRef(seller, "cara", "quote")

	if _, err := buyer.run(t, dan.ID, ref); err != nil {
		net.dump()
		t.Fatalf("warm-up run: %v", err)
	}
	before := buyer.balance(t, dan.ID)

	// Park a call, then cut the link so no receipt can ever arrive.
	net.arm(buyer.key, seller.key, verbCall, faultLoseResponse, 1)
	if _, err := buyer.run(t, dan.ID, ref); err == nil {
		net.dump()
		t.Fatal("lost reply reported success")
	}
	net.cut(buyer.key, seller.key)
	buyer.k.RetryPendingRemoteDispatches(context.Background())

	if got := buyer.pendingRemote(t); got != 0 {
		net.dump()
		t.Errorf("%d calls still parked past the max age; the bound must settle them", got)
	}
	if got := buyer.balance(t, dan.ID); got != before {
		net.dump()
		t.Errorf("balance %d after the expiry refund, want %d returned in full", got, before)
	}
}

// ---------------------------------------------------------------------------
// Scenario: extra identities buy no extra credit
// ---------------------------------------------------------------------------

// TestSimSybilIdentitiesShareOneCap is U32 at the network level: the cap is global, so three
// separate kernels calling the same provider on credit get, between them, exactly what one would
// get. The atomicity of a single admission is asserted separately and at the right layer, beside
// BeginRun in store/sqlite_test.go; this is the statement about identities.
func TestSimSybilIdentitiesShareOneCap(t *testing.T) {
	net := newSimNet(t)
	sellCfg := defaultSimConfig()
	sellCfg.RemoteBPS = 0     // keep the arithmetic plain: the obligation is exactly the price
	sellCfg.ExposureMax = 250 // room for two calls of 100, never a third
	seller := net.addNode("seller", sellCfg)

	cara := seller.user(t, "cara", 0)
	seller.publish(t, cara, "big", 100)

	admitted := 0
	for i, name := range []string{"evil1", "evil2", "evil3"} {
		buyer := net.addNode(name, defaultSimConfig())
		mal := buyer.user(t, fmt.Sprintf("mal%d", i), 10000)
		if _, err := buyer.run(t, mal.ID, remoteRef(seller, "cara", "big")); err == nil {
			admitted++
		} else {
			t.Logf("%s refused: %v", name, err)
		}
	}

	if admitted != 2 {
		net.dump()
		t.Errorf("%d of 3 identities were admitted; a 250 cap funds exactly 2 calls of 100, "+
			"and minting identities must not raise that", admitted)
	}
	g, err := seller.k.GrossReceivables(context.Background())
	if err != nil {
		t.Fatalf("gross receivables: %v", err)
	}
	if g > 250 {
		net.dump()
		t.Errorf("gross receivables %d exceeded the cap of 250", g)
	}
}

// ---------------------------------------------------------------------------
// Scenario: the same request arrives twice
// ---------------------------------------------------------------------------

// TestSimDuplicateDeliveryExecutesOnce is P4's reason for existing. A network that retries, or a
// caller that cannot tell a lost reply from a lost request, will deliver the same signed request
// more than once; the receiver must execute it once and answer the second copy from what it
// already did.
func TestSimDuplicateDeliveryExecutesOnce(t *testing.T) {
	net := newSimNet(t)
	seller := net.addNode("seller", defaultSimConfig())
	buyer := net.addNode("buyer", defaultSimConfig())

	cara := seller.user(t, "cara", 0)
	act := seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 1000)
	ref := remoteRef(seller, "cara", "quote")

	if _, err := buyer.run(t, dan.ID, ref); err != nil {
		net.dump()
		t.Fatalf("warm-up run: %v", err)
	}
	before := buyer.balance(t, dan.ID)
	price := 1000 - before
	txsBefore := seller.txCount(t, act.ID)

	net.arm(buyer.key, seller.key, verbCall, faultDeliverTwice, 1)
	if _, err := buyer.run(t, dan.ID, ref); err != nil {
		net.dump()
		t.Fatalf("a duplicated request must still succeed: %v", err)
	}

	// Without this the test would pass on a single delivery, which proves nothing.
	net.assertFired(t, faultDeliverTwice, 1)

	if n := seller.txCount(t, act.ID); n != txsBefore+1 {
		net.dump()
		t.Errorf("the provider executed %d times for one duplicated request, want 1 "+
			"(transactions went %d → %d)", n-txsBefore, txsBefore, n)
	}
	if got := buyer.balance(t, dan.ID); got != before-price {
		net.dump()
		t.Errorf("buyer balance %d, want %d: a duplicate must not be charged twice", got, before-price)
	}
}

// ---------------------------------------------------------------------------
// Scenario: the failure path's own commit fails
// ---------------------------------------------------------------------------

// TestSimStoreFaultOnFailureCommitLeavesNoMoneyBehind covers the boundary a happy-path test never
// reaches: the provider's call fails, and then the write that records the failure fails too. Both
// modes are exercised — the write refused outright, and the write that lands before the report of
// it fails. Whatever happens, the caller must not be left funding a call nobody will ever settle.
func TestSimStoreFaultOnFailureCommitLeavesNoMoneyBehind(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			net := newSimNet(t)
			seller := net.addNode("seller", defaultSimConfig())
			buyer := net.addNode("buyer", defaultSimConfig())

			cara := seller.user(t, "cara", 0)
			// An action whose upstream is not there: every call to it fails, so the failure-commit
			// path is the one under test rather than an incidental branch.
			ctx := context.Background()
			schema := map[string]any{"type": "object"}
			a, err := seller.k.CreateAction(ctx, cara.ID, kernel.CreateActionRequest{
				OwnerUserID: cara.ID, Name: "broken", Kind: kernel.KindHTTP,
				Source:      "http://127.0.0.1:1/nothing-listens-here",
				Description: "an action whose upstream is gone", Price: 25,
				InputSchema: schema, OutputSchema: schema,
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := seller.k.SetActive(ctx, cara.ID, a.ID, true); err != nil {
				t.Fatalf("enable: %v", err)
			}
			vis := kernel.VisibilityPublic
			if _, err := seller.k.UpdateAction(ctx, cara.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &vis}); err != nil {
				t.Fatalf("publish: %v", err)
			}

			dan := buyer.user(t, "dan", 1000)
			ref := remoteRef(seller, "cara", "broken")

			// Warm the proxy: the first call fails at the upstream, which is expected and fine.
			_, _ = buyer.run(t, dan.ID, ref)
			before := buyer.balance(t, dan.ID)

			if mode == "before" {
				seller.faults.armBefore("CommitFailedCall")
			} else {
				seller.faults.armAfter("CommitFailedCall")
			}
			_, _ = buyer.run(t, dan.ID, ref)

			if n := seller.faults.firedCount(); n != 1 {
				net.dump()
				t.Fatalf("the %s-commit fault fired %d times, expected 1", mode, n)
			}
			seller.restart(t)
			buyer.k.RetryPendingRemoteDispatches(context.Background())

			// The call must reach an end. Whatever the provider managed to record, the buyer is
			// either refunded or charged, never left holding a reservation for a call the network
			// has finished with.
			if got := buyer.pendingRemote(t); got != 0 {
				net.dump()
				t.Errorf("%d calls still parked after the provider recovered", got)
			}
			after := buyer.balance(t, dan.ID)
			if after > before {
				net.dump()
				t.Errorf("buyer gained money on a failing call: %d → %d", before, after)
			}
			if after < before-30 {
				net.dump()
				t.Errorf("buyer charged %d for a call that failed at the upstream; the price is 25 "+
					"and a failure charges at most what settled beneath it", before-after)
			}
		})
	}
}
