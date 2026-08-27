package kernel_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	setupSys(t, k, st)

	if _, err := k.EnsureKernelAccount(ctx, "not-base64url"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for handle containing /, got %v", err)
	}

	if _, err := k.EnsureKernelAccount(ctx, "not-base64url"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for malformed public key, got %v", err)
	}

	shortKey := base64.RawURLEncoding.EncodeToString([]byte("short"))
	if _, err := k.EnsureKernelAccount(ctx, shortKey); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for short public key, got %v", err)
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	validKey := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := k.EnsureKernelAccount(ctx, validKey); err != nil {
		t.Fatalf("valid remote kernel should register: %v", err)
	}
}

func TestBindPetnameCollisionSuffixes(t *testing.T) {
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

	// Three kernels asking for the same name get distinct petnames, and each account holds none:
	// the name lives in the kernel namespace (§13).
	for i, tc := range []struct {
		key  string
		want string
	}{{key1, "remote"}, {key2, "remote-2"}, {key3, "remote-3"}} {
		acct, err := mountKernelForTest(t, k, ctx, tc.key, "remote")
		if err != nil {
			t.Fatalf("mount %d: %v", i, err)
		}
		if acct.Handle != "" {
			t.Errorf("mount %d: a kernel account holds no handle, got %q", i, acct.Handle)
		}
		rk, err := k.ReadKernel(ctx, tc.key)
		if err != nil || rk == nil {
			t.Fatalf("mount %d: read kernel: %v", i, err)
		}
		if rk.Petname != tc.want {
			t.Errorf("mount %d: petname = %q, want %q", i, rk.Petname, tc.want)
		}
	}
}

// A friend and its reciprocal both create the same proxy user at once; every concurrent
// caller must succeed idempotently, never hit a unique-key conflict.
func TestKernelMountConcurrent(t *testing.T) {
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
			_, errs[i] = mountKernelForTest(t, k, context.Background(), key, "remote")
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent create must be idempotent: %v", err)
		}
	}
}

func TestBindPetnameNormalizes(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)

	// The petname is canonicalized on binding and resolves in the kernel namespace only.
	if _, err := mountKernelForTest(t, k, ctx, key, "peerless"); err != nil {
		t.Fatalf("mount kernel: %v", err)
	}
	rk, err := k.ReadKernelByPetname(ctx, "peerless")
	if err != nil || rk == nil || rk.PublicKey != key {
		t.Fatalf("ReadKernelByPetname(peerless): got %v err %v, want key %s", rk, err, key)
	}
	if _, err := k.ReadUserByHandle(ctx, "peerless"); err == nil {
		t.Error("a petname must not resolve in the user namespace")
	}
}

// ---- Remote proxy / manifest tests ----

func TestImportRemoteActionCreatesRemoteProxy(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "remote-peer")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "remote-action-id-1",
		OwnerHandle:  "remote-peer",
		Name:         "sum",
		Description:  "sum action",
		Kind:         kernel.KindHTTP,
		Price:        50,
		RemoteBPS:    500,
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
	a, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}
	// Resolve force-enables to visibility=local: callable by local users, never re-served to a
	// further peer (non-transitive, §8/§13).
	if !a.Active || a.Visibility != kernel.VisibilityLocal {
		t.Errorf("imported proxy: active=%v visibility=%q, want active local", a.Active, a.Visibility)
	}
	if a.Kind != kernel.KindRemoteProxy {
		t.Errorf("kind: got %q, want %q", a.Kind, kernel.KindRemoteProxy)
	}
	if a.RemoteActionID != m.ActionID {
		t.Errorf("remote_action_id: got %q, want %q", a.RemoteActionID, m.ActionID)
	}
	// Two-step (§13): sr = 50 + ceil(50*500/10000) = 53; price = 53 + ceil(53*500/10000) = 53+3 = 56
	if a.Price != 56 {
		t.Errorf("price: got %d, want 56 (two-step: serving markup + import fee)", a.Price)
	}
}

func TestImportRemoteActionReimp(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "reimp-peer")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "reimp-action-id",
		OwnerHandle:  "reimp-peer",
		Name:         "calc",
		Description:  "calc action",
		Kind:         kernel.KindHTTP,
		Price:        10,
		RemoteBPS:    500,
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
	firstResult, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	firstID := firstResult.ID

	// Reimport with updated price — content hash changes → the row is updated in place.
	m.Price = 99
	sig2, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig2
	secondResult, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	second := secondResult
	if second.ID != firstID {
		t.Error("reimport must preserve the same action ID")
	}
	// Two-step (§13): sr = 99 + ceil(99*500/10000) = 104; price = 104 + ceil(104*500/10000) = 104+6 = 110
	if second.Price != 110 {
		t.Errorf("reimport price: got %d, want 110 (two-step: serving markup + import fee)", second.Price)
	}
}

func TestImportRemoteActionUnchangedPreservesActiveAndStats(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "stable-peer")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "stable-action-id",
		OwnerHandle:  "stable-peer",
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
	firstResult, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	firstID := firstResult.ID
	if !firstResult.Active {
		t.Fatal("import should activate the proxy (§8 resolve force-enables)")
	}

	// Re-import the identical manifest (same signature).
	secondResult, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if secondResult.ID != firstID {
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
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "idem-peer")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "idem-action-id",
		OwnerHandle:  "idem-peer",
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
	firstResult, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	firstID := firstResult.ID

	// Second import: price change updates the row in place, ArtifactHash stored as contentHash.
	m.Price = 99
	sign()
	secondResult, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if secondResult.ID != firstID {
		t.Error("reimport must preserve the same action ID")
	}

	// Third import: same manifest as second → unchanged (ArtifactHash stored correctly), id preserved.
	thirdResult, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("third import: %v", err)
	}
	if thirdResult.ID != firstID {
		t.Error("third import must reference the same action ID")
	}
}

func TestImportRemoteActionRejectsInvalidSignature(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "bad-sig-peer")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "bad-sig-action",
		OwnerHandle:  "bad-sig-peer",
		Name:         "greet",
		Kind:         kernel.KindHTTP,
		Price:        0,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Signature:    "invalidsignature",
	}
	_, err = k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err == nil {
		t.Fatal("expected error for invalid manifest signature")
	}
}

func TestImportRemoteActionRejectsNegativePrice(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "neg-price-peer")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "neg-price-action",
		OwnerHandle:  "neg-price-peer",
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

	_, err = k.ImportPeerAction(ctx, remoteUser.ID, m)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("negative price manifest: want ErrInvalidInput, got %v", err)
	}
}

func TestImportRemoteActionRejectsMissingRequiredFields(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "mrf-peer")
	if err != nil {
		t.Fatal(err)
	}

	base := kernel.ActionManifest{
		ActionID:     "mrf-action-1",
		OwnerHandle:  "mrf-peer",
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
			_, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
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
	su := setupUser(t, st, "sys", 0)
	k := newTestKernel(st)
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	owner := setupUser(t, st, "local-owner", 0)
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
	g, err := k.GetGossip(ctx, "", "")
	if err != nil {
		t.Fatalf("GetGossip: %v", err)
	}
	for _, m := range g.ActionManifests {
		if m.ActionID == a.ID {
			t.Error("local action must not appear in gossip")
		}
	}
}

// TestGossipAboutFromSysDescription: the kernel's "about" in gossip is @sys's user description (§13),
// so an operator sets it with the ordinary user-update path rather than a config key.
func TestGossipAboutFromSysDescription(t *testing.T) {
	st := newTestStore(t)
	su := setupUser(t, st, "sys", 0)
	k := newTestKernel(st)
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	su.Description = "the neighbourhood kernel"
	if err := st.UpdateUser(ctx, su); err != nil {
		t.Fatal(err)
	}
	g, err := k.GetGossip(ctx, "", "")
	if err != nil {
		t.Fatalf("GetGossip: %v", err)
	}
	if g.About != "the neighbourhood kernel" {
		t.Errorf("gossip about: got %q, want @sys's description", g.About)
	}
}

func TestGetActionManifestIncludesActionID(t *testing.T) {
	st := newTestStore(t)
	su := setupUser(t, st, "sys", 0)
	k := newTestKernel(st)
	k.SetSigningKey(testSigningKey(), su.ID)
	ctx := context.Background()

	owner := setupUser(t, st, "manifest-owner2", 0)
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
	setupSys(t, nil, st)

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

	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "proxy-peer")
	if err != nil {
		t.Fatal(err)
	}

	m := kernel.ActionManifest{
		ActionID:     "proxy-action-1",
		OwnerHandle:  "proxy-peer",
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

	result, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}
	a := result // ImportPeerAction leaves the proxy active+local, callable by a local caller (§8)

	caller := setupUser(t, st, "proxy-caller", 0)
	p, tr := beginTestRun(t, st, caller.ID, a)

	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID:        caller.ID,
		ExistingTraceID: tr.ID,
		TargetUserID:    remoteUser.ID,
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
func setupSettleProxy(t *testing.T, st kernel.Store, fake *fakeFederationHTTP, priv ed25519.PrivateKey, pub ed25519.PublicKey, remoteActionID string, proxyPrice int64) (*kernel.Kernel, *kernel.Action, *kernel.Account) {
	t.Helper()
	return setupSettleProxyWithKernel(t, st, newTestKernelWithHTTP(st, fake), priv, pub, remoteActionID, proxyPrice)
}

// setupSettleProxyWithKernel is setupSettleProxy against a caller-supplied kernel, so a test
// can configure the kernel (e.g. RemotePendingMaxAge) before importing the proxy.
func setupSettleProxyWithKernel(t *testing.T, st kernel.Store, k *kernel.Kernel, priv ed25519.PrivateKey, pub ed25519.PublicKey, remoteActionID string, proxyPrice int64) (*kernel.Kernel, *kernel.Action, *kernel.Account) {
	t.Helper()
	ctx := context.Background()
	setupSys(t, nil, st)

	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "settle-peer")
	if err != nil {
		t.Fatalf("EnsureKernelAccount: %v", err)
	}
	m := kernel.ActionManifest{
		ActionID: remoteActionID, OwnerHandle: "settle-peer", Name: "settleact",
		Kind: kernel.KindHTTP, Price: proxyPrice, RemoteBPS: kernel.DefaultConfig().RemoteBPS, Description: "s",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	m.Signature, _ = kernel.SignManifest(priv, &m)
	result, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}
	a := result // proxy is active+local after import (§8)
	// Fund the caller with exactly the proxy price so a full refund restores the original balance.
	caller := setupUser(t, st, "settle-caller", a.Price)
	return k, a, caller
}

