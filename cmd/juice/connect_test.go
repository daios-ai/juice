package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
)

// createDelegatedCLIAction creates and activates an oauth_delegated http action owned by ownerID.
func createDelegatedCLIAction(t *testing.T, k *kernel.Kernel, ownerID, name string) string {
	t.Helper()
	ctx := context.Background()
	a, err := k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: name, Kind: kernel.KindHTTP, Price: 0,
		Source: "https://provider.example/api", Description: "delegated",
		InputSchema: minSchema, OutputSchema: minSchema,
		Auth: &kernel.AuthInput{
			Scheme: kernel.AuthSchemeOAuthDelegated,
			Config: map[string]any{"auth_url": "https://p.example/a", "token_url": "https://p.example/t", "client_id": "c"},
		},
	})
	if err != nil {
		t.Fatalf("create delegated action: %v", err)
	}
	if err := k.SetActive(ctx, ownerID, a.ID, true); err != nil {
		t.Fatalf("activate: %v", err)
	}
	return a.ID
}

// TestUserConnectCommandTree pins the CLI surface: `user connect <selector>` (with --device,
// --token, --yes) and `user disconnect [selector]` (with --account).
func TestUserConnectCommandTree(t *testing.T) {
	c := userConnectCmd()
	if c.Use != "connect <selector>" {
		t.Errorf("connect Use = %q", c.Use)
	}
	for _, f := range []string{"device", "token", "yes"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("user connect missing --%s flag", f)
		}
	}
	d := userDisconnectCmd()
	if d.Use != "disconnect [selector]" {
		t.Errorf("disconnect Use = %q", d.Use)
	}
	if d.Flags().Lookup("account") == nil {
		t.Error("user disconnect missing --account flag")
	}
}

// TestUserDisconnectCLI drives `juice user disconnect` end-to-end through the DELETE /v1/grants route.
func TestUserDisconnectCLI(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	uid, tok := makeUser(t, env.k, "grant-cli")
	aid := createDelegatedCLIAction(t, env.k, uid, "inbox")
	plan, err := env.k.ConsentPlan(ctx, uid, "grant-cli/inbox")
	if err != nil || len(plan.Groups) != 1 {
		t.Fatalf("consent plan: %v groups=%d", err, len(plan.Groups))
	}
	if _, err := env.k.CreateGrants(ctx, uid, plan.Groups[0].ProviderKey, []string{aid}, "refresh-tok", ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	if _, err := execTestCmd(t, userDisconnectCmd(), "grant-cli/inbox"); err != nil {
		t.Fatalf("user disconnect: %v", err)
	}
	if views, _ := env.k.ListGrantViews(ctx, uid); len(views) != 0 {
		t.Errorf("grant survived CLI revoke: %d", len(views))
	}
}

// TestDeleteGrantsHasOneSpelling: revocation names a selector or an account, and nothing else. The
// full owner/name is the degenerate single-action selector, so a second ?action= spelling was pure
// duplication (§12 rule 6) — it is gone, and a request carrying only it is an ordinary bad request
// rather than a silent second path.
func TestDeleteGrantsHasOneSpelling(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	uid, tok := makeUser(t, env.k, "grant-alias")
	aid := createDelegatedCLIAction(t, env.k, uid, "inbox")
	plan, err := env.k.ConsentPlan(ctx, uid, "grant-alias/inbox")
	if err != nil || len(plan.Groups) != 1 {
		t.Fatalf("consent plan: %v groups=%d", err, len(plan.Groups))
	}
	if _, err := env.k.CreateGrants(ctx, uid, plan.Groups[0].ProviderKey, []string{aid}, "refresh-tok", ""); err != nil {
		t.Fatalf("grant: %v", err)
	}

	srv := httptest.NewServer(mountFullRouter(&server{kernel: env.k, log: log.Discard()}))
	t.Cleanup(srv.Close)

	resp := httpDo(t, srv, "DELETE", "/v1/grants?action=grant-alias/inbox", nil, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("the legacy ?action= alias must not revoke: status = %d, want 422 (invalid_input)", resp.StatusCode)
	}
	if views, _ := env.k.ListGrantViews(ctx, uid); len(views) != 1 {
		t.Fatalf("the grant must survive a request naming no selector: %d", len(views))
	}
	// The canonical spelling still works.
	ok := httpDo(t, srv, "DELETE", "/v1/grants?selector=grant-alias/inbox", nil, tok)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("selector revoke: status = %d, want 200", ok.StatusCode)
	}
	if views, _ := env.k.ListGrantViews(ctx, uid); len(views) != 0 {
		t.Errorf("grant survived the canonical revoke: %d", len(views))
	}
}

