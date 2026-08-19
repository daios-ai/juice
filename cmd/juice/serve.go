package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
)

func init() {
	var addr string
	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the server",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runServer(addr)
		},
	}
	serveCmd.Flags().StringVar(&addr, "addr", ":4040", "Listen address")
	rootCmd.AddCommand(serveCmd)

	rootCmd.AddCommand(healthCmd())
}

func runServer(addr string) error {
	k, db, logger, httpExec, fedAdapter, specs, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()

	if err := bootstrap(k, globalCfg.Native, specs); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	// Prune natives the build no longer ships (e.g. after a native action is removed): a
	// kind=native row with no registered handler is soft-deleted so it stops being listed and
	// callable on an existing database. Non-fatal — a leftover orphan is not corruption.
	if pruned, err := k.PruneOrphanedNativeActions(context.Background()); err != nil {
		logger.Warn("native.prune_failed", "error", err.Error())
	} else if len(pruned) > 0 {
		logger.Info("native.pruned", "actions", strings.Join(pruned, ","))
	}
	if err := k.ValidateFeeRecipient(context.Background()); err != nil {
		return fmt.Errorf("startup: %w", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestIDMiddleware)
	r.Use(loggingMiddleware(logger))
	r.Use(maxBytesMiddleware) // request body limit (path-aware; see maxBytesMiddleware)

	srv := &server{kernel: k, log: logger}
	if httpExec.auth != nil {
		srv.oauth = newGrantBroker(httpExec.auth)
	}

	// Auth — rate limited: 5 requests/minute per IP, burst of 10.
	authLimiter := ipRateLimiter(5.0/60, 10)
	r.With(authLimiter).Post("/v1/auth/token", srv.postToken)
	r.With(authLimiter).Post("/v1/auth/authorize", srv.postAuthorize)
	r.With(authLimiter).Post("/v1/auth/refresh", srv.postRefresh)
	r.With(authLimiter).Post("/v1/auth/logout", srv.postLogout)
	r.With(authLimiter).Post("/v1/auth/recover/start", srv.postRecoverStart)
	r.With(authLimiter).Post("/v1/auth/recover/complete", srv.postRecoverComplete)

	// Users — rate limited: 3 requests/minute per IP, burst of 5.
	r.With(ipRateLimiter(3.0/60, 5)).Post("/v1/users", srv.postUser)

	registerRoutes(r, srv)

	// Bind explicitly so a bind failure is a real, immediate error, and so --addr host:0
	// (OS-assigned port) works: we then advertise the address we actually bound.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	// Callback base URL for capability composition (§9): configured value, else derived from the
	// bound port as a loopback URL — enough for the co-located (same-machine) endpoint.
	httpExec.callbackURL = globalCfg.HTTPCallbackURL
	if httpExec.callbackURL == "" {
		if _, port, perr := net.SplitHostPort(ln.Addr().String()); perr == nil {
			httpExec.callbackURL = "http://127.0.0.1:" + port
		}
	}
	// Start the federation transport (§13): peers addressed by key, no HTTP endpoints. The
	// libp2p identity is the platform signing key, so the transport IS this kernel's identity.
	fedTransport, ferr := startFedTransport(context.Background(), k, logger)
	// Every outbound contact records whether the peer answered (§13). Two integration points reach
	// all of it: the adapter below (calls, resolves, steps, settlement) and the discovery pass
	// (gossip, which holds the transport directly). `admin inspect` stays out — inspection writes
	// nothing (§14).
	recordContact := newContactRecorder(k.RecordKernelContact)
	if ferr != nil {
		logger.Error("fed.start_failed", "error", ferr)
	} else {
		srv.fed = fedTransport
		fedAdapter.SetTransport(fedTransport)
		fedAdapter.SetContactRecorder(recordContact)
		pub, _ := k.GetConfig(context.Background(), configKeySigningPublic)
		fedAdapter.SetLocalPubKey(pub)
		defer fedTransport.Close()

		// Drive pending remote-proxy calls (§13). The worker drains the work that survived the last
		// shutdown first, then settles into the ordinary timer so a peer coming back online settles
		// parked calls without a restart and the RemotePendingMaxAge refund fires from the running
		// server. bootstrap's own pass runs before this transport exists, so it can only settle
		// max-age expiries — this drain is the first attempt that can actually reach a peer.
		// The snapshot is taken HERE, synchronously, before any HTTP request can create a new
		// trace, so the drain is exactly the pre-existing work and never a moving target.
		retryCtx, retryCancel := context.WithCancel(context.Background())
		defer retryCancel()
		pending, perr := k.PendingRemoteTraces(retryCtx)
		if perr != nil {
			logger.Warn("remote.retry.snapshot_failed", "error", perr.Error())
		}
		go startRemoteRetryLoop(retryCtx, pending, k.PendingRemoteTraces, k.RetryRemoteTrace, globalCfg.remoteRetryInterval())

		// Grow and refresh the known network (§13). One loop: each pass advertises this kernel to the
		// routing-discovery namespace, then pulls gossip from the union of the namespace's providers,
		// the configured bootstrap seeds, and known counterparties, verifying each first-party. Live
		// kernels re-advertise every pass, so the network fills in progressively with no home-grown
		// membership state. With no bootstrap_peers the directory leg is skipped and only counterparties
		// are synced. Best-effort; stops with runServer.
		discCtx, discCancel := context.WithCancel(context.Background())
		defer discCancel()
		disc := fedTransport
		go startDiscoveryLoop(discCtx, globalCfg.discoveryInterval(), func(c context.Context) {
			pctx, cancel := context.WithTimeout(c, discoveryPassTimeout)
			defer cancel()
			discoverOnce(pctx, disc, k.PeerKeys, k.AccumulateGossip, recordContact, k.GossipCursor, k.SetGossipCursor, logger)
		})
	}

	// Reap peers idle past peer_retention_days (§13 Retention) on a slow timer, plus one pass now.
	// DB-only, so it runs regardless of the federation transport; started only when enabled.
	if globalCfg.peerRetention() > 0 {
		sweepCtx, sweepCancel := context.WithCancel(context.Background())
		defer sweepCancel()
		go startPeerRetentionSweep(sweepCtx, k.PurgeIdlePeers, peerRetentionSweepInterval)
	}

	// server.ready is emitted only after a successful bind — the harness waits on this line.
	// It carries the kernel's public key and libp2p listen addrs, because federation no longer
	// exposes them over HTTP (there is no .well-known).
	pubKey, _ := k.GetConfig(context.Background(), configKeySigningPublic)
	readyFields := []any{"addr", ln.Addr().String(), "public_key", pubKey}
	if srv.fed != nil {
		readyFields = append(readyFields, "fed_addrs", srv.fed.ListenAddrs())
	}
	logger.Info("server.ready", readyFields...)

	httpSrv := &http.Server{Handler: r}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		return err
	case <-sigCtx.Done():
		stop()
		logger.Info("server.shutdown")
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}

// everyTick runs work on interval until ctx is cancelled, sequentially (a tick never overlaps the
// previous run). The one loop body behind every background sweeper below.
func everyTick(ctx context.Context, interval time.Duration, work func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			work(ctx)
		}
	}
}