// TestRemoteDispatchUsesStableActionID: dispatch names the peer's stable action id, never the
// cached display reference in Source — on the first call and on the retry that settles a parked
// one (§13). A row cached before handles were bare stores "@owner/name", which no current peer can
// parse; that is a plain error, not a signed rejection, so a name-addressed dispatch would park the
// caller until the max-age bound with no way to self-heal. A remote owner rename drifts the same way.
func TestRemoteDispatchUsesStableActionID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	fake := &fakeFederationHTTP{} // no receipt → the call parks, so the retry path is reachable
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})

	_, a, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "stable-action", 1000)
	a.Source = "@settle-peer/settleact" // the legacy sigil form a v0.12.4+ peer rejects
	if err := st.UpdateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{}}); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("Run: expected ErrTimeout (pending), got %v", err)
	}
	if fake.sentAction != "stable-action" {
		t.Errorf("first dispatch sent %q, want the stable remote action id", fake.sentAction)
	}
	pend, _ := st.ListPendingRemoteTraces(ctx)
	if len(pend) != 1 {
		t.Fatalf("expected 1 pending remote trace, got %d", len(pend))
	}
	ikey := *pend[0].IdempotencyKey
	if fake.sentIdempotencyKey != ikey {
		t.Errorf("first dispatch sent key %q, want the parked %q", fake.sentIdempotencyKey, ikey)
	}

	fake.sentAction, fake.sentIdempotencyKey = "", ""
	k.RetryPendingRemoteDispatches(ctx)
	if fake.sentAction != "stable-action" {
		t.Errorf("retry dispatched %q, want the stable remote action id", fake.sentAction)
	}
	// The retry must re-present the parked key on the wire, or the peer executes the call twice.
	if fake.sentIdempotencyKey != ikey {
		t.Errorf("retry sent key %q, want the parked %q", fake.sentIdempotencyKey, ikey)
	}
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
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})

	_, _, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "exp-action", 1000)
	before, _ := st.ReadUser(ctx, caller.ID)

	// Real root run: the empty receipt makes the proxy call time out; the process stays open and
	// the trace persists in the DB with its idempotency key (beginRun records the dispatch).
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{}}); !errors.Is(err, kernel.ErrTimeout) {
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
	bps := kernel.DefaultConfig().RemoteBPS

	// Empty receiptJSON → the peer is "offline": ExecuteFederation returns no receipt → pending.
	fake := &fakeFederationHTTP{}
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	// Default RemotePendingMaxAge (24h): the trace stays pending, not force-expired.
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})

	_, a, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "ret-action", 1000)
	mp := *a.BasePrice
	premium := (mp*bps + 9999) / 10000

	// Call while the peer is offline → pending, no settled transaction, funds locked.
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{}}); !errors.Is(err, kernel.ErrTimeout) {
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
		Status: kernel.TxSuccess, Charge: mp, Premium: premium, StartedAt: now, CreatedAt: now,
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
	if txs[0].Net != mp+premium {
		t.Errorf("net: got %d, want %d (charge+premium paid to proxy)", txs[0].Net, mp+premium)
	}
	// Caller was funded exactly the proxy price; a success spends it all and closes the process.
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
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})

	_, _, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "await-action", 1000)

	// Offline call → parked, awaiting a receipt.
	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{}}); !errors.Is(err, kernel.ErrTimeout) {
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
	bps := kernel.DefaultConfig().RemoteBPS

	fake := &fakeFederationHTTP{} // offline: no receipt → pending
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})

	_, a, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "wrap-action", 1000)
	mp := *a.BasePrice
	premium := (mp*bps + 9999) / 10000

	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{}}); !errors.Is(err, kernel.ErrTimeout) {
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
		Status: kernel.TxSuccess, Charge: mp, Premium: premium, StartedAt: now, CreatedAt: now,
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

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{},
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

// TestGossipEvidenceExcludesDelegatedAuth: a delegated-auth action is never described abroad (§6 P6,
// §8 D10), so evidence must not name it either — otherwise a peer learns the existence, identity, and
// usage volume of a capability it can never call or even see. An ordinary public action's execution
// stays gossip-eligible, so the exclusion is the scheme, not the leg.
func TestGossipEvidenceExcludesDelegatedAuth(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithHTTP(st, &fakeSuccessHTTP{})
	k.SetSecretBox(b64Box{})
	ctx := context.Background()
	setupSys(t, k, st)
	owner := setupUser(t, st, "evid-owner", 1000)

	plain := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "plain",
		Kind: kernel.KindHTTP, Source: "https://provider.example/api", Active: true,
		Visibility: kernel.VisibilityPublic, Description: "d",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, plain); err != nil {
		t.Fatal(err)
	}
	// A real delegated action: public, active, with the caller's own token attached, so it executes
	// and settles exactly as in production and leaves an ordinary leg-(a) receipt behind.
	delegated := createBearerAction(t, k, owner.ID, "delegated", 0)
	pub := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: delegated.ID, Visibility: &pub}); err != nil {
		t.Fatalf("make delegated public: %v", err)
	}
	if err := k.SetActive(ctx, owner.ID, delegated.ID, true); err != nil {
		t.Fatalf("reactivate delegated: %v", err)
	}
	if _, err := k.AttachBearerGrants(ctx, owner.ID, owner.Handle+"/"+delegated.Name, "", "tok"); err != nil {
		t.Fatalf("AttachBearerGrants: %v", err)
	}

	for _, a := range []*kernel.Action{plain, delegated} {
		_, tr := beginTestRun(t, st, owner.ID, a)
		if _, err := k.TestCall(ctx, kernel.TestCallRequest{
			CallerID: owner.ID, ExistingTraceID: tr.ID, TargetUserID: owner.ID,
			ActionName: a.Name, Args: map[string]any{},
		}); err != nil {
			t.Fatalf("run %s: %v", a.Name, err)
		}
	}

	resp, err := k.GetGossip(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range resp.Evidence {
		if b.EvidenceReceipt != nil && b.EvidenceReceipt.SubjectActionID == delegated.ID {
			t.Error("evidence must not name a delegated-auth action the manifests exclude")
		}
	}
	found := false
	for _, b := range resp.Evidence {
		if b.EvidenceReceipt != nil && b.EvidenceReceipt.SubjectActionID == plain.ID {
			found = true
		}
	}
	if !found {
		t.Error("an ordinary public action's execution must still be gossip-eligible")
	}
}

// TestGossipEvidenceExcludesNonExecutions proves the §13/§15 leg-(b) exclusions end-to-end through
// ACTUAL settlements (not fabricated rows): only a receipt-backed admitted execution is gossip-
// eligible. Three real proxy calls settle on one price-0 proxy — an admitted success, a
// never-dispatched failure (ErrPeerUnreachable, no remote receipt stored), and a quarantined invalid
// receipt (a success claiming a charge the price-0 action cannot have) — and GetGossip must emit
// EXACTLY ONE evidence bundle: the admitted execution.
func TestGossipEvidenceExcludesNonExecutions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "ra-evid", 0)

	mkReceipt := func(txID string, charge int64) string {
		now := time.Now().UTC()
		r := &kernel.Receipt{
			ID: uuid.New().String(), TxID: txID, ActionID: "ra-evid",
			ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: jcsHashForTest(t, `{}`),
			Status: kernel.TxSuccess, Charge: charge, StartedAt: now, CreatedAt: now,
		}
		r.Signature = signReceiptForTest(t, priv, r)
		b, _ := json.Marshal(r)
		return string(b)
	}
	// drive runs one proxy call on a fresh root trace; driveKeyed persists a known idempotency_key on
	// the trace first (a root call reads the key back from the DB), so a rejection receipt can echo it.
	driveKeyed := func(idem string) {
		p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: caller.ID, Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
		tr := &kernel.Trace{ID: uuid.New().String(), ProcessID: p.ID, ActionOwnerID: a.OwnerUserID, ActionID: a.ID, CallerUserID: caller.ID, CreatedAt: time.Now().UTC()}
		if idem != "" {
			tr.IdempotencyKey = &idem
		}
		if err := st.BeginRun(ctx, p, tr, caller.ID, a.Price, 0, 0); err != nil {
			t.Fatalf("BeginRun: %v", err)
		}
		_, _ = k.TestCall(ctx, kernel.TestCallRequest{
			CallerID: caller.ID, ExistingTraceID: tr.ID,
			ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{},
		})
	}
	drive := func() { driveKeyed("") }

	// 1. Admitted execution: a valid success receipt (charge 0 == the proxy's price 0).
	fake.notDispatched, fake.receiptJSON = false, mkReceipt("remote-real-tx", 0)
	drive()
	// 2. Never dispatched: settled locally as ErrPeerUnreachable — no remote receipt is stored.
	fake.notDispatched, fake.receiptJSON = true, ""
	drive()
	// 3. Signed rejection: a zero-charge refusal whose tx_id == the caller's idempotency_key (§13). The
	//    fake echoes the dispatched key, so the origin recognises it as a rejection, not an execution.
	fake.notDispatched, fake.receiptJSON = false, ""
	fake.rejectSignKey, fake.rejectActionID, fake.rejectArgsHash = priv, "ra-evid", jcsHashForTest(t, `{}`)
	driveKeyed("idem-reject-1")
	fake.rejectSignKey = nil
	// 4. Quarantined: a success receipt claiming charge 5 on a price-0 action → invalid → reserve kept
	//    locked, settled as failure (runs LAST — a quarantine deactivates the proxy).
	fake.receiptJSON = mkReceipt("remote-real-tx-2", 5)
	drive()

	resp, err := k.GetGossip(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Evidence) != 1 {
		t.Fatalf("expected exactly 1 gossip evidence bundle (admitted execution only), got %d", len(resp.Evidence))
	}
	if resp.Evidence[0].EvidenceReceipt == nil || resp.Evidence[0].EvidenceReceipt.SubjectActionID != "ra-evid" {
		t.Errorf("the one bundle must be the admitted execution about ra-evid, got %+v", resp.Evidence[0].EvidenceReceipt)
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

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{},
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
			mp := a.Price * 10000 / (10000 + kernel.DefaultConfig().RemoteBPS)

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

			_, err := k.TestCall(ctx, kernel.TestCallRequest{
				CallerID: caller.ID, ExistingTraceID: tr.ID,
				ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{},
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
			// Rule C (§13): a quarantined receipt deactivates the cached proxy so the next call re-resolves.
			if ra, _ := st.ReadAction(ctx, a.ID); ra.Active {
				t.Error("quarantine must deactivate the proxy (rule C)")
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
	bps := kernel.DefaultConfig().RemoteBPS
	ibps := kernel.DefaultConfig().ImportBPS
	k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "valid-action", 1000)
	_, tr := beginTestRun(t, st, caller.ID, a)
	mp := *a.BasePrice
	premium := (mp*bps + 9999) / 10000

	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx", ActionID: "valid-action",
		ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxSuccess, Charge: mp, Premium: premium, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)

	if _, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{},
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
	if tx.Net != mp+premium {
		t.Errorf("net: got %d, want %d (charge+premium, unclamped)", tx.Net, mp+premium)
	}
	wantImportFee := ((mp+premium)*ibps + 9999) / 10000 // ceilDiv((charge+premium)*import_bps, 10000)
	if tx.Fee != wantImportFee {
		t.Errorf("import fee: got %d, want %d", tx.Fee, wantImportFee)
	}
	v, err := k.VerifyRemoteReceipt(ctx, caller.ID, tx.ID)
	if err != nil {
		t.Fatalf("VerifyRemoteReceipt: %v", err)
	}
	if !v.Valid {
		t.Errorf("expected valid receipt, got checks %+v", v.Checks)
	}
}

// A receipt that is otherwise a valid success but carries refresh_proxy is malformed (§13 rule C):
// refresh_proxy is only ever a zero-charge rejection, so the receipt quarantines rather than paying.
func TestSettleRemoteCallRejectsRefreshProxyOnSuccess(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "rp-inv-action", 1000)
	_, tr := beginTestRun(t, st, caller.ID, a)
	mp := *a.BasePrice
	premium := (mp*kernel.DefaultConfig().RemoteBPS + 9999) / 10000

	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx", ActionID: "rp-inv-action",
		ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxSuccess, Charge: mp, Premium: premium,
		RefreshProxy: true, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Fatalf("expected quarantine (ErrExecutionFailed), got %v", err)
	}
	assertUserBalance(t, st, caller.ID, a.Price, 0) // fully refunded, nothing paid
}

// TestRefreshProxyRejectionInvalidatesAndExplains: when the peer's contract has moved under our
// cached row it refuses with a signed refresh_proxy rejection (§8 If-Match). Three things must
// follow, and the third is the one a buyer feels: nothing is charged, the stale row is invalidated
// so the next call re-resolves, and the failure SAYS the provider updated the action instead of
// reading as a generic remote error. It is deliberately NOT reported as changed terms: the contract
// hash also covers the artifact and kind (§6 P6), so this fires on a re-implementation at an
// unchanged price, and consent is the quote pin's job at the funding boundary (§4 precondition 7).
func TestRefreshProxyRejectionInvalidatesAndExplains(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{httpStatus: 409}
	k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "rp-action", 1000)
	_, tr := beginTestRun(t, st, caller.ID, a)
	fake.rejectSignKey, fake.rejectActionID = priv, "rp-action"
	fake.rejectArgsHash = jcsHashForTest(t, `{}`)
	fake.rejectRefreshProxy = true

	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Fatalf("a contract mismatch is an ordinary failure, got %v", err)
	}
	if errors.Is(err, kernel.ErrTermsChanged) {
		t.Error("a contract mismatch must not claim the buyer's terms changed: the hash covers the implementation too")
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Meta["retry"] != "refresh" {
		t.Errorf("the failure must say it is refreshable, got meta %v", ke.Meta)
	}
	if !strings.Contains(err.Error(), "updated this action") || !strings.Contains(err.Error(), "nothing was charged") {
		t.Errorf("the message must name the cause and the cost, got %q", err.Error())
	}
	assertUserBalance(t, st, caller.ID, a.Price, 0) // full refund
	if ra, _ := st.ReadAction(ctx, a.ID); ra.Active {
		t.Error("a contract mismatch must invalidate the cached row so the next call re-resolves")
	}
}

