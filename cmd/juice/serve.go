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
	k, db, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()

	logger, _ := log.New(log.Config{
		Level:    globalCfg.LogLevel,
		FilePath: globalCfg.LogFile,
		Format:   globalCfg.LogFormat,
	})

	if err := bootstrap(k); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if err := k.ValidateFeeRecipient(context.Background()); err != nil {
		return fmt.Errorf("startup: %w", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestIDMiddleware)
	r.Use(loggingMiddleware(logger))
	r.Use(maxBytesMiddleware(1 << 20)) // 1 MiB request body limit

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

	logger.Info("server.start", "addr", addr)

	httpSrv := &http.Server{Addr: addr, Handler: r}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
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

	// Federation call endpoint (unauthenticated; action must be public).
	r.Post("/v1/federation/call", srv.postFederationCall)

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
		r.Post("/v1/processes", srv.postProcess)
		r.Get("/v1/processes/{id}", srv.getProcess)
		r.Post("/v1/processes/{id}/fund", srv.fundProcess)
		r.Post("/v1/processes/{id}/end", srv.endProcess)

		// Calls.
		r.Post("/v1/call", srv.postCall)

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
	})
}

// ---- middleware ----

type ctxKey string

const ctxCallerID ctxKey = "caller_id"

// ipRateLimiter returns a middleware that limits requests from each IP address
// using a token bucket: ratePerSec tokens refilled per second, burst maximum tokens.
// Entries not seen for 5 minutes are evicted by a background goroutine.
func ipRateLimiter(ratePerSec, burst float64) func(http.Handler) http.Handler {
	type entry struct {
		tokens   float64
		lastFill time.Time
		lastSeen time.Time
	}
	var mu sync.Mutex
	entries := make(map[string]*entry)

	// Background cleanup goroutine.
	go func() {
		ticker := time.NewTicker(time.Minute)
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

	allow := func(ip string) bool {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		e, ok := entries[ip]
		if !ok {
			entries[ip] = &entry{tokens: burst - 1, lastFill: now, lastSeen: now}
			return true
		}
		elapsed := now.Sub(e.lastFill).Seconds()
		e.tokens = min(burst, e.tokens+elapsed*ratePerSec)
		e.lastFill = now
		e.lastSeen = now
		if e.tokens < 1 {
			return false
		}
		e.tokens--
		return true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			if ip == "" {
				ip = r.RemoteAddr
			}
			if !allow(ip) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func maxBytesMiddleware(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
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
	u, err := s.kernel.CreateUser(r.Context(), kernel.CreateUserRequest{
		Handle:   req.Handle,
		Email:    req.Email,
		Password: req.Password,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        u.ID,
		"handle":    u.Handle,
		"email":     u.Email,
		"available": u.Available,
		"locked":    u.Locked,
	})
}

// actionResp wraps an action with the computed @owner/name reference field (R8).
type actionResp struct {
	*kernel.Action
	ActionRef string `json:"action"`
}

func withActionRef(a *kernel.Action) actionResp {
	ref := ""
	if a.OwnerHandle != "" && a.Name != "" {
		ref = a.OwnerHandle + "/" + a.Name
	}
	return actionResp{Action: a, ActionRef: ref}
}

func (s *server) getActions(w http.ResponseWriter, r *http.Request) {
	actions, err := s.kernel.ListPublicActions(r.Context(), 200, 0)
	if err != nil {
		writeErr(w, err)
		return
	}

	if owner := r.URL.Query().Get("owner"); owner != "" {
		u, err := s.kernel.ReadUserByHandle(r.Context(), owner)
		if err != nil {
			writeJSON(w, http.StatusOK, []actionResp{})
			return
		}
		filtered := actions[:0]
		for _, a := range actions {
			if a.OwnerUserID == u.ID {
				filtered = append(filtered, a)
			}
		}
		actions = filtered
	}
	if name := r.URL.Query().Get("name"); name != "" {
		filtered := actions[:0]
		for _, a := range actions {
			if a.Name == name {
				filtered = append(filtered, a)
			}
		}
		actions = filtered
	}
	// Strip execution-internal fields from public discovery; authorized users use
	// the authenticated get-by-id endpoint to retrieve source and artifact data.
	resps := make([]actionResp, len(actions))
	for i, a := range actions {
		cp := *a
		cp.Source = ""
		cp.ArtifactHash = ""
		resps[i] = withActionRef(&cp)
	}
	if resps == nil {
		resps = []actionResp{}
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
	var req struct {
		Name         string         `json:"name"`
		Kind         string         `json:"kind"`
		Price        int64          `json:"price"`
		Description  string         `json:"description"`
		InputSchema  map[string]any `json:"input_schema"`
		OutputSchema map[string]any `json:"output_schema"`
		Source       string         `json:"source"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	a, err := s.kernel.CreateAction(r.Context(), callerFrom(r), kernel.CreateActionRequest{
		OwnerUserID:  callerFrom(r),
		Name:         req.Name,
		Kind:         kernel.ActionKind(req.Kind),
		Price:        req.Price,
		Description:  req.Description,
		InputSchema:  req.InputSchema,
		OutputSchema: req.OutputSchema,
		Source:       req.Source,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// Re-read to populate OwnerHandle via store JOIN.
	full, err := s.kernel.ReadActionForSubject(r.Context(), callerFrom(r), a.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, withActionRef(full))
}

func (s *server) getAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := s.kernel.ReadActionForSubject(r.Context(), callerFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, withActionRef(a))
}

func (s *server) listActionRatings(w http.ResponseWriter, r *http.Request) {
	actionID := chi.URLParam(r, "id")
	ratings, err := s.kernel.ListRatings(r.Context(), actionID, 50, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ratings)
}

func (s *server) updateAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Price        *int64         `json:"price"`
		Description  *string        `json:"description"`
		Source       *string        `json:"source"`
		InputSchema  map[string]any `json:"input_schema"`
		OutputSchema map[string]any `json:"output_schema"`
		Public       *bool          `json:"public"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	a, err := s.kernel.UpdateAction(r.Context(), callerFrom(r), kernel.UpdateActionRequest{
		ID:           id,
		Price:        body.Price,
		Description:  body.Description,
		Source:       body.Source,
		InputSchema:  body.InputSchema,
		OutputSchema: body.OutputSchema,
		Public:       body.Public,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, withActionRef(a))
}

func (s *server) setActionActive(active bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := s.kernel.SetActive(r.Context(), callerFrom(r), id, active); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"active": active})
	}
}

func (s *server) deleteAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.kernel.DeleteAction(r.Context(), callerFrom(r), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listProcesses(w http.ResponseWriter, r *http.Request) {
	processes, err := s.kernel.ListProcesses(r.Context(), callerFrom(r), 100, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, processes)
}

func (s *server) postProcess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Funds int64 `json:"funds"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	p, t, err := s.kernel.StartProcess(r.Context(), callerFrom(r), callerFrom(r), req.Funds)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"process_id": p.ID,
		"trace_id":   t.ID,
		"available":  p.Available,
	})
}

func (s *server) getProcess(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, err := s.kernel.ReadProcess(r.Context(), callerFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) fundProcess(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Funds int64 `json:"funds"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.kernel.FundProcess(r.Context(), callerFrom(r), id, req.Funds); err != nil {
		writeErr(w, err)
		return
	}
	p, err := s.kernel.ReadProcess(r.Context(), callerFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) endProcess(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.kernel.EndProcess(r.Context(), callerFrom(r), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) postCall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessID     string         `json:"process_id"`
		ParentTraceID string         `json:"parent_trace_id"`
		Action        string         `json:"action"`
		Args          map[string]any `json:"args"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Action == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("action is required"))
		return
	}
	if req.Args == nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("args is required"))
		return
	}
	reply, err := s.kernel.Call(r.Context(), kernel.CallRequest{
		CallerID:      callerFrom(r),
		ProcessID:     req.ProcessID,
		ParentTraceID: req.ParentTraceID,
		ActionRef:     req.Action,
		Args:          req.Args,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}


func (s *server) listTransactions(w http.ResponseWriter, r *http.Request) {
	callerID := callerFrom(r)
	txs, err := s.kernel.ListTransactions(r.Context(), callerID, kernel.TxFilter{
		ProcessID: r.URL.Query().Get("process_id"),
		Limit:     50,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, txs)
}


func (s *server) getTransaction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tx, err := s.kernel.ReadTransaction(r.Context(), callerFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tx)
}

func (s *server) rateTransaction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Rating float64 `json:"rating"`
		Note   *string `json:"note"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Rating != 0 && req.Rating != 1 {
		writeErr(w, kernel.ErrInvalidInput.Wrap("rating must be 0 or 1"))
		return
	}
	rating, err := s.kernel.RateTransaction(r.Context(), callerFrom(r), id, req.Rating, req.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rating)
}

func (s *server) getReceiptVerification(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	v, err := s.kernel.VerifyRemoteReceipt(r.Context(), callerFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) getStats(w http.ResponseWriter, r *http.Request) {
	actionID := chi.URLParam(r, "action_id")
	stats, err := s.kernel.ReadStats(r.Context(), actionID)
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
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	access, newRT, err := s.kernel.RefreshAccessToken(r.Context(), req.RefreshToken)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"access_token":  access,
		"refresh_token": newRT,
	})
}

func (s *server) postLogout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decodeBody(w, r, &req) {
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
	processID := r.URL.Query().Get("process_id")
	status := r.URL.Query().Get("status")
	steps, err := s.kernel.ListSteps(r.Context(), callerFrom(r), processID, status)
	if err != nil {
		writeErr(w, err)
		return
	}
	if steps == nil {
		steps = []*kernel.Step{}
	}
	writeJSON(w, http.StatusOK, steps)
}

func (s *server) postStep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessID       string          `json:"process_id"`
		Action          string          `json:"action"`
		PartialArgs     json.RawMessage `json:"partial_args"`
		InputSchema     json.RawMessage `json:"input_schema"`
		RequiredCaller  string          `json:"required_caller"`
		ParentTraceID   string          `json:"parent_trace_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ProcessID == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("process_id is required"))
		return
	}
	if req.Action == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("action is required"))
		return
	}
	if req.RequiredCaller == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("required_caller is required"))
		return
	}
	// Resolve @owner/name → actionID.
	ownerHandle, actionName, err := kernel.ParseActionRef(req.Action)
	if err != nil {
		writeErr(w, err)
		return
	}
	owner, err := s.kernel.ReadUserByHandle(r.Context(), ownerHandle)
	if err != nil {
		writeErr(w, kernel.ErrNotFound.Wrap("action owner not found"))
		return
	}
	action, err := s.kernel.ReadActionByOwnerName(r.Context(), owner.ID, actionName)
	if err != nil {
		writeErr(w, kernel.ErrNotFound.Wrap("action not found"))
		return
	}
	// Resolve @handle → userID for required_caller.
	requiredCallerHandle := req.RequiredCaller
	if !strings.HasPrefix(requiredCallerHandle, "@") {
		writeErr(w, kernel.ErrInvalidInput.Wrap("required_caller must be @handle"))
		return
	}
	requiredCallerUser, err := s.kernel.ReadUserByHandle(r.Context(), requiredCallerHandle)
	if err != nil {
		writeErr(w, kernel.ErrNotFound.Wrap("required_caller not found"))
		return
	}
	var parentTraceID *string
	if req.ParentTraceID != "" {
		parentTraceID = &req.ParentTraceID
	}
	step, err := s.kernel.CreateStep(r.Context(), callerFrom(r), req.ProcessID, parentTraceID,
		action.ID, req.PartialArgs, req.InputSchema, requiredCallerUser.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, stepView(step, action))
}

func (s *server) getStep(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	step, err := s.kernel.ReadStep(r.Context(), callerFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	action, _ := s.kernel.ReadAction(r.Context(), step.NextActionID)
	writeJSON(w, http.StatusOK, stepView(step, action))
}

func (s *server) postCompleteStep(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Args *json.RawMessage `json:"args"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Args == nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("args is required"))
		return
	}
	reply, err := s.kernel.CompleteStep(r.Context(), callerFrom(r), id, *req.Args)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// stepView adds a computed action field to a step response.
type stepWithAction struct {
	*kernel.Step
	Action string `json:"action,omitempty"`
}

func stepView(step *kernel.Step, action *kernel.Action) *stepWithAction {
	v := &stepWithAction{Step: step}
	if action != nil {
		v.Action = "@" + action.OwnerHandle + "/" + action.Name
	}
	return v
}

// ---- well-known / federation ----

func (s *server) getWellKnown(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pubKey, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	handle, _ := s.kernel.GetConfig(ctx, configKeySuperuser)
	if handle == "" {
		handle = "@sys"
	}
	baseURL := globalCfg.ServerURL
	writeJSON(w, http.StatusOK, map[string]string{
		"handle":     handle,
		"public_key": pubKey,
		"base_url":   baseURL,
	})
}

func (s *server) postFederationCall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. Require a registered counterparty, identified by its base64url Ed25519 public key.
	cpPubKey := r.URL.Query().Get("counterparty")
	if cpPubKey == "" {
		writeErr(w, kernel.ErrUnauthenticated.Wrap("counterparty required"))
		return
	}
	counterparty, err := s.kernel.ReadUserByPublicKey(ctx, cpPubKey)
	if err != nil || counterparty.RemoteBaseURL == "" {
		writeErr(w, kernel.ErrUnauthenticated.Wrap("counterparty not a registered peer"))
		return
	}

	// 2. Require X-Timestamp within ±5 minutes.
	tsStr := r.Header.Get("X-Timestamp")
	if tsStr == "" {
		writeErr(w, kernel.ErrUnauthenticated.Wrap("X-Timestamp required"))
		return
	}
	ts, parseErr := time.Parse(time.RFC3339, tsStr)
	if parseErr != nil {
		writeErr(w, kernel.ErrUnauthenticated.Wrap("X-Timestamp must be RFC3339"))
		return
	}
	diff := time.Since(ts)
	if diff < -5*time.Minute || diff > 5*time.Minute {
		writeErr(w, kernel.ErrUnauthenticated.Wrap("X-Timestamp out of range"))
		return
	}

	// 3. Require X-Idempotency-Key.
	idempotencyKey := r.Header.Get("X-Idempotency-Key")
	if idempotencyKey == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("X-Idempotency-Key required"))
		return
	}

	// 4. Require action param (needed for signature verification).
	actionParam := r.URL.Query().Get("action")
	if actionParam == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("action query param required"))
		return
	}

	// 5. Read raw body so we can verify args_hash before decoding.
	rawBody, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("could not read request body"))
		return
	}
	argsHash := sha256HexBytes(rawBody)

	// 6. Verify Ed25519 signature (covers action, args_hash, counterparty, idempotency_key, timestamp).
	sigStr := r.Header.Get("X-Signature")
	if verifyErr := kernel.VerifyFederationSignature(counterparty.PublicKey, actionParam, cpPubKey, idempotencyKey, tsStr, argsHash, sigStr); verifyErr != nil {
		writeErr(w, verifyErr)
		return
	}

	ownerHandle, actionName, err := kernel.ParseActionRef(actionParam)
	if err != nil {
		writeErr(w, err)
		return
	}

	var args map[string]any
	if err := json.Unmarshal(rawBody, &args); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}

	// Resolve the action owner and the action — must be active and public.
	owner, err := s.kernel.ReadUserByHandle(ctx, ownerHandle)
	if err != nil || owner == nil {
		writeErr(w, kernel.ErrNotFound.Wrap("action owner not found"))
		return
	}
	action, err := s.kernel.ReadActionByOwnerName(ctx, owner.ID, actionName)
	if err != nil || action == nil {
		writeErr(w, kernel.ErrNotFound.Wrapf("action %s not found", actionParam))
		return
	}
	if !action.Active || !action.Public {
		writeErr(w, kernel.ErrUnauthorized.Wrap("action is not active and public"))
		return
	}

	// 6. Pre-execution idempotency: INSERT pending record.
	now := time.Now().UTC()
	rec := &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     idempotencyKey,
		CounterpartyUserID: counterparty.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	if insertErr := s.kernel.InsertPendingIdempotencyRecord(ctx, rec); insertErr != nil {
		// Unique conflict: key already exists.
		existing, readErr := s.kernel.GetIdempotencyRecord(ctx, idempotencyKey, counterparty.ID)
		if readErr == nil {
			if existing.Status == "complete" {
				var result map[string]any
				_ = json.Unmarshal([]byte(existing.ResultJSON), &result)
				var receipt *kernel.Receipt
				if existing.ReceiptJSON != "" {
					_ = json.Unmarshal([]byte(existing.ReceiptJSON), &receipt)
				}
				if _, isErr := result["error"]; isErr {
					code, _ := result["code"].(string)
					writeJSON(w, kernel.HTTPStatusFromCode(code), map[string]any{"result": result, "receipt": receipt})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"result": result, "receipt": receipt})
				return
			}
			// status == "pending": duplicate in-flight
			writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate in flight"})
			return
		}
		writeErr(w, kernel.ErrInvalidState.Wrap("idempotency check failed"))
		return
	}

	// Execute.
	proc, _, err := s.kernel.StartProcess(ctx, counterparty.ID, counterparty.ID, action.Price)
	if err != nil {
		// No call was attempted; safe to delete the pending record.
		_ = s.kernel.DeleteIdempotencyRecord(ctx, rec.ID)
		writeErr(w, err)
		return
	}
	defer s.kernel.EndProcess(ctx, counterparty.ID, proc.ID)

	reply, callErr := s.kernel.Call(ctx, kernel.CallRequest{
		CallerID:            counterparty.ID,
		ProcessID:           proc.ID,
		TargetUserID:        owner.ID,
		ActionName:          actionName,
		Args:                args,
		IdempotencyRecordID: rec.ID,
	})
	if callErr != nil {
		// Complete the pending record so replays return the error instead of 409.
		// If CommitFailedCall already completed it, this is a no-op (AND status='pending' guard).
		errJSON, _ := json.Marshal(map[string]string{
			"error": callErr.Error(),
			"code":  kernel.KernelErrorCode(callErr),
		})
		_ = s.kernel.CompleteIdempotencyRecordIfPending(ctx, rec.ID, string(errJSON), "")
		writeErr(w, callErr)
		return
	}

	var receipt *kernel.Receipt
	if reply.ReceiptID != "" {
		receipt, _ = s.kernel.GetReceiptByID(ctx, reply.ReceiptID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": reply.Result, "receipt": receipt})
}

func (s *server) getActionManifest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	m, err := s.kernel.GetActionManifest(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ---- me ----

func (s *server) getMe(w http.ResponseWriter, r *http.Request) {
	u, err := s.kernel.ReadUser(r.Context(), callerFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        u.ID,
		"handle":    u.Handle,
		"email":     u.Email,
		"available": u.Available,
		"locked":    u.Locked,
	})
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
				return fmt.Errorf("server unreachable: %w", err)
			}
			defer resp.Body.Close()
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("server returned %d", resp.StatusCode)
			}
			if flagOutput == "json" {
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

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeJSON(r.Body, v); err != nil {
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
	})
}
