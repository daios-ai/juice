package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func TestRequireSuperuser(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)

	admin, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@sys", Email: "sys@sys", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	regular, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@regular", Email: "regular@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	adminToken, err := env.k.Login(ctx, "@sys", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(adminToken); err != nil {
		t.Fatal(err)
	}
	got, err := requireSuperuser(env.k)
	if err != nil {
		t.Fatalf("admin should pass superuser check: %v", err)
	}
	if got != admin.ID {
		t.Fatalf("subject id: got %q, want %q", got, admin.ID)
	}

	regularToken, err := env.k.Login(ctx, "@regular", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(regularToken); err != nil {
		t.Fatal(err)
	}
	_, err = requireSuperuser(env.k)
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("regular user should be unauthorized, got %v", err)
	}
	if regular.ID == "" {
		t.Fatal("regular user setup failed")
	}
}

// TestAdminDepositEnforcesSuperuser exercises the exact code path that
// adminUserDepositCmd uses: requireSuperuser guard followed by k.Deposit.
func TestAdminDepositEnforcesSuperuser(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)

	if err := env.k.FirstBoot(ctx, "pass"); err != nil {
		t.Fatal(err)
	}
	su, err := env.k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	regular, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@regular", Email: "regular@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@recipient", Email: "rec@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Non-superuser token: requireSuperuser must reject before Deposit is reached.
	regularToken, err := env.k.Login(ctx, "@regular", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(regularToken); err != nil {
		t.Fatal(err)
	}
	_, err = requireSuperuser(env.k)
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("non-superuser should be rejected by requireSuperuser, got %v", err)
	}
	_ = regular.ID // referenced above

	// Superuser token: requireSuperuser succeeds, Deposit goes through.
	suToken, err := env.k.Login(ctx, "@sys", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(suToken); err != nil {
		t.Fatal(err)
	}
	subjectID, err := requireSuperuser(env.k)
	if err != nil {
		t.Fatalf("superuser should pass requireSuperuser: %v", err)
	}
	if subjectID != su.ID {
		t.Fatalf("subjectID: got %q, want %q", subjectID, su.ID)
	}
	d, err := env.k.Deposit(ctx, subjectID, recipient.ID, 500, "test grant", "")
	if err != nil {
		t.Fatalf("deposit by superuser: %v", err)
	}
	if d.Amount != 500 {
		t.Errorf("deposit amount: got %d, want 500", d.Amount)
	}
}

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

// TestAdminActionsCmdRendersSingleAt proves `admin actions` prints @owner/name with a
// single leading @ — OwnerHandle already carries it (regression: it used to print @@).
func TestAdminActionsCmdRendersSingleAt(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	if err := env.k.FirstBoot(ctx, "pass"); err != nil {
		t.Fatal(err)
	}
	owner, err := env.k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle: "@owner", Email: "owner@example.com", Password: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "svc", Kind: kernel.KindHTTP, Price: 0,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Source: "http://example.com",
	}); err != nil {
		t.Fatal(err)
	}
	suToken, err := env.k.Login(ctx, "@sys", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(suToken); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error {
		_, e := execTestCmd(t, adminActionsCmd())
		return e
	})
	if !strings.Contains(out, "@owner/svc") {
		t.Errorf("expected @owner/svc in output, got: %q", out)
	}
	if strings.Contains(out, "@@") {
		t.Errorf("double-@ regression in admin actions output: %q", out)
	}
}

