package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestResolveHandle(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@svc-bob", Email: "svc-bob@test.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveHandle(k, ctx, "@svc-bob")
	if err != nil {
		t.Fatalf("resolveHandle(@svc-bob): %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("got ID %q, want %q", got.ID, u.ID)
	}

	// Without @ prefix is also accepted.
	got2, err := resolveHandle(k, ctx, "svc-bob")
	if err != nil {
		t.Fatalf("resolveHandle(svc-bob): %v", err)
	}
	if got2.ID != u.ID {
		t.Errorf("without @: got ID %q, want %q", got2.ID, u.ID)
	}

	_, err = resolveHandle(k, ctx, "@nobody-svc")
	if err == nil {
		t.Error("expected error for missing handle")
	}
}

func TestResolveActionRef(t *testing.T) {
	srv, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	ownerID, ownerTok := makeUser(t, k, "@svc-alice")
	_ = ownerID
	backend := newStepBackend(t)
	actID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@svc-alice", "svc-greet")

	// Resolve by @owner/name.
	got, err := resolveActionRef(k, ctx, "@svc-alice/svc-greet")
	if err != nil {
		t.Fatalf("resolveActionRef(@svc-alice/svc-greet): %v", err)
	}
	if got.ID != actID {
		t.Errorf("by @owner/name: got ID %q, want %q", got.ID, actID)
	}

	// Resolve by raw action ID.
	got2, err := resolveActionRef(k, ctx, actID)
	if err != nil {
		t.Fatalf("resolveActionRef(id): %v", err)
	}
	if got2.ID != actID {
		t.Errorf("by ID: got ID %q, want %q", got2.ID, actID)
	}

	// Bad @owner/name returns error.
	_, err = resolveActionRef(k, ctx, "@nobody-svc/nope")
	if err == nil {
		t.Error("expected error for missing owner")
	}
}

func TestUserView(t *testing.T) {
	u := &kernel.User{
		ID:        "u1",
		Handle:    "@x",
		Email:     "x@test.com",
		Available: 100,
		Locked:    50,
	}
	m := userView(u)
	for _, key := range []string{"id", "handle", "email", "available", "locked"} {
		if _, ok := m[key]; !ok {
			t.Errorf("userView missing key %q", key)
		}
	}
	if len(m) != 5 {
		t.Errorf("userView: expected 5 keys, got %d", len(m))
	}
	if m["id"] != "u1" || m["handle"] != "@x" {
		t.Error("userView: unexpected values")
	}
}

func TestEnrichStep(t *testing.T) {
	step := &kernel.Step{ID: "s1", ProcessID: "p1"}
	action := &kernel.Action{OwnerHandle: "@alice", Name: "greet"}

	v := enrichStep(step, action)
	if v.Action != "@alice/greet" {
		t.Errorf("enrichStep: Action = %q, want @alice/greet", v.Action)
	}
	if v.ID != "s1" {
		t.Errorf("enrichStep: embedded Step.ID = %q, want s1", v.ID)
	}

	// Nil action → empty action field.
	v2 := enrichStep(step, nil)
	if v2.Action != "" {
		t.Errorf("enrichStep(nil action): Action = %q, want empty", v2.Action)
	}
}

func TestEnrichAction(t *testing.T) {
	a := &kernel.Action{ID: "a1", OwnerHandle: "@bob", Name: "ping"}
	r := enrichAction(a)
	if r.ActionRef != "@bob/ping" {
		t.Errorf("enrichAction: ActionRef = %q, want @bob/ping", r.ActionRef)
	}

	// Empty handle → empty ref.
	a2 := &kernel.Action{ID: "a2"}
	r2 := enrichAction(a2)
	if r2.ActionRef != "" {
		t.Errorf("enrichAction(no handle): ActionRef = %q, want empty", r2.ActionRef)
	}
}

func TestValidateRating(t *testing.T) {
	for _, v := range []float64{0, 1} {
		if err := validateRating(v); err != nil {
			t.Errorf("validateRating(%v): unexpected error: %v", v, err)
		}
	}
	for _, v := range []float64{-1, 0.5, 2} {
		if err := validateRating(v); err == nil {
			t.Errorf("validateRating(%v): expected error, got nil", v)
		}
	}
}

func TestGetMe(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	id, _ := makeUser(t, k, "@svc-me")

	view, err := getMe(k, ctx, id)
	if err != nil {
		t.Fatalf("getMe: %v", err)
	}
	if view["id"] != id {
		t.Errorf("getMe: id = %v, want %s", view["id"], id)
	}
	if view["handle"] != "@svc-me" {
		t.Errorf("getMe: handle = %v, want @svc-me", view["handle"])
	}
	for _, key := range []string{"id", "handle", "email", "available", "locked"} {
		if _, ok := view[key]; !ok {
			t.Errorf("getMe: missing key %q", key)
		}
	}
}

func TestCreateAction_ServiceEnrichment(t *testing.T) {
	srv, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	backend := newStepBackend(t)
	ownerID, ownerTok := makeUser(t, k, "@svc-ca")
	_ = ownerTok
	_ = srv
	_ = backend

	a, err := createAction(k, ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID,
		Name:        "svc-make",
		Kind:        kernel.KindHTTP,
		Source:      backend.URL,
	})
	if err != nil {
		t.Fatalf("createAction: %v", err)
	}
	if a.ActionRef != "@svc-ca/svc-make" {
		t.Errorf("ActionRef = %q, want @svc-ca/svc-make", a.ActionRef)
	}
	if a.ID == "" {
		t.Error("ID should not be empty")
	}
}