// TestParkedRemoteCallHandsBackItsProcess: a dispatched call with no signed outcome yet is parked,
// not lost — the allocation stays locked and the process open until a receipt arrives or the
// pending bound expires (§13). The caller must be able to follow that money, so the refusal carries
// the durable handle: which process holds it, since when, and when a refund falls due.
func TestParkedRemoteCallHandsBackItsProcess(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{} // no receipt → parked
	k, _, caller := setupSettleProxy(t, st, fake, priv, pub, "park-action", 1000)

	ref := "settle-peer@settle-peer/settleact"
	_, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: ref, Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("expected a parked call, got %v", err)
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) {
		t.Fatalf("expected a structured error, got %v", err)
	}
	pend, _ := st.ListPendingRemoteTraces(ctx)
	if len(pend) != 1 {
		t.Fatalf("expected 1 parked trace, got %d", len(pend))
	}
	if ke.Meta["process_id"] != pend[0].ProcessID {
		t.Errorf("process handle: got %q, want %q", ke.Meta["process_id"], pend[0].ProcessID)
	}
	since, perr := time.Parse(time.RFC3339, ke.Meta["pending_since"])
	if perr != nil {
		t.Errorf("pending_since must be RFC 3339, got %q", ke.Meta["pending_since"])
	}
	refundAt, rerr := time.Parse(time.RFC3339, ke.Meta["refund_eligible_at"])
	if rerr != nil {
		t.Fatalf("refund_eligible_at must be RFC 3339, got %q", ke.Meta["refund_eligible_at"])
	}
	// The eligibility time is the pending bound the retry loop actually enforces (§13), not a
	// number invented for the message.
	if want := since.Add(24 * time.Hour); !refundAt.Equal(want) {
		t.Errorf("refund_eligible_at = %v, want %v (pending_since + the max pending age)", refundAt, want)
	}
	// A parked call is not a settled one: the money is still reserved, not spent.
	u, _ := st.ReadUser(ctx, caller.ID)
	if u.Locked == 0 {
		t.Error("a parked call must keep its allocation locked")
	}
}

// Rule D (§8): a remote_proxy is kernel-managed; manual enable/disable, update, and delete are all
// rejected. A hand-set public proxy would pass a peer's CanCall and break non-transitivity.
func TestProxyMutationsRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "mut-peer")
	if err != nil {
		t.Fatal(err)
	}
	m := kernel.ActionManifest{
		ActionID: "mut-act", OwnerHandle: "mut-peer", Name: "svc", Description: "svc",
		Kind: kernel.KindHTTP, Price: 5, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	m.Signature, _ = kernel.SignManifest(priv, &m)
	proxy, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}
	pub2 := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: proxy.ID, Visibility: &pub2}); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("UpdateAction(visibility=public) on proxy: want ErrInvalidState, got %v", err)
	}
	if err := k.DeleteAction(ctx, sys.ID, proxy.ID); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("DeleteAction on proxy: want ErrInvalidState, got %v", err)
	}
	// The proxy row is untouched: still active and local, never public.
	if got, _ := st.ReadAction(ctx, proxy.ID); got == nil || got.Visibility != kernel.VisibilityLocal || !got.Active {
		t.Errorf("proxy must stay active+local after rejected mutations, got %+v", got)
	}
}

// B5 (§8, §13 grammar): a proxy is addressable only kernel-qualified (owner@kernel/name) or by raw
// id — never by the legacy bare mount form (mount/owner/name), which must not resolve.
func TestProxyAddressableFormsOnly(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	peer, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "mp-peer")
	m := kernel.ActionManifest{
		ActionID: "mp-act", OwnerHandle: "mp-owner", Name: "act", Description: "svc",
		Kind: kernel.KindHTTP, Price: 5, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	m.Signature, _ = kernel.SignManifest(priv, &m)
	proxy, err := k.ImportPeerAction(ctx, peer.ID, m)
	if err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}
	// Kernel-qualified resolves; raw id resolves; the legacy mount form does not.
	if a, err := k.ResolveAction(ctx, "mp-owner@mp-peer/act"); err != nil || a.ID != proxy.ID {
		t.Errorf("owner@kernel/name: want proxy, got (%v, %v)", a, err)
	}
	if a, err := k.ResolveAction(ctx, proxy.ID); err != nil || a.ID != proxy.ID {
		t.Errorf("raw id: want proxy, got (%v, %v)", a, err)
	}
	if _, err := k.ResolveAction(ctx, "mp-peer/mp-owner/act"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("legacy mount form must not resolve: want ErrNotFound, got %v", err)
	}
}

// D (§3, §14): a sigil-prefixed handle is rejected wherever it enters — a manifest owner_handle is
// not silently embedded into a proxy name, and a step's required-caller "@bob" is not misparsed as a
// kernel-qualified ref with an empty owner.
func TestSigilHandleRejectedAtBoundaries(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	peer, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := kernel.ActionManifest{
		ActionID: "sig-act", OwnerHandle: "@bob", Name: "act", Description: "svc",
		Kind: kernel.KindHTTP, Price: 5, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	m.Signature, _ = kernel.SignManifest(priv, &m)
	if _, err := k.ImportPeerAction(ctx, peer.ID, m); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("import with owner_handle=@bob: want ErrInvalidInput, got %v", err)
	}

	if _, _, err := k.ResolveRequiredCaller(ctx, "@bob"); err == nil {
		t.Error("ResolveRequiredCaller(@bob): want error, got nil")
	}
}

// Rule D (§8): a remote_proxy's active bit is kernel-managed; manual enable/disable is rejected.
func TestSetActiveRejectsRemoteProxy(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "d-peer")
	if err != nil {
		t.Fatal(err)
	}
	m := kernel.ActionManifest{
		ActionID: "d-act", OwnerHandle: "d-peer", Name: "svc", Description: "svc",
		Kind: kernel.KindHTTP, Price: 5, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	m.Signature, _ = kernel.SignManifest(priv, &m)
	proxy, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}
	for _, active := range []bool{false, true} {
		if err := k.SetActive(ctx, sys.ID, proxy.ID, active); !errors.Is(err, kernel.ErrInvalidState) {
			t.Errorf("SetActive(%v) on proxy: want ErrInvalidState, got %v", active, err)
		}
	}
}

// ---- D1: proxy action source is the remote action ref ----

func TestImportRemoteActionSourceIsActionRef(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	peer, err := k.EnsureKernelAccount(ctx, pubB64)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	m := kernel.ActionManifest{
		ActionID: "ref-action-1", OwnerHandle: "ref-peer", Name: "act",
		Kind: kernel.KindHTTP, Price: 0, Description: "d",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	sig, _ := kernel.SignManifest(priv, &m)
	m.Signature = sig
	result, err := k.ImportPeerAction(ctx, peer.ID, m)
	if err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}
	a := result
	// Source is the remote action ref (@owner/name) — never a URL; the peer is resolved by key.
	if a.Source != "ref-peer/act" {
		t.Errorf("source: want @ref-peer/act, got %q", a.Source)
	}
	if a.RemoteActionID != "ref-action-1" {
		t.Errorf("remote_action_id: want ref-action-1, got %q", a.RemoteActionID)
	}
}

// ---- D5: duplicate identity guard ----

func TestRemoteImportOwnerQualifiedNoCollision(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	setupSys(t, nil, st)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	k := newTestKernelWithHTTP(st, &fakeFederationHTTP{})

	const peerPetname = "collide-peer"
	peerKey := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := k.EnsureKernelAccount(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}
	bindPetnameForTest(t, k, ctx, peerKey, peerPetname)
	imp := func(owner, actionID string) {
		m := kernel.ActionManifest{
			ActionID: actionID, OwnerHandle: owner, Name: "greet",
			Kind: kernel.KindHTTP, Price: 0, Description: "g",
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
		}
		m.Signature, _ = kernel.SignManifest(priv, &m)
		if _, err := k.ImportPeerAction(ctx, peer.ID, m); err != nil {
			t.Fatalf("import %s: %v", owner, err)
		}
	}
	imp("alice", "act-alice")
	imp("bob", "act-bob") // same Name "greet", different owner — must NOT collide

	// Two owners on one peer keep distinct proxy rows and both resolve kernel-qualified (§13):
	// alice@peer/greet and bob@peer/greet, addressed by the kernel's local petname.
	for _, owner := range []string{"alice", "bob"} {
		ref := owner + "@" + peerPetname + "/greet"
		a, err := k.ResolveAction(ctx, ref)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if a.OwnerUserID != peer.ID || a.Name != owner+"/greet" {
			t.Errorf("%s resolved to owner=%s name=%q, want the peer account and %q",
				ref, a.OwnerUserID, a.Name, owner+"/greet")
		}
	}
}

func TestStepAuthSignatureDomainDisjoint(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	cp, recip, uid, sid, ts := "cpkey", "recipkey", "user-1", "step-1", "2026-07-31T00:00:00Z"

	sig, err := kernel.SignStepAuthPayload(priv, cp, recip, uid, sid, ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := kernel.VerifyStepAuthSignature(pubB64, cp, recip, uid, sid, ts, sig); err != nil {
		t.Fatalf("valid attestation rejected: %v", err)
	}
	// A different user_id must not verify against the same signature.
	if err := kernel.VerifyStepAuthSignature(pubB64, cp, recip, "other", sid, ts, sig); err == nil {
		t.Error("wrong user_id verified")
	}
	// Domain disjointness: step-complete and step_auth signatures never verify as each other.
	csig, _ := kernel.SignStepPayload(priv, sid, cp, recip, "idem", ts, "ihash")
	if err := kernel.VerifyStepAuthSignature(pubB64, cp, recip, uid, sid, ts, csig); err == nil {
		t.Error("step-complete signature verified as step_auth")
	}
	if err := kernel.VerifyStepSignature(pubB64, sid, cp, recip, "idem", ts, "ihash", sig); err == nil {
		t.Error("step_auth signature verified as step-complete")
	}
}

func TestResolvePrincipal(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "alice", Password: "pw123"})
	if err != nil {
		t.Fatal(err)
	}
	// By bare handle and by raw id resolve to the stable (id, handle).
	for _, ref := range []string{"alice", u.ID} {
		id, handle, err := k.ResolvePrincipal(ctx, ref)
		if err != nil || id != u.ID || handle != "alice" {
			t.Errorf("ResolvePrincipal(%q) = (%q,%q,%v), want (%q,alice,nil)", ref, id, handle, err, u.ID)
		}
	}
	// A sigil-prefixed handle no longer resolves (handles are bare, §14), nor does an unknown ref.
	for _, ref := range []string{"@alice", "nobody"} {
		if _, _, err := k.ResolvePrincipal(ctx, ref); err == nil {
			t.Errorf("ResolvePrincipal(%q): expected error", ref)
		}
	}
}

