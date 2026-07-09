package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	OwnerUserID string    `json:"owner_user_id,omitempty"`
	ActionRef   string    `json:"action"`
	HTTP        *httpView `json:"http,omitempty"`
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

// adjustmentView renders a deposit/withdrawal with the operator and target @handles instead of raw
// user UUIDs; the record's own id is dropped too (no command consumes it — external_key is the
// out-of-band idempotency handle).
type adjustmentView struct {
	*kernel.Adjustment
	ID             string `json:"id,omitempty"`
	OperatorUserID string `json:"operator_user_id,omitempty"`
	TargetUserID   string `json:"target_user_id,omitempty"`
	OperatorHandle string `json:"operator_handle"`
	TargetHandle   string `json:"target_handle"`
}

func enrichAdjustment(a *kernel.Adjustment, uc *userCache) *adjustmentView {
	return &adjustmentView{
		Adjustment:     a,
		OperatorHandle: uc.handle(a.OperatorUserID),
		TargetHandle:   uc.handle(a.TargetUserID),
	}
}

// peerViews projects proxy-peer users into handle+key+balance views, dropping their internal ids.
func peerViews(peers []*kernel.User) []*kernel.PeerView {
	out := make([]*kernel.PeerView, len(peers))
	for i, p := range peers {
		out[i] = &kernel.PeerView{
			Handle: p.Handle, PublicKey: p.PublicKey,
			Available: p.Available, Locked: p.Locked, DeniedAt: p.DeniedAt,
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

func enrichAction(a *kernel.Action) actionResp {
	ref := ""
	if a.OwnerHandle != "" && a.Name != "" {
		ref = a.OwnerHandle + "/" + a.Name
	}
	return actionResp{Action: a, ActionRef: ref, HTTP: httpViewOf(a)}
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
		"id":        u.ID,
		"handle":    u.Handle,
		"email":     u.Email,
		"available": u.Available,
		"locked":    u.Locked,
	}
}

// ---- Resolution helpers ----

// resolveHandle resolves an account by its @handle (kernel-local name) or public key (global name):
// an @-prefixed string is a handle, a bare string is tried as a key first, then a handle.
func resolveHandle(k *kernel.Kernel, ctx context.Context, ident string) (*kernel.User, error) {
	if !strings.HasPrefix(ident, "@") {
		if u, err := k.ReadUserByPublicKey(ctx, ident); err == nil {
			return u, nil
		}
	}
	return k.ReadUserByHandle(ctx, kernel.NormalizeHandle(ident))
}

// resolveActionRef resolves "owner/name" (with or without a leading "@") or a raw action
// ID to a *kernel.Action. Raw action IDs are UUIDs and contain no "/", so any ref with a
// "/" is an action reference whose owner handle is canonicalized before lookup.
func resolveActionRef(k *kernel.Kernel, ctx context.Context, ref string) (*kernel.Action, error) {
	if strings.Contains(ref, "/") {
		if !strings.HasPrefix(ref, "@") {
			ref = "@" + ref
		}
		ownerHandle, actionName, err := kernel.ParseActionRef(ref)
		if err != nil {
			return nil, err
		}
		owner, err := k.ReadUserByHandle(ctx, ownerHandle)
		if err != nil {
			return nil, fmt.Errorf("action owner not found: %w", err)
		}
		return k.ReadActionByOwnerName(ctx, owner.ID, actionName)
	}
	return k.ReadAction(ctx, ref)
}

// ---- User operations ----

func createUser(k *kernel.Kernel, ctx context.Context, req kernel.CreateUserRequest) (map[string]any, error) {
	u, err := k.CreateUser(ctx, req)
	if err != nil {
		return nil, err
	}
	return userView(u), nil
}

func getMe(k *kernel.Kernel, ctx context.Context, callerID string) (map[string]any, error) {
	u, err := k.ReadUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	view := userView(u)
	// Grants are token-free: action ref, requested scopes, created_at (§8). Always present.
	grants, err := k.ListGrantViews(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if grants == nil {
		grants = []*kernel.GrantView{}
	}
	view["grants"] = grants
	return view, nil
}

// startGrant begins a delegated-OAuth consent for an action the caller may use. It resolves the
// action, requires it to be a callable oauth_delegated action, and drives the broker (§8).
func startGrant(k *kernel.Kernel, broker *grantBroker, ctx context.Context, callerID, actionRef, redirectURI, flow string) (*startResult, error) {
	if broker == nil {
		return nil, kernel.ErrInvalidState.Wrap("OAuth consent is not configured on this server")
	}
	a, err := resolveActionRef(k, ctx, actionRef)
	if err != nil {
		return nil, err
	}
	auth, err := k.DelegatedAuthConfig(ctx, callerID, a.ID)
	if err != nil {
		return nil, err
	}
	if flow == "" {
		flow = "code"
	}
	return broker.start(ctx, callerID, a.ID, auth, redirectURI, flow)
}

// completeGrant finishes a consent and, on success, seals+stores the refresh token via CreateGrant.
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
	g, err := k.CreateGrant(ctx, callerID, res.ActionID, res.Refresh)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "complete", "action": k.ActionRef(ctx, res.ActionID), "created_at": g.CreatedAt}, nil
}

// attachToken stores a caller-supplied static token for a delegated_bearer action (§8): the direct
// non-OAuth twin of the start/complete consent flow. The kernel enforces the delegated_bearer scheme
// and callability; the raw token never appears in any read path (R9).
func attachToken(k *kernel.Kernel, ctx context.Context, callerID, actionRef, token string) (map[string]any, error) {
	a, err := resolveActionRef(k, ctx, actionRef)
	if err != nil {
		return nil, err
	}
	g, err := k.AttachBearerGrant(ctx, callerID, a.ID, token)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "connected", "action": k.ActionRef(ctx, a.ID), "created_at": g.CreatedAt}, nil
}

