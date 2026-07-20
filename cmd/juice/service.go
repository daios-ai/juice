package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

// ---- Types ----

// A user id is never a consumable CLI input — users are addressed by @handle everywhere — so the
// output views below render the party's @handle and drop the raw user UUID. The UUID lives on an
// embedded kernel struct, so to omit it we redeclare a same-JSON-named empty field with
// `,omitempty` at the outer level: the shallower field dominates the promoted one and, being empty,
// is omitted. (A plain `json:"-"` would NOT work — it only removes the outer field, leaving the
// promoted one to render.) Resolution is server-side, so HTTP and CLI stay in parity (§14).

// stepWithAction enriches a step with a computed @owner/name action field and the required caller's
// @handle. waiting_on_peer flags a waiting step whose required caller is a peer (proxy) user — work
// parked on someone who may be offline (§13); its age is the step's created_at.
type stepWithAction struct {
	*kernel.Step
	RequiredCallerUserID string `json:"required_caller_user_id,omitempty"`
	Action               string `json:"action,omitempty"`
	CreatedBy            string `json:"created_by,omitempty"` // creating action @owner/name (from parent trace)
	OwnerHandle          string `json:"owner_handle"`         // process owner (payer), like a transaction's owner_handle
	RequiredCallerHandle string `json:"required_caller_handle,omitempty"`
	WaitingOnPeer        bool   `json:"waiting_on_peer,omitempty"`
	// AllowedInput is the derived completion schema (input_schema \ keys(partial_args), §10) for a
	// waiting step, so the required caller can complete it without reading a private target action.
	AllowedInput map[string]any `json:"allowed_input,omitempty"`
}

// processView enriches a process with its owner @handle and awaiting-receipt state and age (§13): a
// process holding a remote-proxy call still waiting for its signed receipt, and when the earliest
// such call started — so an operator can see funds parked on an unreachable peer and for how long.
type processView struct {
	*kernel.Process
	OwnerUserID          string     `json:"owner_user_id,omitempty"`
	OwnerHandle          string     `json:"owner_handle"`
	AwaitingReceipt      bool       `json:"awaiting_receipt"`
	AwaitingReceiptSince *time.Time `json:"awaiting_receipt_since,omitempty"`
}

// txView enriches a transaction with the @handles of its three parties (payer, caller, payee),
// replacing the raw user UUIDs which no command consumes.
type txView struct {
	*kernel.TransactionView
	OwnerUserID  string `json:"owner_user_id,omitempty"`
	CallerUserID string `json:"caller_user_id,omitempty"`
	TargetUserID string `json:"target_user_id,omitempty"`
	OwnerHandle  string `json:"owner_handle"`
	CallerHandle string `json:"caller_handle"`
	TargetHandle string `json:"target_handle"`
}

// actionResp wraps an action with the computed @owner/name reference field and,
// for kind=http, a decomposed view of the request shape so manual and
// OpenAPI-imported actions read identically and round-trip with create/update.
// owner_user_id is shadow-dropped: owner_handle + action (@owner/name) already identify the owner.
type actionResp struct {
	*kernel.Action
	OwnerUserID   string    `json:"owner_user_id,omitempty"`
	ActionRef     string    `json:"action"`
	HTTP          *httpView `json:"http,omitempty"`
	AuthScheme    string    `json:"auth_scheme,omitempty"` // upstream auth scheme name (§8); present only when the action has auth; never config/secrets (R9)
	RequiresGrant bool      `json:"requires_grant"`        // true iff a caller must connect a per-caller grant first (delegated schemes)
	PeerState     string    `json:"peer_state,omitempty"`  // remote_proxy only: "offline" | "unfunded" from the §13 sync cache; omitted when healthy. Display-only.
}

// httpView is the read-side decomposition of an action's HTTPSource. It carries
// no secrets (auth_json is never surfaced, §8).
type httpView struct {
	Method string             `json:"method"`
	URL    string             `json:"url"`
	Params []kernel.HTTPParam `json:"params,omitempty"`
}

// ---- Enrichment helpers ----

// userCache resolves user IDs to display info within one request, reading each user at most once
// (transaction lists reference few distinct users across many rows). handle() falls back to the raw
// id only when the row is truly gone (a purged peer, §13), so an immutable ledger stays legible.
type userCache struct {
	k   *kernel.Kernel
	ctx context.Context
	m   map[string]*kernel.User
}

func newUserCache(k *kernel.Kernel, ctx context.Context) *userCache {
	return &userCache{k: k, ctx: ctx, m: map[string]*kernel.User{}}
}

func (c *userCache) get(id string) *kernel.User {
	if u, ok := c.m[id]; ok {
		return u
	}
	u, _ := c.k.ReadUser(c.ctx, id) // nil on error; cached so a bad id isn't re-read
	c.m[id] = u
	return u
}

func (c *userCache) handle(id string) string {
	if id == "" {
		return ""
	}
	if u := c.get(id); u != nil && u.Handle != "" {
		return u.Handle
	}
	return id
}

func (c *userCache) isPeer(id string) bool {
	u := c.get(id)
	return u != nil && u.PublicKey != ""
}

func enrichStep(k *kernel.Kernel, ctx context.Context, step *kernel.Step, action *kernel.Action, uc *userCache) *stepWithAction {
	v := &stepWithAction{Step: step, RequiredCallerHandle: uc.handle(step.RequiredCallerUserID)}
	if action != nil {
		v.Action = action.OwnerHandle + "/" + action.Name
	}
	// The creating action (what produced this step) carries its meaning; the target action can be a
	// generic sink (e.g. @sys/message parks a @sys/sink step). Resolve it from the parent trace.
	if step.ParentTraceID != nil {
		if tr, err := k.ReadTrace(ctx, *step.ParentTraceID); err == nil {
			v.CreatedBy = k.ActionRef(ctx, tr.ActionID)
			// The step's process owner is the payer of the transaction it will settle into (§10).
			v.OwnerHandle = uc.handle(k.ProcessOwnerID(ctx, tr.ProcessID))
		}
	}
	if step.Status == kernel.StepWaiting {
		v.WaitingOnPeer = uc.isPeer(step.RequiredCallerUserID)
		if action != nil {
			v.AllowedInput = kernel.DeriveAllowedSchema(action.InputSchema, step.PartialArgs)
		}
	}
	return v
}

