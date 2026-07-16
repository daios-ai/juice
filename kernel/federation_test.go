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
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
)

func TestRegisterRemoteKernelValidatesIdentity(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	if _, err := k.AddPeer(ctx, sys.ID, "@bad/handle", "not-base64url"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for handle containing /, got %v", err)
	}

	if _, err := k.AddPeer(ctx, sys.ID, "@bad-key", "not-base64url"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for malformed public key, got %v", err)
	}

	shortKey := base64.RawURLEncoding.EncodeToString([]byte("short"))
	if _, err := k.AddPeer(ctx, sys.ID, "@short-key", shortKey); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for short public key, got %v", err)
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	validKey := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.AddPeer(ctx, sys.ID, "@remote", validKey); err != nil {
		t.Fatalf("valid remote kernel should register: %v", err)
	}
}

func TestCreateOrUpdateProxyPeerHandleConflict(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	pub3, _, _ := ed25519.GenerateKey(rand.Reader)
	key1 := base64.RawURLEncoding.EncodeToString(pub1)
	key2 := base64.RawURLEncoding.EncodeToString(pub2)
	key3 := base64.RawURLEncoding.EncodeToString(pub3)

	u1, err := k.CreateOrUpdateProxyPeer(ctx, "@remote", key1)
	if err != nil {
		t.Fatalf("first peer: %v", err)
	}
	if u1.Handle != "@remote" {
		t.Fatalf("want @remote, got %s", u1.Handle)
	}

	u2, err := k.CreateOrUpdateProxyPeer(ctx, "@remote", key2)
	if err != nil {
		t.Fatalf("second peer: %v", err)
	}
	if u2.Handle != "@remote-2" {
		t.Fatalf("want @remote-2, got %s", u2.Handle)
	}

	u3, err := k.CreateOrUpdateProxyPeer(ctx, "@remote", key3)
	if err != nil {
		t.Fatalf("third peer: %v", err)
	}
	if u3.Handle != "@remote-3" {
		t.Fatalf("want @remote-3, got %s", u3.Handle)
	}
}

// A friend and its reciprocal both create the same proxy user at once; every concurrent
// caller must succeed idempotently, never hit a unique-key conflict.
func TestCreateOrUpdateProxyPeerConcurrent(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	setupSys(t, k, st)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)

	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = k.CreateOrUpdateProxyPeer(context.Background(), "@remote", key)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent create must be idempotent: %v", err)
		}
	}
}

func TestCreateOrUpdateProxyPeerNormalizesHandle(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)

	// Peer self-reports a handle without "@": the local alias is canonicalized.
	u, err := k.CreateOrUpdateProxyPeer(ctx, "peerless", key)
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	if u.Handle != "@peerless" {
		t.Fatalf("want @peerless, got %s", u.Handle)
	}
	got, err := k.ReadUserByHandle(ctx, "peerless")
	if err != nil || got == nil || got.ID != u.ID {
		t.Errorf("ReadUserByHandle(peerless): got %v err %v, want id %s", got, err, u.ID)
	}
}

// ---- Remote proxy / manifest tests ----

func TestImportRemoteActionCreatesRemoteProxy(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@remote-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	// proxyPrice = mp + ceil(mp*ImportBPS/10000) = 50 + ceil(50*500/10000) = 50+3 = 53
	if a.Price != 53 {
		t.Errorf("price: got %d, want 53 (proxyPrice = mp + import duty)", a.Price)
	}
}

func TestImportRemoteActionReimp(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@reimp-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	// proxyPrice = mp + ceil(mp*ImportBPS/10000) = 99 + ceil(99*500/10000) = 99+5 = 104
	if second.Price != 104 {
		t.Errorf("reimport price: got %d, want 104 (proxyPrice = mp + import duty)", second.Price)
	}
}