func revokeGrant(k *kernel.Kernel, ctx context.Context, callerID, actionRef string) (map[string]any, error) {
	a, err := resolveActionRef(k, ctx, actionRef)
	if err != nil {
		return nil, err
	}
	if err := k.RevokeGrant(ctx, callerID, a.ID); err != nil {
		return nil, err
	}
	return map[string]any{"revoked": true, "action": actionRef}, nil
}

func updateMe(k *kernel.Kernel, ctx context.Context, callerID, email, currentPwd, newPwd string) (map[string]any, error) {
	u, err := k.UpdateUser(ctx, callerID, kernel.UpdateUserRequest{
		Email:           email,
		CurrentPassword: currentPwd,
		NewPassword:     newPwd,
	})
	if err != nil {
		return nil, err
	}
	return userView(u), nil
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
	return enrichAction(full), nil
}

func getAction(k *kernel.Kernel, ctx context.Context, callerID, id string) (actionResp, error) {
	a, err := k.ReadActionForSubject(ctx, callerID, id)
	if err != nil {
		return actionResp{}, err
	}
	return enrichAction(a), nil
}

func updateAction(k *kernel.Kernel, ctx context.Context, callerID string, req kernel.UpdateActionRequest) (actionResp, error) {
	a, err := k.UpdateAction(ctx, callerID, req)
	if err != nil {
		return actionResp{}, err
	}
	return enrichAction(a), nil
}