// TestAdminListTxRowsEnrichesAndStaysCanonical proves admin txs resolves @owner/name and
// party handles for human output while keeping --json canonical (rating embedded, no
// display-only fields leaked).
func TestAdminListTxRowsEnrichesAndStaysCanonical(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer backend.Close()

	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@tx-owner")
	_, callerTok := makeUser(t, k, "@tx-caller")

	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": "svc", "kind": "http", "price": 0, "source": backend.URL,
		"description": "test action", "input_schema": minSchema, "output_schema": minSchema,
	}, ownerTok)
	var action kernel.Action
	decodeResponse(t, cr, &action)
	httpDo(t, srv, "POST", "/v1/actions/"+action.ID+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions/"+action.ID, map[string]any{"public": true}, ownerTok).Body.Close()

	call := httpDo(t, srv, "POST", "/v1/run", map[string]any{
		"action": "@tx-owner/svc", "args": map[string]any{},
	}, callerTok)
	var callReply kernel.CallReply
	decodeResponse(t, call, &callReply)
	if callReply.TxID == "" {
		t.Fatal("expected tx_id from run")
	}
	httpDo(t, srv, "POST", "/v1/transactions/"+callReply.TxID+"/rate", map[string]any{"rating": 1}, callerTok).Body.Close()

	rows, err := adminListTxRows(k, context.Background(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.ActionRef != "@tx-owner/svc" {
		t.Errorf("ActionRef: got %q, want @tx-owner/svc", r.ActionRef)
	}
	// Root run: payer (P) and caller (C) are both the run requester (@tx-caller).
	if r.PayerHandle != "@tx-caller" {
		t.Errorf("PayerHandle: got %q, want @tx-caller", r.PayerHandle)
	}
	if r.CallerHandle != "@tx-caller" {
		t.Errorf("CallerHandle: got %q, want @tx-caller", r.CallerHandle)
	}
	if r.Rating == nil || r.Rating.Value != 1 {
		t.Errorf("embedded rating: got %+v, want value 1", r.Rating)
	}

	blob, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	s := string(blob)
	if !strings.Contains(s, `"rating"`) {
		t.Errorf("canonical JSON should include rating: %s", s)
	}
	if strings.Contains(s, "ActionRef") || strings.Contains(s, "PayerHandle") || strings.Contains(s, "CallerHandle") {
		t.Errorf("display-only fields must not leak into canonical JSON: %s", s)
	}
}

// TestBulkImportPeerActions verifies that bulkImportPeerActions fetches all
// actions from the mock peer, imports them, enables them, and makes them public.
func TestBulkImportPeerActions(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	const actionID = "bulk-action-id"
	m := kernel.ActionManifest{
		ActionID:     actionID,
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

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			json.NewEncoder(w).Encode([]map[string]string{{"id": actionID, "name": "hello"}})
		}
	}))
	defer remote.Close()

	ctx := t.Context()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	peerUser, err := k.AddPeer(ctx, sys.ID, "@bulk-peer", pubB64, remote.URL)
	if err != nil {
		t.Fatal(err)
	}

	imported, skipped := bulkImportPeerActions(ctx, k, sys.ID, peerUser, true)
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
		if a.Name == "hello" {
			found = a
			break
		}
	}
	if found == nil {
		t.Fatal("imported action 'hello' not found in ListAllActions")
	}
	if !found.Active {
		t.Error("imported action should be enabled (active=true) after bulkImportPeerActions")
	}
	if !found.Public {
		t.Error("imported action should be public after bulkImportPeerActions")
	}
}

// TestBulkImportPeerActionsSkipsInvalidManifest verifies that when the manifest
// endpoint returns an error, the action is counted as skipped, not imported.
func TestBulkImportPeerActionsSkipsInvalidManifest(t *testing.T) {
	k, _ := newRemoteTestKernel(t)

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/manifest") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode([]map[string]string{{"id": "skip-id", "name": "broken"}})
	}))
	defer remote.Close()

	ctx := t.Context()
	sys, err := k.ReadUserByHandle(ctx, "@sys")
	if err != nil {
		t.Fatal(err)
	}
	peerUser, err := k.AddPeer(ctx, sys.ID, "@skip-peer", pubB64, remote.URL)
	if err != nil {
		t.Fatal(err)
	}

	imported, skipped := bulkImportPeerActions(ctx, k, sys.ID, peerUser, true)
	if imported != 0 {
		t.Errorf("imported: got %d, want 0", imported)
	}
	if skipped != 1 {
		t.Errorf("skipped: got %d, want 1", skipped)
	}
}
