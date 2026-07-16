package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

	// A peer is resolvable by its base64url public key (the global name), not only its @handle.
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := k.AddPeer(ctx, sys.ID, "@svc-peer", keyB64)
	if err != nil {
		t.Fatal(err)
	}
	byKey, err := resolveHandle(k, ctx, keyB64)
	if err != nil || byKey.ID != peer.ID {
		t.Errorf("resolveHandle(key): got %v (err %v), want peer %q", byKey, err, peer.ID)
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

	// Resolve by owner/name without the leading "@".
	got1b, err := resolveActionRef(k, ctx, "svc-alice/svc-greet")
	if err != nil {
		t.Fatalf("resolveActionRef(svc-alice/svc-greet): %v", err)
	}
	if got1b.ID != actID {
		t.Errorf("by owner/name (no @): got ID %q, want %q", got1b.ID, actID)
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
	step := &kernel.Step{ID: "s1"}
	action := &kernel.Action{OwnerHandle: "@alice", Name: "greet"}

	// No parent trace and not waiting, so neither the ref resolver nor the peer check is dialed
	// (nil kernel is safe here).
	v := enrichStep(nil, context.Background(), step, action, newUserCache(nil, context.Background()))
	if v.Action != "@alice/greet" {
		t.Errorf("enrichStep: Action = %q, want @alice/greet", v.Action)
	}
	if v.CreatedBy != "" {
		t.Errorf("enrichStep: CreatedBy = %q, want empty for a nil parent trace", v.CreatedBy)
	}
	if v.ID != "s1" {
		t.Errorf("enrichStep: embedded Step.ID = %q, want s1", v.ID)
	}
	if v.WaitingOnPeer {
		t.Error("enrichStep: WaitingOnPeer should be false")
	}
	// A non-waiting step carries no allowed_input.
	if v.AllowedInput != nil {
		t.Errorf("enrichStep: AllowedInput = %v, want nil for a non-waiting step", v.AllowedInput)
	}

	// Nil action → empty action field; a waiting step to a peer caller flags waiting_on_peer and
	// resolves the required-caller handle from the (pre-seeded) cache.
	peerStep := &kernel.Step{ID: "s2", Status: kernel.StepWaiting, RequiredCallerUserID: "peer1"}
	uc := newUserCache(nil, context.Background())
	uc.m["peer1"] = &kernel.User{Handle: "@peer", PublicKey: "pk"}
	v2 := enrichStep(nil, context.Background(), peerStep, nil, uc)
	if v2.Action != "" {
		t.Errorf("enrichStep(nil action): Action = %q, want empty", v2.Action)
	}
	if !v2.WaitingOnPeer {
		t.Error("enrichStep: WaitingOnPeer should be true")
	}
	if v2.RequiredCallerHandle != "@peer" {
		t.Errorf("enrichStep: RequiredCallerHandle = %q, want @peer", v2.RequiredCallerHandle)
	}

	// A waiting step carries allowed_input = input_schema \ keys(partial_args): the target's declared
	// property `units` is exposed for completion, while the pre-bound `city` is dropped. This lets a
	// required caller who cannot read a private target action still see what to submit.
	schemaAction := &kernel.Action{
		OwnerHandle: "@alice", Name: "weather",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city":  map[string]any{"type": "string"},
				"units": map[string]any{"type": "string"},
			},
			"required": []any{"city", "units"},
		},
	}
	waiting := &kernel.Step{ID: "s3", Status: kernel.StepWaiting, RequiredCallerUserID: "u9", PartialArgs: json.RawMessage(`{"city":"NYC"}`)}
	uc3 := newUserCache(nil, context.Background())
	uc3.m["u9"] = &kernel.User{Handle: "@carol"} // seeded so the peer check doesn't dial the nil kernel
	v3 := enrichStep(nil, context.Background(), waiting, schemaAction, uc3)
	props, ok := v3.AllowedInput["properties"].(map[string]any)
	if !ok {
		t.Fatalf("enrichStep: AllowedInput has no properties: %v", v3.AllowedInput)
	}
	if _, bound := props["city"]; bound {
		t.Error("enrichStep: AllowedInput should not expose the pre-bound key `city`")
	}
	if _, open := props["units"]; !open {
		t.Error("enrichStep: AllowedInput should expose the unbound key `units`")
	}
}

