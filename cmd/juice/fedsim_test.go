// SPDX-License-Identifier: AGPL-3.0-only

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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/native"
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
	verbCall     simVerb = "call"
	verbResolve  simVerb = "resolve"
	verbReveal   simVerb = "reveal"
	verbTask     simVerb = "task"
	verbTransfer simVerb = "transfer"
	verbGrant    simVerb = "grant"
	verbProcess  simVerb = "process"
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
		p.net.logf("%s -> %s %s BLOCKED (link cut)", key8(p.self), key8(peerKey), verb)
		return fed.Response{}, fmt.Errorf("sim: link cut: %w", fed.ErrNotDispatched)
	}
	dst := p.net.node(peerKey)
	if dst == nil {
		return fed.Response{}, fmt.Errorf("sim: no such peer %s: %w", key8(peerKey), fed.ErrNotDispatched)
	}

	switch f := p.net.take(p.self, peerKey, verb); f {
	case faultRefuseBeforeDispatch:
		p.net.logf("%s -> %s %s REFUSED before dispatch", key8(p.self), key8(peerKey), verb)
		return fed.Response{}, fmt.Errorf("sim: refused: %w", fed.ErrNotDispatched)

	case faultLoseResponse:
		// The peer does the work. Only the answer is lost — which is why this must not be
		// reported as not-dispatched: the call may well have executed and been charged.
		resp := call(dst.h)
		p.net.logf("%s -> %s %s delivered (status %d), RESPONSE LOST", key8(p.self), key8(peerKey), verb, resp.Status)
		return fed.Response{}, fmt.Errorf("sim: response lost after dispatch")

	case faultDeliverTwice:
		first := call(dst.h)
		second := call(dst.h)
		p.net.logf("%s -> %s %s delivered TWICE (status %d then %d)", key8(p.self), key8(peerKey), verb, first.Status, second.Status)
		return second, nil

	default:
		resp := call(dst.h)
		p.net.logf("%s -> %s %s status %d %s", key8(p.self), key8(peerKey), verb, resp.Status, body(resp))
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

func (p *simPort) Reveal(ctx context.Context, peerKey string, req fed.RevealRequest) (fed.RevealResponse, error) {
	return p.deliver(ctx, peerKey, verbReveal, func(h *fedHandlers) fed.Response {
		return h.OnReveal(ctx, p.self, req)
	})
}

func (p *simPort) Task(ctx context.Context, peerKey string, req fed.TaskRequest) (fed.TaskResponse, error) {
	return p.deliver(ctx, peerKey, verbTask, func(h *fedHandlers) fed.Response {
		return h.OnTask(ctx, p.self, req)
	})
}

func (p *simPort) Transfer(ctx context.Context, peerKey string, req fed.TransferRequest) (fed.TransferResponse, error) {
	return p.deliver(ctx, peerKey, verbTransfer, func(h *fedHandlers) fed.Response {
		return h.OnTransfer(ctx, p.self, req)
	})
}

func (p *simPort) Grant(ctx context.Context, peerKey string, req fed.GrantRequest) (fed.Response, error) {
	return p.deliver(ctx, peerKey, verbGrant, func(h *fedHandlers) fed.Response {
		return h.OnGrant(ctx, p.self, req)
	})
}

func (p *simPort) Process(ctx context.Context, peerKey string, req fed.ProcessRequest) (fed.Response, error) {
	return p.deliver(ctx, peerKey, verbProcess, func(h *fedHandlers) fed.Response {
		return h.OnProcess(ctx, p.self, req)
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

func key8(key string) string {
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
// kernel/call_test.go and kernel/tasks_test.go, and the same shape production uses in pricedStore.
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
	net, fee int64, stats *kernel.Stats, idempotencyRecordID, taskID string) error {

	return f.fault("CommitCall", func() error {
		return f.Store.CommitCall(ctx, tx, receipt, traceID, callerWalletID, callerWalletKind,
			targetUserID, feeRecipientID, net, fee, stats, idempotencyRecordID, taskID)
	})
}

func (f *faultStore) CommitFailedCall(ctx context.Context, tx *kernel.Transaction,
	buildReceipt func(refund int64) (*kernel.Receipt, error),
	traceID, callerWalletID, callerWalletKind, feeRecipientID string, gross int64,
	stats *kernel.Stats, idempotencyRecordID, taskID string) error {

	return f.fault("CommitFailedCall", func() error {
		return f.Store.CommitFailedCall(ctx, tx, buildReceipt, traceID, callerWalletID,
			callerWalletKind, feeRecipientID, gross, stats, idempotencyRecordID, taskID)
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
	CreditLimit   int64
	Lottery       int64
	LotteryMax    int64
	PeerRetention time.Duration
}

// The default simulated economy pays every obligation exactly (no lottery), so a test asserting an
// amount gets the one it wrote; a test about the draw turns the lottery on deliberately.
func defaultSimConfig() simConfig {
	return simConfig{FeeBPS: 1000, RemoteBPS: 500, ImportBPS: 500, CreditLimit: 100000, LotteryMax: 1_000_000}
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
	econ := testEconomy()
	econ.FeeBPS, econ.RemoteBPS, econ.ImportBPS = sc.FeeBPS, sc.RemoteBPS, sc.ImportBPS
	econ.CreditLimit = sc.CreditLimit
	econ.Lottery, econ.LotteryMax = sc.Lottery, sc.LotteryMax
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
	// A callback address, so an endpoint is handed the capability it composes on (§9). The scenarios
	// that compose call the kernel in-process with it, so the address itself is never dialled.
	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, auth: newAuthenticator(box, db, true, cfg.ScriptTimeout), callbackURL: "http://sim.invalid"}
	k := newKernel(cfg, kernel.Dependencies{Store: base, HTTP: httpExec, Economy: econ})
	k.SetSecretBox(box)
	if err := k.FirstBoot(context.Background(), "sys-pass", ""); err != nil {
		t.Fatalf("%s: first boot: %v", name, err)
	}
	priv := bootstrapSigning(t, k)
	// The one native a scenario here calls: this kernel's own transfer (D18), installed as boot does.
	native.Register(k, []native.Spec{native.Transfer()})
	if err := ensureSysNative(context.Background(), k, "sys", native.Transfer(), 0); err != nil {
		t.Fatalf("%s: install transfer: %v", name, err)
	}
	pub := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))

	adapter := newFedAdapter(pub, k.SignFederation, nil)
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
		h:        &fedHandlers{kernel: k, log: log.Discard(), callLimiter: newKeyLimiter(context.Background(), 10000, 10000)},
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
	native.Register(k, []native.Spec{native.Transfer()})

	adapter := newFedAdapter(s.key, k.SignFederation, nil)
	adapter.SetTransport(&simPort{net: s.net, self: s.key})
	k.SetFederation(adapter)

	s.k, s.adapter = k, adapter
	s.h = &fedHandlers{kernel: k, log: log.Discard(), callLimiter: newKeyLimiter(context.Background(), 10000, 10000)}

	if err := k.Recover(ctx); err != nil {
		t.Fatalf("%s: recover: %v", s.name, err)
	}
}

// ---------------------------------------------------------------------------
// Node conveniences
// ---------------------------------------------------------------------------

// sellerCapital is what a provider holds to serve foreigners. A foreign call is funded by the
// seller, not by the buyer's row (P10), so a provider needs its own price available while the call
// runs; it comes back at settlement, and the ticket brings what the buyer owes.
const sellerCapital = 100000

// user creates a funded local user.
func (s *simNode) user(t *testing.T, handle string, funds int64) *kernel.Account {
	t.Helper()
	ctx := context.Background()
	u, err := s.k.CreateUser(ctx, kernel.CreateUserRequest{Handle: handle + "@" + testOwnName, Password: "userpass"})
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
		Title:        "Test action",
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

// owedBy is the obligation this kernel holds against one peer, or nil when it holds none.
func (s *simNode) owedBy(t *testing.T, peerKey string) *kernel.Owed {
	t.Helper()
	ctx := context.Background()
	// The obligation is named by the call the seller admitted: once committed, by the receipt that
	// names the request it answered — the execution lock is long gone by the time the money is
	// owed (P4, P10).
	var id string
	if err := s.db.QueryRowForTest(ctx,
		`SELECT r.idempotency_key FROM receipts r JOIN traces t ON t.id = r.trace_id
		  WHERE t.caller_kernel=? AND r.idempotency_key <> '' LIMIT 1`,
		peerKey, &id); err != nil || id == "" {
		return nil
	}
	r, _ := s.db.ReadOwed(ctx, id, peerKey)
	return r
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

	cara := seller.user(t, "cara", sellerCapital)
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
	// The seller's books show one obligation the buyer owes: charge 25 + premium 2. It is a ticket,
	// not a balance (P10).
	tk := seller.owedBy(t, buyer.key)
	if tk == nil || tk.Obligation != 27 {
		t.Fatalf("seller's obligation for the buyer = %+v, want 27 (charge 25 + premium 2)", tk)
	}
	// The provider funded its own work and got it back less its own fee: taxable 25, fee
	// ceil(25·10%) = 3, net 22. What the buyer owes arrives later, when the ticket settles.
	if got := seller.balance(t, cara.ID); got != sellerCapital-3 {
		t.Errorf("provider balance = %d, want %d (its own fee of 3 paid)", got, sellerCapital-3)
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

	cara := seller.user(t, "cara", sellerCapital)
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

	cara := seller.user(t, "cara", sellerCapital)
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
	net := newSimNet(t)
	seller := net.addNode("seller", defaultSimConfig())
	buyer := net.addNode("buyer", defaultSimConfig())

	cara := seller.user(t, "cara", sellerCapital)
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

	cara := seller.user(t, "cara", sellerCapital)
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

func TestSimSybilIdentitiesShareOneCap(t *testing.T) {
	net := newSimNet(t)
	sellCfg := defaultSimConfig()
	sellCfg.RemoteBPS = 0     // keep the arithmetic plain: the obligation is exactly the price
	sellCfg.CreditLimit = 250 // room for two calls of 100, never a third
	seller := net.addNode("seller", sellCfg)

	cara := seller.user(t, "cara", sellerCapital)
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
	sysSeller, _ := seller.k.ReadUserByHandle(context.Background(), "sys")
	e, err := seller.k.Exposure(context.Background(), sysSeller.ID)
	if err != nil {
		t.Fatalf("exposure: %v", err)
	}
	if e > 250 {
		net.dump()
		t.Errorf("exposure %d exceeded the limit of 250", e)
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

	cara := seller.user(t, "cara", sellerCapital)
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

			cara := seller.user(t, "cara", sellerCapital)
			// An action whose upstream is not there: every call to it fails, so the failure-commit
			// path is the one under test rather than an incidental branch.
			ctx := context.Background()
			schema := map[string]any{"type": "object"}
			a, err := seller.k.CreateAction(ctx, cara.ID, kernel.CreateActionRequest{
				OwnerUserID: cara.ID, Title: "Test action", Name: "broken", Kind: kernel.KindHTTP,
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

// ---------------------------------------------------------------------------
// Scenario: the lottery settles a cross-kernel obligation
// ---------------------------------------------------------------------------

// TestSimTicketSettlesEitherWay drives a real cross-kernel call and follows the money all the way
// through the draw: the buyer pays, reveals, and the seller's own books close against that reveal.
//
// Two face values, because the draw has two regimes and both must be reached deliberately rather
// than by whichever way today's randomness happens to fall. One above the obligation, where the
// draw genuinely applies and either outcome is valid; one the obligation covers, which is paid
// exactly, so the paying half is exercised on every run.
//
// The invariants are the same throughout. The buyer is charged the advertised price from its budget
// however the draw falls, because the obligation is a separate channel: it comes back to the caller,
// whose own stake then carries the draw. The seller learns the outcome from the reveal and never has
// to trust the buyer's word for it, because it recomputes the draw from what it committed to.
func TestSimTicketSettlesEitherWay(t *testing.T) {
	// mp 25 → sr = 27 → q = 30 at the rates below.
	const q, obligation, capital = int64(30), int64(27), int64(5000)
	for _, c := range []struct {
		name string
		face int64 // the face value the buyer dispatches under
		pays int64 // what a paying draw moves
		sure bool  // a face value the obligation covers is paid exactly, so it cannot lose
	}{
		{"a face value far above the obligation", 1000, 1000, false},
		{"a face value the obligation covers", 10, obligation, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			net := newSimNet(t)
			sellCfg := defaultSimConfig()
			sellCfg.FeeBPS, sellCfg.RemoteBPS = 1000, 700
			buyCfg := defaultSimConfig()
			buyCfg.ImportBPS = 1000
			buyCfg.Lottery = c.face

			seller := net.addNode("seller", sellCfg)
			buyer := net.addNode("buyer", buyCfg)

			cara := seller.user(t, "cara", sellerCapital)
			seller.publish(t, cara, "quote", 25)
			dan := buyer.user(t, "dan", capital)

			req := kernel.RunRequest{CallerID: dan.ID, ActionRef: remoteRef(seller, "cara", "quote"), Args: map[string]any{}, ExternalKey: "ticket-run"}
			reply, err := buyer.k.Run(context.Background(), req)
			if err != nil {
				net.dump()
				t.Fatalf("run: %v", err)
			}

			sold := seller.owedBy(t, buyer.key)
			if sold == nil || sold.Obligation != obligation {
				net.dump()
				t.Fatalf("seller's obligation = %+v, want %d", sold, obligation)
			}
			if _, lottery, _ := kernel.ServingTerms(&sold.Terms); lottery != c.face {
				t.Errorf("the obligation was agreed under a face value of %d, want %d", lottery, c.face)
			}

			// The buyer keeps no obligation of its own: what it paid is the payment row, and what it
			// drew is re-derivable from the trace and the receipt. That the two kernels agree is
			// exactly the check that the money moved matches what the seller says was decided.
			paid := buyer.paidFor(t, sold.ID)
			if paid != c.pays && !(paid == 0 && !c.sure) {
				t.Fatalf("the buyer paid %d, which is neither nothing nor the %d its draw could owe", paid, c.pays)
			}
			// One formula covers both outcomes: the price is charged, the obligation returns to the
			// caller, and the caller's own balance carries whatever the draw decided.
			if want := capital - q + obligation - paid; buyer.balance(t, dan.ID) != want {
				t.Errorf("after a draw paying %d the caller has %d, want %d", paid, buyer.balance(t, dan.ID), want)
			}
			// The run reports what the caller paid — the import fee, plus the ticket's payment when it
			// drew one — and a retry under the same key reports it again from the stored records.
			if want := q - obligation + paid; reply.Charge == nil || *reply.Charge != want {
				t.Fatalf("the run reported charge %s after a draw paying %d, want %d", chargeOf(reply), paid, want)
			}
			again, err := buyer.k.Run(context.Background(), req)
			if err != nil || again.Charge == nil || *again.Charge != *reply.Charge {
				t.Errorf("the replay reported charge %s (%v), want the first run's %d", chargeOf(again), err, *reply.Charge)
			}

			// Nobody records anything by hand. This world has no addresses, so the buyer's signed
			// reveal is itself the finalized payment (D23): one pass of the buyer's own worker sends
			// it, and the seller's books close against it — the provider credited exactly what the
			// draw decided, and the exposure the delivery created discharged by that cash and by
			// nothing else.
			providerBefore, exposureBefore := seller.balance(t, cara.ID), seller.exposure(t)
			// The rail drives the payment to final; telling the seller is its own pass (P10).
			buyer.k.RailPass(context.Background())
			buyer.k.RevealPending(context.Background())
			closed := seller.owedBy(t, buyer.key)
			if closed == nil {
				net.dump()
				t.Fatal("the seller's obligation vanished")
			}
			credited := seller.balance(t, cara.ID) - providerBefore
			wantStatus := kernel.OwedCredited
			if paid == 0 {
				wantStatus = kernel.OwedCancelled
			}
			if closed.Status != wantStatus || credited != paid {
				t.Errorf("a draw paying %d left the obligation %q and credited the provider %d, want %q and %d",
					paid, closed.Status, credited, wantStatus, paid)
			}
			if got := seller.exposure(t); got != exposureBefore-paid {
				t.Errorf("exposure = %d, want %d: the cash received is what discharges it, and nothing else",
					got, exposureBefore-paid)
			}
		})
	}
}

// A losing draw leaves no payment row under its ticket's id, and that id is the caller's to reuse —
// shown to them as the ticket id, and a valid id for a withdrawal. A keyed replay of the call must
// still report what the call cost at settlement, never the withdrawal that took the id afterwards.
func TestSimReplayIgnoresAWithdrawalUnderALosingTicketsID(t *testing.T) {
	net := newSimNet(t)
	sellCfg := defaultSimConfig()
	sellCfg.FeeBPS, sellCfg.RemoteBPS = 1000, 700
	buyCfg := defaultSimConfig()
	buyCfg.ImportBPS = 1000
	buyCfg.Lottery = 1000 // far above the obligation of 27: almost every draw loses
	seller := net.addNode("seller", sellCfg)
	buyer := net.addNode("buyer", buyCfg)
	cara := seller.user(t, "cara", sellerCapital)
	seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 100000)
	ctx := context.Background()

	for i := 0; i < 30; i++ {
		req := kernel.RunRequest{CallerID: dan.ID, ActionRef: remoteRef(seller, "cara", "quote"), Args: map[string]any{}, ExternalKey: fmt.Sprintf("losing-%d", i)}
		reply, err := buyer.k.Run(ctx, req)
		if err != nil {
			net.dump()
			t.Fatalf("run: %v", err)
		}
		if reply.Charge == nil || *reply.Charge != 3 { // the import fee alone: this draw lost
			continue
		}
		trace, err := buyer.k.ReadTrace(ctx, reply.TraceID)
		if err != nil || trace.IdempotencyKey == nil {
			t.Fatalf("read trace: %v", err)
		}
		if _, err := buyer.k.Withdraw(ctx, dan.ID, *trace.IdempotencyKey, 500, ""); err != nil {
			t.Fatalf("a withdrawal under the losing ticket's id: %v", err)
		}
		again, err := buyer.k.Run(ctx, req)
		if err != nil || again.Charge == nil || *again.Charge != 3 {
			t.Errorf("after a withdrawal took the ticket's id, the replay reported charge %s (%v), want 3", chargeOf(again), err)
		}
		return
	}
	t.Fatal("no draw lost in 30 calls against a face value of 1000")
}

// chargeOf renders a reply's charge for a failure message: the amount, or "none" when absent.
func chargeOf(r *kernel.CallReply) string {
	if r == nil || r.Charge == nil {
		return "none"
	}
	return strconv.FormatInt(*r.Charge, 10)
}

// A buyer drawing for more than the seller accepts does not trade with it, and finds that out at
// once. The two kernels agree on nothing here except the protocol: the seller's maximum is its own
// configuration, the buyer's ticket is its own, and neither can see the other's. What must not
// happen is the refusal arriving as silence — the condition is identical on every retry, so a call
// parked against it would hold the caller's funds for a day to reach the same answer (P4, P10).
func TestSimOversizedTicketIsRefusedAtOnce(t *testing.T) {
	net := newSimNet(t)

	sellCfg := defaultSimConfig()
	sellCfg.LotteryMax = 100
	buyCfg := defaultSimConfig()
	buyCfg.Lottery, buyCfg.LotteryMax = 200, 200

	seller := net.addNode("seller", sellCfg)
	buyer := net.addNode("buyer", buyCfg)

	cara := seller.user(t, "cara", sellerCapital)
	seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 5000)

	before := buyer.balance(t, dan.ID)
	_, err := buyer.run(t, dan.ID, remoteRef(seller, "cara", "quote"))
	if err == nil {
		net.dump()
		t.Fatal("a ticket the seller does not accept was served")
	}
	if errors.Is(err, kernel.ErrTimeout) {
		net.dump()
		t.Fatalf("the call parked on a refusal that can never change: %v", err)
	}
	if got := buyer.pendingRemote(t); got != 0 {
		net.dump()
		t.Errorf("the refused call left %d parked traces; want 0", got)
	}
	if got := buyer.balance(t, dan.ID); got != before {
		net.dump()
		t.Errorf("the caller was left %d, want its %d back in full", got, before)
	}
	if got := seller.owedBy(t, buyer.key); got != nil && got.Obligation != 0 {
		net.dump()
		t.Errorf("the seller is owed %d for work it refused to do", got.Obligation)
	}
	// A ticket the seller does accept trades normally, so the refusal is the maximum and not the
	// pair of kernels failing to talk at all.
	buyer2 := net.addNode("buyer2", defaultSimConfig())
	eve := buyer2.user(t, "eve", 5000)
	if _, err := buyer2.run(t, eve.ID, remoteRef(seller, "cara", "quote")); err != nil {
		net.dump()
		t.Fatalf("a ticket inside the seller's maximum: %v", err)
	}
}

// paidFor is what this kernel actually sent for one obligation, read from the payment row the
// settlement created. Nothing means the draw lost and no payment was ever made.
func (s *simNode) paidFor(t *testing.T, id string) int64 {
	t.Helper()
	row, err := s.db.ReadRailTransfer(context.Background(), id)
	if err != nil || row == nil {
		return 0
	}
	return row.Amount
}

// exposure is what this kernel has delivered to foreign buyers and not been paid for.
func (s *simNode) exposure(t *testing.T) int64 {
	t.Helper()
	e, err := s.db.Exposure(context.Background())
	if err != nil {
		t.Fatalf("%s: read exposure: %v", s.name, err)
	}
	return e
}

// ---------------------------------------------------------------------------
// Scenario: a transfer to a user of another kernel (P11)
// ---------------------------------------------------------------------------

// transfer runs this kernel's own transfer for the caller: the action named is the one that runs,
// wherever the recipient is (D18).
func (s *simNode) transfer(t *testing.T, callerID, target string, amount int64, key string) (*kernel.CallReply, error) {
	t.Helper()
	return s.k.Run(context.Background(), kernel.RunRequest{
		CallerID: callerID, ActionRef: "sys@" + testOwnName + "/transfer",
		Args: map[string]any{"target": target, "amount": float64(amount)}, ExternalKey: key,
	})
}

// settleTransfers runs the sender's worker once: the rail pays, then the beneficiary's kernel is
// told whom the payment is for (P11).
func (s *simNode) settleTransfers() {
	s.k.RailPass(context.Background())
	s.k.RevealPending(context.Background())
}

// A transfer to bob@B moves exactly the amount: alice pays it and the price of A's own action, B
// credits bob the amount once the payment arrives and not before, and B's own money and exposure
// never move, because B sold nothing. Lost and repeated announcements, a restart between payment
// and announcement, and a recipient B does not have change none of that.
func TestSimTransferAcrossKernels(t *testing.T) {
	const amount = 300
	for _, c := range []struct {
		name  string
		price int64
		fault simFault
	}{
		{"free, clean network", 0, faultNone},
		{"priced, clean network", 7, faultNone},
		{"announcement delivered twice", 0, faultDeliverTwice},
		{"announcement acknowledged but the answer lost", 0, faultLoseResponse},
		{"announcement refused before dispatch", 0, faultRefuseBeforeDispatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			net := newSimNet(t)
			a := net.addNode("a", defaultSimConfig())
			b := net.addNode("b", defaultSimConfig())
			ctx := context.Background()
			if c.price > 0 {
				if err := ensureSysNative(ctx, a.k, "sys", native.Transfer(), c.price); err != nil {
					t.Fatal(err)
				}
			}
			alice := a.user(t, "alice", 1000)
			bob := b.user(t, "bob", 0)
			bSys, bExposure := b.balance(t, b.sysID), b.exposure(t)

			reply, err := a.transfer(t, alice.ID, "bob@"+b.key, amount, "")
			if err != nil {
				net.dump()
				t.Fatalf("transfer: %v", err)
			}
			if got := a.balance(t, alice.ID); got != 1000-amount-c.price {
				t.Errorf("alice has %d, want %d: the amount and A's price, nothing else", got, 1000-amount-c.price)
			}
			row, _ := a.db.ReadRailTransfer(ctx, reply.TxID)
			if row == nil || row.Kind != kernel.RailKindTransfer || row.Amount != amount {
				t.Fatalf("A's payment for the transfer = %+v, want one transfer of exactly %d", row, amount)
			}
			if got := b.balance(t, bob.ID); got != 0 {
				t.Errorf("bob was credited %d before any payment", got)
			}

			if c.fault != faultNone {
				net.arm(a.key, b.key, verbTransfer, c.fault, 1)
			}
			a.settleTransfers()
			if c.fault != faultNone {
				net.assertFired(t, c.fault, 1)
			}
			if c.fault == faultLoseResponse || c.fault == faultRefuseBeforeDispatch {
				// Unacknowledged, so it is sent again on the next pass (P11).
				a.restart(t)
				a.settleTransfers()
			}
			if got := b.balance(t, bob.ID); got != amount {
				net.dump()
				t.Errorf("bob has %d, want exactly %d", got, amount)
			}
			if got := b.balance(t, b.sysID); got != bSys {
				t.Errorf("B's sys moved from %d to %d", bSys, got)
			}
			if got := b.exposure(t); got != bExposure {
				t.Errorf("B's exposure moved from %d to %d: B delivered nothing", bExposure, got)
			}
			// Nothing further moves on later passes.
			a.settleTransfers()
			if got := b.balance(t, bob.ID); got != amount {
				t.Errorf("a later pass credited bob again: %d", got)
			}
			if v, err := a.k.ReadTransaction(ctx, alice.ID, reply.TxID); err != nil || v.Payment == "" {
				t.Errorf("tx show names no payment: %+v, %v", v, err)
			}
		})
	}
}

// A recipient B does not have is refused before anything moves on A, and a retried transfer under
// one external key is paid once.
func TestSimTransferRefusalAndReplay(t *testing.T) {
	net := newSimNet(t)
	a := net.addNode("a", defaultSimConfig())
	b := net.addNode("b", defaultSimConfig())
	ctx := context.Background()
	alice := a.user(t, "alice", 1000)
	bob := b.user(t, "bob", 0)

	if _, err := a.transfer(t, alice.ID, "nobody@"+b.key, 10, ""); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("transfer to nobody: want ErrNotFound, got %v", err)
	}
	if u, _ := a.db.ReadUser(ctx, alice.ID); u.Available != 1000 || u.Locked != 0 {
		t.Errorf("a refused transfer left alice %d available, %d locked", u.Available, u.Locked)
	}

	// A kernel asking for bob must prove the vault it names, or it is refused; one naming none, on a
	// world without addresses, is answered with bob's id (P11).
	if r := b.h.OnResolve(ctx, a.key, fed.ResolveRequest{Kind: "user", User: "bob", BlockchainAddress: "0xforged", BlockchainProof: "x"}); r.Status != http.StatusForbidden {
		t.Errorf("an unproven vault on a user resolve: status %d, want 403", r.Status)
	}
	if r := b.h.OnResolve(ctx, a.key, fed.ResolveRequest{Kind: "user", User: "bob"}); r.Status != http.StatusOK || !strings.Contains(string(r.Body), bob.ID) {
		t.Errorf("a user resolve: %d %s, want bob's id", r.Status, r.Body)
	}

	first, err := a.transfer(t, alice.ID, "bob@"+b.key, 40, "pay-bob-1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.transfer(t, alice.ID, "bob@"+b.key, 40, "pay-bob-1")
	if err != nil || again.TxID != first.TxID {
		t.Fatalf("a repeat under the same key: %+v, %v; want the first run's transaction %s", again, err, first.TxID)
	}
	if _, err := a.transfer(t, alice.ID, "bob@"+b.key, 41, "pay-bob-1"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("other terms under a used key: want ErrInvalidInput, got %v", err)
	}
	a.settleTransfers()
	if got := a.balance(t, alice.ID); got != 960 {
		t.Errorf("alice has %d, want 960: one payment", got)
	}
	if got := b.balance(t, bob.ID); got != 40 {
		t.Errorf("bob has %d, want 40", got)
	}
}

// A run under an external key whose first attempt is still parked is answered "in flight", not run
// again; once it settles, the repeat is answered with its outcome and charges nothing more (D20).
func TestSimRunKeyWhileTheFirstRunIsParked(t *testing.T) {
	net := newSimNet(t)
	seller := net.addNode("seller", defaultSimConfig())
	buyer := net.addNode("buyer", defaultSimConfig())
	cara := seller.user(t, "cara", sellerCapital)
	act := seller.publish(t, cara, "quote", 25)
	dan := buyer.user(t, "dan", 1000)
	ctx := context.Background()
	keyed := func() (*kernel.CallReply, error) {
		return buyer.k.Run(ctx, kernel.RunRequest{CallerID: dan.ID, ActionRef: remoteRef(seller, "cara", "quote"),
			Args: map[string]any{}, ExternalKey: "quote-1"})
	}
	if _, err := buyer.run(t, dan.ID, remoteRef(seller, "cara", "quote")); err != nil {
		t.Fatalf("warm-up run: %v", err)
	}
	net.arm(buyer.key, seller.key, verbCall, faultLoseResponse, 1)
	if _, err := keyed(); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("the parked run: %v, want a timeout", err)
	}
	parked := buyer.balance(t, dan.ID)
	if _, err := keyed(); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("a repeat while parked: %v, want ErrInvalidState", err)
	}
	if got := buyer.balance(t, dan.ID); got != parked {
		t.Errorf("a repeat while parked moved money: %d, was %d", got, parked)
	}
	buyer.k.RetryPendingRemoteDispatches(ctx)
	settled := buyer.balance(t, dan.ID)
	reply, err := keyed()
	if err != nil || reply.TxID == "" {
		t.Fatalf("a repeat after settlement: %+v %v, want the first run's outcome", reply, err)
	}
	if n := seller.txCount(t, act.ID); n != 2 {
		t.Errorf("seller executed %d times, want 2", n)
	}
	if got := buyer.balance(t, dan.ID); got != settled {
		t.Errorf("the repeat after settlement moved money: %d, was %d", got, settled)
	}
}

// ---------------------------------------------------------------------------
// Scenario: a task reaches the party it is addressed to (U41, P8)
// ---------------------------------------------------------------------------

// messageTo parks a sys/message task on this kernel, sent by sender to the address to, and returns
// its id. The natives are installed as boot installs them.
func (s *simNode) messageTo(t *testing.T, senderID, to string) string {
	t.Helper()
	ctx := context.Background()
	native.Register(s.k, []native.Spec{native.Message(), native.Sink()})
	for _, spec := range []native.Spec{native.Message(), native.Sink()} {
		if err := ensureSysNative(ctx, s.k, "sys", spec, 0); err != nil {
			t.Fatalf("%s: install %s: %v", s.name, spec.Name, err)
		}
	}
	reply, err := s.k.Run(ctx, kernel.RunRequest{CallerID: senderID, ActionRef: "sys@" + testOwnName + "/message",
		Args: map[string]any{"to": to, "message": "hello"}})
	if err != nil {
		t.Fatalf("%s: message %s: %v", s.name, to, err)
	}
	return reply.Result["task_id"].(string)
}

// TestSimTaskReachesItsRecipient is U41 end to end through two real kernels: a task parked on one
// kernel for a user of another appears in that user's own task list, naming nothing but their
// login, and is completed or declined there exactly as a local one — the reply updating the
// entry before the command returns. A second completion is refused and changes nothing.
func TestSimTaskReachesItsRecipient(t *testing.T) {
	net := newSimNet(t)
	holder := net.addNode("holder", defaultSimConfig())
	home := net.addNode("home", defaultSimConfig())
	ana := holder.user(t, "ana", 0)
	alice := home.user(t, "alice", 0)
	ctx := context.Background()

	first := holder.messageTo(t, ana.ID, "alice@"+home.key)
	second := holder.messageTo(t, ana.ID, "alice@"+home.key)
	holder.k.RevealPending(ctx) // the telling worker's pass

	// Delivered, but withheld from alice until she approves ana, who is all she sees meanwhile (U52).
	if open, _ := home.k.ListTasks(ctx, alice.ID, kernel.TaskFilter{}); len(open) != 0 {
		t.Fatalf("alice sees %d tasks from a requester she never approved", len(open))
	}
	if rs, _ := home.k.ListRequesters(ctx, alice.ID); len(rs) != 1 || rs[0].Kernel != holder.key || rs[0].UserID != ana.ID || rs[0].Status != kernel.RequesterPending {
		t.Fatalf("alice's requesters = %+v; want ana on the holder, pending", rs)
	}
	if _, err := home.k.SetRequester(ctx, alice.ID, "ana@"+holder.key, kernel.RequesterApproved); err != nil {
		t.Fatal(err)
	}
	open, err := home.k.ListTasks(ctx, alice.ID, kernel.TaskFilter{})
	if err != nil || len(open) != 2 {
		net.dump()
		t.Fatalf("alice's own list: %d tasks, %v; want both delivered", len(open), err)
	}
	for _, e := range open {
		if e.HolderKey != holder.key || e.Status != kernel.TaskWaiting || e.ActionID != "" || e.CreatedByID != "" {
			t.Errorf("delivered entry %+v: want waiting, held by the holder, its actions withheld", e)
		}
	}

	if _, err := home.k.CompleteTask(ctx, alice.ID, first, json.RawMessage(`{}`)); err != nil {
		net.dump()
		t.Fatalf("complete: %v", err)
	}
	if e, _ := home.k.ReadTask(ctx, alice.ID, first); e.Status != kernel.TaskDone || e.TxID == "" || e.Outcome != kernel.TxSuccess {
		t.Errorf("after completing, alice's entry = %+v; want done with the holder's transaction", e)
	}
	if held, _ := holder.db.ReadTask(ctx, first); held.Status != kernel.TaskDone {
		t.Errorf("the holder's task is %q, want done", held.Status)
	}

	e, err := home.k.CancelTask(ctx, alice.ID, second)
	if err != nil || e.Status != kernel.TaskCancelled {
		t.Fatalf("decline: %+v %v; want the entry cancelled before the command returns", e, err)
	}
	if held, _ := holder.db.ReadTask(ctx, second); held.Status != kernel.TaskCancelled {
		t.Errorf("the holder's task is %q, want cancelled", held.Status)
	}
	// With both tasks answered, nothing holds ana's processes open: the decline closed its own.
	if open, _ := holder.db.ListProcesses(ctx, kernel.ProcessFilter{OwnerUserID: ana.ID}); len(open) != 0 {
		t.Errorf("%d of ana's processes stayed open after their tasks were answered", len(open))
	}

	if _, err := home.k.CompleteTask(ctx, alice.ID, first, json.RawMessage(`{}`)); err == nil {
		t.Error("a second completion was accepted")
	}
}

// TestSimATaskAcrossKernelsLendsNothingAndCanBeBlocked: a task held on one kernel for a user of
// another acts for its requester there (D6, U52). ana's task pays carol when alice answers it, from
// ana's balance reserved at creation, and alice, completing from her own kernel, pays nothing. When
// alice blocks ana, a task of ana's already delivered is declined on its holder, and so is one whose
// notice arrives after, the decline retried with the notice when it first fails. alice's decisions
// on ana use the principal her list holds and never ask ana's kernel.
func TestSimATaskAcrossKernelsLendsNothingAndCanBeBlocked(t *testing.T) {
	net := newSimNet(t)
	holder := net.addNode("holder", defaultSimConfig())
	home := net.addNode("home", defaultSimConfig())
	ana, carol := holder.user(t, "ana", 1000), holder.user(t, "carol", 0)
	alice := home.user(t, "alice", 200)
	ctx := context.Background()
	trace := setupTraceForProcess(t, holder.db, setupProcessHTTP(t, holder.db, ana.ID, 0).ID)
	pay := func(amount int) string {
		t.Helper()
		e, err := holder.k.CreateTaskByRef(ctx, trace, "sys@"+testOwnName+"/transfer", "alice@"+home.key, ana.ID,
			json.RawMessage(fmt.Sprintf(`{"target":"carol@%s","amount":%d}`, testOwnName, amount)))
		if err != nil {
			t.Fatalf("park a transfer task: %v", err)
		}
		return e.ID
	}
	holderStatus := func(id string) kernel.TaskStatus {
		t.Helper()
		task, err := holder.db.ReadTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return task.Status
	}

	paid := pay(300)
	holder.k.RevealPending(ctx)
	if _, err := home.k.SetRequester(ctx, alice.ID, "ana@"+holder.key, kernel.RequesterApproved); err != nil {
		t.Fatal(err)
	}
	if _, err := home.k.CompleteTask(ctx, alice.ID, paid, json.RawMessage(`{}`)); err != nil {
		net.dump()
		t.Fatalf("alice completes ana's transfer task: %v", err)
	}
	if got := holder.balance(t, carol.ID); got != 300 {
		t.Errorf("carol received %d, want 300", got)
	}
	if got := holder.balance(t, ana.ID); got != 700 {
		t.Errorf("ana holds %d, want 700: the value is hers", got)
	}
	if got := home.balance(t, alice.ID); got != 200 {
		t.Errorf("alice holds %d, want 200: answering costs nothing", got)
	}

	// A resolve from alice's kernel to ana's would be refused; no decision makes one.
	net.arm(home.key, holder.key, verbResolve, faultRefuseBeforeDispatch, 1)
	if _, err := home.k.SetRequester(ctx, alice.ID, "ana@"+holder.key, kernel.RequesterPending); err != nil {
		t.Fatalf("revoke without asking ana's kernel: %v", err)
	}
	delivered := pay(50)
	holder.k.RevealPending(ctx)
	if _, err := home.k.SetRequester(ctx, alice.ID, "ana@"+holder.key, kernel.RequesterBlocked); err != nil {
		t.Fatalf("block: %v", err)
	}
	if got := holderStatus(delivered); got != kernel.TaskCancelled {
		t.Errorf("blocking left ana's delivered task %q on its holder, want cancelled", got)
	}
	// The first decline of a task arriving from a blocked requester is lost; the notice stays owed,
	// and the holder's next telling carries the decline through.
	net.arm(home.key, holder.key, verbTask, faultRefuseBeforeDispatch, 1)
	later := pay(50)
	holder.k.RevealPending(ctx)
	net.assertFired(t, faultRefuseBeforeDispatch, 1) // the task decline's, and no resolve's
	if got := holderStatus(later); got != kernel.TaskWaiting {
		t.Fatalf("after the lost decline the task is %q on its holder, want still waiting", got)
	}
	holder.k.RevealPending(ctx)
	if got := holderStatus(later); got != kernel.TaskCancelled {
		t.Errorf("a task arriving from a blocked requester is %q on its holder, want cancelled once the notice is told again", got)
	}
	if got := holder.balance(t, ana.ID); got != 700 {
		t.Errorf("ana holds %d after both declines, want her values back, 700", got)
	}
}

// TestSimTaskNoticeIsToldUntilAcknowledged: a notice whose answer is lost is told again, and the
// repeat is acknowledged without a second entry; a holder this kernel has suspended delivers
// nothing until it is unsuspended (U37).
func TestSimTaskNoticeIsToldUntilAcknowledged(t *testing.T) {
	net := newSimNet(t)
	holder := net.addNode("holder", defaultSimConfig())
	home := net.addNode("home", defaultSimConfig())
	ana := holder.user(t, "ana", 0)
	alice := home.user(t, "alice", 0)
	ctx := context.Background()
	entries := func() int {
		t.Helper()
		got, err := home.k.ListTasks(ctx, alice.ID, kernel.TaskFilter{All: true})
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	owed := func() int {
		t.Helper()
		got, err := holder.db.ListUndeliveredTasks(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}

	if _, err := home.k.SetRequester(ctx, alice.ID, "ana@"+holder.key, kernel.RequesterApproved); err != nil {
		t.Fatal(err)
	}
	if err := home.k.SuspendKernel(ctx, home.sysID, holder.key); err != nil {
		t.Fatal(err)
	}
	holder.messageTo(t, ana.ID, "alice@"+home.key)
	holder.k.RevealPending(ctx)
	if entries() != 0 || owed() != 1 {
		t.Fatalf("a suspended holder delivered: %d entries, %d still owed", entries(), owed())
	}
	if err := home.k.UnsuspendKernel(ctx, home.sysID, holder.key); err != nil {
		t.Fatal(err)
	}

	net.arm(holder.key, home.key, verbTask, faultLoseResponse, 1)
	holder.k.RevealPending(ctx)
	net.assertFired(t, faultLoseResponse, 1)
	if entries() != 1 || owed() != 1 {
		t.Fatalf("after a lost acknowledgement: %d entries, %d owed; want delivered and still owed", entries(), owed())
	}
	holder.k.RevealPending(ctx)
	if entries() != 1 || owed() != 0 {
		t.Errorf("after the resend: %d entries, %d owed; want one entry, nothing owed", entries(), owed())
	}
}

// TestSimRestartDeliversTasksMadeBeforeTheMailbox: a waiting task with no mailbox entry — one made
// before the mailbox existed — is delivered when the kernel starts, and read like any other.
func TestSimRestartDeliversTasksMadeBeforeTheMailbox(t *testing.T) {
	net := newSimNet(t)
	node := net.addNode("solo", defaultSimConfig())
	ana := node.user(t, "ana", 0)
	bob := node.user(t, "bob", 0)
	ctx := context.Background()
	id := node.messageTo(t, ana.ID, "bob@"+testOwnName)
	if err := node.db.ExecForTest(ctx, `DELETE FROM mailbox`); err != nil {
		t.Fatal(err)
	}
	node.restart(t)
	if _, err := node.k.SetRequester(ctx, bob.ID, "ana@"+testOwnName, kernel.RequesterApproved); err != nil {
		t.Fatal(err)
	}
	if e, err := node.k.ReadTask(ctx, bob.ID, id); err != nil || e.Status != kernel.TaskWaiting {
		t.Errorf("after restart: %+v %v; want the waiting task delivered", e, err)
	}
}

// ---------------------------------------------------------------------------
// Scenario: a grant follows its grantor's run across kernels (D10)
// ---------------------------------------------------------------------------

// grantWorld is an upstream that knows its accounts by their tokens, and the actions that reach it:
// a vault that needs the caller's grant, a relay that calls another action for its caller, and a
// parker that leaves a task for one and returns. Relay and parker run as their kernel's own
// composition does, on the capability the kernel hands them.
type grantWorld struct {
	t        *testing.T
	upstream *httptest.Server
	nodes    map[string]*simNode
	mu       sync.Mutex
	accounts map[string]string // token → account
	seen     []string          // accounts the vault answered for, in order
	parked   map[string]string // node → the task its parker last left
}

func newGrantWorld(t *testing.T, nodes ...*simNode) *grantWorld {
	w := &grantWorld{t: t, nodes: map[string]*simNode{}, accounts: map[string]string{}, parked: map[string]string{}}
	for _, n := range nodes {
		w.nodes[n.name] = n
	}
	w.upstream = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		reply := map[string]any{}
		var err error
		if parts[0] == "vault" {
			w.mu.Lock()
			acct := w.accounts[r.Header.Get("X-Api-Key")]
			w.seen = append(w.seen, acct)
			w.mu.Unlock()
			if acct == "" {
				err = errors.New("unknown token")
			}
			reply["account"] = acct
		} else {
			node := w.nodes[parts[1]]
			target, _ := base64.RawURLEncoding.DecodeString(parts[2])
			trace, owner, verr := node.k.VerifyCapability(ctx, r.Header.Get(capabilityHeader))
			switch {
			case verr != nil:
				err = verr
			case parts[0] == "relay":
				// One target after another; the reply is the last one's.
				for _, ref := range strings.Split(string(target), "|") {
					var sub *kernel.CallReply
					if sub, err = node.k.Subcall(ctx, kernel.SubcallRequest{CallerID: owner, ParentTraceID: trace, ActionRef: ref, Args: map[string]any{}}); err != nil {
						break
					}
					reply = sub.Result
				}
			default: // park
				me, _ := node.k.ReadUser(ctx, owner)
				var e *kernel.TaskEntry
				if e, err = node.k.CreateTaskByRef(ctx, trace, string(target), me.Handle+"@"+testOwnName, owner, nil); err == nil {
					w.mu.Lock()
					w.parked[node.name] = e.ID
					w.mu.Unlock()
				}
			}
		}
		rw.Header().Set("Content-Type", "application/json")
		if err != nil {
			rw.WriteHeader(http.StatusBadGateway)
			t.Logf("upstream %s: %v", r.URL.Path, err)
			reply = map[string]any{"error": err.Error()}
		}
		_ = json.NewEncoder(rw).Encode(reply)
	}))
	t.Cleanup(w.upstream.Close)
	return w
}

// action publishes one of the world's actions on a node: kind "vault", or "relay"/"park" aimed at target.
func (w *grantWorld) action(node *simNode, owner *kernel.Account, name, kind, target string, price int64) *kernel.Action {
	w.t.Helper()
	ctx := context.Background()
	req := kernel.CreateActionRequest{OwnerUserID: owner.ID, Title: "Test action", Name: name, Kind: kernel.KindHTTP, Price: price,
		Source: w.upstream.URL + "/vault", Description: kind + " " + name,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}}
	if kind == "vault" {
		req.Auth = &kernel.AuthInput{Scheme: kernel.AuthSchemeDelegatedBearer, Config: map[string]any{"header": "X-Api-Key", "template": "{token}"}}
	} else {
		req.Source = w.upstream.URL + "/" + kind + "/" + node.name + "/" + base64.RawURLEncoding.EncodeToString([]byte(target))
	}
	a, err := node.k.CreateAction(ctx, owner.ID, req)
	if err != nil {
		w.t.Fatalf("%s: create %s: %v", node.name, name, err)
	}
	if err := node.k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		w.t.Fatalf("%s: enable %s: %v", node.name, name, err)
	}
	vis := kernel.VisibilityPublic
	if a, err = node.k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &vis}); err != nil {
		w.t.Fatalf("%s: publish %s: %v", node.name, name, err)
	}
	return a
}

