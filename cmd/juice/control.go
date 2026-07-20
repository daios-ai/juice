package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

func (s *server) ctlAdjust(credit bool) http.HandlerFunc {
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
			// Deposit-by-key opens the peer's billing account (§13): the provider's single deposit
			// both provisions and funds a not-yet-known subscriber's account — this replaces the old
			// friend handshake. Withdraw never auto-provisions (nothing to redeem from a fresh row).
			if credit && !strings.HasPrefix(strings.TrimSpace(req.Handle), "@") {
				kh := strings.TrimSpace(req.Handle)
				short := kh
				if len(short) > 8 {
					short = short[:8]
				}
				if peer, aerr := s.kernel.AddPeer(r.Context(), callerFrom(r), "@k-"+short, kh); aerr == nil {
					u = peer
					err = nil
				}
			}
			if err != nil {
				writeErr(w, err)
				return
			}
		}
		var e *kernel.LedgerEntry
		if credit {
			e, err = s.kernel.Deposit(r.Context(), callerFrom(r), u.ID, req.Amount, req.Reason, req.ExternalKey)
		} else {
			e, err = s.kernel.Withdraw(r.Context(), callerFrom(r), u.ID, req.Amount, req.Reason, req.ExternalKey)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, enrichLedger(e, newUserCache(s.kernel, r.Context())))
	}
}

