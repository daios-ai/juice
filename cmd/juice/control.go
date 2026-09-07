package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"github.com/go-chi/chi/v5"
)

// Superuser supervision (admin/peer verbs — money, access, federation trust, roster) is served
// on the ordinary public TCP API, gated per-route by requireSuperuserMW (an IsSuperuser check),
// exactly as the widened list/disable scope already is (§14). There is no separate control
// surface: authority is the @sys bearer token, so keep it secret and run `serve` behind TLS or
// on loopback. The route registrations live in registerRoutes (serve.go); this file holds the
// superuser handlers and the federation-import helpers they call.

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
	acct, key, err := resolveMixed(s.kernel, r.Context(), chi.URLParam(r, "handle"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if key == "" {
		writeOr(w, acct, nil)
		return
	}
	// A kernel target renders one flat record: its naming state, plus what it owes us when it has
	// traded here. A peer account holds no balance of its own (P10), so none is shown.
	rk, _ := s.kernel.ReadKernel(r.Context(), key)
	out := map[string]any{"public_key": key}
	if rk != nil {
		out["petname"], out["nickname"], out["about"], out["rail_address"] = rk.Petname, rk.Nickname, rk.About, rk.RailAddress
		out["last_seen"] = rk.LastSeen
	}
	if acct != nil {
		out["id"], out["suspended_at"], out["created_at"] = acct.ID, acct.SuspendedAt, acct.CreatedAt
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) ctlSetSuspended(suspend bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident := chi.URLParam(r, "handle")
		acct, key, err := resolveMixed(s.kernel, r.Context(), ident)
		if err != nil {
			writeErr(w, err)
			return
		}
		switch {
		case suspend && key != "":
			// Suspending a kernel provisions its account and freezes it atomically, so a
			// not-yet-transacting kernel can be blocked before its first inbound call (§13).
			err = s.kernel.SuspendKernel(r.Context(), callerFrom(r), key)
		case acct == nil:
			err = kernel.ErrNotFound.Wrapf("%s has no account here", ident)
		case suspend:
			err = s.kernel.SuspendUser(r.Context(), callerFrom(r), acct.ID)
		default:
			err = s.kernel.UnsuspendUser(r.Context(), callerFrom(r), acct.ID)
		}
		writeOr(w, map[string]string{"target": ident}, err)
	}
}

func (s *server) ctlRenameUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NewName string `json:"new_name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	acct, key, err := resolveMixed(s.kernel, r.Context(), chi.URLParam(r, "handle"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if key != "" {
		petname, berr := s.kernel.RenameKernel(r.Context(), callerFrom(r), key, req.NewName)
		if berr != nil {
			writeErr(w, berr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"petname": petname, "public_key": key})
		return
	}
	out, err := s.kernel.RenameUser(r.Context(), callerFrom(r), acct.ID, req.NewName)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"handle": out.Handle})
}

// ctlDeposit records money that arrived from outside (U3). ref names the payment: the operator's own
// record of one where the world has no chain, the transaction that carried it where it has, or the
// settlement a peer says it has paid. Nothing is credited without it (D23).
func (s *server) ctlDeposit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle string `json:"handle"`
		Amount int64  `json:"amount"`
		Reason string `json:"reason"`
		Ref    string `json:"ref"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	u, key, err := resolveMixed(s.kernel, r.Context(), req.Handle)
	if err != nil {
		writeErr(w, err)
		return
	}
	if u == nil {
		// Deposit-by-kernel opens the billing account (§13): the provider's single deposit both
		// provisions and funds a not-yet-known kernel. Provisioning only — a deposit is not our
		// act of naming, so no petname is bound; the operator binds one with `admin rename`.
		if key == "" {
			writeErr(w, kernel.ErrNotFound.Wrapf("%s has no account here", req.Handle))
			return
		}
		if u, err = s.kernel.EnsureKernelAccount(r.Context(), key); err != nil {
			writeErr(w, err)
			return
		}
	}
	e, err := s.kernel.Deposit(r.Context(), callerFrom(r), u.ID, req.Amount, req.Reason, req.Ref)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, enrichLedger(e, newAccountCache(s.kernel, r.Context())))
}

