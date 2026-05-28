package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
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
	serveCmd.Flags().StringVar(&addr, "addr", envOr("JUICE_ADDR", ":8080"), "Listen address")
	rootCmd.AddCommand(serveCmd)
}

func runServer(addr string) error {
	k, db, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()

	logger, _ := log.New(log.Config{
		Level:    envOr("JUICE_LOG_LEVEL", "info"),
		FilePath: envOr("JUICE_LOG_FILE", ""),
		Format:   "text",
	})

	if err := bootstrap(k); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestIDMiddleware)
	r.Use(loggingMiddleware(logger))
	r.Use(maxBytesMiddleware(1 << 20)) // 1 MiB request body limit

	srv := &server{kernel: k, log: logger}

	// Health (unauthenticated).
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Well-known kernel metadata (unauthenticated).
	r.Get("/.well-known/juice-kernel.json", srv.getWellKnown)

	// Federation call endpoint (unauthenticated; action must be public).
	r.Post("/v1/federation/call", srv.postFederationCall)

	// Auth — rate limited: 5 requests/minute per IP, burst of 10.
	authLimiter := ipRateLimiter(5.0/60, 10)
	r.With(authLimiter).Post("/v1/auth/token", srv.postTokenMulti)
	r.With(authLimiter).Post("/v1/auth/authorize", srv.postAuthorize)
	r.With(authLimiter).Post("/v1/auth/refresh", srv.postRefresh)
	r.With(authLimiter).Post("/v1/auth/logout", srv.postLogout)

	// Users — rate limited: 3 requests/minute per IP, burst of 5.
	r.With(ipRateLimiter(3.0/60, 5)).Post("/v1/users", srv.postUser)

	// Public action listing — no auth required (only returns grant-all active actions).
	r.Get("/v1/actions", srv.getActions)

	// Actions (authenticated).
	r.Group(func(r chi.Router) {
		r.Use(srv.authMiddleware)
		r.Post("/v1/actions", srv.postAction)
		r.Get("/v1/actions/{id}", srv.getAction)
		r.Put("/v1/actions/{id}", srv.updateAction)
		r.Post("/v1/actions/{id}/enable", srv.enableAction)
		r.Post("/v1/actions/{id}/disable", srv.disableAction)
		r.Delete("/v1/actions/{id}", srv.deleteAction)
		r.Post("/v1/actions/{id}/acl", srv.grantACL)
		r.Delete("/v1/actions/{id}/acl", srv.revokeACL)
		r.Post("/v1/actions/{id}/grant-all", srv.grantAll)
		r.Post("/v1/actions/{id}/revoke-all", srv.revokeAll)
		r.Get("/v1/actions/{id}/manifest", srv.getActionManifest)

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

		// Stats.
		r.Get("/v1/stats/{action_id}", srv.getStats)

		// Listeners & Events.
		r.Get("/v1/listeners", srv.listListeners)
		r.Post("/v1/listeners", srv.postListener)
		r.Get("/v1/listeners/{id}", srv.getListenerMeta)
		r.Get("/v1/listeners/{id}/events", srv.pollListenerEvents)
		r.Delete("/v1/listeners/{id}", srv.deleteListener)
		r.Post("/v1/events/emit", srv.postEmit)
		r.Post("/v1/events/{id}/consume", srv.postConsumeEvent)

		// Current user.
		r.Get("/v1/me", srv.getMe)
	})

	logger.Info("server.start", "addr", addr)
	return http.ListenAndServe(addr, r)
}

// ---- server ----

type server struct {
	kernel *kernel.Kernel
	log    *log.Logger
}

// ---- middleware ----

type ctxKey string

const ctxSubjectID ctxKey = "subject_id"

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
		subjectID, err := s.kernel.VerifyToken(tok)
		if err != nil {
			writeErr(w, err)
			return
		}
		// Check suspension.
		u, err := s.kernel.ReadUser(r.Context(), subjectID)
		if err == nil && u.SuspendedAt != nil {
			writeErr(w, kernel.ErrUnauthenticated.Wrap("account suspended"))
			return
		}
		ctx := context.WithValue(r.Context(), ctxSubjectID, subjectID)
		ctx = log.WithSubjectUserID(ctx, subjectID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func subjectFrom(r *http.Request) string {
	return subjectFromContext(r.Context())
}

func subjectFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxSubjectID).(string)
	return v
}

// ---- handlers ----

func (s *server) postToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle   string `json:"handle"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	tok, err := s.kernel.Login(r.Context(), req.Handle, req.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (s *server) postUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle   string `json:"handle"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
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
	writeJSON(w, http.StatusCreated, u)
}