// startRemoteRetryLoop is the durable worker §13 relies on to settle parked remote calls without a
// restart. It runs the two phases every such worker runs, in order and in one goroutine: first it
// drains `drain` — the work that was already pending when the transport came up — retrying each once
// so a call parked across a restart moves as soon as there is a carrier, not one interval later.
// Then it enters the ordinary schedule: every interval it lists the pending traces and retries only
// those a backoffScheduler says are due, so a long-offline peer is backed off rather than hammered
// and a call still mid-inline-round-trip isn't duplicate-dispatched. Runs are sequential (a tick
// never overlaps the previous one); every retry is idempotent (same key → the remote replays), so
// the drain and the first tick overlapping on one trace costs a replay, never a second execution.
// Stops when ctx is cancelled.
func startRemoteRetryLoop(ctx context.Context, drain []*kernel.Trace, list func(context.Context) ([]*kernel.Trace, error), retry func(context.Context, *kernel.Trace) error, interval time.Duration) {
	for _, tr := range drain {
		if ctx.Err() != nil {
			return
		}
		_ = retry(ctx, tr)
	}
	sched := newBackoffScheduler(interval)
	everyTick(ctx, interval, func(ctx context.Context) {
		traces, err := list(ctx)
		if err != nil {
			return
		}
		for _, tr := range sched.due(traces, time.Now()) {
			_ = retry(ctx, tr)
		}
	})
}

// peerRetentionSweepInterval is how often the running server reaps idle peers (§13 Retention).
// Retention is a day-scale policy, so hourly resolution is ample; kept a fixed const rather than a
// config knob to avoid surface — the policy itself (peer_retention_days) is the tunable.
const peerRetentionSweepInterval = time.Hour

// startPeerRetentionSweep reaps peers idle past PeerRetention (§13) once immediately, then every
// interval until ctx is cancelled. PurgeIdlePeers is a no-op when retention is disabled, so this is
// safe to start unconditionally; runs are sequential. Mirrors startRemoteRetryLoop.
func startPeerRetentionSweep(ctx context.Context, purge func(context.Context) (int, error), interval time.Duration) {
	_, _ = purge(ctx) // one pass at startup
	everyTick(ctx, interval, func(ctx context.Context) { _, _ = purge(ctx) })
}

// discoveryPullTimeout bounds a single gossip pull and discoveryPassTimeout the whole pass, so one
// slow or unreachable candidate can't stall the ticker. Routing discovery re-surfaces live kernels
// each pass, so a candidate skipped this pass is simply retried next.
const (
	discoveryPullTimeout      = 10 * time.Second // bounds a hung pull; a reachable peer returns in ms, so this only paces a cold relay resolve before the pass moves on
	discoveryDirectoryTimeout = 10 * time.Second // bounds advertise+enumerate so a slow DHT can't consume the whole pass and starve counterparty sync
	discoveryPassTimeout      = 30 * time.Second
)

// contactRecorder journals one outbound contact observation (§13). Synchronous and best-effort: the
// write is detached from the caller's context, because the very timeout that proves a peer
// unreachable would otherwise cancel the write recording it, and its error is dropped, because a
// display-cache write must never change the result of the operation that observed it.
type contactRecorder func(ctx context.Context, peerKey string, outcome contactOutcome, credit *int64)

// newContactRecorder is where an undecided outcome stops: only proof is persisted, so callers report
// what happened and none of them has to know that "may have arrived" means "write nothing".
func newContactRecorder(record func(context.Context, string, bool, *int64) error) contactRecorder {
	return func(ctx context.Context, peerKey string, outcome contactOutcome, credit *int64) {
		if peerKey == "" || outcome == contactUnknown {
			return
		}
		_ = record(context.WithoutCancel(ctx), peerKey, outcome == contactReached, credit)
	}
}

// fedDiscoverer is the transport capability the discovery pass needs; *fed.Transport satisfies it,
// and tests supply a fake so the pass logic is exercised without libp2p.
type fedDiscoverer interface {
	Advertise(ctx context.Context) (time.Duration, error)
	DiscoverProviders(ctx context.Context) ([]string, error)
	BootstrapKeys() []string
	Gossip(ctx context.Context, peerKey, cursor string) (json.RawMessage, error)
}