func TestLazyResolveRemoteCachesProxy(t *testing.T) {
	st := newTestStore(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	m := kernel.ActionManifest{
		ActionID: "ra-1", OwnerID: "remote-bob-id", OwnerHandle: "bob", Name: "greet",
		RemoteBPS: 500, Description: "greet", Kind: kernel.KindHTTP, Price: 100,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	fake := &fakeFederationHTTP{resolveManifest: &m}
	k := newTestKernelWithHTTP(st, fake)
	ctx := context.Background()

	// A cold call to bob@<key>/greet lazily resolves the signed manifest and caches a local proxy.
	a, err := k.ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil {
		t.Fatalf("lazy resolve: %v", err)
	}
	if a.Kind != kernel.KindRemoteProxy || !a.Active || a.Visibility != kernel.VisibilityLocal {
		t.Errorf("proxy row: kind=%v active=%v vis=%v", a.Kind, a.Active, a.Visibility)
	}
	if a.Price != 111 { // two-step: sr=100+5=105; price=105+ceil(105*500/10000)=105+6=111
		t.Errorf("price: got %d, want 111 (two-step: serving markup + import fee)", a.Price)
	}
	if a.RemoteOwnerID != "remote-bob-id" {
		t.Errorf("remote_owner_id: got %q, want remote-bob-id", a.RemoteOwnerID)
	}
	// Second resolve is a cache hit: same row, and the resolver need not be consulted.
	fake.resolveManifest = nil
	a2, err := k.ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil || a2.ID != a.ID {
		t.Errorf("cache hit: err=%v id2=%v want %v", err, a2.ID, a.ID)
	}

	// Rule A (§8): an inactive proxy is a cache miss — the next resolve re-resolves and reactivates it
	// in place (id preserved), so a drift-deactivated proxy is never permanently dead.
	if err := st.DeactivateImportedIfHash(ctx, a.ID, a.ArtifactHash, time.Now().UTC()); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	fake.resolveManifest = &m
	a3, err := k.ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil || a3.ID != a.ID || !a3.Active {
		t.Errorf("inactive re-resolve: err=%v id=%v active=%v (want same id, active)", err, a3.ID, a3.Active)
	}
	// With the row inactive and no resolver reachable, the reference does not resolve (no dead-row serve).
	if err := st.DeactivateImportedIfHash(ctx, a.ID, a.ArtifactHash, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	fake.resolveManifest = nil
	if _, err := k.ResolveAction(ctx, "bob@"+pubB64+"/greet"); err == nil {
		t.Error("inactive proxy with no resolver should not resolve")
	}
}

// TestColdResolveIndexesAndBinds: a verified outbound resolve must leave the action USABLE, not
// merely cached. It is indexed for lookup (§9 — otherwise buying an action removes it from search,
// since the proxy shadows the discovery row it replaces) and it binds a petname (§13 first
// meaningful use). Both are asserted through the real ResolveAction path, not by calling the
// helpers directly — that is what the previous coverage missed.
func TestColdResolveIndexesAndBinds(t *testing.T) {
	newPeer := func(t *testing.T, handle string) (kernel.ActionManifest, string) {
		t.Helper()
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		key := base64.RawURLEncoding.EncodeToString(pub)
		m := kernel.ActionManifest{
			ActionID: "ra-" + handle, OwnerID: "remote-" + handle, OwnerHandle: "bob", Name: "greet",
			RemoteBPS: 500, Description: "greet a person warmly", Kind: kernel.KindHTTP, Price: 100,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
		}
		sig, err := kernel.SignManifest(priv, &m)
		if err != nil {
			t.Fatal(err)
		}
		m.Signature = sig
		return m, key
	}

	for _, tc := range []struct {
		name, nickname string
		preAccount     bool
	}{
		// Cold peer: nothing known about it before the resolve.
		{"cold peer", "provider-a", false},
		// The case that actually broke: the peer already holds an account (it called us, or we
		// deposited to it), so the bind must NOT be conditioned on the account being absent.
		{"peer with an existing account", "provider-b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			m, key := newPeer(t, tc.nickname)
			k := newTestKernelWithHTTP(st, &fakeFederationHTTP{resolveManifest: &m})
			ctx := context.Background()
			// A valid, free nickname is available for the bind to seed from.
			if err := st.UpsertKernel(ctx, key, tc.nickname, "", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if tc.preAccount {
				if _, err := k.EnsureKernelAccount(ctx, key); err != nil {
					t.Fatal(err)
				}
			}

			a, err := k.ResolveAction(ctx, "bob@"+key+"/greet")
			if err != nil {
				t.Fatalf("cold resolve: %v", err)
			}

			// Indexed: the lexical leg must reach the proxy, else it is unfindable once resolved.
			ids, err := st.SearchActionsLexical(ctx, "greet warmly", 10)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, id := range ids {
				found = found || id == a.ID
			}
			if !found {
				t.Errorf("resolved proxy %s is absent from the lexical index: %v", a.ID, ids)
			}

			// Bound: first meaningful use names the peer (§13), seeded from its valid nickname.
			rk, err := k.ReadKernel(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if rk.Petname != tc.nickname {
				t.Errorf("petname = %q, want %q (first meaningful use must bind)", rk.Petname, tc.nickname)
			}
		})
	}
}

func TestVerifyRemoteReceiptValid(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	setupSys(t, nil, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	k := newTestKernelWithHTTP(st, fake)

	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "verify-peer")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	m := kernel.ActionManifest{
		ActionID: "verify-action-1", OwnerHandle: "verify-peer", Name: "vact",
		Kind: kernel.KindHTTP, Price: 0, Description: "v",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	msig, _ := kernel.SignManifest(priv, &m)
	m.Signature = msig
	result, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	a := result // proxy is active+local after import (§8)

	caller := setupUser(t, st, "verify-caller", 0)
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

	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: remoteUser.ID, ActionName: "verify-peer/vact", Args: map[string]any{},
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
	caller := setupUser(t, st, "vrr-caller", 100)

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
	pubFed3 := kernel.VisibilityPublic
	if _, err := k.UpdateAction(ctx, sys.ID, kernel.UpdateActionRequest{ID: a.ID, Visibility: &pubFed3}); err != nil {
		t.Fatal(err)
	}
	if err := k.SetActive(ctx, sys.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}

	p, tr := beginTestRun(t, st, caller.ID, a)
	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
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
	setupSys(t, nil, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)

	fake := &fakeFederationHTTP{}
	k := newTestKernelWithHTTP(st, fake)

	remoteUser, _ := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	m := kernel.ActionManifest{
		ActionID: "tamper-action-1", OwnerHandle: "tamper-peer", Name: "tact",
		Kind: kernel.KindHTTP, Price: 0, Description: "t",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	msig, _ := kernel.SignManifest(priv, &m)
	m.Signature = msig
	result, _ := k.ImportPeerAction(ctx, remoteUser.ID, m)
	a := result // proxy is active+local after import (§8)

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

	caller := setupUser(t, st, "tamper-caller", 0)
	p, tr := beginTestRun(t, st, caller.ID, a)
	// A receipt signed with the wrong key must be rejected: no settlement, trace stays open.
	_, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: remoteUser.ID, ActionName: "tamper-peer/tact", Args: map[string]any{},
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
	setupSys(t, nil, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{}
	k := newTestKernelWithHTTP(st, fake)

	remoteUser, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	bindPetnameForTest(t, k, ctx, base64.RawURLEncoding.EncodeToString(pub), "del-peer")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	m := kernel.ActionManifest{
		ActionID: "del-action-1", OwnerHandle: "del-peer", Name: "dact",
		Kind: kernel.KindHTTP, Price: 0, Description: "d",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-deadbeef", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	msig, _ := kernel.SignManifest(priv, &m)
	m.Signature = msig
	result, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	a := result // proxy is active+local after import (§8)

	caller := setupUser(t, st, "del-caller", 0)
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

	reply, err := k.TestCall(ctx, kernel.TestCallRequest{
		CallerID: caller.ID, ExistingTraceID: tr.ID,
		TargetUserID: remoteUser.ID, ActionName: "del-peer/dact", Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	_ = reply

	// The proxy row vanishes at the store level (e.g. peer-retention purge) — the kernel forbids a
	// manual proxy delete (§8), so model the purge with a direct store delete.
	if err := st.DeleteAction(ctx, a.ID); err != nil {
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
	setupSys(t, k, st)

	// Register a peer so we have a counterpartyID.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := k.EnsureKernelAccount(ctx, pubB64)
	if err != nil {
		t.Fatalf("EnsureKernelAccount: %v", err)
	}

	r, err := k.CreateSignedRejectionReceipt(peer.ID, "owner/some-action", "argsHash123", "idem-key-456", "action inactive", false)
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

	kDisabled := kernel.New(kernel.Dependencies{Store: st, Config: baseCfg(), Logger: log.Default()})
	setupSys(t, kDisabled, st)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	peer, err := kDisabled.EnsureKernelAccount(ctx, pubB64)
	if err != nil {
		t.Fatalf("EnsureKernelAccount: %v", err)
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
	if err := st.UpsertKernel(ctx, pubB64, "old-peer", "", time.Now().UTC()); err != nil {
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
	kEnabled := kernel.New(kernel.Dependencies{Store: st, Config: cfg, Logger: log.Default()})
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
	if kernels, _ := kEnabled.ListKernels(ctx, true, 0, 0); len(kernels) != 0 {
		t.Errorf("ListKernels = %d, want 0 (kernel identity forgotten)", len(kernels))
	}
	u, err := kEnabled.ReadUser(ctx, peer.ID)
	if err != nil {
		t.Fatalf("anchor user row must remain: %v", err)
	}
	if u.KernelPublicKey != "" {
		t.Errorf("public_key must be cleared, got %q", u.KernelPublicKey)
	}
	if dk, _ := kEnabled.ReadKernel(ctx, pubB64); dk != nil {
		t.Error("discovered_kernels row for the purged peer must be deleted")
	}
}

// TestFriendDoesNotReexportImportedProxies pins §13 non-transitivity: a kernel serves manifests and
// gossips only its OWN actions. An imported remote_proxy — even active+public — is never re-served,
// so a peer friending this kernel cannot reach a third kernel's actions through it.
func TestFriendDoesNotReexportImportedProxies(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	// Our own active+public action.
	owner := setupUser(t, st, "localprov", 0)
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
	peerC, err := k.EnsureKernelAccount(ctx, base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	m := kernel.ActionManifest{
		ActionID: "c-act-1", OwnerHandle: "peer-c", Name: "sum", Description: "c sum",
		Kind: kernel.KindHTTP, Price: 50, InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"}, ArtifactHash: "sha256-c", Stats: &kernel.Stats{},
		UpdatedAt: time.Now(),
	}
	sig, _ := kernel.SignManifest(priv, &m)
	m.Signature = sig
	proxy, err := k.ImportPeerAction(ctx, peerC.ID, m)
	if err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}
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
	g, err := k.GetGossip(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var sawOwn, sawProxy bool
	for _, m := range g.ActionManifests {
		sawOwn = sawOwn || m.ActionID == own.ID
		sawProxy = sawProxy || m.ActionID == proxy.ID
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
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})

	_, _, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "nd-action", 1000)
	before, _ := st.ReadUser(ctx, caller.ID)

	_, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{}})
	if !errors.Is(err, kernel.ErrPeerUnreachable) {
		t.Fatalf("Run: expected ErrPeerUnreachable, got %v", err)
	}
	var ke *kernel.KernelError
	if !errors.As(err, &ke) || ke.Meta["peer"] != "settle-peer" {
		t.Errorf("expected Meta[peer]=settle-peer, got %+v", err)
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
	bps := kernel.DefaultConfig().RemoteBPS

	fake := &fakeFederationHTTP{} // first dispatch: offline (no receipt) → parked pending
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})

	_, a, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "retry-nd-action", 1000)
	mp := a.Price * 10000 / (10000 + bps)

	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{}}); !errors.Is(err, kernel.ErrTimeout) {
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

// TestSettleRemoteFailureClassification freezes the §13 outcome classification, which is gated on the
// rejection marker (receipt tx_id == our dispatched idempotency_key, §6 P4) and never on charge or
// transport status alone: a rejection on 402 is our exhausted credit there (ErrPeerUnfunded), any
// other rejection is the peer refusing us (ErrUnauthorized + peer meta), and an EXECUTED failure stays
// ErrExecutionFailed even at charge 0 on transport 402 — the case a charge-based test would misread,
// since the serving kernel returns the execution error's status alongside the real receipt.
//
// The same marker decides the cache (§13 Rule C): a peer that refuses to serve an action at all has
// told us our cached row is wrong, whatever its reason field says, so the row is invalidated and the
// next call re-resolves. Two exemptions: a 402 rejection (our credit is exhausted, the action is
// fine) and an executed failure (the action ran — the cache was right).
func TestSettleRemoteFailureClassification(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		rejection   bool // true → a real signed rejection (tx_id = idempotency_key)
		want        error
		stillCached bool // proxy survives the outcome
	}{
		{"rejection_402_is_peer_unfunded", 402, true, kernel.ErrPeerUnfunded, true},
		{"rejection_403_is_peer_refused", 403, true, kernel.ErrUnauthorized, false},
		{"rejection_422_is_peer_refused", 422, true, kernel.ErrUnauthorized, false},
		{"executed_zero_charge_402_is_execution_failed", 402, false, kernel.ErrExecutionFailed, true},
		{"executed_zero_charge_422_is_execution_failed", 422, false, kernel.ErrExecutionFailed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			fake := &fakeFederationHTTP{httpStatus: tc.status}
			k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "unfunded-action", 1000)
			_, tr := beginTestRun(t, st, caller.ID, a)

			if tc.rejection {
				// The fake signs a rejection exactly as a serving kernel does: tx_id = our key.
				fake.rejectSignKey, fake.rejectActionID = priv, "unfunded-action"
				fake.rejectArgsHash = jcsHashForTest(t, `{}`)
			} else {
				// An ordinary executed failure that consumed nothing: charge 0, but its OWN tx_id.
				now := time.Now().UTC()
				r := &kernel.Receipt{
					ID: uuid.New().String(), TxID: uuid.New().String(), ActionID: "unfunded-action",
					ArgsHash: jcsHashForTest(t, `{}`), Status: kernel.TxFailure, Charge: 0,
					Reason: "execution_failed", StartedAt: now, CreatedAt: now,
				}
				r.Signature = signReceiptForTest(t, priv, r)
				b, _ := json.Marshal(r)
				fake.receiptJSON = string(b)
			}

			_, err := k.TestCall(ctx, kernel.TestCallRequest{
				CallerID: caller.ID, ExistingTraceID: tr.ID,
				ActionRef: "settle-peer@settle-peer/settleact", Args: map[string]any{},
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			// The two peer-attributed classes name the peer so a client can act on it; the execution
			// class must never be attributed to the peer's funding or its refusal.
			if tc.want != kernel.ErrExecutionFailed {
				var ke *kernel.KernelError
				if !errors.As(err, &ke) || ke.Meta["peer"] != "settle-peer" {
					t.Errorf("expected Meta[peer]=settle-peer, got %+v", err)
				}
			} else if errors.Is(err, kernel.ErrPeerUnfunded) || errors.Is(err, kernel.ErrUnauthorized) {
				t.Error("an executed failure must not be reported as a funding or refusal condition")
			}
			// Rule C (§13): a refusal to serve invalidates the cached row; a funding condition and
			// an executed failure leave it alone.
			if ra, _ := st.ReadAction(ctx, a.ID); ra.Active != tc.stillCached {
				t.Errorf("proxy active = %v, want %v", ra.Active, tc.stillCached)
			}
		})
	}
}

// TestGetGossipCounterpartyBalance: gossip reports the requester's credit here only for a known,
// non-suspended key; nil for strangers, suspended keys, and anonymous pulls (§13 peer sync).
func TestGetGossipCounterpartyBalance(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	setupSys(t, k, st)

	sys, _ := st.ReadUserByHandle(context.Background(), "sys")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	friendKey := base64.RawURLEncoding.EncodeToString(pub)
	friend, err := k.EnsureKernelAccount(ctx, friendKey)
	if err != nil {
		t.Fatalf("EnsureKernelAccount: %v", err)
	}
	if _, err := k.Deposit(ctx, sys.ID, friend.ID, 777, "", ""); err != nil {
		t.Fatalf("Deposit: %v", err)
	}

	g, err := k.GetGossip(ctx, friendKey, "")
	if err != nil {
		t.Fatalf("GetGossip: %v", err)
	}
	if g.CounterpartyBalance == nil || *g.CounterpartyBalance != 777 {
		t.Errorf("friend: expected counterparty_balance 777, got %v", g.CounterpartyBalance)
	}

	// Anonymous, stranger, and suspended all omit the field.
	if g, _ := k.GetGossip(ctx, "", ""); g.CounterpartyBalance != nil {
		t.Error("anonymous pull must not carry counterparty_balance")
	}
	strangerPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if g, _ := k.GetGossip(ctx, base64.RawURLEncoding.EncodeToString(strangerPub), ""); g.CounterpartyBalance != nil {
		t.Error("stranger must not carry counterparty_balance")
	}
	if err := k.SuspendUser(ctx, sys.ID, friend.ID); err != nil {
		t.Fatalf("SuspendUser: %v", err)
	}
	if g, _ := k.GetGossip(ctx, friendKey, ""); g.CounterpartyBalance != nil {
		t.Error("suspended peer must not carry counterparty_balance")
	}
}

// TestRecordKernelContact: a successful contact persists last_seen and the reported credit, a failed
// one lands on its own column, and unknown or suspended keys are no-ops (§13 contact cache).
func TestRecordKernelContact(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	k := newTestKernel(st)
	sys := setupSys(t, k, st)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.RawURLEncoding.EncodeToString(pub)
	acct, err := k.EnsureKernelAccount(ctx, key)
	if err != nil {
		t.Fatalf("EnsureKernelAccount: %v", err)
	}

	credit := int64(555)
	if err := k.RecordKernelContact(ctx, key, true, &credit); err != nil {
		t.Fatalf("RecordKernelContact: %v", err)
	}
	got, _ := st.ReadKernel(ctx, key)
	if got.LastSeen == nil {
		t.Error("expected last_seen set after a successful contact")
	}
	if got.PeerCredit == nil || *got.PeerCredit != 555 {
		t.Errorf("expected peer_credit 555, got %v", got.PeerCredit)
	}

	// A nil credit advances last_seen but keeps the prior credit (COALESCE).
	if err := k.RecordKernelContact(ctx, key, true, nil); err != nil {
		t.Fatalf("RecordKernelContact nil: %v", err)
	}
	got, _ = st.ReadKernel(ctx, key)
	if got.PeerCredit == nil || *got.PeerCredit != 555 {
		t.Errorf("nil credit must keep prior 555, got %v", got.PeerCredit)
	}

	// A failure records separately, leaving the success in place for a reader to compare against.
	if err := k.RecordKernelContact(ctx, key, false, nil); err != nil {
		t.Fatalf("RecordKernelContact failure: %v", err)
	}
	got, _ = st.ReadKernel(ctx, key)
	if got.LastContactFailedAt == nil {
		t.Error("expected last_contact_failed_at set after a failed contact")
	}
	if got.LastSeen == nil {
		t.Error("a failure must not clear last_seen")
	}

	// Suspension governs whose requests this kernel answers; reachability is a fact about the network
	// that gates nothing, so it keeps being recorded — a suspended peer the operator can still see is
	// reachable is the honest display, and freezing it would only make the roster lie.
	if err := k.SuspendUser(ctx, sys.ID, acct.ID); err != nil {
		t.Fatalf("SuspendUser: %v", err)
	}
	before, _ := st.ReadKernel(ctx, key)
	if err := k.RecordKernelContact(ctx, key, true, nil); err != nil {
		t.Fatalf("suspended contact: %v", err)
	}
	after, _ := st.ReadKernel(ctx, key)
	if !after.LastSeen.After(*before.LastSeen) {
		t.Errorf("suspended peer's last_seen did not advance: %v → %v", before.LastSeen, after.LastSeen)
	}

	// Unknown key is a no-op (no error).
	strangerPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := k.RecordKernelContact(ctx, base64.RawURLEncoding.EncodeToString(strangerPub), true, &credit); err != nil {
		t.Errorf("unknown key should be a no-op, got %v", err)
	}
}

// Scenario (review finding 1): a federated call parked on a remote dispatch must have its INBOUND
// idempotency record completed by whichever settlement finally resolves it. Before the dispatch
// payload carried the record id, the retry loop settled the money but left the record pending, so
// the requesting peer was answered "duplicate in flight" until expiry and could never learn the
// outcome of work it had paid for.
func TestParkedDispatchCompletesInboundIdempotencyRecordOnRetry(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bps := kernel.DefaultConfig().RemoteBPS

	fake := &fakeFederationHTTP{} // no receipt yet → the dispatch parks
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})

	_, a, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "inbound-rec", 1000)
	mp := a.Price * 10000 / (10000 + bps)

	// An inbound peer's record, exactly as the federation handler inserts before executing.
	rec := &kernel.IdempotencyRecord{
		ID: uuid.New().String(), IdempotencyKey: "inbound-key", CounterpartyUserID: caller.ID,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}
	if err := st.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// Run it as that peer's call: the remote is offline, so the dispatch parks.
	if _, err := k.RunFederated(ctx, caller.ID, a.OwnerUserID, a.Name, map[string]any{}, rec.ID); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("expected ErrTimeout (parked), got %v", err)
	}
	got, err := st.ReadIdempotencyRecord(ctx, "inbound-key", caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Fatalf("record should be pending while the dispatch is parked, got %q", got.Status)
	}

	// The peer returns with a valid signed receipt; the retry loop settles the parked dispatch.
	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx", ActionID: "inbound-rec",
		ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxSuccess, Charge: mp, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)
	k.RetryPendingRemoteDispatches(ctx)

	// The settlement that resolved the dispatch must also have completed the inbound record —
	// otherwise the peer's replay is answered "duplicate in flight" forever.
	got, err = st.ReadIdempotencyRecord(ctx, "inbound-key", caller.ID)
	if err != nil {
		t.Fatalf("ReadIdempotencyRecord after retry: %v", err)
	}
	if got.Status != "complete" {
		t.Errorf("record status = %q, want complete: the peer can never learn the outcome otherwise", got.Status)
	}
	if got.ReceiptJSON == "" {
		t.Error("a completed record must carry the signed receipt the peer settles on")
	}
}

// Scenario (design review): forced closure is also a final settlement for a parked dispatch, so it
// too must complete the inbound record. Without it, an operator ending a process strands the
// requesting peer on "duplicate in flight" until expiry.
func TestEndProcessCompletesInboundIdempotencyRecordOfAParkedDispatch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	fake := &fakeFederationHTTP{} // offline peer → the dispatch parks
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})

	_, a, caller := setupSettleProxyWithKernel(t, st, k, priv, pub, "close-rec", 1000)
	rec := &kernel.IdempotencyRecord{
		ID: uuid.New().String(), IdempotencyKey: "close-key", CounterpartyUserID: caller.ID,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}
	if err := st.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := k.RunFederated(ctx, caller.ID, a.OwnerUserID, a.Name, map[string]any{}, rec.ID); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("expected ErrTimeout (parked), got %v", err)
	}

	procs, _ := st.ListProcesses(ctx, caller.ID, 10, 0)
	if len(procs) != 1 {
		t.Fatalf("expected 1 open process, got %d", len(procs))
	}
	if err := k.EndProcess(ctx, caller.ID, procs[0].ID); err != nil {
		t.Fatalf("EndProcess: %v", err)
	}

	got, err := st.ReadIdempotencyRecord(ctx, "close-key", caller.ID)
	if err != nil {
		t.Fatalf("ReadIdempotencyRecord after closure: %v", err)
	}
	if got.Status != "complete" {
		t.Errorf("record status = %q, want complete after forced closure", got.Status)
	}
}