// enrichProcess resolves the owner @handle and flags a process awaiting a remote receipt, with the
// earliest such call's start time from the awaiting-receipt map (kernel.AwaitingReceiptSince).
func enrichProcess(p *kernel.Process, since map[string]time.Time, uc *userCache) *processView {
	v := &processView{Process: p, OwnerHandle: uc.handle(p.OwnerUserID)}
	if t, ok := since[p.ID]; ok {
		tt := t
		v.AwaitingReceipt = true
		v.AwaitingReceiptSince = &tt
	}
	return v
}

// ledgerView renders a ledger entry (deposit, withdrawal, or transfer) with the operator,
// source, and destination @handles instead of raw user UUIDs; the record's own id is dropped
// too (no command consumes it — external_key is the idempotency handle). from_handle is absent
// on a deposit (no source) and to_handle on a withdrawal (no destination).
type ledgerView struct {
	*kernel.LedgerEntry
	ID             string `json:"id,omitempty"`
	OperatorUserID string `json:"operator_user_id,omitempty"`
	FromUserID     string `json:"from_user_id,omitempty"`
	ToUserID       string `json:"to_user_id,omitempty"`
	OperatorHandle string `json:"operator_handle"`
	FromHandle     string `json:"from_handle,omitempty"`
	ToHandle       string `json:"to_handle,omitempty"`
}

func enrichLedger(e *kernel.LedgerEntry, uc *userCache) *ledgerView {
	v := &ledgerView{LedgerEntry: e, OperatorHandle: uc.handle(e.OperatorUserID)}
	if e.FromUserID != "" {
		v.FromHandle = uc.handle(e.FromUserID)
	}
	if e.ToUserID != "" {
		v.ToHandle = uc.handle(e.ToUserID)
	}
	return v
}

// peerViews projects proxy-peer users into handle+key+balance views (plus the §13 sync cache:
// our credit on the peer and when we last reached it), dropping their internal ids.
func peerViews(peers []*kernel.User) []*kernel.PeerView {
	out := make([]*kernel.PeerView, len(peers))
	for i, p := range peers {
		out[i] = &kernel.PeerView{
			Handle: p.Handle, PublicKey: p.PublicKey,
			Available: p.Available, Locked: p.Locked, SuspendedAt: p.SuspendedAt,
			PeerCredit: p.PeerCredit, LastSeen: p.PeerLastSeen,
		}
	}
	return out
}

// enrichTx resolves the @handles of a transaction's three parties (payer, caller, payee).
func enrichTx(tv *kernel.TransactionView, uc *userCache) *txView {
	return &txView{
		TransactionView: tv,
		OwnerHandle:     uc.handle(tv.OwnerUserID),
		CallerHandle:    uc.handle(tv.CallerUserID),
		TargetHandle:    uc.handle(tv.TargetUserID),
	}
}

func enrichAction(k *kernel.Kernel, a *kernel.Action) actionResp {
	ref := ""
	if a.OwnerHandle != "" && a.Name != "" {
		ref = a.OwnerHandle + "/" + a.Name
	}
	scheme, requiresGrant := k.ActionAuthInfo(a)
	return actionResp{Action: a, ActionRef: ref, HTTP: httpViewOf(a), AuthScheme: scheme, RequiresGrant: requiresGrant}
}

// peerStateStaleAfter is how old a peer's last sync may be before its proxies read as offline (§13):
// 3× the discovery interval tolerates a couple of missed passes before flagging.
func peerStateStaleAfter() time.Duration { return 3 * globalCfg.discoveryInterval() }

// peerStateFor annotates a remote_proxy action with its peer's cached liveness/funding (§13),
// display-only. "offline": the peer's last successful gossip sync is missing or older than
// staleAfter. "unfunded": our cached credit on the peer is below the action's remote manifest price.
// "offline" takes precedence — a stale credit figure is not actionable. "" when healthy or the
// owner row is gone.
func peerStateFor(k *kernel.Kernel, owner *kernel.User, price int64, staleAfter time.Duration) string {
	if owner == nil {
		return ""
	}
	if owner.PeerLastSeen == nil || time.Since(*owner.PeerLastSeen) > staleAfter {
		return "offline"
	}
	if owner.PeerCredit != nil && *owner.PeerCredit < k.RemoteManifestPrice(price) {
		return "unfunded"
	}
	return ""
}

// httpViewOf decomposes a kind=http action's stored HTTPSource into a uniform
// {method, url, params} view, or nil if the action is not http or has no source.
func httpViewOf(a *kernel.Action) *httpView {
	if a.Kind != kernel.KindHTTP || a.Source == "" {
		return nil
	}
	var s kernel.HTTPSource
	if err := json.Unmarshal([]byte(a.Source), &s); err != nil {
		return nil
	}
	return &httpView{Method: s.Method, URL: s.BaseURL + s.Path, Params: s.Params}
}

// parseParams parses repeatable "name:in" CLI bindings into HTTPParam values.
func parseParams(specs []string) ([]kernel.HTTPParam, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	params := make([]kernel.HTTPParam, 0, len(specs))
	for _, s := range specs {
		name, in, ok := strings.Cut(s, ":")
		if !ok || name == "" || in == "" {
			return nil, fmt.Errorf("invalid --param %q: expected name:in (in=path|query|body)", s)
		}
		params = append(params, kernel.HTTPParam{Name: name, In: in})
	}
	return params, nil
}

func userView(u *kernel.User) map[string]any {
	return map[string]any{
		"id":          u.ID,
		"handle":      u.Handle,
		"description": u.Description,
		"available":   u.Available,
		"locked":      u.Locked,
	}
}

// ---- Resolution helpers ----

// resolveHandle resolves an account by its @handle (kernel-local name) or public key (global name):
// an @-prefixed string is a handle, a bare string is tried as a key first, then a handle.
func resolveHandle(k *kernel.Kernel, ctx context.Context, ident string) (*kernel.User, error) {
	return k.ResolveUser(ctx, ident)
}

// resolveActionRef resolves "@owner/name" (with or without a leading "@") or a raw action ID.
func resolveActionRef(k *kernel.Kernel, ctx context.Context, ref string) (*kernel.Action, error) {
	return k.ResolveAction(ctx, ref)
}

// ---- User operations ----

func createUser(k *kernel.Kernel, ctx context.Context, req kernel.CreateUserRequest) (map[string]any, error) {
	u, err := k.CreateUser(ctx, req)
	if err != nil {
		return nil, err
	}
	return userView(u), nil
}