// signUp connects a user to a selector with a token, by the one path a user has: their own kernel.
func (w *grantWorld) signUp(home *simNode, user *kernel.Account, serving *simNode, selector, token, account string) {
	w.t.Helper()
	w.mu.Lock()
	w.accounts[token] = account
	w.mu.Unlock()
	status, body, err := home.k.ForwardGrant(context.Background(), user.ID, serving.key, kernel.GrantOp{Kind: "token", Selector: selector, Token: token})
	if err != nil || status != http.StatusOK {
		home.net.dump()
		w.t.Fatalf("sign %s up to %s: status %d body %v err %v", user.Handle, selector, status, body, err)
	}
}

// account runs ref as the user and returns the upstream account the run reached.
func (w *grantWorld) account(node *simNode, userID, ref string) (string, error) {
	reply, err := node.run(w.t, userID, ref)
	if err != nil {
		return "", err
	}
	acct, _ := reply.Result["account"].(string)
	return acct, nil
}

// TestSimGrantFollowsTheRunAcrossKernels is the requirement itself: once signed up, a user is
// recognised by an application on another kernel however the run reaches it — directly, or through
// an action hosted on the user's kernel, on the application's, or on a third — and never by anyone
// else's credential. Nothing but the hosting differs between the cases, and neither does the answer.
func TestSimGrantFollowsTheRunAcrossKernels(t *testing.T) {
	net := newSimNet(t)
	home, mid, app := net.addNode("home", defaultSimConfig()), net.addNode("mid", defaultSimConfig()), net.addNode("app", defaultSimConfig())
	w := newGrantWorld(t, home, mid, app)
	ctx := context.Background()

	carol, carl := app.user(t, "carol", sellerCapital), app.user(t, "carl", sellerCapital)
	vault := w.action(app, carol, "vault", "vault", "", 0)
	vaultRef := remoteRef(app, "carol", "vault")
	atapp := w.action(app, carl, "atapp", "relay", "carol@"+testOwnName+"/vault", 0)
	// The relay's own owner is signed up too: that credential must never serve anyone else.
	w.accounts["tok-carl"] = "carl"
	if _, err := app.k.AttachBearerGrants(ctx, kernel.User(carl.ID), "carol@"+testOwnName+"/vault", "", "tok-carl"); err != nil {
		t.Fatalf("carl signs up: %v", err)
	}
	w.action(mid, mid.user(t, "mia", sellerCapital), "atmid", "relay", vaultRef, 0)
	alice, dan := home.user(t, "alice", 1000), home.user(t, "dan", 1000)
	w.action(home, home.user(t, "bob", 1000), "athome", "relay", vaultRef, 0)
	// An application on the user's own kernel, reached through an action hosted elsewhere.
	hank := home.user(t, "hank", sellerCapital)
	w.action(home, hank, "hvault", "vault", "", 0)
	w.action(mid, mid.user(t, "max", sellerCapital), "homeward", "relay", remoteRef(home, "hank", "hvault"), 0)

	routes := map[string]string{
		"directly":                             vaultRef,
		"through an action at home":            "bob@" + testOwnName + "/athome",
		"through an action at the application": remoteRef(app, "carl", "atapp"),
		"through an action on a third kernel":  remoteRef(mid, "mia", "atmid"),
	}

	// Before sign-up the direct call is refused before anything moves, and the application's kernel
	// never hears of it; through any action the run fails and no credential is used.
	if _, err := w.account(home, alice.ID, vaultRef); !errors.Is(err, kernel.ErrGrantRequired) {
		t.Fatalf("direct call before sign-up: %v, want ErrGrantRequired", err)
	}
	if n := app.txCount(t, vault.ID); n != 0 || home.balance(t, alice.ID) != 1000 {
		t.Fatalf("a call refused for want of a grant left %d transactions at the application and balance %d", n, home.balance(t, alice.ID))
	}
	for name, ref := range routes {
		if acct, err := w.account(home, alice.ID, ref); err == nil {
			t.Errorf("before sign-up, %s reached account %q", name, acct)
		}
	}
	if len(w.seen) != 0 {
		t.Fatalf("the upstream was reached with %v before anyone signed up", w.seen)
	}

	w.signUp(home, alice, app, vaultRef, "tok-alice", "alice")
	w.accounts["tok-alice-home"] = "alice-at-home"
	if _, err := home.k.AttachBearerGrants(ctx, kernel.User(alice.ID), "hank@"+testOwnName+"/hvault", "", "tok-alice-home"); err != nil {
		t.Fatalf("alice signs up at home: %v", err)
	}
	routes["home again, through an action elsewhere"] = remoteRef(mid, "max", "homeward")

	for name, ref := range routes {
		want := "alice"
		if strings.HasPrefix(name, "home again") {
			want = "alice-at-home"
		}
		if acct, err := w.account(home, alice.ID, ref); err != nil || acct != want {
			net.dump()
			t.Errorf("%s: account %q, err %v; want %q", name, acct, err, want)
		}
		// A user who has not signed up is never served, by any route, with anybody's credential.
		if acct, err := w.account(home, dan.ID, ref); err == nil {
			t.Errorf("%s: a user with no grant reached account %q", name, acct)
		}
	}
	for _, acct := range w.seen {
		if acct != "alice" && acct != "alice-at-home" {
			t.Fatalf("the upstream answered for %q; only alice ran anything that may reach it (%v)", acct, w.seen)
		}
	}

	// A call that names no run is nobody's run: the action's own owner's credential does not serve it.
	used := len(w.seen)
	if reply, err := app.k.RunFederated(ctx, home.key, atapp, map[string]any{}, "", kernel.BuyerTerms{}); err == nil || len(w.seen) != used {
		t.Errorf("a call carrying no reference was served (%+v, %v) with a credential of the action owner's", reply, err)
	}

	// What the user sees is the grant, by its name here, and no credential.
	views, _ := home.k.ListGrantViews(ctx, alice.ID)
	listed, _ := json.Marshal(views)
	if !strings.Contains(string(listed), "carol@") || strings.Contains(string(listed), "tok-alice") {
		t.Errorf("alice's grants read %s: want the remote action named and no token", listed)
	}
	// And the application's kernel holds it under her principal, not under any local user.
	if _, err := app.db.ReadGrant(ctx, kernel.Principal{Kernel: home.key, UserID: alice.ID}, vault.ID); err != nil {
		t.Errorf("the application holds no grant for alice@home: %v", err)
	}
}