// Scenario (review finding 2): the inbound record was persisted only inside dispatch_json, which
// is written only for remote proxies — so a crash mid-execution of a federated call to a LOCAL
// action left the record pending for 24h and every peer retry got "duplicate in flight". The
// record now rides on the trace, so crash recovery settles it for every action kind.
func TestCrashRecoveryCompletesInboundRecordForALocalAction(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = testSigningKey()
	k := kernel.New(kernel.Dependencies{Store: st, HTTP: &fakeSuccessHTTP{}, Config: cfg, Logger: log.Default()})

	owner := setupUser(t, st, "local-owner", 0)
	peer := setupUser(t, st, "local-peer", 500)
	// A LOCAL action — no remote proxy, so no dispatch payload is ever written.
	action := setupLocalAction(t, st, owner.ID, "local-act", 0)

	rec := &kernel.IdempotencyRecord{
		ID: uuid.New().String(), IdempotencyKey: "local-key", CounterpartyUserID: peer.ID,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}
	if err := st.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-execution: fund and open the call's trace exactly as beginRun does,
	// then leave it orphaned (no transaction) for Recover to settle.
	p := &kernel.Process{ID: uuid.New().String(), OwnerUserID: peer.ID, Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
	tr := &kernel.Trace{
		ID: uuid.New().String(), ProcessID: p.ID, ActionOwnerID: owner.ID, ActionID: action.ID,
		CallerUserID: peer.ID, IdempotencyRecordID: &rec.ID, CreatedAt: time.Now().UTC(),
	}
	if err := st.BeginRun(ctx, p, tr, peer.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	if err := k.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	got, err := st.ReadIdempotencyRecord(ctx, "local-key", peer.ID)
	if err != nil {
		t.Fatalf("ReadIdempotencyRecord after recovery: %v", err)
	}
	if got.Status != "complete" {
		t.Errorf("record status = %q, want complete: a crashed federated call to a local action "+
			"must not strand its peer until expiry", got.Status)
	}
}

// ---- Discovery gossip (§13) ----

// pexKernel builds a kernel with a known signing key (so the test knows ourKey) and a chosen
// discovery interval.
func pexKernel(st kernel.Store, interval time.Duration) (*kernel.Kernel, string) {
	priv := testSigningKey()
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.FeeRecipientID = testIssuerUserID
	cfg.SigningKey = priv
	cfg.DiscoveryInterval = interval
	k := kernel.New(kernel.Dependencies{Store: st, Config: cfg, Logger: log.Default()})
	return k, base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

func pexKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(pub)
}

// Gossip carries no membership, and serving a pull provisions nothing (§13): discovery of which
// kernels exist is routing discovery's job, so a puller is neither learned as a discovered kernel
// nor given an account by the act of pulling.
func TestGossipNoMembershipNoProvision(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	k, _ := pexKernel(st, 0)
	setupSys(t, k, st)
	req := pexKey(t)
	if _, err := k.GetGossip(ctx, req, ""); err != nil {
		t.Fatal(err)
	}
	if dk, _ := st.ReadKernel(ctx, req); dk != nil {
		t.Error("serving a gossip pull learned the requester as a discovered kernel; membership is routing discovery's job, not gossip's")
	}
	if u, _ := st.ReadAccountByKernelKey(ctx, req); u != nil {
		t.Error("serving a gossip pull provisioned a user account (no billing relationship from gossip)")
	}
}

// AccumulateGossip enforces the authenticated-key binding and a valid bare identity (§13): a reply
// claiming a key other than the one dialed, or carrying an invalid handle, is a failed pull, never
// verified; a valid pull refreshes the discovered-kernel row.
func TestAccumulateGossipBinding(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	k, _ := pexKernel(st, 0)
	setupSys(t, k, st)
	sender := pexKey(t)

	// binding: a reply claiming an identity other than the authenticated key is rejected.
	if _, err := k.AccumulateGossip(ctx, &kernel.GossipResponse{PublicKey: sender, Handle: "ok"}, pexKey(t)); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("introducer mismatch: got %v, want ErrInvalidInput", err)
	}
	// invalid identity: empty and non-bare handles are failed pulls, not verified ones.
	for _, bad := range []string{"", "a/b", "a@b"} {
		if _, err := k.AccumulateGossip(ctx, &kernel.GossipResponse{PublicKey: sender, Handle: bad}, sender); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("handle %q: got %v, want ErrInvalidInput", bad, err)
		}
	}
	// a valid pull refreshes the discovered-kernel row (verified).
	if _, err := k.AccumulateGossip(ctx, &kernel.GossipResponse{PublicKey: sender, Handle: "sendername"}, sender); err != nil {
		t.Fatal(err)
	}
	if dk, _ := st.ReadKernel(ctx, sender); dk == nil || dk.Nickname != "sendername" {
		t.Error("a verified pull did not refresh the discovered-kernel row")
	}
}