// connectorView is one directory in the GET /v1/me tree (§8): the actions the caller has granted
// under one folder (the action ref up to its last "/", e.g. @chat or @chat/inbox), grouped with the
// upstream account(s) whose credential backs them. The directory is a display grouping only — it
// never gates a credential; the token binding stays per-action and fact-derived (§8 confused-deputy
// defense), so grouping by it changes nothing about which credential dispatch applies.
type connectorView struct {
	Directory   string                   `json:"directory"`   // the folder the actions live in (@owner or @owner/path)
	Connections []*kernel.ConnectionView `json:"connections"` // upstream account(s) backing this directory (usually one)
	Actions     []*kernel.GrantView      `json:"actions"`     // token-free granted actions under this directory
}

// directoryOf returns the folder a granted action belongs to: its ref up to the LAST "/", so
// @chat/inbox/send and @chat/inbox/read both group under @chat/inbox (not a flat @chat). A
// top-level action like @chat/create-room groups under @chat. Falls back to the whole ref when the
// action did not resolve to @owner/name (an unbackfilled legacy grant showing a raw id).
func directoryOf(actionRef string) string {
	if i := strings.LastIndexByte(actionRef, '/'); i >= 0 {
		return actionRef[:i]
	}
	return actionRef
}

func getMe(k *kernel.Kernel, ctx context.Context, callerID string) (map[string]any, error) {
	u, err := k.ReadUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	view := userView(u)

	grants, err := k.ListGrantViews(ctx, callerID)
	if err != nil {
		return nil, err
	}
	conns, err := k.ListConnectionViews(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if conns == nil {
		conns = []*kernel.ConnectionView{}
	}
	connByKey := make(map[string]*kernel.ConnectionView, len(conns))
	for _, c := range conns {
		connByKey[c.ProviderKey] = c
	}

	// Group grants into a directory tree: one node per connector (owner namespace), each carrying
	// the account(s) that back it. Grants are token-free (§8); grouping is display only.
	order := []string{}
	byDir := map[string]*connectorView{}
	for _, g := range grants {
		dir := directoryOf(g.Action)
		node := byDir[dir]
		if node == nil {
			node = &connectorView{Directory: dir, Connections: []*kernel.ConnectionView{}, Actions: []*kernel.GrantView{}}
			byDir[dir] = node
			order = append(order, dir)
		}
		node.Actions = append(node.Actions, g)
	}
	for _, node := range byDir {
		sort.Slice(node.Actions, func(i, j int) bool { return node.Actions[i].Action < node.Actions[j].Action })
		seen := map[string]bool{}
		for _, g := range node.Actions {
			if g.ProviderKey == "" || seen[g.ProviderKey] {
				continue
			}
			seen[g.ProviderKey] = true
			if c := connByKey[g.ProviderKey]; c != nil {
				node.Connections = append(node.Connections, c)
			}
		}
	}
	sort.Strings(order)
	connectors := make([]*connectorView, 0, len(order))
	for _, dir := range order {
		connectors = append(connectors, byDir[dir])
	}
	view["connectors"] = connectors
	// The full account inventory, so a connection with zero grants still surfaces as unused (§8/§14).
	view["connections"] = conns
	return view, nil
}

// planGrants expands a selector into the consent plan user connect walks (§8).
func planGrants(k *kernel.Kernel, ctx context.Context, callerID, selector string) (*kernel.ConsentPlan, error) {
	return k.ConsentPlan(ctx, callerID, selector)
}

// grantGroup finds the plan group for a provider_key; when provider is empty it must resolve to a
// single group.
func grantGroup(plan *kernel.ConsentPlan, provider string) (*kernel.ConsentGroup, error) {
	if provider == "" {
		if len(plan.Groups) != 1 {
			return nil, kernel.ErrInvalidInput.Wrap("selector resolves to multiple providers; specify one")
		}
		return &plan.Groups[0], nil
	}
	for i := range plan.Groups {
		if plan.Groups[i].ProviderKey == provider {
			return &plan.Groups[i], nil
		}
	}
	return nil, kernel.ErrNotFound.Wrap("no connectable actions for that provider in the selector")
}

func groupActionIDs(g *kernel.ConsentGroup) []string {
	ids := make([]string, len(g.Actions))
	for i, a := range g.Actions {
		ids[i] = a.ActionID
	}
	return ids
}

func grantRefs(k *kernel.Kernel, ctx context.Context, grants []*kernel.Grant) []string {
	refs := make([]string, len(grants))
	for i, g := range grants {
		refs[i] = k.ActionRef(ctx, g.ActionID)
	}
	return refs
}

// startGrant begins delegated-OAuth consent for one provider group of a selector (§8). When the
// caller's connection already covers the group's scope union, it mints the grants instantly and
// returns {status:"granted"}; otherwise it drives the broker's browser/device flow.
func startGrant(k *kernel.Kernel, broker *grantBroker, ctx context.Context, callerID, selector, provider, redirectURI, flow string) (any, error) {
	if broker == nil {
		return nil, kernel.ErrInvalidState.Wrap("OAuth consent is not configured on this server")
	}
	plan, err := k.ConsentPlan(ctx, callerID, selector)
	if err != nil {
		return nil, err
	}
	g, err := grantGroup(plan, provider)
	if err != nil {
		return nil, err
	}
	ids := groupActionIDs(g)
	scopesJSON, _ := json.Marshal(g.Scopes)
	if g.Covered {
		grants, err := k.CreateGrants(ctx, callerID, g.ProviderKey, ids, "", string(scopesJSON))
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "granted", "provider": g.Provider, "actions": grantRefs(k, ctx, grants)}, nil
	}
	auth, err := k.DelegatedAuthConfig(ctx, callerID, ids[0])
	if err != nil {
		return nil, err
	}
	// Request the union of the group's scopes in the single browser step.
	if auth.Config == nil {
		auth.Config = map[string]any{}
	}
	auth.Config["scopes"] = strings.Join(g.Scopes, " ")
	if flow == "" {
		flow = "code"
	}
	return broker.start(ctx, callerID, ids, g.ProviderKey, string(scopesJSON), auth, redirectURI, flow)
}