func TestGetAction_EnrichesRef(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	ownerID, _ := makeUser(t, k, "@svc-ga")
	backend := newStepBackend(t)

	a, err := createAction(k, ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: "svc-lookup", Kind: kernel.KindHTTP, Source: backend.URL,
	})
	if err != nil {
		t.Fatalf("createAction: %v", err)
	}

	got, err := getAction(k, ctx, ownerID, a.ID)
	if err != nil {
		t.Fatalf("getAction: %v", err)
	}
	if got.ActionRef != "@svc-ga/svc-lookup" {
		t.Errorf("getAction ActionRef = %q, want @svc-ga/svc-lookup", got.ActionRef)
	}
}

func TestListPublicActions_FilterAndStrip(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	ownerID, _ := makeUser(t, k, "@svc-lpa")
	backend := newStepBackend(t)

	a, err := createAction(k, ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: "svc-pub", Kind: kernel.KindHTTP,
		Source: backend.URL, Description: "public test action",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatalf("createAction: %v", err)
	}
	// Enable and make public.
	if err := enableAction(k, ctx, ownerID, a.ID); err != nil {
		t.Fatalf("enableAction: %v", err)
	}
	if _, err := updateAction(k, ctx, ownerID, kernel.UpdateActionRequest{ID: a.ID, Public: boolPtr(true)}); err != nil {
		t.Fatalf("updateAction public: %v", err)
	}

	// List all public — should appear.
	resps, err := listPublicActions(k, ctx, "", "", "", 50, 0)
	if err != nil {
		t.Fatalf("listPublicActions: %v", err)
	}
	found := false
	for _, r := range resps {
		if r.ID == a.ID {
			found = true
			if r.Action.Source != "" {
				t.Error("Source should be stripped in public listing")
			}
			if r.Action.ArtifactHash != "" {
				t.Error("ArtifactHash should be stripped in public listing")
			}
		}
	}
	if !found {
		t.Error("public action not found in listPublicActions")
	}

	// Filter by owner handle.
	byOwner, err := listPublicActions(k, ctx, "", "@svc-lpa", "", 50, 0)
	if err != nil {
		t.Fatalf("listPublicActions by owner: %v", err)
	}
	if len(byOwner) == 0 {
		t.Error("no results filtering by owner @svc-lpa")
	}

	// Filter by name.
	byName, err := listPublicActions(k, ctx, "", "", "svc-pub", 50, 0)
	if err != nil {
		t.Fatalf("listPublicActions by name: %v", err)
	}
	if len(byName) == 0 {
		t.Error("no results filtering by name svc-pub")
	}
}

func boolPtr(b bool) *bool { return &b }

