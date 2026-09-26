// SPDX-License-Identifier: AGPL-3.0-only

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
		Handle: "svc-bob@k", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := k.ResolveLocalPrincipal(ctx, "svc-bob@k")
	if err != nil {
		t.Fatalf("ResolveLocalPrincipal(svc-bob@k): %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("got ID %q, want %q", got.ID, u.ID)
	}
	// A bare handle is not an address (D15).
	if _, err := k.ResolveLocalPrincipal(ctx, "svc-bob"); err == nil {
		t.Error("a bare handle must be refused")
	}
	if _, err := k.ResolveLocalPrincipal(ctx, "nobody-svc@k"); err == nil {
		t.Error("expected error for missing handle")
	}

	// A peer is resolvable by its base64url public key (the global name) in the kernel position.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := k.EnsureKernelAccount(ctx, keyB64)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := k.ResolveKernel(ctx, keyB64)
	if err != nil || kr.Local || kr.Account == nil || kr.Account.ID != peer.ID {
		t.Errorf("ResolveKernel(key): got %+v (err %v), want peer %q", kr, err, peer.ID)
	}
}

func TestResolveActionRef(t *testing.T) {
	srv, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	ownerID, ownerTok := makeUser(t, k, "svc-alice")
	_ = ownerID
	backend := newStepBackend(t)
	actID, _ := createStepAction(t, srv, backend.URL, ownerTok, "svc-alice", "svc-greet")

	// Resolve by @owner/name.
	got, err := k.ResolveAction(ctx, "svc-alice@k/svc-greet")
	if err != nil {
		t.Fatalf("resolveActionRef(@svc-alice/svc-greet): %v", err)
	}
	if got.ID != actID {
		t.Errorf("by @owner/name: got ID %q, want %q", got.ID, actID)
	}

	// Resolve by owner/name without the leading "@".
	got1b, err := k.ResolveAction(ctx, "svc-alice@k/svc-greet")
	if err != nil {
		t.Fatalf("resolveActionRef(svc-alice/svc-greet): %v", err)
	}
	if got1b.ID != actID {
		t.Errorf("by owner/name (no @): got ID %q, want %q", got1b.ID, actID)
	}

	// Resolve by raw action ID.
	got2, err := k.ResolveAction(ctx, actID)
	if err != nil {
		t.Fatalf("resolveActionRef(id): %v", err)
	}
	if got2.ID != actID {
		t.Errorf("by ID: got ID %q, want %q", got2.ID, actID)
	}

	// Bad @owner/name returns error.
	_, err = k.ResolveAction(ctx, "nobody-svc@k/nope")
	if err == nil {
		t.Error("expected error for missing owner")
	}
}

func TestUserView(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "x@k", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	m := userView(ctx, k, u)
	for _, key := range []string{"id", "address", "description", "available", "locked"} {
		if _, ok := m[key]; !ok {
			t.Errorf("userView missing key %q", key)
		}
	}
	if len(m) != 5 {
		t.Errorf("userView: expected 5 keys, got %d", len(m))
	}
	if m["id"] != u.ID || m["address"] != "x@k" {
		t.Errorf("userView: unexpected values %v", m)
	}
}