// discoverOnce runs one discovery pass (§13). It advertises this kernel to the routing-discovery
// namespace, then pulls gossip from the union of the namespace's providers, configured bootstrap
// seeds, and known counterparties. On a VERIFIED pull — one authenticated as the key we dialed
// (g.PublicKey == key) that accumulates cleanly — it refreshes the catalog, advances the evidence
// cursor, and (for a counterparty) caches liveness/credit. A non-verified pull (transport error, bad
// JSON, key mismatch, or accumulate rejection) is logged and retried a later pass; discovery holds no
// per-candidate attempt state, since routing discovery re-surfaces live kernels every pass. One
// structured discovery.pass summary ends the pass: Debug when nothing failed, Info otherwise.
func discoverOnce(ctx context.Context, d fedDiscoverer,
	peerKeys func(context.Context) []string,
	accumulate func(context.Context, *kernel.GossipResponse, string) (string, error),
	recordContact contactRecorder,
	getCursor func(context.Context, string) string,
	setCursor func(context.Context, string, string) error,
	logger *log.Logger) {

	start := time.Now()
	keys := map[string]bool{}
	for _, k := range peerKeys(ctx) {
		keys[k] = true
	}
	// Directory discovery (advertise + enumerate providers) is time-boxed and skipped entirely with no
	// bootstrap seeds, so a slow or unreachable DHT can never starve counterparty sync — the pull loop
	// below always runs for known counterparties (§13). Bootstrap keys join the pull set as seeds.
	if boot := d.BootstrapKeys(); len(boot) > 0 {
		for _, k := range boot {
			keys[k] = true
		}
		dctx, dcancel := context.WithTimeout(ctx, discoveryDirectoryTimeout)
		if _, err := d.Advertise(dctx); err != nil {
			logger.Debug("discovery.advertise.failed", "error", err.Error())
		}
		providers, derr := d.DiscoverProviders(dctx)
		dcancel()
		if derr != nil {
			logger.Debug("discovery.enumerate.failed", "error", derr.Error())
		}
		for _, k := range providers {
			keys[k] = true
		}
	}

	var ok, failed int
	for key := range keys {
		pstart := time.Now()
		fail := func(stage string, err error) {
			failed++
			logger.Info("discovery.pull.failed",
				"key", key, "stage", stage, "error", err.Error(),
				"elapsed_ms", time.Since(pstart).Milliseconds())
		}
		pctx, cancel := context.WithTimeout(ctx, discoveryPullTimeout)
		cursor := getCursor(ctx, key)
		raw, err := d.Gossip(pctx, key, cursor)
		cancel()
		if err != nil {
			// The rotation retries a later pass. Only a dial that never connected proves the peer is
			// unreachable (§13); a stream that broke mid-pull proves nothing and records nothing.
			recordContact(ctx, key, contactFromErr(err), nil)
			fail("transport", err)
			continue
		}
		var g kernel.GossipResponse
		if uerr := json.Unmarshal(raw, &g); uerr != nil {
			fail("decode", uerr) // malformed reply
			continue
		}
		if g.PublicKey != key {
			// a responder claiming an identity other than the key we dialed
			fail("mismatch", fmt.Errorf("public_key mismatch: claimed %s", g.PublicKey))
			continue
		}
		next, aerr := accumulate(ctx, &g, key) // introducer = the authenticated key, never the claimed one
		if aerr != nil {
			fail("accumulate", aerr) // an invalid handle or any other rejection is a failed pull, not a verified one
			continue
		}
		ok++
		if next != "" && next != cursor {
			_ = setCursor(ctx, key, next)
		}
		// Recorded after accumulation, which is what upserts the kernel row: an earlier write would
		// no-op on a first contact. CounterpartyBalance is set only for a known counterparty (§13),
		// so passing it through needs no separate roster check.
		recordContact(ctx, key, contactReached, g.CounterpartyBalance)
	}

	fields := []any{"candidates", len(keys), "ok", ok, "failed", failed, "duration_ms", time.Since(start).Milliseconds()}
	if failed == 0 {
		logger.Debug("discovery.pass", fields...)
	} else {
		logger.Info("discovery.pass", fields...)
	}
}

// startDiscoveryLoop refreshes the known network (§13) once immediately, then every interval until
// ctx is cancelled. Mirrors startPeerRetentionSweep; the pass is injected so the loop is testable
// without libp2p. Runs are sequential (a tick never overlaps the previous pass).
func startDiscoveryLoop(ctx context.Context, interval time.Duration, pass func(context.Context)) {
	pass(ctx) // one pass at startup
	everyTick(ctx, interval, pass)
}

// backoffScheduler decides which pending remote traces are due for a retry, spacing each trace's
// attempts with exponential backoff so a peer that stays offline is retried ever-less-often (bounded
// anyway by RemotePendingMaxAge, §13). State is in-memory and per-serve-process: a restart re-drives
// everything once via bootstrap, so nothing is lost. It is the only sweeper of pending traces during
// live serving (Recover/bootstrap run only at startup), so a plain map needs no locking.
type backoffScheduler struct {
	base    time.Duration
	entries map[string]*backoffEntry
}

type backoffEntry struct {
	attempts int
	nextAt   time.Time
}

func newBackoffScheduler(base time.Duration) *backoffScheduler {
	return &backoffScheduler{base: base, entries: map[string]*backoffEntry{}}
}

// due returns the traces to retry now and advances their schedules. A trace is first scheduled one
// base interval after its creation (skipping the inline round-trip window); each retry pushes the
// next attempt out by base<<min(attempts,5) — doubling up to a 32×base cap. Traces absent from the
// list are resolved (settled/cancelled) and their state is pruned.
func (b *backoffScheduler) due(traces []*kernel.Trace, now time.Time) []*kernel.Trace {
	live := make(map[string]struct{}, len(traces))
	var out []*kernel.Trace
	for _, tr := range traces {
		live[tr.ID] = struct{}{}
		e := b.entries[tr.ID]
		if e == nil {
			e = &backoffEntry{nextAt: tr.CreatedAt.Add(b.base)}
			b.entries[tr.ID] = e
		}
		if now.Before(e.nextAt) {
			continue
		}
		out = append(out, tr)
		e.attempts++
		shift := e.attempts - 1 // 1st retry waits base, then 2×, 4×, … capped at 32×
		if shift > 5 {
			shift = 5
		}
		e.nextAt = now.Add(b.base << shift)
	}
	for id := range b.entries {
		if _, ok := live[id]; !ok {
			delete(b.entries, id)
		}
	}
	return out
}

// ---- server ----

type server struct {
	kernel *kernel.Kernel
	log    *log.Logger
	fed    fedClient    // federation transport (§13); nil until runServer starts it
	oauth  *grantBroker // delegated-OAuth consent broker (§8); nil when no credentials box
}

// fedClient is the outbound half of the libp2p transport the admin handlers need. *fed.Transport
// satisfies it; keeping it an interface lets tests supply a fake to exercise online/offline paths
// without a real network.
type fedClient interface {
	Gossip(ctx context.Context, peerKey, cursor string) (json.RawMessage, error)
	Step(ctx context.Context, peerKey string, req fed.StepRequest) (fed.StepResponse, error)
	Probe(ctx context.Context, peerKey string) fed.Reachability
	ListenAddrs() []string
	Close() error
}

// fedOpTimeout bounds any single outbound federation call an admin command makes, so an offline
// peer fails promptly (§13) rather than stalling on the DHT resolve/dial up to the client timeout.
const fedOpTimeout = 8 * time.Second

