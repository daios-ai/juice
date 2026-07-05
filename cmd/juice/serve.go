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
	k, db, logger, httpExec, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()

	if err := bootstrap(k, globalCfg.Native); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
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

	// Auth — rate limited: 5 requests/minute per IP, burst of 10.
	authLimiter := ipRateLimiter(5.0/60, 10)
	r.With(authLimiter).Post("/v1/auth/token", srv.postTokenMulti)
	r.With(authLimiter).Post("/v1/auth/authorize", srv.postAuthorize)
	r.With(authLimiter).Post("/v1/auth/refresh", srv.postRefresh)
	r.With(authLimiter).Post("/v1/auth/logout", srv.postLogout)

	// Users — rate limited: 3 requests/minute per IP, burst of 5.
	r.With(ipRateLimiter(3.0/60, 5)).Post("/v1/users", srv.postUser)

	registerRoutes(r, srv)

	// Bind explicitly so a bind failure is a real, immediate error, and so --addr host:0
	// (OS-assigned port) works: we then advertise the address we actually bound.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	// Start the federation transport (§13): peers addressed by key, no HTTP endpoints. The
	// libp2p identity is the platform signing key, so the transport IS this kernel's identity.
	fedTransport, ferr := startFedTransport(context.Background(), k, logger)
	if ferr != nil {
		logger.Error("fed.start_failed", "error", ferr)
	} else {
		srv.fed = fedTransport
		httpExec.fedTransport = fedTransport
		httpExec.localPubKey, _ = k.GetConfig(context.Background(), configKeySigningPublic)
		defer fedTransport.Close()

		// Drive pending remote-proxy calls on a timer so a peer coming back online settles parked
		// calls without a restart, and the RemotePendingMaxAge refund fires from the running server
		// (§13). bootstrap already ran one pass for calls pending at the last shutdown; this keeps
		// them moving. The loop stops when runServer returns.
		retryCtx, retryCancel := context.WithCancel(context.Background())
		defer retryCancel()
		go startRemoteRetryLoop(retryCtx, k.PendingRemoteTraces, k.RetryRemoteTrace, globalCfg.remoteRetryInterval())

		// Grow the known network (§13): advertise under the discovery rendezvous and pull gossip
		// from the bootstrap seeds + enumerated DHT providers into discovered_kernels, so a fresh
		// box has a directory to friend from without already knowing anyone. Empty bootstrap_peers
		// means neither announce nor discover, so skip the loop entirely. Best-effort; stops with
		// runServer.
		if len(globalCfg.BootstrapPeers) > 0 {
			discCtx, discCancel := context.WithCancel(context.Background())
			defer discCancel()
			disc := fedTransport
			go startDiscoveryLoop(discCtx, globalCfg.discoveryInterval(), func(c context.Context) {
				pctx, cancel := context.WithTimeout(c, discoveryPassTimeout)
				defer cancel()
				discoverOnce(pctx, disc, k.AccumulateGossip, logger)
			})
		}
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

// startRemoteRetryLoop is the "server ticker" §13 relies on to settle parked remote calls without a
// restart. Every interval it lists the pending remote traces and retries only those a backoffScheduler
// says are due, so a long-offline peer is backed off rather than hammered every tick, and a call still
// mid-inline-round-trip isn't duplicate-dispatched. Runs are sequential (a tick never overlaps the
// previous one); the retry is idempotent (same key → the remote replays). Stops when ctx is cancelled.
func startRemoteRetryLoop(ctx context.Context, list func(context.Context) ([]*kernel.Trace, error), retry func(context.Context, *kernel.Trace) error, interval time.Duration) {
	sched := newBackoffScheduler(interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			traces, err := list(ctx)
			if err != nil {
				continue
			}
			for _, tr := range sched.due(traces, time.Now()) {
				_ = retry(ctx, tr)
			}
		}
	}
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
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = purge(ctx)
		}
	}
}

// discoveryFanout caps how many DHT-enumerated provider keys one pass pulls gossip from, and
// discoveryPassTimeout bounds a whole pass so a slow DHT or an unreachable peer can't stall the
// ticker. The directory keeps filling across passes, so a per-pass cap costs only latency.
const (
	discoveryFanout      = 25
	discoveryPassTimeout = 30 * time.Second
)