// completeGrant finishes a consent and, on success, homes the refresh token on the group's
// connection and mints one grant per action (§8).
func completeGrant(k *kernel.Kernel, broker *grantBroker, ctx context.Context, callerID, state, code string) (map[string]any, error) {
	if broker == nil {
		return nil, kernel.ErrInvalidState.Wrap("OAuth consent is not configured on this server")
	}
	res, err := broker.complete(ctx, state, callerID, code)
	if err != nil {
		return nil, err
	}
	if res.Status == "pending" {
		return map[string]any{"status": "pending"}, nil
	}
	grants, err := k.CreateGrants(ctx, callerID, res.ProviderKey, res.ActionIDs, res.Refresh, res.ScopesJSON)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "complete", "provider": kernel.ProviderLabel(res.ProviderKey), "actions": grantRefs(k, ctx, grants), "created_at": grants[0].CreatedAt}, nil
}

// attachToken stores a caller-supplied static token across a selector's delegated_bearer group
// (§8): the direct non-OAuth twin of the start/complete consent flow, minting one grant per action
// against one connection. The raw token never appears in any read path.
func attachToken(k *kernel.Kernel, ctx context.Context, callerID, selector, provider, token string) (map[string]any, error) {
	grants, err := k.AttachBearerGrants(ctx, callerID, selector, provider, token)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "connected", "provider": kernel.ProviderLabel(provider), "actions": grantRefs(k, ctx, grants), "created_at": grants[0].CreatedAt}, nil
}

// revokeGrantsBySelector deletes the caller's grants matching a selector (grants only, §8).
func revokeGrantsBySelector(k *kernel.Kernel, ctx context.Context, callerID, selector string) (map[string]any, error) {
	revoked, err := k.RevokeGrantsBySelector(ctx, callerID, selector)
	if err != nil {
		return nil, err
	}
	return map[string]any{"revoked": revoked}, nil
}

// revokeConnection deletes the caller's connection for a provider and cascades its grants (§8).
func revokeConnection(k *kernel.Kernel, ctx context.Context, callerID, providerKey string) (map[string]any, error) {
	revoked, err := k.RevokeConnection(ctx, callerID, providerKey)
	if err != nil {
		return nil, err
	}
	return map[string]any{"revoked": revoked, "connection": kernel.ProviderLabel(providerKey)}, nil
}

func updateMe(k *kernel.Kernel, ctx context.Context, callerID string, description *string, currentPwd, newPwd string) (map[string]any, error) {
	u, err := k.UpdateUser(ctx, callerID, kernel.UpdateUserRequest{
		Description:     description,
		CurrentPassword: currentPwd,
		NewPassword:     newPwd,
	})
	if err != nil {
		return nil, err
	}
	return userView(u), nil
}

// startRecovery issues a password-recovery challenge nonce (§12).
func startRecovery(k *kernel.Kernel, ctx context.Context, handle string) (map[string]any, error) {
	nonce, err := k.StartRecovery(ctx, handle)
	if err != nil {
		return nil, err
	}
	return map[string]any{"nonce": nonce, "expires_in_seconds": 600}, nil
}

// completeRecovery verifies the signed challenge and resets the password (§12).
func completeRecovery(k *kernel.Kernel, ctx context.Context, handle, nonce, signature, newPwd string) (map[string]any, error) {
	if err := k.CompleteRecovery(ctx, handle, nonce, signature, newPwd); err != nil {
		return nil, err
	}
	return map[string]any{"status": "ok"}, nil
}

// ---- Action operations ----

func createAction(k *kernel.Kernel, ctx context.Context, callerID string, req kernel.CreateActionRequest) (actionResp, error) {
	a, err := k.CreateAction(ctx, callerID, req)
	if err != nil {
		return actionResp{}, err
	}
	// Re-read to populate OwnerHandle via store JOIN.
	full, err := k.ReadActionForSubject(ctx, callerID, a.ID)
	if err != nil {
		return actionResp{}, err
	}
	return enrichAction(k, full), nil
}

func getAction(k *kernel.Kernel, ctx context.Context, callerID, id string) (actionResp, error) {
	a, err := k.ReadActionForSubject(ctx, callerID, id)
	if err != nil {
		return actionResp{}, err
	}
	r := enrichAction(k, a)
	if a.Kind == kernel.KindRemoteProxy {
		owner, _ := k.ReadUser(ctx, a.OwnerUserID)
		r.PeerState = peerStateFor(k, owner, a.Price, peerStateStaleAfter())
	}
	return r, nil
}

func updateAction(k *kernel.Kernel, ctx context.Context, callerID string, req kernel.UpdateActionRequest) (actionResp, error) {
	a, err := k.UpdateAction(ctx, callerID, req)
	if err != nil {
		return actionResp{}, err
	}
	return enrichAction(k, a), nil
}

// listPublicActions returns actions visible to the caller, optionally filtered by owner handle and name.
// Unauthenticated: active public actions only.
// Authenticated (no owner filter): active public+local actions union caller's own active actions, deduplicated.
// Authenticated with owner filter resolving to caller: all their actions regardless of active/visibility.
// Source and ArtifactHash are stripped from all results.
func listPublicActions(k *kernel.Kernel, ctx context.Context, callerID, ownerHandle, name string, includeInactive bool, limit, offset int) ([]actionResp, error) {
	// The superuser sees every owner's rows (supervision is scope on the normal endpoint, §14);
	// everyone else starts from the public+active set and unions their own below. Inactive rows are
	// dropped at the end unless includeInactive (the `all` param / `--all`) is set — so the default
	// list is active-only for everyone, like `docker ps`.
	superuser := callerID != "" && k.IsSuperuser(ctx, callerID)
	var actions []*kernel.Action
	var err error
	if superuser {
		actions, err = k.ListAllActions(ctx, limit, offset)
	} else {
		// Authenticated (session) callers are local users, so they see local actions too; an
		// unauthenticated listing sees public only (§4/§14).
		actions, err = k.ListVisibleActions(ctx, callerID != "", limit, offset)
	}
	if err != nil {
		return nil, err
	}
	if ownerHandle != "" {
		u, err := k.ReadUserByHandle(ctx, ownerHandle)
		if err != nil {
			return []actionResp{}, nil
		}
		if !superuser && callerID != "" && callerID == u.ID {
			// §3: an owner may list all their own actions regardless of active/public.
			actions, err = k.ListOwnedActions(ctx, u.ID, limit, offset)
			if err != nil {
				return nil, err
			}
			includeInactive = true
		} else {
			filtered := actions[:0]
			for _, a := range actions {
				if a.OwnerUserID == u.ID {
					filtered = append(filtered, a)
				}
			}
			actions = filtered
			if superuser {
				// Scoping supervision to one owner is an explicit request for that owner's
				// full picture (§3), so inactive rows show without needing `--all`.
				includeInactive = true
			}
		}
	} else if !superuser && callerID != "" {
		owned, err := k.ListOwnedActions(ctx, callerID, limit, offset)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool, len(actions))
		for _, a := range actions {
			seen[a.ID] = true
		}
		for _, a := range owned {
			if (a.Active || includeInactive) && !seen[a.ID] {
				actions = append(actions, a)
			}
		}
	}
	if !includeInactive {
		kept := actions[:0]
		for _, a := range actions {
			if a.Active {
				kept = append(kept, a)
			}
		}
		actions = kept
	}
	if name != "" {
		filtered := actions[:0]
		for _, a := range actions {
			if a.Name == name {
				filtered = append(filtered, a)
			}
		}
		actions = filtered
	}
	resps := make([]actionResp, len(actions))
	uc := newUserCache(k, ctx) // shared so listing is O(distinct peer owners), not O(rows)
	staleAfter := peerStateStaleAfter()
	for i, a := range actions {
		cp := *a
		r := enrichAction(k, &cp) // decompose http view before hiding the raw blob
		if cp.Kind == kernel.KindRemoteProxy {
			r.PeerState = peerStateFor(k, uc.get(cp.OwnerUserID), cp.Price, staleAfter)
		}
		cp.Source = ""
		cp.ArtifactHash = ""
		resps[i] = r
	}
	if resps == nil {
		resps = []actionResp{}
	}
	return resps, nil
}