// registerRoutes mounts all application routes onto r for the given server.
// Rate-limited routes (auth, user creation) are registered by the caller before this call.
func registerRoutes(r chi.Router, srv *server) {
	// Health (unauthenticated). Doubles as an identity banner so someone can see which kernel
	// they're pointed at before logging in: the handle and public key are the kernel's advertised
	// federation identity (§13), not secrets — only the private key is withheld.
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		pub, _ := srv.kernel.GetConfig(r.Context(), configKeySigningPublic)
		writeJSON(w, http.StatusOK, map[string]string{
			"status":     "ok",
			"handle":     globalCfg.KernelHandle,
			"public_key": pub,
		})
	})

	// Federation has no HTTP surface: peer identity, inbound calls, manifests, gossip, and
	// inspection travel over the libp2p transport (§13), started in runServer. Public action
	// listing stays on HTTP for local/user clients.
	r.Get("/v1/actions", srv.getActions)
	// Ratings are public reputation evidence (§13, §16): readable wherever the action is visible,
	// anonymously for a public action. Optional auth, gated per-action inside the handler.
	r.Get("/v1/actions/{id}/ratings", srv.listActionRatings)

	// Actions (authenticated).
	r.Group(func(r chi.Router) {
		r.Use(srv.authMiddleware)
		r.Post("/v1/actions/import", srv.importOpenAPI)
		r.Post("/v1/actions/unimport", srv.unimportOpenAPI)
		r.Post("/v1/actions", srv.postAction)
		r.Get("/v1/actions/{id}", srv.getAction)
		r.Put("/v1/actions/{id}", srv.updateAction)
		r.Post("/v1/actions/{id}/enable", srv.setActionActive(true))
		r.Post("/v1/actions/{id}/disable", srv.setActionActive(false))
		r.Delete("/v1/actions/{id}", srv.deleteAction)

		// Processes.
		r.Get("/v1/processes", srv.listProcesses)
		r.Get("/v1/processes/{id}", srv.getProcess)
		r.Post("/v1/processes/{id}/end", srv.endProcess)

		// Run.
		r.Post("/v1/run", srv.postRun)

		// Transactions.
		r.Get("/v1/transactions", srv.listTransactions)
		r.Get("/v1/transactions/{id}", srv.getTransaction)
		r.Post("/v1/transactions/{id}/rate", srv.rateTransaction)
		r.Get("/v1/transactions/{id}/receipt-verification", srv.getReceiptVerification)

		// Stats.
		r.Get("/v1/stats/{id}", srv.getStats)

		// Steps (reads are JWT-only; the POSTs accept a capability too — see below).
		r.Get("/v1/steps", srv.listSteps)
		r.Get("/v1/steps/{id}", srv.getStep)

		// Current user.
		r.Get("/v1/me", srv.getMe)
		r.Put("/v1/me", srv.putMe)

		// Peer-to-peer credit transfer and the caller's own ledger (§12). Not superuser:
		// the caller moves their own funds, gated by authMiddleware alone.
		r.Post("/v1/transfers", srv.postTransfer)
		r.Get("/v1/ledger", srv.getLedger)

		// Delegated-auth grants and connections (§8). The client hosts the loopback redirect; the
		// kernel holds only in-memory PKCE/device state and performs the token exchange itself.
		r.Get("/v1/grants/plan", srv.getGrantPlan)
		r.Post("/v1/grants/start", srv.postGrantStart)
		r.Post("/v1/grants/complete", srv.postGrantComplete)
		r.Post("/v1/grants", srv.postGrant)
		r.Delete("/v1/grants", srv.deleteGrant)
	})

	// Composition surface (§9): step creation/completion accept a user JWT or a trace-scoped
	// capability; /v1/call is the capability-only HTTP twin of juice.call (a subcall, no wallet path).
	r.With(srv.authOrCapability).Post("/v1/steps", srv.postStep)
	r.With(srv.authOrCapability).Post("/v1/steps/{id}/complete", srv.postCompleteStep)
	r.With(srv.capabilityOnly).Post("/v1/call", srv.postCall)

	// Superuser supervision (money, access, federation trust, roster) — same TCP API, gated
	// per-route by requireSuperuserMW (§14). Not a separate surface; authority is the @sys bearer.
	r.Group(func(r chi.Router) {
		r.Use(srv.authMiddleware, srv.requireSuperuserMW)
		r.Get("/control/users", srv.ctlListUsers)
		r.Get("/control/users/{handle}", srv.ctlShowUser)
		r.Post("/control/users/{handle}/suspend", srv.ctlSetSuspended(true))
		r.Post("/control/users/{handle}/unsuspend", srv.ctlSetSuspended(false))
		r.Post("/control/users/{handle}/rename", srv.ctlRenameUser)
		r.Post("/control/deposit", srv.ctlAdjust(true))
		r.Post("/control/withdraw", srv.ctlAdjust(false))
		r.Post("/control/peers/settle", srv.ctlSettlePeer)
		r.Get("/control/peers", srv.ctlListPeers)
		r.Get("/control/peers/inspect", srv.ctlInspectPeer)
		r.Get("/control/identity", srv.ctlIdentity)
	})
}

// ---- middleware ----

type ctxKey string

const (
	ctxCallerID ctxKey = "caller_id"
	ctxCapTrace ctxKey = "cap_trace"
	ctxCapOwner ctxKey = "cap_owner"
)

// rateLimitKey resolves the client key for the rate limiter and whether to exempt it. A genuine
// loopback client — the operator's own CLI talking to its own kernel, with no proxy header — is
// exempt: it is already inside the trust boundary the limiter defends. Only a same-host process can
// present a loopback RemoteAddr, so when one also carries X-Forwarded-For we are behind a co-located
// reverse proxy; we then key on the real client (the last forwarded hop, which the trusted proxy
// appended) so external callers are limited per-client rather than lumped into one loopback bucket.
func rateLimitKey(r *http.Request) (key string, exempt bool) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
		xff := r.Header.Get("X-Forwarded-For")
		if xff == "" {
			return "", true
		}
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1]), false
	}
	return ip, false
}