// mountKernelForTest performs the outbound-use composite of the kernel lifecycle (§13): observe,
// bind a petname, and open the billing account — what a verified action or user resolve does. Tests
// that only need one of the three call it directly instead.
func mountKernelForTest(t *testing.T, k *kernel.Kernel, ctx context.Context, publicKey, petname string) (*kernel.Account, error) {
	t.Helper()
	if _, err := k.BindPetname(ctx, publicKey, petname, false); err != nil {
		return nil, err
	}
	return k.EnsureKernelAccount(ctx, publicKey)
}

// bindPetnameForTest binds a kernel's local petname, the naming half of the outbound-use lifecycle
// (§13) — what a verified resolve does before the proxy is addressed as owner@petname/name.
func bindPetnameForTest(t *testing.T, k *kernel.Kernel, ctx context.Context, publicKey, petname string) {
	t.Helper()
	if _, err := k.BindPetname(ctx, publicKey, petname, false); err != nil {
		t.Fatalf("bind petname %s: %v", petname, err)
	}
}

// ---- Kernel lifecycle and Stiegler naming (§13) ----

// TestKernelLifecycleSeparation pins the three operations apart: observation names nothing and funds
// nothing, provisioning opens an account without naming, and only our own outbound act binds a
// petname. That separation is what keeps a nickname unable to capture a local name.
func TestKernelLifecycleSeparation(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)
	key := testKernelKey(1)

	// 1. Observe: a kernel row, no petname, no account.
	if err := k.ObserveKernel(ctx, key, "acme", "a kernel"); err != nil {
		t.Fatalf("ObserveKernel: %v", err)
	}
	rk, err := k.ReadKernel(ctx, key)
	if err != nil || rk == nil {
		t.Fatalf("ReadKernel: %v", err)
	}
	if rk.Nickname != "acme" || rk.Petname != "" {
		t.Errorf("after observe: nickname=%q petname=%q, want acme and unbound", rk.Nickname, rk.Petname)
	}
	if acct, _ := k.ReadAccountByKernelKey(ctx, key); acct != nil {
		t.Error("observation must not open an account")
	}

	// 2. Ensure account: an account, still no petname, and learned metadata is preserved.
	if _, err := k.EnsureKernelAccount(ctx, key); err != nil {
		t.Fatalf("EnsureKernelAccount: %v", err)
	}
	rk, _ = k.ReadKernel(ctx, key)
	if rk.Petname != "" {
		t.Errorf("provisioning must not bind a petname, got %q", rk.Petname)
	}
	if rk.Nickname != "acme" || rk.About != "a kernel" {
		t.Errorf("provisioning must preserve observation: nickname=%q about=%q", rk.Nickname, rk.About)
	}

	// 3. Bind: automatic binding seeds from the cached nickname.
	got, err := k.BindPetname(ctx, key, "", false)
	if err != nil {
		t.Fatalf("BindPetname: %v", err)
	}
	if got != "acme" {
		t.Errorf("automatic bind seeded %q, want the cached nickname acme", got)
	}

	// A later nickname change never retargets the bound petname (§13).
	if err := k.ObserveKernel(ctx, key, "renamed", ""); err != nil {
		t.Fatal(err)
	}
	if rk, _ = k.ReadKernel(ctx, key); rk.Petname != "acme" || rk.Nickname != "renamed" {
		t.Errorf("nickname change retargeted the petname: petname=%q nickname=%q", rk.Petname, rk.Nickname)
	}
}

// TestBindPetnameFallsBackToMechanical: with no usable nickname the automatic seed is k-<key8>, so a
// cold call by raw key always ends up with something addressable.
func TestBindPetnameFallsBackToMechanical(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	for _, tc := range []struct{ name, nickname string }{
		{"absent", ""},
		{"key-shaped", testKernelKey(9)},
		{"contains slash", "bad/name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := testKernelKey(20 + len(tc.name))
			if err := k.ObserveKernel(ctx, key, tc.nickname, ""); err != nil {
				t.Fatal(err)
			}
			got, err := k.BindPetname(ctx, key, "", false)
			if err != nil {
				t.Fatalf("BindPetname: %v", err)
			}
			if want := "k-" + key[:8]; got != want {
				t.Errorf("petname = %q, want the mechanical %q", got, want)
			}
		})
	}
}

// TestBindPetnameExactRejectsCollision: an operator bind is exact. Silently suffixing an explicit
// request would name a different kernel than the operator asked for.
func TestBindPetnameExactRejectsCollision(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)
	first, second := testKernelKey(3), testKernelKey(4)

	if _, err := k.BindPetname(ctx, first, "taken", true); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if _, err := k.BindPetname(ctx, second, "taken", true); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("explicit collision: want ErrInvalidInput, got %v", err)
	}
	// Automatic binding of the same seed still suffixes rather than failing.
	got, err := k.BindPetname(ctx, second, "taken", false)
	if err != nil || got != "taken-2" {
		t.Errorf("automatic bind = %q (%v), want taken-2", got, err)
	}
	// A petname may never take a key or id shape, or the resolver's disjointness collapses (§14).
	for _, bad := range []string{testKernelKey(5), uuid.New().String(), "with/slash", "with@at"} {
		if _, err := k.BindPetname(ctx, testKernelKey(6), bad, true); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("BindPetname(%q): want ErrInvalidInput, got %v", bad, err)
		}
	}
}

// TestHandleAndPetnameShareOneString: the two namespaces are independent, which is the point of the
// split — a user and a kernel may both be called minibox locally.
func TestHandleAndPetnameShareOneString(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "minibox", Password: "password123"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	key := testKernelKey(7)
	if got, err := k.BindPetname(ctx, key, "minibox", true); err != nil || got != "minibox" {
		t.Fatalf("petname bind alongside an identical handle: got %q, %v", got, err)
	}
	u, err := k.ResolveUser(ctx, "minibox")
	if err != nil || u.KernelPublicKey != "" {
		t.Errorf("the user namespace must still resolve to the local user, got %v (%v)", u, err)
	}
	rk, err := k.ReadKernelByPetname(ctx, "minibox")
	if err != nil || rk == nil || rk.PublicKey != key {
		t.Errorf("the kernel namespace must resolve to the kernel, got %v (%v)", rk, err)
	}
}

// TestRenameUserRejectsKernelAccountByID: a kernel account holds no handle, so renaming it by its
// account id would name the wrong entity; the operator is pointed at the kernel namespace.
func TestRenameUserRejectsKernelAccountByID(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	acct, err := k.EnsureKernelAccount(ctx, testKernelKey(8))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.RenameUser(ctx, sys.ID, acct.ID, "newname"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("rename kernel account by id: want ErrInvalidInput, got %v", err)
	}
}

// TestKernelMountIdempotentUnderConcurrency: concurrent first use of one key converges — one
// account, one petname — and concurrent binds of one seed across distinct keys stay distinct.
func TestKernelMountIdempotentUnderConcurrency(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)
	key := testKernelKey(10)

	const n = 8
	ids := make([]string, n)
	names := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if name, err := k.BindPetname(ctx, key, "converge", false); err == nil {
				names[i] = name
			}
			if acct, err := k.EnsureKernelAccount(ctx, key); err == nil {
				ids[i] = acct.ID
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if ids[i] != ids[0] || ids[i] == "" {
			t.Fatalf("account %d = %q, want the single winner %q", i, ids[i], ids[0])
		}
		if names[i] != names[0] || names[i] == "" {
			t.Fatalf("petname %d = %q, want the single winner %q", i, names[i], names[0])
		}
	}

	// Distinct keys racing for one seed each get a distinct petname.
	var wg2 sync.WaitGroup
	got := make([]string, n)
	for i := 0; i < n; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			got[i], _ = k.BindPetname(ctx, testKernelKey(100+i), "rival", false)
		}(i)
	}
	wg2.Wait()
	seen := map[string]bool{}
	for i, name := range got {
		if name == "" {
			t.Fatalf("bind %d produced no petname", i)
		}
		if seen[name] {
			t.Fatalf("petname %q was bound twice", name)
		}
		seen[name] = true
	}
}

// TestSuspendKernelProvisionsAtomically: a kernel can be frozen before it ever calls, and the
// provisioning and the freeze land together — no window in which an inbound call sees it active.
func TestSuspendKernelProvisionsAtomically(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)
	key := testKernelKey(11)

	if err := k.SuspendKernel(ctx, sys.ID, key); err != nil {
		t.Fatalf("SuspendKernel: %v", err)
	}
	acct, err := k.ReadAccountByKernelKey(ctx, key)
	if err != nil || acct == nil {
		t.Fatalf("suspend must provision the account: %v", err)
	}
	if acct.SuspendedAt == nil {
		t.Error("the provisioned account must already be suspended")
	}
	// The next inbound call cannot arrive as a fresh unsuspended account.
	again, err := k.EnsureKernelAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != acct.ID || again.SuspendedAt == nil {
		t.Errorf("EnsureKernelAccount created a second, unsuspended account: %+v", again)
	}
	// Suspension is not our act of naming.
	if rk, _ := k.ReadKernel(ctx, key); rk == nil || rk.Petname != "" {
		t.Errorf("suspend must bind no petname, got %+v", rk)
	}
}

// testKernelKey builds a distinct, well-formed base64url Ed25519 public key for test n.
func testKernelKey(n int) string {
	b := bytes.Repeat([]byte{byte(n)}, ed25519.PublicKeySize)
	return base64.RawURLEncoding.EncodeToString(b)
}

// TestResolvePrincipalRefusesNamelessAccounts: /juice/fed/resolve/1 answers with a principal a peer
// will address by name, so an id landing on a kernel account or a purged tombstone — neither of
// which has a handle — must not resolve (§13).
func TestResolvePrincipalRefusesNamelessAccounts(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	acct, err := k.EnsureKernelAccount(ctx, testKernelKey(31))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.ResolvePrincipal(ctx, acct.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("kernel account by id: want ErrNotFound, got %v", err)
	}
	// A live local user still resolves, by handle and by id.
	u, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "resolvable", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"resolvable", u.ID} {
		id, handle, err := k.ResolvePrincipal(ctx, ref)
		if err != nil || id != u.ID || handle != "resolvable" {
			t.Errorf("ResolvePrincipal(%q) = %s/%s (%v), want the live user", ref, id, handle, err)
		}
	}
}