func TestImportRemoteActionUnchangedPreservesActiveAndStats(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@stable-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@idem-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@bad-sig-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@neg-price-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	remoteUser, err := k.AddPeer(ctx, sys.ID, "@mrf-peer", base64.RawURLEncoding.EncodeToString(pub))
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

// TestLocalActionNotExported: a local-visibility action is never served as a manifest nor gossiped
// (§13); only public actions cross the kernel boundary. This keeps friendship non-transitive and is
// how an imported proxy (set local) stays unreachable by peers.
func TestLocalActionNotExported(t *testing.T) {
	st := newTestStore(t)
	su := setupUser(t, st, "@sys", 0)
	k := newTestKernel(st)
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	owner := setupUser(t, st, "@local-owner", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "localonly",
		Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityLocal,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Source: "https://example.com/call", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	if _, err := k.GetActionManifest(ctx, a.ID); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("local action manifest: want ErrUnauthorized, got %v", err)
	}
	g, err := k.GetGossip(ctx, "")
	if err != nil {
		t.Fatalf("GetGossip: %v", err)
	}
	for _, ga := range g.Actions {
		if ga.ActionID == a.ID {
			t.Error("local action must not appear in gossip")
		}
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
		Visibility:   kernel.VisibilityPublic,
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

	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "remote-tx-1",
		// action_id, args_hash, and reply_hash must match the request: settlement now enforces them
		// (the call below uses remote action "proxy-action-1" with args {} and reply {}).
		ActionID: "proxy-action-1", ArgsHash: jcsHashForTest(t, `{}`),
		ReplyHash: jcsHashForTest(t, `{}`),
		Status:    kernel.TxSuccess, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	receiptBytes, _ := json.Marshal(r)
	fakeReceiptJSON := string(receiptBytes)
	fake := &fakeFederationHTTP{receiptJSON: fakeReceiptJSON}
	k := newTestKernelWithHTTP(st, fake)

	remoteUser, err := k.AddPeer(ctx, sys.ID, "@proxy-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	pubFed := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubFed}); err != nil {
		t.Fatalf("UpdateAction public: %v", err)
	}

	caller := setupUser(t, st, "@proxy-caller", 0)
	p, tr := beginTestRun(t, st, caller.ID, a)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID:        caller.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    "@proxy-peer",
		ActionName:      "proxy-peer/add",
		Args:            map[string]any{},
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

// ---- #2/S2: settlement preconditions (action_id + args_hash) ----

// setupSettleProxy creates an active+public remote proxy action and a funded caller with a
// pre-funded root trace, returning everything needed to drive a remote settlement through Call.
// The fake's receiptJSON is left empty for the caller to set.
func setupSettleProxy(t *testing.T, st kernel.Store, fake *fakeFederationHTTP, priv ed25519.PrivateKey, pub ed25519.PublicKey, remoteActionID string, proxyPrice int64) (*kernel.Kernel, *kernel.Action, *kernel.User) {
	t.Helper()
	return setupSettleProxyWithKernel(t, st, newTestKernelWithHTTP(st, fake), priv, pub, remoteActionID, proxyPrice)
}

// setupSettleProxyWithKernel is setupSettleProxy against a caller-supplied kernel, so a test
// can configure the kernel (e.g. RemotePendingMaxAge) before importing the proxy.
func setupSettleProxyWithKernel(t *testing.T, st kernel.Store, k *kernel.Kernel, priv ed25519.PrivateKey, pub ed25519.PublicKey, remoteActionID string, proxyPrice int64) (*kernel.Kernel, *kernel.Action, *kernel.User) {
	t.Helper()
	ctx := context.Background()
	sys := setupSys(t, nil, st)

	remoteUser, err := k.AddPeer(ctx, sys.ID, "@settle-peer", base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	m := kernel.ActionManifest{
		ActionID: remoteActionID, OwnerHandle: "@settle-peer", Name: "settleact",
		Kind: kernel.KindHTTP, Price: proxyPrice, Description: "s",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	m.Signature, _ = kernel.SignManifest(priv, &m)
	result, err := k.ImportRemoteAction(ctx, sys.ID, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportRemoteAction: %v", err)
	}
	a := result.Created[0]
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	pubFed := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubFed}); err != nil {
		t.Fatal(err)
	}
	// Fund the caller with exactly the proxy price so a full refund restores the original balance.
	caller := setupUser(t, st, "@settle-caller", a.Price)
	return k, a, caller
}

// TestRetryExpiredRemoteTraceSettlesAsFailure (#5): past RemotePendingMaxAge a never-settled
// remote-proxy call is settled as a failure with full refund, not retried forever (§13).
func TestRetryExpiredRemoteTraceSettlesAsFailure(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	// Empty receiptJSON → ExecuteFederation always reports "pending" (genuine silence).
	fake := &fakeFederationHTTP{}
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	cfg.RemotePendingMaxAge = time.Nanosecond // any pending trace is immediately past the bound
	k := kernel.New(st, nil, fake, nil, cfg, log.Default())

	_, _, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "exp-action", 1000)
	before, _ := st.ReadUser(ctx, caller.ID)

	// Real root run: the empty receipt makes the proxy call time out; the process stays open and
	// the trace persists in the DB with its idempotency key (beginRun records the dispatch).
	if _, err := k.Run(ctx, caller.ID, "@settle-peer/settle-peer/settleact", map[string]any{}); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("Run: expected ErrTimeout, got %v", err)
	}
	if pend, _ := st.ListPendingRemoteTraces(ctx); len(pend) != 1 {
		t.Fatalf("expected 1 pending remote trace after timeout, got %d", len(pend))
	}

	// The periodic retrier runs. The trace is already past the 1ns window → terminal failure.
	k.RetryPendingRemoteDispatches(ctx)

	if pend, _ := st.ListPendingRemoteTraces(ctx); len(pend) != 0 {
		t.Fatalf("expected 0 pending traces after expiry settlement, got %d", len(pend))
	}
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{})
	var failures int
	for _, tx := range txs {
		if tx.Status == kernel.TxFailure {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("expected exactly 1 failure transaction for the expired call, got %d", failures)
	}
	// Full refund: the root failure auto-closes the process and returns the caller's funds.
	after, _ := st.ReadUser(ctx, caller.ID)
	if after.Available != before.Available {
		t.Errorf("expected full refund to %d, got %d", before.Available, after.Available)
	}
	if after.Locked != 0 {
		t.Errorf("expected 0 locked after refund, got %d", after.Locked)
	}
}

// TestRetryPendingRemoteTraceSettlesWhenPeerReturns: a call parked because the peer was offline
// settles as success once RetryPendingRemoteDispatches runs and the peer answers with a valid
// receipt — no restart, no interrupted refund. This is the value the serve retry loop delivers (§13).
func TestRetryPendingRemoteTraceSettlesWhenPeerReturns(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bps := kernel.DefaultConfig().ImportBPS

	// Empty receiptJSON → the peer is "offline": ExecuteFederation returns no receipt → pending.
	fake := &fakeFederationHTTP{}
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	// Default RemotePendingMaxAge (24h): the trace stays pending, not force-expired.
	k := kernel.New(st, nil, fake, nil, cfg, log.Default())

	_, a, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "ret-action", 1000)
	mp := a.Price * 10000 / (10000 + bps)

	// Call while the peer is offline → pending, no settled transaction, funds locked.
	if _, err := k.Run(ctx, caller.ID, "@settle-peer/settle-peer/settleact", map[string]any{}); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("Run: expected ErrTimeout (pending), got %v", err)
	}
	if pend, _ := st.ListPendingRemoteTraces(ctx); len(pend) != 1 {
		t.Fatalf("expected 1 pending remote trace, got %d", len(pend))
	}
	if txs, _ := st.ListTransactions(ctx, kernel.TxFilter{}); len(txs) != 0 {
		t.Fatalf("expected no settled transaction while pending, got %d", len(txs))
	}

	// The peer returns: it now answers with a valid signed success receipt.
	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx", ActionID: "ret-action",
		ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxSuccess, Charge: mp, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)

	// The retry loop's action settles the parked call — no restart involved.
	k.RetryPendingRemoteDispatches(ctx)

	if pend, _ := st.ListPendingRemoteTraces(ctx); len(pend) != 0 {
		t.Fatalf("expected 0 pending traces after retry settled, got %d", len(pend))
	}
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{})
	if len(txs) != 1 || txs[0].Status != kernel.TxSuccess {
		t.Fatalf("expected 1 success transaction after retry, got %+v", txs)
	}
	if txs[0].Net != mp {
		t.Errorf("net: got %d, want %d", txs[0].Net, mp)
	}
	// Caller was funded exactly the proxy price (mp+duty); a success spends it all and closes the process.
	after, _ := st.ReadUser(ctx, caller.ID)
	if after.Locked != 0 {
		t.Errorf("expected 0 locked after settlement, got %d", after.Locked)
	}
}