// ipRateLimiter returns a middleware that limits requests per client using a token bucket, keyed by
// rateLimitKey (genuine loopback is exempt). Entries not seen for 5 minutes are evicted by a
// background goroutine.
func ipRateLimiter(ratePerSec, burst float64) func(http.Handler) http.Handler {
	type entry struct {
		lim      *rate.Limiter
		lastSeen time.Time
	}
	var mu sync.Mutex
	entries := make(map[string]*entry)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cutoff := time.Now().Add(-5 * time.Minute)
			mu.Lock()
			for ip, e := range entries {
				if e.lastSeen.Before(cutoff) {
					delete(entries, ip)
				}
			}
			mu.Unlock()
		}
	}()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, exempt := rateLimitKey(r)
			if exempt {
				next.ServeHTTP(w, r)
				return
			}
			mu.Lock()
			e, ok := entries[key]
			if !ok {
				e = &entry{lim: rate.NewLimiter(rate.Limit(ratePerSec), int(burst))}
				entries[key] = e
			}
			e.lastSeen = time.Now()
			allow := e.lim.Allow()
			mu.Unlock()
			if !allow {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Request-body limits. Most endpoints take a tight default; the action create/update
// routes accept a precompiled WASM artifact (POST /v1/actions "wasm_artifact") or WASM
// source, which routinely exceed 1 MiB, so they get a larger cap.
const (
	defaultMaxBodyBytes = 1 << 20  // 1 MiB
	actionMaxBodyBytes  = 16 << 20 // 16 MiB — accommodates compiled WASM artifacts
)

// maxBytesMiddleware caps the request body, choosing the limit by route so a large WASM
// artifact may be registered over HTTP without loosening the default everywhere.
func maxBytesMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(defaultMaxBodyBytes)
		if isActionWriteRoute(r) {
			limit = actionMaxBodyBytes
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// isActionWriteRoute reports whether r targets an action create/update endpoint — the
// only write paths that may carry a base64 WASM artifact or WASM source.
func isActionWriteRoute(r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/actions":
		return true
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/actions/"):
		return true
	default:
		return false
	}
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.New().String()
		ctx := log.WithRequestID(r.Context(), id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func loggingMiddleware(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.With(r.Context()).Info("http.request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeErr(w, kernel.ErrUnauthenticated.Wrap("missing bearer token"))
			return
		}
		tok := strings.TrimPrefix(auth, "Bearer ")
		callerID, err := s.kernel.VerifyToken(tok)
		if err != nil {
			writeErr(w, err)
			return
		}
		// Verify caller exists and is not suspended.
		u, err := s.kernel.ReadUser(r.Context(), callerID)
		if err != nil {
			writeErr(w, kernel.ErrUnauthenticated.Wrap("caller not found"))
			return
		}
		if u.SuspendedAt != nil {
			writeErr(w, kernel.ErrUnauthenticated.Wrap("account suspended"))
			return
		}
		ctx := context.WithValue(r.Context(), ctxCallerID, callerID)
		ctx = log.WithCallerUserID(ctx, callerID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func callerFrom(r *http.Request) string {
	return callerFromContext(r.Context())
}

// verifyCap reads and verifies the trace-scoped composition capability (§9), returning the
// named trace and its action owner, and stashing them on the context.
func (s *server) verifyCap(r *http.Request) (context.Context, error) {
	tok := r.Header.Get(capabilityHeader)
	if tok == "" {
		return r.Context(), nil // no capability presented
	}
	trace, owner, err := s.kernel.VerifyCapability(r.Context(), tok)
	if err != nil {
		return nil, err
	}
	ctx := context.WithValue(r.Context(), ctxCapTrace, trace)
	ctx = context.WithValue(ctx, ctxCapOwner, owner)
	return ctx, nil
}

// capFromContext returns the capability's trace and action owner, and whether one is present.
func capFromContext(r *http.Request) (trace, owner string, ok bool) {
	trace, _ = r.Context().Value(ctxCapTrace).(string)
	owner, _ = r.Context().Value(ctxCapOwner).(string)
	return trace, owner, trace != ""
}

// capabilityOnly authorizes a route by capability alone (no user JWT) — used by /v1/call, which
// has no wallet path and must never run as a user.
func (s *server) capabilityOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(capabilityHeader) == "" {
			writeErr(w, kernel.ErrUnauthenticated.Wrap("capability required"))
			return
		}
		ctx, err := s.verifyCap(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authOrCapability accepts either a capability (§9) or a user JWT — used by the step routes,
// which a user or a composing endpoint may both drive.
func (s *server) authOrCapability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(capabilityHeader) != "" {
			ctx, err := s.verifyCap(r)
			if err != nil {
				writeErr(w, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		s.authMiddleware(next).ServeHTTP(w, r)
	})
}

func callerFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxCallerID).(string)
	return v
}

// optionalAuth extracts and verifies a Bearer token without failing the request.
// Returns the caller user ID or "" if absent, invalid, or suspended.
func (s *server) optionalAuth(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	callerID, err := s.kernel.VerifyToken(strings.TrimPrefix(auth, "Bearer "))
	if err != nil {
		return ""
	}
	u, err := s.kernel.ReadUser(r.Context(), callerID)
	if err != nil || u.SuspendedAt != nil {
		return ""
	}
	return callerID
}

// ---- handlers ----

func (s *server) postUser(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req kernel.CreateUserRequest) (any, int, error) {
		view, err := createUser(s.kernel, r.Context(), req)
		return view, http.StatusCreated, err
	})(w, r)
}

func (s *server) postRecoverStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle string `json:"handle"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	view, err := startRecovery(s.kernel, r.Context(), req.Handle)
	writeOr(w, view, err)
}

func (s *server) postRecoverComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle    string `json:"handle"`
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"`
		Password  string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	view, err := completeRecovery(s.kernel, r.Context(), req.Handle, req.Nonce, req.Signature, req.Password)
	writeOr(w, view, err)
}

func (s *server) getActions(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	limit, offset := listBounds(r)
	resps, err := listPublicActions(s.kernel, r.Context(), s.optionalAuth(r),
		r.URL.Query().Get("owner"), r.URL.Query().Get("name"), all, limit, offset)
	writeOr(w, resps, err)
}

func (s *server) importOpenAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SpecURL string `json:"spec_url"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.SpecURL == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("spec_url is required"))
		return
	}
	specBytes, err := fetchOpenAPISpec(r.Context(), req.SpecURL, s.kernel.AllowsLocalSources())
	if err != nil {
		writeErr(w, err)
		return
	}
	result, err := s.kernel.ImportOpenAPI(r.Context(), callerFrom(r), callerFrom(r), req.SpecURL, specBytes)
	writeOr(w, result, err)
}

func (s *server) unimportOpenAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SpecURL     string `json:"spec_url"`
		Name        string `json:"name"`
		OwnerHandle string `json:"owner_handle"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.SpecURL == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("spec_url is required"))
		return
	}
	sub := callerFrom(r)
	ownerID := sub
	if req.OwnerHandle != "" {
		owner, err := s.kernel.ReadUserByHandle(r.Context(), req.OwnerHandle)
		if err != nil {
			writeErr(w, kernel.ErrNotFound.Wrap("owner not found"))
			return
		}
		ownerID = owner.ID
	}
	actions, err := s.kernel.UnimportOpenAPI(r.Context(), sub, ownerID, req.SpecURL, req.Name)
	writeOr(w, actions, err)
}

