// SPDX-License-Identifier: AGPL-3.0-only

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

// A user id is never a consumable CLI input — users are addressed by handle@kernel everywhere — so the
// output views below name every party by its address (D20) and drop the raw ids. An id lives on
// an embedded kernel struct, so to omit it we redeclare a same-JSON-named empty field with
// `,omitempty` at the outer level: the shallower field dominates the promoted one and, being empty,
// is omitted. (A plain `json:"-"` would NOT work — it only removes the outer field, leaving the
// promoted one to render.) Resolution is server-side, so HTTP and CLI stay in parity (§14).

// stepWithAction enriches a step with its action's address, its creator's, and the parties as
// addresses. waiting_on_peer flags a waiting step whose required caller is a peer's account — work
// parked on someone who may be offline (§13); its age is the step's created_at.
type stepWithAction struct {
	*kernel.Step
	RequiredCallerUserID   string  `json:"required_caller_user_id,omitempty"`
	RequiredCallerRemoteID *string `json:"required_caller_remote_id,omitempty"`
	RequiredCallerHandle   string  `json:"required_caller_handle,omitempty"`
	Action                 string  `json:"action,omitempty"`
	CreatedBy              string  `json:"created_by,omitempty"` // the creating action's address (from the parent trace)
	Owner                  string  `json:"owner"`                // process owner (payer)
	RequiredCaller         string  `json:"required_caller,omitempty"`
	WaitingOnPeer          bool    `json:"waiting_on_peer,omitempty"`
	// AllowedInput is the derived completion schema (input_schema \ keys(partial_args), §10) for a
	// waiting step, so the required caller can complete it without reading a private target action.
	AllowedInput map[string]any `json:"allowed_input,omitempty"`
}

// processView enriches a process with its owner's address and awaiting-receipt state and age (§13): a
// process holding a remote-proxy call still waiting for its signed receipt, and when the earliest
// such call started — so an operator can see funds parked on an unreachable peer and for how long.
type processView struct {
	*kernel.Process
	OwnerUserID          string     `json:"owner_user_id,omitempty"`
	Owner                string     `json:"owner"`
	AwaitingReceipt      bool       `json:"awaiting_receipt"`
	AwaitingReceiptSince *time.Time `json:"awaiting_receipt_since,omitempty"`
}