func enableAction(k *kernel.Kernel, ctx context.Context, callerID, id string) error {
	return k.SetActive(ctx, callerID, id, true)
}

func disableAction(k *kernel.Kernel, ctx context.Context, callerID, id string) error {
	return k.SetActive(ctx, callerID, id, false)
}

func deleteAction(k *kernel.Kernel, ctx context.Context, callerID, id string) error {
	return k.DeleteAction(ctx, callerID, id)
}

func actionStats(k *kernel.Kernel, ctx context.Context, id string) (*kernel.Stats, error) {
	return k.ReadStats(ctx, id)
}

// ---- Process operations ----

func listProcesses(k *kernel.Kernel, ctx context.Context, callerID string, limit, offset int) ([]*processView, error) {
	processes, err := k.ListProcesses(ctx, callerID, limit, offset)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(processes))
	for i, p := range processes {
		ids[i] = p.ID
	}
	since, err := k.AwaitingReceiptSince(ctx, ids)
	if err != nil {
		return nil, err
	}
	uc := newUserCache(k, ctx)
	views := make([]*processView, len(processes))
	for i, p := range processes {
		views[i] = enrichProcess(p, since, uc)
	}
	return views, nil
}

func getProcess(k *kernel.Kernel, ctx context.Context, callerID, id string) (*processView, error) {
	p, err := k.ReadProcess(ctx, callerID, id)
	if err != nil {
		return nil, err
	}
	since, err := k.AwaitingReceiptSince(ctx, []string{p.ID})
	if err != nil {
		return nil, err
	}
	return enrichProcess(p, since, newUserCache(k, ctx)), nil
}

func endProcess(k *kernel.Kernel, ctx context.Context, callerID, id string) error {
	return k.EndProcess(ctx, callerID, id)
}

// ---- Step operations ----

type createStepParams struct {
	TraceID        string
	ActionRef      string
	RequiredCaller string
	PartialArgs    json.RawMessage
	// ViaCapability skips the precondition-4 trace-use check: a capability's trace authority is
	// the executing action owning that trace (§9), matching the WASM juice.step_create path, which
	// calls CreateStep directly without an external authorization check.
	ViaCapability bool
}

// createStep resolves ActionRef and RequiredCaller, enforces precondition-4 for the trace,
// calls kernel.CreateStep, and returns an enriched *stepWithAction.
// Used by both HTTP and CLI surfaces.
func createStep(k *kernel.Kernel, ctx context.Context, callerID string, p createStepParams) (*stepWithAction, error) {
	action, err := resolveActionRef(k, ctx, p.ActionRef)
	if err != nil {
		return nil, err
	}
	callerUser, err := resolveHandle(k, ctx, p.RequiredCaller)
	if err != nil {
		return nil, fmt.Errorf("required_caller not found: %w", err)
	}
	// Precondition-4: external (JWT) caller must be authorized to use the trace; a capability
	// carries that authority in the token itself.
	if !p.ViaCapability {
		if err := k.AuthorizeTraceUse(ctx, callerID, p.TraceID); err != nil {
			return nil, err
		}
	}
	step, err := k.CreateStep(ctx, p.TraceID, action.ID, p.PartialArgs, callerUser.ID)
	if err != nil {
		return nil, err
	}
	uc := newUserCache(k, ctx)
	uc.m[callerUser.ID] = callerUser // already resolved; avoid a redundant read
	return enrichStep(k, ctx, step, action, uc), nil
}

func listSteps(k *kernel.Kernel, ctx context.Context, callerID, processID, status string, limit, offset int) ([]*stepWithAction, error) {
	steps, err := k.ListSteps(ctx, callerID, processID, status, limit, offset)
	if err != nil {
		return nil, err
	}
	views := make([]*stepWithAction, len(steps))
	uc := newUserCache(k, ctx)
	for i, step := range steps {
		action, _ := k.ReadAction(ctx, step.ActionID)
		views[i] = enrichStep(k, ctx, step, action, uc)
	}
	return views, nil
}

func getStep(k *kernel.Kernel, ctx context.Context, callerID, id string) (*stepWithAction, error) {
	step, err := k.ReadStep(ctx, callerID, id)
	if err != nil {
		return nil, err
	}
	action, _ := k.ReadAction(ctx, step.ActionID)
	return enrichStep(k, ctx, step, action, newUserCache(k, ctx)), nil
}

func completeStep(k *kernel.Kernel, ctx context.Context, callerID, id string, args json.RawMessage) (*kernel.StepReply, error) {
	return k.CompleteStep(ctx, callerID, id, args)
}

// ---- Transaction operations ----

func listTransactions(k *kernel.Kernel, ctx context.Context, callerID string, f kernel.TxFilter) ([]*txView, error) {
	txs, err := k.ListTransactions(ctx, callerID, f)
	if err != nil {
		return nil, err
	}
	uc := newUserCache(k, ctx)
	views := make([]*txView, len(txs))
	for i, tv := range txs {
		views[i] = enrichTx(tv, uc)
	}
	return views, nil
}

