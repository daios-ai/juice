package kernel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

func TestRegisterRemoteKernelValidatesIdentity(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	if _, err := k.AddPeer(ctx, sys.ID, "@bad/handle", "not-base64url", "https://remote.example.com"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for handle containing /, got %v", err)
	}

	if _, err := k.AddPeer(ctx, sys.ID, "@bad-key", "not-base64url", "https://remote.example.com"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for malformed public key, got %v", err)
	}

	shortKey := base64.RawURLEncoding.EncodeToString([]byte("short"))
	if _, err := k.AddPeer(ctx, sys.ID, "@short-key", shortKey, "https://remote.example.com"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for short public key, got %v", err)
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	validKey := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.AddPeer(ctx, sys.ID, "@bad-url", validKey, "ftp://remote.example.com"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for unsupported URL scheme, got %v", err)
	}
	if _, err := k.AddPeer(ctx, sys.ID, "@remote", validKey, "https://remote.example.com"); err != nil {
		t.Fatalf("valid remote kernel should register: %v", err)
	}
}

// ---- Remote proxy / manifest tests ----

func TestImportRemoteActionCreatesRemoteProxy(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@remote-peer", base64.RawURLEncoding.EncodeToString(pub), "https://remote.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "remote-action-id-1",
		OwnerHandle:  "@remote-peer",
		Name:         "sum",
		Description:  "sum action",
		Kind:         kernel.KindHTTP,
		Price:        50,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef",
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig
	result, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportRemoteAction: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(result.Created))
	}
	a := result.Created[0]
	if a.Kind != kernel.KindRemoteProxy {
		t.Errorf("kind: got %q, want %q", a.Kind, kernel.KindRemoteProxy)
	}
	if a.RemoteActionID != m.ActionID {
		t.Errorf("remote_action_id: got %q, want %q", a.RemoteActionID, m.ActionID)
	}
	if a.Price != 50 {
		t.Errorf("price: got %d, want 50", a.Price)
	}
}

func TestImportRemoteActionReimp(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@reimp-peer", base64.RawURLEncoding.EncodeToString(pub), "https://reimp.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "reimp-action-id",
		OwnerHandle:  "@reimp-peer",
		Name:         "calc",
		Description:  "calc action",
		Kind:         kernel.KindHTTP,
		Price:        10,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef",
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig
	firstResult, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if len(firstResult.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(firstResult.Created))
	}
	firstID := firstResult.Created[0].ID

	// Reimport with updated price — content hash changes → Updated.
	m.Price = 99
	sig2, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig2
	secondResult, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if len(secondResult.Updated) != 1 {
		t.Fatalf("expected 1 updated action, got %d", len(secondResult.Updated))
	}
	second := secondResult.Updated[0]
	if second.ID != firstID {
		t.Error("reimport must preserve the same action ID")
	}
	if second.Price != 99 {
		t.Errorf("reimport price: got %d, want 99", second.Price)
	}
}

func TestImportRemoteActionUnchangedPreservesActiveAndStats(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@stable-peer", base64.RawURLEncoding.EncodeToString(pub), "https://stable.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "stable-action-id",
		OwnerHandle:  "@stable-peer",
		Name:         "stable",
		Kind:         kernel.KindHTTP,
		Price:        5,
		Description:  "A stable action",
		ArtifactHash: "abc123",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	// First import.
	firstResult, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if len(firstResult.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(firstResult.Created))
	}
	firstID := firstResult.Created[0].ID

	// Activate it so we can verify active state is preserved.
	firstResult.Created[0].Active = true
	firstResult.Created[0].Source = "https://stable.example.com/v1/federation/call?action=%40stable-peer%2Fstable&counterparty="
	_ = st.UpdateAction(ctx, firstResult.Created[0])

	// Re-import the identical manifest (same signature).
	secondResult, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if len(secondResult.Unchanged) != 1 {
		t.Fatalf("expected 1 unchanged action, got: created=%d updated=%d unchanged=%d",
			len(secondResult.Created), len(secondResult.Updated), len(secondResult.Unchanged))
	}
	if secondResult.Unchanged[0].ID != firstID {
		t.Error("unchanged reimport must preserve the same action ID")
	}
	// Action must remain active.
	a, _ := st.ReadAction(ctx, firstID)
	if !a.Active {
		t.Error("unchanged reimport must preserve active state")
	}
}