// TestAwaitingReceiptSince: a process holding a remote call parked for its receipt is reported by
// AwaitingReceiptSince (with the call's start time); once it settles, it drops out (§13).
func TestAwaitingReceiptSince(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	fake := &fakeFederationHTTP{} // offline: no receipt → pending
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(st, nil, fake, nil, cfg, log.Default())

	_, _, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "await-action", 1000)

	// Offline call → parked, awaiting a receipt.
	if _, err := k.Run(ctx, caller.ID, "@settle-peer/settle-peer/settleact", map[string]any{}); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("Run: expected ErrTimeout, got %v", err)
	}
	pend, _ := st.ListPendingRemoteTraces(ctx)
	if len(pend) != 1 {
		t.Fatalf("expected 1 pending trace, got %d", len(pend))
	}
	awaitingPID := pend[0].ProcessID

	// A process with no pending remote call is not reported.
	if since, _ := k.AwaitingReceiptSince(ctx, []string{"no-such-process"}); len(since) != 0 {
		t.Fatalf("expected no awaiting processes for an unrelated id, got %d", len(since))
	}

	since, err := k.AwaitingReceiptSince(ctx, []string{awaitingPID})
	if err != nil {
		t.Fatal(err)
	}
	ts, ok := since[awaitingPID]
	if !ok {
		t.Fatalf("process %s not reported awaiting", pend[0].ProcessID)
	}
	if !ts.Equal(pend[0].CreatedAt) {
		t.Errorf("awaiting-since = %v, want the pending trace's created_at %v", ts, pend[0].CreatedAt)
	}
}

// TestPendingRemoteTracesAndRetryWrappers: the serve-loop-facing wrappers behave like the bulk
// method — PendingRemoteTraces lists the parked call, and RetryRemoteTrace settles it once the peer
// answers with a valid receipt.
func TestPendingRemoteTracesAndRetryWrappers(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bps := kernel.DefaultConfig().ImportBPS

	fake := &fakeFederationHTTP{} // offline: no receipt → pending
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(st, nil, fake, nil, cfg, log.Default())

	_, a, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "wrap-action", 1000)
	mp := a.Price * 10000 / (10000 + bps)

	if _, err := k.Run(ctx, caller.ID, "@settle-peer/settle-peer/settleact", map[string]any{}); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("Run: expected ErrTimeout, got %v", err)
	}
	pending, err := k.PendingRemoteTraces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("PendingRemoteTraces: expected 1, got %d", len(pending))
	}

	// Peer returns with a valid receipt; RetryRemoteTrace settles the one trace.
	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx", ActionID: "wrap-action",
		ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxSuccess, Charge: mp, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)

	if err := k.RetryRemoteTrace(ctx, pending[0]); err != nil {
		t.Fatalf("RetryRemoteTrace: %v", err)
	}
	if p, _ := k.PendingRemoteTraces(ctx); len(p) != 0 {
		t.Fatalf("expected 0 pending after retry settled, got %d", len(p))
	}
}

func TestSettleRemoteCallRejectsWrongActionID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "settle-action-1", 0)
	_, tr := beginTestRun(t, st, caller.ID, a)

	// Receipt is validly signed but carries the WRONG action_id.
	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx", ActionID: "some-other-action",
		ArgsHash: jcsHashForTest(t, `{}`), Status: kernel.TxSuccess, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: "@settle-peer", ActionName: "settle-peer/settleact", Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("expected ErrTimeout on action_id mismatch, got %v", err)
	}
	// No settled transaction must exist: the trace stays open for retry.
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{})
	if len(txs) != 0 {
		t.Fatalf("expected no settled transaction, got %d", len(txs))
	}
}

func TestSettleRemoteCallRejectsWrongArgsHash(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "settle-action-2", 0)
	_, tr := beginTestRun(t, st, caller.ID, a)

	// Receipt is validly signed with the right action_id but a MISMATCHED args_hash.
	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx", ActionID: "settle-action-2",
		ArgsHash: jcsHashForTest(t, `{"tampered":true}`), Status: kernel.TxSuccess, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)

	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: "@settle-peer", ActionName: "settle-peer/settleact", Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("expected ErrTimeout on args_hash mismatch, got %v", err)
	}
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{})
	if len(txs) != 0 {
		t.Fatalf("expected no settled transaction, got %d", len(txs))
	}
}

// ---- Finding 1: validly-signed but economically-invalid receipts are quarantined ----