// TestSimWaitingWorkKeepsTheGrantUntilTheProcessEnds: an action on another kernel leaves a task
// waiting and returns. The user's process stays open for it, so the task still reaches the
// application as the user when it completes later — exactly as a task waiting at home would — and
// closes once that kernel says the work is done. Ending the process first ends the grant's use there.
func TestSimWaitingWorkKeepsTheGrantUntilTheProcessEnds(t *testing.T) {
	net := newSimNet(t)
	home, mid, app := net.addNode("home", defaultSimConfig()), net.addNode("mid", defaultSimConfig()), net.addNode("app", defaultSimConfig())
	w := newGrantWorld(t, home, mid, app)
	ctx := context.Background()

	w.action(app, app.user(t, "carol", sellerCapital), "vault", "vault", "", 0)
	vaultRef := remoteRef(app, "carol", "vault")
	mia := mid.user(t, "mia", sellerCapital)
	w.action(mid, mia, "later", "park", vaultRef, 0)
	alice, zoe := home.user(t, "alice", 1000), home.user(t, "zoe", 1000)
	w.signUp(home, alice, app, vaultRef, "tok-alice", "alice")

	park := func(user *kernel.Account) (processID, taskID string) {
		t.Helper()
		reply, err := home.run(t, user.ID, remoteRef(mid, "mia", "later"))
		if err != nil {
			net.dump()
			t.Fatalf("%s runs the parker: %v", user.Handle, err)
		}
		return reply.ProcessID, w.parked["mid"]
	}
	status := func(processID string) kernel.ProcessStatus {
		p, err := home.db.ReadProcess(ctx, processID)
		if err != nil {
			t.Fatal(err)
		}
		return p.Status
	}
	complete := func(taskID string) (string, error) {
		reply, err := mid.k.CompleteTask(ctx, mia.ID, taskID, json.RawMessage(`{}`))
		if err != nil {
			return "", err
		}
		acct, _ := reply.Result["account"].(string)
		return acct, nil
	}

	// The call returned, and the process is open all the same.
	pid, task := park(alice)
	if status(pid) != kernel.ProcessOpen {
		t.Fatalf("alice's process closed while the work it began waits on another kernel")
	}
	// Whose run it is, is told only to a kernel she holds a grant on: the one that merely holds her
	// work learns nothing by asking, though it carries the same reference.
	p, _ := home.db.ReadProcess(ctx, pid)
	if resp := home.h.OnProcess(ctx, mid.key, fed.ProcessRequest{Kind: "check", Ref: p.Ref}); resp.Status == http.StatusOK {
		t.Errorf("a kernel alice holds no grant on was told whose run it serves: %s", resp.Body)
	}
	if resp := home.h.OnProcess(ctx, app.key, fed.ProcessRequest{Kind: "check", Ref: p.Ref}); !strings.Contains(string(resp.Body), alice.ID) {
		t.Errorf("the application was not told whose run it serves: %d %s", resp.Status, resp.Body)
	}
	if acct, err := complete(task); err != nil || acct != "alice" {
		net.dump()
		t.Fatalf("the task completed later reached %q, err %v; want alice", acct, err)
	}
	mid.k.ReleaseClosedProcesses(ctx)
	if status(pid) != kernel.ProcessClosed {
		t.Errorf("alice's process is still open after the kernel holding its work said it was done")
	}
	// Told again, nothing reopens and nothing breaks.
	mid.k.ReleaseClosedProcesses(ctx)

	// Ending the process ends what it may do everywhere: the waiting work no longer acts as her.
	pid, task = park(alice)
	if err := home.k.EndProcess(ctx, alice.ID, pid); err != nil {
		t.Fatalf("end process: %v", err)
	}
	before := len(w.seen)
	if acct, err := complete(task); err == nil {
		t.Errorf("after alice ended her process, its waiting work still reached %q", acct)
	}
	if len(w.seen) != before {
		t.Errorf("the upstream was reached after the process ended")
	}

	// Signing up while the work waits is enough: consent is read when it is used, not when the run began.
	_, task = park(zoe)
	w.signUp(home, zoe, app, vaultRef, "tok-zoe", "zoe")
	if acct, err := complete(task); err != nil || acct != "zoe" {
		net.dump()
		t.Errorf("a user who signed up while work waited reached %q, err %v; want zoe", acct, err)
	}
}