func TestEnrichProcess(t *testing.T) {
	p := &kernel.Process{ID: "p1", Status: kernel.ProcessOpen}
	when := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	uc := newUserCache(nil, context.Background())
	uc.m["owner1"] = &kernel.User{Handle: "@owner"}
	p.OwnerUserID = "owner1"

	// Not awaiting: no entry in the since map.
	v := enrichProcess(p, map[string]time.Time{}, uc)
	if v.AwaitingReceipt || v.AwaitingReceiptSince != nil {
		t.Errorf("expected not awaiting, got %+v", v)
	}
	if v.ID != "p1" {
		t.Errorf("embedded Process.ID = %q, want p1", v.ID)
	}
	if v.OwnerHandle != "@owner" {
		t.Errorf("owner_handle = %q, want @owner", v.OwnerHandle)
	}

	// Awaiting: since map carries this process → flag + timestamp surface.
	v2 := enrichProcess(p, map[string]time.Time{"p1": when}, uc)
	if !v2.AwaitingReceipt || v2.AwaitingReceiptSince == nil || !v2.AwaitingReceiptSince.Equal(when) {
		t.Errorf("expected awaiting since %v, got %+v", when, v2)
	}
}

func TestEnrichAction(t *testing.T) {
	a := &kernel.Action{ID: "a1", OwnerHandle: "@bob", Name: "ping"}
	r := enrichAction(&kernel.Kernel{}, a)
	if r.ActionRef != "@bob/ping" {
		t.Errorf("enrichAction: ActionRef = %q, want @bob/ping", r.ActionRef)
	}

	// Empty handle → empty ref.
	a2 := &kernel.Action{ID: "a2"}
	r2 := enrichAction(&kernel.Kernel{}, a2)
	if r2.ActionRef != "" {
		t.Errorf("enrichAction(no handle): ActionRef = %q, want empty", r2.ActionRef)
	}
}

