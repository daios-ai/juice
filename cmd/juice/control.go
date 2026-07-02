package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/go-chi/chi/v5"
)

// The superuser control plane. admin/peer supervision runs over a local Unix-domain socket
// served by `juice serve`, never the public TCP API — so `serve` is the only process that
// opens SQLite (no second-writer contention), while authority stays filesystem-based: a
// 0600 socket next to the DB, plus a valid @sys bearer token (§14). The socket path is
// derived from --db so the client needs no configuration.

// allowLocalPeers reports whether peer federation HTTP may reach local/private addresses.
// allow_local_peer_urls targets peer traffic only; allow_local_sources enables everything.
func allowLocalPeers() bool {
	return globalCfg.AllowLocalPeerURLs || globalCfg.AllowLocalSources
}

func controlSocketPath(dbPath string) string {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		abs = dbPath
	}
	return filepath.Join(filepath.Dir(abs), "juice-control.sock")
}

// ---------------------------------------------------------------------------
// Server: control listener + router
// ---------------------------------------------------------------------------

// startControlPlane binds the control socket and serves the superuser router on it. The
// returned closer stops the server and removes the socket.
func startControlPlane(srv *server, dbPath string) (io.Closer, error) {
	path := controlSocketPath(dbPath)
	_ = os.Remove(path) // clear a stale socket left by a crashed server
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	httpSrv := &http.Server{Handler: srv.controlRouter()}
	go func() { _ = httpSrv.Serve(ln) }()
	return &controlPlane{httpSrv: httpSrv, path: path}, nil
}

type controlPlane struct {
	httpSrv *http.Server
	path    string
}

func (c *controlPlane) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.httpSrv.Shutdown(ctx)
	_ = os.Remove(c.path)
	return err
}

func (s *server) controlRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(s.authMiddleware, s.requireSuperuserMW)
	r.Get("/control/users", s.ctlListUsers)
	r.Get("/control/users/{handle}", s.ctlShowUser)
	r.Post("/control/users/{handle}/suspend", s.ctlSetSuspended(true))
	r.Post("/control/users/{handle}/unsuspend", s.ctlSetSuspended(false))
	r.Post("/control/deposit", s.ctlAdjust(kernel.DirectionCredit))
	r.Post("/control/withdraw", s.ctlAdjust(kernel.DirectionDebit))
	r.Get("/control/actions", s.ctlListActions)
	r.Post("/control/actions/disable", s.ctlDisableAction)
	r.Get("/control/processes", s.ctlListProcesses)
	r.Get("/control/txs", s.ctlListTxs)
	r.Get("/control/steps", s.ctlListSteps)
	r.Get("/control/peers", s.ctlListPeers)
	r.Get("/control/peers/inspect", s.ctlInspectPeer)
	r.Post("/control/peers/friend", s.ctlFriendPeer)
	r.Post("/control/peers/unfriend", s.ctlUnfriendPeer)
	return r
}