func getTransaction(k *kernel.Kernel, ctx context.Context, callerID, id string) (*txView, error) {
	tv, err := k.ReadTransaction(ctx, callerID, id)
	if err != nil {
		return nil, err
	}
	return enrichTx(tv, newUserCache(k, ctx)), nil
}

// validateRating returns ErrInvalidInput if v is not 0 or 1.
func validateRating(v float64) error {
	if v != 0 && v != 1 {
		return kernel.ErrInvalidInput.Wrap("rating must be 0 or 1")
	}
	return nil
}

func rateTransaction(k *kernel.Kernel, ctx context.Context, callerID, id string, rating float64, note *string) (*kernel.Rating, error) {
	if err := validateRating(rating); err != nil {
		return nil, err
	}
	return k.RateTransaction(ctx, callerID, id, rating, note)
}

func verifyReceipt(k *kernel.Kernel, ctx context.Context, callerID, id string) (*kernel.ReceiptVerification, error) {
	return k.VerifyRemoteReceipt(ctx, callerID, id)
}

// ---- Run ----

func run(k *kernel.Kernel, ctx context.Context, callerID, actionRef string, args map[string]any) (*kernel.CallReply, error) {
	return k.Run(ctx, callerID, actionRef, args)
}

// ---- Federation ----

// checkFederationTimestamp enforces the §13 ±5 minute freshness window on a signed request.
func checkFederationTimestamp(tsStr string) error {
	ts, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return kernel.ErrUnauthenticated.Wrap("timestamp must be RFC3339")
	}
	if diff := time.Since(ts); diff < -5*time.Minute || diff > 5*time.Minute {
		return kernel.ErrUnauthenticated.Wrap("timestamp out of range")
	}
	return nil
}

// peerStepView is what a remote peer may see of a step parked for it: the request, not the
// requester. Deliberately NOT stepWithAction — that is the local operator's view, and reusing it
// shipped a peer the creating action's name, the process owner's @handle, and raw local ids.
// A user identity crossing a kernel boundary is precisely what §5's encapsulation forbids, so the
// peer-facing shape is a separate type whose fields must each be justified rather than inherited.
//
// Kept, because each is bound FOR the completer: partial_args is the payload channel (§14 has
// @sys/message put its body there so the recipient can read it), and allowed_input is §14's
// explicit substitute for reading a target action that may be private. Everything else — the
// action ref (it names a local owner), created_by, owner_handle, and every trace/action/tx id —
// is local composition detail the completer does not need in order to complete.
type peerStepView struct {
	ID           string          `json:"id"`
	PartialArgs  json.RawMessage `json:"partial_args,omitempty"`
	AllowedInput map[string]any  `json:"allowed_input,omitempty"`
	Price        int64           `json:"price"`
	CreatedAt    time.Time       `json:"created_at"`
}

func newPeerStepView(s *kernel.Step, action *kernel.Action) *peerStepView {
	v := &peerStepView{ID: s.ID, PartialArgs: s.PartialArgs, Price: s.Price, CreatedAt: s.CreatedAt}
	if action != nil {
		v.AllowedInput = kernel.DeriveAllowedSchema(action.InputSchema, s.PartialArgs)
	}
	return v
}

// maxPeerStepPage bounds one step-list reply. Reaching it sets `truncated` rather than silently
// dropping the tail: an operator must never read a capped page as "nothing is parked for you".
const maxPeerStepPage = 200

// handleFederationStepList returns the waiting steps whose required caller is the requesting peer
// (§10, §13). Read-only: an unknown key gets an empty list rather than a lazily provisioned account
// — provisioning is reserved for a call, which is what actually creates a billing relationship.
func handleFederationStepList(k *kernel.Kernel, ctx context.Context, cpPubKey, tsStr, sigStr, cursor string) (int, map[string]any, error) {
	if err := checkFederationTimestamp(tsStr); err != nil {
		return 0, nil, err
	}
	self, err := k.GetConfig(ctx, configKeySigningPublic)
	if err != nil || self == "" {
		return 0, nil, kernel.ErrInvalidState.Wrap("signing key not configured")
	}
	if err := kernel.VerifyStepListSignature(cpPubKey, cpPubKey, self, tsStr, sigStr); err != nil {
		return 0, nil, err
	}
	// A store failure must not read as "nothing is parked for you" — that is precisely the
	// conclusion which leaves funds stranded. Only a genuinely absent key gets the empty list.
	peer, err := k.ReadUserByPublicKey(ctx, cpPubKey)
	if err != nil && !errors.Is(err, kernel.ErrNotFound) {
		return 0, nil, err
	}
	if peer == nil || peer.PublicKey == "" {
		return http.StatusOK, map[string]any{"steps": []*stepWithAction{}}, nil
	}
	// Scoped in SQL, oldest first: ListSteps' predicate also matches every step inside a process
	// this peer owns (its own inbound calls), which would crowd the completable ones out of the
	// page. A suspended peer is refused by requireActiveUser inside the kernel call.
	steps, nextCursor, err := k.ListStepsAwaitingCaller(ctx, peer.ID, maxPeerStepPage, cursor)
	if err != nil {
		return 0, nil, err
	}
	views := make([]*peerStepView, len(steps))
	for i, s := range steps {
		action, _ := k.ReadAction(ctx, s.ActionID)
		views[i] = newPeerStepView(s, action)
	}
	body := map[string]any{"steps": views}
	if len(views) == maxPeerStepPage {
		body["truncated"] = true
		body["next_cursor"] = nextCursor
	}
	return http.StatusOK, body, nil
}