func (s *server) getActions(w http.ResponseWriter, r *http.Request) {
	actions, err := s.kernel.ListActions(r.Context(), true, 50, 0)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	a, err := s.kernel.CreateAction(r.Context(), kernel.CreateActionRequest{
		OwnerUserID:  subjectFrom(r),
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
	writeJSON(w, http.StatusCreated, a)
}

func (s *server) getAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := s.kernel.ReadActionForSubject(r.Context(), subjectFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *server) updateAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Price        *int64         `json:"price"`
		Description  *string        `json:"description"`
		Source       *string        `json:"source"`
		InputSchema  map[string]any `json:"input_schema"`
		OutputSchema map[string]any `json:"output_schema"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	a, err := s.kernel.UpdateAction(r.Context(), subjectFrom(r), kernel.UpdateActionRequest{
		ID:           id,
		Price:        body.Price,
		Description:  body.Description,
		Source:       body.Source,
		InputSchema:  body.InputSchema,
		OutputSchema: body.OutputSchema,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *server) enableAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.kernel.SetActive(r.Context(), subjectFrom(r), id, true); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"active": true})
}

func (s *server) disableAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.kernel.SetActive(r.Context(), subjectFrom(r), id, false); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"active": false})
}

func (s *server) deleteAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.kernel.DeleteAction(r.Context(), subjectFrom(r), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) grantACL(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		SubjectUserID string `json:"subject_user_id"`
		Permission    string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	if err := s.kernel.GrantACL(r.Context(), req.SubjectUserID, id,
		kernel.Permission(req.Permission), subjectFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) revokeACL(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		SubjectUserID string `json:"subject_user_id"`
		Permission    string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	if err := s.kernel.RevokeACL(r.Context(), req.SubjectUserID, id,
		kernel.Permission(req.Permission), subjectFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listProcesses(w http.ResponseWriter, r *http.Request) {
	processes, err := s.kernel.ListProcesses(r.Context(), subjectFrom(r), 100, 0)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	p, t, err := s.kernel.StartProcess(r.Context(), subjectFrom(r), req.Funds)
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
	p, err := s.kernel.ReadProcess(r.Context(), id)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	if err := s.kernel.FundProcess(r.Context(), subjectFrom(r), id, req.Funds); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) endProcess(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.kernel.EndProcess(r.Context(), subjectFrom(r), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) postCall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProcessID     string         `json:"process_id"`
		ParentTraceID string         `json:"parent_trace_id"`
		Target        string         `json:"target"`
		ActionName    string         `json:"action_name"`
		Args          map[string]any `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	reply, err := s.kernel.Call(r.Context(), kernel.CallRequest{
		SubjectID:     subjectFrom(r),
		ProcessID:     req.ProcessID,
		ParentTraceID: req.ParentTraceID,
		TargetUserID:  req.Target,
		ActionName:    req.ActionName,
		Args:          req.Args,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

func (s *server) listTransactions(w http.ResponseWriter, r *http.Request) {
	subjectID := subjectFrom(r)
	txs, err := s.kernel.ListTransactions(r.Context(), kernel.TxFilter{
		OwnerUserID: subjectID,
		ProcessID:   r.URL.Query().Get("process_id"),
		Limit:       50,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, txs)
}

func (s *server) getTransaction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tx, err := s.kernel.ReadTransaction(r.Context(), subjectFrom(r), id)
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	if err := s.kernel.RateTransaction(r.Context(), subjectFrom(r), id, req.Rating); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	handle := r.FormValue("handle")
	password := r.FormValue("password")
	challenge := r.FormValue("code_challenge")
	redirectURI := r.FormValue("redirect_uri")
	if handle == "" || password == "" || challenge == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("handle, password, and code_challenge are required"))
		return
	}
	redirect, err := s.kernel.StartAuthCode(r.Context(), handle, password, challenge, redirectURI)
	if err != nil {
		writeErr(w, err)
		return
	}
	if redirectURI != "" {
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
}

func (s *server) postRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	if err := s.kernel.RevokeRefreshToken(r.Context(), req.RefreshToken); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Override postToken to support both password grant and authorization_code grant.
// The existing postToken handles password grant; this adds code exchange.
func (s *server) postTokenMulti(w http.ResponseWriter, r *http.Request) {
	// Detect form vs JSON.
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			writeErr(w, kernel.ErrInvalidInput.Wrap("invalid form data"))
			return
		}
		grantType := r.FormValue("grant_type")
		switch grantType {
		case "authorization_code":
			code := r.FormValue("code")
			verifier := r.FormValue("code_verifier")
			access, refresh, err := s.kernel.ExchangeAuthCode(r.Context(), code, verifier)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{
				"access_token":  access,
				"refresh_token": refresh,
			})
		default:
			writeErr(w, kernel.ErrInvalidInput.Wrap("unsupported grant_type"))
		}
		return
	}
	// Fall back to JSON password grant.
	s.postToken(w, r)
}

// ---- Listener / Event handlers ----