// TestSimOutcomeWaitsForItsRegistration: the answer to a call that left work waiting is given out
// only once the kernel the run began on holds that work registered — the first time and on every
// retry — so there is no moment at which that kernel's process may close under it (D2).
func TestSimOutcomeWaitsForItsRegistration(t *testing.T) {
	net := newSimNet(t)
	home, mid, app := net.addNode("home", defaultSimConfig()), net.addNode("mid", defaultSimConfig()), net.addNode("app", defaultSimConfig())
	w := newGrantWorld(t, home, mid, app)
	ctx := context.Background()
	w.action(app, app.user(t, "carol", sellerCapital), "vault", "vault", "", 0)
	w.action(mid, mid.user(t, "mia", sellerCapital), "later", "park", remoteRef(app, "carol", "vault"), 0)
	alice := home.user(t, "alice", 1000)
	ref := remoteRef(mid, "mia", "later")
	// Warm the proxy, so what the cut meets is the registration and nothing before it.
	if _, err := home.run(t, alice.ID, ref); err != nil {
		t.Fatalf("warm: %v", err)
	}

	net.cut(mid.key, home.key)
	if _, err := home.run(t, alice.ID, ref); err == nil {
		t.Fatalf("the call returned an outcome while its work could not be registered")
	}
	if n := home.pendingRemote(t); n != 1 {
		t.Fatalf("home holds %d calls in doubt, want the 1 whose answer is withheld", n)
	}
	// Asked again while still cut off, the answer is still withheld: a retry does not get around it.
	home.k.RetryPendingRemoteDispatches(ctx)
	if n := home.pendingRemote(t); n != 1 {
		t.Fatalf("a retry was answered before the registration: %d calls in doubt", n)
	}
	net.blocked = map[string]map[string]bool{}
	home.k.RetryPendingRemoteDispatches(ctx)
	if n := home.pendingRemote(t); n != 0 {
		net.dump()
		t.Fatalf("the call did not settle once the registration could be made: %d in doubt", n)
	}
	var open int
	if err := home.db.QueryRowForTest(ctx, `SELECT COUNT(*) FROM processes p JOIN registrations r ON r.process_id=p.id
		 WHERE p.status='open' AND r.released=0 AND r.kernel=?`, mid.key, &open); err != nil || open != 2 {
		t.Errorf("home holds %d open processes registered for mid (err %v), want 2: the warm run's and the withheld one's", open, err)
	}
}