// handleFederationStepComplete resumes a waiting step on behalf of the requesting peer (§10, §13).
// Unlike a call, the requester parks nothing locally — the step's price was parked here at creation
// — so failures are plain typed errors: there is no remote trace awaiting a signed rejection.
func handleFederationStepComplete(k *kernel.Kernel, ctx context.Context, cpPubKey, tsStr, idempotencyKey, stepID, sigStr string, rawInput []byte) (int, map[string]any, error) {
	if err := checkFederationTimestamp(tsStr); err != nil {
		return 0, nil, err
	}
	self, err := k.GetConfig(ctx, configKeySigningPublic)
	if err != nil || self == "" {
		return 0, nil, kernel.ErrInvalidState.Wrap("signing key not configured")
	}
	if err := kernel.VerifyStepSignature(cpPubKey, stepID, cpPubKey, self, idempotencyKey, tsStr, sha256HexBytes(rawInput), sigStr); err != nil {
		return 0, nil, err
	}
	// A stranger can hold no step here: CreateStep resolves required_caller to an existing user,
	// so an unknown key is necessarily not the required caller of anything.
	peer, err := k.ReadUserByPublicKey(ctx, cpPubKey)
	if err != nil && !errors.Is(err, kernel.ErrNotFound) {
		return 0, nil, err
	}
	if peer == nil || peer.PublicKey == "" {
		return 0, nil, kernel.ErrUnauthorized.Wrap("unknown peer")
	}

	now := time.Now().UTC()
	rec := &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     idempotencyKey,
		CounterpartyUserID: peer.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	if insertErr := k.InsertPendingIdempotencyRecord(ctx, rec); insertErr != nil {
		existing, readErr := k.GetIdempotencyRecord(ctx, idempotencyKey, peer.ID)
		if readErr != nil {
			return 0, nil, kernel.ErrInvalidState.Wrap("idempotency check failed")
		}
		if existing.Status != "complete" {
			return duplicateInFlight()
		}
		return replayStepRecord(existing, stepID)
	}

	// Federated: the record id rides into the kernel, so whichever commit finally settles this
	// completion — here, or later via the remote-dispatch retry loop, the max-age bound, or a
	// forced closure — completes the record atomically with the transaction (§5, §13). The service
	// layer therefore disposes of the record only in the cases where NO commit will ever happen.
	reply, err := k.CompleteStepFederated(ctx, peer.ID, stepID, rawInput, rec.ID)
	if err != nil {
		// Disposition follows CompleteStep's outcome contract (§10) — never a re-read of the step's
		// status, which cannot distinguish these three cases:
		switch {
		case errors.Is(err, kernel.ErrTimeout):
			// Claimed and dispatched to a peer; the receipt may still arrive and commit, and that
			// commit now completes the record. Leave it PENDING so a replay honestly reports a
			// duplicate in flight rather than claiming an outcome that has not happened yet.
		case reply != nil:
			// A transaction committed and then failed; the commit already completed the record.
			// Hand the peer its ids so it can find the transaction it was charged for.
			err = withSettlementMeta(err, reply)
		default:
			// Nothing settled and the step is waiting again: no commit will ever complete this
			// record, so drop it — otherwise a corrected retry is locked out by a key that
			// produced no result.
			_ = k.DeleteIdempotencyRecord(ctx, rec.ID)
		}
		return 0, nil, err
	}
	body := map[string]any{"result": reply.Result, "tx_id": reply.TxID, "trace_id": reply.TraceID, "step_id": stepID}
	if reply.ReceiptID != "" {
		receipt, _ := k.GetReceiptByID(ctx, reply.ReceiptID)
		body["receipt"] = receipt
	}
	return http.StatusOK, body, nil
}

// replayStepRecord rebuilds a completion reply from a completed idempotency record. The kernel
// stores the two halves the same way for every commit path — result_json is the bare action result
// (or an {error,code} body), receipt_json the signed receipt — so success is discriminated on the
// RECEIPT's status rather than by probing the result for an "error" key, which a legitimate result
// carrying its own "error" field would trip.
func replayStepRecord(rec *kernel.IdempotencyRecord, stepID string) (int, map[string]any, error) {
	var result map[string]any
	_ = json.Unmarshal([]byte(rec.ResultJSON), &result)
	var receipt *kernel.Receipt
	if rec.ReceiptJSON != "" {
		_ = json.Unmarshal([]byte(rec.ReceiptJSON), &receipt)
	}
	if receipt == nil {
		// No transaction committed (a pre-execution rejection): there is nothing to point at.
		return replayStatus(result, nil), result, nil
	}
	body := map[string]any{
		"result": result, "tx_id": receipt.TxID, "trace_id": receipt.TraceID,
		"step_id": stepID, "receipt": receipt,
	}
	if receipt.Status != kernel.TxSuccess {
		// A settled FAILURE still charged the caller, so the replay carries the same ids the fresh
		// response did — otherwise a retry after a dropped connection loses the only pointer to the
		// transaction it paid for, which is the loss withSettlementMeta exists to prevent.
		body["error"], body["code"] = result["error"], result["code"]
		body["meta"] = map[string]string{
			"step_id": stepID, "tx_id": receipt.TxID,
			"trace_id": receipt.TraceID, "receipt_id": receipt.ID,
		}
	}
	return replayStatus(result, receipt), body, nil
}

// replayStatus is the HTTP status a replayed idempotency record must carry. It reads the signed
// RECEIPT's status, not the result body: a settled failure must never replay as success, and
// probing the result for an "error" key misclassifies a successful action whose own output happens
// to carry that field (e.g. a validator returning {"error": null}). The result body is consulted
// only for records with no receipt — the pre-execution rejections, which never committed.
func replayStatus(result map[string]any, receipt *kernel.Receipt) int {
	if receipt != nil {
		if receipt.Status == kernel.TxSuccess {
			return http.StatusOK
		}
		code, _ := result["code"].(string)
		return kernel.HTTPStatusFromCode(code)
	}
	if _, isErr := result["error"]; !isErr {
		return http.StatusOK
	}
	code, _ := result["code"].(string)
	return kernel.HTTPStatusFromCode(code)
}

// duplicateInFlight is the §13 reply for a replay that arrives while the first attempt is still
// running. It carries a code so the requesting kernel re-raises a typed error: without one,
// ErrorFromCode("") degrades it to execution_failed and an operator reads a transient duplicate
// as a hard failure and stops retrying.
func duplicateInFlight() (int, map[string]any, error) {
	return http.StatusConflict, map[string]any{
		"error": "duplicate in flight",
		"code":  kernel.ErrInvalidState.Code,
	}, nil
}

// settleIdempotencyWithReceipt marks a record complete, falling back to deleting it if that write
// fails. A record stuck pending answers every retry with 409 "duplicate in flight" forever, with
// the money already spent and the result unreachable; deleting it instead lets a retry through to
// an honest typed error. Neither is good, but only one is a dead end.
func settleIdempotencyWithReceipt(k *kernel.Kernel, ctx context.Context, recID, resultJSON, receiptJSON string) {
	if err := k.CompleteIdempotencyRecordIfPending(ctx, recID, resultJSON, receiptJSON); err != nil {
		_ = k.DeleteIdempotencyRecord(ctx, recID)
	}
}

