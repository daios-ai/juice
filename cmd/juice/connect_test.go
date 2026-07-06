package main

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
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

// TestUserConnectCommandTree pins the CLI surface: `user connect <action>` (with --device) and
// `user disconnect <action>`.
func TestUserConnectCommandTree(t *testing.T) {
	c := userConnectCmd()
	if c.Use != "connect <action>" {
		t.Errorf("connect Use = %q", c.Use)
	}
	if c.Flags().Lookup("device") == nil {
		t.Error("user connect missing --device flag")
	}
	if userDisconnectCmd().Use != "disconnect <action>" {
		t.Errorf("disconnect Use = %q", userDisconnectCmd().Use)
	}
}

// TestUserDisconnectCLI drives `juice user disconnect` end-to-end through the DELETE /v1/grants route.
func TestUserDisconnectCLI(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	uid, tok := makeUser(t, env.k, "@grant-cli")
	aid := createDelegatedCLIAction(t, env.k, uid, "inbox")
	if _, err := env.k.CreateGrant(ctx, uid, aid, "refresh-tok"); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}

	if _, err := execTestCmd(t, userDisconnectCmd(), "@grant-cli/inbox"); err != nil {
		t.Fatalf("user disconnect: %v", err)
	}
	if views, _ := env.k.ListGrantViews(ctx, uid); len(views) != 0 {
		t.Errorf("grant survived CLI revoke: %d", len(views))
	}
}

// TestRunGrantRequiredNonTTY: `juice run` on a grant-needing action in a non-interactive test
// env returns the structured ErrGrantRequired (no hang on a browser prompt).
func TestRunGrantRequiredNonTTY(t *testing.T) {
	env := newTestEnv(t)
	uid, tok := makeUser(t, env.k, "@run-grant")
	createDelegatedCLIAction(t, env.k, uid, "inbox")
	if err := saveToken(tok); err != nil {
		t.Fatal(err)
	}
	_, err := execTestCmd(t, runCmd(), "@run-grant/inbox", "{}")
	if !errors.Is(err, kernel.ErrGrantRequired) {
		t.Fatalf("run without grant: got %v, want ErrGrantRequired", err)
	}
	if grantActionRef(err, "fallback") != "@run-grant/inbox" {
		t.Errorf("grantActionRef did not recover the action from meta: %q", grantActionRef(err, "fallback"))
	}
}
