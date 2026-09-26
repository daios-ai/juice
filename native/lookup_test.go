// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/store"
	"github.com/google/uuid"
)

type fakeEmbedder struct{}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	const dims = 8
	vec := make([]float32, dims)
	for i, b := range []byte(text) {
		vec[i%dims] += float32(b)
	}
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	if norm > 0 {
		sq := float32(math.Sqrt(float64(norm)))
		for i := range vec {
			vec[i] /= sq
		}
	}
	return vec, nil
}

func newLookupTestKernel(t *testing.T) (*kernel.Kernel, kernel.Store) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	issuerID := uuid.New().String()
	hash, _ := kernel.HashPassword("pw")
	_ = db.CreateUser(context.Background(), &kernel.Account{
		ID: issuerID, Handle: "@_issuer",
		PasswordHash: hash, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = issuerID
	cfg.SigningKey = priv
	k := kernel.New(kernel.Dependencies{Store: db, Embedder: &fakeEmbedder{}, Config: cfg})
	// The test kernel calls itself `k`, so a test address reads alice@k (D15).
	_ = db.SetConfig(context.Background(), "signing_public_key", base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
	if err := k.BindOwnName(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	return k, db
}

func seedOwner(t *testing.T, st kernel.Store, handle string) *kernel.Account {
	t.Helper()
	hash, _ := kernel.HashPassword("pw")
	u := &kernel.Account{
		ID: uuid.New().String(), Handle: handle,
		PasswordHash: hash, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("seedOwner %s: %v", handle, err)
	}
	return u
}

// seedAction creates an active public action. price is optional and defaults to 0; pass one only
// where the price itself is under test (a nonzero price would otherwise unfund step-parking tests).
func seedAction(t *testing.T, st kernel.Store, ownerID, name, desc string, price ...int64) *kernel.Action {
	t.Helper()
	var p int64
	if len(price) > 0 {
		p = price[0]
	}
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: ownerID, Name: name,
		Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic, Description: desc,
		Price:     p,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatalf("seedAction %s: %v", name, err)
	}
	emb := &fakeEmbedder{}
	vec, _ := emb.Embed(context.Background(), desc)
	_ = st.UpsertEmbedding(context.Background(), a.ID, vec)
	return a
}

func TestExecuteLookup_MissingQuery(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeLookup(context.Background(), map[string]any{}, "", k)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing query, got %v", err)
	}
}

func TestExecuteLookup_EmptyQuery(t *testing.T) {
	k, _ := newLookupTestKernel(t)
	_, err := executeLookup(context.Background(), map[string]any{"query": ""}, "", k)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty query, got %v", err)
	}
}

func TestExecuteLookup_ReturnsMatchingAction(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	owner := seedOwner(t, st, "alice")
	seedAction(t, st, owner.ID, "weather", "weather forecast temperature rain", 7)

	result, err := executeLookup(ctx, map[string]any{"query": "weather forecast"}, "", k)
	if err != nil {
		t.Fatalf("executeLookup: %v", err)
	}
	items, ok := result["results"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected non-empty results, got %v", result)
	}
	first, _ := items[0].(map[string]any)
	if first["action"] != "alice@k/weather" {
		t.Errorf("expected alice@k/weather first, got %v", first["action"])
	}
	for _, required := range []string{"action_id", "score", "input_schema", "output_schema"} {
		if _, ok := first[required]; !ok {
			t.Errorf("result missing %q", required)
		}
	}
	// The all-in price is returned for every hit, local or discovered (§9).
	if first["price"] != int64(7) {
		t.Errorf("expected price 7, got %v", first["price"])
	}
	// The consolidated ref replaces the separate name/owner_handle fields.
	for _, banned := range []string{"name", "owner_handle", "uses", "failures"} {
		if _, ok := first[banned]; ok {
			t.Errorf("result must not include field %q", banned)
		}
	}
}

func TestExecuteLookup_DefaultLimitIsTen(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	owner := seedOwner(t, st, "bob")
	for i := 0; i < 15; i++ {
		seedAction(t, st, owner.ID, fmt.Sprintf("/svc%d", i), "generic service endpoint")
	}

	result, err := executeLookup(ctx, map[string]any{"query": "generic service"}, "", k)
	if err != nil {
		t.Fatalf("executeLookup: %v", err)
	}
	items, _ := result["results"].([]any)
	if len(items) > 10 {
		t.Errorf("default limit should cap at 10, got %d results", len(items))
	}
}

func TestExecuteLookup_CustomLimit(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	owner := seedOwner(t, st, "carol")
	for i := 0; i < 5; i++ {
		seedAction(t, st, owner.ID, fmt.Sprintf("/item%d", i), "query item result")
	}

	result, err := executeLookup(ctx, map[string]any{"query": "query item", "limit": float64(3)}, "", k)
	if err != nil {
		t.Fatalf("executeLookup: %v", err)
	}
	items, _ := result["results"].([]any)
	if len(items) > 3 {
		t.Errorf("expected at most 3 results with limit=3, got %d", len(items))
	}
}