// TestSimDisconnectAndStaleGrants: a disconnect takes effect on the user's own kernel at once,
// whether or not the other kernel can be told; an operation that arrives late cannot undo a later
// one; and when the two kernels disagree the serving one is right, at no cost to anyone.
func TestSimDisconnectAndStaleGrants(t *testing.T) {
	net := newSimNet(t)
	home, app := net.addNode("home", defaultSimConfig()), net.addNode("app", defaultSimConfig())
	w := newGrantWorld(t, home, app)
	ctx := context.Background()
	carol := app.user(t, "carol", sellerCapital)
	vault := w.action(app, carol, "vault", "vault", "", 20)
	ref := remoteRef(app, "carol", "vault")
	alice := home.user(t, "alice", 1000)
	principal := kernel.Principal{Kernel: home.key, UserID: alice.ID, Handle: "alice"}
	disconnect := func(op kernel.GrantOp) error {
		op.Kind = "disconnect"
		status, body, err := home.k.ForwardGrant(ctx, alice.ID, app.key, op)
		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("status %d: %v", status, body)
		}
		return err
	}
	refusedHere := func(when string) {
		t.Helper()
		calls, funds := app.txCount(t, vault.ID), home.balance(t, alice.ID)
		if _, err := w.account(home, alice.ID, ref); !errors.Is(err, kernel.ErrGrantRequired) {
			t.Errorf("%s: run gave %v, want ErrGrantRequired", when, err)
		}
		if app.txCount(t, vault.ID) != calls || home.balance(t, alice.ID) != funds {
			t.Errorf("%s: the refusal reached the application or moved money", when)
		}
	}

	w.signUp(home, alice, app, ref, "tok-alice", "alice")
	if acct, err := w.account(home, alice.ID, ref); err != nil || acct != "alice" {
		t.Fatalf("signed up: %q, %v", acct, err)
	}
	if err := disconnect(kernel.GrantOp{Selector: ref}); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	refusedHere("after a disconnect by selector")
	if _, err := app.db.ReadGrant(ctx, principal, vault.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("the application still holds the grant: %v", err)
	}

	// Cut off from the application, the disconnect still holds here, and says it must be sent again.
	w.signUp(home, alice, app, ref, "tok-alice", "alice")
	net.cut(home.key, app.key)
	if err := disconnect(kernel.GrantOp{Selector: ref}); !errors.Is(err, kernel.ErrPeerUnreachable) {
		t.Fatalf("disconnect while cut off: %v, want ErrPeerUnreachable", err)
	}
	net.blocked = map[string]map[string]bool{}
	refusedHere("after a disconnect the application never heard")
	if err := disconnect(kernel.GrantOp{Selector: ref}); err != nil {
		t.Fatalf("disconnect, sent again: %v", err)
	}

	// The whole account, named where it is held; its credential goes with it.
	w.signUp(home, alice, app, ref, "tok-alice", "alice")
	conns, _ := app.db.ListConnectionsByUser(ctx, principal)
	if len(conns) != 1 {
		t.Fatalf("the application holds %d connections for alice, want 1", len(conns))
	}
	if err := disconnect(kernel.GrantOp{Account: conns[0].ProviderKey}); err != nil {
		t.Fatalf("disconnect the account: %v", err)
	}
	refusedHere("after disconnecting the account")
	if conns, _ = app.db.ListConnectionsByUser(ctx, principal); len(conns) != 0 {
		t.Errorf("the credential outlived its account's disconnect")
	}

	// An operation older than one already applied is refused, however validly it is signed: a
	// disconnect delayed on the way cannot delete the sign-up that followed it.
	w.signUp(home, alice, app, ref, "tok-alice", "alice")
	late := kernel.GrantOp{Kind: "disconnect", Selector: ref, Counterparty: home.key, Recipient: app.key,
		Timestamp: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), ForUserID: alice.ID, ForHandle: "alice"}
	sig, _ := testNet.SignGrantOp(home.priv, late)
	raw, _ := json.Marshal(late)
	if resp := app.h.OnGrant(ctx, home.key, fed.GrantRequest{Counterparty: home.key, Signature: sig, Op: raw}); resp.Status == http.StatusOK {
		t.Fatalf("a disconnect older than the sign-up was applied: %s", resp.Body)
	}
	if acct, err := w.account(home, alice.ID, ref); err != nil || acct != "alice" {
		t.Errorf("after the late disconnect was refused: %q, %v; want alice", acct, err)
	}

	// The application drops the grant without this kernel hearing. The next call is refused there,
	// signed, at no charge; the action is as cached, so its proxy stays; and this kernel's record
	// goes, so the call after that is refused here.
	if _, err := app.k.RevokeGrantsBySelector(ctx, principal, "carol@"+testOwnName+"/vault"); err != nil {
		t.Fatalf("revoke at the application: %v", err)
	}
	funds := home.balance(t, alice.ID)
	proxy, _ := home.k.ResolveAction(ctx, ref)
	if _, err := w.account(home, alice.ID, ref); !errors.Is(err, kernel.ErrGrantRequired) {
		net.dump()
		t.Fatalf("call on a grant the application dropped: %v, want ErrGrantRequired", err)
	}
	if got := home.balance(t, alice.ID); got != funds {
		t.Errorf("the refusal cost %d", funds-got)
	}
	// Read as stored, not resolved afresh: resolving would mend a proxy the refusal had retired.
	if stored, err := home.db.ReadAction(ctx, proxy.ID); err != nil || !stored.Active {
		t.Errorf("the proxy did not survive a missing grant: %+v, %v", stored, err)
	}
	if views, _ := home.k.ListGrantViews(ctx, alice.ID); len(views) != 0 {
		t.Errorf("alice still lists %d grants the application no longer holds", len(views))
	}
	refusedHere("once this kernel's record is gone")
	w.signUp(home, alice, app, ref, "tok-alice", "alice")
	if acct, err := w.account(home, alice.ID, ref); err != nil || acct != "alice" {
		t.Errorf("signed up again: %q, %v", acct, err)
	}
}

