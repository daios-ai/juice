package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

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
		Short: "Start the HTTP API server",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runServer(addr)
		},
	}
	serveCmd.Flags().StringVar(&addr, "addr", ":4040", "Listen address")
	rootCmd.AddCommand(serveCmd)

	rootCmd.AddCommand(healthCmd())
}

func runServer(addr string) error {
	k, db, logger, err := openKernel()
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
	// Under --addr :0 the real port is only known now; advertise it so federation
	// (.well-known, gossip, reciprocal) reports where we actually listen. A configured
	// server_url (e.g. a public proxy URL) takes precedence and is left as bootstrap set it.
	if globalCfg.ServerURL == "" {
		_ = k.SetConfig(context.Background(), "kernel_base_url", "http://"+ln.Addr().String())
	}
	// server.ready is emitted only after a successful bind — the harness waits on this line
	// (and reads the real addr from it) instead of blind-polling /health.
	logger.Info("server.ready", "addr", ln.Addr().String())

	// Superuser supervision (admin/peer) is served on a local Unix socket, never TCP, so
	// `serve` is the sole process that opens the DB (§14).
	if control, cerr := startControlPlane(srv, flagDB); cerr != nil {
		logger.Error("control.start_failed", "error", cerr)
	} else {
		defer control.Close()
	}

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

// ---- server ----

type server struct {
	kernel *kernel.Kernel
	log    *log.Logger
}

// registerRoutes mounts all application routes onto r for the given server.
// Rate-limited routes (auth, user creation) are registered by the caller before this call.
func registerRoutes(r chi.Router, srv *server) {
	// Health (unauthenticated).
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Well-known kernel metadata (unauthenticated).
	r.Get("/.well-known/juice-kernel.json", srv.getWellKnown)

	// Federation endpoints (unauthenticated).
	r.Post("/v1/federation/call", srv.postFederationCall)
	r.Get("/v1/gossip", srv.getGossip)
	r.Post("/v1/peers", srv.postPeer)

	// Public action routes — no auth required.
	r.Get("/v1/actions", srv.getActions)
	r.Get("/v1/actions/{id}/manifest", srv.getActionManifest)

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
		processes = []*kernel.Process{}
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


// ---- well-known / federation ----

func (s *server) getWellKnown(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pubKey, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	handle := globalCfg.PeerHandle
	if handle == "" {
		handle, _ = s.kernel.GetConfig(ctx, configKeySuperuser)
	}
	if handle == "" {
		handle = "@sys"
	}
	// kernel_base_url is the address we actually advertise: the configured server_url, or
	// the real bound address under --addr :0 (set in runServer).
	baseURL, _ := s.kernel.GetConfig(ctx, "kernel_base_url")
	writeJSON(w, http.StatusOK, map[string]string{
		"handle":     handle,
		"public_key": pubKey,
		"base_url":   baseURL,
	})
}

func (s *server) getGossip(w http.ResponseWriter, r *http.Request) {
	gossip, err := s.kernel.GetGossip(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gossip)
}

// postPeer handles inbound friend requests from remote kernels.
// Body: {handle, public_key, base_url, timestamp, signature}
// signature = SignPeerRequest({handle, public_key, base_url, timestamp}) by the requester.
func (s *server) postPeer(w http.ResponseWriter, r *http.Request) {
	handle(func(r *http.Request, req struct {
		Handle    string `json:"handle"`
		PublicKey string `json:"public_key"`
		BaseURL   string `json:"base_url"`
		Timestamp string `json:"timestamp"`
		Signature string `json:"signature"`
	}) (any, int, error) {
		ctx := r.Context()

		// Verify timestamp (±5 min).
		ts, err := time.Parse(time.RFC3339, req.Timestamp)
		if err != nil {
			return nil, 0, kernel.ErrInvalidInput.Wrap("timestamp must be RFC3339")
		}
		if diff := time.Since(ts); diff < -5*time.Minute || diff > 5*time.Minute {
			return nil, 0, kernel.ErrUnauthenticated.Wrap("timestamp out of range")
		}

		// Verify Ed25519 signature against the public key embedded in the request.
		if err := kernel.VerifyPeerRequestSignature(req.PublicKey, req.Handle, req.PublicKey, req.BaseURL, req.Timestamp, req.Signature); err != nil {
			return nil, 0, kernel.ErrUnauthorized.Wrap("invalid peer request signature")
		}

		// Deny check; capture existing peer so we can skip reciprocal if already known.
		existing, _ := s.kernel.ReadUserByPublicKey(ctx, req.PublicKey)
		if existing != nil && existing.DeniedAt != nil {
			return nil, 0, kernel.ErrUnauthorized.Wrap("peer is denied")
		}

		if !globalCfg.PeerAutoAccept {
			_ = s.kernel.AccumulateGossip(ctx, &kernel.GossipResponse{
				PublicKey: req.PublicKey,
				Handle:    req.Handle,
				BaseURL:   req.BaseURL,
			}, "friend-request")
			return map[string]any{"status": "pending"}, http.StatusAccepted, nil
		}

		u, err := s.kernel.CreateOrUpdateProxyPeer(ctx, req.Handle, req.PublicKey, req.BaseURL)
		if err != nil {
			return nil, 0, err
		}
		// Only send a reciprocal friend request if the peer was previously unknown.
		// This prevents mutual sendReciprocal cascades where each server keeps responding
		// to the other's reciprocal, flooding both DBs with concurrent writes.
		if existing == nil {
			go s.sendReciprocal(req.BaseURL)
		}
		return map[string]any{"id": u.ID, "handle": u.Handle}, http.StatusOK, nil
	})(w, r)
}

// sendReciprocal sends a signed friend request back to peerBaseURL/v1/peers. Best-effort.
func (s *server) sendReciprocal(peerBaseURL string) {
	ctx := context.Background()
	pubKeyB64, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	localHandle := globalCfg.PeerHandle
	if localHandle == "" {
		localHandle, _ = s.kernel.GetConfig(ctx, configKeySuperuser)
	}
	localBaseURL, _ := s.kernel.GetConfig(ctx, "kernel_base_url")
	if pubKeyB64 == "" || localBaseURL == "" {
		return
	}
	sig, ts, err := s.kernel.SignPeerRequestNow(localHandle, pubKeyB64, localBaseURL)
	if err != nil {
		return
	}
	body, _ := json.Marshal(map[string]string{
		"handle":     localHandle,
		"public_key": pubKeyB64,
		"base_url":   localBaseURL,
		"timestamp":  ts,
		"signature":  sig,
	})
	exec := &httpActionExecutor{timeout: 10 * time.Second, allowLocal: globalCfg.AllowLocalSources}
	_, _, _ = doHTTP(ctx, http.MethodPost, strings.TrimRight(peerBaseURL, "/")+"/v1/peers",
		map[string]string{"Content-Type": "application/json"}, strings.NewReader(string(body)),
		exec.timeout, exec.allowLocal)
}

func (s *server) postFederationCall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cpPubKey := r.URL.Query().Get("counterparty")
	if cpPubKey == "" {
		writeErr(w, kernel.ErrUnauthenticated.Wrap("counterparty required"))
		return
	}
	tsStr := r.Header.Get("X-Timestamp")
	if tsStr == "" {
		writeErr(w, kernel.ErrUnauthenticated.Wrap("X-Timestamp required"))
		return
	}
	idempotencyKey := r.Header.Get("X-Idempotency-Key")
	if idempotencyKey == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("X-Idempotency-Key required"))
		return
	}
	actionParam := r.URL.Query().Get("action")
	if actionParam == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("action query param required"))
		return
	}
	rawBody, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("could not read request body"))
		return
	}
	sigStr := r.Header.Get("X-Signature")
	status, body, callErr := handleFederationCall(s.kernel, ctx, cpPubKey, tsStr, idempotencyKey, actionParam, sigStr, rawBody)
	if callErr != nil {
		writeErr(w, callErr)
		return
	}
	writeJSON(w, status, body)
}

func (s *server) getActionManifest(w http.ResponseWriter, r *http.Request) {
	m, err := s.kernel.GetActionManifest(r.Context(), pathID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
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