func (s *server) postAction(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req kernel.CreateActionRequest) (any, int, error) {
		req.OwnerUserID = callerFrom(r) // authority, never the wire (§14)
		a, err := createAction(s.kernel, r.Context(), callerFrom(r), req)
		return a, http.StatusCreated, err
	})(w, r)
}

func (s *server) getAction(w http.ResponseWriter, r *http.Request) {
	a, err := getAction(s.kernel, r.Context(), callerFrom(r), pathID(r))
	writeOr(w, a, err)
}

// ratingView is the public reputation projection of a Rating (§13, §16): the market signal only,
// never the rater identity, the transaction/receipt it links, or the signature.
type ratingView struct {
	Value   int       `json:"value"`
	Note    *string   `json:"note"`
	Created time.Time `json:"created_at"`
}

func (s *server) listActionRatings(w http.ResponseWriter, r *http.Request) {
	// Gate on the action's own visibility (anonymous caller allowed for a public action); the read
	// is independent of the action's active state so reputation survives deactivation (§8).
	if _, err := s.kernel.ReadActionForSubject(r.Context(), s.optionalAuth(r), pathID(r)); err != nil {
		writeErr(w, err)
		return
	}
	limit, offset := listBounds(r)
	ratings, err := s.kernel.ListRatings(r.Context(), pathID(r), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	views := make([]ratingView, 0, len(ratings))
	for _, rt := range ratings {
		views = append(views, ratingView{Value: int(rt.Rating), Note: rt.Note, Created: rt.CreatedAt})
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *server) updateAction(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req kernel.UpdateActionRequest) (any, int, error) {
		req.ID = pathID(r) // path-derived, never the wire
		a, err := updateAction(s.kernel, r.Context(), callerFrom(r), req)
		return a, http.StatusOK, err
	})(w, r)
}

func (s *server) setActionActive(active bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var err error
		if active {
			err = s.kernel.SetActive(r.Context(), callerFrom(r), pathID(r), true)
		} else {
			err = s.kernel.SetActive(r.Context(), callerFrom(r), pathID(r), false)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"active": active})
	}
}

func (s *server) deleteAction(w http.ResponseWriter, r *http.Request) {
	if err := s.kernel.DeleteAction(r.Context(), callerFrom(r), pathID(r)); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listProcesses(w http.ResponseWriter, r *http.Request) {
	limit, offset := listBounds(r)
	processes, err := listProcesses(s.kernel, r.Context(), callerFrom(r), limit, offset)
	writeOr(w, processes, err)
}

func (s *server) getProcess(w http.ResponseWriter, r *http.Request) {
	p, err := getProcess(s.kernel, r.Context(), callerFrom(r), pathID(r))
	writeOr(w, p, err)
}

func (s *server) endProcess(w http.ResponseWriter, r *http.Request) {
	if err := s.kernel.EndProcess(r.Context(), callerFrom(r), pathID(r)); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) postRun(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req kernel.RunRequest) (any, int, error) {
		if req.ActionRef == "" {
			return nil, 0, kernel.ErrInvalidInput.Wrap("action is required")
		}
		if req.Args == nil { // absent or null; {} decodes to a non-nil empty map (§14)
			return nil, 0, kernel.ErrInvalidInput.Wrap("args is required")
		}
		req.CallerID = callerFrom(r)
		reply, err := s.kernel.Run(r.Context(), req)
		return reply, http.StatusOK, err
	})(w, r)
}

func (s *server) listTransactions(w http.ResponseWriter, r *http.Request) {
	limit, offset := listBounds(r)
	txs, err := listTransactions(s.kernel, r.Context(), callerFrom(r), kernel.TxFilter{
		ProcessID: r.URL.Query().Get("process_id"),
		Limit:     limit,
		Offset:    offset,
	})
	writeOr(w, txs, err)
}

func (s *server) getTransaction(w http.ResponseWriter, r *http.Request) {
	tx, err := getTransaction(s.kernel, r.Context(), callerFrom(r), pathID(r))
	writeOr(w, tx, err)
}

func (s *server) rateTransaction(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req struct {
		Rating float64 `json:"rating"`
		Note   *string `json:"note"`
	}) (any, int, error) {
		rating, err := s.kernel.RateTransaction(r.Context(), callerFrom(r), pathID(r), req.Rating, req.Note)
		return rating, http.StatusOK, err
	})(w, r)
}

func (s *server) getReceiptVerification(w http.ResponseWriter, r *http.Request) {
	v, err := s.kernel.VerifyRemoteReceipt(r.Context(), callerFrom(r), pathID(r))
	writeOr(w, v, err)
}

func (s *server) getStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.kernel.ReadStats(r.Context(), pathID(r))
	writeOr(w, stats, err)
}

// ---- PKCE / auth handlers ----

