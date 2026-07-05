package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
	"github.com/go-chi/chi/v5"
)

// Superuser supervision (admin/peer verbs — money, access, federation trust, roster) is served
// on the ordinary public TCP API, gated per-route by requireSuperuserMW (an IsSuperuser check),
// exactly as the widened list/disable scope already is (§14). There is no separate control
// surface: authority is the @sys bearer token, so keep it secret and run `serve` behind TLS or
// on loopback. The route registrations live in registerRoutes (serve.go); this file holds the
// superuser handlers and the federation-import helpers they call.

// allowLocalPeers reports whether outbound federation-import HTTP fetches may reach
// local/private addresses. Federation transport itself is libp2p (§13); this remains
// only for the action-import fetch paths, gated by the action-source dev escape hatch.
func allowLocalPeers() bool {
	return globalCfg.AllowLocalSources
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
// Client helpers
// ---------------------------------------------------------------------------

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