// listPublicActions returns actions visible to the caller, optionally filtered by owner handle and name.
// Unauthenticated: active+public actions only.
// Authenticated (no owner filter): active+public union caller's own active actions, deduplicated.
// Authenticated with owner filter resolving to caller: all their actions regardless of active/public.
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
		actions, err = k.ListPublicActions(ctx, limit, offset)
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
	for i, a := range actions {
		cp := *a
		r := enrichAction(&cp) // decompose http view before hiding the raw blob
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
	// Precondition-4: external caller must be authorized to use the trace.
	if err := k.AuthorizeTraceUse(ctx, callerID, p.TraceID); err != nil {
		return nil, err
	}
	step, err := k.CreateStep(ctx, p.TraceID, action.ID, p.PartialArgs, callerUser.ID)
	if err != nil {
		return nil, err
	}
	uc := newUserCache(k, ctx)
	uc.m[callerUser.ID] = callerUser // already resolved; avoid a redundant read
	return enrichStep(k, ctx, step, action, uc), nil
}

func listSteps(k *kernel.Kernel, ctx context.Context, callerID, processID, status string) ([]*stepWithAction, error) {
	steps, err := k.ListSteps(ctx, callerID, processID, status)
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

// handleFederationCall validates the inbound federation request (counterparty, timestamp,
// signature) and executes the call. Returns (httpStatus, responseBody, err).
func handleFederationCall(k *kernel.Kernel, ctx context.Context, cpPubKey, tsStr, idempotencyKey, actionParam, sigStr string, rawBody []byte) (int, map[string]any, error) {
	argsHash := sha256HexBytes(rawBody)

	counterparty, err := k.ReadUserByPublicKey(ctx, cpPubKey)
	if err != nil || counterparty.PublicKey == "" {
		return 0, nil, kernel.ErrUnauthenticated.Wrap("counterparty not a registered peer")
	}
	ts, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return 0, nil, kernel.ErrUnauthenticated.Wrap("X-Timestamp must be RFC3339")
	}
	diff := time.Since(ts)
	if diff < -5*time.Minute || diff > 5*time.Minute {
		return 0, nil, kernel.ErrUnauthenticated.Wrap("X-Timestamp out of range")
	}
	if err := kernel.VerifyFederationSignature(counterparty.PublicKey, actionParam, cpPubKey, idempotencyKey, tsStr, argsHash, sigStr); err != nil {
		return 0, nil, err
	}
	// §13.2: denied peers are rejected with a signed rejection receipt so the caller can settle.
	if counterparty.DeniedAt != nil {
		// Resolve the action UUID so VerifyRemoteReceipt can match receipt.action_id against
		// the caller's stored RemoteActionID. Fall back to the ref string if lookup fails.
		denialActionID := actionParam
		if oh, an, parseErr := kernel.ParseActionRef(actionParam); parseErr == nil {
			if denialOwner, ownerErr := k.ReadUserByHandle(ctx, oh); ownerErr == nil && denialOwner != nil {
				if act, actErr := k.ReadActionByOwnerName(ctx, denialOwner.ID, an); actErr == nil && act != nil {
					denialActionID = act.ID
				}
			}
		}
		receipt, signErr := k.CreateSignedRejectionReceipt(counterparty.ID, denialActionID, argsHash, idempotencyKey, "counterparty denied")
		if signErr != nil {
			return 0, nil, kernel.ErrUnauthenticated.Wrap("counterparty is denied")
		}
		return http.StatusForbidden, map[string]any{"error": "counterparty denied", "receipt": receipt}, nil
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
				if _, isErr := result["error"]; isErr {
					code, _ := result["code"].(string)
					return kernel.HTTPStatusFromCode(code), map[string]any{"result": result, "receipt": receipt}, nil
				}
				return http.StatusOK, map[string]any{"result": result, "receipt": receipt}, nil
			}
			return http.StatusConflict, map[string]any{"error": "duplicate in flight"}, nil
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
			_ = k.CompleteIdempotencyRecordIfPending(ctx, rec.ID, string(errJSON), string(receiptJSON))
			return http.StatusUnprocessableEntity, map[string]any{"error": callErr.Error(), "receipt": receipt}, nil
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
			_ = k.CompleteIdempotencyRecordIfPending(ctx, rec.ID, string(errJSON), string(receiptJSON))
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