// peerStateFor derives a remote proxy's §13 liveness/funding annotation from the peer's sync cache:
// offline (missing/stale last_seen) takes precedence over unfunded (cached credit below mp); healthy
// yields "". A zero-value kernel has ImportBPS 0, so RemoteManifestPrice(p) == p.
func TestPeerStateFor(t *testing.T) {
	k := &kernel.Kernel{}
	stale := time.Hour
	fresh := time.Now().Add(-time.Minute)
	old := time.Now().Add(-2 * time.Hour)
	credit := func(v int64) *int64 { return &v }

	cases := []struct {
		name  string
		owner *kernel.User
		price int64
		want  string
	}{
		{"nil owner", nil, 10, ""},
		{"never synced", &kernel.User{}, 10, "offline"},
		{"stale last_seen", &kernel.User{PeerLastSeen: &old}, 10, "offline"},
		{"fresh underfunded", &kernel.User{PeerLastSeen: &fresh, PeerCredit: credit(5)}, 10, "unfunded"},
		{"fresh funded", &kernel.User{PeerLastSeen: &fresh, PeerCredit: credit(20)}, 10, ""},
		{"fresh unknown credit", &kernel.User{PeerLastSeen: &fresh}, 10, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := peerStateFor(k, tc.owner, tc.price, stale); got != tc.want {
				t.Errorf("peerStateFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// peerViews carries the §13 sync cache (our credit on the peer, last_seen) into the projection.
func TestPeerViewsCarrySyncCache(t *testing.T) {
	seen := time.Now()
	credit := int64(64)
	views := peerViews([]*kernel.User{{Handle: "@b", PublicKey: "k", PeerCredit: &credit, PeerLastSeen: &seen}})
	if len(views) != 1 || views[0].PeerCredit == nil || *views[0].PeerCredit != 64 || views[0].LastSeen == nil {
		t.Errorf("peerViews dropped the sync cache: %+v", views[0])
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

// TestActionAuthFieldsExposed: action reads surface the non-secret auth_scheme and requires_grant,
// and never the config or secrets (§8/R9).
func TestActionAuthFieldsExposed(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	ownerID, _ := makeUser(t, k, "@svc-auth")
	backend := newStepBackend(t)

	mk := func(name string, auth *kernel.AuthInput) actionResp {
		a, err := createAction(k, ctx, ownerID, kernel.CreateActionRequest{
			OwnerUserID: ownerID, Name: name, Kind: kernel.KindHTTP, Source: backend.URL,
			Description: "d", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			Auth: auth,
		})
		if err != nil {
			t.Fatalf("createAction %s: %v", name, err)
		}
		got, err := getAction(k, ctx, ownerID, a.ID)
		if err != nil {
			t.Fatalf("getAction %s: %v", name, err)
		}
		return got
	}

	// delegated_bearer → scheme exposed, requires_grant true.
	if r := mk("db-svc", &kernel.AuthInput{Scheme: kernel.AuthSchemeDelegatedBearer}); r.AuthScheme != "delegated_bearer" || !r.RequiresGrant {
		t.Errorf("delegated_bearer: scheme=%q requires_grant=%v, want delegated_bearer/true", r.AuthScheme, r.RequiresGrant)
	}

	// basic (owner-held) → scheme exposed, no grant, and the secret never serializes.
	basic := mk("basic-svc", &kernel.AuthInput{Scheme: kernel.AuthSchemeBasic, Secrets: map[string]any{"username": "u", "password": "topsecret"}})
	if basic.AuthScheme != "basic" || basic.RequiresGrant {
		t.Errorf("basic: scheme=%q requires_grant=%v, want basic/false", basic.AuthScheme, basic.RequiresGrant)
	}
	if b, _ := json.Marshal(basic); strings.Contains(string(b), "topsecret") {
		t.Errorf("basic response leaked the secret: %s", b)
	}

	// no upstream auth → no scheme, no grant.
	if r := mk("plain-svc", nil); r.AuthScheme != "" || r.RequiresGrant {
		t.Errorf("no-auth: scheme=%q requires_grant=%v, want empty/false", r.AuthScheme, r.RequiresGrant)
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
	if _, err := updateAction(k, ctx, ownerID, kernel.UpdateActionRequest{ID: a.ID, Visibility: visPtr(kernel.VisibilityPublic)}); err != nil {
		t.Fatalf("updateAction public: %v", err)
	}

	// List all public — should appear.
	resps, err := listPublicActions(k, ctx, "", "", "", false, 50, 0)
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
	byOwner, err := listPublicActions(k, ctx, "", "@svc-lpa", "", false, 50, 0)
	if err != nil {
		t.Fatalf("listPublicActions by owner: %v", err)
	}
	if len(byOwner) == 0 {
		t.Error("no results filtering by owner @svc-lpa")
	}

	// Filter by name.
	byName, err := listPublicActions(k, ctx, "", "", "svc-pub", false, 50, 0)
	if err != nil {
		t.Fatalf("listPublicActions by name: %v", err)
	}
	if len(byName) == 0 {
		t.Error("no results filtering by name svc-pub")
	}
}

func boolPtr(b bool) *bool { return &b }

func visPtr(v kernel.ActionVisibility) *kernel.ActionVisibility { return &v }

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
		TraceID: traceID, ActionRef: "@svc-ls/svc-ls-action",
		RequiredCaller: "@svc-ls-hook", PartialArgs: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("createStep: %v", err)
	}

	steps, err := listSteps(k, ctx, ownerID, p.ID, "", 50, 0)
	if err != nil {
		t.Fatalf("listSteps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("expected at least one step")
	}
	if steps[0].Action != "@svc-ls/svc-ls-action" {
		t.Errorf("listSteps: action field = %q, want @svc-ls/svc-ls-action", steps[0].Action)
	}
	if steps[0].OwnerHandle != "@svc-ls" {
		t.Errorf("listSteps: owner_handle = %q, want @svc-ls (the process owner)", steps[0].OwnerHandle)
	}
}

func TestGetStep_Enriched(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	ctx := context.Background()

	backend := newStepBackend(t)
	ownerID, ownerTok := makeUser(t, k, "@svc-gs")
	giveCredits(t, k, ownerID, 500)
	hookID, _ := makeUser(t, k, "@svc-gs-hook")

	_, _ = createStepAction(t, srv, backend.URL, ownerTok, "@svc-gs", "svc-gs-action")

	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	view, err := createStep(k, ctx, ownerID, createStepParams{
		TraceID: traceID, ActionRef: "@svc-gs/svc-gs-action",
		RequiredCaller: "@svc-gs-hook", PartialArgs: json.RawMessage(`{}`),
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
	if got.OwnerHandle != "@svc-gs" {
		t.Errorf("getStep owner_handle = %q, want @svc-gs (the process owner)", got.OwnerHandle)
	}
	// The required caller is not the owner, yet must still see the owner_handle — the resolver
	// is unauthorized, so a non-owner viewer of the step gets the owner without a process-read.
	asHook, err := getStep(k, ctx, hookID, view.ID)
	if err != nil {
		t.Fatalf("getStep as required caller: %v", err)
	}
	if asHook.OwnerHandle != "@svc-gs" {
		t.Errorf("getStep(required caller) owner_handle = %q, want @svc-gs", asHook.OwnerHandle)
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
		TraceID:        traceID,
		ActionRef:      "@svc-step-owner/svc-notify",
		RequiredCaller: "@svc-webhook",
		PartialArgs:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("createStep by @owner/name: %v", err)
	}
	if view.Action != "@svc-step-owner/svc-notify" {
		t.Errorf("action field: got %q, want %q", view.Action, "@svc-step-owner/svc-notify")
	}
	if view.Step.ActionID != actID {
		t.Errorf("next_action_id: got %q, want %q", view.Step.ActionID, actID)
	}

	// Create step by raw action ID resolves to the same action.
	p2 := setupProcessHTTP(t, db, ownerID, 100)
	traceID2 := setupTraceForProcess(t, db, p2.ID)
	view2, err := createStep(k, ctx, ownerID, createStepParams{
		TraceID:        traceID2,
		ActionRef:      actID,
		RequiredCaller: "@svc-webhook",
		PartialArgs:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("createStep by action ID: %v", err)
	}
	if view2.Step.ActionID != actID {
		t.Errorf("next_action_id by ID: got %q, want %q", view2.Step.ActionID, actID)
	}
}

func TestParseParams(t *testing.T) {
	got, err := parseParams([]string{"city:path", "q:query", "data:body"})
	if err != nil {
		t.Fatalf("parseParams: %v", err)
	}
	if len(got) != 3 || got[0] != (kernel.HTTPParam{Name: "city", In: "path"}) {
		t.Errorf("parseParams result unexpected: %+v", got)
	}
	if _, err := parseParams([]string{"noColon"}); err == nil {
		t.Error("expected error for malformed param spec")
	}
	if _, err := parseParams(nil); err != nil {
		t.Errorf("nil specs should be nil,nil: %v", err)
	}
}

// TestManualHTTPActionRoundTrip: a manual kind=http action read back through the
// service layer exposes the same {method,url,params} it was created with.
func TestManualHTTPActionRoundTrip(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	ownerID, _ := makeUser(t, k, "@svc-httpview")

	resp, err := createAction(k, ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: "weather", Kind: kernel.KindHTTP,
		Source: "https://api.example.com/weather/{city}", Method: "GET",
		Params: []kernel.HTTPParam{{Name: "city", In: "path"}},
	})
	if err != nil {
		t.Fatalf("createAction: %v", err)
	}
	if resp.HTTP == nil {
		t.Fatal("expected http view on create response")
	}
	if resp.HTTP.Method != "GET" || resp.HTTP.URL != "https://api.example.com/weather/{city}" {
		t.Errorf("http view: %+v", resp.HTTP)
	}

	got, err := getAction(k, ctx, ownerID, resp.ID)
	if err != nil {
		t.Fatalf("getAction: %v", err)
	}
	if got.HTTP == nil || got.HTTP.Method != "GET" || got.HTTP.URL != "https://api.example.com/weather/{city}" {
		t.Errorf("read-back http view mismatch: %+v", got.HTTP)
	}
	if len(got.HTTP.Params) != 1 || got.HTTP.Params[0].In != "path" {
		t.Errorf("read-back params: %+v", got.HTTP.Params)
	}
}

// userCache.handle returns the @handle, falling back to the raw id only when the user row is gone
// (a purged peer, §13), and empty for an empty id.
func TestUserCacheHandleFallback(t *testing.T) {
	uc := newUserCache(nil, context.Background())
	uc.m["u1"] = &kernel.User{Handle: "@alice"}
	uc.m["gone"] = nil // cached miss (purged/unknown) → fall back to the id
	if got := uc.handle("u1"); got != "@alice" {
		t.Errorf("handle(u1) = %q, want @alice", got)
	}
	if got := uc.handle("gone"); got != "gone" {
		t.Errorf("handle(gone) = %q, want raw-id fallback", got)
	}
	if got := uc.handle(""); got != "" {
		t.Errorf("handle(empty) = %q, want empty", got)
	}
}

// enrichTx resolves the three party handles and — critically — the raw *_user_id UUIDs must not
// survive to the JSON (the omitempty-shadow drop, where a plain json:"-" would fail).
func TestEnrichTxDropsUUIDs(t *testing.T) {
	tv := &kernel.TransactionView{Transaction: &kernel.Transaction{
		ID: "tx1", OwnerUserID: "o", CallerUserID: "c", TargetUserID: "t",
	}}
	uc := newUserCache(nil, context.Background())
	uc.m["o"] = &kernel.User{Handle: "@owner"}
	uc.m["c"] = &kernel.User{Handle: "@caller"}
	uc.m["t"] = &kernel.User{Handle: "@target"}

	v := enrichTx(tv, uc)
	if v.OwnerHandle != "@owner" || v.CallerHandle != "@caller" || v.TargetHandle != "@target" {
		t.Fatalf("handles: %q/%q/%q", v.OwnerHandle, v.CallerHandle, v.TargetHandle)
	}
	b, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"owner_user_id", "caller_user_id", "target_user_id"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s must be dropped from tx JSON, got %v", k, m[k])
		}
	}
	if m["owner_handle"] != "@owner" || m["id"] != "tx1" {
		t.Errorf("expected owner_handle=@owner and id=tx1, got %v / %v", m["owner_handle"], m["id"])
	}
}

// TestListActionsActiveOnlyByDefault: the superuser's default action list is active-only (so a
// deactivated proxy disappears, like after unfriend); includeInactive brings inactive rows back.
func TestListActionsActiveOnlyByDefault(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}

	ownerID, _ := makeUser(t, k, "@svc-inact")
	backend := newStepBackend(t)
	a, err := createAction(k, ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: "dead", Kind: kernel.KindHTTP, Source: backend.URL,
		Description: "inactive action", InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updateAction(k, ctx, ownerID, kernel.UpdateActionRequest{ID: a.ID, Visibility: visPtr(kernel.VisibilityPublic)}); err != nil {
		t.Fatal(err)
	}
	// Left inactive (never enabled).

	has := func(resps []actionResp) bool {
		for _, r := range resps {
			if r.ID == a.ID {
				return true
			}
		}
		return false
	}
	def, _ := listPublicActions(k, ctx, sys.ID, "", "", false, 50, 0)
	if has(def) {
		t.Error("superuser default list must exclude an inactive action")
	}
	all, _ := listPublicActions(k, ctx, sys.ID, "", "", true, 50, 0)
	if !has(all) {
		t.Error("superuser --all list must include the inactive action")
	}
}
