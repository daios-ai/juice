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

// listBounds parses the standard limit/offset query params for every list endpoint:
// default limit 50, ceiling 200, offset floored at 0. The ceiling bounds any single
// response so no request pulls an unbounded result set.
func listBounds(r *http.Request) (limit, offset int) {
	limit = qInt(r, "limit", 50)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset = qInt(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	return
}

// ---------------------------------------------------------------------------
// Server: handlers (thin wires over the same kernel calls the CLI used in-process)
// ---------------------------------------------------------------------------

func (s *server) ctlListUsers(w http.ResponseWriter, r *http.Request) {
	limit, offset := listBounds(r)
	users, err := s.kernel.ListUsers(r.Context(), limit, offset)
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

func (s *server) ctlRenameUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NewHandle string `json:"new_handle"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	u, err := resolveHandle(s.kernel, r.Context(), chi.URLParam(r, "handle"))
	if err != nil {
		writeErr(w, err)
		return
	}
	out, err := s.kernel.RenameUser(r.Context(), callerFrom(r), u.ID, req.NewHandle)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"handle": out.Handle})
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
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, enrichAdjustment(adj, newUserCache(s.kernel, r.Context())))
	}
}

func (s *server) ctlListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.kernel.ListPeers(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	// Friended peers by default; denied (unfriended) peers are hidden unless ?all=1, like
	// action list hides inactive rows. The row still exists — this is display scope only.
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	if !all {
		kept := peers[:0]
		for _, p := range peers {
			if p.DeniedAt == nil {
				kept = append(kept, p)
			}
		}
		peers = kept
	}
	out := map[string]any{"peers": peerViews(peers)}
	if r.URL.Query().Get("gossip") == "1" {
		roster, err := s.kernel.DiscoveryRoster(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		out["roster"] = roster
	}
	writeJSON(w, http.StatusOK, out)
}

// ctlInspectPeer has defined behavior whether the peer is up or down (§13). It always reports
// reachability; a reachable peer yields live identity/actions/friends; an unreachable but
// previously-friended peer degrades to the last-known local data; a stranger that is unreachable
// yields an empty view with source="none". Every remote call is bounded by fedOpTimeout so an
// offline peer fails in seconds, not on the client timeout.
func (s *server) ctlInspectPeer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ident := strings.TrimSpace(r.URL.Query().Get("key"))
	peerKey := ident
	// Resolve a local reference to its peer key. A reference that resolves to a local account with
	// no public key is a plain user, not a federation peer: reject it rather than probe the handle
	// as if it were a key (inspect is a peer-only window, §13). Only a reference that resolves to no
	// local account at all is treated as a raw stranger key to probe (inspect <key> before friending).
	if u, err := resolveHandle(s.kernel, ctx, ident); err == nil {
		if u.PublicKey == "" {
			writeErr(w, kernel.ErrInvalidInput.Wrapf("%q is a local user, not a federation peer", ident))
			return
		}
		peerKey = u.PublicKey
	} else if strings.HasPrefix(ident, "@") {
		// An @handle that names no local account: there is no peer to inspect (a stranger is
		// inspected by key, not by an unfriended handle). Don't probe the handle as if it were a key.
		writeErr(w, kernel.ErrNotFound.Wrapf("no peer %q", ident))
		return
	}
	if s.fed == nil {
		writeErr(w, kernel.ErrInvalidState.Wrap("federation transport not running"))
		return
	}
	octx, cancel := context.WithTimeout(ctx, fedOpTimeout)
	defer cancel()

	reach := s.fed.Probe(octx, peerKey)
	resp := map[string]any{"reachability": reach, "online": reach.Path != "unreachable"}

	// Live view when the peer answers.
	if iRaw, err := s.fed.Inspect(octx, peerKey); err == nil {
		var g kernel.GossipResponse
		if json.Unmarshal(iRaw, &g) == nil {
			resp["handle"], resp["public_key"] = g.Handle, g.PublicKey
			resp["actions"], resp["friends"] = g.Actions, g.Friends
			resp["source"] = "live"
			// On-demand peer sync: the live inspect just learned this peer is up and (for a friend)
			// our credit there. Persist it so peer_state / last_seen refresh immediately instead of
			// waiting for the discovery timer. RecordPeerSync no-ops for strangers/denied peers (§13).
			_ = s.kernel.RecordPeerSync(ctx, g.PublicKey, g.CounterpartyBalance)
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}
	// Offline (or unparseable): fall back to what we hold locally about a friended peer.
	if handle, pk, actions, err := s.kernel.PeerLocalView(ctx, ident); err == nil {
		resp["handle"], resp["public_key"], resp["actions"] = handle, pk, actions
		resp["friends"], resp["source"] = []kernel.GossipFriendView{}, "local"
	} else {
		resp["handle"], resp["public_key"] = "", peerKey
		resp["actions"], resp["friends"] = []kernel.GossipAction{}, []kernel.GossipFriendView{}
		resp["source"] = "none"
	}
	writeJSON(w, http.StatusOK, resp)
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

	// Resolve the peer by key and read its gossip (identity + actions + friends). Bounded by
	// fedOpTimeout so an offline peer fails promptly and clearly — you cannot friend a kernel you
	// cannot reach. The gossip's public_key must match the key we dialed (a consistency check).
	octx, cancel := context.WithTimeout(ctx, fedOpTimeout)
	defer cancel()
	gRaw, err := s.fed.Gossip(octx, peerKey)
	if err != nil {
		writeErr(w, kernel.ErrExecutionFailed.Wrapf("cannot friend %s: peer is unreachable (offline?)", peerKey))
		return
	}
	var g kernel.GossipResponse
	if json.Unmarshal(gRaw, &g) != nil || g.PublicKey != peerKey {
		writeErr(w, kernel.ErrExecutionFailed.Wrap("peer gossip identity mismatch"))
		return
	}

	// Register the peer locally, send a signed friend handshake so it registers + reciprocates,
	// import its active public actions, then accumulate its gossip.
	u, err := s.kernel.CreateOrUpdateProxyPeer(ctx, g.Handle, peerKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	// An explicit friend re-establishes trust: clear any prior denial (unfriend sets it). The
	// inbound handshake path (OnFriend) deliberately does NOT — a denied peer can't un-deny itself.
	if u.DeniedAt != nil {
		if err := s.kernel.UndenyPeer(ctx, callerFrom(r), u.Handle); err != nil {
			writeErr(w, err)
			return
		}
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
	// Accept @handle or the peer's key. Purely local (no transport), so it works whether or not the
	// peer is reachable. A clear not-found when the identifier names no friended peer.
	u, err := resolveHandle(s.kernel, r.Context(), req.Handle)
	if err != nil {
		writeErr(w, kernel.ErrNotFound.Wrapf("no friended peer %q", req.Handle))
		return
	}
	// A local account with no key is not a peer: never deny-list a plain user (which would block it).
	if u.PublicKey == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrapf("%q is a local user, not a federation peer", req.Handle))
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
		// Friending is an explicit trust act: activate every one of the peer's proxies, including
		// Unchanged ones. A re-friend after unfriend sees byte-identical manifests (→ Unchanged) whose
		// Active was cleared by the unfriend cascade; without this they'd stay dead and uncallable.
		t := true
		for _, act := range append(append(result.Created, result.Updated...), result.Unchanged...) {
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