func (s *server) ctlListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.kernel.ListPeers(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	// Active peers by default; suspended peers are hidden unless ?all=1, like action list hides
	// inactive rows. The row still exists — this is display scope only.
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	if !all {
		kept := peers[:0]
		for _, p := range peers {
			if p.SuspendedAt == nil {
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
	// A local account with no public key is a plain user, not a federation peer, and an @handle
	// naming no account is not a peer either — resolvePeerKey rejects both rather than probing the
	// handle as if it were a key (inspect is a peer-only window, §13). An unresolvable non-@
	// identifier is a raw stranger key, which is exactly the inspect-before-subscribing case.
	peerKey, err := s.resolvePeerKey(ctx, ident)
	if err != nil {
		writeErr(w, err)
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

func (s *server) ctlSubscribePeer(w http.ResponseWriter, r *http.Request) {
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

	// Subscribe is a purely local import: read the peer's gossip (identity + actions), mount it
	// under its self-reported handle, import its active public actions, and accumulate its gossip.
	// Nothing is written to the peer — its billing account here is opened by a deposit, not a
	// handshake. Bounded by fedOpTimeout so an offline peer fails promptly. The gossip's public_key
	// must match the key we dialed (a consistency check).
	octx, cancel := context.WithTimeout(ctx, fedOpTimeout)
	defer cancel()
	gRaw, err := s.fed.Gossip(octx, peerKey)
	if err != nil {
		writeErr(w, kernel.ErrExecutionFailed.Wrapf("cannot subscribe to %s: peer is unreachable (offline?)", peerKey))
		return
	}
	var g kernel.GossipResponse
	if json.Unmarshal(gRaw, &g) != nil || g.PublicKey != peerKey {
		writeErr(w, kernel.ErrExecutionFailed.Wrap("peer gossip identity mismatch"))
		return
	}

	u, err := s.kernel.CreateOrUpdateProxyPeer(ctx, g.Handle, peerKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	imported, skipped := bulkImportPeerActionsFed(ctx, s.fed, s.kernel, callerFrom(r), peerKey, u)
	pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	_ = s.kernel.AccumulateGossip(ctx, &g, pub)
	writeJSON(w, http.StatusOK, map[string]any{"handle": u.Handle, "imported": imported, "skipped": skipped})
}

// resolvePeerKey maps an @handle / key / id reference to a peer's public key, rejecting a local
// account that is not a federation peer. Shared by the peer commands.
func (s *server) resolvePeerKey(ctx context.Context, ident string) (string, error) {
	if ident == "" {
		return "", kernel.ErrInvalidInput.Wrap("a peer @handle or public key is required")
	}
	if u, err := resolveHandle(s.kernel, ctx, ident); err == nil {
		if u.PublicKey == "" {
			return "", kernel.ErrInvalidInput.Wrapf("%q is a local user, not a federation peer", ident)
		}
		return u.PublicKey, nil
	}
	if strings.HasPrefix(ident, "@") {
		return "", kernel.ErrNotFound.Wrapf("no peer %q", ident)
	}
	// An unresolvable non-@ identifier is treated as a raw stranger key (inspecting or addressing a
	// peer before it is known locally) — the transport reports it unreachable if it is not one.
	return ident, nil
}

// stepRoundTrip signs, dispatches, and unwraps one outbound /juice/fed/step/1 request.
func (s *server) stepRoundTrip(ctx context.Context, peerKey string, req fed.StepRequest, timeout time.Duration) (map[string]any, error) {
	if s.fed == nil {
		return nil, kernel.ErrInvalidState.Wrap("federation transport not running")
	}
	octx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := s.fed.Step(octx, peerKey, req)
	if err != nil {
		// Preserve the §13 dispatch distinction, as the call path does. Only a provably-never-sent
		// request is "unreachable"; anything else (a timeout while the peer runs the resumed call,
		// a mid-stream failure) may already have executed and settled there, so it must not be
		// reported as if nothing happened. Retrying is safe and is how the real result is recovered:
		// the idempotency key is derived from the request, so a repeat returns the stored outcome.
		if errors.Is(err, fed.ErrNotDispatched) {
			return nil, kernel.ErrPeerUnreachable.Wrapf("cannot reach %s (offline?)", peerKey).WithMeta("peer", peerKey)
		}
		return nil, kernel.ErrTimeout.Wrapf(
			"no reply from %s; the request may have executed there — retry to recover its result", peerKey).WithMeta("peer", peerKey)
	}
	var body map[string]any
	if json.Unmarshal(resp.Body, &body) != nil {
		return nil, kernel.ErrExecutionFailed.Wrap("malformed peer response")
	}
	if resp.Status >= 300 {
		msg, _ := body["error"].(string)
		if msg == "" {
			msg = "peer rejected the step request"
		}
		code, _ := body["code"].(string)
		out := kernel.ErrorFromCode(code).Wrap(msg)
		// Preserve the peer's structured metadata (e.g. the settled tx a failed completion was
		// charged for) so it reaches the operator instead of dying at the hop.
		if meta, ok := body["meta"].(map[string]any); ok {
			for k, v := range meta {
				if sv, ok := v.(string); ok {
					out = out.WithMeta(k, sv)
				}
			}
		}
		return nil, out
	}
	return body, nil
}

// ctlPeerSteps lists the waiting steps a peer holds for this kernel (§13). These are the
// continuations a peer parked for us — invisible before /juice/fed/step/1 existed.
func (s *server) ctlPeerSteps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	peerKey, err := s.resolvePeerKey(ctx, strings.TrimSpace(r.URL.Query().Get("key")))
	if err != nil {
		writeErr(w, err)
		return
	}
	pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	startCursor := strings.TrimSpace(r.URL.Query().Get("after"))

	// Follow the peer's pages until exhausted rather than exposing an --offset flag. The whole
	// point of this command is that no parked step stays invisible; an operator who has to
	// paginate by hand to avoid stranding funds is the failure it exists to prevent. Bounded so a
	// hostile or broken peer cannot spin us forever, and the bound is reported, never silent.
	const maxPages = 10
	all := []any{}
	truncated, warning, cursor := false, "", startCursor
	for page := 0; ; page++ {
		sig, ts, err := s.kernel.SignStepList(pub, peerKey)
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := s.stepRoundTrip(ctx, peerKey, fed.StepRequest{
			Kind: "list", Counterparty: pub, Timestamp: ts, Signature: sig, Cursor: cursor,
		}, fedOpTimeout)
		if err != nil {
			// Nothing collected yet: a hard error. Returning 200 with an empty list would read as
			// "nothing is parked for you" — the precise misreading that strands funds.
			if len(all) == 0 {
				writeErr(w, err)
				return
			}
			// Pages already in hand: report them with a warning rather than discarding them.
			// Partial visibility of parked funds beats none, which is this command's whole purpose.
			truncated = true
			warning = fmt.Sprintf("listing stopped after %d page(s): %v", page, err)
			break
		}
		steps, _ := body["steps"].([]any)
		all = append(all, steps...)
		more, _ := body["truncated"].(bool)
		if !more {
			break
		}
		// The peer claims more but sent nothing (or no cursor to advance by): stop, and say so —
		// silently reporting a complete list here would hide whatever it is still holding.
		next, _ := body["next_cursor"].(string)
		if len(steps) == 0 || next == "" || next == cursor {
			truncated = true
			break
		}
		cursor = next
		if page+1 >= maxPages {
			truncated = true
			break
		}
	}
	out := map[string]any{"steps": all}
	if truncated {
		out["truncated"] = true
		// Hand back where to resume. Without it a listing stopped by the page bound or by a peer
		// failure is a dead end: the remaining steps hold parked funds and no command could reach
		// them. `admin steps --after <cursor>` continues from here.
		if cursor != "" {
			out["next_cursor"] = cursor
		}
	}
	if warning != "" {
		out["warning"] = warning
	}
	writeJSON(w, http.StatusOK, out)
}

// ctlCompletePeerStep completes a step a peer parked for this kernel (§10, §13). The reply's
// receipt is displayed, not stored: the completion settles wholly on the peer, and this kernel
// parked nothing that a local transaction would have to settle against.
func (s *server) ctlCompletePeerStep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key    string          `json:"key"`
		StepID string          `json:"step_id"`
		Input  json.RawMessage `json:"input"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	ctx := r.Context()
	peerKey, err := s.resolvePeerKey(ctx, strings.TrimSpace(req.Key))
	if err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(req.StepID) == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrap("step_id is required"))
		return
	}
	// Normalize the input to exactly the bytes the transport will put on the wire before hashing
	// it: marshaling the outer StepRequest compacts and HTML-escapes an embedded RawMessage, so
	// hashing the caller's raw body would sign bytes the peer never sees. Marshaling a RawMessage
	// is idempotent, so this is a fixed point — send and hash the same slice (the convention the
	// federation call path follows, http_exec.go).
	input := []byte(req.Input)
	if len(input) == 0 {
		input = []byte("{}")
	}
	input, err = json.Marshal(json.RawMessage(input))
	if err != nil {
		writeErr(w, kernel.ErrInvalidInput.Wrap("input must be valid JSON"))
		return
	}
	inputHash := sha256HexBytes(input)

	pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	// Derive the idempotency key from the request rather than minting a fresh UUID per attempt:
	// a retry after a timeout must present the SAME key, or the peer cannot recognize it as a
	// duplicate and the real tx_id/receipt of an already-executed completion is lost. Same peer +
	// step + input ⇒ same key, with no state to persist (the remote-proxy path stores its key on
	// the trace; a step completion has no local trace to hang one on).
	idempotencyKey := sha256HexBytes([]byte("juice/fed/step/1|" + peerKey + "|" + req.StepID + "|" + inputHash))
	sig, ts, err := s.kernel.SignStep(req.StepID, pub, peerKey, idempotencyKey, inputHash)
	if err != nil {
		writeErr(w, err)
		return
	}
	body, err := s.stepRoundTrip(ctx, peerKey, fed.StepRequest{
		Kind: "complete", Counterparty: pub, Timestamp: ts, Signature: sig,
		StepID: req.StepID, IdempotencyKey: idempotencyKey, Input: json.RawMessage(input),
	}, fedStepTimeout)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// ctlIdentity reports this kernel's federation identity: public key, handle, and libp2p listen
// addresses. This is how an operator obtains the key to share for friending, now that the
// .well-known document is gone (§13).
func (s *server) ctlIdentity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	handle := globalCfg.KernelHandle
	var about string
	if sys, err := s.kernel.ReadUserByHandle(ctx, "@sys"); err == nil && sys != nil {
		about = sys.Description
	}
	var addrs []string
	if s.fed != nil {
		addrs = s.fed.ListenAddrs()
	}
	writeJSON(w, http.StatusOK, map[string]any{"handle": handle, "public_key": pub, "about": about, "addrs": addrs})
}

func (s *server) ctlUnsubscribePeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle string `json:"handle"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	// Accept @handle or the peer's key. Purely local (no transport), so it works whether or not the
	// peer is reachable. A clear not-found when the identifier names no known peer.
	u, err := resolveHandle(s.kernel, r.Context(), req.Handle)
	if err != nil {
		writeErr(w, kernel.ErrNotFound.Wrapf("no peer %q", req.Handle))
		return
	}
	// A local account with no key is not a peer: there is no catalog to drop.
	if u.PublicKey == "" {
		writeErr(w, kernel.ErrInvalidInput.Wrapf("%q is a local user, not a federation peer", req.Handle))
		return
	}
	err = s.kernel.Unsubscribe(r.Context(), callerFrom(r), u.Handle)
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

// manifestFetcher is the transport capability bulk import needs; *fed.Transport satisfies it,
// and tests supply a fake so the import + enable + publish logic is unit-testable without libp2p.
type manifestFetcher interface {
	Manifests(ctx context.Context, peerKey string) ([]json.RawMessage, error)
}

// bulkImportPeerActionsFed fetches a peer's manifests over the transport and imports them as
// enabled, local remote_proxy actions. Local (not public) keeps friendship non-transitive: a peer
// cannot reach this kernel's imports even by name, and they are never re-served in manifests/gossip
// (§13). Returns the counts imported and skipped.
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
		local := kernel.VisibilityLocal
		for _, act := range append(append(result.Created, result.Updated...), result.Unchanged...) {
			_ = enableAction(k, ctx, subjectID, act.ID)
			_, _ = k.UpdateAction(ctx, subjectID, kernel.UpdateActionRequest{ID: act.ID, Visibility: &local})
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