// ctlListDeposits shows what is waiting on the operator, in one list: money that has arrived whose
// sender nobody has claimed, and obligations a buyer says it has paid whose money this kernel has
// not yet seen. Both wait on the same decision, so both belong in the same view.
func (s *server) ctlListDeposits(w http.ResponseWriter, r *http.Request) {
	ctx, caller := r.Context(), callerFrom(r)
	held, err := s.kernel.ListHeldDeposits(ctx, caller)
	if err != nil {
		writeErr(w, err)
		return
	}
	owed, err := s.kernel.ListOwed(ctx, caller)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, awaiting{
		Deposits: railTransferViews(s.kernel, ctx, held),
		Owed:     owedViews(s.kernel, ctx, owed),
	})
}

func (s *server) ctlListPeers(w http.ResponseWriter, r *http.Request) {
	// The roster (§14): every known kernel by public key — counterparties this kernel has traded
	// with and kernels known only from discovery — itself excluded, served by one store query.
	// Suspended counterparties are included only with ?all=1, like action list hides inactive.
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	limit, offset := listBounds(r)
	views, err := s.kernel.ListKernels(r.Context(), all, limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// ctlInspectPeer has defined behavior whether the peer is up or down (§13). It always reports
// reachability; a reachable peer yields live identity/actions/evidence; an unreachable but
// previously-known peer degrades to the last-known local data; a stranger that is unreachable
// yields an empty view with source="none". Every remote call is bounded by fedOpTimeout so an
// offline peer fails in seconds, not on the client timeout.
func (s *server) ctlInspectPeer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ident := strings.TrimSpace(r.URL.Query().Get("key"))
	// A local account with no public key is a plain user, not a federation peer, and an @handle
	// naming no account is not a peer either — resolvePeerKey rejects both rather than probing the
	// handle as if it were a key (inspect is a peer-only window, §13). An unresolvable non-@
	// identifier is a raw stranger key, which is exactly the inspect-before-peering case.
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
	// Retained evidence about this subject kernel, grouped by issuer (§13) — the reputation display
	// that replaces the deleted introducer roster. Local; works online or offline.
	if ev, eerr := s.kernel.SubjectEvidence(ctx, peerKey); eerr == nil {
		resp["evidence"] = ev
	}
	// Local account state when this peer has traded here (§14 inspect): whether it is suspended,
	// independent of whether it is currently reachable. No balance: a peer row holds no money (P10).
	if pu, _ := s.kernel.ReadAccountByKernelKey(ctx, peerKey); pu != nil {
		resp["account"] = map[string]any{"suspended": pu.SuspendedAt != nil}
	}

	// Live view when the peer answers: a fresh gossip pull (identity + own signed manifests).
	// The evidence page is ignored here; the persistent discovery loop ingests it.
	if gRaw, err := s.fed.Gossip(octx, peerKey, ""); err == nil {
		var g kernel.GossipResponse
		if json.Unmarshal(gRaw, &g) == nil {
			resp["nickname"], resp["public_key"] = g.Handle, g.PublicKey
			resp["petname"] = s.kernel.KernelName(ctx, g.PublicKey)
			if resp["petname"] == g.PublicKey {
				resp["petname"] = "" // unbound: KernelName falls back to the key
			}
			resp["about"] = g.About
			resp["actions"] = s.kernel.PeerCatalog(g.ActionManifests)
			resp["source"] = "live"
			if steps, serr := s.kernel.PeerStepsAwaitingUs(octx, peerKey); serr == nil && steps != nil {
				resp["steps"] = steps
			}
			// Inspect writes nothing (§14): the discovery loop owns cache refresh, so a diagnostic
			// read never makes retention or display state depend on being observed.
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}
	// Offline (or unparseable): fall back to what we hold locally — the slim discovered-kernel row
	// and/or a proxy-user identity — plus the cached discovery docs for this kernel (§13). A peer we
	// know locally (either way) is source="local"; a total stranger is "none".
	known := false
	if dk, _ := s.kernel.ReadKernel(ctx, peerKey); dk != nil {
		resp["petname"], resp["nickname"], resp["public_key"], resp["about"] = dk.Petname, dk.Nickname, dk.PublicKey, dk.About
		known = true
	}
	if known {
		resp["source"] = "local"
	} else {
		resp["petname"], resp["nickname"], resp["public_key"], resp["source"] = "", "", peerKey, "none"
	}
	// Same projection as the live branch, so the offline answer differs only in freshness — never in
	// shape, and never in what the price means.
	if acts, aerr := s.kernel.PeerCatalogCached(ctx, peerKey); aerr == nil {
		resp["actions"] = acts
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolvePeerKey maps an @handle / key / id reference to a peer's public key, rejecting a local
// account that is not a federation peer. Shared by the peer commands.
func (s *server) resolvePeerKey(ctx context.Context, ident string) (string, error) {
	if ident == "" {
		return "", kernel.ErrInvalidInput.Wrap("a peer handle or public key is required")
	}
	// Kernel namespace first: a petname or raw key names a kernel directly, with or without an
	// account here (§13). Only then fall back to the account namespace, to reject a local user.
	if key, _, err := s.kernel.ResolveKernelKey(ctx, ident); err == nil {
		return key, nil
	}
	if u, err := s.kernel.ResolveUser(ctx, ident); err == nil {
		if u.KernelPublicKey == "" {
			return "", kernel.ErrInvalidInput.Wrapf("%q is a local user, not a federation peer", ident)
		}
		return u.KernelPublicKey, nil
	}
	// An unresolvable identifier with public-key shape is a raw stranger key (inspecting or addressing
	// a peer before it is known locally) — the transport reports it unreachable if it is not real.
	// Anything else is simply an unknown peer (§14 productions: a bare handle never means a key).
	if kernel.IsPublicKey(ident) {
		return ident, nil
	}
	return "", kernel.ErrNotFound.Wrapf("no peer %q", ident)
}

// ctlIdentity reports this kernel's federation identity: public key, handle, and libp2p listen
// addresses. This is how an operator obtains the key to share for friending, now that the
// .well-known document is gone (§13).
func (s *server) ctlIdentity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pub, _ := s.kernel.GetConfig(ctx, configKeySigningPublic)
	handle := globalCfg.KernelHandle
	var about string
	if sys, err := s.kernel.ReadUserByHandle(ctx, "sys"); err == nil && sys != nil {
		about = sys.Description
	}
	var addrs []string
	if s.fed != nil {
		addrs = s.fed.ListenAddrs()
	}
	econ := s.kernel.Economy()
	exposure, _ := s.kernel.Exposure(ctx, callerFrom(r))
	out := map[string]any{"handle": handle, "public_key": pub, "about": about, "addrs": addrs,
		"lottery": econ.Lottery, "credit_limit": econ.CreditLimit, "exposure": exposure,
		"fee_bps": econ.FeeBPS, "remote_bps": econ.RemoteBPS, "import_bps": econ.ImportBPS}
	// The rail position: what is held, what is promised elsewhere, and whether the books still add
	// up (D23). An operator reads this before believing any other number here.
	if rep, err := s.kernel.RailInspect(ctx, callerFrom(r)); err == nil {
		out["network"] = rep.Network.Name
		out["network_digest"] = rep.Network.Digest
		out["rail_address"] = rep.Address
		out["finalized"] = rep.Finalized
		out["sys"] = map[string]any{"earnings": rep.Position.SysAvailable,
			"pending_payouts": rep.Position.PendingPayouts, "held_deposits": rep.Position.HeldDeposits,
			"refill_locks": rep.Position.RefillLocks}
		out["solvency"] = map[string]any{"liabilities": rep.Position.Liabilities,
			"vault": rep.Position.Vault, "gap": rep.Gap}
		out["custody"] = map[string]any{"checked": rep.CustodyChecked, "difference": rep.Custody, "ok": rep.CustodyOK}
		if rep.StopReason != "" {
			out["stop"] = map[string]any{"reason": rep.StopReason, "since": rep.StopSince}
		}
	}
	writeJSON(w, http.StatusOK, out)
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
// Client helpers
// ---------------------------------------------------------------------------
