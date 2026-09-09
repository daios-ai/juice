package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
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

// txSummary is the list shape of a transaction. A call's arguments and result are read by id;
// paging them would make a list of fifty calls carry fifty payloads.
type txSummary struct {
	txView                    // by value: encoding/json will not allocate an embedded pointer to an unexported type on decode, and the CLI decodes these rows
	ArgsJSON  json.RawMessage `json:"args,omitempty"`
	ReplyJSON json.RawMessage `json:"result,omitempty"`
}

// actionSummary is the list shape of an action. What a detail read carries beyond it — the authored
// source and the compiled artifact — is fetched by id, never paged (API.md: a wasm action's detail
// read still carries its source).
type actionSummary struct {
	actionResp
	Source       string `json:"source,omitempty"`
	WasmArtifact string `json:"wasm_artifact,omitempty"`
}

func summaries(resps []actionResp) []actionSummary {
	out := make([]actionSummary, len(resps))
	for i, r := range resps {
		out[i] = actionSummary{actionResp: r}
	}
	return out
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
	QuoteHash     string    `json:"quote_hash"`            // the terms a caller may pin on a run (§4 precondition 7)
}

// httpView is the read-side decomposition of an action's HTTPSource. It carries
// no secrets (auth_json is never surfaced, §8).
type httpView struct {
	Method string             `json:"method"`
	URL    string             `json:"url"`
	Params []kernel.HTTPParam `json:"params,omitempty"`
}

// ---- Enrichment helpers ----

// accountCache resolves user IDs to display info within one request, reading each user at most once
// (transaction lists reference few distinct users across many rows). handle() falls back to the raw
// id only when the row is truly gone (a purged peer, §13), so an immutable ledger stays legible.
type accountCache struct {
	k   *kernel.Kernel
	ctx context.Context
	m   map[string]*kernel.Account
}

func newAccountCache(k *kernel.Kernel, ctx context.Context) *accountCache {
	return &accountCache{k: k, ctx: ctx, m: map[string]*kernel.Account{}}
}

func (c *accountCache) get(id string) *kernel.Account {
	if u, ok := c.m[id]; ok {
		return u
	}
	u, _ := c.k.ReadUser(c.ctx, id) // nil on error; cached so a bad id isn't re-read
	c.m[id] = u
	return u
}

// reference renders an account as something a command can consume (§14). A local user shows its
// handle; a kernel account shows its petname, falling back to the kernel's public key when no
// petname is bound — a key always resolves, so the output stays actionable. Only a purged tombstone
// falls back to the raw id.
func (c *accountCache) reference(id string) string {
	if id == "" {
		return ""
	}
	u := c.get(id)
	if u == nil {
		return id
	}
	if u.Handle != "" {
		return u.Handle
	}
	if u.KernelPublicKey != "" {
		if rk, err := c.k.ReadKernel(c.ctx, u.KernelPublicKey); err == nil && rk != nil && rk.Petname != "" {
			return rk.Petname
		}
		return u.KernelPublicKey
	}
	return id
}

func (c *accountCache) isPeer(id string) bool {
	u := c.get(id)
	return u != nil && u.KernelPublicKey != ""
}