// TestExecuteLookupRemoteHitNamingAndFreshness freezes what a buyer reads off a hit they cannot
// verify themselves (§13). Two things ride on a remote hit and neither on a local one:
//
//   - observed_at: when this kernel last verified the authority's own description of the action.
//     A local action has none — this kernel IS its authority, so its row is not an observation.
//   - the kernel qualifier goes through the one naming rule: a bound petname once we have met the
//     peer, its self-certifying key before that. Both resolve, so either is runnable as printed.
func TestExecuteLookupRemoteHitNamingAndFreshness(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	// A local action, for contrast.
	owner := seedOwner(t, st, "alice")
	seedAction(t, st, owner.ID, "weather", "forecast the weather for a city")

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	peerKey := base64.RawURLEncoding.EncodeToString(pub)
	observed := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Second)
	doc := &kernel.DiscoveryDoc{
		KernelPublicKey: peerKey, Handle: "prov", ActionID: "act-remote", Name: "weather",
		Description: "forecast the weather for a city", ServingPrice: 10, ObservedAt: observed,
	}
	if err := st.ApplyCatalogPage(ctx, peerKey, []*kernel.DiscoveryDoc{doc}, "", 0); err != nil {
		t.Fatalf("ReplaceDiscoveryDocs: %v", err)
	}
	caller := seedUserWithBalance(t, st, "buyer", 0)

	hits := func() (local, remote map[string]any) {
		t.Helper()
		res, err := executeLookup(ctx, map[string]any{"query": "forecast the weather"}, caller.ID, k)
		if err != nil {
			t.Fatalf("executeLookup: %v", err)
		}
		for _, it := range res["results"].([]any) {
			m := it.(map[string]any)
			if m["action_id"] == "act-remote" {
				remote = m
			} else {
				local = m
			}
		}
		if remote == nil || local == nil {
			t.Fatalf("expected both a local and a discovered hit, got %v", res["results"])
		}
		return local, remote
	}

	local, remote := hits()
	if _, ok := local["observed_at"]; ok {
		t.Error("a local action is authoritative here; it must carry no observation date")
	}
	if remote["observed_at"] != observed.Format(time.RFC3339) {
		t.Errorf("observed_at: got %v, want %v", remote["observed_at"], observed.Format(time.RFC3339))
	}
	// Before we have met the peer, the key is the only name that resolves.
	if want := "prov@" + peerKey + "/weather"; remote["action"] != want {
		t.Errorf("unmet peer must render by key: got %v, want %v", remote["action"], want)
	}
	// Once it has a local name, that is what a buyer sees — and it resolves in kernel position.
	if _, err := k.BindPetname(ctx, peerKey, "weatherco", true); err != nil {
		t.Fatalf("BindPetname: %v", err)
	}
	if _, remote = hits(); remote["action"] != "prov@weatherco/weather" {
		t.Errorf("a named peer must render by petname: got %v", remote["action"])
	}

	// Reachability of the hosting kernel rides every remote hit, so a search result can say when the
	// peer was last reached instead of presenting a dead kernel's actions as if nothing were wrong.
	// Both are absent until the corresponding contact has happened.
	local, remote = hits()
	if _, ok := remote["last_seen"]; ok {
		t.Error("last_seen present before any contact")
	}
	failedAt := time.Now().UTC().Truncate(time.Second)
	if err := st.RecordKernelContact(ctx, peerKey, false, failedAt); err != nil {
		t.Fatalf("RecordKernelContact: %v", err)
	}
	if _, remote = hits(); remote["last_contact_failed_at"] != failedAt.Format(time.RFC3339) {
		t.Errorf("last_contact_failed_at: got %v, want %v", remote["last_contact_failed_at"], failedAt.Format(time.RFC3339))
	}
	seenAt := failedAt.Add(time.Minute)
	if err := st.RecordKernelContact(ctx, peerKey, true, seenAt); err != nil {
		t.Fatalf("RecordKernelContact: %v", err)
	}
	local, remote = hits()
	if remote["last_seen"] != seenAt.Format(time.RFC3339) {
		t.Errorf("last_seen: got %v, want %v", remote["last_seen"], seenAt.Format(time.RFC3339))
	}
	// The failure stays: the kernel reports both facts and judges neither — comparing them is the
	// reader's job (§13).
	if remote["last_contact_failed_at"] != failedAt.Format(time.RFC3339) {
		t.Errorf("a success erased the earlier failure: %v", remote["last_contact_failed_at"])
	}
	// A local action has no hosting kernel to be out of reach.
	if _, ok := local["last_seen"]; ok {
		t.Error("a local action must carry no reachability fields")
	}
}

// A hit carries the same record a person reads on the action, so an agent choosing between
// candidates has the evidence a person would (U10, U46). It is the record, not a rank: the
// ordering is relevance alone.
func TestExecuteLookupHitsCarryTheirEvidence(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()
	owner := seedOwner(t, st, "alice")
	seedAction(t, st, owner.ID, "weather", "weather forecast temperature rain", 7)

	result, err := executeLookup(ctx, map[string]any{"query": "weather forecast"}, "", k)
	if err != nil {
		t.Fatalf("executeLookup: %v", err)
	}
	items, _ := result["results"].([]any)
	if len(items) == 0 {
		t.Fatalf("expected a hit, got %v", result)
	}
	hit, _ := items[0].(map[string]any)
	// The hit carries the record as the reply will: JSON values, because the kernel checks a
	// native's result against its own output schema before paying for it.
	record, ok := hit["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("a hit must carry the action's record, got %T", hit["evidence"])
	}
	if cap, _ := record["retained_cap"].(float64); int(cap) != kernel.EvidenceRetainedPerReporter {
		t.Errorf("retained_cap = %v, want the window the counts must be read against (%d)",
			record["retained_cap"], kernel.EvidenceRetainedPerReporter)
	}
	if record["observed_by_others"] == nil {
		t.Error("an action nobody else has reported on must say so with an empty list, not a null")
	}
}