func (s *server) listListeners(w http.ResponseWriter, r *http.Request) {
	listeners, err := s.kernel.ListListeners(r.Context(), subjectFrom(r), 100, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listeners)
}

func (s *server) postListener(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceUserID   string `json:"source_user_id"`
		EventName      string `json:"event_name"`
		TargetActionID string `json:"target_action_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	l, err := s.kernel.CreateListener(r.Context(), kernel.CreateListenerRequest{
		OwnerUserID:    subjectFrom(r),
		SourceUserID:   req.SourceUserID,
		EventName:      req.EventName,
		TargetActionID: req.TargetActionID,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (s *server) getListenerMeta(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, err := s.kernel.GetListener(r.Context(), subjectFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *server) pollListenerEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	events, err := s.kernel.PollListener(r.Context(), subjectFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"listener_id": id, "events": events})
}

func (s *server) deleteListener(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.kernel.DeleteListener(r.Context(), subjectFrom(r), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) postEmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EventName string         `json:"event_name"`
		Args      map[string]any `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	eventIDs, err := s.kernel.EmitEvent(r.Context(), subjectFrom(r), req.EventName, req.Args, "")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_ids": eventIDs})
}

func (s *server) postConsumeEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "id")
	var req struct {
		ProcessID string `json:"process_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	if req.ProcessID == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("process_id is required"))
		return
	}
	reply, err := s.kernel.ConsumeEvent(r.Context(), subjectFrom(r), eventID, req.ProcessID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

func (s *server) grantAll(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.kernel.GrantAll(r.Context(), subjectFrom(r), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) revokeAll(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.kernel.RevokeAll(r.Context(), subjectFrom(r), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- well-known / federation ----

func (s *server) getWellKnown(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pubKey, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	handle, _ := s.kernel.GetConfig(ctx, configKeySuperuser)
	if handle == "" {
		handle = "@sys"
	}
	baseURL := envOr("JUICE_BASE_URL", "")
	writeJSON(w, http.StatusOK, map[string]string{
		"handle":     handle,
		"public_key": pubKey,
		"base_url":   baseURL,
	})
}

func (s *server) postFederationCall(w http.ResponseWriter, r *http.Request) {
	actionName := r.URL.Query().Get("action")
	if actionName == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("action query param required"))
		return
	}
	var args map[string]any
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("invalid JSON"))
		return
	}
	ctx := r.Context()

	// Idempotency: replay a prior response if the same key is presented.
	idempotencyKey := r.Header.Get("X-Idempotency-Key")
	if idempotencyKey != "" {
		if rec, err := s.kernel.GetIdempotencyRecord(ctx, idempotencyKey, ""); err == nil {
			var receipt *kernel.Receipt
			if rec.ReceiptID != nil {
				receipt, _ = s.kernel.GetReceiptByID(ctx, *rec.ReceiptID)
			}
			writeJSON(w, http.StatusOK, map[string]any{"result": nil, "receipt": receipt})
			return
		}
	}

	suHandle, _ := s.kernel.GetConfig(ctx, configKeySuperuser)
	su, err := s.kernel.ReadUserByHandle(ctx, suHandle)
	if err != nil {
		writeErr(w, kernel.ErrInvalidState.Wrap("kernel not bootstrapped"))
		return
	}
	proc, _, err := s.kernel.StartProcess(ctx, su.ID, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer s.kernel.EndProcess(ctx, su.ID, proc.ID)
	reply, err := s.kernel.Call(ctx, kernel.CallRequest{
		SubjectID:    su.ID,
		ProcessID:    proc.ID,
		TargetUserID: su.ID,
		ActionName:   actionName,
		Args:         args,
	})
	if err != nil {
		writeErr(w, err)
		return
	}

	// Fetch receipt and record idempotency entry.
	var receipt *kernel.Receipt
	if reply.TxID != "" {
		receipt, _ = s.kernel.GetReceiptByTxID(ctx, reply.TxID)
	}
	if idempotencyKey != "" && receipt != nil {
		_ = s.kernel.CreateIdempotencyRecord(ctx, &kernel.IdempotencyRecord{
			ID:                 uuid.New().String(),
			IdempotencyKey:     idempotencyKey,
			CounterpartyUserID: "",
			ReceiptID:          &receipt.ID,
			CreatedAt:          time.Now().UTC(),
			ExpiresAt:          time.Now().UTC().Add(24 * time.Hour),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"result": reply.Result, "receipt": receipt})
}

func (s *server) getActionManifest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	m, err := s.kernel.GetActionManifest(r.Context(), subjectFrom(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ---- me ----

func (s *server) getMe(w http.ResponseWriter, r *http.Request) {
	u, err := s.kernel.ReadUser(r.Context(), subjectFrom(r))
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

// ---- response helpers ----

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