// requireSuperuserMW rejects any caller whose handle is not the configured superuser. It runs
// after authMiddleware, so the caller is already authenticated and unsuspended.
func (s *server) requireSuperuserMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.kernel.ReadUser(r.Context(), callerFrom(r))
		if err != nil {
			writeErr(w, err)
			return
		}
		want, _ := s.kernel.GetConfig(r.Context(), configKeySuperuser)
		if want == "" {
			want = superuserHandle
		}
		if u.Handle != want {
			writeErr(w, kernel.ErrUnauthorized.Wrap("superuser required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func qInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// Server: handlers (thin wires over the same kernel calls the CLI used in-process)
// ---------------------------------------------------------------------------

func (s *server) ctlListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.kernel.ListUsers(r.Context(), qInt(r, "limit", 50), qInt(r, "offset", 0))
	writeOr(w, users, err)
}

func (s *server) ctlShowUser(w http.ResponseWriter, r *http.Request) {
	u, err := resolveHandle(s.kernel, r.Context(), chi.URLParam(r, "handle"))
	writeOr(w, u, err)
}

func (s *server) ctlSetSuspended(suspend bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := resolveHandle(s.kernel, r.Context(), chi.URLParam(r, "handle"))
		if err != nil {
			writeErr(w, err)
			return
		}
		if suspend {
			err = s.kernel.SuspendUser(r.Context(), callerFrom(r), u.ID)
		} else {
			err = s.kernel.UnsuspendUser(r.Context(), callerFrom(r), u.ID)
		}
		writeOr(w, map[string]string{"handle": u.Handle}, err)
	}
}

func (s *server) ctlAdjust(direction string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Handle      string `json:"handle"`
			Amount      int64  `json:"amount"`
			Reason      string `json:"reason"`
			ExternalKey string `json:"external_key"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		u, err := resolveHandle(s.kernel, r.Context(), req.Handle)
		if err != nil {
			writeErr(w, err)
			return
		}
		var adj *kernel.Adjustment
		if direction == kernel.DirectionCredit {
			adj, err = s.kernel.Deposit(r.Context(), callerFrom(r), u.ID, req.Amount, req.Reason, req.ExternalKey)
		} else {
			adj, err = s.kernel.Withdraw(r.Context(), callerFrom(r), u.ID, req.Amount, req.Reason, req.ExternalKey)
		}
		writeOr(w, adj, err)
	}
}

func (s *server) ctlListActions(w http.ResponseWriter, r *http.Request) {
	actions, err := s.kernel.ListAllActions(r.Context(), qInt(r, "limit", 50), qInt(r, "offset", 0))
	writeOr(w, actions, err)
}

func (s *server) ctlDisableAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	a, err := resolveActionRef(s.kernel, r.Context(), req.Ref)
	if err != nil {
		writeErr(w, err)
		return
	}
	err = s.kernel.SetActive(r.Context(), callerFrom(r), a.ID, false)
	writeOr(w, map[string]string{"id": a.ID}, err)
}

func (s *server) ctlListProcesses(w http.ResponseWriter, r *http.Request) {
	procs, err := s.kernel.ListAllProcesses(r.Context(), qInt(r, "limit", 50), qInt(r, "offset", 0))
	writeOr(w, procs, err)
}

func (s *server) ctlListTxs(w http.ResponseWriter, r *http.Request) {
	rows, err := adminListTxRows(s.kernel, r.Context(), qInt(r, "limit", 50), qInt(r, "offset", 0))
	writeOr(w, rows, err)
}

func (s *server) ctlListSteps(w http.ResponseWriter, r *http.Request) {
	steps, err := s.kernel.ListSteps(r.Context(), callerFrom(r),
		r.URL.Query().Get("process"), r.URL.Query().Get("status"))
	writeOr(w, steps, err)
}

func (s *server) ctlListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.kernel.ListPeers(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := map[string]any{"peers": peers}
	if r.URL.Query().Get("gossip") == "1" {
		discovered, err := s.kernel.ListDiscoveredKernels(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		out["discovered"] = discovered
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) ctlInspectPeer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	base := strings.TrimRight(r.URL.Query().Get("url"), "/")
	allow := allowLocalPeers()
	wk, err := fetchWellKnown(ctx, base, allow)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := map[string]any{"handle": wk.Handle, "public_key": wk.PublicKey, "base_url": wk.BaseURL}
	if g := fetchGossip(ctx, base, allow); g != nil {
		out["actions"] = g.Actions
		out["friends"] = g.Friends
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) ctlFriendPeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	ctx := r.Context()
	allow := allowLocalPeers()
	wk, err := fetchWellKnown(ctx, strings.TrimRight(req.URL, "/"), allow)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Register the peer locally (clears any prior denial), announce ourselves, import their
	// active public actions, then accumulate their gossip to discover their friends.
	u, err := s.kernel.CreateOrUpdateProxyPeer(ctx, wk.Handle, wk.PublicKey, wk.BaseURL)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.announcePeer(ctx, wk.BaseURL, allow)
	imported, skipped := bulkImportPeerActions(ctx, s.kernel, callerFrom(r), u, allow)
	if g := fetchGossip(ctx, wk.BaseURL, allow); g != nil {
		pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
		_ = s.kernel.AccumulateGossip(ctx, g, pub)
	}
	writeJSON(w, http.StatusOK, map[string]any{"handle": u.Handle, "imported": imported, "skipped": skipped})
}

func (s *server) ctlUnfriendPeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle string `json:"handle"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	handle := kernel.NormalizeHandle(req.Handle)
	err := s.kernel.DenyPeer(r.Context(), callerFrom(r), handle)
	writeOr(w, map[string]string{"handle": handle}, err)
}

// writeOr writes v as JSON on success, or the error otherwise.
func writeOr(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---------------------------------------------------------------------------
// Server: peer outbound helpers (relocated here so `serve` owns all federation HTTP)
// ---------------------------------------------------------------------------

type wellKnown struct {
	Handle    string `json:"handle"`
	PublicKey string `json:"public_key"`
	BaseURL   string `json:"base_url"`
}

func fetchWellKnown(ctx context.Context, base string, allow bool) (*wellKnown, error) {
	body, status, err := doHTTP(ctx, http.MethodGet, base+"/.well-known/juice-kernel.json", nil, nil, 15*time.Second, allow)
	if err != nil {
		return nil, errUnreachable(base, err)
	}
	if status != http.StatusOK {
		return nil, kernel.ErrExecutionFailed.Wrapf("peer well-known returned status %d", status)
	}
	var wk wellKnown
	if err := json.Unmarshal(body, &wk); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("parse well-known: %v", err)
	}
	return &wk, nil
}

func fetchGossip(ctx context.Context, base string, allow bool) *kernel.GossipResponse {
	body, status, err := doHTTP(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/gossip", nil, nil, 15*time.Second, allow)
	if err != nil || status != http.StatusOK {
		return nil
	}
	var g kernel.GossipResponse
	if json.Unmarshal(body, &g) != nil {
		return nil
	}
	return &g
}

// announcePeer sends a signed friend request to the peer's /v1/peers so it can reciprocate.
func (s *server) announcePeer(ctx context.Context, peerBaseURL string, allow bool) {
	pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	handle := globalCfg.PeerHandle
	if handle == "" {
		handle, _ = s.kernel.GetConfig(ctx, configKeySuperuser)
	}
	// The address we advertise: the real bound port (set at boot under --addr :0).
	baseURL, _ := s.kernel.GetConfig(ctx, "kernel_base_url")
	if pub == "" || baseURL == "" {
		return
	}
	sig, ts, err := s.kernel.SignPeerRequestNow(handle, pub, baseURL)
	if err != nil {
		return
	}
	body, _ := json.Marshal(map[string]string{
		"handle": handle, "public_key": pub, "base_url": baseURL, "timestamp": ts, "signature": sig,
	})
	_, _, _ = doHTTP(ctx, http.MethodPost, strings.TrimRight(peerBaseURL, "/")+"/v1/peers",
		map[string]string{"Content-Type": "application/json"}, strings.NewReader(string(body)), 15*time.Second, allow)
}

// bulkImportPeerActions fetches a peer's active public actions and imports them as enabled,
// public remote_proxy actions. Returns the counts imported and skipped.
func bulkImportPeerActions(ctx context.Context, k *kernel.Kernel, subjectID string, peer *kernel.User, allowLocal bool) (imported, skipped int) {
	base := strings.TrimRight(peer.RemoteBaseURL, "/")
	listBody, status, err := doHTTP(ctx, http.MethodGet, base+"/v1/actions", nil, nil, 30*time.Second, allowLocal)
	if err != nil || status != http.StatusOK {
		return 0, 0
	}
	var actions []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if json.Unmarshal(listBody, &actions) != nil {
		return 0, 0
	}
	for _, a := range actions {
		mBody, mStatus, mErr := doHTTP(ctx, http.MethodGet, base+"/v1/actions/"+a.ID+"/manifest", nil, nil, 30*time.Second, allowLocal)
		if mErr != nil || mStatus != http.StatusOK {
			skipped++
			continue
		}
		var m kernel.ActionManifest
		if json.Unmarshal(mBody, &m) != nil {
			skipped++
			continue
		}
		result, rErr := k.ReconcileRemoteAction(ctx, subjectID, peer.Handle, a.Name, &m)
		if rErr != nil {
			skipped++
			continue
		}
		t := true
		for _, act := range append(result.Created, result.Updated...) {
			_ = enableAction(k, ctx, subjectID, act.ID)
			_, _ = k.UpdateAction(ctx, subjectID, kernel.UpdateActionRequest{ID: act.ID, Public: &t})
		}
		imported += len(result.Created) + len(result.Unchanged)
	}
	return imported, skipped
}

// ---------------------------------------------------------------------------
// Client: HTTP over the control socket
// ---------------------------------------------------------------------------

// A context carrying the control-socket path routes apiDo over the Unix socket instead of
// TCP. admin/peer commands attach it via ctlCall/ctlEmit; user-facing commands never do, so
// the transport choice is per-call and cannot leak between commands.
type ctlCtxKey struct{}

func withControlSocket(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctlCtxKey{}, controlSocketPath(flagDB))
}

func controlSockFromCtx(ctx context.Context) string {
	s, _ := ctx.Value(ctlCtxKey{}).(string)
	return s
}

func ctlCall(ctx context.Context, method, path string, body, out any) error {
	return apiCall(withControlSocket(ctx), method, path, body, out)
}

func ctlEmit(method, path string, body any) error {
	return apiEmitCtx(withControlSocket(context.Background()), method, path, body)
}

// doControlHTTP performs one request over the control socket. The URL host is a placeholder;
// the dialer ignores it and connects to the socket.
func doControlHTTP(ctx context.Context, sock, method, path string, headers map[string]string, body io.Reader) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return nil, 0, kernel.ErrInvalidInput.Wrapf("invalid request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}

// ctlPath appends limit/offset query parameters when set.
func ctlPath(path string, limit, offset int) string {
	q := ""
	if limit > 0 {
		q = "limit=" + strconv.Itoa(limit)
	}
	if offset > 0 {
		if q != "" {
			q += "&"
		}
		q += "offset=" + strconv.Itoa(offset)
	}
	if q == "" {
		return path
	}
	return path + "?" + q
}
