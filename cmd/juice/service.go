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

// stepWithAction enriches a step with a computed @owner/name action field.
type stepWithAction struct {
	*kernel.Step
	Action string `json:"action,omitempty"`
}

// actionResp wraps an action with the computed @owner/name reference field and,
// for kind=http, a decomposed view of the request shape so manual and
// OpenAPI-imported actions read identically and round-trip with create/update.
type actionResp struct {
	*kernel.Action
	ActionRef string    `json:"action"`
	HTTP      *httpView `json:"http,omitempty"`
}

// httpView is the read-side decomposition of an action's HTTPSource. It carries
// no secrets (auth_json is never surfaced, §8).
type httpView struct {
	Method string             `json:"method"`
	URL    string             `json:"url"`
	Params []kernel.HTTPParam `json:"params,omitempty"`
}

// ---- Enrichment helpers ----

func enrichStep(step *kernel.Step, action *kernel.Action) *stepWithAction {
	v := &stepWithAction{Step: step}
	if action != nil {
		v.Action = action.OwnerHandle + "/" + action.Name
	}
	return v
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

// resolveHandle resolves a handle string to a *kernel.User.
// Accepts handle with or without the leading "@".
func resolveHandle(k *kernel.Kernel, ctx context.Context, handle string) (*kernel.User, error) {
	return k.ReadUserByHandle(ctx, kernel.NormalizeHandle(handle))
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
	return userView(u), nil
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
func listPublicActions(k *kernel.Kernel, ctx context.Context, callerID, ownerHandle, name string, limit, offset int) ([]actionResp, error) {
	actions, err := k.ListPublicActions(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	if ownerHandle != "" {
		u, err := k.ReadUserByHandle(ctx, ownerHandle)
		if err != nil {
			return []actionResp{}, nil
		}
		if callerID != "" && callerID == u.ID {
			actions, err = k.ListOwnedActions(ctx, u.ID, limit, offset)
			if err != nil {
				return nil, err
			}
		} else {
			filtered := actions[:0]
			for _, a := range actions {
				if a.OwnerUserID == u.ID {
					filtered = append(filtered, a)
				}
			}
			actions = filtered
		}
	} else if callerID != "" {
		owned, err := k.ListOwnedActions(ctx, callerID, limit, offset)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool, len(actions))
		for _, a := range actions {
			seen[a.ID] = true
		}
		for _, a := range owned {
			if a.Active && !seen[a.ID] {
				actions = append(actions, a)
			}
		}
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

func listOwnedActions(k *kernel.Kernel, ctx context.Context, callerID string, limit, offset int) ([]actionResp, error) {
	actions, err := k.ListOwnedActions(ctx, callerID, limit, offset)
	if err != nil {
		return nil, err
	}
	resps := make([]actionResp, len(actions))
	for i, a := range actions {
		resps[i] = enrichAction(a)
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

func listProcesses(k *kernel.Kernel, ctx context.Context, callerID string, limit, offset int) ([]*kernel.Process, error) {
	return k.ListProcesses(ctx, callerID, limit, offset)
}

func getProcess(k *kernel.Kernel, ctx context.Context, callerID, id string) (*kernel.Process, error) {
	return k.ReadProcess(ctx, callerID, id)
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
	return enrichStep(step, action), nil
}

func listSteps(k *kernel.Kernel, ctx context.Context, callerID, processID, status string) ([]*stepWithAction, error) {
	steps, err := k.ListSteps(ctx, callerID, processID, status)
	if err != nil {
		return nil, err
	}
	views := make([]*stepWithAction, len(steps))
	for i, step := range steps {
		action, _ := k.ReadAction(ctx, step.ActionID)
		views[i] = enrichStep(step, action)
	}
	return views, nil
}

func getStep(k *kernel.Kernel, ctx context.Context, callerID, id string) (*stepWithAction, error) {
	step, err := k.ReadStep(ctx, callerID, id)
	if err != nil {
		return nil, err
	}
	action, _ := k.ReadAction(ctx, step.ActionID)
	return enrichStep(step, action), nil
}

func completeStep(k *kernel.Kernel, ctx context.Context, callerID, id string, args json.RawMessage) (*kernel.StepReply, error) {
	return k.CompleteStep(ctx, callerID, id, args)
}

// ---- Transaction operations ----

func listTransactions(k *kernel.Kernel, ctx context.Context, callerID string, f kernel.TxFilter) ([]*kernel.TransactionView, error) {
	return k.ListTransactions(ctx, callerID, f)
}

func getTransaction(k *kernel.Kernel, ctx context.Context, callerID, id string) (*kernel.TransactionView, error) {
	return k.ReadTransaction(ctx, callerID, id)
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
	if err != nil || counterparty.RemoteBaseURL == "" {
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
		receipt, signErr := k.CreateSignedRejectionReceipt(counterparty.ID, denialActionID, argsHash, idempotencyKey)
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
	if !action.Active || !action.Public {
		return 0, nil, kernel.ErrUnauthorized.Wrap("action is not active and public")
	}

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
		if receipt, signErr := k.CreateSignedRejectionReceipt(counterparty.ID, action.ID, argsHash, idempotencyKey); signErr == nil {
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