// A receipt whose signature/action_id/args_hash are correct but whose economics breach §13
// (charge out of range, success≠mp, reply_hash mismatch, unknown status) must NOT be clamped and
// committed as if valid — it settles terminally as a failure (charge 0, full refund) and is not
// left open for retry. This is the fix for the clamp-and-commit defect.
func TestSettleRemoteCallQuarantinesInvalidReceipt(t *testing.T) {
	cases := []struct {
		name   string
		status kernel.TxStatus
		charge func(mp int64) int64
		reply  string // reply_hash source JSON; "" means the correct reply ({})
	}{
		{"success_charge_over_mp", kernel.TxSuccess, func(mp int64) int64 { return mp + 1 }, ""},
		{"failure_charge_over_mp", kernel.TxFailure, func(mp int64) int64 { return mp + 1 }, ""},
		{"failure_charge_negative", kernel.TxFailure, func(int64) int64 { return -1 }, ""},
		{"success_reply_hash_mismatch", kernel.TxSuccess, func(mp int64) int64 { return mp }, `{"tampered":true}`},
		{"unknown_status", kernel.TxStatus("weird"), func(int64) int64 { return 0 }, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			fake := &fakeFederationHTTP{}
			k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "q-action", 1000)
			_, tr := beginTestRun(t, st, caller.ID, a)
			// mp is the remote manifest price; a.Price (= q) funds the caller and is fully refunded.
			mp := a.Price * 10000 / (10000 + kernel.DefaultConfig().ImportBPS)

			replyHash := jcsHashForTest(t, `{}`)
			if tc.reply != "" {
				replyHash = jcsHashForTest(t, tc.reply)
			}
			now := time.Now().UTC()
			r := &kernel.Receipt{
				ID: uuid.New().String(), TxID: "rtx", ActionID: "q-action",
				ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: replyHash,
				Status: tc.status, Charge: tc.charge(mp), StartedAt: now, CreatedAt: now,
			}
			r.Signature = signReceiptForTest(t, priv, r)
			b, _ := json.Marshal(r)
			fake.receiptJSON = string(b)

			_, err := k.Call(ctx, kernel.CallRequest{
				CallerID: caller.ID, ExistingTraceID: tr.ID,
				TargetUserID: "@settle-peer", ActionName: "settle-peer/settleact", Args: map[string]any{},
			})
			// Terminal failure, not ErrTimeout (no retry) and not a silent success.
			if !errors.Is(err, kernel.ErrExecutionFailed) {
				t.Fatalf("expected ErrExecutionFailed, got %v", err)
			}
			txs, _ := st.ListTransactions(ctx, kernel.TxFilter{})
			if len(txs) != 1 {
				t.Fatalf("expected 1 committed transaction, got %d", len(txs))
			}
			if tx := txs[0]; tx.Status != kernel.TxFailure || tx.Net != 0 || tx.Fee != 0 {
				t.Fatalf("quarantine must commit a zero-charge failure: status=%s net=%d fee=%d", tx.Status, tx.Net, tx.Fee)
			}
			// Caller fully refunded: original balance restored, nothing left locked.
			assertUserBalance(t, st, caller.ID, a.Price, 0)
			// Settled, therefore excluded from the retry set — no livelock.
			if pending, _ := st.ListPendingRemoteTraces(ctx); len(pending) != 0 {
				t.Errorf("expected no pending remote traces, got %d", len(pending))
			}
		})
	}
}

// A valid receipt settles with its charge intact (no clamp), and VerifyRemoteReceipt — now a pure
// confirmation of invariants already enforced at commit time — reports valid.
func TestSettleRemoteCallValidChargeNotClamped(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	bps := kernel.DefaultConfig().ImportBPS
	k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "valid-action", 1000)
	_, tr := beginTestRun(t, st, caller.ID, a)
	mp := a.Price * 10000 / (10000 + bps)

	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx", ActionID: "valid-action",
		ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxSuccess, Charge: mp, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)

	if _, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: "@settle-peer", ActionName: "settle-peer/settleact", Args: map[string]any{},
	}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{})
	if len(txs) != 1 {
		t.Fatalf("expected 1 transaction, got %d", len(txs))
	}
	tx := txs[0]
	if tx.Status != kernel.TxSuccess {
		t.Fatalf("status: got %s, want success", tx.Status)
	}
	if tx.Net != mp {
		t.Errorf("net: got %d, want %d (mp, unclamped)", tx.Net, mp)
	}
	wantDuty := (mp*bps + 9999) / 10000 // ceilDiv(mp*bps, 10000)
	if tx.Fee != wantDuty {
		t.Errorf("duty: got %d, want %d", tx.Fee, wantDuty)
	}
	v, err := k.VerifyRemoteReceipt(ctx, caller.ID, tx.ID)
	if err != nil {
		t.Fatalf("VerifyRemoteReceipt: %v", err)
	}
	if !v.Valid {
		t.Errorf("expected valid receipt, got checks %+v", v.Checks)
	}
}

// ---- D1: proxy action source is the remote action ref ----