// A forwarded grant operation is admitted only as its kernel signed it, for this kernel, now, for a
// named user, on that kernel's own connection — and never from a kernel this one has suspended.
func TestSimGrantOperationsAreAdmittedOnlyAsSigned(t *testing.T) {
	net := newSimNet(t)
	home, app := net.addNode("home", defaultSimConfig()), net.addNode("app", defaultSimConfig())
	w := newGrantWorld(t, home, app)
	ctx := context.Background()
	w.action(app, app.user(t, "carol", sellerCapital), "vault", "vault", "", 0)
	alice := home.user(t, "alice", 0)
	stranger := net.addNode("stranger", defaultSimConfig())

	send := func(change func(op *kernel.GrantOp, signer **simNode, conn *string), after func(op *kernel.GrantOp)) int {
		op := kernel.GrantOp{Kind: "plan", Selector: "carol@" + testOwnName + "/vault", Counterparty: home.key, Recipient: app.key,
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano), ForUserID: alice.ID, ForHandle: "alice"}
		signer, conn := home, home.key
		if change != nil {
			change(&op, &signer, &conn)
		}
		sig, _ := testNet.SignGrantOp(signer.priv, op)
		if after != nil {
			after(&op)
		}
		raw, _ := json.Marshal(op)
		return app.h.OnGrant(ctx, conn, fed.GrantRequest{Counterparty: op.Counterparty, Signature: sig, Op: raw}).Status
	}
	if got := send(nil, nil); got != http.StatusOK {
		t.Fatalf("an operation signed as it should be: status %d", got)
	}
	for name, tc := range map[string]struct {
		change func(op *kernel.GrantOp, signer **simNode, conn *string)
		after  func(op *kernel.GrantOp)
	}{
		"addressed to another kernel": {change: func(op *kernel.GrantOp, _ **simNode, _ *string) { op.Recipient = stranger.key }},
		"signed long ago": {change: func(op *kernel.GrantOp, _ **simNode, _ *string) {
			op.Timestamp = time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
		}},
		"signed by another kernel":       {change: func(_ *kernel.GrantOp, signer **simNode, _ *string) { *signer = stranger }},
		"changed after it was signed":    {after: func(op *kernel.GrantOp) { op.Selector = "carol@" + testOwnName }},
		"naming a user by id alone":      {change: func(op *kernel.GrantOp, _ **simNode, _ *string) { op.ForHandle = "" }},
		"naming no user":                 {change: func(op *kernel.GrantOp, _ **simNode, _ *string) { op.ForUserID = "" }},
		"arriving on another connection": {change: func(_ *kernel.GrantOp, _ **simNode, conn *string) { *conn = stranger.key }},
		"of a kind nobody defined":       {change: func(op *kernel.GrantOp, _ **simNode, _ *string) { op.Kind = "seize" }},
	} {
		if got := send(tc.change, tc.after); got == http.StatusOK {
			t.Errorf("an operation %s was admitted", name)
		}
	}
	if err := app.k.SuspendKernel(ctx, app.sysID, home.key); err != nil {
		t.Fatal(err)
	}
	if got := send(nil, nil); got == http.StatusOK {
		t.Errorf("an operation from a suspended kernel was admitted")
	}
}

