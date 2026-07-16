package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
)

func newAdminTestKernel(t *testing.T) *kernel.Kernel {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "admin_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "admin-test-secret"
	return kernel.New(db, nil, nil, nil, cfg, log.Discard())
}

func TestAdminListUsers(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	for i := 0; i < 3; i++ {
		_, err := k.CreateUser(ctx, kernel.CreateUserRequest{
			Handle:   "@user" + string(rune('a'+i)),
			Email:    "user" + string(rune('a'+i)) + "@example.com",
			Password: "pass",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	users, err := k.ListUsers(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Errorf("expected 3 users, got %d", len(users))
	}
}

func TestAdminSuspendUnsuspend(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	if err := k.FirstBoot(ctx, "pass"); err != nil {
		t.Fatal(err)
	}
	admin, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@target", Email: "target@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Suspend.
	if err := k.SuspendUser(ctx, admin.ID, u.ID); err != nil {
		t.Fatal(err)
	}

	// Login should fail.
	if _, err := k.Login(ctx, "@target", "pass"); err == nil {
		t.Error("expected login to fail for suspended user")
	}

	// Unsuspend.
	if err := k.UnsuspendUser(ctx, admin.ID, u.ID); err != nil {
		t.Fatal(err)
	}

	// Login should succeed.
	if _, err := k.Login(ctx, "@target", "pass"); err != nil {
		t.Errorf("expected login to succeed after unsuspend, got: %v", err)
	}
}

func TestAdminDeposit(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	if err := k.FirstBoot(ctx, "pass"); err != nil {
		t.Fatal(err)
	}
	admin, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@recipient", Email: "r@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Deposit succeeds and balance increases.
	d, err := k.Deposit(ctx, admin.ID, u.ID, 500, "initial grant", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Amount != 500 {
		t.Errorf("deposit amount: got %d, want 500", d.Amount)
	}
	if d.OperatorUserID != admin.ID {
		t.Errorf("operator: got %q, want %q", d.OperatorUserID, admin.ID)
	}

	u2, err := k.ReadUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u2.Available != 500 {
		t.Errorf("available after deposit: got %d, want 500", u2.Available)
	}

	// Second deposit accumulates.
	if _, err := k.Deposit(ctx, admin.ID, u.ID, 200, "top-up", ""); err != nil {
		t.Fatal(err)
	}
	u3, _ := k.ReadUser(ctx, u.ID)
	if u3.Available != 700 {
		t.Errorf("available after second deposit: got %d, want 700", u3.Available)
	}

	// Zero amount rejected.
	if _, err := k.Deposit(ctx, admin.ID, u.ID, 0, "", ""); err == nil {
		t.Error("expected error for zero amount")
	}

	// Negative amount rejected.
	if _, err := k.Deposit(ctx, admin.ID, u.ID, -1, "", ""); err == nil {
		t.Error("expected error for negative amount")
	}

	// Unknown user rejected.
	if _, err := k.Deposit(ctx, admin.ID, "nonexistent", 100, "", ""); err == nil {
		t.Error("expected error for unknown target user")
	}
}

// Superuser enforcement lives in requireSuperuserMW on the TCP admin routes; it is covered
// end-to-end in control_test.go (TestAdminSuperuserGate).

func TestAdminListAllActions(t *testing.T) {
	ctx := context.Background()
	k := newAdminTestKernel(t)

	u, _ := k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@owner", Email: "owner@example.com", Password: "pass",
	})

	for i := 0; i < 3; i++ {
		_, err := k.CreateAction(ctx, u.ID, kernel.CreateActionRequest{
			OwnerUserID:  u.ID,
			Name:         "action" + string(rune('a'+i)),
			Kind:         kernel.KindHTTP,
			Price:        0,
			InputSchema:  map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
			Source:       "http://example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	actions, err := k.ListAllActions(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 3 {
		t.Errorf("expected 3 actions, got %d", len(actions))
	}
}

// The @sys system-wide tx view is now the standard `tx list` (ListTransactions already drops
// the party filter for superusers); superuser scope on the read endpoints is covered in
// control/serve tests. The bespoke enriched admin-txs view was removed with adminListTxRows.

// fakeManifestFetcher returns canned manifest frames, standing in for the transport so the
// import + enable + publish logic is tested without libp2p.
type fakeManifestFetcher struct{ frames []json.RawMessage }

func (f *fakeManifestFetcher) Manifests(_ context.Context, _ string) ([]json.RawMessage, error) {
	return f.frames, nil
}

// TestBulkImportPeerActions verifies that bulkImportPeerActionsFed imports a peer's manifests,
// enables them, and makes them public.
func TestBulkImportPeerActions(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	m := kernel.ActionManifest{
		ActionID:     "bulk-action-id",
		OwnerHandle:  "@bulk-peer",
		Name:         "hello",
		Description:  "says hello",
		Kind:         kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-bulk",
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig
	mBytes, _ := json.Marshal(m)

	ctx := t.Context()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	peerUser, err := k.AddPeer(ctx, sys.ID, "@bulk-peer", pubB64)
	if err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeManifestFetcher{frames: []json.RawMessage{mBytes}}
	imported, skipped := bulkImportPeerActionsFed(ctx, fetcher, k, sys.ID, pubB64, peerUser)
	if imported != 1 {
		t.Errorf("imported: got %d, want 1", imported)
	}
	if skipped != 0 {
		t.Errorf("skipped: got %d, want 0", skipped)
	}

	actions, err := k.ListAllActions(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found *kernel.Action
	for _, a := range actions {
		if a.Name == "bulk-peer/hello" { // owner-qualified local name
			found = a
			break
		}
	}
	if found == nil {
		t.Fatal("imported action 'bulk-peer/hello' not found in ListAllActions")
	}
	if !found.Active {
		t.Error("imported action should be enabled (active=true) after bulk import")
	}
	if found.Visibility != kernel.VisibilityLocal {
		t.Error("imported action should be local after bulk import (non-transitive: never re-served to peers, §13)")
	}
}

// TestBulkImportPeerActionsSkipsInvalidManifest verifies that a manifest frame that fails to
// verify is counted as skipped, not imported.
func TestBulkImportPeerActionsSkipsInvalidManifest(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	ctx := t.Context()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	peerUser, err := k.AddPeer(ctx, sys.ID, "@skip-peer", pubB64)
	if err != nil {
		t.Fatal(err)
	}

	// A manifest with an invalid signature (never signed) must be skipped, not imported.
	bad := kernel.ActionManifest{
		ActionID: "skip-id", OwnerHandle: "@skip-peer", Name: "broken", Description: "d",
		Kind: kernel.KindHTTP, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "x",
		Stats: &kernel.Stats{}, UpdatedAt: time.Now(), Signature: "bad",
	}
	badBytes, _ := json.Marshal(bad)

	fetcher := &fakeManifestFetcher{frames: []json.RawMessage{badBytes}}
	imported, skipped := bulkImportPeerActionsFed(ctx, fetcher, k, sys.ID, pubB64, peerUser)
	if imported != 0 {
		t.Errorf("imported: got %d, want 0", imported)
	}
	if skipped != 1 {
		t.Errorf("skipped: got %d, want 1", skipped)
	}
}