func TestImportRemoteActionSourceIsActionRef(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	peer, err := k.AddPeer(ctx, sys.ID, "@ref-peer", pubB64)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	m := kernel.ActionManifest{
		ActionID: "ref-action-1", OwnerHandle: "@ref-peer", Name: "act",
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
	// Source is the remote action ref (@owner/name) — never a URL; the peer is resolved by key.
	if a.Source != "@ref-peer/act" {
		t.Errorf("source: want @ref-peer/act, got %q", a.Source)
	}
	if a.RemoteActionID != "ref-action-1" {
		t.Errorf("remote_action_id: want ref-action-1, got %q", a.RemoteActionID)
	}
}

// ---- D5: duplicate identity guard ----

func TestRegisterRemoteKernelResolvesDuplicateHandle(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	pub1B64 := base64.RawURLEncoding.EncodeToString(pub1)
	pub2B64 := base64.RawURLEncoding.EncodeToString(pub2)

	u1, err := k.AddPeer(ctx, sys.ID, "@dup-handle", pub1B64)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if u1.Handle != "@dup-handle" {
		t.Fatalf("want @dup-handle, got %s", u1.Handle)
	}
	// Same preferred handle, different public key → auto-resolved to suffix.
	u2, err := k.AddPeer(ctx, sys.ID, "@dup-handle", pub2B64)
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if u2.Handle != "@dup-handle-2" {
		t.Errorf("want @dup-handle-2, got %s", u2.Handle)
	}
}

// ---- D4: VerifyRemoteReceipt ----

// TestRemoteImportOwnerQualifiedNoCollision: two owners on peer B with the same action name both
// import under the one proxy user, owner-qualified (alice/greet, bob/greet), with no collision — and
// each resolves via mount descent (@B.alice/greet → account @B, action "alice/greet").
func TestRemoteImportOwnerQualifiedNoCollision(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sys := setupSys(t, nil, st)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	k := newTestKernelWithHTTP(st, &fakeFederationHTTP{})

	peer, err := k.AddPeer(ctx, sys.ID, "@B", base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	imp := func(owner, actionID string) {
		m := kernel.ActionManifest{
			ActionID: actionID, OwnerHandle: owner, Name: "greet",
			Kind: kernel.KindHTTP, Price: 0, Description: "g",
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
		}
		m.Signature, _ = kernel.SignManifest(priv, &m)
		if _, err := k.ImportRemoteAction(ctx, sys.ID, peer.ID, m); err != nil {
			t.Fatalf("import %s: %v", owner, err)
		}
	}
	imp("@alice", "act-alice")
	imp("@bob", "act-bob") // same Name "greet", different owner — must NOT collide

	// Addressed @B/<owner>/<name>: parses to owner @B and name "<owner>/greet", so alice's and bob's
	// same-named actions are distinct rows and both resolve under the one @B mount.
	for _, tc := range []struct{ addr, wantName string }{
		{"@B/alice/greet", "alice/greet"},
		{"@B/bob/greet", "bob/greet"},
	} {
		oh, an, err := kernel.ParseActionRef(tc.addr)
		if err != nil {
			t.Fatalf("%s: %v", tc.addr, err)
		}
		owner, err := k.ReadUserByHandle(ctx, oh)
		if err != nil || owner.ID != peer.ID || an != tc.wantName {
			t.Errorf("%s resolved to owner=%v name=%q, want the @B mount and %q", tc.addr, oh, an, tc.wantName)
		}
		if _, err := k.ReadActionByOwnerName(ctx, owner.ID, an); err != nil {
			t.Errorf("%s: action %q not found under @B: %v", tc.addr, an, err)
		}
	}
}

func TestVerifyRemoteReceiptValid(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sys := setupSys(t, nil, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	k := newTestKernelWithHTTP(st, fake)

	remoteUser, err := k.AddPeer(ctx, sys.ID, "@verify-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	pubFed2 := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubFed2}); err != nil {
		t.Fatal(err)
	}

	caller := setupUser(t, st, "@verify-caller", 0)
	p, tr := beginTestRun(t, st, caller.ID, a)

	// Build a receipt whose fields match what Call() will record in the transaction.
	// ActionID must be the remote action's ID (manifest ActionID), not the local proxy ID.
	remoteReceipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: "remote-sys",
		TxID: "remote-tx-verify", TraceID: "t1", ActionID: m.ActionID,
		CallerUserID: "c1", ProcessID: "p1",
		ArgsHash:  jcsHashForTest(t, `{}`),
		ReplyHash: jcsHashForTest(t, `{}`),
		Status:    kernel.TxSuccess, Gross: 0, Net: 0, Fee: 0,
		StartedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	// Sign with the remote peer's private key using the same method as the kernel.
	receiptSig := signReceiptForTest(t, priv, remoteReceipt)
	remoteReceipt.Signature = receiptSig
	receiptBytes, _ := json.Marshal(remoteReceipt)
	fake.receiptJSON = string(receiptBytes)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: "@verify-peer", ActionName: "verify-peer/vact", Args: map[string]any{},
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
	pubFed3 := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubFed3}); err != nil {
		t.Fatal(err)
	}

	p, tr := beginTestRun(t, st, caller.ID, a)
	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
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

	remoteUser, _ := k.AddPeer(ctx, sys.ID, "@tamper-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	pubFed4 := kernel.VisibilityPublic
	_, _ = k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubFed4})

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
	p, tr := beginTestRun(t, st, caller.ID, a)
	// A receipt signed with the wrong key must be rejected: no settlement, trace stays open.
	_, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: "@tamper-peer", ActionName: "tamper-peer/tact", Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("expected ErrTimeout for invalid signature, got %v", err)
	}
	// No transaction should be stored: the bad receipt must never settle.
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{ProcessID: p.ID})
	if len(txs) != 0 {
		t.Errorf("expected no transactions after invalid-signature rejection, got %d", len(txs))
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

	remoteUser, err := k.AddPeer(ctx, sys.ID, "@del-peer", base64.RawURLEncoding.EncodeToString(pub))
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
	pubFed := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubFed}); err != nil {
		t.Fatal(err)
	}

	caller := setupUser(t, st, "@del-caller", 0)
	p, tr := beginTestRun(t, st, caller.ID, a)

	remoteReceipt := &kernel.Receipt{
		ID: uuid.New().String(), IssuerUserID: "rs",
		TxID: "del-tx-1", TraceID: "t1", ActionID: m.ActionID,
		CallerUserID: "c1", ProcessID: "p1",
		ArgsHash:  jcsHashForTest(t, `{}`),
		ReplyHash: jcsHashForTest(t, `{}`),
		Status:    kernel.TxSuccess, Gross: 0, Net: 0, Fee: 0,
		StartedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	remoteReceipt.Signature = signReceiptForTest(t, priv, remoteReceipt)
	receiptBytes, _ := json.Marshal(remoteReceipt)
	fake.receiptJSON = string(receiptBytes)

	reply, err := k.Call(ctx, kernel.CallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: "@del-peer", ActionName: "del-peer/dact", Args: map[string]any{},
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
	peer, err := k.AddPeer(ctx, sys.ID, "@rejection-peer", pubB64)
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	r, err := k.CreateSignedRejectionReceipt(peer.ID, "@owner/some-action", "argsHash123", "idem-key-456", "action inactive")
	if err != nil {
		t.Fatalf("CreateSignedRejectionReceipt: %v", err)
	}

	if r.Status != kernel.TxFailure {
		t.Errorf("expected status failure, got %s", r.Status)
	}
	if r.Gross != 0 || r.Net != 0 || r.Fee != 0 {
		t.Errorf("expected zero charge, got gross=%d net=%d fee=%d", r.Gross, r.Net, r.Fee)
	}
	if r.Reason != "action inactive" {
		t.Errorf("expected the supplied reason, got %q", r.Reason)
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
	peerB, err := k.AddPeer(ctx, sys.ID, "@deny-peer-b", pubBB64)
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	// Create an action owned by B's proxy user (simulates B sharing an action with A).
	act := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: peerB.ID, Name: "b-act",
		Kind: kernel.KindRemoteProxy, Active: true, Price: 0,
		Source:      "https://deny-b.example.com/v1/federation/call?action=@b/b-act&counterparty=x",
		InputSchema: map[string]any{}, OutputSchema: map[string]any{},
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

	// UndenyPeer clears the denial but does NOT reactivate proxy actions —
	// proxies are re-enabled explicitly via remote import.
	if err := k.UndenyPeer(ctx, sys.ID, "@deny-peer-b"); err != nil {
		t.Fatalf("UndenyPeer: %v", err)
	}
	stillInactive, _ := k.ReadAction(ctx, act.ID)
	if stillInactive.Active {
		t.Error("action should remain inactive after UndenyPeer (must re-import to reactivate)")
	}
	peerBRead, _ = k.ReadUser(ctx, peerB.ID)
	if peerBRead.DeniedAt != nil {
		t.Error("peer B should have denied_at cleared after UndenyPeer")
	}
}

// TestPurgeIdlePeers (§13 Retention): a peer idle past PeerRetention at zero balance is purged —
// its proxy actions, stats, and discovered_kernels rows deleted and its identity forgotten — while
// the anchor user row survives. PeerRetention <= 0 disables the sweep.
func TestPurgeIdlePeers(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	baseCfg := func() kernel.Config {
		cfg := kernel.DefaultConfig()
		cfg.TokenSecret = "test-secret"
		cfg.IssuerUserID = testIssuerUserID
		cfg.FeeRecipientID = testIssuerUserID
		cfg.SigningKey = testSigningKey()
		return cfg
	}

	kDisabled := kernel.New(st, nil, nil, nil, baseCfg(), log.Default())
	sys := setupSys(t, kDisabled, st)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := kDisabled.AddPeer(ctx, sys.ID, "@old-peer", pubB64)
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	act := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: peer.ID, Name: "p-act",
		Kind: kernel.KindRemoteProxy, Active: true, Price: 0,
		Source:      "https://old-peer.example.com/call",
		InputSchema: map[string]any{}, OutputSchema: map[string]any{},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, act); err != nil {
		t.Fatalf("create action: %v", err)
	}
	if err := st.UpsertStats(ctx, &kernel.Stats{ActionID: act.ID, Uses: 3, Successes: 3, LastUsedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOrUpdateDiscoveredKernel(ctx, &kernel.DiscoveredKernel{PublicKey: pubB64, IntroducedBy: "x", Handle: "@old-peer", StatsJSON: json.RawMessage("{}"), FirstSeen: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	// Disabled by default (PeerRetention == 0): a no-op that touches nothing.
	if n, err := kDisabled.PurgeIdlePeers(ctx); err != nil || n != 0 {
		t.Fatalf("disabled purge: n=%d err=%v, want 0/nil", n, err)
	}
	if _, err := kDisabled.ReadAction(ctx, act.ID); err != nil {
		t.Fatalf("action must survive while purge disabled: %v", err)
	}

	// Enabled with a tiny retention so the just-created peer is immediately idle.
	cfg := baseCfg()
	cfg.PeerRetention = time.Nanosecond
	kEnabled := kernel.New(st, nil, nil, nil, cfg, log.Default())
	n, err := kEnabled.PurgeIdlePeers(ctx)
	if err != nil {
		t.Fatalf("PurgeIdlePeers: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d peers, want 1", n)
	}

	if _, err := kEnabled.ReadAction(ctx, act.ID); err == nil {
		t.Error("proxy action must be deleted after purge")
	}
	if peers, _ := kEnabled.ListPeers(ctx); len(peers) != 0 {
		t.Errorf("ListPeers = %d, want 0 (peer identity forgotten)", len(peers))
	}
	u, err := kEnabled.ReadUser(ctx, peer.ID)
	if err != nil {
		t.Fatalf("anchor user row must remain: %v", err)
	}
	if u.PublicKey != "" {
		t.Errorf("public_key must be cleared, got %q", u.PublicKey)
	}
	dks, _ := kEnabled.ListDiscoveredKernels(ctx)
	for _, d := range dks {
		if d.PublicKey == pubB64 {
			t.Error("discovered_kernels row for the purged peer must be deleted")
		}
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
	peerA, err := k.AddPeer(ctx, sys.ID, "@gossip-transacted", pubAB64)
	if err != nil {
		t.Fatalf("AddPeer A: %v", err)
	}
	_, privB, _ := ed25519.GenerateKey(rand.Reader)
	pubB := privB.Public().(ed25519.PublicKey)
	pubBB64 := base64.RawURLEncoding.EncodeToString(pubB)
	_, err = k.AddPeer(ctx, sys.ID, "@gossip-untransacted", pubBB64)
	if err != nil {
		t.Fatalf("AddPeer B: %v", err)
	}

	// Create a proxy action owned by peer A with uses > 0.
	actA := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: peerA.ID, Name: "a-act",
		Kind: kernel.KindRemoteProxy, Active: true, Price: 10,
		Source:      "https://gossip-a.example.com/v1/federation/call?action=@gossip-transacted/a-act&counterparty=x",
		InputSchema: map[string]any{}, OutputSchema: map[string]any{},
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

	gossip, err := k.GetGossip(ctx, "")
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

// DiscoveryRoster groups discovered_kernels by kernel and lists each introducer's report:
// self-reported (introduced by the kernel itself) vs hearsay (introduced by a third party). Action
// names stay owner-qualified (@owner/name), never bare.
func TestDiscoveryRoster(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	keyFor := func() string {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		return base64.RawURLEncoding.EncodeToString(pub)
	}
	pubA, pubB, pubC := keyFor(), keyFor(), keyFor()

	// A self-reports an owner-qualified action and a transacted friend B.
	gA := &kernel.GossipResponse{
		PublicKey: pubA, Handle: "@a",
		Actions: []kernel.GossipAction{{ActionID: "a1", Name: "@sys/greet", Uses: 40, Rating: 0.8, Price: 5}},
		Friends: []kernel.GossipFriendView{{Handle: "@b", PublicKey: pubB,
			Actions: []kernel.GossipAction{{ActionID: "b1", Name: "@bob/translate", Uses: 3}}}},
	}
	if err := k.AccumulateGossip(ctx, gA, pubA); err != nil {
		t.Fatal(err)
	}
	// C introduces A too (third-party hearsay).
	gC := &kernel.GossipResponse{
		PublicKey: pubC, Handle: "@c",
		Friends: []kernel.GossipFriendView{{Handle: "@a", PublicKey: pubA,
			Actions: []kernel.GossipAction{{ActionID: "a1", Name: "@sys/greet", Uses: 41}}}},
	}
	if err := k.AccumulateGossip(ctx, gC, pubC); err != nil {
		t.Fatal(err)
	}

	roster, err := k.DiscoveryRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var a *kernel.KernelRoster
	for _, r := range roster {
		if r.PublicKey == pubA {
			a = r
		}
	}
	if a == nil {
		t.Fatal("kernel A missing from roster")
	}
	var selfReported, viaC bool
	for _, s := range a.Sources {
		if s.SelfReported && s.IntroducedBy == pubA {
			selfReported = true
			if len(s.Actions) == 0 || s.Actions[0].Name != "@sys/greet" {
				t.Errorf("self-report actions = %+v, want first name @sys/greet", s.Actions)
			}
		}
		if !s.SelfReported && s.IntroducedBy == pubC {
			viaC = true
		}
	}
	if !selfReported || !viaC {
		t.Errorf("A sources: selfReported=%v viaC=%v (sources=%+v)", selfReported, viaC, a.Sources)
	}
}

// TestFriendDoesNotReexportImportedProxies pins §13 non-transitivity: a kernel serves manifests and
// gossips only its OWN actions. An imported remote_proxy — even active+public — is never re-served,
// so a peer friending this kernel cannot reach a third kernel's actions through it.
func TestFriendDoesNotReexportImportedProxies(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	// Our own active+public action.
	owner := setupUser(t, st, "@localprov", 0)
	own := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "mine", Kind: kernel.KindHTTP,
		Active: true, Visibility: kernel.VisibilityPublic, Price: 10, Description: "own action",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Source:    `{"type":"http","base_url":"https://api.example.com","method":"POST","path":"/"}`,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, own); err != nil {
		t.Fatal(err)
	}

	// An imported proxy from peer C, made active+public exactly as the bulk friend-import does.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	peerC, err := k.AddPeer(ctx, sys.ID, "@peer-c", base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := kernel.ActionManifest{
		ActionID: "c-act-1", OwnerHandle: "@peer-c", Name: "sum", Description: "c sum",
		Kind: kernel.KindHTTP, Price: 50, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "sha256-c", Stats: &kernel.Stats{},
		UpdatedAt: time.Now(),
	}
	sig, _ := kernel.SignManifest(priv, &m)
	m.Signature = sig
	res, err := k.ImportRemoteAction(ctx, sys.ID, peerC.ID, m)
	if err != nil || len(res.Created) != 1 {
		t.Fatalf("ImportRemoteAction: %v (created %d)", err, len(res.Created))
	}
	proxy := res.Created[0]
	proxy.Active, proxy.Visibility = true, kernel.VisibilityPublic
	if err := st.UpdateAction(ctx, proxy); err != nil {
		t.Fatal(err)
	}

	// The imported proxy must NOT be re-exported as a manifest.
	if _, err := k.GetActionManifest(ctx, proxy.ID); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("proxy manifest: got %v, want ErrUnauthorized", err)
	}
	// Our own action still is (didn't over-filter).
	if _, err := k.GetActionManifest(ctx, own.ID); err != nil {
		t.Errorf("own manifest should succeed: %v", err)
	}
	// Gossip lists our own action, never the imported proxy as one of ours.
	g, err := k.GetGossip(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var sawOwn, sawProxy bool
	for _, ga := range g.Actions {
		sawOwn = sawOwn || ga.ActionID == own.ID
		sawProxy = sawProxy || ga.ActionID == proxy.ID
	}
	if !sawOwn {
		t.Error("gossip should include our own action")
	}
	if sawProxy {
		t.Error("gossip must NOT advertise an imported proxy as our own action")
	}
}

// TestRemoteCallNotDispatchedFailsFast: a first dispatch the transport provably never sent (§13
// never-dispatched) settles immediately as ErrPeerUnreachable with a full refund — a settled failure
// transaction, no pending trace left to retry.
func TestRemoteCallNotDispatchedFailsFast(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	fake := &fakeFederationHTTP{notDispatched: true} // resolve/connect failed: provably never sent
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(st, nil, fake, nil, cfg, log.Default())

	_, _, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "nd-action", 1000)
	before, _ := st.ReadUser(ctx, caller.ID)

	_, err := k.Run(ctx, caller.ID, "@settle-peer/settle-peer/settleact", map[string]any{})
	if !errors.Is(err, kernel.ErrPeerUnreachable) {
		t.Fatalf("Run: expected ErrPeerUnreachable, got %v", err)
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Meta["peer"] != "@settle-peer" {
		t.Errorf("expected Meta[peer]=@settle-peer, got %+v", err)
	}
	// Settled as a failure, not parked: no pending trace, exactly one failure transaction.
	if pend, _ := st.ListPendingRemoteTraces(ctx); len(pend) != 0 {
		t.Fatalf("expected 0 pending traces (settled, not parked), got %d", len(pend))
	}
	txs, _ := st.ListTransactions(ctx, kernel.TxFilter{})
	if len(txs) != 1 || txs[0].Status != kernel.TxFailure {
		t.Fatalf("expected 1 failure transaction, got %+v", txs)
	}
	// Full refund: the root failure auto-closes the process and returns the caller's funds.
	after, _ := st.ReadUser(ctx, caller.ID)
	if after.Available != before.Available || after.Locked != 0 {
		t.Errorf("expected full refund to %d/0, got %d/%d", before.Available, after.Available, after.Locked)
	}
}

// TestRetryNeverFailsFastOnNotDispatched: the retry path must NOT fail-fast on a connection failure
// (§13) — a parked trace's request may already have executed remotely, so only a signed receipt (or
// the max-age bound) may settle it. A NotDispatched retry leaves the trace pending.
func TestRetryNeverFailsFastOnNotDispatched(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bps := kernel.DefaultConfig().ImportBPS

	fake := &fakeFederationHTTP{} // first dispatch: offline (no receipt) → parked pending
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(st, nil, fake, nil, cfg, log.Default())

	_, a, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "retry-nd-action", 1000)
	mp := a.Price * 10000 / (10000 + bps)

	if _, err := k.Run(ctx, caller.ID, "@settle-peer/settle-peer/settleact", map[string]any{}); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("Run: expected ErrTimeout (parked), got %v", err)
	}
	if pend, _ := st.ListPendingRemoteTraces(ctx); len(pend) != 1 {
		t.Fatalf("expected 1 pending trace, got %d", len(pend))
	}

	// The retrier runs while the transport still can't connect (NotDispatched). It must NOT settle.
	fake.notDispatched = true
	k.RetryPendingRemoteDispatches(ctx)
	if pend, _ := st.ListPendingRemoteTraces(ctx); len(pend) != 1 {
		t.Fatalf("retry fail-fasted a parked trace: expected 1 pending, got %d", len(pend))
	}
	if txs, _ := st.ListTransactions(ctx, kernel.TxFilter{}); len(txs) != 0 {
		t.Fatalf("expected no settled transaction, got %d", len(txs))
	}

	// The peer finally answers with a valid receipt → the parked call settles.
	fake.notDispatched = false
	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx", ActionID: "retry-nd-action",
		ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxSuccess, Charge: mp, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)
	k.RetryPendingRemoteDispatches(ctx)
	if pend, _ := st.ListPendingRemoteTraces(ctx); len(pend) != 0 {
		t.Fatalf("expected 0 pending after the receipt settled, got %d", len(pend))
	}
}

// TestSettleRemoteCallPeerUnfunded: a signed zero-charge rejection carried on transport status 402
// (the remote's ErrInsufficientFunds: our credit there is exhausted) settles as ErrPeerUnfunded with
// the peer handle in meta; a rejection on 422 stays a plain ErrExecutionFailed.
func TestSettleRemoteCallPeerUnfunded(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		unfunded bool
	}{
		{"402_is_peer_unfunded", 402, true},
		{"422_is_execution_failed", 422, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			fake := &fakeFederationHTTP{httpStatus: tc.status}
			k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "unfunded-action", 1000)
			_, tr := beginTestRun(t, st, caller.ID, a)

			// A validly-signed zero-charge rejection (status=failure), the remote's refusal.
			now := time.Now().UTC()
			r := &kernel.Receipt{
				ID: uuid.New().String(), TxID: "rtx", ActionID: "unfunded-action",
				ArgsHash: jcsHashForTest(t, `{}`), Status: kernel.TxFailure, Charge: 0,
				Reason: "insufficient balance", StartedAt: now, CreatedAt: now,
			}
			r.Signature = signReceiptForTest(t, priv, r)
			b, _ := json.Marshal(r)
			fake.receiptJSON = string(b)

			_, err := k.Call(ctx, kernel.CallRequest{
				CallerID: caller.ID, ExistingTraceID: tr.ID,
				TargetUserID: "@settle-peer", ActionName: "settle-peer/settleact", Args: map[string]any{},
			})
			if tc.unfunded {
				if !errors.Is(err, kernel.ErrPeerUnfunded) {
					t.Fatalf("expected ErrPeerUnfunded on 402, got %v", err)
				}
				var ke *kernel.KernelError
				if !errors.As(err, &ke) || ke.Meta["peer"] != "@settle-peer" {
					t.Errorf("expected Meta[peer]=@settle-peer, got %+v", err)
				}
			} else {
				if !errors.Is(err, kernel.ErrExecutionFailed) {
					t.Fatalf("expected ErrExecutionFailed on 422, got %v", err)
				}
				if errors.Is(err, kernel.ErrPeerUnfunded) {
					t.Error("422 rejection must not be attributed to peer_unfunded")
				}
			}
		})
	}
}