// TestTombstoneIsNeverALiveTarget: a purged peer keeps a resolvable id as the ledger anchor (§13
// Retention), and every mutating path must test for a *live user* rather than infer one from "not a
// peer" — otherwise a rename would hand a purged peer's history a fresh handle.
func TestTombstoneIsNeverALiveTarget(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)

	acct, err := k.EnsureKernelAccount(ctx, testKernelKey(41))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PurgePeerCascade(ctx, acct.ID); err != nil {
		t.Fatalf("PurgePeerCascade: %v", err)
	}
	tomb, err := k.ReadUser(ctx, acct.ID)
	if err != nil || tomb == nil {
		t.Fatalf("the anchor must stay readable: %v", err)
	}
	if tomb.IsLiveUser() || tomb.IsPeer() {
		t.Fatalf("a tombstone is neither a live user nor a peer, got %+v", tomb)
	}

	payer, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "payer", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Deposit(ctx, sys.ID, payer.ID, 100, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := k.RenameUser(ctx, sys.ID, tomb.ID, "resurrected"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("rename a tombstone: want ErrInvalidInput, got %v", err)
	}
	if _, err := k.Transfer(ctx, payer.ID, tomb.ID, 10, "", ""); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("transfer to a tombstone: want ErrInvalidInput, got %v", err)
	}
	// The kernel enforces it too, not only the HTTP resolver: supervision cannot fund or freeze a
	// tombstone, and a step parked on one would hold its price with no actor able to free it (§10).
	if _, err := k.Deposit(ctx, sys.ID, tomb.ID, 10, "", ""); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("deposit to a tombstone: want ErrNotFound, got %v", err)
	}
	if _, err := k.Withdraw(ctx, sys.ID, tomb.ID, 10, "", ""); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("withdraw from a tombstone: want ErrNotFound, got %v", err)
	}
	if err := k.SuspendUser(ctx, sys.ID, tomb.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("suspend a tombstone: want ErrNotFound, got %v", err)
	}
	if _, _, err := k.ResolveRequiredCaller(ctx, tomb.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("park a step on a tombstone: want ErrNotFound, got %v", err)
	}
}

// TestManifestMonetaryBoundsRejected: a signature proves authorship, not sanity. A signed manifest
// carrying a negative price or an out-of-range remote_bps is refused at the authoritative import
// path and skipped at gossip ingest (§8, §13), so peer-supplied numbers never reach the proxy price
// or the discovery cache.
func TestManifestMonetaryBoundsRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	peerKey := base64.RawURLEncoding.EncodeToString(pub)
	remoteUser, err := k.EnsureKernelAccount(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}

	signed := func(id string, price, rbps int64) *kernel.ActionManifest {
		m := &kernel.ActionManifest{
			ActionID: id, OwnerID: "u1", OwnerHandle: "prov", Name: id,
			Description: "a priced remote action", Kind: kernel.KindHTTP,
			Price: price, RemoteBPS: rbps,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			Stats: &kernel.Stats{}, UpdatedAt: time.Now().UTC(),
		}
		sig, serr := kernel.SignManifest(priv, m)
		if serr != nil {
			t.Fatal(serr)
		}
		m.Signature = sig
		return m
	}

	bad := map[string]*kernel.ActionManifest{
		"negative-price":   signed("negative-price", -1, 500),
		"negative-bps":     signed("negative-bps", 100, -1),
		"out-of-range-bps": signed("out-of-range-bps", 100, 10001),
	}
	for name, m := range bad {
		if _, err := k.ImportPeerAction(ctx, remoteUser.ID, *m); err == nil {
			t.Errorf("%s: authoritative import must refuse an out-of-range manifest", name)
		}
	}

	// Gossip ingest skips the same manifests while indexing a sound one alongside them.
	good := signed("sound", 100, 500)
	g := &kernel.GossipResponse{
		PublicKey: peerKey, Handle: "peerk",
		ActionManifests: []*kernel.ActionManifest{good, bad["negative-price"], bad["negative-bps"], bad["out-of-range-bps"]},
	}
	if _, err := k.AccumulateGossip(ctx, g, peerKey); err != nil {
		t.Fatalf("AccumulateGossip: %v", err)
	}
	docs, err := k.DiscoveryDocsForKernel(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].ActionID != "sound" {
		t.Fatalf("only the sound manifest may be indexed, got %d docs: %+v", len(docs), docs)
	}
	// sr = 100 + ceil(100*500/10000) = 105.
	if docs[0].ServingPrice != 105 {
		t.Errorf("serving_price = %d, want 105", docs[0].ServingPrice)
	}
}