func TestEnrichStep(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	alice, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "alice@k", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	action := &kernel.Action{OwnerUserID: alice.ID, OwnerHandle: "alice", Name: "greet"}
	step := &kernel.Step{ID: "s1", RequiredCallerUserID: alice.ID}
	v := enrichStep(k, ctx, step, action, k.NewNames())
	if v.Action != "alice@k/greet" {
		t.Errorf("enrichStep: Action = %q, want alice@k/greet", v.Action)
	}
	if v.RequiredCaller != "alice@k" {
		t.Errorf("enrichStep: RequiredCaller = %q, want alice@k", v.RequiredCaller)
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

	// Nil action → empty action field; a waiting step addressed to a peer kernel flags
	// waiting_on_peer and names the kernel bare — a user always carries `@`, a kernel never does.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	peerKey := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := k.EnsureKernelAccount(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.BindPetname(ctx, peerKey, "peer", true); err != nil {
		t.Fatal(err)
	}
	peerStep := &kernel.Step{ID: "s2", Status: kernel.StepWaiting, RequiredCallerUserID: peer.ID}
	v2 := enrichStep(k, ctx, peerStep, nil, k.NewNames())
	if v2.Action != "" {
		t.Errorf("enrichStep(nil action): Action = %q, want empty", v2.Action)
	}
	if !v2.WaitingOnPeer {
		t.Error("enrichStep: WaitingOnPeer should be true")
	}
	if v2.RequiredCaller != "peer" {
		t.Errorf("enrichStep: RequiredCaller = %q, want peer", v2.RequiredCaller)
	}

	// A step parked for a principal on a peer names that principal beneath the peer's local name.
	// The handle it went by when the step was made is display; the stable id underneath is what
	// authorises the completion, so a rename there leaves the step addressed and only this stales.
	remoteID := "u-9f2c"
	named := &kernel.Step{ID: "s3", Status: kernel.StepWaiting, RequiredCallerUserID: peer.ID,
		RequiredCallerRemoteID: &remoteID, RequiredCallerHandle: "bob"}
	if got := enrichStep(k, ctx, named, nil, k.NewNames()).RequiredCaller; got != "bob@peer" {
		t.Errorf("enrichStep: RequiredCaller = %q, want bob@peer", got)
	}
	// A row parked before the handle was kept still renders, by the id it does hold.
	unnamed := &kernel.Step{ID: "s4", Status: kernel.StepWaiting, RequiredCallerUserID: peer.ID,
		RequiredCallerRemoteID: &remoteID}
	if got := enrichStep(k, ctx, unnamed, nil, k.NewNames()).RequiredCaller; got != "u-9f2c@peer" {
		t.Errorf("enrichStep(no handle): RequiredCaller = %q, want u-9f2c@peer", got)
	}

	// A waiting step carries allowed_input = input_schema \ keys(partial_args): the target's declared
	// property `units` is exposed for completion, while the pre-bound `city` is dropped. This lets a
	// required caller who cannot read a private target action still see what to submit.
	schemaAction := &kernel.Action{
		OwnerUserID: alice.ID, OwnerHandle: "alice", Name: "weather",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city":  map[string]any{"type": "string"},
				"units": map[string]any{"type": "string"},
			},
			"required": []any{"city", "units"},
		},
	}
	waiting := &kernel.Step{ID: "s3", Status: kernel.StepWaiting, RequiredCallerUserID: alice.ID, PartialArgs: json.RawMessage(`{"city":"NYC"}`)}
	v3 := enrichStep(k, ctx, waiting, schemaAction, k.NewNames())
	props, ok := v3.AllowedInput["properties"].(map[string]any)
	if !ok {
		t.Fatalf("enrichStep: AllowedInput has no properties: %v", v3.AllowedInput)
	}
	if _, bound := props["city"]; bound {
		t.Error("enrichStep: pre-bound city must not be offered for completion")
	}
	if _, free := props["units"]; !free {
		t.Error("enrichStep: units must be offered for completion")
	}
}

func TestEnrichProcess(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	owner, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "owner@k", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	p := &kernel.Process{ID: "p1", Status: kernel.ProcessOpen, OwnerUserID: owner.ID}
	when := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	// Not awaiting: no entry in the since map.
	v := enrichProcess(ctx, p, map[string]time.Time{}, k.NewNames())
	if v.AwaitingReceipt || v.AwaitingReceiptSince != nil {
		t.Errorf("expected not awaiting, got %+v", v)
	}
	if v.ID != "p1" {
		t.Errorf("embedded Process.ID = %q, want p1", v.ID)
	}
	if v.Owner != "owner@k" {
		t.Errorf("owner = %q, want owner@k", v.Owner)
	}

	// Awaiting: since map carries this process → flag + timestamp surface.
	v2 := enrichProcess(ctx, p, map[string]time.Time{"p1": when}, k.NewNames())
	if !v2.AwaitingReceipt || v2.AwaitingReceiptSince == nil || !v2.AwaitingReceiptSince.Equal(when) {
		t.Errorf("expected awaiting since %v, got %+v", when, v2)
	}
}

