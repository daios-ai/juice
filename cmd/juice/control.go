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

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
	"github.com/go-chi/chi/v5"
)

// The superuser control plane. admin/peer supervision runs over a local Unix-domain socket
// served by `juice serve`, never the public TCP API — so `serve` is the only process that
// opens SQLite (no second-writer contention), while authority stays filesystem-based: a
// 0600 socket next to the DB, plus a valid @sys bearer token (§14). The socket path is
// derived from --db so the client needs no configuration.

// allowLocalPeers reports whether outbound federation-import HTTP fetches may reach
// local/private addresses. Federation transport itself is libp2p (§13); this remains
// only for the action-import fetch paths, gated by the action-source dev escape hatch.
func allowLocalPeers() bool {
	return globalCfg.AllowLocalSources
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
	r.Get("/control/peers", s.ctlListPeers)
	r.Get("/control/peers/inspect", s.ctlInspectPeer)
	r.Post("/control/peers/friend", s.ctlFriendPeer)
	r.Post("/control/peers/unfriend", s.ctlUnfriendPeer)
	r.Get("/control/identity", s.ctlIdentity)
	return r
}

// requireSuperuserMW rejects any caller whose handle is not the configured superuser. It runs
// after authMiddleware, so the caller is already authenticated and unsuspended.
func (s *server) requireSuperuserMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.kernel.IsSuperuser(r.Context(), callerFrom(r)) {
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
	peerKey := strings.TrimSpace(r.URL.Query().Get("key"))
	// Accept an @handle for an already-friended peer, not just its key; a stranger's raw key
	// (no local account yet) falls through unchanged.
	if u, err := resolveHandle(s.kernel, ctx, peerKey); err == nil && u.PublicKey != "" {
		peerKey = u.PublicKey
	}
	if s.fed == nil {
		writeErr(w, kernel.ErrInvalidState.Wrap("federation transport not running"))
		return
	}
	iRaw, err := s.fed.Inspect(ctx, peerKey)
	if err != nil {
		writeErr(w, kernel.ErrExecutionFailed.Wrapf("cannot reach peer: %v", err))
		return
	}
	var g kernel.GossipResponse
	if json.Unmarshal(iRaw, &g) != nil {
		writeErr(w, kernel.ErrExecutionFailed.Wrap("could not parse peer inspect document"))
		return
	}
	// Reachability diagnostics replace the browser-reachable endpoint that no longer exists.
	reach := s.fed.Probe(ctx, peerKey)
	writeJSON(w, http.StatusOK, map[string]any{
		"handle":       g.Handle,
		"public_key":   g.PublicKey,
		"actions":      g.Actions,
		"friends":      g.Friends,
		"reachability": reach,
	})
}

func (s *server) ctlFriendPeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if s.fed == nil {
		writeErr(w, kernel.ErrInvalidState.Wrap("federation transport not running"))
		return
	}
	ctx := r.Context()
	peerKey := strings.TrimSpace(req.Key)

	// Resolve the peer by key and read its gossip (identity + actions + friends). The gossip's
	// public_key must match the key we dialed — the transport authenticated the connection by
	// key, so this is a consistency check, not the trust boundary.
	gRaw, err := s.fed.Gossip(ctx, peerKey)
	if err != nil {
		writeErr(w, kernel.ErrExecutionFailed.Wrapf("cannot reach peer: %v", err))
		return
	}
	var g kernel.GossipResponse
	if json.Unmarshal(gRaw, &g) != nil || g.PublicKey != peerKey {
		writeErr(w, kernel.ErrExecutionFailed.Wrap("peer gossip identity mismatch"))
		return
	}

	// Register the peer locally (clears any prior denial), send a signed friend handshake so it
	// registers + reciprocates, import its active public actions, then accumulate its gossip.
	u, err := s.kernel.CreateOrUpdateProxyPeer(ctx, g.Handle, peerKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.announcePeerFed(ctx, peerKey)
	imported, skipped := bulkImportPeerActionsFed(ctx, s.fed, s.kernel, callerFrom(r), peerKey, u)
	pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	_ = s.kernel.AccumulateGossip(ctx, &g, pub)
	writeJSON(w, http.StatusOK, map[string]any{"handle": u.Handle, "imported": imported, "skipped": skipped})
}

// ctlIdentity reports this kernel's federation identity: public key, handle, and libp2p listen
// addresses. This is how an operator obtains the key to share for friending, now that the
// .well-known document is gone (§13).
func (s *server) ctlIdentity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	handle := globalCfg.KernelHandle
	var addrs []string
	if s.fed != nil {
		addrs = s.fed.ListenAddrs()
	}
	writeJSON(w, http.StatusOK, map[string]any{"handle": handle, "public_key": pub, "addrs": addrs})
}

func (s *server) ctlUnfriendPeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle string `json:"handle"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	// Accept @handle or the peer's key (the global name it was friended by).
	u, err := resolveHandle(s.kernel, r.Context(), req.Handle)
	if err != nil {
		writeErr(w, err)
		return
	}
	err = s.kernel.DenyPeer(r.Context(), callerFrom(r), u.Handle)
	writeOr(w, map[string]string{"handle": u.Handle}, err)
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
// Server: peer outbound helpers (over the libp2p federation transport, §13)
// ---------------------------------------------------------------------------

// announcePeerFed sends a signed friend request to a peer by key so it registers + reciprocates.
func (s *server) announcePeerFed(ctx context.Context, peerKey string) {
	if s.fed == nil {
		return
	}
	pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	handle := globalCfg.KernelHandle
	if pub == "" {
		return
	}
	sig, ts, err := s.kernel.SignPeerRequestNow(handle, pub)
	if err != nil {
		return
	}
	_, _ = s.fed.Friend(ctx, peerKey, fed.FriendRequest{Handle: handle, PublicKey: pub, Timestamp: ts, Signature: sig})
}

// manifestFetcher is the transport capability bulk import needs; *fed.Transport satisfies it,
// and tests supply a fake so the import + enable + publish logic is unit-testable without libp2p.
type manifestFetcher interface {
	Manifests(ctx context.Context, peerKey string) ([]json.RawMessage, error)
}

// bulkImportPeerActionsFed fetches a peer's manifests over the transport and imports them as
// enabled, public remote_proxy actions. Returns the counts imported and skipped.
func bulkImportPeerActionsFed(ctx context.Context, tr manifestFetcher, k *kernel.Kernel, subjectID, peerKey string, peer *kernel.User) (imported, skipped int) {
	manifests, err := tr.Manifests(ctx, peerKey)
	if err != nil {
		return 0, 0
	}
	for _, raw := range manifests {
		var m kernel.ActionManifest
		if json.Unmarshal(raw, &m) != nil {
			skipped++
			continue
		}
		result, rErr := k.ReconcileRemoteAction(ctx, subjectID, peer.Handle, m.Name, &m)
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