// txView enriches a transaction with the addresses of its three parties (payer, caller, payee) and
// of its action, replacing the raw ids and the stored name, which no command consumes.
type txView struct {
	*kernel.TransactionView
	OwnerUserID  string `json:"owner_user_id,omitempty"`
	CallerUserID string `json:"caller_user_id,omitempty"`
	TargetUserID string `json:"target_user_id,omitempty"`
	ActionName   string `json:"action_name,omitempty"`
	Owner        string `json:"owner"`
	Caller       string `json:"caller"`
	Target       string `json:"target"`
	Action       string `json:"action"`
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

// actionResp wraps an action with its address and, for kind=http, a decomposed view of the request
// shape so manual and OpenAPI-imported actions read identically and round-trip with create/update.
// owner_user_id and owner_handle are shadow-dropped: the address names the owner.
type actionResp struct {
	*kernel.Action
	OwnerUserID   string    `json:"owner_user_id,omitempty"`
	OwnerHandle   string    `json:"owner_handle,omitempty"`
	RemoteOwnerID string    `json:"remote_owner_id,omitempty"` // a proxy's match key (D13), not a read
	ActionRef     string    `json:"action"`
	HTTP          *httpView `json:"http,omitempty"`
	AuthScheme    string    `json:"auth_scheme,omitempty"` // upstream auth scheme name (§8); present only when the action has auth; never config/secrets (R9)
	RequiresGrant bool      `json:"requires_grant"`        // true iff a caller must connect a per-caller grant first (delegated schemes)
	QuoteHash     string    `json:"quote_hash"`            // the terms a caller may pin on a run (§4 precondition 7)
	// Evidence is what this kernel holds about the action's conduct, on the detail read. Present
	// for every action, local or remote: the subject is whoever runs it (§13, U39).
	Evidence *kernel.ActionRecord `json:"evidence,omitempty"`
}

// httpView is the read-side decomposition of an action's HTTPSource. It carries
// no secrets (auth_json is never surfaced, §8).
type httpView struct {
	Method string             `json:"method"`
	URL    string             `json:"url"`
	Params []kernel.HTTPParam `json:"params,omitempty"`
}

// ---- Enrichment helpers ----

// isPeer reports whether an account stands for a peer kernel.
func isPeer(k *kernel.Kernel, ctx context.Context, id string) bool {
	u, err := k.ReadUser(ctx, id)
	return err == nil && u != nil && u.KernelPublicKey != ""
}

func enrichStep(k *kernel.Kernel, ctx context.Context, step *kernel.Step, action *kernel.Action, names *kernel.Names) *stepWithAction {
	v := &stepWithAction{Step: step, RequiredCaller: names.Address(ctx, step.RequiredCaller()), Action: names.Action(ctx, action)}
	// The creating action (what produced this step) carries its meaning; the target action can be a
	// generic sink (e.g. sys/message parks a sys/sink step). Resolve it from the parent trace.
	if step.ParentTraceID != nil {
		if tr, err := k.ReadTrace(ctx, *step.ParentTraceID); err == nil {
			v.CreatedBy = k.ActionAddressByID(ctx, tr.ActionID)
			// The step's process owner is the payer of the transaction it will settle into (§10).
			v.Owner = names.Address(ctx, kernel.Principal{AccountID: k.ProcessOwnerID(ctx, tr.ProcessID)})
		}
	}
	if step.Status == kernel.StepWaiting {
		v.WaitingOnPeer = isPeer(k, ctx, step.RequiredCallerUserID)
		if action != nil {
			v.AllowedInput = kernel.DeriveAllowedSchema(action.InputSchema, step.PartialArgs)
		}
	}
	return v
}

// enrichProcess names the owner by address and flags a process awaiting a remote receipt, with the
// earliest such call's start time from the awaiting-receipt map (kernel.AwaitingReceiptSince).
func enrichProcess(ctx context.Context, p *kernel.Process, since map[string]time.Time, names *kernel.Names) *processView {
	v := &processView{Process: p, Owner: names.Address(ctx, kernel.Principal{AccountID: p.OwnerUserID})}
	if t, ok := since[p.ID]; ok {
		tt := t
		v.AwaitingReceipt = true
		v.AwaitingReceiptSince = &tt
	}
	return v
}

// ledgerView renders a ledger entry (deposit, withdrawal, or transfer) with the operator,
// source, and destination addresses instead of raw user UUIDs. The record's own id is dropped (no
// command consumes it), and so is external_key: it is the writer's own idempotency token, which the
// writer already holds, and publishing it invited a reader to hand back a name that was never
// theirs. from_handle is absent on a deposit (no source) and to_handle on a withdrawal (no
// destination).
type ledgerView struct {
	*kernel.LedgerEntry
	ID             string `json:"id,omitempty"`
	ExternalKey    string `json:"external_key,omitempty"`
	OperatorUserID string `json:"operator_user_id,omitempty"`
	FromUserID     string `json:"from_user_id,omitempty"`
	ToUserID       string `json:"to_user_id,omitempty"`
	Operator       string `json:"operator"`
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
}

// ratingView withholds the rater's id: the rater is the caller, and a party is never a raw id
// (D20). The ratings listing already answers this way.
type ratingView struct {
	*kernel.Rating
	RaterUserID string `json:"rater_user_id,omitempty"`
}

// accountView withholds an account's own id: a user is addressed by its address, never by an id,
// and only GET /v1/me answers with the caller's own (D20).
type accountView struct {
	*kernel.Account
	ID      string `json:"id,omitempty"`
	Handle  string `json:"handle,omitempty"`
	Address string `json:"address"`
}

func accountView1(ctx context.Context, names *kernel.Names, a *kernel.Account) accountView {
	return accountView{Account: a, Address: names.Address(ctx, kernel.Principal{AccountID: a.ID})}
}

func accountViews(ctx context.Context, names *kernel.Names, as []*kernel.Account) []accountView {
	out := make([]accountView, 0, len(as))
	for _, a := range as {
		out = append(out, accountView1(ctx, names, a))
	}
	return out
}

// railTransferView renders one external movement the way a person reads it: the party by name, and
// the internal account id withheld (§14).
type railTransferView struct {
	*kernel.RailTransfer
	Party string `json:"party,omitempty"`
}

// railTransferViews names the party on each row: a withdrawal's is the account it leaves.
func railTransferViews(k *kernel.Kernel, ctx context.Context, rows []*kernel.RailTransfer) []*railTransferView {
	names := k.NewNames()
	out := make([]*railTransferView, 0, len(rows))
	for _, r := range rows {
		out = append(out, &railTransferView{RailTransfer: r, Party: names.Address(ctx, kernel.Principal{AccountID: r.Party})})
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
	names := k.NewNames()
	out := make([]*owedView, 0, len(rows))
	for _, r := range rows {
		out = append(out, &owedView{Owed: r, Peer: names.Address(ctx, kernel.Principal{AccountID: r.PeerUserID})})
	}
	return out
}

func enrichLedger(ctx context.Context, e *kernel.LedgerEntry, names *kernel.Names) *ledgerView {
	one := func(id string) string { return names.Address(ctx, kernel.Principal{AccountID: id}) }
	return &ledgerView{LedgerEntry: e, Operator: one(e.OperatorUserID), From: one(e.FromUserID), To: one(e.ToUserID)}
}

// enrichTx names a transaction's three parties (payer, caller, payee) and its action. The action's
// address is the target's plus the stored name: a proxy transaction keeps the folded name (D4), so
// the owner is split out of it rather than re-read from a row that may since be gone.
func enrichTx(ctx context.Context, tv *kernel.TransactionView, names *kernel.Names) *txView {
	name := tv.ActionName
	target := tv.Target()
	if tv.RemoteActionID != "" {
		owner, rest := kernel.SplitProxyName(name)
		if target.Handle == "" {
			target.Handle = owner
		}
		name = rest
	}
	return &txView{
		TransactionView: tv,
		Owner:           names.Address(ctx, kernel.Principal{AccountID: tv.OwnerUserID}),
		Caller:          names.Address(ctx, tv.Caller()),
		Target:          names.Address(ctx, target),
		Action:          names.Address(ctx, target) + "/" + name,
	}
}

func enrichAction(ctx context.Context, k *kernel.Kernel, a *kernel.Action, names *kernel.Names) actionResp {
	scheme, requiresGrant := k.ActionAuthInfo(a)
	// Respond from a copy: enrichment drops the raw source below, which does not belong on the
	// caller's row.
	cp := *a
	view := httpViewOf(&cp)
	// For kind=http the decomposed object IS the read shape; `source` holds the same object as an
	// encoded string, and serving both would double-encode it (R2). The kind decides that, never
	// whether the stored source happens to parse — a row too malformed to decompose must not be the
	// one that leaks the blob. A wasm action keeps its source: authored text, a documented read (§9).
	if cp.Kind == kernel.KindHTTP {
		cp.Source = ""
	}
	return actionResp{Action: &cp, ActionRef: names.Action(ctx, &cp), HTTP: view, AuthScheme: scheme, RequiresGrant: requiresGrant, QuoteHash: kernel.QuoteHash(a)}
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

func userView(ctx context.Context, k *kernel.Kernel, u *kernel.Account) map[string]any {
	v := map[string]any{
		"id":          u.ID,
		"address":     k.Address(ctx, kernel.Principal{AccountID: u.ID}),
		"description": u.Description,
		"available":   u.Available,
		"locked":      u.Locked,
	}
	if u.BlockchainAddress != "" {
		v["blockchain_address"] = u.BlockchainAddress
	}
	return v
}

// ---- Resolution helpers ----

// resolveTarget turns an admin target into the thing its noun names, and never the other. The noun
// is the route the request arrived on — `users` or `peers` — so a petname that happens to equal a
// handle can no more take a deposit than be mistaken for the account that owns it, and nothing has
// to guess from the shape of a name (D15, D20).
func resolveTarget(k *kernel.Kernel, ctx context.Context, ident, noun string) (*kernel.Account, string, error) {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return nil, "", kernel.ErrInvalidInput.Wrapf("name the %s", noun)
	}
	if noun == "user" {
		// A user is named by address, so a key never resolves here however well it would; a purged
		// account's tombstone resolves but names no live target (§13 Retention).
		acct, err := k.ResolveLocalPrincipal(ctx, ident)
		if err != nil {
			return nil, "", err
		}
		return acct, "", nil
	}
	// A peer is named by its public key or the petname this kernel gave it; its account exists
	// only once money has been involved, so a nil one is ordinary (D15).
	kr, err := k.ResolveKernel(ctx, ident)
	if err != nil || kr.Local {
		return nil, "", kernel.ErrNotFound.Wrapf("%s is not a peer here; peers are named by petname or public key", ident)
	}
	return kr.Account, kr.Key, nil
}

// ---- User operations ----

func createUser(k *kernel.Kernel, ctx context.Context, req kernel.CreateUserRequest) (map[string]any, error) {
	u, err := k.CreateUser(ctx, req)
	if err != nil {
		return nil, err
	}
	return userView(ctx, k, u), nil
}

// connectorView is one directory in the GET /v1/me tree (§8): the actions the caller has granted
// under one folder (the action ref up to its last "/", e.g. @chat or @chat/inbox), grouped with the
// upstream account(s) whose credential backs them. The directory is a display grouping only — it
// never gates a credential; the token binding stays per-action and fact-derived (§8 confused-deputy
// defense), so grouping by it changes nothing about which credential dispatch applies.
type connectorView struct {
	Directory   string                   `json:"directory"`   // the folder the actions live in (owner@kernel or owner@kernel/path)
	Connections []*kernel.ConnectionView `json:"connections"` // upstream account(s) backing this directory (usually one)
	Actions     []*kernel.GrantView      `json:"actions"`     // token-free granted actions under this directory
}

// directoryOf returns the folder a granted action belongs to: its ref up to the LAST "/", so
// @chat/inbox/send and @chat/inbox/read both group under @chat/inbox (not a flat @chat). A
// top-level action like @chat/create-room groups under @chat. Falls back to the whole ref when the
// action did not resolve to owner@kernel/name.
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
	view := userView(ctx, k, u)

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
		refs[i] = k.ActionAddressByID(ctx, g.ActionID)
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
	return userView(ctx, k, u), nil
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
	return enrichAction(ctx, k, full, k.NewNames()), nil
}

// detailRead is one action read in full: the row, its display fields, and the record this kernel
// holds about how it has behaved. Every path that reads ONE action goes through here — by id or by
// reference, local or remote — so no reader is shown a different action than another (U39).
func detailRead(k *kernel.Kernel, ctx context.Context, a *kernel.Action) actionResp {
	resp := enrichAction(ctx, k, a, k.NewNames())
	resp.Evidence = k.ActionRecord(ctx, a)
	return resp
}

func getAction(k *kernel.Kernel, ctx context.Context, callerID, id string) (actionResp, error) {
	a, err := k.ReadActionForSubject(ctx, callerID, id)
	if err != nil {
		return actionResp{}, err
	}
	return detailRead(k, ctx, a), nil
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
	return summaries([]actionResp{detailRead(k, ctx, a)}), nil
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
	names := k.NewNames()
	out := make([]actionResp, 0, len(as))
	for _, a := range as {
		out = append(out, enrichAction(ctx, k, a, names))
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
	var remoteOwner string // the handle folded into a proxy's stored name, when the owner is on a peer
	if ownerHandle != "" {
		// The owner is an address. Here it names a user; on a peer it names the peer's account —
		// which holds every proxy of that kernel — plus the owner's handle folded into each row.
		addr, err := kernel.ParseAddress(ownerHandle)
		if err != nil || addr.Name != "" {
			return nil, kernel.ErrInvalidInput.Wrap("owner must be handle@kernel")
		}
		kr, err := k.ResolveKernel(ctx, addr.Kernel)
		if err != nil {
			return []actionSummary{}, nil
		}
		u := kr.Account
		if kr.Local {
			if u, err = k.ReadUserByHandle(ctx, addr.Handle); err != nil {
				return []actionSummary{}, nil
			}
		} else {
			if u == nil {
				return []actionSummary{}, nil
			}
			remoteOwner = addr.Handle
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
	if name != "" || remoteOwner != "" {
		filtered := actions[:0]
		for _, a := range actions {
			n := a.Name
			if a.Kind == kernel.KindRemoteProxy {
				var owner string
				owner, n = kernel.SplitProxyName(a.Name)
				if remoteOwner != "" && owner != remoteOwner {
					continue
				}
			}
			if name == "" || n == name {
				filtered = append(filtered, a)
			}
		}
		actions = filtered
	}
	resps := make([]actionResp, len(actions))
	names := k.NewNames() // shared so listing is O(distinct owners), not O(rows)
	for i, a := range actions {
		r := enrichAction(ctx, k, a, names)
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
	names := k.NewNames()
	views := make([]*processView, len(processes))
	for i, p := range processes {
		views[i] = enrichProcess(ctx, p, since, names)
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
	return enrichProcess(ctx, p, since, k.NewNames()), nil
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
	// RequiredCaller is an address, here or on a peer: resolved to the routing account, plus the
	// completer's stable remote id and handle when they are on a peer.
	caller, err := k.ResolvePrincipal(ctx, p.RequiredCaller)
	if err != nil {
		return nil, err // typed: a malformed address is the caller's fault, an unknown one is not found
	}
	// Precondition-4: external (JWT) caller must be authorized to use the trace; a capability
	// carries that authority in the token itself.
	if !p.ViaCapability {
		if err := k.AuthorizeTraceUse(ctx, callerID, p.TraceID); err != nil {
			return nil, err
		}
	}
	step, err := k.CreateStep(ctx, p.TraceID, action.ID, p.PartialArgs, caller)
	if err != nil {
		return nil, err
	}
	return enrichStep(k, ctx, step, action, k.NewNames()), nil
}

func listSteps(k *kernel.Kernel, ctx context.Context, callerID, processID, status string, limit, offset int) ([]*stepWithAction, error) {
	steps, err := k.ListSteps(ctx, callerID, processID, status, limit, offset)
	if err != nil {
		return nil, err
	}
	views := make([]*stepWithAction, len(steps))
	names := k.NewNames()
	for i, step := range steps {
		action, _ := k.ReadAction(ctx, step.ActionID)
		views[i] = enrichStep(k, ctx, step, action, names)
	}
	return views, nil
}

func getStep(k *kernel.Kernel, ctx context.Context, callerID, id string) (*stepWithAction, error) {
	step, err := k.ReadStep(ctx, callerID, id)
	if err != nil {
		return nil, err
	}
	action, _ := k.ReadAction(ctx, step.ActionID)
	return enrichStep(k, ctx, step, action, k.NewNames()), nil
}

// ---- Transaction operations ----

func listTransactions(k *kernel.Kernel, ctx context.Context, callerID string, f kernel.TxFilter) ([]*txSummary, error) {
	txs, err := k.ListTransactions(ctx, callerID, f)
	if err != nil {
		return nil, err
	}
	names := k.NewNames()
	views := make([]*txSummary, len(txs))
	for i, tv := range txs {
		views[i] = &txSummary{txView: *enrichTx(ctx, tv, names)}
	}
	return views, nil
}

func getTransaction(k *kernel.Kernel, ctx context.Context, callerID, id string) (*txView, error) {
	tv, err := k.ReadTransaction(ctx, callerID, id)
	if err != nil {
		return nil, err
	}
	return enrichTx(ctx, tv, k.NewNames()), nil
}