// fedDiscoverer is the transport capability the discovery pass needs; *fed.Transport satisfies it,
// and tests supply a fake so the pass logic is exercised without libp2p.
type fedDiscoverer interface {
	Advertise(ctx context.Context) error
	BootstrapKeys() []string
	DiscoverProviders(ctx context.Context, limit int) []string
	Gossip(ctx context.Context, peerKey string) (json.RawMessage, error)
}

// discoverOnce runs one known-network refresh (§13): advertise under the discovery rendezvous, then
// pull gossip from the bootstrap seeds plus enumerated DHT providers and accumulate each into the
// discovered-kernels table. Best-effort throughout — an offline DHT or peer is skipped, never fatal.
// The introducer is the pulled kernel's own key (a first-party self-report); AccumulateGossip records
// that kernel's transacted friends introduced-by it.
func discoverOnce(ctx context.Context, d fedDiscoverer, accumulate func(context.Context, *kernel.GossipResponse, string) error, logger *log.Logger) {
	if err := d.Advertise(ctx); err != nil {
		logger.Debug("discovery.advertise_failed", "error", err)
	}
	keys := map[string]bool{}
	for _, k := range d.BootstrapKeys() {
		keys[k] = true
	}
	for _, k := range d.DiscoverProviders(ctx, discoveryFanout) {
		keys[k] = true
	}
	for key := range keys {
		raw, err := d.Gossip(ctx, key)
		if err != nil {
			continue // peer offline or unreachable; a later pass retries
		}
		var g kernel.GossipResponse
		if json.Unmarshal(raw, &g) != nil || g.PublicKey == "" {
			continue
		}
		_ = accumulate(ctx, &g, g.PublicKey)
	}
}

// startDiscoveryLoop refreshes the known network (§13) once immediately, then every interval until
// ctx is cancelled. Mirrors startPeerRetentionSweep; the pass is injected so the loop is testable
// without libp2p. Runs are sequential (a tick never overlaps the previous pass).
func startDiscoveryLoop(ctx context.Context, interval time.Duration, pass func(context.Context)) {
	pass(ctx) // one pass at startup
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass(ctx)
		}
	}
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
	fed    *fed.Transport // federation transport (§13); nil until runServer starts it
}

// registerRoutes mounts all application routes onto r for the given server.
// Rate-limited routes (auth, user creation) are registered by the caller before this call.
func registerRoutes(r chi.Router, srv *server) {
	// Health (unauthenticated).
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Federation has no HTTP surface: peer identity, the friend handshake, inbound calls,
	// manifests, gossip, and inspection travel over the libp2p transport (§13), started in
	// runServer. Public action listing stays on HTTP for local/user clients.
	r.Get("/v1/actions", srv.getActions)

	// Actions (authenticated).
	r.Group(func(r chi.Router) {
		r.Use(srv.authMiddleware)
		r.Post("/v1/actions/import", srv.importOpenAPI)
		r.Post("/v1/actions/unimport", srv.unimportOpenAPI)
		r.Post("/v1/actions", srv.postAction)
		r.Get("/v1/actions/{id}", srv.getAction)
		r.Get("/v1/actions/{id}/ratings", srv.listActionRatings)
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
		r.Get("/v1/stats/{action_id}", srv.getStats)

		// Steps.
		r.Get("/v1/steps", srv.listSteps)
		r.Post("/v1/steps", srv.postStep)
		r.Get("/v1/steps/{id}", srv.getStep)
		r.Post("/v1/steps/{id}/complete", srv.postCompleteStep)

		// Current user.
		r.Get("/v1/me", srv.getMe)
		r.Put("/v1/me", srv.putMe)
	})

	// Superuser supervision (money, access, federation trust, roster) — same TCP API, gated
	// per-route by requireSuperuserMW (§14). Not a separate surface; authority is the @sys bearer.
	r.Group(func(r chi.Router) {
		r.Use(srv.authMiddleware, srv.requireSuperuserMW)
		r.Get("/control/users", srv.ctlListUsers)
		r.Get("/control/users/{handle}", srv.ctlShowUser)
		r.Post("/control/users/{handle}/suspend", srv.ctlSetSuspended(true))
		r.Post("/control/users/{handle}/unsuspend", srv.ctlSetSuspended(false))
		r.Post("/control/deposit", srv.ctlAdjust(kernel.DirectionCredit))
		r.Post("/control/withdraw", srv.ctlAdjust(kernel.DirectionDebit))
		r.Get("/control/peers", srv.ctlListPeers)
		r.Get("/control/peers/inspect", srv.ctlInspectPeer)
		r.Post("/control/peers/friend", srv.ctlFriendPeer)
		r.Post("/control/peers/unfriend", srv.ctlUnfriendPeer)
		r.Get("/control/identity", srv.ctlIdentity)
	})
}