func (s *server) postAuthorize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle        string `json:"handle"`
		Password      string `json:"password"`
		CodeChallenge string `json:"code_challenge"`
		RedirectURI   string `json:"redirect_uri"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Handle == "" || req.Password == "" || req.CodeChallenge == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("handle, password, and code_challenge are required"))
		return
	}
	redirect, err := s.kernel.StartAuthCode(r.Context(), req.Handle, req.Password, req.CodeChallenge, req.RedirectURI)
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.RedirectURI != "" {
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
}

func (s *server) postRefresh(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req struct {
		RefreshToken string `json:"refresh_token"`
	}) (any, int, error) {
		access, newRT, err := s.kernel.RefreshAccessToken(r.Context(), req.RefreshToken)
		if err != nil {
			return nil, 0, err
		}
		return map[string]string{"access_token": access, "refresh_token": newRT}, http.StatusOK, nil
	})(w, r)
}

func (s *server) postLogout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		writeErr(w, kernel.ErrUnauthenticated.Wrap("refresh_token is required"))
		return
	}
	if err := s.kernel.RevokeRefreshToken(r.Context(), req.RefreshToken); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postToken handles POST /v1/auth/token (JSON only): the authorization-code + PKCE exchange,
// the sole token-issuing form §14 defines. A password never reaches this route — credentials go
// to /v1/auth/authorize, which mints the code this exchanges (§12).
func (s *server) postToken(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		RedirectURI  string `json:"redirect_uri"`
	}) (any, int, error) {
		access, refresh, err := s.kernel.ExchangeAuthCode(r.Context(), req.Code, req.CodeVerifier, req.RedirectURI)
		if err != nil {
			return nil, 0, err
		}
		return map[string]string{"access_token": access, "refresh_token": refresh}, http.StatusOK, nil
	})(w, r)
}

// ---- Step handlers ----

func (s *server) listSteps(w http.ResponseWriter, r *http.Request) {
	limit, offset := listBounds(r)
	views, err := listSteps(s.kernel, r.Context(), callerFrom(r),
		r.URL.Query().Get("process_id"), r.URL.Query().Get("status"), limit, offset)
	writeOr(w, views, err)
}

func (s *server) postStep(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req createStepParams) (any, int, error) {
		// A capability supplies the trace (the cap IS the trace, §9); a JWT caller supplies trace_id.
		capTrace, capOwner, isCap := capFromContext(r)
		callerID := callerFrom(r)
		switch {
		case isCap && req.TraceID != "":
			return nil, 0, kernel.ErrInvalidInput.Wrap("trace_id must not be sent with a capability")
		case isCap:
			req.TraceID, callerID, req.ViaCapability = capTrace, capOwner, true
		case req.TraceID == "":
			return nil, 0, kernel.ErrInvalidInput.Wrap("trace_id is required")
		}
		if req.ActionRef == "" {
			return nil, 0, kernel.ErrInvalidInput.Wrap("action is required")
		}
		if req.RequiredCaller == "" {
			return nil, 0, kernel.ErrInvalidInput.Wrap("required_caller is required")
		}
		if req.PartialArgs == nil {
			return nil, 0, kernel.ErrInvalidInput.Wrap("partial_args is required")
		}
		req.RequiredCaller = kernel.NormalizeHandle(req.RequiredCaller)
		view, err := createStep(s.kernel, r.Context(), callerID, req)
		return view, http.StatusCreated, err
	})(w, r)
}

func (s *server) getStep(w http.ResponseWriter, r *http.Request) {
	step, err := getStep(s.kernel, r.Context(), callerFrom(r), pathID(r))
	writeOr(w, step, err)
}

func (s *server) postCompleteStep(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req struct {
		Args *json.RawMessage `json:"args"`
		Peer string           `json:"peer"`
	}) (any, int, error) {
		if req.Args == nil {
			return nil, 0, kernel.ErrInvalidInput.Wrap("args is required")
		}
		capTrace, capOwner, isCap := capFromContext(r)
		// The --peer request is signed by the whole kernel, so it is a session-caller path only. A
		// capability is local to its trace and carries no supervision or federation authority (§9) —
		// and it presents no session caller, which downstream reads as the kernel-level form. Checked
		// before the branch, or an untrusted endpoint would dispatch abroad as the operator.
		if isCap && req.Peer != "" {
			return nil, 0, kernel.ErrUnauthorized.Wrap("a capability cannot complete a step on a peer")
		}
		// A peer-held step is completed over /juice/fed/step/1 (§13) — the same command, since a step
		// is a step. An ordinary authenticated user may complete a remote step addressed to THEM: the
		// home kernel attaches a step_auth attestation naming their stable id, and the serving kernel
		// enforces it against the step's required remote caller. A superuser completes kernel-level
		// (no attestation) — the request acts as the whole kernel, for a kernel-addressed step.
		if req.Peer != "" {
			callerID := callerFrom(r)
			forUserID := callerID
			if s.kernel.IsSuperuser(r.Context(), callerID) {
				forUserID = "" // kernel-level completion (no per-user attestation)
			}
			peerKey, err := s.resolvePeerKey(r.Context(), strings.TrimSpace(req.Peer))
			if err != nil {
				return nil, 0, err
			}
			body, err := s.kernel.CompletePeerStep(r.Context(), peerKey, pathID(r), *req.Args, forUserID)
			return body, http.StatusOK, err
		}
		// Under a capability the caller is the executing action's owner AND the authority is the
		// capability's own trace (§9): CompleteStepInTrace enforces both, so the cap completes only
		// steps its own trace parked, never one living in another user's process.
		if isCap {
			reply, err := s.kernel.CompleteStepInTrace(r.Context(), capOwner, capTrace, pathID(r), *req.Args)
			return reply, http.StatusOK, err
		}
		reply, err := s.kernel.CompleteStep(r.Context(), callerFrom(r), pathID(r), *req.Args)
		return reply, http.StatusOK, err
	})(w, r)
}

// postCall is the capability-only HTTP twin of juice.call (§9): a subcall on the capability's
// trace. There is no wallet/BeginRun path here, making C3's wallet-exclusion structural.
func (s *server) postCall(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req kernel.SubcallRequest) (any, int, error) {
		if req.ActionRef == "" {
			return nil, 0, kernel.ErrInvalidInput.Wrap("action is required")
		}
		if req.Args == nil {
			return nil, 0, kernel.ErrInvalidInput.Wrap("args is required")
		}
		trace, owner, ok := capFromContext(r)
		if !ok {
			return nil, 0, kernel.ErrUnauthenticated.Wrap("capability required")
		}
		req.CallerID, req.ParentTraceID = owner, trace
		reply, err := s.kernel.Subcall(r.Context(), req)
		return reply, http.StatusOK, err
	})(w, r)
}

// ---- me ----

func (s *server) getMe(w http.ResponseWriter, r *http.Request) {
	view, err := getMe(s.kernel, r.Context(), callerFrom(r))
	writeOr(w, view, err)
}

func (s *server) putMe(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req kernel.UpdateUserRequest) (any, int, error) {
		view, err := updateMe(s.kernel, r.Context(), callerFrom(r), req)
		return view, http.StatusOK, err
	})(w, r)
}

// postTransfer moves credits from the authenticated caller to a nominated local recipient (§12).
func (s *server) postTransfer(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, body struct {
		Recipient   string `json:"recipient"`
		Amount      int64  `json:"amount"`
		Reason      string `json:"reason"`
		ExternalKey string `json:"external_key"`
	}) (any, int, error) {
		recipient, err := s.kernel.ResolveUser(r.Context(), body.Recipient)
		if err != nil {
			return nil, 0, err
		}
		e, err := s.kernel.Transfer(r.Context(), callerFrom(r), recipient.ID, body.Amount, body.Reason, body.ExternalKey)
		if err != nil {
			return nil, 0, err
		}
		return enrichLedger(e, newAccountCache(s.kernel, r.Context())), http.StatusOK, nil
	})(w, r)
}

// getLedger returns the caller's own ledger entries (deposits, withdrawals, transfers).
func (s *server) getLedger(w http.ResponseWriter, r *http.Request) {
	limit, offset := listBounds(r)
	entries, err := s.kernel.ListLedger(r.Context(), callerFrom(r), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	uc := newAccountCache(s.kernel, r.Context())
	views := make([]*ledgerView, len(entries))
	for i, e := range entries {
		views[i] = enrichLedger(e, uc)
	}
	writeJSON(w, http.StatusOK, views)
}

// ---- delegated-OAuth grants (§8) ----

// getGrantPlan expands a selector into the consent plan user connect walks (§8).
func (s *server) getGrantPlan(w http.ResponseWriter, r *http.Request) {
	selector := r.URL.Query().Get("selector")
	if selector == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("selector query parameter is required"))
		return
	}
	res, err := s.kernel.ConsentPlan(r.Context(), callerFrom(r), selector)
	writeOr(w, res, err)
}

func (s *server) postGrantStart(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, body struct {
		Selector    string `json:"selector"`
		Provider    string `json:"provider"`
		RedirectURI string `json:"redirect_uri"`
		Flow        string `json:"flow"`
	}) (any, int, error) {
		res, err := startGrant(s.kernel, s.oauth, r.Context(), callerFrom(r), body.Selector, body.Provider, body.RedirectURI, body.Flow)
		return res, http.StatusOK, err
	})(w, r)
}

func (s *server) postGrantComplete(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, body struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}) (any, int, error) {
		res, err := completeGrant(s.kernel, s.oauth, r.Context(), callerFrom(r), body.State, body.Code)
		return res, http.StatusOK, err
	})(w, r)
}

// postGrant is the direct token-store for a delegated_bearer group (§8): a paste-once static token,
// no browser roundtrip. OAuth groups use start/complete instead.
func (s *server) postGrant(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, body struct {
		Selector string `json:"selector"`
		Provider string `json:"provider"`
		Token    string `json:"token"`
	}) (any, int, error) {
		res, err := attachToken(s.kernel, r.Context(), callerFrom(r), body.Selector, body.Provider, body.Token)
		return res, http.StatusOK, err
	})(w, r)
}

// deleteGrant revokes by selector (grants only) or by account (connection + cascade), §8. A full
// owner/name is the degenerate single-action selector, so there is one spelling per intent.
func (s *server) deleteGrant(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if account := q.Get("account"); account != "" {
		res, err := revokeConnection(s.kernel, r.Context(), callerFrom(r), account)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	selector := q.Get("selector")
	if selector == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("selector or account query parameter is required"))
		return
	}
	res, err := revokeGrantsBySelector(s.kernel, r.Context(), callerFrom(r), selector)
	writeOr(w, res, err)
}

// ---- health command ----

func healthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check server health",
		RunE: func(_ *cobra.Command, _ []string) error {
			// Resolve the target through the single client resolver (§14: --server), like every
			// other command — no bespoke URL that could hit another kernel.
			base := serverBaseURL()
			resp, err := http.Get(base + "/health") //nolint:noctx
			if err != nil {
				return errUnreachable(base, err)
			}
			defer resp.Body.Close()
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			if resp.StatusCode != http.StatusOK {
				return kernel.ErrExecutionFailed.Wrapf("server returned status %d", resp.StatusCode)
			}
			if flagJSON {
				return printJSON(body)
			}
			h, _ := body["handle"].(string)
			pk, _ := body["public_key"].(string)
			fmt.Printf("ok  %s  %s\n", h, pk)
			return nil
		},
	}
	return cmd
}

// ---- response helpers ----

// pathID extracts the {id} URL parameter.
func pathID(r *http.Request) string { return chi.URLParam(r, "id") }

// handle wraps a typed request/response handler: decodes body, calls fn, writes JSON.
// fn returns (response, httpStatus, error); status is ignored on error.
func handle[Req, Resp any](fn func(*http.Request, Req) (Resp, int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if !decodeBody(w, r, &req) {
			return
		}
		resp, status, err := fn(r, req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, status, resp)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := kernel.HTTPStatus(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{
		"error": fmt.Sprintf("%v", err),
		"code":  kernel.KernelErrorCode(err),
	}
	// Structured, machine-actionable context (e.g. the action a grant is required for),
	// so clients act on fields rather than parsing the message (§8).
	if ke, ok := err.(*kernel.KernelError); ok && len(ke.Meta) > 0 {
		body["meta"] = ke.Meta
	}
	_ = json.NewEncoder(w).Encode(body)
}

// ---- Federation transport wiring ----
//
// The libp2p federation transport (§13) addresses peers by key, with no HTTP endpoints. Inbound
// protocol streams are answered by fedHandlers, which reuses the same verification and settlement
// logic (handleFederationCall, etc.); outbound remote-proxy calls go out over the transport.

// startFedTransport builds and starts the federation transport for a serving kernel. It reads the
// platform signing key from config (the same key bootstrap loaded) so the libp2p identity is the
// kernel's Ed25519 identity (§12). AllowPrivateAddrs mirrors allow_local_sources so the flow
// harness can run a whole network on loopback.
func startFedTransport(ctx context.Context, k *kernel.Kernel, logger *log.Logger) (*fed.Transport, error) {
	privB64, _ := k.GetConfig(ctx, configKeySigningPrivate)
	privBytes, err := base64.RawURLEncoding.DecodeString(privB64)
	if err != nil || len(privBytes) != ed25519.PrivateKeySize {
		return nil, kernel.ErrInvalidState.Wrap("signing key unavailable for federation transport")
	}
	handlers := &fedHandlers{kernel: k, log: logger, callLimiter: newKeyLimiter(50, 100)}
	tr, err := fed.New(ctx, fed.Config{
		SigningKey:        ed25519.PrivateKey(privBytes),
		BootstrapPeers:    globalCfg.BootstrapPeers,
		Handlers:          handlers,
		AllowPrivateAddrs: globalCfg.AllowLocalSources,
	})
	if err != nil {
		return nil, err
	}
	return tr, nil
}