func TestImportRemoteActionIdempotentAfterUpdate(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@idem-peer", base64.RawURLEncoding.EncodeToString(pub), "https://idem.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "idem-action-id",
		OwnerHandle:  "@idem-peer",
		Name:         "svc",
		Description:  "svc action",
		Kind:         kernel.KindHTTP,
		Price:        10,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef",
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}
	sign := func() {
		t.Helper()
		sig, err := kernel.SignManifest(priv, &m)
		if err != nil {
			t.Fatal(err)
		}
		m.Signature = sig
	}

	sign()
	firstResult, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil || len(firstResult.Created) != 1 {
		t.Fatalf("first import: err=%v created=%d", err, len(firstResult.Created))
	}
	firstID := firstResult.Created[0].ID

	// Second import: price change → Updated, ArtifactHash stored as contentHash.
	m.Price = 99
	sign()
	secondResult, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil || len(secondResult.Updated) != 1 {
		t.Fatalf("second import: err=%v updated=%d", err, len(secondResult.Updated))
	}
	if secondResult.Updated[0].ID != firstID {
		t.Error("reimport must preserve the same action ID")
	}

	// Third import: same manifest as second → Unchanged (ArtifactHash stored correctly).
	thirdResult, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("third import: %v", err)
	}
	if len(thirdResult.Unchanged) != 1 {
		t.Errorf("expected 1 unchanged, got created=%d updated=%d unchanged=%d",
			len(thirdResult.Created), len(thirdResult.Updated), len(thirdResult.Unchanged))
	}
	if thirdResult.Unchanged[0].ID != firstID {
		t.Error("third import must reference the same action ID")
	}
}

func TestImportRemoteActionRejectsInvalidSignature(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@bad-sig-peer", base64.RawURLEncoding.EncodeToString(pub), "https://badsig.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "bad-sig-action",
		OwnerHandle:  "@bad-sig-peer",
		Name:         "greet",
		Kind:         kernel.KindHTTP,
		Price:        0,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Signature:    "invalidsignature",
	}
	_, err = k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err == nil {
		t.Fatal("expected error for invalid manifest signature")
	}
}

func TestImportRemoteActionRejectsNegativePrice(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@neg-price-peer", base64.RawURLEncoding.EncodeToString(pub), "https://neg.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "neg-price-action",
		OwnerHandle:  "@neg-price-peer",
		Name:         "cheap",
		Kind:         kernel.KindHTTP,
		Price:        -1,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	_, err = k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("negative price manifest: want ErrInvalidInput, got %v", err)
	}
}