// ---- middleware ----

type ctxKey string

const ctxCallerID ctxKey = "caller_id"

// ipRateLimiter returns a middleware that limits requests per IP using a token bucket.
// Entries not seen for 5 minutes are evicted by a background goroutine.
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
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			if ip == "" {
				ip = r.RemoteAddr
			}
			mu.Lock()
			e, ok := entries[ip]
			if !ok {
				e = &entry{lim: rate.NewLimiter(rate.Limit(ratePerSec), int(burst))}
				entries[ip] = e
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
	var req struct {
		Handle   string `json:"handle"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	view, err := createUser(s.kernel, r.Context(), kernel.CreateUserRequest{
		Handle:   req.Handle,
		Email:    req.Email,
		Password: req.Password,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *server) getActions(w http.ResponseWriter, r *http.Request) {
	resps, err := listPublicActions(s.kernel, r.Context(), s.optionalAuth(r),
		r.URL.Query().Get("owner"), r.URL.Query().Get("name"), 200, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resps)
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
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
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
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, actions)
}

func (s *server) postAction(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req struct {
		Name         string             `json:"name"`
		Kind         string             `json:"kind"`
		Price        int64              `json:"price"`
		Description  string             `json:"description"`
		InputSchema  map[string]any     `json:"input_schema"`
		OutputSchema map[string]any     `json:"output_schema"`
		Source       string             `json:"source"`
		Method       string             `json:"method"`
		Params       []kernel.HTTPParam `json:"params"`
		WasmArtifact string             `json:"wasm_artifact"`
		Auth         *kernel.AuthInput  `json:"auth"`
	}) (any, int, error) {
		a, err := createAction(s.kernel, r.Context(), callerFrom(r), kernel.CreateActionRequest{
			OwnerUserID:  callerFrom(r),
			Name:         req.Name,
			Kind:         kernel.ActionKind(req.Kind),
			Price:        req.Price,
			Description:  req.Description,
			InputSchema:  req.InputSchema,
			OutputSchema: req.OutputSchema,
			Source:       req.Source,
			Method:       req.Method,
			Params:       req.Params,
			WasmArtifact: req.WasmArtifact,
			Auth:         req.Auth,
		})
		return a, http.StatusCreated, err
	})(w, r)
}

func (s *server) getAction(w http.ResponseWriter, r *http.Request) {
	a, err := getAction(s.kernel, r.Context(), callerFrom(r), pathID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *server) listActionRatings(w http.ResponseWriter, r *http.Request) {
	ratings, err := s.kernel.ListRatings(r.Context(), pathID(r), 50, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	if ratings == nil {
		ratings = []*kernel.Rating{}
	}
	writeJSON(w, http.StatusOK, ratings)
}

func (s *server) updateAction(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, body struct {
		Price        *int64              `json:"price"`
		Description  *string             `json:"description"`
		Source       *string             `json:"source"`
		Method       *string             `json:"method"`
		Params       *[]kernel.HTTPParam `json:"params"`
		InputSchema  map[string]any      `json:"input_schema"`
		OutputSchema map[string]any      `json:"output_schema"`
		Public       *bool               `json:"public"`
		Auth         *kernel.AuthInput   `json:"auth"`
	}) (any, int, error) {
		a, err := updateAction(s.kernel, r.Context(), callerFrom(r), kernel.UpdateActionRequest{
			ID:           pathID(r),
			Price:        body.Price,
			Description:  body.Description,
			Source:       body.Source,
			Method:       body.Method,
			Params:       body.Params,
			InputSchema:  body.InputSchema,
			OutputSchema: body.OutputSchema,
			Public:       body.Public,
			Auth:         body.Auth,
		})
		return a, http.StatusOK, err
	})(w, r)
}

func (s *server) setActionActive(active bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var err error
		if active {
			err = enableAction(s.kernel, r.Context(), callerFrom(r), pathID(r))
		} else {
			err = disableAction(s.kernel, r.Context(), callerFrom(r), pathID(r))
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"active": active})
	}
}

func (s *server) deleteAction(w http.ResponseWriter, r *http.Request) {
	if err := deleteAction(s.kernel, r.Context(), callerFrom(r), pathID(r)); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listProcesses(w http.ResponseWriter, r *http.Request) {
	processes, err := listProcesses(s.kernel, r.Context(), callerFrom(r), 100, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	if processes == nil {
		processes = []*processView{}
	}
	writeJSON(w, http.StatusOK, processes)
}

func (s *server) getProcess(w http.ResponseWriter, r *http.Request) {
	p, err := getProcess(s.kernel, r.Context(), callerFrom(r), pathID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) endProcess(w http.ResponseWriter, r *http.Request) {
	if err := endProcess(s.kernel, r.Context(), callerFrom(r), pathID(r)); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) postRun(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req struct {
		Action string          `json:"action"`
		Args   *map[string]any `json:"args"`
	}) (any, int, error) {
		if req.Action == "" {
			return nil, 0, kernel.ErrInvalidInput.Wrap("action is required")
		}
		if req.Args == nil {
			return nil, 0, kernel.ErrInvalidInput.Wrap("args is required")
		}
		reply, err := run(s.kernel, r.Context(), callerFrom(r), req.Action, *req.Args)
		return reply, http.StatusOK, err
	})(w, r)
}

func (s *server) listTransactions(w http.ResponseWriter, r *http.Request) {
	txs, err := listTransactions(s.kernel, r.Context(), callerFrom(r), kernel.TxFilter{
		ProcessID: r.URL.Query().Get("process_id"),
		Limit:     50,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if txs == nil {
		txs = []*kernel.TransactionView{}
	}
	writeJSON(w, http.StatusOK, txs)
}

func (s *server) getTransaction(w http.ResponseWriter, r *http.Request) {
	tx, err := getTransaction(s.kernel, r.Context(), callerFrom(r), pathID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tx)
}

func (s *server) rateTransaction(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req struct {
		Rating float64 `json:"rating"`
		Note   *string `json:"note"`
	}) (any, int, error) {
		rating, err := rateTransaction(s.kernel, r.Context(), callerFrom(r), pathID(r), req.Rating, req.Note)
		return rating, http.StatusOK, err
	})(w, r)
}

func (s *server) getReceiptVerification(w http.ResponseWriter, r *http.Request) {
	v, err := verifyReceipt(s.kernel, r.Context(), callerFrom(r), pathID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) getStats(w http.ResponseWriter, r *http.Request) {
	stats, err := actionStats(s.kernel, r.Context(), chi.URLParam(r, "action_id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
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
		writeErr(w, kernel.ErrUnauthenticated.Wrap("refresh_token required"))
		return
	}
	if err := s.kernel.RevokeRefreshToken(r.Context(), req.RefreshToken); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postTokenMulti handles POST /v1/auth/token (JSON only).
// grant_type "authorization_code" exchanges a PKCE code for tokens;
// all other values (or omitted) are treated as password grant.
func (s *server) postTokenMulti(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GrantType    string `json:"grant_type"`
		Handle       string `json:"handle"`
		Password     string `json:"password"`
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		RedirectURI  string `json:"redirect_uri"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.GrantType == "authorization_code" {
		access, refresh, err := s.kernel.ExchangeAuthCode(r.Context(), req.Code, req.CodeVerifier, req.RedirectURI)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"access_token": access, "refresh_token": refresh})
		return
	}
	tok, err := s.kernel.Login(r.Context(), req.Handle, req.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// ---- Step handlers ----

func (s *server) listSteps(w http.ResponseWriter, r *http.Request) {
	views, err := listSteps(s.kernel, r.Context(), callerFrom(r),
		r.URL.Query().Get("process_id"), r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *server) postStep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TraceID        string          `json:"trace_id"`
		ActionID       string          `json:"action_id"`
		PartialArgs    json.RawMessage `json:"partial_args"`
		RequiredCaller string          `json:"required_caller"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.TraceID == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("trace_id is required"))
		return
	}
	if req.ActionID == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("action_id is required"))
		return
	}
	if req.RequiredCaller == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("required_caller is required"))
		return
	}
	if req.PartialArgs == nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("partial_args is required"))
		return
	}
	req.RequiredCaller = kernel.NormalizeHandle(req.RequiredCaller)
	if req.RequiredCaller == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("required_caller is required"))
		return
	}
	view, err := createStep(s.kernel, r.Context(), callerFrom(r), createStepParams{
		TraceID:        req.TraceID,
		ActionRef:      req.ActionID,
		RequiredCaller: req.RequiredCaller,
		PartialArgs:    req.PartialArgs,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *server) getStep(w http.ResponseWriter, r *http.Request) {
	step, err := getStep(s.kernel, r.Context(), callerFrom(r), pathID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, step)
}

func (s *server) postCompleteStep(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req struct {
		Args *json.RawMessage `json:"args"`
	}) (any, int, error) {
		if req.Args == nil {
			return nil, 0, kernel.ErrInvalidInput.Wrap("args is required")
		}
		reply, err := completeStep(s.kernel, r.Context(), callerFrom(r), pathID(r), *req.Args)
		return reply, http.StatusOK, err
	})(w, r)
}

// ---- me ----

func (s *server) getMe(w http.ResponseWriter, r *http.Request) {
	view, err := getMe(s.kernel, r.Context(), callerFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *server) putMe(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, body struct {
		Email           string `json:"email"`
		CurrentPassword string `json:"current_password"`
		Password        string `json:"password"`
	}) (any, int, error) {
		view, err := updateMe(s.kernel, r.Context(), callerFrom(r), body.Email, body.CurrentPassword, body.Password)
		return view, http.StatusOK, err
	})(w, r)
}

// ---- health command ----

func healthCmd() *cobra.Command {
	var healthURL string
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check server health",
		RunE: func(_ *cobra.Command, _ []string) error {
			resp, err := http.Get(healthURL + "/health") //nolint:noctx
			if err != nil {
				return kernel.ErrInvalidState.
					Wrapf("cannot reach juice server at %s (is `juice serve` running?)", healthURL).
					Because(err)
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
			fmt.Println("ok")
			return nil
		},
	}
	cmd.Flags().StringVar(&healthURL, "url", "http://localhost:4040", "Server base URL")
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
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": fmt.Sprintf("%v", err),
		"code":  kernel.KernelErrorCode(err),
	})
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
	handlers := &fedHandlers{kernel: k, log: logger}
	tr, err := fed.New(ctx, fed.Config{
		SigningKey:        ed25519.PrivateKey(privBytes),
		BootstrapPeers:    globalCfg.BootstrapPeers,
		Handlers:          handlers,
		AllowPrivateAddrs: globalCfg.AllowLocalSources,
	})
	if err != nil {
		return nil, err
	}
	// Back-reference so inbound auto-accept can reciprocate over the same transport.
	handlers.transport = tr
	return tr, nil
}

// fedHandlers answers inbound federation protocol streams.
type fedHandlers struct {
	kernel    *kernel.Kernel
	log       *log.Logger
	transport *fed.Transport // set after New so reciprocal friend requests can go out
}

// OnCall verifies and executes an inbound federation call, returning the settlement envelope.
func (h *fedHandlers) OnCall(ctx context.Context, _ string, req fed.CallRequest) fed.CallResponse {
	status, body, err := handleFederationCall(h.kernel, ctx, req.Counterparty, req.Timestamp,
		req.IdempotencyKey, req.Action, req.Signature, []byte(req.Args))
	if err != nil {
		// No receipt to settle on → the caller treats this as pending (retry), exactly as the
		// HTTP path did when it returned an error status with no receipt body.
		b, _ := json.Marshal(map[string]string{"error": err.Error(), "code": kernel.KernelErrorCode(err)})
		return fed.CallResponse{Status: kernel.HTTPStatusFromCode(kernel.KernelErrorCode(err)), Body: b}
	}
	b, _ := json.Marshal(body)
	return fed.CallResponse{Status: status, Body: b}
}

// OnFriend verifies and accepts (or holds pending) an inbound friend request.
func (h *fedHandlers) OnFriend(ctx context.Context, _ string, req fed.FriendRequest) fed.FriendResponse {
	ts, err := time.Parse(time.RFC3339, req.Timestamp)
	if err != nil {
		return fed.FriendResponse{Status: "rejected", Error: "timestamp must be RFC3339"}
	}
	if diff := time.Since(ts); diff < -5*time.Minute || diff > 5*time.Minute {
		return fed.FriendResponse{Status: "rejected", Error: "timestamp out of range"}
	}
	if err := kernel.VerifyPeerRequestSignature(req.PublicKey, req.Handle, req.PublicKey, req.Timestamp, req.Signature); err != nil {
		return fed.FriendResponse{Status: "rejected", Error: "invalid signature"}
	}
	existing, _ := h.kernel.ReadUserByPublicKey(ctx, req.PublicKey)
	if existing != nil && existing.DeniedAt != nil {
		return fed.FriendResponse{Status: "rejected", Error: "peer is denied"}
	}
	if !globalCfg.PeerAutoAccept {
		_ = h.kernel.AccumulateGossip(ctx, &kernel.GossipResponse{PublicKey: req.PublicKey, Handle: req.Handle}, "friend-request")
		return fed.FriendResponse{Status: "pending"}
	}
	if _, err := h.kernel.CreateOrUpdateProxyPeer(ctx, req.Handle, req.PublicKey); err != nil {
		return fed.FriendResponse{Status: "rejected", Error: err.Error()}
	}
	// Reciprocate only for a previously-unknown peer, so two auto-accepting kernels don't
	// ping-pong friend requests forever.
	if existing == nil && h.transport != nil {
		go h.sendReciprocal(req.PublicKey)
	}
	return fed.FriendResponse{Status: "accepted"}
}

// OnManifest returns one signed manifest per active public action (chunked, relay-safe).
func (h *fedHandlers) OnManifest(ctx context.Context, _ string) ([]json.RawMessage, error) {
	actions, err := h.kernel.ListPublicActions(ctx, 200, 0)
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, a := range actions {
		if !a.Active {
			continue
		}
		m, err := h.kernel.GetActionManifest(ctx, a.ID)
		if err != nil {
			continue
		}
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// OnGossip returns the gossip document (§13).
func (h *fedHandlers) OnGossip(ctx context.Context, _ string) (json.RawMessage, error) {
	g, err := h.kernel.GetGossip(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(g)
}

// OnInspect returns identity + public actions + transacted friends. Gossip already carries all
// three, so the inspect document is the gossip document viewed by a prospective friend.
func (h *fedHandlers) OnInspect(ctx context.Context, peerKey string) (json.RawMessage, error) {
	return h.OnGossip(ctx, peerKey)
}

// sendReciprocal sends a signed friend request back to a peer by key over the transport.
func (h *fedHandlers) sendReciprocal(peerKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pubKeyB64, _ := h.kernel.GetConfig(ctx, configKeySigningPublic)
	localHandle := globalCfg.KernelHandle
	if pubKeyB64 == "" {
		return
	}
	sig, ts, err := h.kernel.SignPeerRequestNow(localHandle, pubKeyB64)
	if err != nil {
		return
	}
	_, _ = h.transport.Friend(ctx, peerKey, fed.FriendRequest{
		Handle: localHandle, PublicKey: pubKeyB64, Timestamp: ts, Signature: sig,
	})
}