// When the kernel a run began on cannot be asked, or is one this kernel has suspended, there is no
// grantor: the call is refused, nothing is charged and no credential is used. Not being reachable
// is not a revocation — the same call works once it can be asked again.
func TestSimNoGrantorWithoutTheOriginsWord(t *testing.T) {
	net := newSimNet(t)
	home, mid, app := net.addNode("home", defaultSimConfig()), net.addNode("mid", defaultSimConfig()), net.addNode("app", defaultSimConfig())
	w := newGrantWorld(t, home, mid, app)
	ctx := context.Background()
	w.action(app, app.user(t, "carol", sellerCapital), "vault", "vault", "", 20)
	ref := remoteRef(app, "carol", "vault")
	w.action(mid, mid.user(t, "mia", sellerCapital), "atmid", "relay", ref, 0)
	alice := home.user(t, "alice", 1000)
	w.signUp(home, alice, app, ref, "tok-alice", "alice")
	if acct, err := w.account(home, alice.ID, ref); err != nil || acct != "alice" {
		t.Fatalf("signed up: %q, %v", acct, err)
	}
	funds, used := home.balance(t, alice.ID), len(w.seen)

	net.cut(app.key, home.key)
	if acct, err := w.account(home, alice.ID, ref); err == nil {
		t.Errorf("served as %q while the kernel the run began on could not be asked", acct)
	}
	net.blocked = map[string]map[string]bool{}
	if home.balance(t, alice.ID) != funds || len(w.seen) != used {
		t.Errorf("a call refused for want of an answer cost %d or used a credential", funds-home.balance(t, alice.ID))
	}
	if acct, err := w.account(home, alice.ID, ref); err != nil || acct != "alice" {
		net.dump()
		t.Errorf("once it could be asked again: %q, %v; want alice", acct, err)
	}

	used = len(w.seen)
	if err := app.k.SuspendKernel(ctx, app.sysID, home.key); err != nil {
		t.Fatal(err)
	}
	if acct, err := w.account(home, alice.ID, remoteRef(mid, "mia", "atmid")); err == nil || len(w.seen) != used {
		t.Errorf("a run begun on a suspended kernel was served as %q through a kernel that is not", acct)
	}
}