func TestImportRemoteActionRejectsMissingRequiredFields(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@mrf-peer", base64.RawURLEncoding.EncodeToString(pub), "https://mrf.example.com")
	if err != nil {
		t.Fatal(err)
	}

	base := kernel.ActionManifest{
		ActionID:     "mrf-action-1",
		OwnerHandle:  "@mrf-peer",
		Name:         "mrf-svc",
		Description:  "mrf desc",
		Kind:         kernel.KindHTTP,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef",
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}

	cases := []struct {
		name   string
		mutate func(*kernel.ActionManifest)
	}{
		{"missing name", func(m *kernel.ActionManifest) { m.Name = "" }},
		{"missing description", func(m *kernel.ActionManifest) { m.Description = "" }},
		{"missing input_schema", func(m *kernel.ActionManifest) { m.InputSchema = nil }},
		{"missing stats", func(m *kernel.ActionManifest) { m.Stats = nil }},
		{"missing updated_at", func(m *kernel.ActionManifest) { m.UpdatedAt = time.Time{} }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			tc.mutate(&m)
			sig, _ := kernel.SignManifest(priv, &m)
			m.Signature = sig
			_, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
			if !errors.Is(err, kernel.ErrInvalidInput) {
				t.Errorf("want ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestGetActionManifestIncludesActionID(t *testing.T) {
	st := newTestStore(t)
	su := setupUser(t, st, "@sys", 0)
	k := newTestKernel(st)
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	owner := setupUser(t, st, "@manifest-owner2", 0)
	a := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "manifest2",
		Kind:         kernel.KindHTTP,
		Active:       true,
		Public:       true,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Source:       "https://example.com/call",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	m, err := k.GetActionManifest(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetActionManifest: %v", err)
	}
	if m.ActionID != a.ID {
		t.Errorf("manifest.action_id: got %q, want %q", m.ActionID, a.ID)
	}
	if m.Signature == "" {
		t.Error("manifest signature must be set")
	}
}

// ---- Remote proxy execution test ----

func TestCallRemoteProxyRecordsReceiptHash(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sys := setupSys(t, nil, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	fakeReceiptJSON := `{"tx_id":"remote-tx-1","status":"success"}`
	fake := &fakeFederationHTTP{receiptJSON: fakeReceiptJSON}
	k := newTestKernelWithHTTP(st, fake)

	remoteUser, err := k.AddPeer(ctx, sys.ID, "@proxy-peer", base64.RawURLEncoding.EncodeToString(pub), "https://proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "proxy-action-1",
		OwnerHandle:  "@proxy-peer",
		Name:         "add",
		Kind:         kernel.KindHTTP,
		Price:        0,
		Description:  "add two numbers",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef",
		Stats:        &kernel.Stats{},
		UpdatedAt:    time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	result, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportRemoteAction: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(result.Created))
	}
	a := result.Created[0]

	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	pubFed := true
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubFed}); err != nil {
		t.Fatalf("UpdateAction public: %v", err)
	}

	caller := setupUser(t, st, "@proxy-caller", 0)
	p := setupProcess(t, st, caller.ID, 0)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID:     caller.ID,
		ProcessID:    p.ID,
		IsRootCall:   true,
		TargetUserID: "@proxy-peer",
		ActionName:   "add",
		Args:         map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if reply == nil {
		t.Fatal("expected non-nil reply")
	}

	txs, err := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("expected 1 transaction, got %d", len(txs))
	}
	tx := txs[0]
	if tx.RemoteReceiptHash == "" {
		t.Fatal("expected RemoteReceiptHash to be set")
	}
	h := sha256.Sum256([]byte(fakeReceiptJSON))
	expected := fmt.Sprintf("%x", h)
	if tx.RemoteReceiptHash != expected {
		t.Errorf("RemoteReceiptHash: got %s, want %s", tx.RemoteReceiptHash, expected)
	}
	if tx.RemoteReceiptJSON != fakeReceiptJSON {
		t.Errorf("RemoteReceiptJSON: got %q, want %q", tx.RemoteReceiptJSON, fakeReceiptJSON)
	}
	if tx.Status != kernel.TxSuccess {
		t.Errorf("expected TxSuccess, got %s", tx.Status)
	}
}

// ---- D1: source URL update on base URL change ----

func TestRegisterRemoteKernelUpdatesSourceURLs(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	peer, err := k.AddPeer(ctx, sys.ID, "@url-update-peer", pubB64, "https://old.example.com")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Import an action whose Source URL contains the old base URL.
	m := kernel.ActionManifest{
		ActionID: "url-update-action-1", OwnerHandle: "@url-update-peer", Name: "act",
		Kind: kernel.KindHTTP, Price: 0, Description: "d",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	sig, _ := kernel.SignManifest(priv, &m)
	m.Signature = sig
	result, err := k.ImportRemoteAction(ctx, sys.ID, peer.ID, m)
	if err != nil {
		t.Fatalf("ImportRemoteAction: %v", err)
	}
	a := result.Created[0]
	if !contains(a.Source, "old.example.com") {
		t.Fatalf("expected old base URL in source, got %s", a.Source)
	}

	// Re-register with new base URL using the same public key.
	if _, err := k.AddPeer(ctx, sys.ID, "@url-update-peer", pubB64, "https://new.example.com"); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	// Source URL on existing proxy action must reflect new base URL.
	updated, err := st.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("ReadAction: %v", err)
	}
	if contains(updated.Source, "old.example.com") {
		t.Errorf("old base URL still present in source after base URL change: %s", updated.Source)
	}
	if !contains(updated.Source, "new.example.com") {
		t.Errorf("new base URL not found in source: %s", updated.Source)
	}
}

// ---- D5: duplicate identity guard ----

func TestRegisterRemoteKernelRejectsDuplicateHandle(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	pub1B64 := base64.RawURLEncoding.EncodeToString(pub1)
	pub2B64 := base64.RawURLEncoding.EncodeToString(pub2)

	if _, err := k.AddPeer(ctx, sys.ID, "@dup-handle", pub1B64, "https://a.example.com"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Same handle, different public key → must fail.
	if _, err := k.AddPeer(ctx, sys.ID, "@dup-handle", pub2B64, "https://b.example.com"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for duplicate handle, got %v", err)
	}
}

func TestRegisterRemoteKernelRejectsDuplicateBaseURL(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	pub1B64 := base64.RawURLEncoding.EncodeToString(pub1)
	pub2B64 := base64.RawURLEncoding.EncodeToString(pub2)

	if _, err := k.AddPeer(ctx, sys.ID, "@dup-url-a", pub1B64, "https://shared.example.com"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Same base URL, different public key → must fail.
	if _, err := k.AddPeer(ctx, sys.ID, "@dup-url-b", pub2B64, "https://shared.example.com"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for duplicate base URL, got %v", err)
	}
}

// ---- D4: VerifyRemoteReceipt ----

func TestVerifyRemoteReceiptValid(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sys := setupSys(t, nil, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	k := newTestKernelWithHTTP(st, fake)

	remoteUser, err := k.AddPeer(ctx, sys.ID, "@verify-peer", base64.RawURLEncoding.EncodeToString(pub), "https://verify.example.com")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	m := kernel.ActionManifest{
		ActionID: "verify-action-1", OwnerHandle: "@verify-peer", Name: "vact",
		Kind: kernel.KindHTTP, Price: 0, Description: "v",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	msig, _ := kernel.SignManifest(priv, &m)
	m.Signature = msig
	result, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	a := result.Created[0]
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	pubFed2 := true
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubFed2}); err != nil {
		t.Fatal(err)
	}

	caller := setupUser(t, st, "@verify-caller", 0)
	p := setupProcess(t, st, caller.ID, 0)

	// Build a receipt whose fields match what Call() will record in the transaction.
	// ActionID must be the remote action's ID (manifest ActionID), not the local proxy ID.
	remoteReceipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: "remote-sys",
		TxID: "remote-tx-verify", TraceID: "t1", ActionID: m.ActionID,
		CallerUserID: "c1", ProcessID: "p1",
		ArgsHash:  jcsHashForTest(t, `{}`),
		ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxSuccess, Gross: 0, Net: 0, Fee: 0,
		StartedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	// Sign with the remote peer's private key using the same method as the kernel.
	receiptSig := signReceiptForTest(t, priv, remoteReceipt)
	remoteReceipt.Signature = receiptSig
	receiptBytes, _ := json.Marshal(remoteReceipt)
	fake.receiptJSON = string(receiptBytes)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall: true,
		TargetUserID: "@verify-peer", ActionName: "vact", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	_ = reply

	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	v, err := k.VerifyRemoteReceipt(ctx, caller.ID, txs[0].ID)
	if err != nil {
		t.Fatalf("VerifyRemoteReceipt: %v", err)
	}
	if !v.Checks.ReceiptHash {
		t.Error("expected ReceiptHash check=true")
	}
	if !v.Checks.Signature {
		t.Error("expected Signature check=true")
	}
	if !v.Checks.ActionID {
		t.Error("expected ActionID check=true")
	}
}

func TestVerifyRemoteReceiptNonRemoteProxy(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sys := setupSys(t, nil, st)
	caller := setupUser(t, st, "@vrr-caller", 100)

	// Use a fake HTTP executor so we can call a KindHTTP action and get a local tx.
	fakeHTTP := &fakeSuccessHTTP{}
	k := newTestKernelWithHTTP(st, fakeHTTP)
	p := setupProcess(t, st, caller.ID, 10)

	a, err := k.CreateAction(ctx, sys.ID, kernel.CreateActionRequest{
		OwnerUserID:  sys.ID,
		Name:         "vrr-local",
		Kind:         kernel.KindHTTP,
		Source:       "https://local.example.com/api",
		Description:  "local",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Price:        5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	pubFed3 := true
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubFed3}); err != nil {
		t.Fatal(err)
	}

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall: true,
		TargetUserID: sys.ID, ActionName: "vrr-local", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	_ = reply

	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	if len(txs) == 0 {
		t.Fatal("no transactions")
	}
	_, err = k.VerifyRemoteReceipt(ctx, caller.ID, txs[0].ID)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState for non-remote-proxy tx, got %v", err)
	}
}

func TestVerifyRemoteReceiptSignatureTamper(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sys := setupSys(t, nil, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)

	fake := &fakeFederationHTTP{}
	k := newTestKernelWithHTTP(st, fake)

	remoteUser, _ := k.AddPeer(ctx, sys.ID, "@tamper-peer", base64.RawURLEncoding.EncodeToString(pub), "https://tamper.example.com")
	m := kernel.ActionManifest{
		ActionID: "tamper-action-1", OwnerHandle: "@tamper-peer", Name: "tact",
		Kind: kernel.KindHTTP, Price: 0, Description: "t",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	msig, _ := kernel.SignManifest(priv, &m)
	m.Signature = msig
	result, _ := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	a := result.Created[0]
	_ = k.SetActive(ctx, sys.ID, a.ID, true)
	pubFed4 := true
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubFed4})

	remoteReceipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: "rs",
		TxID: "rt", TraceID: "t1", ActionID: a.ID,
		CallerUserID: "c1", ProcessID: "p1",
		ArgsHash: "aa", ReplyHash: "bb",
		Status: kernel.TxSuccess, Gross: 0, Net: 0, Fee: 0,
		StartedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	// Sign with a different key — signature should fail verification.
	remoteReceipt.Signature = signReceiptForTest(t, otherPriv, remoteReceipt)
	receiptBytes, _ := json.Marshal(remoteReceipt)
	fake.receiptJSON = string(receiptBytes)

	caller := setupUser(t, st, "@tamper-caller", 0)
	p := setupProcess(t, st, caller.ID, 0)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall: true,
		TargetUserID: "@tamper-peer", ActionName: "tact", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	_ = reply

	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	v, err := k.VerifyRemoteReceipt(ctx, caller.ID, txs[0].ID)
	if err != nil {
		t.Fatalf("VerifyRemoteReceipt: %v", err)
	}
	if v.Checks.Signature {
		t.Error("expected Signature check=false for receipt signed with wrong key")
	}
	if v.Valid {
		t.Error("expected Valid=false for tampered receipt")
	}
}

// TestVerifyRemoteReceiptAfterProxyDeleted verifies that VerifyRemoteReceipt still
// returns valid:true after the local remote_proxy action has been soft-deleted.
func TestVerifyRemoteReceiptAfterProxyDeleted(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sys := setupSys(t, nil, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	k := newTestKernelWithHTTP(st, fake)

	remoteUser, err := k.AddPeer(ctx, sys.ID, "@del-peer", base64.RawURLEncoding.EncodeToString(pub), "https://del.example.com")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	m := kernel.ActionManifest{
		ActionID: "del-action-1", OwnerHandle: "@del-peer", Name: "dact",
		Kind: kernel.KindHTTP, Price: 0, Description: "d",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	msig, _ := kernel.SignManifest(priv, &m)
	m.Signature = msig
	result, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	a := result.Created[0]
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	pubFed := true
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pubFed}); err != nil {
		t.Fatal(err)
	}

	caller := setupUser(t, st, "@del-caller", 0)
	p := setupProcess(t, st, caller.ID, 0)

	remoteReceipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: "rs",
		TxID: "del-tx-1", TraceID: "t1", ActionID: m.ActionID,
		CallerUserID: "c1", ProcessID: "p1",
		ArgsHash:  jcsHashForTest(t, `{}`),
		ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxSuccess, Gross: 0, Net: 0, Fee: 0,
		StartedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	remoteReceipt.Signature = signReceiptForTest(t, priv, remoteReceipt)
	receiptBytes, _ := json.Marshal(remoteReceipt)
	fake.receiptJSON = string(receiptBytes)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ProcessID: p.ID, IsRootCall: true,
		TargetUserID: "@del-peer", ActionName: "dact", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	_ = reply

	// Soft-delete the proxy action.
	if err := k.DeleteAction(ctx, sys.ID, a.ID); err != nil {
		t.Fatalf("DeleteAction: %v", err)
	}

	// VerifyRemoteReceipt must still work after deletion.
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	if len(txs) == 0 {
		t.Fatal("no transactions found")
	}
	v, err := k.VerifyRemoteReceipt(ctx, caller.ID, txs[0].ID)
	if err != nil {
		t.Fatalf("VerifyRemoteReceipt after deletion: %v", err)
	}
	if !v.Checks.ReceiptHash {
		t.Error("expected ReceiptHash check=true after proxy deletion")
	}
	if !v.Checks.Signature {
		t.Error("expected Signature check=true after proxy deletion")
	}
	if !v.Checks.ActionID {
		t.Error("expected ActionID check=true after proxy deletion")
	}
	if !v.Valid {
		t.Error("expected Valid=true after proxy deletion")
	}
}

func TestCreateSignedRejectionReceipt(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	// Register a peer so we have a counterpartyID.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := k.AddPeer(ctx, sys.ID, "@rejection-peer", pubB64, "https://rejection.example.com")
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	r, err := k.CreateSignedRejectionReceipt(peer.ID, "@owner/some-action", "argsHash123", "idem-key-456")
	if err != nil {
		t.Fatalf("CreateSignedRejectionReceipt: %v", err)
	}

	if r.Status != kernel.TxFailure {
		t.Errorf("expected status failure, got %s", r.Status)
	}
	if r.Gross != 0 || r.Net != 0 || r.Fee != 0 {
		t.Errorf("expected zero charge, got gross=%d net=%d fee=%d", r.Gross, r.Net, r.Fee)
	}
	if r.Reason != "denied" {
		t.Errorf("expected reason=denied, got %q", r.Reason)
	}
	if r.Signature == "" {
		t.Error("rejection receipt must be signed")
	}
	if r.CallerUserID != peer.ID {
		t.Errorf("expected CallerUserID=%s, got %s", peer.ID, r.CallerUserID)
	}
}

func TestDenyPeerDeactivatesActionsAndCancelsSteps(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	// Register peer B.
	_, privB, _ := ed25519.GenerateKey(rand.Reader)
	pubB := privB.Public().(ed25519.PublicKey)
	pubBB64 := base64.RawURLEncoding.EncodeToString(pubB)
	peerB, err := k.AddPeer(ctx, sys.ID, "@deny-peer-b", pubBB64, "https://deny-b.example.com")
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	// Create an action owned by B's proxy user (simulates B sharing an action with A).
	act := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: peerB.ID, Name: "b-act",
		Kind: kernel.KindRemoteProxy, Active: true, Price: 0,
		Source: "https://deny-b.example.com/v1/federation/call?action=@b/b-act&counterparty=x",
		InputSchema:  map[string]any{}, OutputSchema: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, act); err != nil {
		t.Fatalf("create action: %v", err)
	}
	// Verify it starts active.
	before, _ := k.ReadAction(ctx, act.ID)
	if !before.Active {
		t.Fatal("action should be active before deny")
	}

	// DenyPeer should deactivate owned actions.
	if err := k.DenyPeer(ctx, sys.ID, "@deny-peer-b"); err != nil {
		t.Fatalf("DenyPeer: %v", err)
	}
	after, _ := k.ReadAction(ctx, act.ID)
	if after.Active {
		t.Error("action should be inactive after DenyPeer")
	}
	peerBRead, _ := k.ReadUser(ctx, peerB.ID)
	if peerBRead.DeniedAt == nil {
		t.Error("peer B should have denied_at set")
	}

	// UndenyPeer should reactivate owned actions.
	if err := k.UndenyPeer(ctx, sys.ID, "@deny-peer-b"); err != nil {
		t.Fatalf("UndenyPeer: %v", err)
	}
	reactivated, _ := k.ReadAction(ctx, act.ID)
	if !reactivated.Active {
		t.Error("action should be active again after UndenyPeer")
	}
	peerBRead, _ = k.ReadUser(ctx, peerB.ID)
	if peerBRead.DeniedAt != nil {
		t.Error("peer B should have denied_at cleared after UndenyPeer")
	}
}