// TestGetGossipCounterpartyBalance: gossip reports the requester's credit here only for a friended,
// non-denied key; nil for strangers, denied keys, and anonymous pulls (§13 peer sync).
func TestGetGossipCounterpartyBalance(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	sys := setupSys(t, k, st)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	friendKey := base64.RawURLEncoding.EncodeToString(pub)
	friend, err := k.AddPeer(ctx, sys.ID, "@a-friend", friendKey)
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if _, err := k.Deposit(ctx, sys.ID, friend.ID, 777, "", ""); err != nil {
		t.Fatalf("Deposit: %v", err)
	}

	g, err := k.GetGossip(ctx, friendKey)
	if err != nil {
		t.Fatalf("GetGossip: %v", err)
	}
	if g.CounterpartyBalance == nil || *g.CounterpartyBalance != 777 {
		t.Errorf("friend: expected counterparty_balance 777, got %v", g.CounterpartyBalance)
	}

	// Anonymous, stranger, and denied all omit the field.
	if g, _ := k.GetGossip(ctx, ""); g.CounterpartyBalance != nil {
		t.Error("anonymous pull must not carry counterparty_balance")
	}
	strangerPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if g, _ := k.GetGossip(ctx, base64.RawURLEncoding.EncodeToString(strangerPub)); g.CounterpartyBalance != nil {
		t.Error("stranger must not carry counterparty_balance")
	}
	if err := k.DenyPeer(ctx, sys.ID, friend.Handle); err != nil {
		t.Fatalf("DenyPeer: %v", err)
	}
	if g, _ := k.GetGossip(ctx, friendKey); g.CounterpartyBalance != nil {
		t.Error("denied peer must not carry counterparty_balance")
	}
}