// createBearerCLIAction creates and activates a delegated_bearer http action owned by ownerID.
func createBearerCLIAction(t *testing.T, k *kernel.Kernel, ownerID, name, source string) {
	t.Helper()
	ctx := context.Background()
	a, err := k.CreateAction(ctx, ownerID, kernel.CreateActionRequest{
		OwnerUserID: ownerID, Name: name, Kind: kernel.KindHTTP, Price: 0,
		Source: source, Description: "bearer", InputSchema: minSchema, OutputSchema: minSchema,
		Auth: &kernel.AuthInput{Scheme: kernel.AuthSchemeDelegatedBearer},
	})
	if err != nil {
		t.Fatalf("create bearer action %s: %v", name, err)
	}
	if err := k.SetActive(ctx, ownerID, a.ID, true); err != nil {
		t.Fatalf("activate %s: %v", name, err)
	}
}

// TestUserConnectTokenBatchCLI: `user connect @owner/dir --token` connects a directory of
// delegated_bearer actions in one command into one connection; disconnect by selector clears them.
func TestUserConnectTokenBatchCLI(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	uid, tok := makeUser(t, env.k, "chatcli")
	createBearerCLIAction(t, env.k, uid, "chat/send", "https://api.chat.example/x")
	createBearerCLIAction(t, env.k, uid, "chat/history", "https://api.chat.example/x")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	if _, err := execTestCmd(t, userConnectCmd(), "chatcli/chat", "--token", "ghp_x"); err != nil {
		t.Fatalf("user connect --token: %v", err)
	}
	conns, _ := env.k.ListConnectionViews(ctx, uid)
	if len(conns) != 1 || conns[0].Actions != 2 {
		t.Fatalf("expected one connection with two actions, got %+v", conns)
	}

	// Disconnect by selector removes both grants; the connection remains (now unused).
	if _, err := execTestCmd(t, userDisconnectCmd(), "chatcli/chat"); err != nil {
		t.Fatalf("user disconnect: %v", err)
	}
	if views, _ := env.k.ListGrantViews(ctx, uid); len(views) != 0 {
		t.Errorf("grants survived selector disconnect: %d", len(views))
	}
	if conns, _ := env.k.ListConnectionViews(ctx, uid); len(conns) != 1 || !conns[0].Unused {
		t.Errorf("connection should remain and be unused after selector disconnect, got %+v", conns)
	}
}

// TestRunGrantRequiredNonTTY: `juice run` on a grant-needing action in a non-interactive test
// env returns the structured ErrGrantRequired (no hang on a browser prompt).
func TestRunGrantRequiredNonTTY(t *testing.T) {
	env := newTestEnv(t)
	uid, tok := makeUser(t, env.k, "run-grant")
	createDelegatedCLIAction(t, env.k, uid, "inbox")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}
	_, err := execTestCmd(t, runCmd(), "run-grant/inbox", "{}")
	if !errors.Is(err, kernel.ErrGrantRequired) {
		t.Fatalf("run without grant: got %v, want ErrGrantRequired", err)
	}
	if grantActionRef(err, "fallback") != "run-grant/inbox" {
		t.Errorf("grantActionRef did not recover the action from meta: %q", grantActionRef(err, "fallback"))
	}
}