// handleFederationCall validates the inbound federation request (counterparty, timestamp,
// signature) and executes the call. Returns (httpStatus, responseBody, err).
func handleFederationCall(k *kernel.Kernel, ctx context.Context, cpPubKey, tsStr, idempotencyKey, actionParam, sigStr string, rawBody []byte) (int, map[string]any, error) {
	argsHash := sha256HexBytes(rawBody)

	if err := checkFederationTimestamp(tsStr); err != nil {
		return 0, nil, err
	}
	// cpPubKey is the transport-authenticated caller key (OnCall proved connection key == counterparty);
	// verify the request signature against it before touching state.
	if err := kernel.VerifyFederationSignature(cpPubKey, actionParam, cpPubKey, idempotencyKey, tsStr, argsHash, sigStr); err != nil {
		return 0, nil, err
	}
	// Resolve or lazily provision the caller's billing account (§13, handshake-free): a
	// signature-valid caller with no account here gets a zero-balance one, so a price-0 call
	// succeeds and a priced call hits the normal insufficient-funds rejection the provider clears
	// with a deposit. A suspended counterparty needs no gate here — RunFederated rejects it via
	// requireActiveUser and the pre-execution branch signs a zero-charge rejection receipt.
	counterparty, err := k.ReadUserByPublicKey(ctx, cpPubKey)
	if err != nil || counterparty == nil || counterparty.PublicKey == "" {
		short := cpPubKey
		if len(short) > 8 {
			short = short[:8]
		}
		counterparty, err = k.CreateOrUpdateProxyPeer(ctx, "@k-"+short, cpPubKey)
		if err != nil {
			return 0, nil, err
		}
	}

	ownerHandle, actionName, err := kernel.ParseActionRef(actionParam)
	if err != nil {
		return 0, nil, err
	}

	var args map[string]any
	if err := json.Unmarshal(rawBody, &args); err != nil {
		return 0, nil, kernel.ErrInvalidInput.Wrap("invalid JSON")
	}

	owner, err := k.ReadUserByHandle(ctx, ownerHandle)
	if err != nil || owner == nil {
		return 0, nil, kernel.ErrNotFound.Wrap("action owner not found")
	}
	action, err := k.ReadActionByOwnerName(ctx, owner.ID, actionName)
	if err != nil || action == nil {
		return 0, nil, kernel.ErrNotFound.Wrapf("action %s not found", actionParam)
	}
	// A known-but-non-executable action (inactive, non-public, suspended owner) is NOT rejected
	// here: letting the call flow into RunFederated makes CanCall fail before any transaction, and
	// the pre-execution branch below signs a zero-charge rejection receipt carrying action.ID — so
	// the caller settles immediately instead of pinning funds until the 24h pending bound (§13).
	// Only a genuinely absent/unverifiable action stays a plain error (its ID can't match the
	// caller's stored RemoteActionID, so a receipt there would just re-pin the caller).

	now := time.Now().UTC()
	rec := &kernel.IdempotencyRecord{
		ID:                 uuid.New().String(),
		IdempotencyKey:     idempotencyKey,
		CounterpartyUserID: counterparty.ID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	if insertErr := k.InsertPendingIdempotencyRecord(ctx, rec); insertErr != nil {
		existing, readErr := k.GetIdempotencyRecord(ctx, idempotencyKey, counterparty.ID)
		if readErr == nil {
			if existing.Status == "complete" {
				var result map[string]any
				_ = json.Unmarshal([]byte(existing.ResultJSON), &result)
				var receipt *kernel.Receipt
				if existing.ReceiptJSON != "" {
					_ = json.Unmarshal([]byte(existing.ReceiptJSON), &receipt)
				}
				return replayStatus(result, receipt), map[string]any{"result": result, "receipt": receipt}, nil
			}
			return duplicateInFlight()
		}
		return 0, nil, kernel.ErrInvalidState.Wrap("idempotency check failed")
	}

	reply, callErr := k.RunFederated(ctx, counterparty.ID, owner.ID, actionName, args, rec.ID)
	if callErr != nil {
		errJSON, _ := json.Marshal(map[string]string{
			"error": callErr.Error(),
			"code":  kernel.KernelErrorCode(callErr),
		})
		// A committed transaction (reply carries a receipt) means the call settled — possibly
		// with charge > 0 from settled descendants. Return THAT receipt so the caller settles
		// the real charge, preserving bilateral conservation rather than under-paying with 0.
		if reply != nil && reply.ReceiptID != "" {
			receipt, _ := k.GetReceiptByID(ctx, reply.ReceiptID)
			receiptJSON, _ := json.Marshal(receipt)
			settleIdempotencyWithReceipt(k, ctx, rec.ID, string(errJSON), string(receiptJSON))
			return http.StatusUnprocessableEntity, map[string]any{"error": callErr.Error(), "receipt": receipt}, nil
		}
		// A parked remote dispatch has committed nothing yet and may still settle with a real
		// charge; its own settlement completes the record (§13). Signing a zero-charge rejection
		// here would answer the peer with an outcome that has not happened.
		if errors.Is(callErr, kernel.ErrTimeout) {
			return 0, nil, callErr
		}
		// Pre-execution rejection (no transaction committed, e.g. insufficient funds): sign a
		// zero-charge rejection receipt so the caller can settle locally without leaving the
		// trace pending.
		status, msg := http.StatusUnprocessableEntity, callErr.Error()
		if errors.Is(callErr, kernel.ErrInsufficientFunds) {
			status, msg = http.StatusPaymentRequired, "insufficient balance"
		}
		if receipt, signErr := k.CreateSignedRejectionReceipt(counterparty.ID, action.ID, argsHash, idempotencyKey, msg); signErr == nil {
			receiptJSON, _ := json.Marshal(receipt)
			settleIdempotencyWithReceipt(k, ctx, rec.ID, string(errJSON), string(receiptJSON))
			return status, map[string]any{"error": msg, "receipt": receipt}, nil
		}
		_ = k.DeleteIdempotencyRecord(ctx, rec.ID)
		return 0, nil, callErr
	}

	var receipt *kernel.Receipt
	if reply.ReceiptID != "" {
		receipt, _ = k.GetReceiptByID(ctx, reply.ReceiptID)
	}
	return http.StatusOK, map[string]any{"result": reply.Result, "receipt": receipt}, nil
}