// A missing grant met inside an action is an ordinary failure of that action (U13): the work it had
// already bought stays bought and paid, the caller pays for that and no more, and nothing but the
// grant stands between the same run and success.
func TestSimMissingGrantInsideAnActionKeepsDeliveredWorkPaid(t *testing.T) {
	net := newSimNet(t)
	home, mid, app := net.addNode("home", defaultSimConfig()), net.addNode("mid", defaultSimConfig()), net.addNode("app", defaultSimConfig())
	w := newGrantWorld(t, home, mid, app)
	w.action(app, app.user(t, "carol", sellerCapital), "vault", "vault", "", 0)
	vaultRef := remoteRef(app, "carol", "vault")
	leaf := mid.publish(t, mid.user(t, "lea", sellerCapital), "leaf", 7)
	w.action(mid, mid.user(t, "mia", sellerCapital), "pair", "relay", "lea@"+testOwnName+"/leaf|"+vaultRef, 100)
	alice := home.user(t, "alice", 1000)
	pair := remoteRef(mid, "mia", "pair")

	if acct, err := w.account(home, alice.ID, pair); err == nil {
		t.Fatalf("with no grant the pair reached account %q", acct)
	}
	paid := 1000 - home.balance(t, alice.ID)
	if n := mid.txCount(t, leaf.ID); n != 1 || paid <= 0 {
		net.dump()
		t.Fatalf("the leaf settled %d times and the caller paid %d: delivered work must stay settled and paid for", n, paid)
	}
	w.signUp(home, alice, app, vaultRef, "tok-alice", "alice")
	if acct, err := w.account(home, alice.ID, pair); err != nil || acct != "alice" {
		net.dump()
		t.Fatalf("with the grant: %q, %v; want alice", acct, err)
	}
	if whole := 1000 - paid - home.balance(t, alice.ID); whole <= paid {
		t.Errorf("the failed run cost %d and the whole one %d: a failure must cost only what was delivered", paid, whole)
	}
}