// TestRecordPeerSync: a friend sync persists last_seen and the reported credit; unknown and denied
// keys are no-ops (§13 peer sync).
func TestRecordPeerSync(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	sys := setupSys(t, k, st)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)
	friend, err := k.AddPeer(ctx, sys.ID, "@sync-friend", key)
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	credit := int64(555)
	if err := k.RecordPeerSync(ctx, key, &credit); err != nil {
		t.Fatalf("RecordPeerSync: %v", err)
	}
	got, _ := st.ReadUser(ctx, friend.ID)
	if got.PeerLastSeen == nil {
		t.Error("expected peer_last_seen set after sync")
	}
	if got.PeerCredit == nil || *got.PeerCredit != 555 {
		t.Errorf("expected peer_credit 555, got %v", got.PeerCredit)
	}

	// A nil credit refreshes last_seen but keeps the prior credit (COALESCE).
	if err := k.RecordPeerSync(ctx, key, nil); err != nil {
		t.Fatalf("RecordPeerSync nil: %v", err)
	}
	got, _ = st.ReadUser(ctx, friend.ID)
	if got.PeerCredit == nil || *got.PeerCredit != 555 {
		t.Errorf("nil credit must keep prior 555, got %v", got.PeerCredit)
	}

	// Unknown key is a no-op (no error).
	strangerPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := k.RecordPeerSync(ctx, base64.RawURLEncoding.EncodeToString(strangerPub), &credit); err != nil {
		t.Errorf("unknown key should be a no-op, got %v", err)
	}
}