func enrichStep(k *kernel.Kernel, ctx context.Context, step *kernel.Step, action *kernel.Action, uc *accountCache) *stepWithAction {
	v := &stepWithAction{Step: step, RequiredCallerHandle: uc.reference(step.RequiredCallerUserID)}
	// A step addressed to a principal on a peer names that principal, not merely the kernel that
	// hosts them (§13): the completer is who may complete it, and completion already demands their
	// attested id. Rendered the way every remote reference is, beneath the peer's local name.
	if step.RequiredCallerRemoteID != nil {
		v.RequiredCallerHandle = *step.RequiredCallerRemoteID + "@" + uc.reference(step.RequiredCallerUserID)
	}
	if action != nil {
		v.Action = actionRef(action, uc)
	}
	// The creating action (what produced this step) carries its meaning; the target action can be a
	// generic sink (e.g. @sys/message parks a @sys/sink step). Resolve it from the parent trace.
	if step.ParentTraceID != nil {
		if tr, err := k.ReadTrace(ctx, *step.ParentTraceID); err == nil {
			v.CreatedBy = k.ActionRef(ctx, tr.ActionID)
			// The step's process owner is the payer of the transaction it will settle into (§10).
			v.OwnerHandle = uc.reference(k.ProcessOwnerID(ctx, tr.ProcessID))
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
func enrichProcess(p *kernel.Process, since map[string]time.Time, uc *accountCache) *processView {
	v := &processView{Process: p, OwnerHandle: uc.reference(p.OwnerUserID)}
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

// railTransferView renders one external movement the way a person reads it: the party by name, and
// the internal account id withheld (§14).
type railTransferView struct {
	*kernel.RailTransfer
	Party     string `json:"party,omitempty"`
	PartyName string `json:"party_handle,omitempty"`
}

// railTransferViews names the party on each row: a withdrawal's is the account it leaves.
func railTransferViews(k *kernel.Kernel, ctx context.Context, rows []*kernel.RailTransfer) []*railTransferView {
	uc := newAccountCache(k, ctx)
	out := make([]*railTransferView, 0, len(rows))
	for _, r := range rows {
		out = append(out, &railTransferView{RailTransfer: r, PartyName: uc.reference(r.Party)})
	}
	return out
}

// awaiting is what the operator has still to act on, in two real lists rather than a third model
// that flattens them: payments that arrived from a sender nobody has registered, and obligations a
// buyer says it paid whose money has not been seen. Each row is named by the `id` that closes it.
type awaiting struct {
	Deposits []*railTransferView `json:"deposits"`
	Owed     []*owedView         `json:"owed"`
}

// owedView renders the buyer as a reference: a raw account id is never a surface value (D20).
type owedView struct {
	*kernel.Owed
	Peer string `json:"peer"`
}

func owedViews(k *kernel.Kernel, ctx context.Context, rows []*kernel.Owed) []*owedView {
	uc := newAccountCache(k, ctx)
	out := make([]*owedView, 0, len(rows))
	for _, r := range rows {
		out = append(out, &owedView{Owed: r, Peer: uc.reference(r.PeerUserID)})
	}
	return out
}

func enrichLedger(e *kernel.LedgerEntry, uc *accountCache) *ledgerView {
	v := &ledgerView{LedgerEntry: e, OperatorHandle: uc.reference(e.OperatorUserID)}
	if e.FromUserID != "" {
		v.FromHandle = uc.reference(e.FromUserID)
	}
	if e.ToUserID != "" {
		v.ToHandle = uc.reference(e.ToUserID)
	}
	return v
}

// enrichTx resolves the @handles of a transaction's three parties (payer, caller, payee).
func enrichTx(tv *kernel.TransactionView, uc *accountCache) *txView {
	return &txView{
		TransactionView: tv,
		OwnerHandle:     uc.reference(tv.OwnerUserID),
		CallerHandle:    uc.reference(tv.CallerUserID),
		TargetHandle:    uc.reference(tv.TargetUserID),
	}
}

// actionRef renders an action's reference through the one canonical formatter (§13 grammar),
// populating the display owner first: a kernel account holds no handle, so its petname — else its
// key — is the mount alias FormatActionRef needs to render owner@kernel/name. Writing the field
// back also keeps `owner_handle` non-empty on the response (R8).
func actionRef(a *kernel.Action, uc *accountCache) string {
	if a.OwnerHandle == "" {
		a.OwnerHandle = uc.reference(a.OwnerUserID)
	}
	if a.Name == "" {
		return ""
	}
	return kernel.FormatActionRef(a)
}

func enrichAction(k *kernel.Kernel, a *kernel.Action, uc *accountCache) actionResp {
	scheme, requiresGrant := k.ActionAuthInfo(a)
	// Respond from a copy: enrichment writes display fields (actionRef backfills owner_handle) and
	// drops the raw source below, neither of which belongs on the caller's row.
	cp := *a
	view := httpViewOf(&cp)
	// For kind=http the decomposed object IS the read shape; `source` holds the same object as an
	// encoded string, and serving both would double-encode it (R2). The kind decides that, never
	// whether the stored source happens to parse — a row too malformed to decompose must not be the
	// one that leaks the blob. A wasm action keeps its source: authored text, a documented read (§9).
	if cp.Kind == kernel.KindHTTP {
		cp.Source = ""
	}
	return actionResp{Action: &cp, ActionRef: actionRef(&cp, uc), HTTP: view, AuthScheme: scheme, RequiresGrant: requiresGrant, QuoteHash: kernel.QuoteHash(a)}
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

func userView(u *kernel.Account) map[string]any {
	v := map[string]any{
		"id":          u.ID,
		"handle":      u.Handle,
		"description": u.Description,
		"available":   u.Available,
		"locked":      u.Locked,
	}
	if u.RailAddress != "" {
		v["rail_address"] = u.RailAddress
	}
	return v
}

// ---- Resolution helpers ----

// resolveMixed resolves a target that may name either namespace — the only commands that need it are
// show, rename, suspend/unsuspend, and deposit/withdraw (§14). It returns an account for a local
// user and a public key for a kernel; the shapes are self-identifying, so only a bare name can be
// ambiguous, and a bare name matching both a handle and a petname is refused rather than guessed:
// money and moderation must never pick a target silently. Kernel-only commands (settle, inspect,
// step --peer) call the kernel resolver directly instead.
func resolveMixed(k *kernel.Kernel, ctx context.Context, ident string) (*kernel.Account, string, error) {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return nil, "", kernel.ErrInvalidInput.Wrap("a user handle, kernel petname, or public key is required")
	}
	if kernel.IsPublicKey(ident) {
		key, acct, err := k.ResolveKernelKey(ctx, ident)
		if err != nil {
			return nil, "", err
		}
		return acct, key, nil
	}
	acct, aerr := k.ResolveUser(ctx, ident)
	if aerr == nil && !acct.IsLiveUser() && !acct.IsPeer() {
		// A purged peer's tombstone still carries a resolvable id, but it names no live entity
		// (§13 Retention): it must never become the target of a rename, a deposit, or a suspend.
		return nil, "", kernel.ErrNotFound.Wrapf("%s is a purged account, not a live target", ident)
	}
	rk, rerr := k.ReadKernelByPetname(ctx, ident)
	switch {
	case aerr == nil && rk != nil && rerr == nil && acct.KernelPublicKey != rk.PublicKey:
		return nil, "", kernel.ErrInvalidInput.Wrapf(
			"%q names both a local user and a kernel; use the account id or the kernel's public key", ident)
	case rk != nil && rerr == nil:
		kacct, _ := k.ReadAccountByKernelKey(ctx, rk.PublicKey)
		return kacct, rk.PublicKey, nil
	case aerr == nil:
		return acct, acct.KernelPublicKey, nil
	}
	return nil, "", kernel.ErrNotFound.Wrapf("%s not found", ident)
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
// action did not resolve to @owner/name.
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
	return map[string]any{"status": "granted", "provider": kernel.ProviderLabel(res.ProviderKey), "actions": grantRefs(k, ctx, grants), "created_at": grants[0].CreatedAt}, nil
}

// attachToken stores a caller-supplied static token across a selector's delegated_bearer group
// (§8): the direct non-OAuth twin of the start/complete consent flow, minting one grant per action
// against one connection. The raw token never appears in any read path.
func attachToken(k *kernel.Kernel, ctx context.Context, callerID, selector, provider, token string) (map[string]any, error) {
	grants, err := k.AttachBearerGrants(ctx, callerID, selector, provider, token)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "granted", "provider": kernel.ProviderLabel(provider), "actions": grantRefs(k, ctx, grants), "created_at": grants[0].CreatedAt}, nil
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

func updateMe(k *kernel.Kernel, ctx context.Context, callerID string, req kernel.UpdateUserRequest) (map[string]any, error) {
	u, err := k.UpdateUser(ctx, callerID, req)
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
	return enrichAction(k, full, newAccountCache(k, ctx)), nil
}

func getAction(k *kernel.Kernel, ctx context.Context, callerID, id string) (actionResp, error) {
	a, err := k.ReadActionForSubject(ctx, callerID, id)
	if err != nil {
		return actionResp{}, err
	}
	return enrichAction(k, a, newAccountCache(k, ctx)), nil
}

// resolveActionRef answers the listing endpoint's reference mode: one reference resolved by the
// kernel's own resolver, so a reference means here exactly what it means when called, then read
// back through the ordinary per-row gate. A miss is an empty list rather than an error, matching
// every other filter on this endpoint.
func resolveActionRef(k *kernel.Kernel, ctx context.Context, callerID, ref string) ([]actionSummary, error) {
	a, err := k.ResolveAction(ctx, ref)
	if err != nil {
		if errors.Is(err, kernel.ErrNotFound) {
			return []actionSummary{}, nil
		}
		return nil, err
	}
	a, err = k.ReadActionForSubject(ctx, callerID, a.ID)
	if err != nil {
		if errors.Is(err, kernel.ErrNotFound) || errors.Is(err, kernel.ErrUnauthorized) {
			return []actionSummary{}, nil
		}
		return nil, err
	}
	return summaries([]actionResp{enrichAction(k, a, newAccountCache(k, ctx))}), nil
}

// enrichActions projects the rows one mutation touched, in the order they were written.
// importResp is the wire shape of an import: the same projection every action read uses, grouped by
// what the reconciliation did to each row. The kernel's ImportResult stays internal, as Action does.
type importResp struct {
	Created     []actionResp      `json:"created"`
	Unchanged   []actionResp      `json:"unchanged"`
	Updated     []actionResp      `json:"updated"`
	Deactivated []actionResp      `json:"deactivated"`
	Rejected    []importRejection `json:"rejected"`
}

type importRejection struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

func enrichImport(k *kernel.Kernel, ctx context.Context, r *kernel.ImportResult) importResp {
	out := importResp{
		Created:     enrichActions(k, ctx, r.Created),
		Unchanged:   enrichActions(k, ctx, r.Unchanged),
		Updated:     enrichActions(k, ctx, r.Updated),
		Deactivated: enrichActions(k, ctx, r.Deactivated),
		Rejected:    make([]importRejection, 0, len(r.Rejected)),
	}
	for _, rj := range r.Rejected {
		out.Rejected = append(out.Rejected, importRejection{Key: rj.Key, Reason: rj.Reason})
	}
	return out
}

func enrichActions(k *kernel.Kernel, ctx context.Context, as []*kernel.Action) []actionResp {
	cache := newAccountCache(k, ctx)
	out := make([]actionResp, 0, len(as))
	for _, a := range as {
		out = append(out, enrichAction(k, a, cache))
	}
	return out
}

func updateActions(k *kernel.Kernel, ctx context.Context, callerID, target string, req kernel.UpdateActionRequest) ([]actionResp, error) {
	as, err := k.UpdateActionMany(ctx, callerID, target, req)
	if err != nil {
		return nil, err
	}
	return enrichActions(k, ctx, as), nil
}

func setActionsActive(k *kernel.Kernel, ctx context.Context, callerID, target string, active bool) ([]actionResp, error) {
	as, err := k.SetActiveMany(ctx, callerID, target, active)
	if err != nil {
		return nil, err
	}
	return enrichActions(k, ctx, as), nil
}

func deleteActions(k *kernel.Kernel, ctx context.Context, callerID, target string) ([]actionResp, error) {
	as, err := k.DeleteActionMany(ctx, callerID, target)
	if err != nil {
		return nil, err
	}
	return enrichActions(k, ctx, as), nil
}

// listPublicActions returns actions visible to the caller, optionally filtered by owner handle and name.
// Unauthenticated: active public actions only.
// Authenticated (no owner filter): active public+local actions union caller's own active actions, deduplicated.
// Authenticated with owner filter resolving to caller: all their actions regardless of active/visibility.
// Source and ArtifactHash are stripped from all results.
func listPublicActions(k *kernel.Kernel, ctx context.Context, callerID, ownerHandle, name string, includeInactive bool, limit, offset int) ([]actionSummary, error) {
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
			// A proxy row's owner is a kernel account, which holds no handle: the owner filter
			// then names the kernel (petname or key), the same reference `owner@kernel/name`
			// carries (§13). Falling through keeps one query serving both namespaces.
			if _, acct, kerr := k.ResolveKernelKey(ctx, ownerHandle); kerr == nil && acct != nil {
				u = acct
			} else {
				return []actionSummary{}, nil
			}
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
	uc := newAccountCache(k, ctx) // shared so listing is O(distinct peer owners), not O(rows)
	for i, a := range actions {
		r := enrichAction(k, a, uc)
		// Lists summarize (§14): no source of any kind and no artifact hash, whatever a detail read
		// would show. enrichAction responds from its own copy, so this never touches the row.
		r.Source = ""
		r.ArtifactHash = ""
		resps[i] = r
	}
	if resps == nil {
		resps = []actionResp{}
	}
	return summaries(resps), nil
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
	uc := newAccountCache(k, ctx)
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
	return enrichProcess(p, since, newAccountCache(k, ctx)), nil
}

// ---- Step operations ----

// createStepParams is both the POST /v1/steps body and the shared HTTP/CLI input (§14): one shape
// for the contract. ViaCapability is authority the middleware establishes, never wire input.
type createStepParams struct {
	TraceID        string          `json:"trace_id"`
	ActionRef      string          `json:"action"`
	RequiredCaller string          `json:"required_caller"`
	PartialArgs    json.RawMessage `json:"partial_args"`
	// ViaCapability skips the precondition-4 trace-use check: a capability's trace authority is
	// the executing action owning that trace (§9), matching the WASM juice.step_create path, which
	// calls CreateStep directly without an external authorization check.
	ViaCapability bool `json:"-"`
}

// createStep resolves ActionRef and RequiredCaller, enforces precondition-4 for the trace,
// calls kernel.CreateStep, and returns an enriched *stepWithAction.
// Used by both HTTP and CLI surfaces.
func createStep(k *kernel.Kernel, ctx context.Context, callerID string, p createStepParams) (*stepWithAction, error) {
	action, err := k.ResolveAction(ctx, p.ActionRef)
	if err != nil {
		return nil, err
	}
	// RequiredCaller may be a local handle or a remote user@kernel (§13): resolve to the routing
	// account id plus the completer's stable remote id (empty for a local recipient).
	requiredCallerID, remoteID, err := k.ResolveRequiredCaller(ctx, p.RequiredCaller)
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
	step, err := k.CreateStep(ctx, p.TraceID, action.ID, p.PartialArgs, requiredCallerID, remoteID)
	if err != nil {
		return nil, err
	}
	return enrichStep(k, ctx, step, action, newAccountCache(k, ctx)), nil
}

func listSteps(k *kernel.Kernel, ctx context.Context, callerID, processID, status string, limit, offset int) ([]*stepWithAction, error) {
	steps, err := k.ListSteps(ctx, callerID, processID, status, limit, offset)
	if err != nil {
		return nil, err
	}
	views := make([]*stepWithAction, len(steps))
	uc := newAccountCache(k, ctx)
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
	return enrichStep(k, ctx, step, action, newAccountCache(k, ctx)), nil
}

// ---- Transaction operations ----

func listTransactions(k *kernel.Kernel, ctx context.Context, callerID string, f kernel.TxFilter) ([]*txSummary, error) {
	txs, err := k.ListTransactions(ctx, callerID, f)
	if err != nil {
		return nil, err
	}
	uc := newAccountCache(k, ctx)
	views := make([]*txSummary, len(txs))
	for i, tv := range txs {
		views[i] = &txSummary{txView: *enrichTx(tv, uc)}
	}
	return views, nil
}

func getTransaction(k *kernel.Kernel, ctx context.Context, callerID, id string) (*txView, error) {
	tv, err := k.ReadTransaction(ctx, callerID, id)
	if err != nil {
		return nil, err
	}
	return enrichTx(tv, newAccountCache(k, ctx)), nil
}