func TestListSteps_Enriched(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	ctx := context.Background()

	backend := newStepBackend(t)
	ownerID, ownerTok := makeUser(t, k, "@svc-ls")
	giveCredits(t, k, ownerID, 500)
	makeUser(t, k, "@svc-ls-hook")

	_, _ = createStepAction(t, srv, backend.URL, ownerTok, "@svc-ls", "svc-ls-action")

	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	_, err := createStep(k, ctx, ownerID, createStepParams{
		ProcessID: p.ID, ParentTraceID: traceID,
		ActionRef: "@svc-ls/svc-ls-action", RequiredCaller: "@svc-ls-hook",
		PartialArgs: json.RawMessage(`{}`), InputSchema: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("createStep: %v", err)
	}

	steps, err := listSteps(k, ctx, ownerID, p.ID, "")
	if err != nil {
		t.Fatalf("listSteps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("expected at least one step")
	}
	if steps[0].Action != "@svc-ls/svc-ls-action" {
		t.Errorf("listSteps: action field = %q, want @svc-ls/svc-ls-action", steps[0].Action)
	}
}

func TestGetStep_Enriched(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	ctx := context.Background()

	backend := newStepBackend(t)
	ownerID, ownerTok := makeUser(t, k, "@svc-gs")
	giveCredits(t, k, ownerID, 500)
	makeUser(t, k, "@svc-gs-hook")

	_, _ = createStepAction(t, srv, backend.URL, ownerTok, "@svc-gs", "svc-gs-action")

	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	view, err := createStep(k, ctx, ownerID, createStepParams{
		ProcessID: p.ID, ParentTraceID: traceID,
		ActionRef: "@svc-gs/svc-gs-action", RequiredCaller: "@svc-gs-hook",
		PartialArgs: json.RawMessage(`{}`), InputSchema: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("createStep: %v", err)
	}

	got, err := getStep(k, ctx, ownerID, view.ID)
	if err != nil {
		t.Fatalf("getStep: %v", err)
	}
	if got.Action != "@svc-gs/svc-gs-action" {
		t.Errorf("getStep action = %q, want @svc-gs/svc-gs-action", got.Action)
	}
	if got.ID != view.ID {
		t.Errorf("getStep ID = %q, want %q", got.ID, view.ID)
	}
}

func TestRateTransaction_Validation(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	// Invalid rating — must fail before touching kernel.
	_, err := rateTransaction(k, ctx, "any", "any", 0.5, nil)
	if err == nil {
		t.Error("rateTransaction(0.5): expected validation error, got nil")
	}

	_, err = rateTransaction(k, ctx, "any", "any", 2, nil)
	if err == nil {
		t.Error("rateTransaction(2): expected validation error, got nil")
	}
}

func TestCreateStep_SharedBehavior(t *testing.T) {
	backend := newStepBackend(t)
	srv, k, db := newTestHTTPServerFull(t)
	ctx := context.Background()

	ownerID, ownerTok := makeUser(t, k, "@svc-step-owner")
	giveCredits(t, k, ownerID, 500)
	makeUser(t, k, "@svc-webhook")

	actID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@svc-step-owner", "svc-notify")

	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	// Create step by @owner/name.
	view, err := createStep(k, ctx, ownerID, createStepParams{
		ProcessID:      p.ID,
		ParentTraceID:  traceID,
		ActionRef:      "@svc-step-owner/svc-notify",
		RequiredCaller: "@svc-webhook",
		PartialArgs:    json.RawMessage(`{}`),
		InputSchema:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("createStep by @owner/name: %v", err)
	}
	if view.Action != "@svc-step-owner/svc-notify" {
		t.Errorf("action field: got %q, want %q", view.Action, "@svc-step-owner/svc-notify")
	}
	if view.Step.NextActionID != actID {
		t.Errorf("next_action_id: got %q, want %q", view.Step.NextActionID, actID)
	}

	// Create step by raw action ID resolves to the same action.
	p2 := setupProcessHTTP(t, db, ownerID, 100)
	traceID2 := setupTraceForProcess(t, db, p2.ID)
	view2, err := createStep(k, ctx, ownerID, createStepParams{
		ProcessID:      p2.ID,
		ParentTraceID:  traceID2,
		ActionRef:      actID,
		RequiredCaller: "@svc-webhook",
		PartialArgs:    json.RawMessage(`{}`),
		InputSchema:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("createStep by action ID: %v", err)
	}
	if view2.Step.NextActionID != actID {
		t.Errorf("next_action_id by ID: got %q, want %q", view2.Step.NextActionID, actID)
	}
}
