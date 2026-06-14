package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daios-ai/juice/kernel"
)

// ---- Types ----

// stepWithAction enriches a step with a computed @owner/name action field.
type stepWithAction struct {
	*kernel.Step
	Action string `json:"action,omitempty"`
}

// actionResp wraps an action with the computed @owner/name reference field.
type actionResp struct {
	*kernel.Action
	ActionRef string `json:"action"`
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
	return actionResp{Action: a, ActionRef: ref}
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
	if !strings.HasPrefix(handle, "@") {
		handle = "@" + handle
	}
	return k.ReadUserByHandle(ctx, handle)
}

// resolveActionRef resolves "@owner/name" or a raw action ID to a *kernel.Action.
func resolveActionRef(k *kernel.Kernel, ctx context.Context, ref string) (*kernel.Action, error) {
	if strings.HasPrefix(ref, "@") {
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

// listPublicActions returns public actions, optionally filtered by owner handle and name.
// When callerID matches the owner, their private/inactive actions are included.
// Source and ArtifactHash are stripped for public discovery.
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
		cp.Source = ""
		cp.ArtifactHash = ""
		resps[i] = enrichAction(&cp)
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
	ProcessID      string
	ParentTraceID  string
	ActionRef      string
	RequiredCaller string
	PartialArgs    json.RawMessage
	InputSchema    json.RawMessage
}

// createStep resolves ActionRef and RequiredCaller, calls kernel.CreateStep,
// and returns an enriched *stepWithAction. Used by both HTTP and CLI surfaces.
func createStep(k *kernel.Kernel, ctx context.Context, callerID string, p createStepParams) (*stepWithAction, error) {
	action, err := resolveActionRef(k, ctx, p.ActionRef)
	if err != nil {
		return nil, err
	}
	callerUser, err := resolveHandle(k, ctx, p.RequiredCaller)
	if err != nil {
		return nil, fmt.Errorf("required_caller not found: %w", err)
	}
	var parentTraceID *string
	if p.ParentTraceID != "" {
		parentTraceID = &p.ParentTraceID
	}
	step, err := k.CreateStep(ctx, callerID, p.ProcessID, parentTraceID, action.ID, p.PartialArgs, p.InputSchema, callerUser.ID)
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
		action, _ := k.ReadAction(ctx, step.NextActionID)
		views[i] = enrichStep(step, action)
	}
	return views, nil
}

func getStep(k *kernel.Kernel, ctx context.Context, callerID, id string) (*stepWithAction, error) {
	step, err := k.ReadStep(ctx, callerID, id)
	if err != nil {
		return nil, err
	}
	action, _ := k.ReadAction(ctx, step.NextActionID)
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