// TestProxyRepricesOnImportBPSChange: an imported action is a CATALOG entry, so its total is derived
// from the seller's stored price and the CURRENT import fee (§8, §16). Changing local policy reprices
// it everywhere at once — with no re-resolve, no manifest change, and no peer contact.
func TestProxyRepricesOnImportBPSChange(t *testing.T) {
	st := newTestStore(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	m := kernel.ActionManifest{
		ActionID: "ra-price", OwnerID: "remote-bob", OwnerHandle: "bob", Name: "greet",
		RemoteBPS: 500, Description: "greet", Kind: kernel.KindHTTP, Price: 100,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	kernelAt := func(importBPS int64) *kernel.Kernel {
		cfg := kernel.DefaultConfig()
		cfg.TokenSecret = "test-secret"
		cfg.IssuerUserID = testIssuerUserID
		cfg.FeeRecipientID = testIssuerUserID
		cfg.SigningKey = testSigningKey()
		cfg.ImportBPS = importBPS
		return kernel.New(kernel.Dependencies{Store: st, HTTP: &fakeFederationHTTP{resolveManifest: &m}, Federation: &fakeFederationHTTP{resolveManifest: &m}, Config: cfg, Logger: log.Default()})
	}
	ctx := context.Background()

	// mp=100, remote_bps=500 → sr=105. At import_bps=500 the total is 105+6=111.
	a, err := kernelAt(500).ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil {
		t.Fatalf("cold resolve: %v", err)
	}
	if a.Price != 111 || a.BasePrice == nil || *a.BasePrice != 100 {
		t.Fatalf("import: price=%d base=%v, want 111 and 100", a.Price, a.BasePrice)
	}

	// A second kernel over the SAME store at 2000 bps: 105 + ceil(105*2000/10000) = 105+21 = 126.
	// No re-resolve happens — the manifest is unchanged and the row is already active.
	reader := kernelAt(2000)
	got, err := reader.ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.ID != a.ID {
		t.Fatalf("expected the same cached row, got %s want %s", got.ID, a.ID)
	}
	if got.Price != 126 {
		t.Errorf("repriced total = %d, want 126", got.Price)
	}
	// Every read boundary agrees, not just the resolver.
	byID, err := reader.ReadAction(ctx, a.ID)
	if err != nil || byID.Price != 126 {
		t.Errorf("ReadAction price = %d (err %v), want 126", byID.Price, err)
	}
	// The stored seller price is untouched by any read.
	if byID.BasePrice == nil || *byID.BasePrice != 100 {
		t.Errorf("base price must not move: %v", byID.BasePrice)
	}
}

// TestLegacyProxyHealsOnNextFundedUse: a row imported before the seller's price was stored has its
// total frozen and cannot be re-derived. It re-resolves once, acquiring an exact price from the
// signed manifest — even though the contract hash is UNCHANGED, the case reconcileImport skips.
func TestLegacyProxyHealsOnNextFundedUse(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	m := kernel.ActionManifest{
		ActionID: "ra-legacy", OwnerID: "remote-bob", OwnerHandle: "bob", Name: "greet",
		RemoteBPS: 500, Description: "greet", Kind: kernel.KindHTTP, Price: 100,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	fake := &fakeFederationHTTP{resolveManifest: &m}
	kf := newTestKernelWithHTTP(st, fake)
	a, err := kf.ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-041 shape: an active row with the total stored but no seller price.
	a.BasePrice = nil
	if err := st.UpdateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	legacy, _ := st.ReadAction(ctx, a.ID)
	if legacy.BasePrice != nil || !legacy.Active {
		t.Fatalf("setup: want an active row with no base price, got %+v", legacy)
	}

	// The contract is unchanged, so reconcile lands in Unchanged — the path that used to write
	// nothing. The row must still come back with an exact seller price, id and active state intact.
	healed, err := kf.ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil {
		t.Fatalf("healing resolve: %v", err)
	}
	if healed.ID != a.ID || !healed.Active {
		t.Errorf("healing must preserve id and active state: %+v", healed)
	}
	if healed.BasePrice == nil || *healed.BasePrice != 100 {
		t.Fatalf("base price after heal = %v, want 100", healed.BasePrice)
	}
}

// TestSettlementUsesDispatchedRate: the funding boundary freezes the rate, so a call dispatched at
// one import fee settles at THAT fee even if local policy moves while it is in flight (§16). Without
// the snapshot a parked call would refund against a rate it was never locked at, and the caller's
// balance would not reconcile.
func TestSettlementUsesDispatchedRate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeFederationHTTP{} // no receipt → the peer is offline and the call parks

	// Dispatch under the default 500 bps: mp=1000 → sr=1050 → q=1103.
	k, a, caller := setupSettleProxy(t, st, fake, priv, pub, "ra-rate", 1000)
	if a.Price != 1103 {
		t.Fatalf("setup price = %d, want 1103", a.Price)
	}
	before, _ := st.ReadUser(ctx, caller.ID)

	if _, err := k.Run(ctx, kernel.RunRequest{CallerID: caller.ID, ActionRef: a.ID, Args: map[string]any{}}); !errors.Is(err, kernel.ErrTimeout) {
		t.Fatalf("Run: expected ErrTimeout (parked), got %v", err)
	}
	pend, _ := st.ListPendingRemoteTraces(ctx)
	if len(pend) != 1 {
		t.Fatalf("expected 1 parked trace, got %d", len(pend))
	}
	var d struct {
		Gross     int64  `json:"gross"`
		ImportBPS *int64 `json:"import_bps"`
		RemoteBPS *int64 `json:"remote_bps"`
	}
	if err := json.Unmarshal([]byte(*pend[0].DispatchJSON), &d); err != nil {
		t.Fatal(err)
	}
	if d.Gross != 1103 {
		t.Errorf("dispatch gross = %d, want the locked 1103", d.Gross)
	}
	if d.ImportBPS == nil || *d.ImportBPS != 500 || d.RemoteBPS == nil || *d.RemoteBPS != 500 {
		t.Fatalf("dispatch must freeze both rates, got import=%v remote=%v", d.ImportBPS, d.RemoteBPS)
	}

	// The operator now quadruples the import fee. The catalog reprices; this in-flight call must not.
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret, cfg.IssuerUserID, cfg.FeeRecipientID = "test-secret", testIssuerUserID, testIssuerUserID
	cfg.SigningKey, cfg.ImportBPS = testSigningKey(), 2000
	repriced := kernel.New(kernel.Dependencies{Store: st, HTTP: fake, Federation: fake, Config: cfg, Logger: log.Default()})
	if got, _ := repriced.ReadAction(ctx, a.ID); got.Price != 1260 { // 1050 + ceil(1050*2000/10000)
		t.Fatalf("catalog price after the change = %d, want 1260", got.Price)
	}

	// The peer returns and FAILS the call at zero charge: the full 1103 must come back, restoring
	// the caller's original balance exactly. Refunding against 1281 would mint credits.
	now := time.Now().UTC()
	r := &kernel.Receipt{
		ID: uuid.New().String(), TxID: "rtx-rate", ActionID: "ra-rate",
		ArgsHash: jcsHashForTest(t, `{}`), ReplyHash: jcsHashForTest(t, `{}`),
		Status: kernel.TxFailure, Charge: 0, Premium: 0, StartedAt: now, CreatedAt: now,
	}
	r.Signature = signReceiptForTest(t, priv, r)
	b, _ := json.Marshal(r)
	fake.receiptJSON = string(b)
	repriced.RetryPendingRemoteDispatches(ctx)

	after, _ := st.ReadUser(ctx, caller.ID)
	if after.Available != before.Available || after.Locked != 0 {
		t.Errorf("after refund: available=%d locked=%d, want available=%d locked=0",
			after.Available, after.Locked, before.Available)
	}
}

// TestEveryReadPathReprices: pricing a proxy is not any one caller's job — the derivation lives on
// the store handle, so EVERY read carries it (§16). This pins that: the same repriced row must read
// identically through the id path, the reference path, the listing, and sys/lookup. A caller that
// bypassed the boundary would show the stale total here.
func TestEveryReadPathReprices(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	m := kernel.ActionManifest{
		ActionID: "ra-paths", OwnerID: "remote-bob", OwnerHandle: "bob", Name: "greet",
		RemoteBPS: 500, Description: "greet a person warmly", Kind: kernel.KindHTTP, Price: 100,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	m.Signature, _ = kernel.SignManifest(priv, &m)

	at := func(importBPS int64) *kernel.Kernel {
		cfg := kernel.DefaultConfig()
		cfg.TokenSecret, cfg.IssuerUserID, cfg.FeeRecipientID = "test-secret", testIssuerUserID, testIssuerUserID
		cfg.SigningKey, cfg.ImportBPS = testSigningKey(), importBPS
		return kernel.New(kernel.Dependencies{Store: st, HTTP: &fakeFederationHTTP{resolveManifest: &m}, Federation: &fakeFederationHTTP{resolveManifest: &m}, Embedder: &fakeEmbedder{}, Config: cfg, Logger: log.Default()})
	}
	a, err := at(500).ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil {
		t.Fatal(err)
	}
	caller := setupUser(t, st, "paths-caller", 0)

	// sr = 105; at 2000 bps the total is 105 + ceil(105*2000/10000) = 126.
	k := at(2000)
	const want = int64(126)

	byID, err := k.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	byRef, err := k.ResolveAction(ctx, "bob@"+pubB64+"/greet")
	if err != nil {
		t.Fatal(err)
	}
	byRawID, err := k.ResolveAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	forSubject, err := k.ReadActionForSubject(ctx, caller.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]*kernel.Action{
		"ReadAction": byID, "ResolveAction(ref)": byRef,
		"ResolveAction(id)": byRawID, "ReadActionForSubject": forSubject,
	} {
		if got.Price != want {
			t.Errorf("%s price = %d, want %d", name, got.Price, want)
		}
	}
	listed, err := k.ListOwnedActions(ctx, a.OwnerUserID, 50, 0)
	if err != nil || len(listed) == 0 {
		t.Fatalf("listing: %d rows, err %v", len(listed), err)
	}
	for _, got := range listed {
		if got.ID == a.ID && got.Price != want {
			t.Errorf("ListOwnedActions price = %d, want %d", got.Price, want)
		}
	}

	// sys/lookup reads through the same boundary, so it needs no pricing logic of its own.
	res, err := k.Lookup(ctx, kernel.LookupRequest{Query: "greet warmly", Limit: 10, CallerID: caller.ID})
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, r := range res {
		if r.Action != nil && r.Action.ID == a.ID {
			seen = true
			if r.Price != want {
				t.Errorf("lookup price = %d, want %d", r.Price, want)
			}
		}
	}
	if !seen {
		t.Error("the resolved proxy must appear in lookup (it is indexed at resolve)")
	}
}

// TestDiscoveredQuoteHashMatchesProxy proves the §15 invariant that a discovered catalog hit
// and the proxy it resolves to carry the SAME quote hash: a buyer who pins a hash read from
// sys/lookup must be able to run the action without the pin being refused as changed terms.
// The two hashes are produced by different code paths (gossip ingest → discovery doc → indicative
// pricing, versus signed manifest → import → stored proxy row), so nothing but this test keeps
// them equal. Exercised through public behaviour only.
func TestDiscoveredQuoteHashMatchesProxy(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{})
	ctx := context.Background()
	setupSys(t, k, st)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	peerKey := base64.RawURLEncoding.EncodeToString(pub)

	m := kernel.ActionManifest{
		ActionID: "quote-parity-action", OwnerID: "remote-owner-id", OwnerHandle: "carol",
		Name: "forecast", Description: "distinctive barometric forecasting service",
		Kind: kernel.KindHTTP, Price: 100, RemoteBPS: 500,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "sha256-parity", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
	}
	sig, err := kernel.SignManifest(priv, &m)
	if err != nil {
		t.Fatal(err)
	}
	m.Signature = sig

	// Leg 1: learn it from gossip, then read the hash lookup would show a local caller.
	if _, err := k.AccumulateGossip(ctx, &kernel.GossipResponse{
		PublicKey: peerKey, Handle: "carolkernel", ActionManifests: []*kernel.ActionManifest{&m},
	}, ""); err != nil {
		t.Fatalf("AccumulateGossip: %v", err)
	}
	buyer, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "quote-buyer", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "distinctive barometric forecasting service", Limit: 10, CallerID: buyer.ID})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	var discovered *kernel.LookupResult
	for _, r := range results {
		if r.Discovered != nil && r.Discovered.ActionID == m.ActionID {
			discovered = r
			break
		}
	}
	if discovered == nil {
		t.Fatal("gossiped action did not surface as a discovered lookup hit")
	}
	if discovered.QuoteHash == "" {
		t.Fatal("a discovered hit must carry a quote hash a buyer can pin")
	}

	// Leg 2: resolve the same manifest into a local proxy row.
	remoteUser, err := k.EnsureKernelAccount(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := k.ImportPeerAction(ctx, remoteUser.ID, m)
	if err != nil {
		t.Fatalf("ImportPeerAction: %v", err)
	}

	if got, want := kernel.QuoteHash(proxy), discovered.QuoteHash; got != want {
		t.Errorf("quote hash differs between catalog and resolved proxy\n proxy      %s\n discovered %s\nprices: proxy=%d discovered=%d", got, want, proxy.Price, discovered.Price)
	}
}

// TestResolveRemoteApplicationRoot: a kernel-qualified application root costs one resolve round
// trip — the peer applies the index convention and returns the index manifest, cached as
// owner/name/index — and none thereafter; a root reference is requested with an empty action name.
func TestResolveRemoteApplicationRoot(t *testing.T) {
	st := newTestStore(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	sign := func(m *kernel.ActionManifest) *kernel.ActionManifest {
		sig, err := kernel.SignManifest(priv, m)
		if err != nil {
			t.Fatal(err)
		}
		m.Signature = sig
		return m
	}
	manifest := func(name string) *kernel.ActionManifest {
		return sign(&kernel.ActionManifest{
			ActionID: "ra-" + name, OwnerID: "remote-bob-id", OwnerHandle: "bob", Name: name,
			RemoteBPS: 500, Description: "d", Kind: kernel.KindHTTP, Price: 100,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
		})
	}

	// The peer answers a request for the group with the group's index action.
	fake := &fakeFederationHTTP{resolveManifest: manifest("mail/index")}
	k := newTestKernelWithHTTP(st, fake)
	ctx := context.Background()

	a, err := k.ResolveAction(ctx, "bob@"+pubB64+"/mail")
	if err != nil {
		t.Fatalf("resolve application root: %v", err)
	}
	if a.Name != "bob/mail/index" {
		t.Errorf("proxy row name: got %q, want bob/mail/index", a.Name)
	}
	if len(fake.resolvedRefs) != 1 || fake.resolvedRefs[0] != "bob/mail" {
		t.Errorf("one request carrying the reference as written: got %v", fake.resolvedRefs)
	}
	// The cached row answers the same reference thereafter, with no further request.
	fake.resolvedRefs = nil
	a2, err := k.ResolveAction(ctx, "bob@"+pubB64+"/mail")
	if err != nil || a2.ID != a.ID {
		t.Fatalf("cache hit: err=%v id=%v want %v", err, a2.ID, a.ID)
	}
	if len(fake.resolvedRefs) != 0 {
		t.Errorf("cached root must not dial: got %v", fake.resolvedRefs)
	}

	// An owner root (no action name at all) is requested with an empty name; the peer's resolver
	// answers with that owner's index.
	st2 := newTestStore(t)
	fake2 := &fakeFederationHTTP{resolveManifest: manifest("index")}
	k2 := newTestKernelWithHTTP(st2, fake2)
	if _, err := k2.ResolveAction(ctx, "bob@"+pubB64); err != nil {
		t.Fatalf("resolve owner root: %v", err)
	}
	if len(fake2.resolvedRefs) != 1 || fake2.resolvedRefs[0] != "bob/" {
		t.Errorf("root request must carry an empty action name: got %v", fake2.resolvedRefs)
	}

	// A peer that is unreachable reports exactly that, and never as a miss.
	st3 := newTestStore(t)
	fake3 := &fakeFederationHTTP{resolveErr: kernel.ErrPeerUnreachable.Wrap("offline")}
	k3 := newTestKernelWithHTTP(st3, fake3)
	if _, err := k3.ResolveAction(ctx, "bob@"+pubB64+"/mail"); !errors.Is(err, kernel.ErrPeerUnreachable) {
		t.Errorf("want ErrPeerUnreachable, got %v", err)
	}
}

// TestResolveRemoteManifestBoundToRequest: a resolve answer must answer the reference requested —
// same owner, and either the action named or its index child — else it is refused before anything
// is cached, bound, or provisioned. Otherwise a peer could serve any signed action of its own and
// have it executed under the reference the caller typed.
func TestResolveRemoteManifestBoundToRequest(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	manifest := func(owner, name string) *kernel.ActionManifest {
		m := &kernel.ActionManifest{
			ActionID: "ra-x", OwnerID: "remote-id", OwnerHandle: owner, Name: name,
			RemoteBPS: 500, Description: "d", Kind: kernel.KindHTTP, Price: 100,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
		}
		sig, err := kernel.SignManifest(priv, m)
		if err != nil {
			t.Fatal(err)
		}
		m.Signature = sig
		return m
	}

	for _, tc := range []struct {
		label string
		ref   string
		m     *kernel.ActionManifest
	}{
		{"another owner", "bob@" + pubB64 + "/mail", manifest("carol", "mail")},
		{"another action", "bob@" + pubB64 + "/mail", manifest("bob", "other")},
		{"another group's index", "bob@" + pubB64 + "/mail", manifest("bob", "other/index")},
		{"root answered by a named action", "bob@" + pubB64, manifest("bob", "mail")},
	} {
		st := newTestStore(t)
		fake := &fakeFederationHTTP{resolveManifest: tc.m}
		k := newTestKernelWithHTTP(st, fake)
		ctx := context.Background()

		if _, err := k.ResolveAction(ctx, tc.ref); !errors.Is(err, kernel.ErrUnauthorized) {
			t.Errorf("%s: want ErrUnauthorized, got %v", tc.label, err)
		}
		// Nothing is kept from a reply that did not answer the request.
		acts, err := st.ListAllActions(ctx, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range acts {
			if a.Kind == kernel.KindRemoteProxy {
				t.Errorf("%s: cached a proxy for a mismatched reply", tc.label)
			}
		}
		if acct, _ := st.ReadAccountByKernelKey(ctx, pubB64); acct != nil {
			t.Errorf("%s: provisioned an account for a mismatched reply", tc.label)
		}
	}
}

// TestStaleExactProxyReResolvesBeforeIndex: a cached proxy that exists but cannot be served is a
// cache miss to re-resolve, not an absence to walk past — otherwise an inactive exact proxy would
// silently degrade to a cached index while the exact action is still live on the peer (§13 rule A).
func TestStaleExactProxyReResolvesBeforeIndex(t *testing.T) {
	st := newTestStore(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	manifest := func(name string) *kernel.ActionManifest {
		m := &kernel.ActionManifest{
			ActionID: "ra-" + name, OwnerID: "remote-bob-id", OwnerHandle: "bob", Name: name,
			RemoteBPS: 500, Description: "d", Kind: kernel.KindHTTP, Price: 100,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			ArtifactHash: "h", Stats: &kernel.Stats{}, UpdatedAt: time.Now(),
		}
		sig, err := kernel.SignManifest(priv, m)
		if err != nil {
			t.Fatal(err)
		}
		m.Signature = sig
		return m
	}
	fake := &fakeFederationHTTP{}
	k := newTestKernelWithHTTP(st, fake)
	ctx := context.Background()

	// Cache both the exact action and the group's index, each by its own reference.
	fake.resolveManifest = manifest("mail")
	exact, err := k.ResolveAction(ctx, "bob@"+pubB64+"/mail")
	if err != nil {
		t.Fatalf("resolve exact: %v", err)
	}
	fake.resolveManifest = manifest("mail/index")
	if _, err := k.ResolveAction(ctx, "bob@"+pubB64+"/mail/index"); err != nil {
		t.Fatalf("resolve index: %v", err)
	}

	// Deactivate the exact row: the reference must go back to the peer for THAT action, never
	// answer with the index cached beneath it.
	if err := st.DeactivateImportedIfHash(ctx, exact.ID, exact.ArtifactHash, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	fake.resolvedRefs = nil
	fake.resolveManifest = manifest("mail")
	got, err := k.ResolveAction(ctx, "bob@"+pubB64+"/mail")
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if got.ID != exact.ID {
		t.Errorf("a stale exact proxy must re-resolve in place: got %s, want %s", got.ID, exact.ID)
	}
	if len(fake.resolvedRefs) != 1 || fake.resolvedRefs[0] != "bob/mail" {
		t.Errorf("must re-resolve the exact reference: got %v", fake.resolvedRefs)
	}
}