func TestGetGossipOnlyIncludesTransactedFriends(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	// Register two peers: one transacted, one not.
	_, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubA := privA.Public().(ed25519.PublicKey)
	pubAB64 := base64.RawURLEncoding.EncodeToString(pubA)
	peerA, err := k.AddPeer(ctx, sys.ID, "@gossip-transacted", pubAB64, "https://gossip-a.example.com")
	if err != nil {
		t.Fatalf("AddPeer A: %v", err)
	}
	_, privB, _ := ed25519.GenerateKey(rand.Reader)
	pubB := privB.Public().(ed25519.PublicKey)
	pubBB64 := base64.RawURLEncoding.EncodeToString(pubB)
	_, err = k.AddPeer(ctx, sys.ID, "@gossip-untransacted", pubBB64, "https://gossip-b.example.com")
	if err != nil {
		t.Fatalf("AddPeer B: %v", err)
	}

	// Create a proxy action owned by peer A with uses > 0.
	actA := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: peerA.ID, Name: "a-act",
		Kind: kernel.KindRemoteProxy, Active: true, Price: 10,
		Source: "https://gossip-a.example.com/v1/federation/call?action=@gossip-transacted/a-act&counterparty=x",
		InputSchema:  map[string]any{}, OutputSchema: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, actA); err != nil {
		t.Fatalf("create actA: %v", err)
	}
	now := time.Now().UTC()
	if err := st.UpsertStats(ctx, &kernel.Stats{
		ActionID: actA.ID, Uses: 2, Successes: 2, LastUsedAt: now,
	}); err != nil {
		t.Fatalf("UpsertStats: %v", err)
	}

	gossip, err := k.GetGossip(ctx)
	if err != nil {
		t.Fatalf("GetGossip: %v", err)
	}

	// Only peer A (transacted) should appear in Friends.
	if len(gossip.Friends) != 1 {
		t.Fatalf("expected 1 transacted friend, got %d", len(gossip.Friends))
	}
	if gossip.Friends[0].Handle != "@gossip-transacted" {
		t.Errorf("expected @gossip-transacted, got %s", gossip.Friends[0].Handle)
	}
	if len(gossip.Friends[0].Actions) != 1 {
		t.Fatalf("expected 1 action in friend view, got %d", len(gossip.Friends[0].Actions))
	}
	if gossip.Friends[0].Actions[0].Uses != 2 {
		t.Errorf("expected Uses=2, got %d", gossip.Friends[0].Actions[0].Uses)
	}
}