func TestEnrichAction(t *testing.T) {
	k := newTestKernel(t)
	ctx := context.Background()
	if err := k.BindOwnName(ctx, testOwnName); err != nil {
		t.Fatal(err)
	}
	bob, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "bob@k", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	a := &kernel.Action{ID: "a1", OwnerUserID: bob.ID, OwnerHandle: "bob", Name: "ping"}
	r := enrichAction(ctx, k, a, k.NewNames())
	if r.ActionRef != "bob@k/ping" {
		t.Errorf("enrichAction: ActionRef = %q, want bob@k/ping", r.ActionRef)
	}

	// A remote proxy is owned by a kernel account: its address is the remote owner's, beneath the
	// peer's local name.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)
	if err := k.ObserveKernel(ctx, key, "provider", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := k.BindPetname(ctx, key, "", false); err != nil {
		t.Fatal(err)
	}
	mount, err := k.EnsureKernelAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &kernel.Action{ID: "a2", OwnerUserID: mount.ID, Name: "bob/greet", Kind: kernel.KindRemoteProxy}
	r2 := enrichAction(ctx, k, proxy, k.NewNames())
	if r2.ActionRef != "bob@provider/greet" {
		t.Errorf("proxy ActionRef = %q, want bob@provider/greet", r2.ActionRef)
	}
	// The response is built from a copy, so enrichment never writes display state back onto the row.
	if proxy.OwnerHandle != "" {
		t.Errorf("enrichAction mutated the caller's action: owner_handle = %q", proxy.OwnerHandle)
	}
	// owner_handle is not a response field: the address names the owner.
	b, _ := json.Marshal(r2)
	if strings.Contains(string(b), `"owner_handle"`) {
		t.Errorf("owner_handle leaked into the response: %s", b)
	}
}

// An http action's request shape is served once, as the decomposed object — never also as the
// encoded string it is stored in (R2, no double-encoded JSON). A wasm action keeps its source,
// which is authored text and a documented read path (§9).
func TestEnrichActionOmitsEncodedHTTPSource(t *testing.T) {
	k := newTestKernel(t)
	ctx := context.Background()
	names := k.NewNames()

	src := `{"method":"GET","base_url":"https://api.example.com","path":"/v1/ping"}`
	httpAction := &kernel.Action{ID: "h1", OwnerHandle: "bob", Name: "ping", Kind: kernel.KindHTTP, Source: src}
	r := enrichAction(ctx, k, httpAction, names)
	if r.Source != "" {
		t.Errorf("http read still carries the encoded source: %q", r.Source)
	}
	if r.HTTP == nil || r.HTTP.Method != "GET" || r.HTTP.URL != "https://api.example.com/v1/ping" {
		t.Errorf("http view = %+v, want the decomposed request shape", r.HTTP)
	}
	if httpAction.Source != src {
		t.Errorf("the caller's row lost its source: %q", httpAction.Source)
	}

	wasm := &kernel.Action{ID: "w1", OwnerHandle: "bob", Name: "calc", Kind: kernel.KindWasm, Source: "package main"}
	if got := enrichAction(ctx, k, wasm, names); got.Source != "package main" {
		t.Errorf("wasm source = %q, want it preserved on a detail read", got.Source)
	}

	// The kind decides the read shape, not whether the stored source parses: a row too malformed to
	// decompose is exactly the one that must not fall back to serving the encoded blob.
	broken := &kernel.Action{ID: "h2", OwnerHandle: "bob", Name: "bad", Kind: kernel.KindHTTP, Source: "not json"}
	if got := enrichAction(ctx, k, broken, names); got.Source != "" || got.HTTP != nil {
		t.Errorf("malformed http row: source = %q, http = %+v; want neither served", got.Source, got.HTTP)
	}
}

func TestGetMe(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	id, _ := makeUser(t, k, "svc-me")

	view, err := getMe(k, ctx, id)
	if err != nil {
		t.Fatalf("getMe: %v", err)
	}
	if view["id"] != id {
		t.Errorf("getMe: id = %v, want %s", view["id"], id)
	}
	if view["address"] != "svc-me@k" {
		t.Errorf("getMe: address = %v, want svc-me@k", view["address"])
	}
	for _, key := range []string{"id", "address", "description", "available", "locked"} {
		if _, ok := view[key]; !ok {
			t.Errorf("getMe: missing key %q", key)
		}
	}
}

func TestCreateAction_ServiceEnrichment(t *testing.T) {
	srv, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	backend := newStepBackend(t)
	ownerID, ownerTok := makeUser(t, k, "svc-ca")
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
	if a.ActionRef != "svc-ca@k/svc-make" {
		t.Errorf("ActionRef = %q, want svc-ca@k/svc-make", a.ActionRef)
	}
	if a.ID == "" {
		t.Error("ID should not be empty")
	}
}

func TestGetAction_EnrichesRef(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()

	ownerID, _ := makeUser(t, k, "svc-ga")
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
	if got.ActionRef != "svc-ga@k/svc-lookup" {
		t.Errorf("getAction ActionRef = %q, want svc-ga@k/svc-lookup", got.ActionRef)
	}
}

// TestActionAuthFieldsExposed: action reads surface the non-secret auth_scheme and requires_grant,
// and never the config or secrets (§8/R9).
func TestActionAuthFieldsExposed(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	ownerID, _ := makeUser(t, k, "svc-auth")
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

	ownerID, _ := makeUser(t, k, "svc-lpa")
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
	if err := k.SetActive(ctx, ownerID, a.ID, true); err != nil {
		t.Fatalf("enableAction: %v", err)
	}
	if _, err := updateActions(k, ctx, ownerID, a.ID, kernel.UpdateActionRequest{Visibility: visPtr(kernel.VisibilityPublic)}); err != nil {
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
	byOwner, err := listPublicActions(k, ctx, "", "svc-lpa@k", "", false, 50, 0)
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

func visPtr(v kernel.ActionVisibility) *kernel.ActionVisibility { return &v }

func TestListSteps_Enriched(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	ctx := context.Background()

	backend := newStepBackend(t)
	ownerID, ownerTok := makeUser(t, k, "svc-ls")
	giveCredits(t, k, ownerID, 500)
	makeUser(t, k, "svc-ls-hook")

	_, _ = createStepAction(t, srv, backend.URL, ownerTok, "svc-ls", "svc-ls-action")

	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	_, err := createStep(k, ctx, ownerID, createStepParams{
		TraceID: traceID, ActionRef: "svc-ls@k/svc-ls-action",
		RequiredCaller: "svc-ls-hook@k", PartialArgs: json.RawMessage(`{}`),
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
	if steps[0].Action != "svc-ls@k/svc-ls-action" {
		t.Errorf("listSteps: action field = %q, want svc-ls@k/svc-ls-action", steps[0].Action)
	}
	if steps[0].Owner != "svc-ls@k" {
		t.Errorf("listSteps: owner = %q, want svc-ls@k (the process owner)", steps[0].Owner)
	}
}

func TestGetStep_Enriched(t *testing.T) {
	srv, k, db := newTestHTTPServerFull(t)
	ctx := context.Background()

	backend := newStepBackend(t)
	ownerID, ownerTok := makeUser(t, k, "svc-gs")
	giveCredits(t, k, ownerID, 500)
	hookID, _ := makeUser(t, k, "svc-gs-hook")

	_, _ = createStepAction(t, srv, backend.URL, ownerTok, "svc-gs", "svc-gs-action")

	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	view, err := createStep(k, ctx, ownerID, createStepParams{
		TraceID: traceID, ActionRef: "svc-gs@k/svc-gs-action",
		RequiredCaller: "svc-gs-hook@k", PartialArgs: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("createStep: %v", err)
	}

	got, err := getStep(k, ctx, ownerID, view.ID)
	if err != nil {
		t.Fatalf("getStep: %v", err)
	}
	if got.Action != "svc-gs@k/svc-gs-action" {
		t.Errorf("getStep action = %q, want svc-gs@k/svc-gs-action", got.Action)
	}
	if got.ID != view.ID {
		t.Errorf("getStep ID = %q, want %q", got.ID, view.ID)
	}
	if got.Owner != "svc-gs@k" {
		t.Errorf("getStep owner = %q, want svc-gs@k (the process owner)", got.Owner)
	}
	// The required caller is not the owner, yet must still see the owner_handle — the resolver
	// is unauthorized, so a non-owner viewer of the step gets the owner without a process-read.
	asHook, err := getStep(k, ctx, hookID, view.ID)
	if err != nil {
		t.Fatalf("getStep as required caller: %v", err)
	}
	if asHook.Owner != "svc-gs@k" {
		t.Errorf("getStep(required caller) owner_handle = %q, want @svc-gs", asHook.Owner)
	}
}

func TestCreateStep_SharedBehavior(t *testing.T) {
	backend := newStepBackend(t)
	srv, k, db := newTestHTTPServerFull(t)
	ctx := context.Background()

	ownerID, ownerTok := makeUser(t, k, "svc-step-owner")
	giveCredits(t, k, ownerID, 500)
	makeUser(t, k, "svc-webhook")

	actID, _ := createStepAction(t, srv, backend.URL, ownerTok, "svc-step-owner", "svc-notify")

	p := setupProcessHTTP(t, db, ownerID, 100)
	traceID := setupTraceForProcess(t, db, p.ID)

	// Create step by @owner/name.
	view, err := createStep(k, ctx, ownerID, createStepParams{
		TraceID:        traceID,
		ActionRef:      "svc-step-owner@k/svc-notify",
		RequiredCaller: "svc-webhook@k",
		PartialArgs:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("createStep by @owner/name: %v", err)
	}
	if view.Action != "svc-step-owner@k/svc-notify" {
		t.Errorf("action field: got %q, want %q", view.Action, "svc-step-owner@k/svc-notify")
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
		RequiredCaller: "svc-webhook@k",
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
	ownerID, _ := makeUser(t, k, "svc-httpview")

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

// Names.Address names a user by address, falls back to the raw id only when the account row is
// gone (a purged peer, §13), and is empty for an empty id.
func TestNamesAddressFallback(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "alice@k", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	names := k.NewNames()
	if got := names.Address(ctx, kernel.Principal{AccountID: u.ID}); got != "alice@k" {
		t.Errorf("Address(alice) = %q, want alice@k", got)
	}
	if got := names.Address(ctx, kernel.Principal{AccountID: "gone"}); got != "gone" {
		t.Errorf("Address(gone) = %q, want raw-id fallback", got)
	}
	if got := names.Address(ctx, kernel.Principal{}); got != "" {
		t.Errorf("Address(empty) = %q, want empty", got)
	}
}

// enrichTx names the three parties and the action by address and — critically — the raw *_user_id
// UUIDs and the stored action_name must not survive to the JSON (the omitempty-shadow drop, where a
// plain json:"-" would fail).
func TestEnrichTxDropsUUIDs(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	ids := map[string]string{}
	for _, h := range []string{"owner", "caller", "target"} {
		u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: h + "@k", Password: "password"})
		if err != nil {
			t.Fatal(err)
		}
		ids[h] = u.ID
	}
	tv := &kernel.TransactionView{Transaction: &kernel.Transaction{
		ID: "tx1", OwnerUserID: ids["owner"], CallerUserID: ids["caller"], TargetUserID: ids["target"], ActionName: "ping",
	}}
	v := enrichTx(ctx, tv, k.NewNames())
	if v.Owner != "owner@k" || v.Caller != "caller@k" || v.Target != "target@k" || v.Action != "target@k/ping" {
		t.Fatalf("parties: %q/%q/%q action %q", v.Owner, v.Caller, v.Target, v.Action)
	}
	b, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"owner_user_id", "caller_user_id", "target_user_id", "action_name"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s must be dropped from tx JSON, got %v", k, m[k])
		}
	}
	if m["owner"] != "owner@k" || m["id"] != "tx1" {
		t.Errorf("expected owner=owner@k and id=tx1, got %v / %v", m["owner"], m["id"])
	}
}

// TestListActionsActiveOnlyByDefault: the superuser's default action list is active-only (so a
// deactivated proxy disappears, like after unfriend); includeInactive brings inactive rows back.
func TestListActionsActiveOnlyByDefault(t *testing.T) {
	_, k, _ := newTestHTTPServerFull(t)
	ctx := context.Background()
	sys, err := k.ReadUserByHandle(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}

	ownerID, _ := makeUser(t, k, "svc-inact")
	backend := newStepBackend(t)
	a, err := createAction(k, ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: "dead", Kind: kernel.KindHTTP, Source: backend.URL,
		Description: "inactive action", InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updateActions(k, ctx, ownerID, a.ID, kernel.UpdateActionRequest{Visibility: visPtr(kernel.VisibilityPublic)}); err != nil {
		t.Fatal(err)
	}
	// Left inactive (never enabled).

	has := func(resps []actionSummary) bool {
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

// TestATargetIsWhatItsNounNames: every admin route says which namespace its target belongs to, so
// a name is resolved as that kind or not at all — a petname equal to a handle can no more take a
// deposit than be mistaken for the account that owns it, and nothing is ever guessed from a name's
// shape (§14).
func TestATargetIsWhatItsNounNames(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()

	local, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "shared@k", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.BindPetname(ctx, key, "kernelonly", true); err != nil {
		t.Fatal(err)
	}
	// The same string is both a handle and a petname here, which is exactly the case the noun
	// settles: neither route has to ask which was meant.
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	key2 := base64.RawURLEncoding.EncodeToString(pub2)
	if _, err := k.BindPetname(ctx, key2, "shared", true); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, ident, noun string
		wantAcct, wantKey string
	}{
		{"an address under users", "shared@k", "user", local.ID, ""},
		{"a petname under peers", "kernelonly", "peer", "", key},
		{"a raw key under peers", key, "peer", "", key},
		{"a petname under users", "kernelonly@k", "user", "", ""},
		{"a raw key under users", key, "user", "", ""},
		{"one name, two namespaces: the noun decides", "shared", "peer", "", key2},
		{"a name that is neither", "nobody@k", "user", "", ""},
		{"this kernel's own name is not a peer", "k", "peer", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			acct, gotKey, err := resolveTarget(k, ctx, c.ident, c.noun)
			if c.wantAcct == "" && c.wantKey == "" {
				if err == nil {
					t.Fatalf("%s resolved as a %s: acct=%v key=%q", c.ident, c.noun, acct, gotKey)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if c.wantKey != "" && gotKey != c.wantKey {
				t.Errorf("key: got %q, want %q", gotKey, c.wantKey)
			}
			if c.wantAcct != "" && (acct == nil || acct.ID != c.wantAcct) {
				t.Errorf("account: got %v, want %s", acct, c.wantAcct)
			}
		})
	}
}

// TestAccountCacheReferenceRendersKernels: an unbound kernel account renders as its public key, not
// a raw UUID — §14 requires a rendered identity to be a consumable command input, and a key is one.
func TestAccountCacheReferenceRendersKernels(t *testing.T) {
	k, _ := newRemoteTestKernel(t)
	ctx := context.Background()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)

	acct, err := k.EnsureKernelAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := k.Address(ctx, kernel.Principal{AccountID: acct.ID}); got != key {
		t.Errorf("unbound kernel account rendered %q, want its key %s", got, key)
	}
	if _, err := k.BindPetname(ctx, key, "named-peer", true); err != nil {
		t.Fatal(err)
	}
	if got := k.Address(ctx, kernel.Principal{AccountID: acct.ID}); got != "named-peer" {
		t.Errorf("bound kernel account rendered %q, want its petname", got)
	}
}

// TestATargetIsNeverATombstone: the mixed admin commands take an id, so the purged-peer anchor
// must be refused there too — it names no live entity (§13).
func TestATargetIsNeverATombstone(t *testing.T) {
	k, st := newRemoteTestKernel(t)
	ctx := context.Background()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	acct, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PurgePeerCascade(ctx, acct.ID); err != nil {
		t.Fatal(err)
	}
	// A tombstone has no handle, so no address reaches it, and an id is not an address.
	if _, _, err := resolveTarget(k, ctx, acct.ID, "user"); err == nil {
		t.Error("tombstone id: want an error")
	}
}

// A list is a summary. What a detail read carries beyond it — an action's authored source and
// compiled artifact, a call's arguments and result — is fetched by id, never paged: a page of
// fifty wasm actions would otherwise carry fifty compiled modules. The projections are checked on
// what they serialize, which is what a client sees.
func TestListProjectionsDropThePayloadsADetailReadKeeps(t *testing.T) {
	a := actionResp{Action: &kernel.Action{ID: "w", Name: "calc", Kind: kernel.KindWasm,
		Source: "package main", WasmArtifact: "AGFzbQ"}}
	detail, _ := json.Marshal(a)
	list, _ := json.Marshal(actionSummary{actionResp: a})
	for _, key := range []string{`"source"`, `"wasm_artifact"`} {
		if !strings.Contains(string(detail), key) {
			t.Errorf("a detail read lost %s", key)
		}
		if strings.Contains(string(list), key) {
			t.Errorf("a list row carries %s", key)
		}
	}
	if !strings.Contains(string(list), `"name":"calc"`) {
		t.Error("the list row lost the fields it should keep")
	}

	tv := &txView{TransactionView: &kernel.TransactionView{Transaction: &kernel.Transaction{ID: "t",
		ArgsJSON: json.RawMessage(`{"big":1}`), ReplyJSON: json.RawMessage(`{"big":2}`),
		RemoteReceiptJSON: `{"charge":1}`}}, Owner: "bob@k"}
	txDetail, _ := json.Marshal(tv)
	txList, _ := json.Marshal(txSummary{txView: *tv})
	for _, key := range []string{`"args"`, `"result"`} {
		if !strings.Contains(string(txDetail), key) {
			t.Errorf("a transaction detail read lost %s", key)
		}
		if strings.Contains(string(txList), key) {
			t.Errorf("a transaction list row carries %s", key)
		}
	}
	// The receipt is evidence, not payload, and stays on the row; the handle stays, the id does not.
	if !strings.Contains(string(txList), `"remote_receipt_json"`) || !strings.Contains(string(txList), `"owner":"bob@k"`) {
		t.Error("the transaction list row lost its receipt or its handle")
	}
	if strings.Contains(string(txList), `"owner_user_id"`) {
		t.Error("the transaction list row carries a raw user id")
	}
	// The CLI decodes these rows and relays them; a shape it cannot decode prints nothing, which is
	// how `tx list` came to show an empty list for a kernel with transactions.
	var back []*txSummary
	if err := json.Unmarshal([]byte("["+string(txList)+"]"), &back); err != nil {
		t.Fatalf("the CLI cannot decode a list row: %v", err)
	}
	if len(back) != 1 || back[0].ID != "t" || back[0].Owner != "bob@k" {
		t.Errorf("the row did not survive the CLI round-trip: %+v", back)
	}
	var actions []actionSummary
	if err := json.Unmarshal([]byte("["+string(list)+"]"), &actions); err != nil || len(actions) != 1 || actions[0].Name != "calc" {
		t.Errorf("the CLI cannot decode an action list row: %v %+v", err, actions)
	}
}
